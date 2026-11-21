package convert

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/security"
)

func testConfig() domain.Config {
	cfg := domain.DefaultConfig()
	cfg.Limits.MaxFileSizeMB = 1
	cfg.Limits.DownloadTimeoutSeconds = 5
	return cfg
}

func TestOfficePDFDoesNotStartAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := OfficePDF(ctx, filepath.Join(t.TempDir(), "not-opened.docx"), "docx", t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation before opening Office input, got %v", err)
	}
}

func TestOfficeTimeoutUsesConversionFailureCode(t *testing.T) {
	err := officeFailure(errors.New("Office conversion timeout"))
	if code := domain.CodeOf(err); code != domain.CodeOfficeConvertFailed {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeOfficeConvertFailed, err)
	}
}

func TestRawInvalidBase64IsInvalidMessage(t *testing.T) {
	cfg := testConfig()
	job := domain.Job{JobID: "raw-bad", Format: "raw", DataBase64: "!!!not-base64!!!"}
	_, _, err := Prepare(context.Background(), job, cfg, t.TempDir(), "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if code := domain.CodeOf(err); code != domain.CodeInvalidMessage {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeInvalidMessage, err)
	}
}

func TestRawEmptyPayloadIsInvalidMessage(t *testing.T) {
	cfg := testConfig()
	job := domain.Job{JobID: "raw-empty", Format: "raw", DataBase64: base64.StdEncoding.EncodeToString(nil)}
	_, _, err := Prepare(context.Background(), job, cfg, t.TempDir(), "")
	if code := domain.CodeOf(err); code != domain.CodeInvalidMessage {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeInvalidMessage, err)
	}
}

func TestRawOversizedIsFileTooLarge(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.MaxFileSizeMB = 1
	payload := bytes.Repeat([]byte("x"), 2*1024*1024)
	job := domain.Job{JobID: "raw-big", Format: "raw", DataBase64: base64.StdEncoding.EncodeToString(payload)}
	_, _, err := Prepare(context.Background(), job, cfg, t.TempDir(), "")
	if code := domain.CodeOf(err); code != domain.CodeFileTooLarge {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeFileTooLarge, err)
	}
}

func TestRawValidPayloadPassesThrough(t *testing.T) {
	cfg := testConfig()
	want := []byte("\x1b@hello ESC/POS")
	job := domain.Job{JobID: "raw-ok", Format: "raw", DataBase64: base64.StdEncoding.EncodeToString(want)}
	pdf, raw, err := Prepare(context.Background(), job, cfg, t.TempDir(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pdf != "" {
		t.Fatalf("raw jobs must not produce a PDF path, got %q", pdf)
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("payload changed: %q", raw)
	}
}

func TestPDFDataURLBadBase64IsFormatMismatch(t *testing.T) {
	cfg := testConfig()
	job := domain.Job{JobID: "pdf-bad-b64", Format: "pdf", FileURL: "data:application/pdf;base64,%%%not base64%%%"}
	_, _, err := Prepare(context.Background(), job, cfg, t.TempDir(), "")
	if code := domain.CodeOf(err); code != domain.CodeFormatMismatch {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeFormatMismatch, err)
	}
}

func TestPDFDataURLWrongMagicIsFormatMismatch(t *testing.T) {
	cfg := testConfig()
	payload := []byte("this is definitely not a PDF")
	job := domain.Job{
		JobID:   "pdf-wrong-magic",
		Format:  "pdf",
		FileURL: "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(payload),
	}
	_, _, err := Prepare(context.Background(), job, cfg, t.TempDir(), "")
	if code := domain.CodeOf(err); code != domain.CodeFormatMismatch {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeFormatMismatch, err)
	}
}

