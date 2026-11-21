package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/printer"
	"github.com/shiuoei/print-bridge/internal/store"
)

// desktopTestBackend is a minimal printer backend so the pages can query the
// printer list without touching the real spooler.
type desktopTestBackend struct{}

func (desktopTestBackend) List(context.Context) ([]string, error) {
	return []string{"HPRT N41", "HP Laser MFP"}, nil
}
func (desktopTestBackend) Info(_ context.Context, name string) (*domain.PrinterInfo, error) {
	return &domain.PrinterInfo{
		Name: name,
		Papers: []domain.PrinterPaper{
			{ID: "256", Name: "A4", WidthMM: 210, HeightMM: 297},
			{ID: "276", Name: "100x180", WidthMM: 100, HeightMM: 180},
		},
	}, nil
}
func (desktopTestBackend) PrintPDF(context.Context, string, string, int, *domain.Paper) (printer.Submission, error) {
	return printer.Submission{Backend: "test"}, nil
}
func (desktopTestBackend) PrintRaw(context.Context, string, []byte) (printer.Submission, error) {
	return printer.Submission{Backend: "test"}, nil
}

// statusFor builds a status row with the history metadata the UI renders.
func statusFor(jobID, status, printer, paper string, copies int, batchIndex int) domain.Status {
	return domain.Status{
		JobID:       jobID,
		RequestID:   "REQ-" + jobID,
		Status:      status,
		Message:     status,
		Source:      domain.SourceWebSocket,
		PrinterName: printer,
		PaperName:   paper,
		Copies:      copies,
		At:          time.Now().UTC(),
	}
}

// newTestDesktop builds a desktop over a throwaway store without starting the
// agent, IPC listener or window. It is enough to render and drive the pages,
// which is what these tests need.
func newTestDesktop(t *testing.T) *desktop {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PRINTBRIDGE_DATA_DIR", dir)
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	d := &desktop{
		store:   s,
		service: command.Service{Store: s, Printer: desktopTestBackend{}},
		tab:     tabOverview,
	}
	// load copies the store's config, default language included, into d.
	d.load()
	return d
}

// textsOf renders one view pass and returns every text it drew.
func textsOf(t *testing.T, view func(*ui.Context)) []string {
	t.Helper()
	tester := ui.NewTester(view, 880, 680)
	tester.Frame()
	return tester.Texts()
}

// containsText reports whether any rendered string contains want.
func containsText(texts []string, want string) bool {
	for _, text := range texts {
		if strings.Contains(text, want) {
			return true
		}
	}
	return false
}

// TestSettingsPageShowsConfigTransferButtons verifies the configuration-transfer
// entry points are reachable from the UI, not only from the CLI.
func TestSettingsPageShowsConfigTransferButtons(t *testing.T) {
	d := newTestDesktop(t)
	texts := textsOf(t, func(c *ui.Context) { d.settings(c) })
	for _, want := range []string{
		d.tr("config_transfer"),
		d.tr("export_config"),
		d.tr("import_config"),
	} {
		if !containsText(texts, want) {
			t.Errorf("settings page does not render %q; got %v", want, texts)
		}
	}
}

// TestRemotePageShowsDeviceIdentity verifies the remote tab exposes device id,
// name, interval and retries — four fields that previously existed only in the
// CLI.
func TestRemotePageShowsDeviceIdentity(t *testing.T) {
	d := newTestDesktop(t)
	texts := textsOf(t, func(c *ui.Context) { d.remote(c) })
	for _, want := range []string{
		d.tr("remote_identity"),
		d.tr("device_id"),
		d.tr("device_name"),
		d.tr("poll_interval"),
		d.tr("max_report_retries"),
		d.tr("generate_device_id"),
	} {
		if !containsText(texts, want) {
			t.Errorf("remote page does not render %q; got %v", want, texts)
		}
	}
}

// TestHistoryPageShowsTableColumns verifies the history table renders its
// columns and the empty state rather than a flat list.
func TestHistoryPageShowsTableColumns(t *testing.T) {
	d := newTestDesktop(t)
	texts := textsOf(t, func(c *ui.Context) { d.historyPage(c) })
	for _, want := range []string{
		d.tr("col_time"),
		d.tr("col_status"),
		d.tr("col_source"),
		d.tr("col_job"),
		d.tr("col_printer"),
		d.tr("col_copies"),
		d.tr("clear_history"),
		d.tr("no_jobs"),
	} {
		if !containsText(texts, want) {
			t.Errorf("history page does not render %q; got %v", want, texts)
		}
	}
}

