package main

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/store"
)

// TestRenderDeadLettersToPNG renders the dead-letter panel with entries so the
// backlog layout can be reviewed. Skipped unless PRINTBRIDGE_UI_SNAPSHOTS is set.
func TestRenderDeadLettersToPNG(t *testing.T) {
	dir := os.Getenv("PRINTBRIDGE_UI_SNAPSHOTS")
	if dir == "" {
		t.Skip("set PRINTBRIDGE_UI_SNAPSHOTS to a directory to write page snapshots")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	d := newTestDesktop(t)
	ctx := t.Context()
	for _, e := range []struct{ id, job, err string }{
		{"evt-1", "JOB-1001", "connection refused"},
		{"evt-2", "JOB-1002", "HTTP 503"},
	} {
		if err := d.store.AddRemoteEvent(ctx, store.RemoteEvent{
			EventID: e.id, JobID: e.job, RequestID: "REQ", Status: "success",
			OccurredAt: time.Now().UTC().Format(time.RFC3339Nano),
		}); err != nil {
			t.Fatal(err)
		}
		if err := d.store.FailRemoteEvent(ctx, e.id, 10, e.err); err != nil {
			t.Fatal(err)
		}
	}
	d.refreshRemoteBacklog()

	tester := ui.NewTester(func(c *ui.Context) {
		ui.Column(c).Fill().Padding(24).Gap(16).Background(palette.canvas).Children(func() {
			d.deadLettersPanel(c)
		})
	}, 880, 700)
	tester.Frame()
	path := filepath.Join(dir, "dead-letters.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, tester.Image()); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", path)
}
