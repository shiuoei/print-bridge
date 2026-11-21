//go:build windows

package printer

import (
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// SumatraPDF 3.6.1's -print-settings knows three scaling tokens (noscale,
// shrink, fit) and has no centring token at all. The business front-end expresses
// its choice in actual/fill terms, and the Node agent it replaces printed both as
// "fit". fill must NOT become "stretch": SumatraPDF does not even accept stretch,
// and per-axis scaling distorts a shipping label — the one outcome that must
// never happen.
func TestSumatraScaleTokenMapping(t *testing.T) {
	cases := []struct {
		mode   string
		center bool
		want   string
	}{
		{domain.ScaleActual, false, "noscale"},
		{domain.ScaleActual, true, "shrink"}, // centred, never enlarged
		{domain.ScaleFill, false, "fit"},     // 按纸张比例 = keep aspect ratio, not stretch
		{domain.ScaleFill, true, "fit"},
		{domain.ScaleFitWidth, false, "fit"},
		{domain.ScaleFitHeight, true, "fit"},
		{"", false, "fit"},
		{"", true, "fit"},
		{"bogus", false, "fit"},
		{"bogus", true, "fit"},
	}
	for _, c := range cases {
		if got := sumatraScaleToken(c.mode, c.center); got != c.want {
			t.Errorf("sumatraScaleToken(%q, %t) = %q, want %q", c.mode, c.center, got, c.want)
		}
	}
}

// Stretch must never be emitted for a label job: it is the one token that
// distorts by scaling the two axes independently (and SumatraPDF does not accept
// it). This test exists to stop a future "cleanup" from reintroducing the mapping.
func TestLabelScalingNeverStretches(t *testing.T) {
	for _, center := range []bool{false, true} {
		for _, mode := range []string{"", domain.ScaleActual, domain.ScaleFill, domain.ScaleFitWidth, domain.ScaleFitHeight} {
			if got := sumatraScaleToken(mode, center); got == "stretch" {
				t.Errorf("sumatraScaleToken(%q, %t) = stretch; a label must never be scaled per-axis", mode, center)
			}
		}
	}
}

// Only tokens SumatraPDF actually parses may reach the command line. Unknown
// tokens are silently ignored (ApplyPrintSettings has no error branch), so a typo
// would print with the wrong layout instead of failing loudly — worth a guard.
func TestSumatraScaleTokenIsAlwaysSupported(t *testing.T) {
	allowed := map[string]bool{"noscale": true, "shrink": true, "fit": true}
	for _, center := range []bool{false, true} {
		for _, mode := range []string{"", domain.ScaleActual, domain.ScaleFill, domain.ScaleFitWidth, domain.ScaleFitHeight, "bogus", "FIT"} {
			if got := sumatraScaleToken(mode, center); !allowed[got] {
				t.Errorf("sumatraScaleToken(%q, %t) = %q, which is not a -print-settings scaling token", mode, center, got)
			}
		}
	}
}

// Centring an actual-size job must reach SumatraPDF as shrink: noscale is
// top-left by design, so "center" would otherwise be a no-op (it is not a
// -print-settings token).
func TestActualSizeCentringSelectsShrink(t *testing.T) {
	if got := sumatraScaleToken(domain.ScaleActual, true); got != "shrink" {
		t.Fatalf("actual+center = %q, want shrink", got)
	}
	if got := sumatraScaleToken(domain.ScaleActual, false); got != "noscale" {
		t.Fatalf("actual+no-center = %q, want noscale", got)
	}
}

// An explicit direction wins over the paper's aspect ratio; "auto" and empty
// keep deriving it from the paper.
func TestPaperOrientationForDirection(t *testing.T) {
	portraitPaper := &domain.Paper{WidthMM: 100, HeightMM: 150}
	landscapePaper := &domain.Paper{WidthMM: 150, HeightMM: 100}
	squarePaper := &domain.Paper{WidthMM: 100, HeightMM: 100}

	cases := []struct {
		paper  *domain.Paper
		dir    string
		expect string
	}{
		{portraitPaper, "", "portrait"},
		{landscapePaper, "", "landscape"},
		{squarePaper, "", "portrait"},
		{portraitPaper, "landscape", "landscape"},
		{landscapePaper, "portrait", "portrait"},
		{portraitPaper, "auto", "portrait"},
		{landscapePaper, "auto", "landscape"},
		{nil, "landscape", "landscape"},
		{nil, "", "portrait"},
	}
	for _, c := range cases {
		if got := paperOrientationFor(c.paper, c.dir); got != c.expect {
			t.Errorf("paperOrientationFor(%v, %q) = %q, want %q", c.paper, c.dir, got, c.expect)
		}
	}
}
