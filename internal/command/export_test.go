package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/store"
)

// seedExportService builds a service with two jobs of different statuses.
func seedExportService(t *testing.T) (Service, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	for _, row := range []struct{ id, status string }{
		{"JOB-OK", "submitted"},
		{"JOB-BAD", "failed"},
	} {
		if err := s.AddStatus(ctx, domain.Status{
			JobID: row.id, Status: row.status, Message: row.status,
			PrinterName: "P", PaperName: "A4", Copies: 1, At: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return Service{Store: s}, s
}

// TestExportHistoryCommandCSV covers the default format.
func TestExportHistoryCommandCSV(t *testing.T) {
	svc, _ := seedExportService(t)
	var r Response
	if err := svc.exportHistory(context.Background(), Request{Command: "history.export"}, &r); err != nil {
		t.Fatal(err)
	}
	if r.RowsCount != 2 {
		t.Fatalf("count = %d, want 2", r.RowsCount)
	}
	if r.Value != "csv" {
		t.Fatalf("format = %q, want csv", r.Value)
	}
	if !strings.Contains(string(r.Rows), "job_id") || !strings.Contains(string(r.Rows), "JOB-OK") {
		t.Fatalf("CSV content unexpected: %s", r.Rows)
	}
}

// TestExportHistoryCommandJSON covers the JSON format.
func TestExportHistoryCommandJSON(t *testing.T) {
	svc, _ := seedExportService(t)
	var r Response
	if err := svc.exportHistory(context.Background(), Request{Format: "json"}, &r); err != nil {
		t.Fatal(err)
	}
	if r.Value != "json" {
		t.Fatalf("format = %q", r.Value)
	}
	if !strings.Contains(string(r.Rows), `"job_id"`) {
		t.Fatalf("JSON content unexpected: %s", r.Rows)
	}
}

// TestExportHistoryCommandRejectsUnknownFormat keeps a typo from silently producing
// a file in the wrong format.
func TestExportHistoryCommandRejectsUnknownFormat(t *testing.T) {
	svc, _ := seedExportService(t)
	var r Response
	err := svc.exportHistory(context.Background(), Request{Format: "xml"}, &r)
	if err == nil {
		t.Fatal("an unknown format was accepted")
	}
	if domain.CodeOf(err) != domain.CodeArgumentInvalid {
		t.Fatalf("code = %q", domain.CodeOf(err))
	}
}

// TestExportHistoryCommandFiltersByStatus keeps the export aligned with the table.
func TestExportHistoryCommandFiltersByStatus(t *testing.T) {
	svc, _ := seedExportService(t)
	var r Response
	if err := svc.exportHistory(context.Background(), Request{Status: "failed"}, &r); err != nil {
		t.Fatal(err)
	}
	if r.RowsCount != 1 {
		t.Fatalf("count = %d, want 1", r.RowsCount)
	}
	if strings.Contains(string(r.Rows), "JOB-OK") {
		t.Fatal("the status filter did not exclude the other job")
	}
}

// TestExportHistoryCommandDaysWindow covers the --days window.
func TestExportHistoryCommandDaysWindow(t *testing.T) {
	svc, s := seedExportService(t)
	// Add a job older than the window.
	if err := s.AddStatus(context.Background(), domain.Status{
		JobID: "JOB-OLD", Status: "submitted", Message: "old",
		At: time.Now().UTC().Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	var r Response
	if err := svc.exportHistory(context.Background(), Request{Days: 3}, &r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(r.Rows), "JOB-OLD") {
		t.Fatal("the day window did not exclude the older job")
	}
}
