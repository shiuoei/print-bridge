package printer

import (
	"os"
	"strings"
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
)

func papers(list ...domain.PrinterPaper) []domain.PrinterPaper { return list }

func paper(w, h float64, name string) domain.PrinterPaper {
	return domain.PrinterPaper{Name: name, WidthMM: w, HeightMM: h}
}

// TestResolvePaperNameMatchesExact covers the common case: the caller asks for a
// size the driver advertises and gets that driver's own paper name back.
func TestResolvePaperNameMatchesExact(t *testing.T) {
	available := papers(
		paper(76, 130, "76mm * 130mm"),
		paper(100, 180, "菜鸟标准 100mm * 180mm"),
		paper(210, 297, "A4"),
	)
	cases := []struct {
		want   domain.Paper
		expect string
	}{
		{domain.Paper{WidthMM: 100, HeightMM: 180}, "菜鸟标准 100mm * 180mm"},
		{domain.Paper{WidthMM: 210, HeightMM: 297}, "A4"},
		{domain.Paper{WidthMM: 76, HeightMM: 130}, "76mm * 130mm"},
	}
	for _, tc := range cases {
		got, ok := resolvePaperName(available, &tc.want)
		if !ok || got != tc.expect {
			t.Errorf("%gx%g: got (%q,%v), want %q", tc.want.WidthMM, tc.want.HeightMM, got, ok, tc.expect)
		}
	}
}

// TestResolvePaperNameRoundsToNearestStock proves a round request resolves to the
// closest advertised stock, which is how label printers describe their rolls.
func TestResolvePaperNameRoundsToNearestStock(t *testing.T) {
	available := papers(paper(76, 130, "76mm * 130mm"))
	got, ok := resolvePaperName(available, &domain.Paper{WidthMM: 76.2, HeightMM: 130.2})
	if !ok || got != "76mm * 130mm" {
		t.Fatalf("got (%q,%v)", got, ok)
	}
}

// TestResolvePaperNameAcceptsRotation proves a landscape request matches a
// portrait driver paper: stock selection is orientation-independent.
func TestResolvePaperNameAcceptsRotation(t *testing.T) {
	available := papers(paper(100, 180, "label 100x180"))
	got, ok := resolvePaperName(available, &domain.Paper{WidthMM: 180, HeightMM: 100})
	if !ok || got != "label 100x180" {
		t.Fatalf("rotated request: got (%q,%v)", got, ok)
	}
}

// TestResolvePaperNameRejectsUnknown proves an unavailable size is reported rather
// than silently printing on the device default. This is the bug that produced
// 76x130 labels when 100x180 was requested.
func TestResolvePaperNameRejectsUnknown(t *testing.T) {
	available := papers(paper(76, 130, "76mm * 130mm"))
	if got, ok := resolvePaperName(available, &domain.Paper{WidthMM: 100, HeightMM: 180}); ok {
		t.Fatalf("accepted an unavailable size as %q", got)
	}
}

// TestResolvePaperNameSkipsCommaNames proves names SumatraPDF cannot express are
// not selected, because -print-settings is comma separated.
func TestResolvePaperNameSkipsCommaNames(t *testing.T) {
	available := papers(
		paper(100, 180, "label, 100x180"),
		paper(100, 180, "label 100x180"),
	)
	got, ok := resolvePaperName(available, &domain.Paper{WidthMM: 100, HeightMM: 180})
	if !ok || got != "label 100x180" {
		t.Fatalf("got (%q,%v), want the comma-free name", got, ok)
	}
}

// TestResolvePaperNameHandlesEmptyInput guards the degenerate cases.
func TestResolvePaperNameHandlesEmptyInput(t *testing.T) {
	if _, ok := resolvePaperName(nil, &domain.Paper{WidthMM: 100, HeightMM: 180}); ok {
		t.Fatal("matched against no papers")
	}
	if _, ok := resolvePaperName(papers(paper(100, 180, "x")), nil); ok {
		t.Fatal("matched a nil request")
	}
	if _, ok := resolvePaperName(papers(paper(0, 0, "zero")), &domain.Paper{WidthMM: 100, HeightMM: 180}); ok {
		t.Fatal("matched a zero-size paper")
	}
}

