package main

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
)

// TestRenderDoctorReportToPNG renders the diagnostics page with a completed doctor
// run so the grouped layout, counters and suggestions can be reviewed. Skipped
// unless PRINTBRIDGE_UI_SNAPSHOTS names an output directory.
func TestRenderDoctorReportToPNG(t *testing.T) {
	dir := os.Getenv("PRINTBRIDGE_UI_SNAPSHOTS")
	if dir == "" {
		t.Skip("set PRINTBRIDGE_UI_SNAPSHOTS to a directory to write page snapshots")
	}
	d := newTestDesktop(t)
	d.runDoctor()
	if d.doctor == nil {
		t.Skip("doctor produced no report in this environment")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(func(c *ui.Context) {
		ui.Column(c).Fill().Padding(24).Gap(16).Background(palette.canvas).Children(func() {
			d.doctorPage(c)
		})
	}, 880, 1100)
	tester.Frame()
	path := filepath.Join(dir, "diagnostics-run.png")
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

// TestRenderDarkThemeToPNG renders the overview in the dark theme so the palette
// can be checked. Skipped unless PRINTBRIDGE_UI_SNAPSHOTS is set.
func TestRenderDarkThemeToPNG(t *testing.T) {
	dir := os.Getenv("PRINTBRIDGE_UI_SNAPSHOTS")
	if dir == "" {
		t.Skip("set PRINTBRIDGE_UI_SNAPSHOTS to a directory to write page snapshots")
	}
	d := newTestDesktop(t)
	d.theme = themeDark
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tester := ui.NewTester(d.view, 880, 900)
	tester.Frame()
	path := filepath.Join(dir, "overview-dark.png")
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