// TestHistoryPageRendersJobRow inserts one job and checks the table shows its
// values, including the printer/paper/copies recorded by the new history columns.
func TestHistoryPageRendersJobRow(t *testing.T) {
	d := newTestDesktop(t)
	if err := d.store.AddStatus(t.Context(), statusFor("JOB-UI-1", "submitted", "HPRT N41", "100x180mm", 3, 0)); err != nil {
		t.Fatal(err)
	}
	d.refreshHistory()
	if len(d.historyJobs) != 1 {
		t.Fatalf("history jobs = %d, want 1", len(d.historyJobs))
	}
	job := d.historyJobs[0]
	if job.PrinterName != "HPRT N41" || job.PaperName != "100x180mm" || job.Copies != 3 {
		t.Fatalf("history row lost metadata: %#v", job)
	}
	if job.Source == "" {
		t.Fatal("history row has no source")
	}
	if job.FinishedAt.IsZero() {
		t.Fatal("a terminal job has no finished time")
	}

	texts := textsOf(t, func(c *ui.Context) { d.historyPage(c) })
	for _, want := range []string{"JOB-UI-1", "HPRT N41", "100x180mm"} {
		if !containsText(texts, want) {
			t.Errorf("history table does not render %q; got %v", want, texts)
		}
	}
}