func TestPDFDataURLValidPasses(t *testing.T) {
	cfg := testConfig()
	payload := []byte("%PDF-1.4\n%%EOF\n")
	job := domain.Job{
		JobID:   "pdf-ok",
		Format:  "pdf",
		FileURL: "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(payload),
	}
	pdf, raw, err := Prepare(context.Background(), job, cfg, t.TempDir(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if raw != nil {
		t.Fatal("pdf jobs must not return raw bytes")
	}
	body, readErr := os.ReadFile(pdf)
	if readErr != nil {
		t.Fatalf("read generated PDF: %v", readErr)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("PDF bytes changed: %q", body)
	}
}

func TestUnknownFormatIsUnsupportedFormat(t *testing.T) {
	cfg := testConfig()
	// The format is rejected before the download, so an unreachable URL does
	// not mask the classification.
	job := domain.Job{JobID: "unsupported", Format: "tiff", FileURL: "https://example.com/a.tiff"}
	_, _, err := Prepare(context.Background(), job, cfg, t.TempDir(), "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if code := domain.CodeOf(err); code != domain.CodeUnsupportedFormat {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeUnsupportedFormat, err)
	}
}

func TestVerifyPDFBadMagicIsFormatMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a.pdf")
	if err := os.WriteFile(path, []byte("GIF89a-garbage"), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	err := VerifyPDF(path)
	if code := domain.CodeOf(err); code != domain.CodeFormatMismatch {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeFormatMismatch, err)
	}
}

func TestVerifyPDFTooShortIsFormatMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short.pdf")
	if err := os.WriteFile(path, []byte("%PD"), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if code := domain.CodeOf(VerifyPDF(path)); code != domain.CodeFormatMismatch {
		t.Fatalf("code = %q, want %q", code, domain.CodeFormatMismatch)
	}
}

func TestVerifyPDFMissingFileIsInternalError(t *testing.T) {
	err := VerifyPDF(filepath.Join(t.TempDir(), "missing.pdf"))
	if code := domain.CodeOf(err); code != domain.CodeInternalError {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeInternalError, err)
	}
}

func TestDownloadFailurePassesThroughUnchanged(t *testing.T) {
	cfg := testConfig()
	// A loopback URL is blocked by the download guard. The download layer
	// classifies it as DOWNLOAD_FAILED and Prepare must not re-wrap it.
	job := domain.Job{JobID: "dl-blocked", Format: "pdf", FileURL: "http://127.0.0.1/secret.pdf"}
	_, _, err := Prepare(context.Background(), job, cfg, t.TempDir(), "")
	if code := domain.CodeOf(err); code != domain.CodeDownloadFailed {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeDownloadFailed, err)
	}
	if !errors.Is(err, security.ErrBlockedAddress) {
		t.Fatalf("the blocked-address sentinel must survive: %v", err)
	}
}

func TestOOXMLGarbageIsFormatMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake.docx")
	if err := os.WriteFile(path, []byte("not a zip at all"), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, _, err := OfficePDF(context.Background(), path, "docx", t.TempDir())
	if code := domain.CodeOf(err); code != domain.CodeFormatMismatch {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeFormatMismatch, err)
	}
}

