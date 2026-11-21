package agent

import (
	"context"
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/printer"
	"github.com/shiuoei/print-bridge/internal/store"
)

// newTrackingTestAgent builds an Agent over a throwaway store with the given
// backend, without starting the listener or worker.
func newTrackingTestAgent(t *testing.T, p printer.Backend) *Agent {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s, p)
}

// trackingPrinter is a fake backend that reports a submission and a verdict, so
// the agent's tracking policy can be tested without a real print queue.
type trackingPrinter struct {
	submission printer.Submission
	outcome    printer.TrackingOutcome
	trackCalls int
}

func (p *trackingPrinter) List(context.Context) ([]string, error) { return []string{"Test"}, nil }
func (p *trackingPrinter) Info(context.Context, string) (*domain.PrinterInfo, error) {
	return &domain.PrinterInfo{Name: "Test"}, nil
}
func (p *trackingPrinter) PrintPDF(context.Context, string, string, int, *domain.Paper) (printer.Submission, error) {
	return p.submission, nil
}
func (p *trackingPrinter) PrintRaw(context.Context, string, []byte) (printer.Submission, error) {
	return p.submission, nil
}
func (p *trackingPrinter) Track(context.Context, printer.Submission, string) printer.TrackingOutcome {
	p.trackCalls++
	return p.outcome
}

// TestTrackSubmissionReportsPlatformVerdict covers the whole point of the tracking
// layer: when the platform positively reports completion, the job may say so.
func TestTrackSubmissionReportsPlatformVerdict(t *testing.T) {
	p := &trackingPrinter{
		submission: printer.Submission{Backend: "cups", SystemJobID: "P-1", Tracking: true},
		outcome:    printer.TrackingOutcome{State: printer.TrackingCompleted, Message: "CUPS reports the print job as completed"},
	}
	a := newTrackingTestAgent(t, p)
	a.trackSubmission("job-1", p.submission, "Test")
	if p.trackCalls != 1 {
		t.Fatalf("Track called %d times, want 1", p.trackCalls)
	}
}

// TestTrackSubmissionSkipsUntrackableBackend is the core safety property: a
// backend that cannot expose a job id must never be asked, so the job stays at
// `submitted` rather than being upgraded to a claim nobody can verify.
func TestTrackSubmissionSkipsUntrackableBackend(t *testing.T) {
	p := &trackingPrinter{
		submission: printer.Submission{Backend: "windows-sumatra", Tracking: false},
		outcome:    printer.TrackingOutcome{State: printer.TrackingCompleted},
	}
	a := newTrackingTestAgent(t, p)
	a.trackSubmission("job-2", p.submission, "Test")
	if p.trackCalls != 0 {
		t.Fatalf("Track called %d times for an untrackable backend, want 0", p.trackCalls)
	}
}

// TestTrackSubmissionUnknownIsNotCompleted guards the conservative mapping: an
// unknown verdict must not become `completed`.
func TestTrackSubmissionUnknownIsNotCompleted(t *testing.T) {
	p := &trackingPrinter{
		submission: printer.Submission{Backend: "cups", SystemJobID: "P-3", Tracking: true},
		outcome:    printer.TrackingOutcome{State: printer.TrackingUnknown, Message: "CUPS job status was no longer available"},
	}
	a := newTrackingTestAgent(t, p)
	a.trackSubmission("job-3", p.submission, "Test")
	if p.trackCalls != 1 {
		t.Fatalf("Track called %d times, want 1", p.trackCalls)
	}
}

// TestBackendWithoutTrackerIsHandled keeps a backend that reports Tracking=true
// but does not implement Tracker from panicking. The agent must fall back to
// leaving the job at `submitted`.
func TestBackendWithoutTrackerIsHandled(t *testing.T) {
	a := newTrackingTestAgent(t, &untrackableBackend{})
	// Must not panic, and must not upgrade the job.
	a.trackSubmission("job-4", printer.Submission{Tracking: true, SystemJobID: "x"}, "Test")
}

// untrackableBackend implements Backend but deliberately not Tracker.
type untrackableBackend struct{}

func (untrackableBackend) List(context.Context) ([]string, error) { return nil, nil }
func (untrackableBackend) Info(context.Context, string) (*domain.PrinterInfo, error) {
	return &domain.PrinterInfo{}, nil
}
func (untrackableBackend) PrintPDF(context.Context, string, string, int, *domain.Paper) (printer.Submission, error) {
	return printer.Submission{}, nil
}
func (untrackableBackend) PrintRaw(context.Context, string, []byte) (printer.Submission, error) {
	return printer.Submission{}, nil
}
