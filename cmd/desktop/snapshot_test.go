package main

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/store"
)

// TestRenderPagesToPNG writes one PNG per page so the layout can be reviewed
// without launching the window. It is a manual inspection aid, skipped unless
// PRINTBRIDGE_UI_SNAPSHOTS names an output directory.
func TestRenderPagesToPNG(t *testing.T) {
	dir := os.Getenv("PRINTBRIDGE_UI_SNAPSHOTS")
	if dir == "" {
		t.Skip("set PRINTBRIDGE_UI_SNAPSHOTS to a directory to write page snapshots")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	d := newTestDesktop(t)
	// Seed history so the table and detail pane have content.
	for _, row := range []struct {
		id, status, printer, paper string
		copies                     int
	}{
		{"JOB-1001", "submitted", "HPRT N41", "100x180mm", 1},
		{"JOB-1002", "failed", "HP Laser MFP", "210x297mm", 2},
		{"JOB-1003", "completed", "HPRT N41", "100x180mm", 3},
	} {
		if err := d.store.AddStatus(t.Context(), statusFor(row.id, row.status, row.printer, row.paper, row.copies, 0)); err != nil {
			t.Fatal(err)
		}
	}
	d.refreshHistory()

	// Seed a dead letter so the remote tab shows the backlog panel populated.
	if err := d.store.AddRemoteEvent(t.Context(), store.RemoteEvent{
		EventID: "evt-snapshot", JobID: "JOB-1002", RequestID: "REQ-1002",
		Status: "success", Message: "submitted to system print queue",
		OccurredAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.FailRemoteEvent(t.Context(), "evt-snapshot", 10, "connection refused"); err != nil {
		t.Fatal(err)
	}
	d.refreshRemoteBacklog()

	pages := map[string]func(*ui.Context){
		"overview":    d.overview,
		"printers":    d.printing,
		"access":      d.security,
		"remote":      d.remote,
		"history":     d.historyPage,
		"diagnostics": d.diagnostics,
		"settings":    d.settings,
		"about":       d.aboutPage,
	}
	for name, page := range pages {
		tester := ui.NewTester(func(c *ui.Context) {
			ui.Column(c).Fill().Padding(24).Gap(16).Background(palette.canvas).Children(func() {
				page(c)
			})
		}, 880, 900)
		tester.Frame()
		writePNG(t, filepath.Join(dir, name+".png"), tester)
	}

	// Dialogs get their own snapshots.
	d.initExportFields()
	d.openModal(modalExport)
	writeModalPNG(t, filepath.Join(dir, "modal-export.png"), d, func(c *ui.Context) { d.exportModal(c) })
	d.closeModal()
	d.openModal(modalImport)
	writeModalPNG(t, filepath.Join(dir, "modal-import.png"), d, func(c *ui.Context) { d.importModal(c) })
	d.closeModal()
	d.openModal(modalClearHistory)
	writeModalPNG(t, filepath.Join(dir, "modal-clear-history.png"), d, func(c *ui.Context) { d.clearHistoryModal(c) })
	d.closeModal()
	d.openModal(modalExportHistory)
	writeModalPNG(t, filepath.Join(dir, "modal-export-history.png"), d, func(c *ui.Context) { d.exportHistoryModal(c) })
}

func writeModalPNG(t *testing.T, path string, d *desktop, view func(*ui.Context)) {
	t.Helper()
	tester := ui.NewTester(view, 880, 680)
	tester.Frame()
	writePNG(t, path, tester)
}

func writePNG(t *testing.T, path string, tester *ui.Tester) {
	t.Helper()
	img := tester.Image()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", path)
}

var _ = domain.SourceWebSocket