func TestOOXMLWrongKindIsFormatMismatch(t *testing.T) {
	// A valid ZIP that has [Content_Types].xml but no word/document.xml is a
	// spreadsheet-like container, so a docx job must reject it.
	path := filepath.Join(t.TempDir(), "wrong.docx")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	zw := zip.NewWriter(f)
	for _, name := range []string{"[Content_Types].xml", "xl/workbook.xml"} {
		w, writeErr := zw.Create(name)
		if writeErr != nil {
			t.Fatalf("zip entry %s: %v", name, writeErr)
		}
		if _, writeErr := w.Write([]byte("<xml/>")); writeErr != nil {
			t.Fatalf("zip body %s: %v", name, writeErr)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	_, _, err = OfficePDF(context.Background(), path, "docx", t.TempDir())
	if code := domain.CodeOf(err); code != domain.CodeFormatMismatch {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeFormatMismatch, err)
	}
}

func TestOfficeConversionUnavailableIsOfficeConvertFailed(t *testing.T) {
	// This environment-dependent test is meaningful only where no converter
	// can run. Windows may have Office or WPS COM registrations even when
	// LibreOffice is absent; the test cannot safely infer their absence.
	if runtime.GOOS == "windows" {
		t.Skip("Windows Office/WPS COM availability is not controlled by this test")
	}
	if _, err := findLibreOffice(); err == nil {
		t.Skip("LibreOffice is installed; the unavailable path cannot be exercised here")
	}
	path := filepath.Join(t.TempDir(), "ok.docx")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	zw := zip.NewWriter(f)
	for _, name := range []string{"[Content_Types].xml", "word/document.xml"} {
		w, writeErr := zw.Create(name)
		if writeErr != nil {
			t.Fatalf("zip entry %s: %v", name, writeErr)
		}
		if _, writeErr := w.Write([]byte("<xml/>")); writeErr != nil {
			t.Fatalf("zip body %s: %v", name, writeErr)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _, err = OfficePDF(ctx, path, "docx", t.TempDir())
	if code := domain.CodeOf(err); code != domain.CodeOfficeConvertFailed {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeOfficeConvertFailed, err)
	}
	if !strings.Contains(strings.ToLower(domain.MessageOf(err)), "office conversion") {
		t.Fatalf("message should explain the converter problem: %v", err)
	}
}

func TestImagePDFValidPNGProducesPDF(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.png")
	if err := writePNG(src, 4, 4); err != nil {
		t.Fatalf("write png: %v", err)
	}
	dest := filepath.Join(dir, "out.pdf")
	paper := &domain.Paper{WidthMM: 210, HeightMM: 297}
	if err := ImagePDF(src, dest, paper); err != nil {
		t.Fatalf("ImagePDF: %v", err)
	}
	if err := VerifyPDF(dest); err != nil {
		t.Fatalf("generated PDF does not verify: %v", err)
	}
}

func TestImagePDFGarbageIsFormatMismatch(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "fake.png")
	if err := os.WriteFile(src, []byte("not an image"), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	err := ImagePDF(src, filepath.Join(dir, "out.pdf"), nil)
	if code := domain.CodeOf(err); code != domain.CodeFormatMismatch {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeFormatMismatch, err)
	}
}

func TestImageInvalidDimensionsIsFormatMismatch(t *testing.T) {
	// Both the degenerate and the bomb case are format mismatches: the bytes
	// decoded, but they cannot be turned into a printable page.
	for _, bounds := range []struct {
		name   string
		w, h   int
		expect string
	}{
		{"zero width", 0, 10, "at least 1 pixel"},
		{"zero height", 10, 0, "at least 1 pixel"},
		{"negative", -3, 4, "at least 1 pixel"},
		{"pixel cap", 20000, 20000, "100000000"},
	} {
		err := validateImageBounds(bounds.w, bounds.h)
		if err == nil {
			t.Fatalf("%s: expected an error", bounds.name)
		}
		if code := domain.CodeOf(err); code != domain.CodeFormatMismatch {
			t.Fatalf("%s: code = %q, want %q (err: %v)", bounds.name, code, domain.CodeFormatMismatch, err)
		}
		if !strings.Contains(domain.MessageOf(err), bounds.expect) {
			t.Fatalf("%s: message %q should contain %q", bounds.name, domain.MessageOf(err), bounds.expect)
		}
	}
	// A normal page-sized raster must still be accepted.
	if err := validateImageBounds(2480, 3508); err != nil {
		t.Fatalf("valid A4 raster rejected: %v", err)
	}
}

func TestImagePDFWriteFailureIsInternalError(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.png")
	if err := writePNG(src, 2, 2); err != nil {
		t.Fatalf("write png: %v", err)
	}
	// A destination inside a non-existent directory cannot be created.
	dest := filepath.Join(dir, "missing-subdir", "out.pdf")
	err := ImagePDF(src, dest, nil)
	if code := domain.CodeOf(err); code != domain.CodeInternalError {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeInternalError, err)
	}
}

