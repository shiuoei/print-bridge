package store

import (
	"context"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// TestHistoryRecordsJobMetadata is the point of the history columns: a row must
// answer which printer, paper, copies, source and batch a job used, without
// consulting the current configuration.
func TestHistoryRecordsJobMetadata(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	st := domain.Status{
		JobID: "meta-1", RequestID: "REQ", Status: "queued", Message: "queued",
		BatchID: "BATCH-1", Source: domain.SourceRemote,
		PrinterName: "HPRT N41", PaperName: "100x180mm", Copies: 3,
		At: time.Now().UTC(),
	}
	if err := s.AddStatus(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.Latest(ctx, "meta-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.BatchID != "BATCH-1" {
		t.Errorf("batch_id = %q, want BATCH-1", got.BatchID)
	}
	if got.Source != domain.SourceRemote {
		t.Errorf("source = %q, want %q", got.Source, domain.SourceRemote)
	}
	if got.PrinterName != "HPRT N41" {
		t.Errorf("printer_name = %q", got.PrinterName)
	}
	if got.PaperName != "100x180mm" {
		t.Errorf("paper_name = %q", got.PaperName)
	}
	if got.Copies != 3 {
		t.Errorf("copies = %d, want 3", got.Copies)
	}
}

// TestHistoryDefaultsSourceToWebSocket keeps a caller that does not set Source
// from producing an empty column.
func TestHistoryDefaultsSourceToWebSocket(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.AddStatus(ctx, domain.Status{JobID: "j", Status: "queued", Message: "queued", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Latest(ctx, "j")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != domain.SourceWebSocket {
		t.Fatalf("source = %q, want %q", got.Source, domain.SourceWebSocket)
	}
}

// TestFinishedAtOnlySetForTerminalStatuses mirrors the rule: a running
// job has no finish time, and the last terminal transition wins.
func TestFinishedAtOnlySetForTerminalStatuses(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.AddStatus(ctx, domain.Status{JobID: "f1", Status: "queued", Message: "queued", At: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddStatus(ctx, domain.Status{JobID: "f1", Status: "printing", Message: "printing", At: now}); err != nil {
		t.Fatal(err)
	}
	var finished any
	if err := s.db.QueryRow(`SELECT finished_at FROM job_status WHERE job_id='f1' ORDER BY id DESC LIMIT 1`).Scan(&finished); err != nil {
		t.Fatal(err)
	}
	if finished != nil {
		t.Fatalf("a running job has finished_at = %v, want NULL", finished)
	}

	if err := s.AddStatus(ctx, domain.Status{JobID: "f1", Status: "submitted", Message: "submitted", At: now}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT finished_at FROM job_status WHERE job_id='f1' ORDER BY id DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatal("a terminal job has no finished_at")
	}
}

// TestTerminalStatusesAreRecognized pins the set that sets finished_at.
func TestTerminalStatusesAreRecognized(t *testing.T) {
	for _, status := range []string{"submitted", "completed", "failed", "unknown", "cancelled"} {
		if !domain.IsTerminalStatus(status) {
			t.Errorf("%q should be terminal", status)
		}
	}
	for _, status := range []string{"queued", "downloading", "printing", ""} {
		if domain.IsTerminalStatus(status) {
			t.Errorf("%q should not be terminal", status)
		}
	}
}

// TestJobHistoryCarriesMetadata keeps the timeline view from losing the columns.
func TestJobHistoryCarriesMetadata(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	for _, status := range []string{"queued", "printing", "submitted"} {
		if err := s.AddStatus(ctx, domain.Status{
			JobID: "tl", Status: status, Message: status, At: now,
			Source: domain.SourceWebSocket, PrinterName: "P", PaperName: "A4", Copies: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	timeline, err := s.JobHistory(ctx, "tl")
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline) != 3 {
		t.Fatalf("timeline length = %d, want 3", len(timeline))
	}
	for i, st := range timeline {
		if st.PrinterName != "P" || st.PaperName != "A4" || st.Copies != 1 {
			t.Errorf("row %d lost metadata: %#v", i, st)
		}
	}
}
