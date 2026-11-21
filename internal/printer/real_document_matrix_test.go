package printer_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/convert"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/printer"
)

var pdfPageCount = regexp.MustCompile(`(?m)^Pages:\s+(\d+)\s*$`)

// Explicitly gated: this sends four one-page jobs to a real A4 queue.
func TestRealOfficeAndHTMLPrintSubmission(t *testing.T) {
	if os.Getenv("PRINTBRIDGE_MATRIX_SEND_FOUR") != "1" {
		t.Skip("set PRINTBRIDGE_MATRIX_SEND_FOUR=1 to submit four real pages")
	}
	name := os.Getenv("PRINTBRIDGE_REAL_PRINTER")
	root := os.Getenv("PRINTBRIDGE_MATRIX_OFFICE_DIR")
	if name == "" || root == "" {
		t.Fatal("set PRINTBRIDGE_REAL_PRINTER and PRINTBRIDGE_MATRIX_OFFICE_DIR")
	}
	if _, err := exec.LookPath("pdfinfo"); err != nil {
		t.Fatal("pdfinfo is required to enforce the four-page limit")
	}
	paper := &domain.Paper{WidthMM: 210, HeightMM: 297}
	p := printer.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	info, err := p.Info(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	supportsA4 := false
	for _, size := range info.Papers {
		if size.WidthMM == 210 && size.HeightMM == 297 {
			supportsA4 = true
		}
	}
	if !supportsA4 {
		t.Fatalf("%q does not advertise A4", name)
	}
	for _, format := range []string{"docx", "xlsx", "pptx", "raw-html"} {
		t.Run(format, func(t *testing.T) {
			var pdf string
			var err error
			work := t.TempDir()
			if format == "raw-html" {
				job := domain.Job{JobID: "real-html", Format: "raw-html", HTML: "<!doctype html><html><body><h1>PrintBridge HTML test</h1><p>One A4 page.</p></body></html>", Paper: paper}
				pdf, _, err = convert.RenderHTML(ctx, job, domain.DefaultConfig(), work, "")
			} else {
				source := filepath.Join(root, format, "healthy-basic."+format)
				pdf, _, err = convert.OfficePDF(ctx, source, format, work)
			}
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.CommandContext(ctx, "pdfinfo", pdf).Output()
			if err != nil {
				t.Fatalf("pdfinfo: %v", err)
			}
			match := pdfPageCount.FindSubmatch(out)
			if len(match) != 2 {
				t.Fatalf("cannot read page count: %s", out)
			}
			pages, _ := strconv.Atoi(string(match[1]))
			if pages != 1 {
				t.Fatalf("%s produces %d pages; expected exactly 1", format, pages)
			}
			printCtx, stop := context.WithTimeout(ctx, 30*time.Second)
			defer stop()
			started := time.Now()
			if _, err := p.PrintPDF(printCtx, pdf, name, 1, paper); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: 1 A4 page submitted to %q in %s; physical output requires inspection", format, name, time.Since(started).Round(time.Millisecond))
		})
	}
}

// Explicitly gated: this submits one URL HTML page to a real A4 printer.
func TestRealURLHTMLPrintSubmission(t *testing.T) {
	if os.Getenv("PRINTBRIDGE_MATRIX_SEND_URL_HTML") != "1" {
		t.Skip("set PRINTBRIDGE_MATRIX_SEND_URL_HTML=1 to submit one real A4 page")
	}
	name := os.Getenv("PRINTBRIDGE_REAL_PRINTER")
	if name == "" {
		t.Fatal("set PRINTBRIDGE_REAL_PRINTER")
	}
	if _, err := exec.LookPath("pdfinfo"); err != nil {
		t.Fatal("pdfinfo is required to enforce the one-page limit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	p := printer.New()
	info, err := p.Info(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	paper := &domain.Paper{WidthMM: 210, HeightMM: 297}
	supportsA4 := false
	for _, size := range info.Papers {
		if size.WidthMM == paper.WidthMM && size.HeightMM == paper.HeightMM {
			supportsA4 = true
		}
	}
	if !supportsA4 {
		t.Fatalf("%q does not advertise A4", name)
	}
	job := domain.Job{JobID: "real-url-html", Format: "html", FileURL: "https://example.com/", Paper: paper}
	pdf, _, err := convert.RenderHTML(ctx, job, domain.DefaultConfig(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(ctx, "pdfinfo", pdf).Output()
	if err != nil {
		t.Fatalf("pdfinfo: %v", err)
	}
	match := pdfPageCount.FindSubmatch(out)
	if len(match) != 2 {
		t.Fatalf("cannot read page count: %s", out)
	}
	pages, _ := strconv.Atoi(string(match[1]))
	if pages != 1 {
		t.Fatalf("URL HTML produces %d pages; expected exactly 1", pages)
	}
	if _, err := p.PrintPDF(ctx, pdf, name, 1, paper); err != nil {
		t.Fatal(err)
	}
	t.Logf("URL HTML: 1 A4 page submitted to %q; physical output requires inspection", name)
}
