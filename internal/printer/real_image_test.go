package printer_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/convert"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/printer"
)

// This hardware-gated test submits exactly one converted image to the named
// system queue. A nil return proves spool submission, not physical output.
func TestRealImagePrintSubmission(t *testing.T) {
	name := os.Getenv("PRINTBRIDGE_REAL_PRINTER")
	source := os.Getenv("PRINTBRIDGE_MATRIX_IMAGE_FILE")
	width := os.Getenv("PRINTBRIDGE_MATRIX_PAPER_WIDTH_MM")
	height := os.Getenv("PRINTBRIDGE_MATRIX_PAPER_HEIGHT_MM")
	if name == "" || source == "" || width == "" || height == "" {
		t.Skip("set PRINTBRIDGE_REAL_PRINTER, PRINTBRIDGE_MATRIX_IMAGE_FILE and paper width/height to submit one image")
	}
	w, err := strconv.ParseFloat(width, 64)
	if err != nil {
		t.Fatal(err)
	}
	h, err := strconv.ParseFloat(height, 64)
	if err != nil {
		t.Fatal(err)
	}
	paper := &domain.Paper{WidthMM: w, HeightMM: h}
	if !paper.Valid() {
		t.Fatalf("invalid paper dimensions %.1f x %.1f mm", w, h)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal(err)
	}
	pdf := filepath.Join(t.TempDir(), "image.pdf")
	if err := convert.ImagePDF(source, pdf, paper); err != nil {
		t.Fatal(err)
	}
	if err := convert.VerifyPDF(pdf); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	if _, err := printer.New().PrintPDF(ctx, pdf, name, 1, paper); err != nil {
		t.Fatal(err)
	}
	t.Logf("image submitted to %q with %.1f x %.1f mm paper in %s; physical output unverified", name, w, h, time.Since(started).Round(time.Millisecond))
}
