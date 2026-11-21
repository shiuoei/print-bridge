package main

import (
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
)

// timeNowUTC is a short alias so the seed helpers read cleanly.
func timeNowUTC() time.Time { return time.Now().UTC() }

// commandRequestExport builds the request the export button sends.
func commandRequestExport(format, status, query string) command.Request {
	return command.Request{
		Command: "history.export",
		Format:  format,
		Status:  status,
		Query:   query,
		Limit:   50000,
	}
}

// seedHistoryJob inserts one job so the history page has a row to export.
func seedHistoryJob(t *testing.T, d *desktop, jobID, status string) {
	t.Helper()
	if err := d.store.AddStatus(t.Context(), domain.Status{
		JobID: jobID, RequestID: "REQ-" + jobID, Status: status, Message: status,
		Source: domain.SourceWebSocket, PrinterName: "HPRT N41", PaperName: "100x180mm",
		Copies: 2, At: timeNowUTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestHistoryPageShowsExportButton keeps the export reachable from the table.
func TestHistoryPageShowsExportButton(t *testing.T) {
	d := newTestDesktop(t)
	texts := textsOf(t, func(c *ui.Context) { d.historyPage(c) })
	if !containsText(texts, d.tr("export_history")) {
		t.Errorf("history page has no export button; got %v", texts)
	}
}

// TestExportHistoryModalRendersScopeAndFormats checks the dialog states what it will
// export and offers both formats.
func TestExportHistoryModalRendersScopeAndFormats(t *testing.T) {
	d := newTestDesktop(t)
	seedHistoryJob(t, d, "JOB-1", "submitted")
	d.refreshHistory()
	d.openModal(modalExportHistory)
	texts := textsOf(t, func(c *ui.Context) { d.exportHistoryModal(c) })
	for _, want := range []string{
		d.tr("export_history_title"),
		d.tr("export_history_body"),
		d.tr("export_format_csv"),
		d.tr("export_format_json"),
		d.tr("export_history_confirm"),
		d.tr("cancel"),
	} {
		if !containsText(texts, want) {
			t.Errorf("export dialog does not render %q; got %v", want, texts)
		}
	}
}

// TestExportHistoryScopeReflectsFilter keeps the dialog honest about the filter.
func TestExportHistoryScopeReflectsFilter(t *testing.T) {
	d := newTestDesktop(t)
	d.historyFilter = d.tr("status_failed")
	d.historyQuery = "INV"
	d.openModal(modalExportHistory)
	texts := textsOf(t, func(c *ui.Context) { d.exportHistoryModal(c) })
	joined := strings.Join(texts, " ")
	if !strings.Contains(joined, d.tr("status_failed")) {
		t.Errorf("the dialog does not mention the active status filter: %v", texts)
	}
	if !strings.Contains(joined, "INV") {
		t.Errorf("the dialog does not mention the active search: %v", texts)
	}
}

// TestExportHistoryProducesFileContent covers the service call the button makes,
// including that the format and filter reach the exporter.
func TestExportHistoryProducesFileContent(t *testing.T) {
	d := newTestDesktop(t)
	seedHistoryJob(t, d, "JOB-OK", "submitted")
	seedHistoryJob(t, d, "JOB-BAD", "failed")
	d.refreshHistory()

	r := d.service.Execute(t.Context(), commandRequestExport("csv", "failed", ""))
	if r.Error != "" {
		t.Fatalf("export failed: %s", r.Error)
	}
	if r.RowsCount != 1 {
		t.Fatalf("count = %d, want 1", r.RowsCount)
	}
	if !strings.Contains(string(r.Rows), "JOB-BAD") {
		t.Fatalf("export missing the filtered job: %s", r.Rows)
	}
	if strings.Contains(string(r.Rows), "JOB-OK") {
		t.Fatal("export ignored the status filter")
	}
}

// TestExportFormatDefaultsToCSV keeps an untouched dialog exporting something valid.
func TestExportFormatDefaultsToCSV(t *testing.T) {
	d := newTestDesktop(t)
	if d.exportHistoryFormat != "" {
		t.Fatalf("initial format = %q, want empty (resolved to csv at export)", d.exportHistoryFormat)
	}
	seedHistoryJob(t, d, "JOB-1", "submitted")
	r := d.service.Execute(t.Context(), commandRequestExport("", "", ""))
	if r.Error != "" {
		t.Fatalf("export failed: %s", r.Error)
	}
	if r.Value != "csv" {
		t.Fatalf("format = %q, want csv", r.Value)
	}
}

// TestExportHistoryScopeReadsCleanly checks an unset filter reads as "any"/"none"
// rather than borrowing the status label.
func TestExportHistoryScopeReadsCleanly(t *testing.T) {
	d := newTestDesktop(t)
	seedHistoryJob(t, d, "JOB-1", "submitted")
	d.refreshHistory()
	d.historyFilter = d.tr("filter_any")
	d.historyQuery = ""
	d.openModal(modalExportHistory)
	texts := textsOf(t, func(c *ui.Context) { d.exportHistoryModal(c) })
	joined := strings.Join(texts, " ")
	if strings.Contains(joined, "search: "+d.tr("filter_any")) {
		t.Errorf("the scope line reused the status label for the search field: %v", texts)
	}
	if !strings.Contains(joined, d.tr("filter_any_query")) {
		t.Errorf("the scope line does not state the empty search: %v", texts)
	}
}
