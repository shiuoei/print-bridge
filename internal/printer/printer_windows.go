//go:build windows

package printer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unsafe"

	"github.com/shiuoei/print-bridge/internal/domain"
	"golang.org/x/sys/windows"
)

type platformBackend struct{}

const (
	// backendWindowsRaw and backendWindowsSumatra name the two Windows paths so
	// logs and job history can say which one served a submission.
	backendWindowsRaw     = "windows-raw-spooler"
	backendWindowsSumatra = "windows-sumatra"
)

var spool = windows.NewLazySystemDLL("winspool.drv")

var openPrinter = spool.NewProc("OpenPrinterW")
var closePrinter = spool.NewProc("ClosePrinter")
var startDoc = spool.NewProc("StartDocPrinterW")
var endDoc = spool.NewProc("EndDocPrinter")
var startPage = spool.NewProc("StartPagePrinter")
var endPage = spool.NewProc("EndPagePrinter")
var writePrinter = spool.NewProc("WritePrinter")
var enumPrinters = spool.NewProc("EnumPrintersW")

type docInfo struct {
	Name     *uint16
	Output   *uint16
	DataType *uint16
}
type printerInfo4 struct {
	Name       *uint16
	Server     *uint16
	Attributes uint32
}

func winErr(ok uintptr, err error) error {
	if ok != 0 {
		return nil
	}
	if err != nil && err != windows.ERROR_SUCCESS {
		return err
	}
	return errors.New("Windows print spooler failed")
}

func (platformBackend) List(ctx context.Context) ([]string, error) {
	const localAndConnections = 0x2 | 0x4
	var needed, count uint32
	enumPrinters.Call(localAndConnections, 0, 4, 0, 0, uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
	if needed == 0 {
		return []string{}, nil
	}
	buf := make([]byte, needed)
	ok, _, err := enumPrinters.Call(localAndConnections, 0, 4, uintptr(unsafe.Pointer(&buf[0])), uintptr(needed), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
	if e := winErr(ok, err); e != nil {
		return nil, e
	}
	items := unsafe.Slice((*printerInfo4)(unsafe.Pointer(&buf[0])), count)
	names := make([]string, 0, count)
	for _, item := range items {
		names = append(names, windows.UTF16PtrToString(item.Name))
	}
	return names, nil
}

func (platformBackend) PrintRaw(ctx context.Context, name string, data []byte) (Submission, error) {
	if name == "" {
		return Submission{}, errors.New("raw printing requires a printer name")
	}
	if len(data) == 0 {
		return Submission{}, errors.New("empty raw data")
	}
	select {
	case <-ctx.Done():
		return Submission{}, ctx.Err()
	default:
	}
	printerName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return Submission{}, err
	}
	var handle windows.Handle
	ok, _, callErr := openPrinter.Call(uintptr(unsafe.Pointer(printerName)), uintptr(unsafe.Pointer(&handle)), 0)
	if err := winErr(ok, callErr); err != nil {
		return Submission{}, err
	}
	defer closePrinter.Call(uintptr(handle))
	docName, _ := windows.UTF16PtrFromString("PrintBridge Job")
	dataType, _ := windows.UTF16PtrFromString("RAW")
	doc := docInfo{Name: docName, DataType: dataType}
	jobID, _, callErr := startDoc.Call(uintptr(handle), 1, uintptr(unsafe.Pointer(&doc)))
	if err := winErr(jobID, callErr); err != nil {
		return Submission{}, err
	}
	defer endDoc.Call(uintptr(handle))
	ok, _, callErr = startPage.Call(uintptr(handle))
	if err := winErr(ok, callErr); err != nil {
		return Submission{}, err
	}
	defer endPage.Call(uintptr(handle))
	var written uint32
	ok, _, callErr = writePrinter.Call(uintptr(handle), uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), uintptr(unsafe.Pointer(&written)))
	if err := winErr(ok, callErr); err != nil {
		return Submission{}, err
	}
	if int(written) != len(data) {
		return Submission{}, fmt.Errorf("short spool write: %d/%d", written, len(data))
	}
	// The spooler hands back a job id from StartDocPrinter, but the Windows PDF
	// path goes through SumatraPDF and the spooler does not expose a handle this
	// agent can query for a verdict. Reporting tracking=false keeps the job at
	// `submitted`, which is the strongest claim the platform supports here.
	return Submission{Backend: backendWindowsRaw, SystemJobID: strconv.FormatUint(uint64(jobID), 10), Tracking: false}, nil
}

func (p platformBackend) PrintPDF(ctx context.Context, path, name string, copies int, paper *domain.Paper) (Submission, error) {
	return p.PrintPDFWithOptions(ctx, path, name, copies, paper, PrintOptions{})
}

