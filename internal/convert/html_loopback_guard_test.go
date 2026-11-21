package convert

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// These tests cover two defects that the sandbox previously hid: the HTML
// renderer produced PDFs with no images at all, and it did not refuse
// unauthorised loopback sub-resources. Both came from the same cause — the
// "<-loopback>" proxy rule made Chromium drop loopback sub-resources, so the
// request policy never saw them either.

// onePixelPNG is a valid 1x1 PNG, so a loaded image produces a real XObject in
// the PDF instead of a failed decode.
func onePixelPNG() []byte {
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x03, 0x01, 0x01, 0x00, 0x18, 0xdd, 0x8d,
		0xb0, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
		0x44, 0xae, 0x42, 0x60, 0x82,
	}
}

// TestSameOriginLoopbackImageIsRendered is the regression for "PDF silently has
// no images": an image served from the page's own origin must appear in the
// printed PDF.
func TestSameOriginLoopbackImageIsRendered(t *testing.T) {
	requireRenderer(t)
	served := make(chan struct{}, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><img src="/pic.png" width="50" height="50"></body></html>`))
	})
	mux.HandleFunc("/pic.png", func(w http.ResponseWriter, r *http.Request) {
		served <- struct{}{}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(onePixelPNG())
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	job := domain.Job{JobID: "same-origin-image", Format: "html", FileURL: srv.URL + "/", WaitMS: 2000}
	pdfPath, _, err := RenderHTML(context.Background(), job, domain.DefaultConfig(), t.TempDir(), srv.URL)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	select {
	case <-served:
	default:
		t.Fatal("the page's own image was never requested")
	}
	data, err := os.ReadFile(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	// A rendered image becomes an XObject in the PDF. Without it the PDF is a
	// text-only shell and the printout is silently missing content.
	if !strings.Contains(string(data), "/XObject") {
		t.Fatalf("PDF has no image XObject (%d bytes); the image was dropped", len(data))
	}
}

// TestUnauthorisedLoopbackSubresourceIsRefusedAndFails is the regression for the
// security half: a page must not be able to read another local port, and the
// render must fail rather than print an incomplete page.
func TestUnauthorisedLoopbackSubresourceIsRefusedAndFails(t *testing.T) {
	requireRenderer(t)
	reached := make(chan struct{}, 4)
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
		_, _ = w.Write([]byte("SECRET"))
	}))
	defer secret.Close()

	pageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><img src="` + secret.URL + `/x.png"></body></html>`))
	}))
	defer pageSrv.Close()

	job := domain.Job{JobID: "cross-origin", Format: "html", FileURL: pageSrv.URL + "/", WaitMS: 2000}
	_, _, err := RenderHTML(context.Background(), job, domain.DefaultConfig(), t.TempDir(), pageSrv.URL)
	if err == nil {
		t.Fatal("render succeeded despite an unauthorised loopback sub-resource")
	}
	select {
	case <-reached:
		t.Fatal("SECURITY: the unauthorised loopback target was actually fetched")
	default:
	}
	if !strings.Contains(domain.MessageOf(err), "blocked sub-resource") {
		t.Fatalf("error = %q, want it to name the blocked sub-resource", domain.MessageOf(err))
	}
}

// TestLoopbackGuardURLClassification pins the URL classification the guard uses.
func TestLoopbackGuardURLClassification(t *testing.T) {
	loopback := []string{
		"http://127.0.0.1:8080/x", "https://127.0.0.1/x", "http://localhost:3000/",
		"http://[::1]:9000/x", "http://127.0.0.2/", // any 127/8 is loopback
	}
	for _, raw := range loopback {
		if !isLoopbackURL(raw) {
			t.Errorf("%q not classified as loopback", raw)
		}
	}
	public := []string{
		"https://example.com/x", "http://10.0.0.5/x", "http://169.254.169.254/",
		"http://192.168.1.1/", "not a url",
	}
	for _, raw := range public {
		if isLoopbackURL(raw) {
			t.Errorf("%q wrongly classified as loopback", raw)
		}
	}
}