func TestRendererUnavailableKeepsPrefixAndInternalCode(t *testing.T) {
	// The agent layer and docs key off this exact prefix, while the wire code
	// must stay INTERNAL_ERROR because clients have no render code.
	restore := stubBrowserPath(func() (string, error) { return "", errors.New("executable file not found in $PATH") })
	defer restore()

	cfg := testConfig()
	job := domain.Job{JobID: "html-no-browser", Format: "raw-html", HTML: "<html></html>"}
	_, _, err := RenderHTML(context.Background(), job, cfg, t.TempDir(), "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if code := domain.CodeOf(err); code != domain.CodeInternalError {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeInternalError, err)
	}
	if !strings.HasPrefix(domain.MessageOf(err), "renderer-unavailable: ") {
		t.Fatalf("message must keep the renderer-unavailable prefix: %q", domain.MessageOf(err))
	}
	if !IsRendererUnavailable(err) {
		t.Fatal("IsRendererUnavailable must recognize the message")
	}
	if domain.HasCode(err, domain.CodeRendererUnavailable) {
		t.Fatal("RENDERER_UNAVAILABLE must not appear on the wire; the SDK does not define it")
	}
}

func TestCancellationStaysCancellation(t *testing.T) {
	// The agent reports `cancelled` by matching context.Canceled. Classification
	// must not turn a cancelled conversion into a `failed` job with a code.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	restore := stubBrowserPath(func() (string, error) { return "fake-chrome", nil })
	defer restore()

	cfg := testConfig()
	job := domain.Job{JobID: "cancelled", Format: "html", FileURL: "https://example.com/"}
	_, _, err := RenderHTML(ctx, job, cfg, t.TempDir(), "")
	if err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must stay matchable: %v", err)
	}
}

// stubBrowserPath swaps the browser probe for the duration of one test.
func stubBrowserPath(fn func() (string, error)) func() {
	previous := browserPath
	browserPath = fn
	return func() { browserPath = previous }
}

func TestHTMLTooLargeIsFileTooLarge(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.MaxFileSizeMB = 1
	job := domain.Job{
		JobID:  "html-big",
		Format: "raw-html",
		HTML:   strings.Repeat("a", 2*1024*1024),
	}
	// The size guard runs before the browser probe, so this is deterministic
	// even on a machine without Chromium.
	_, _, err := RenderHTML(context.Background(), job, cfg, t.TempDir(), "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if code := domain.CodeOf(err); code != domain.CodeFileTooLarge {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeFileTooLarge, err)
	}
}

func TestHTMLInvalidURLIsDownloadFailed(t *testing.T) {
	// A stub browser keeps the assertion on the URL guard rather than on
	// whatever Chromium build happens to be installed on this host.
	restore := stubBrowserPath(func() (string, error) { return "fake-chrome", nil })
	defer restore()

	cfg := testConfig()
	// URL validation runs before any browser launch, so this is deterministic.
	for _, raw := range []string{"file:///etc/passwd", "ftp://example.com/x.html", "http://127.0.0.1/x.html", "http://10.1.2.3/x.html"} {
		job := domain.Job{JobID: "html-bad-url", Format: "html", FileURL: raw}
		_, _, err := RenderHTML(context.Background(), job, cfg, t.TempDir(), "")
		if err == nil {
			t.Fatalf("%q: expected an error", raw)
		}
		if code := domain.CodeOf(err); code != domain.CodeDownloadFailed {
			t.Fatalf("%q: code = %q, want %q (err: %v)", raw, code, domain.CodeDownloadFailed, err)
		}
		if !errors.Is(err, security.ErrBlockedAddress) && raw != "file:///etc/passwd" && raw != "ftp://example.com/x.html" {
			t.Fatalf("%q: blocked-address sentinel lost: %v", raw, err)
		}
	}
}

func writePNG(path string, w, h int) error {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 32, B: 32, A: 255})
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