// PrintPDFWithOptions submits a PDF, applying the page-fitting choices the job
// was accepted with.
//
// SumatraPDF 3.6.1's -print-settings understands three scaling tokens
// (noscale, shrink, fit) and the two orientations — no stretch, no
// fit_width/fit_height, and no centring setting at all. Some business front-ends
// express their choice in other terms, so each one is mapped onto the closest
// token instead of being rejected: a wrong-but-close scale still prints a usable
// label, whereas refusing the job stops the shipment.
//
// Centring is a property of the scale token, not a setting: noscale puts the page
// at the physical paper's top-left (SumatraPDF does that on purpose so forms and
// labels of varying size line up), while shrink and fit centre the page on the
// paper and keep the content inside the printable area. PrintOptions.Center
// therefore picks between shrink (centred, never enlarged) and noscale (1:1,
// top-left) for an actual-size job; for the other modes the token is fit and
// SumatraPDF centres regardless.
func (p platformBackend) PrintPDFWithOptions(ctx context.Context, path, name string, copies int, paper *domain.Paper, opts PrintOptions) (Submission, error) {
	tool, err := commandPath("SumatraPDF.exe")
	if err != nil {
		return Submission{}, err
	}
	// SumatraPDF's `-print-to` is already a silent print; there is no `-silent`
	// flag and passing one aborts with "ParseFlags: argName: '-silent'".
	args := []string{}
	printTo := name
	if printTo == "" {
		args = append(args, "-print-to-default")
	} else {
		args = append(args, "-print-to", name)
	}
	// SumatraPDF's -print-settings takes a comma-separated list:
	//   - "<n>x"       copies (always sent, even for 1)
	//   - scaling      one of noscale/shrink/fit, which also decides centring
	//   - orientation  derived from the requested paper, so a landscape request
	//                  on a portrait form is not rotated onto two pages
	//   - "paper=<driver name>" the paper resolved above
	// A token SumatraPDF does not know is silently ignored (ApplyPrintSettings has
	// no error branch), so nothing here may rely on an unknown token working.
	settings := []string{strconv.Itoa(copies) + "x", sumatraScaleToken(opts.ScaleMode, opts.Center)}
	if paper != nil {
		settings = append(settings, paperOrientationFor(paper, opts.Direction))
		paperName, err := p.driverPaperName(ctx, printTo, paper)
		if err != nil {
			return Submission{}, err
		}
		settings = append(settings, "paper="+paperName)
	}
	args = append(args, "-print-settings", strings.Join(settings, ","))
	args = append(args, path)
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err := commandError(out, err); err != nil {
		return Submission{}, err
	}
	// SumatraPDF returns no queue job id, and the Windows spooler cannot be asked
	// for a verdict from this path. tracking=false is the honest answer and keeps
	// the job at `submitted`.
	return Submission{Backend: backendWindowsSumatra, Tracking: false}, nil
}

// sumatraScaleToken maps a job's scale_mode and centring choice onto one of the
// three scaling tokens -print-settings understands.
//
// The business settings panel offers only 实际大小 and 按纸张比例, which arrive as
// "actual" and "fill"; a previous implementation printed both of them as "fit".
// "fill" is therefore mapped to "fit" here as well, NOT to "stretch": stretch
// scales width and height independently, which distorts a shipping label — the
// one outcome that must never happen. The panel's own wording is 宽高占比
// (preserve the aspect ratio), so "fit" is both the compatible choice and the
// correct one.
//
// fit_width and fit_height have no single-axis equivalent in SumatraPDF; they
// also degrade to "fit" (complete containment, never cropped).
//
// An actual-size job has no centring option in SumatraPDF: noscale is top-left by
// design. Asking for centring maps it to shrink instead — the same fit calculation
// capped at 1:1, so it never enlarges, always centres, and keeps every printable
// pixel inside the paper's printable area.
func sumatraScaleToken(mode string, center bool) string {
	if mode == domain.ScaleActual {
		if center {
			return "shrink"
		}
		return "noscale"
	}
	// "fill", "fit_width", "fit_height", empty and anything unrecognised all
	// print as a complete page scaled into the paper (and centred, as fit does).
	return "fit"
}

// paperOrientation reports the orientation a requested paper implies. Square
// stock stays portrait, which matches the documented rule.
func paperOrientation(paper *domain.Paper) string {
	return paperOrientationFor(paper, "")
}

// paperOrientationFor resolves the orientation token. An explicit request wins
// over the paper's own aspect ratio, except that "auto" (and empty) means
// "derive it from the paper", which is the historical behaviour.
//
// Square stock stays portrait in both derivations, matching the documented rule.
func paperOrientationFor(paper *domain.Paper, direction string) string {
	switch direction {
	case "portrait":
		return "portrait"
	case "landscape":
		return "landscape"
	}
	if paper != nil && paper.WidthMM > paper.HeightMM {
		return "landscape"
	}
	return "portrait"
}

// driverPaperName translates the requested millimetre size into the paper name
// this printer driver actually advertises.
//
// Passing a literal size (for example `paper=100x180mm`) to SumatraPDF does not
// fail: the setting is silently dropped and the job prints on the device default.
// On a label printer that means every label comes out at the wrong size, so an
// unmatched request is reported as PAPER_NOT_FOUND instead of being printed wrong.
func (p platformBackend) driverPaperName(ctx context.Context, printerName string, paper *domain.Paper) (string, error) {
	if printerName == "" {
		sys, err := p.SystemDefault(ctx)
		if err != nil || sys == "" {
			return "", domain.NewProtocolError(domain.CodePrinterNotConfigured, "cannot resolve the default printer for the requested paper")
		}
		printerName = sys
	}
	info, err := p.Info(ctx, printerName)
	if err != nil {
		// SumatraPDF silently ignores literal millimetre sizes. If the queue
		// cannot be queried, printing could use the wrong paper or a missing
		// printer, so report the lookup failure instead.
		return "", err
	}
	if name, ok := resolvePaperName(info.Papers, paper); ok {
		return name, nil
	}
	return "", domain.NewProtocolErrorf(domain.CodePaperNotFound,
		"printer %q has no paper of %gx%g mm among its %d advertised sizes",
		printerName, paper.WidthMM, paper.HeightMM, len(info.Papers))
}
