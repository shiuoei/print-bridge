//go:build !windows

package printer

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"

	"github.com/shiuoei/print-bridge/internal/domain"
)

const (
	// backendCUPS and backendCUPSRaw name the macOS/Linux paths so logs and job
	// history can say which one served a submission.
	backendCUPS    = "cups"
	backendCUPSRaw = "cups-raw"
)

type platformBackend struct{}

func (platformBackend) List(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "lpstat", "-a").Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			names = append(names, fields[0])
		}
	}
	return names, nil
}

func (platformBackend) PrintPDF(ctx context.Context, path, name string, copies int, paper *domain.Paper) (Submission, error) {
	// Resolve the queue first so a mistyped printer name is reported as
	// PRINTER_NOT_FOUND instead of a generic PRINT_FAILED carrying lp's stderr.
	target, err := ensureCUPSPrinter(ctx, name)
	if err != nil {
		return Submission{}, err
	}
	args := []string{"-d", target}
	if copies > 1 {
		args = append(args, "-n", strconv.Itoa(copies))
	}
	if paper != nil {
		args = append(args, "-o", "media="+cupsMediaOption(ctx, target, paper))
	}
	args = append(args, path)
	out, err := exec.CommandContext(ctx, "lp", args...).CombinedOutput()
	if err != nil {
		return Submission{}, commandError(out, err)
	}
	return Submission{
		Backend:     backendCUPS,
		SystemJobID: parseCUPSJobID(string(out)),
		Tracking:    parseCUPSJobID(string(out)) != "",
	}, nil
}

func (platformBackend) PrintRaw(ctx context.Context, name string, data []byte) (Submission, error) {
	target, err := ensureCUPSPrinter(ctx, name)
	if err != nil {
		return Submission{}, err
	}
	args := []string{"-o", "raw", "-d", target}
	cmd := exec.CommandContext(ctx, "lp", args...)
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Submission{}, commandError(out, err)
	}
	jobID := parseCUPSJobID(string(out))
	return Submission{Backend: backendCUPSRaw, SystemJobID: jobID, Tracking: jobID != ""}, nil
}

// cupsMediaOption builds the `-o media=` token for a requested size.
func cupsMediaOption(ctx context.Context, printerName string, paper *domain.Paper) string {
	if paper == nil {
		return ""
	}
	if info, err := (platformBackend{}).Info(ctx, printerName); err == nil {
		if name, ok := resolvePaperName(info.Papers, paper); ok {
			// lpoptions reports the PPD keyword (for example `iso_a4_210x297mm`)
			// and `lp -o media=` expects exactly that token. A friendly display
			// name would not resolve, so prefer the id when one exists.
			for _, candidate := range info.Papers {
				if candidate.Name == name && candidate.ID != "" {
					return candidate.ID
				}
			}
			return mediaTokenFromName(name)
		}
	}
	return "Custom." + formatMM(paper.WidthMM) + "x" + formatMM(paper.HeightMM) + "mm"
}

// Track asks CUPS for this submission's fate.
//
// `lpstat -W completed -o` lists completed jobs only, so a match proves the job
// finished. Absence proves nothing: CUPS expires finished jobs from the
// completed list, so no match means "cannot tell", which must be reported as
// unknown rather than failed.
func (platformBackend) Track(ctx context.Context, submission Submission, printerName string) TrackingOutcome {
	if submission.SystemJobID == "" {
		return TrackingOutcome{State: TrackingUnknown, Message: "CUPS did not expose a print job id"}
	}
	out, err := lpOutput(ctx, "lpstat", "-W", "completed", "-o")
	if err != nil {
		return TrackingOutcome{State: TrackingUnknown, Message: "CUPS completed-job listing unavailable"}
	}
	if cupsCompletedContains(string(out), submission.SystemJobID) {
		return TrackingOutcome{State: TrackingCompleted, Message: "CUPS reports the print job as completed"}
	}
	return TrackingOutcome{State: TrackingUnknown, Message: "CUPS job status was no longer available"}
}

// ensureCUPSPrinter verifies the queue exists and resolves the default when the
// caller did not name one.
func ensureCUPSPrinter(ctx context.Context, name string) (string, error) {
	if name != "" {
		if err := cupsPrinterExists(ctx, name); err != nil {
			return "", err
		}
		return name, nil
	}
	defaultName, err := platformBackend{}.SystemDefault(ctx)
	if err != nil || defaultName == "" {
		return "", domain.NewProtocolError(domain.CodePrinterNotConfigured, "no printer selected and CUPS has no default destination")
	}
	return defaultName, nil
}

// cupsPrinterExists reports PRINTER_NOT_FOUND when the queue is unknown.
func cupsPrinterExists(ctx context.Context, name string) error {
	out, err := lpOutput(ctx, "lpstat", "-p", name)
	if err == nil {
		return nil
	}
	if unknownPrinterOutput(out) {
		return domain.NewProtocolErrorf(domain.CodePrinterNotFound, "printer not found: %s", name)
	}
	return commandError(out, err)
}
