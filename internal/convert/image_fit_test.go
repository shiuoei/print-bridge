package convert

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// writeTestPNG writes a solid-colour PNG of the given pixel size and returns
// its path. (Named to avoid colliding with the helper in errors_test.go.)
func writeTestPNG(t *testing.T, dir string, w, h int) string {
	t.Helper()
	path := filepath.Join(dir, "src.png")
	if err := writePNG(path, w, h); err != nil {
		t.Fatal(err)
	}
	return path
}

// placement parses the `cm` transform from a generated PDF content stream.
func placement(t *testing.T, pdfPath string) (scaleX, scaleY, tx, ty float64) {
	t.Helper()
	data, err := os.ReadFile(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`q ([0-9.]+) 0 0 ([0-9.]+) ([0-9.]+) ([0-9.]+) cm /Im0 Do Q`)
	m := re.FindSubmatch(data)
	if m == nil {
		t.Fatalf("no placement transform found in %s", pdfPath)
	}
	vals := make([]float64, 4)
	for i := 0; i < 4; i++ {
		v, err := strconv.ParseFloat(string(m[i+1]), 64)
		if err != nil {
			t.Fatal(err)
		}
		vals[i] = v
	}
	return vals[0], vals[1], vals[2], vals[3]
}

// mediaBox parses the page MediaBox from a generated PDF.
func mediaBox(t *testing.T, pdfPath string) (w, h float64) {
	t.Helper()
	data, err := os.ReadFile(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`/MediaBox \[0 0 ([0-9.]+) ([0-9.]+)\]`)
	m := re.FindSubmatch(data)
	if m == nil {
		t.Fatalf("no MediaBox found in %s", pdfPath)
	}
	w, _ = strconv.ParseFloat(string(m[1]), 64)
	h, _ = strconv.ParseFloat(string(m[2]), 64)
	return w, h
}

// TestImagePDFPreservesAspectRatio is the regression guard for the defect where
// every image was stretched to fill the whole page. A 4:3 image on a 100x180
// label must be letterboxed, not skewed.
func TestImagePDFPreservesAspectRatio(t *testing.T) {
	dir := t.TempDir()
	src := writeTestPNG(t, dir, 400, 300) // 4:3
	dest := filepath.Join(dir, "out.pdf")
	paper := &domain.Paper{WidthMM: 100, HeightMM: 180}

	if err := ImagePDF(src, dest, paper); err != nil {
		t.Fatalf("ImagePDF: %v", err)
	}

	pageW, pageH := mediaBox(t, dest)
	if math.Abs(pageW-mmToPt(100)) > 0.01 || math.Abs(pageH-mmToPt(180)) > 0.01 {
		t.Fatalf("MediaBox = %.2fx%.2f pt, want %.2fx%.2f", pageW, pageH, mmToPt(100), mmToPt(180))
	}

	sx, sy, tx, ty := placement(t, dest)
	// Contain: the 4:3 image is limited by the 100 mm page width.
	wantW := mmToPt(100)
	wantH := wantW * 300.0 / 400.0
	if math.Abs(sx-wantW) > 0.05 || math.Abs(sy-wantH) > 0.05 {
		t.Fatalf("image size = %.3fx%.3f pt, want %.3fx%.3f (aspect must be preserved)", sx, sy, wantW, wantH)
	}
	// Centred vertically in the 180 mm page.
	if math.Abs(ty-(pageH-wantH)/2) > 0.05 {
		t.Fatalf("translate y = %.3f, want %.3f", ty, (pageH-wantH)/2)
	}
	if math.Abs(tx) > 0.05 {
		t.Fatalf("translate x = %.3f, want 0 (horizontally filled)", tx)
	}
	// The image must not be skewed: both axes share one scale factor.
	if math.Abs(sx/sy-400.0/300.0) > 0.01 {
		t.Fatalf("scale ratio %.4f, want 400/300 (image was distorted)", sx/sy)
	}
}

// TestImagePDFTallImageLetterboxesHorizontally covers the other orientation.
func TestImagePDFTallImageLetterboxesHorizontally(t *testing.T) {
	dir := t.TempDir()
	src := writeTestPNG(t, dir, 300, 900) // 1:3
	dest := filepath.Join(dir, "out.pdf")
	paper := &domain.Paper{WidthMM: 100, HeightMM: 180}

	if err := ImagePDF(src, dest, paper); err != nil {
		t.Fatalf("ImagePDF: %v", err)
	}
	sx, sy, tx, ty := placement(t, dest)
	wantH := mmToPt(180)
	wantW := wantH * 300.0 / 900.0
	if math.Abs(sy-wantH) > 0.05 || math.Abs(sx-wantW) > 0.05 {
		t.Fatalf("image size = %.3fx%.3f pt, want %.3fx%.3f", sx, sy, wantW, wantH)
	}
	pageW, _ := mediaBox(t, dest)
	if math.Abs(tx-(pageW-wantW)/2) > 0.05 {
		t.Fatalf("translate x = %.3f, want %.3f", tx, (pageW-wantW)/2)
	}
	if math.Abs(ty) > 0.05 {
		t.Fatalf("translate y = %.3f, want 0", ty)
	}
}

// TestImagePDFWithoutPaperUsesLabelDPI keeps a bare image job at its intended
// physical size instead of an arbitrary 96 dpi size.
func TestImagePDFWithoutPaperUsesLabelDPI(t *testing.T) {
	dir := t.TempDir()
	src := writeTestPNG(t, dir, 203, 406) // exactly 1x2 inch at 203 dpi
	dest := filepath.Join(dir, "out.pdf")

	if err := ImagePDF(src, dest, nil); err != nil {
		t.Fatalf("ImagePDF: %v", err)
	}
	pageW, pageH := mediaBox(t, dest)
	wantW, wantH := mmToPt(25.4), mmToPt(50.8)
	if math.Abs(pageW-wantW) > 0.05 || math.Abs(pageH-wantH) > 0.05 {
		t.Fatalf("MediaBox = %.2fx%.2f pt, want %.2fx%.2f (1x2 inch at 203 dpi)", pageW, pageH, wantW, wantH)
	}
}

// TestFitContainRejectsInvalidInput keeps NaN/Inf/zero from producing a broken
// PDF instead of a classified error.
func TestFitContainRejectsInvalidInput(t *testing.T) {
	cases := []struct{ iw, ih, pw, ph float64 }{
		{0, 10, 100, 100},
		{10, 0, 100, 100},
		{10, 10, 0, 100},
		{10, 10, 100, 0},
		{math.NaN(), 10, 100, 100},
		{10, math.Inf(1), 100, 100},
		{-5, 10, 100, 100},
	}
	for _, c := range cases {
		if _, ok := fitContain(c.iw, c.ih, c.pw, c.ph); ok {
			t.Fatalf("fitContain(%v,%v,%v,%v) accepted invalid input", c.iw, c.ih, c.pw, c.ph)
		}
	}
}

// TestFitContainCentres checks the arithmetic directly.
func TestFitContainCentres(t *testing.T) {
	// 2:1 image on a square page: full width, half height, centred.
	got, ok := fitContain(200, 100, 100, 100)
	if !ok {
		t.Fatal("valid input rejected")
	}
	if got.width != 100 || got.height != 50 {
		t.Fatalf("size = %.2fx%.2f, want 100x50", got.width, got.height)
	}
	if got.x != 0 || got.y != 25 {
		t.Fatalf("origin = (%.2f,%.2f), want (0,25)", got.x, got.y)
	}
}