// TestHistoryFilterNarrowsResults checks the status filter actually filters.
func TestHistoryFilterNarrowsResults(t *testing.T) {
	d := newTestDesktop(t)
	if err := d.store.AddStatus(t.Context(), statusFor("ok-1", "submitted", "P", "A4", 1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := d.store.AddStatus(t.Context(), statusFor("bad-1", "failed", "P", "A4", 1, 0)); err != nil {
		t.Fatal(err)
	}
	d.historyFilter = d.tr("status_failed")
	d.refreshHistory()
	if len(d.historyJobs) != 1 || d.historyJobs[0].JobID != "bad-1" {
		t.Fatalf("filter did not narrow results: %#v", d.historyJobs)
	}
}

// TestExportModalRendersFields verifies the export dialog lists all eight fields
// and, when a token exists and the password is empty, the warning that protects it.
func TestExportModalRendersFields(t *testing.T) {
	d := newTestDesktop(t)
	// A token must exist for the empty-password warning to be relevant at all.
	c := d.store.Config()
	c.Remote.BearerToken = "existing-token"
	if err := d.store.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	d.initExportFields()
	d.openModal(modalExport)
	d.modal.password = ""
	texts := textsOf(t, func(c *ui.Context) { d.exportModal(c) })
	for _, name := range transferFieldOrder {
		want := d.tr(fieldLabelKey[name])
		if !containsText(texts, want) {
			t.Errorf("export dialog does not list %q; got %v", want, texts)
		}
	}
	if !containsText(texts, d.tr("export_empty_password_warning")) {
		t.Errorf("export dialog omits the empty-password warning; got %v", texts)
	}
}

// TestExportModalHidesWarningWithPassword keeps the warning from being noise when
// the operator did set a password.
func TestExportModalHidesWarningWithPassword(t *testing.T) {
	d := newTestDesktop(t)
	c := d.store.Config()
	c.Remote.BearerToken = "existing-token"
	if err := d.store.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	d.initExportFields()
	d.openModal(modalExport)
	d.modal.password = "a-real-password"
	texts := textsOf(t, func(c *ui.Context) { d.exportModal(c) })
	if containsText(texts, d.tr("export_empty_password_warning")) {
		t.Errorf("warning shown despite a password; got %v", texts)
	}
}

// TestExportFieldSelectionIsHonoured checks the checkboxes drive the export set.
func TestExportFieldSelectionIsHonoured(t *testing.T) {
	d := newTestDesktop(t)
	d.initExportFields()
	if got := len(d.selectedTransferFields()); got != len(transferFieldOrder) {
		t.Fatalf("default selection = %d fields, want %d", got, len(transferFieldOrder))
	}
	*d.exportFieldFlag("allowed-origins") = false
	*d.exportFieldFlag("allowed-ips") = false
	got := d.selectedTransferFields()
	for _, name := range got {
		if name == "allowed-origins" || name == "allowed-ips" {
			t.Fatalf("unticked field %q is still selected: %v", name, got)
		}
	}
	if len(got) != len(transferFieldOrder)-2 {
		t.Fatalf("selection = %v, want two fewer fields", got)
	}
}

// TestImportModalRendersPicker verifies the import dialog exposes the file picker
// and password field before any preview exists.
func TestImportModalRendersPicker(t *testing.T) {
	d := newTestDesktop(t)
	d.openModal(modalImport)
	texts := textsOf(t, func(c *ui.Context) { d.importModal(c) })
	for _, want := range []string{
		d.tr("import_title"),
		d.tr("import_file"),
		d.tr("choose_file"),
		d.tr("no_file_chosen"),
		d.tr("import_password"),
		d.tr("preview_import"),
	} {
		if !containsText(texts, want) {
			t.Errorf("import dialog does not render %q; got %v", want, texts)
		}
	}
}

// TestImportPreviewListsChanges checks the preview shows the diff it will apply.
func TestImportPreviewListsChanges(t *testing.T) {
	d := newTestDesktop(t)
	before := d.store.Config()
	after := before
	after.Service.Port = 19090
	after.Security.AllowedOrigins = []string{"https://new.example"}
	changes := diffForUI(before, after)
	if len(changes) != 2 {
		t.Fatalf("diff = %v, want 2 entries", changes)
	}
	if !strings.Contains(strings.Join(changes, "\n"), "service.port") {
		t.Fatalf("diff does not mention the port: %v", changes)
	}
}

// TestDiffForUIRedactsToken keeps the preview from printing a secret.
func TestDiffForUIRedactsToken(t *testing.T) {
	before := newTestDesktop(t).store.Config()
	after := before
	after.Remote.BearerToken = "super-secret-token"
	changes := strings.Join(diffForUI(before, after), "\n")
	if strings.Contains(changes, "super-secret-token") {
		t.Fatalf("diff leaked the token: %s", changes)
	}
	if !strings.Contains(changes, "remote.bearer_token") {
		t.Fatalf("diff does not mention the token field: %s", changes)
	}
}

// TestClearHistoryModalRendersConfirm keeps the destructive action behind an
// explicit confirmation.
func TestClearHistoryModalRendersConfirm(t *testing.T) {
	d := newTestDesktop(t)
	d.openModal(modalClearHistory)
	texts := textsOf(t, func(c *ui.Context) { d.clearHistoryModal(c) })
	for _, want := range []string{
		d.tr("clear_history_title"),
		d.tr("clear_history_body"),
		d.tr("clear_history_confirm"),
		d.tr("cancel"),
	} {
		if !containsText(texts, want) {
			t.Errorf("clear-history dialog does not render %q; got %v", want, texts)
		}
	}
}

// TestStatusAndSourceLabelsLocalize checks the label helpers fall back to the raw
// token instead of rendering an empty cell.
func TestStatusAndSourceLabelsLocalize(t *testing.T) {
	d := newTestDesktop(t)
	if got := d.statusLabel("submitted"); got == "submitted" {
		t.Errorf("statusLabel did not localize: %q", got)
	}
	if got := d.statusLabel("brand-new-status"); got != "brand-new-status" {
		t.Errorf("statusLabel(%q) = %q, want the raw token", "brand-new-status", got)
	}
	if got := d.sourceLabel("remote"); got == "remote" {
		t.Errorf("sourceLabel did not localize: %q", got)
	}
	if got := d.sourceLabel(""); got != "" {
		t.Errorf("sourceLabel(\"\") = %q, want empty", got)
	}
}

// TestCopiesLabelHandlesRawJobs keeps the copies column readable for raw jobs,
// which carry no copies value.
func TestCopiesLabelHandlesRawJobs(t *testing.T) {
	if got := copiesLabel(0); got != "-" {
		t.Errorf("copiesLabel(0) = %q, want -", got)
	}
	if got := copiesLabel(3); got != "3" {
		t.Errorf("copiesLabel(3) = %q, want 3", got)
	}
}
