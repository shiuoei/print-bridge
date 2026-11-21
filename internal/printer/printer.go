package printer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// ErrPrinterNotFound reports that the requested printer does not exist on this
// system. The agent layer maps it to the wire error code PRINTER_NOT_FOUND, so it
// is a sentinel: callers must test it with errors.Is, and backends must wrap it
// (fmt.Errorf("...: %w", ErrPrinterNotFound)) rather than compare strings.
var ErrPrinterNotFound = errors.New("printer not found")

// Submission describes what a backend knows about a job it just handed to the
// operating system's print queue.
//
// It exists so the agent can distinguish "submitted, and the platform will tell
// us what happened next" from "submitted, and that is all we can ever know".
// Reporting `completed` without a verifiable system job id would be a lie, so
// Tracking is false whenever the platform did not expose one.
type Submission struct {
	// Backend identifies which path served the job (for example
	// "windows-sumatra" or "linux-cups"), so logs can name it.
	Backend string
	// SystemJobID is the queue's identifier for this job, when it disclosed one.
	SystemJobID string
	// Tracking reports whether Track can return a meaningful outcome. It is true
	// only when SystemJobID is usable.
	Tracking bool
}

// TrackingOutcome is the conservative verdict a backend can reach about a job it
// already submitted.
type TrackingOutcome struct {
	State   TrackingState
	Message string
}

// TrackingState enumerates the outcomes a backend may report.
type TrackingState int

const (
	// TrackingUnknown means the platform could not be asked, or answered in a way
	// that does not prove completion. It must be reported as `unknown`, never
	// silently upgraded to `completed`.
	TrackingUnknown TrackingState = iota
	// TrackingCompleted means the platform positively reports the job as done.
	TrackingCompleted
	// TrackingFailed means the platform reports the job as aborted.
	TrackingFailed
)

// Tracker is implemented by backends that can query the operating system's print
// queue for the fate of a submission. It is a separate interface because on
// Windows the spooler does not expose a usable job handle through the PDF path,
// so those backends legitimately have nothing to track.
type Tracker interface {
	// Track reports the platform's verdict for one previous submission.
	Track(ctx context.Context, submission Submission, printerName string) TrackingOutcome
}

// PrintOptions carries the page-fitting choices a job was accepted with.
//
// It is passed through an optional interface rather than added to Backend's
// PrintPDF signature: every caller that does not care about scaling keeps
// calling PrintPDF exactly as before, and a backend that cannot honour a field
// reports that instead of silently dropping it.
type PrintOptions struct {
	// ScaleMode is one of domain.ScaleActual/Fill/FitWidth/FitHeight, or empty.
	ScaleMode string
	// Direction is "portrait", "landscape", "auto" or empty.
	Direction string
	// Center asks for the page to be centred on the paper when it is smaller
	// than it. The Windows backend can only express centring through the scale
	// token: SumatraPDF has no centring setting, but its fit/shrink modes centre
	// the page and its noscale mode deliberately starts at the paper's top-left.
	// Center therefore decides which of those two an actual-size job gets.
	Center bool
}

// OptionedPdfPrinter is implemented by backends that can apply PrintOptions.
// Windows does; the CUPS path has no equivalent for these knobs yet and falls
// back to plain PrintPDF, which prints correctly but without the fine scaling.
type OptionedPdfPrinter interface {
	PrintPDFWithOptions(ctx context.Context, path, name string, copies int, paper *domain.Paper, opts PrintOptions) (Submission, error)
}

// summarizeOptions renders opts for a log line without exposing a struct dump.
func summarizeOptions(opts PrintOptions) string {
	parts := make([]string, 0, 3)
	if opts.ScaleMode != "" {
		parts = append(parts, "scale="+opts.ScaleMode)
	}
	if opts.Direction != "" {
		parts = append(parts, "direction="+opts.Direction)
	}
	if opts.Center {
		parts = append(parts, "center")
	}
	if len(parts) == 0 {
		return "default"
	}
	return strings.Join(parts, ",")
}

// Backend is the platform printing surface. Implementations must return
// non-nil, empty slices for capability lists they cannot enumerate, and must
// wrap ErrPrinterNotFound for unknown printers.
//
// PrintPDF and PrintRaw return a Submission so the caller can keep the system
// job id for tracking. A backend that cannot expose one returns an empty
// SystemJobID and Tracking=false, which keeps the job at `submitted`.
type Backend interface {
	List(context.Context) ([]string, error)
	// Info reports the system capabilities of one printer. Unknown fields stay
	// nil/empty; guessing a paper size or DPI would make the UI lie about what
	// the device can actually do.
	Info(context.Context, string) (*domain.PrinterInfo, error)
	PrintPDF(context.Context, string, string, int, *domain.Paper) (Submission, error)
	PrintRaw(context.Context, string, []byte) (Submission, error)
}

// SystemDefaulter is implemented by backends that can report the operating
// system's default printer. It is a separate interface because "what the OS
// would print to" and "what this agent is configured to use" are different
// questions and several call paths need only the former.
type SystemDefaulter interface {
	SystemDefault(ctx context.Context) (string, error)
}

func New() Backend { return platformBackend{} }

func commandPath(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	p := filepath.Join(filepath.Dir(self), name)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	p = filepath.Join("resources", "windows-amd64", name)
	if _, err := os.Stat(p); err == nil {
		return filepath.Abs(p)
	}
	return "", fmt.Errorf("%s not found", name)
}

func commandError(out []byte, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("print command: %w: %s", err, strings.TrimSpace(string(out)))
}

// boolPtr and intPtr build the optional capability fields. They exist so the
// platform files can say "we know this" without repeating address-taking.
func boolPtr(v bool) *bool { return &v }

func intPtr(v int) *int { return &v }

func stringPtr(v string) *string { return &v }
