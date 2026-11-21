package convert

import (
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// Business front-ends submit label bytes they already hold as
// data_base64 instead of hosting them first. These tests prove the conversion
// path treats them exactly like a downloaded file, including the checks that
// stop a wrong payload from reaching a printer.
func TestInlinePDFIsVerifiedAndWritten(t *testing.T) {
	dir := t.TempDir()
	cfg := domain.DefaultConfig()
	pdf := "%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n"
	job := domain.Job{
		JobID: "inline-pdf", Format: "pdf",
		DataBase64: base64.StdEncoding.EncodeToString([]byte(pdf)),
	}
	path, raw, err := Prepare(context.Background(), job, cfg, dir, "")
	if err != nil {
		t.Fatalf("inline pdf: %v", err)
	}
	if raw != nil || path == "" {
		t.Fatalf("path=%q raw=%v", path, raw)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != pdf {
		t.Fatalf("written bytes differ: %q", got)
	}
}

// A payload that is not a PDF must be rejected as FORMAT_MISMATCH, exactly as a
// downloaded one would be: this is what stops a mislabelled label from being
// submitted to the queue.
func TestInlinePDFThatIsNotAPDFIsRejected(t *testing.T) {
	dir := t.TempDir()
	cfg := domain.DefaultConfig()
	job := domain.Job{
		JobID: "bad-pdf", Format: "pdf",
		DataBase64: base64.StdEncoding.EncodeToString([]byte("definitely not a pdf")),
	}
	_, _, err := Prepare(context.Background(), job, cfg, dir, "")
	if err == nil {
		t.Fatal("a non-PDF inline payload was accepted")
	}
	if code := domain.CodeOf(err); code != domain.CodeFormatMismatch {
		t.Fatalf("code = %q, want %q", code, domain.CodeFormatMismatch)
	}
}

func TestInlineInvalidBase64IsRejected(t *testing.T) {
	dir := t.TempDir()
	cfg := domain.DefaultConfig()
	job := domain.Job{JobID: "bad-b64", Format: "pdf", DataBase64: "!!!not base64!!!"}
	_, _, err := Prepare(context.Background(), job, cfg, dir, "")
	if err == nil {
		t.Fatal("invalid base64 was accepted")
	}
	if code := domain.CodeOf(err); code != domain.CodeInvalidMessage {
		t.Fatalf("code = %q, want %q", code, domain.CodeInvalidMessage)
	}
}

// The limit applies to the decoded bytes: base64 inflates by a third, so a limit
// checked on the encoded string would admit an oversized file.
func TestInlinePayloadHonoursSizeLimit(t *testing.T) {
	dir := t.TempDir()
	cfg := domain.DefaultConfig()
	cfg.Limits.MaxFileSizeMB = 1
	payload := make([]byte, (1<<20)+1024) // just over 1 MiB decoded
	copy(payload, "%PDF-")
	job := domain.Job{
		JobID: "big", Format: "pdf",
		DataBase64: base64.StdEncoding.EncodeToString(payload),
	}
	_, _, err := Prepare(context.Background(), job, cfg, dir, "")
	if err == nil {
		t.Fatal("an oversized inline payload was accepted")
	}
	if code := domain.CodeOf(err); code != domain.CodeFileTooLarge {
		t.Fatalf("code = %q, want %q", code, domain.CodeFileTooLarge)
	}
}

// An inline image goes through the same decoder as a downloaded one.
func TestInlineImageBecomesPDF(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.png")
	if err := writeINeedPNG(src, 120, 80); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	cfg := domain.DefaultConfig()
	job := domain.Job{
		JobID: "inline-image", Format: "image",
		DataBase64: base64.StdEncoding.EncodeToString(b),
		Paper:      &domain.Paper{WidthMM: 100, HeightMM: 150},
	}
	path, _, err := Prepare(context.Background(), job, cfg, t.TempDir(), "")
	if err != nil {
		t.Fatalf("inline image: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "%PDF-") {
		t.Fatalf("conversion did not produce a PDF: %q", data[:16])
	}
}

// writeINeedPNG writes a small valid PNG without depending on the other test
// file's helper, whose name is specific to that file's intent.
func writeINeedPNG(path string, w, h int) error {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 120, B: 200, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
