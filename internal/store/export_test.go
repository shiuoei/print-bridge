package store

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// seedExportRow inserts one job with the history metadata an export must carry.
func seedExportRow(t *testing.T, s *Store, jobID, status, printer, paper string, copies int, at time.Time) {
	t.Helper()
	st := domain.Status{
		JobID: jobID, RequestID: "REQ-" + jobID, Status: status, Message: status + " message",
		Source: domain.SourceWebSocket, PrinterName: printer, PaperName: paper, Copies: copies, At: at,
	}
	if err := s.AddStatus(context.Background(), st); err != nil {
		t.Fatal(err)
	}
}

// TestExportHistoryCSVHasHeaderAndRows covers the CSV shape: BOM, header, values.
func TestExportHistoryCSVHasHeaderAndRows(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	seedExportRow(t, s, "JOB-1", "submitted", "HPRT N41", "100x180mm", 3, now)

	data, count, err := s.ExportHistory(context.Background(), ExportOptions{Format: ExportCSV})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if !strings.HasPrefix(string(data), "\ufeff") {
		t.Error("CSV is missing the UTF-8 BOM that makes Excel read it as UTF-8")
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(data), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatalf("not valid CSV: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want header + 1 row", len(records))
	}
	if records[0][0] != "job_id" || records[0][1] != "status" {
		t.Fatalf("header = %v", records[0])
	}
	row := records[1]
	if row[0] != "JOB-1" || row[1] != "submitted" || row[4] != "HPRT N41" || row[6] != "3" {
		t.Fatalf("row = %v", row)
	}
	// Timestamps are rendered in UTC RFC3339 so a spreadsheet can parse them.
	if _, err := time.Parse(time.RFC3339, row[8]); err != nil {
		t.Fatalf("updated_at %q is not RFC3339: %v", row[8], err)
	}
}

// TestExportHistoryJSONIsParseable covers the JSON shape.
func TestExportHistoryJSONIsParseable(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedExportRow(t, s, "JOB-1", "failed", "P", "A4", 1, time.Now().UTC())

	data, count, err := s.ExportHistory(context.Background(), ExportOptions{Format: ExportJSON})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	var decoded []JobSummary
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if len(decoded) != 1 || decoded[0].JobID != "JOB-1" || decoded[0].LatestStatus != "failed" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

// TestExportHistoryFiltersByStatus keeps the export consistent with the table
// filter.
func TestExportHistoryFiltersByStatus(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	seedExportRow(t, s, "ok-1", "submitted", "P", "A4", 1, now)
	seedExportRow(t, s, "bad-1", "failed", "P", "A4", 1, now)

	_, count, err := s.ExportHistory(context.Background(), ExportOptions{Format: ExportCSV, Status: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	data, _, err := s.ExportHistory(context.Background(), ExportOptions{Format: ExportCSV, Status: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ok-1") {
		t.Fatal("the status filter did not exclude the other job")
	}
}

// TestExportHistoryFiltersByQuery covers the job-id search.
func TestExportHistoryFiltersByQuery(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	seedExportRow(t, s, "INV-100", "submitted", "P", "A4", 1, now)
	seedExportRow(t, s, "LABEL-200", "submitted", "P", "A4", 1, now)

	data, count, err := s.ExportHistory(context.Background(), ExportOptions{Format: ExportCSV, Query: "INV"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || !strings.Contains(string(data), "INV-100") {
		t.Fatalf("count = %d, data has INV-100 = %v", count, strings.Contains(string(data), "INV-100"))
	}
	if strings.Contains(string(data), "LABEL-200") {
		t.Fatal("the query filter did not exclude the other job")
	}
}

// TestExportHistoryFiltersBySince covers the time window.
func TestExportHistoryFiltersBySince(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	seedExportRow(t, s, "old-1", "submitted", "P", "A4", 1, now.Add(-72*time.Hour))
	seedExportRow(t, s, "new-1", "submitted", "P", "A4", 1, now)

	data, count, err := s.ExportHistory(context.Background(), ExportOptions{
		Format: ExportCSV, Since: now.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if strings.Contains(string(data), "old-1") {
		t.Fatal("the time window did not exclude the older job")
	}
}

// TestExportHistoryRespectsLimit bounds one export.
func TestExportHistoryRespectsLimit(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		seedExportRow(t, s, "job-"+string(rune('a'+i)), "submitted", "P", "A4", 1, now.Add(time.Duration(i)*time.Minute))
	}
	_, count, err := s.ExportHistory(context.Background(), ExportOptions{Format: ExportCSV, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
}

// TestExportHistoryCSVQuotesSpecialCharacters makes sure a comma or quote in a
// message does not corrupt the file.
func TestExportHistoryCSVQuotesSpecialCharacters(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.AddStatus(ctx, domain.Status{
		JobID: "JOB-QUOTE", Status: "failed",
		Message: `failed: "printer, offline"`, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	data, _, err := s.ExportHistory(ctx, ExportOptions{Format: ExportCSV})
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(data), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatalf("quoting broke the CSV: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d", len(records))
	}
	if !strings.Contains(records[1][10], "printer, offline") {
		t.Fatalf("message = %q", records[1][10])
	}
}

// TestParseExportFormat validates the format names.
func TestParseExportFormat(t *testing.T) {
	for _, ok := range []string{"", "csv", "CSV", "json", " JSON "} {
		if _, err := ParseExportFormat(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	if _, err := ParseExportFormat("xml"); err == nil {
		t.Error("xml accepted")
	}
}

// TestExportHistoryEmptyIsStillValid keeps an empty export from producing a broken
// file: a header-only CSV and an empty JSON array are both valid.
func TestExportHistoryEmptyIsStillValid(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	csvData, count, err := s.ExportHistory(context.Background(), ExportOptions{Format: ExportCSV})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(csvData), "\ufeff"))).ReadAll()
	if err != nil || len(records) != 1 {
		t.Fatalf("empty CSV = %v (err %v)", records, err)
	}

	jsonData, _, err := s.ExportHistory(context.Background(), ExportOptions{Format: ExportJSON})
	if err != nil {
		t.Fatal(err)
	}
	var decoded []JobSummary
	if err := json.Unmarshal(jsonData, &decoded); err != nil {
		t.Fatalf("empty JSON invalid: %v", err)
	}
}
