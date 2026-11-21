//go:build windows

package printer

import (
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// SumatraPDF's -print-settings knows exactly four scaling tokens. The
// business front-end expresses its choice in actual/fill terms, and the Node
// agent it replaces printed both as "fit". fill must NOT become "stretch":
// stretch scales width and height independently, which distorts a shipping
// label — the one outcome that must never happen.
func TestSumatraScaleTokenMapping(t *testing.T) {
	cases := map[string]string{
		domain.ScaleActual:    "noscale",
		domain.ScaleFill:      "fit", // 按纸张比例 = keep aspect ratio, not stretch
		domain.ScaleFitWidth:  "fit",
		domain.ScaleFitHeight: "fit",
		"":                    "fit",
		"bogus":               "fit",
	}
	for mode, want := range cases {
		if got := sumatraScaleToken(mode); got != want {
			t.Errorf("sumatraScaleToken(%q) = %q, want %q", mode, got, want)
		}
	}
}

// Stretch must never be emitted for a label job: it is the one token that
// distorts by scaling the two axes independently. This test exists to stop a
// future "cleanup" from reintroducing the mapping.
func TestLabelScalingNeverStretches(t *testing.T) {
	for _, mode := range []string{"", domain.ScaleActual, domain.ScaleFill, domain.ScaleFitWidth, domain.ScaleFitHeight} {
		if got := sumatraScaleToken(mode); got == "stretch" {
			t.Errorf("sumatraScaleToken(%q) = stretch; a label must never be scaled per-axis", mode)
		}
	}
}

// Only the four documented tokens may ever reach the command line: an unknown
// token makes SumatraPDF abort the print ("ParseFlags"), so a typo here would
// take every label down with it.
func TestSumatraScaleTokenIsAlwaysOneOfFour(t *testing.T) {
	allowed := map[string]bool{"noscale": true, "shrink": true, "fit": true, "stretch": true}
	for _, mode := range []string{"", domain.ScaleActual, domain.ScaleFill, domain.ScaleFitWidth, domain.ScaleFitHeight, "bogus", "FIT"} {
		if got := sumatraScaleToken(mode); !allowed[got] {
			t.Errorf("sumatraScaleToken(%q) = %q, which is not a -print-settings scaling token", mode, got)
		}
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