// TestResolvePaperNamePrefersClosest proves ties on one dimension pick the paper
// with the smaller total error.
func TestResolvePaperNamePrefersClosest(t *testing.T) {
	available := papers(
		paper(100, 181, "far"),
		paper(100, 180, "near"),
	)
	got, _ := resolvePaperName(available, &domain.Paper{WidthMM: 100, HeightMM: 180})
	if got != "near" {
		t.Fatalf("got %q, want nearest", got)
	}
}

// TestRealPrinterResolvesRequestedPaper is the hardware-gated check that a real
// driver's advertised papers actually resolve. It does not print: it only reads
// capabilities, so it is safe to run repeatedly.
//
//	PRINTBRIDGE_REAL_PRINTER="HPRT N41" go test ./internal/printer -run RealPrinterResolves -v
func TestRealPrinterResolvesRequestedPaper(t *testing.T) {
	name := strings.TrimSpace(os.Getenv("PRINTBRIDGE_REAL_PRINTER"))
	if name == "" {
		t.Skip("set PRINTBRIDGE_REAL_PRINTER to a real queue name")
	}
	info, err := New().Info(t.Context(), name)
	if err != nil {
		t.Fatalf("Info(%q): %v", name, err)
	}
	if len(info.Papers) == 0 {
		t.Skipf("%s advertises no paper sizes", name)
	}
	// Round-trip every advertised paper through the resolver. Name identity cannot
	// be required: real label drivers advertise many papers with the same
	// dimensions under different names (HPRT lists dozens of 100x180 courier
	// labels), and any of them prints at that physical size. The invariant is that
	// the resolved name denotes the same dimensions.
	byName := map[string]domain.PrinterPaper{}
	for _, p := range info.Papers {
		byName[p.Name] = p
	}
	for _, p := range info.Papers {
		if p.Name == "" || strings.ContainsAny(p.Name, ",\r\n") {
			continue
		}
		want := &domain.Paper{WidthMM: p.WidthMM, HeightMM: p.HeightMM}
		got, ok := resolvePaperName(info.Papers, want)
		if !ok {
			t.Errorf("paper %q (%gx%g) was not resolvable", p.Name, p.WidthMM, p.HeightMM)
			continue
		}
		resolved, known := byName[got]
		if !known {
			t.Errorf("resolver returned unknown paper %q", got)
			continue
		}
		if !sameDims(resolved, want) {
			t.Errorf("paper %q (%gx%g) resolved to %q (%gx%g)",
				p.Name, p.WidthMM, p.HeightMM, resolved.Name, resolved.WidthMM, resolved.HeightMM)
		}
	}
	// A size the driver does not advertise must be refused.
	if _, ok := resolvePaperName(info.Papers, &domain.Paper{WidthMM: 999, HeightMM: 999}); ok {
		t.Error("resolver accepted a size the driver does not advertise")
	}
	t.Logf("%s: resolved %d advertised papers", name, len(info.Papers))
}

// sameDims reports whether two papers describe the same physical stock, allowing
// the 180x100 / 100x180 rotation.
func sameDims(p domain.PrinterPaper, want *domain.Paper) bool {
	directW := absDiff(p.WidthMM, want.WidthMM)
	directH := absDiff(p.HeightMM, want.HeightMM)
	if directW <= paperMatchToleranceMM && directH <= paperMatchToleranceMM {
		return true
	}
	return absDiff(p.WidthMM, want.HeightMM) <= paperMatchToleranceMM &&
		absDiff(p.HeightMM, want.WidthMM) <= paperMatchToleranceMM
}
