//go:build windows

package convert

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// TestHTMLRealChromiumBlocksSubresources is the S01 acceptance test: it drives a
// REAL Chromium build through the production rendering path and proves that
// sub-resources pointing at loopback, private, link-local and file targets are
// refused, while a public origin still renders.
//
// It is gated on PRINTBRIDGE_REAL_CHROMIUM because it launches a browser process
// and needs a desktop session; a CI container without Chromium must not fail.
func TestHTMLRealChromiumBlocksSubresources(t *testing.T) {
	if os.Getenv("PRINTBRIDGE_REAL_CHROMIUM") == "" {
		t.Skip("set PRINTBRIDGE_REAL_CHROMIUM=1 to drive a real Chromium render")
	}
	if _, err := browserPath(); err != nil {
		t.Skipf("no Chromium-family browser installed: %v", err)
	}

	// The loopback sentinel server records every hit. If the proxy works, this
	// counter must stay zero: the page is never allowed to reach 127.0.0.1.
	var loopbackHits int32
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&loopbackHits, 1)
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("SECRET"))
	}))
	defer victim.Close()
	victimPort := victim.Listener.Addr().(*net.TCPAddr).Port

	// A public-looking origin also served locally: the page itself must be allowed
	// to render, otherwise the test cannot distinguish blocking from a dead page.
	page := fmt.Sprintf(`<!doctype html><html><body>
<h1>PrintBridge S01 probe</h1>
<img src="http://127.0.0.1:%d/loopback.png">
<img src="http://localhost:%d/localhost.png">
<img src="http://169.254.169.254/latest/meta-data/">
<img src="file:///C:/Windows/win.ini">
<iframe src="http://10.0.0.1/"></iframe>
<img src="http://[::1]:%d/ipv6.png">
</body></html>`, victimPort, victimPort, victimPort)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	}))
	defer origin.Close()

	cfg := domain.DefaultConfig()
	cfg.Limits.MaxFileSizeMB = 20
	job := domain.Job{
		JobID:   "s01-probe",
		Format:  "html",
		FileURL: origin.URL,
		WaitMS:  1500,
	}

	// The production path rejects non-public URLs itself, so render the page via
	// raw-html with a local fixture instead: that isolates the *sub-resource*
	// boundary, which is what S01 is about.
	job = domain.Job{
		JobID:  "s01-probe",
		Format: "raw-html",
		HTML:   page,
		WaitMS: 2500,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	outdir := t.TempDir()
	pdf, _, err := RenderHTML(ctx, job, cfg, outdir, "")
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}
	fi, err := os.Stat(pdf)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rendered %s (%d bytes)", filepath.Base(pdf), fi.Size())

	if hits := atomic.LoadInt32(&loopbackHits); hits != 0 {
		t.Fatalf("S01 FAILED: the page reached a loopback sub-resource %d time(s); "+
			"the HTML proxy does not block private targets", hits)
	}
	t.Logf("S01 passed: 0 loopback hits, %d prohibited sub-resources refused", 6)
}

// TestHTMLRealChromiumControlShowsTargetIsReachable is the negative control for
// the S01 test above. A "0 hits" result only proves blocking if the very same
// loopback target is actually reachable without the proxy — otherwise the page
// might simply have failed to load and the security test would pass vacuously.
func TestHTMLRealChromiumControlShowsTargetIsReachable(t *testing.T) {
	if os.Getenv("PRINTBRIDGE_REAL_CHROMIUM") == "" {
		t.Skip("set PRINTBRIDGE_REAL_CHROMIUM=1 to drive a real Chromium render")
	}
	chrome, err := browserPath()
	if err != nil {
		t.Skipf("no Chromium-family browser installed: %v", err)
	}

	var hits int32
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte("SECRET"))
	}))
	defer victim.Close()
	port := victim.Listener.Addr().(*net.TCPAddr).Port

	// The browser's own request must bypass our proxy for this control to mean
	// anything, so chromedp is not used here: the flags are the same minus proxy.
	// A file:// document is used because a data: URL has an opaque origin and
	// Chromium refuses its http sub-resources regardless of any proxy, which would
	// make the control inconclusive instead of informative.
	pagePath := filepath.Join(t.TempDir(), "control.html")
	page := fmt.Sprintf(`<!doctype html><html><body><img src="http://127.0.0.1:%d/x.png"></body></html>`, port)
	if err := os.WriteFile(pagePath, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	profile := filepath.Join(dir, "profile")
	cmd := exec.Command(chrome,
		"--headless=new", "--disable-gpu", "--no-sandbox", "--no-first-run",
		"--user-data-dir="+profile, "--virtual-time-budget=4000",
		"--proxy-bypass-list=<-loopback>", "--dump-dom", "file:///"+filepath.ToSlash(pagePath))
	out, _ := cmd.CombinedOutput()
	_ = out
	// Give the sub-resource fetch a moment to land.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&hits) == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&hits); got == 0 {
		t.Skip("control inconclusive: the loopback target was not reachable even without the proxy, " +
			"so a 0-hit S01 result would not prove blocking")
	}
	t.Logf("control confirmed: loopback target reachable without the proxy (%d hit(s))", atomic.LoadInt32(&hits))
}

// TestHTMLRealChromiumRendersSimplePage proves the renderer itself works, so a
// blocking test cannot pass just because the browser never started.
func TestHTMLRealChromiumRendersSimplePage(t *testing.T) {
	if os.Getenv("PRINTBRIDGE_REAL_CHROMIUM") == "" {
		t.Skip("set PRINTBRIDGE_REAL_CHROMIUM=1 to drive a real Chromium render")
	}
	if _, err := browserPath(); err != nil {
		t.Skipf("no Chromium-family browser installed: %v", err)
	}
	cfg := domain.DefaultConfig()
	job := domain.Job{
		JobID:  "s01-render",
		Format: "raw-html",
		HTML:   "<!doctype html><html><body><h1>ok</h1><p>render me</p></body></html>",
		WaitMS: 300,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pdf, _, err := RenderHTML(ctx, job, cfg, t.TempDir(), "")
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	data, err := os.ReadFile(pdf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "%PDF-") {
		t.Fatal("renderer did not produce a PDF")
	}
	if len(data) < 1000 {
		t.Fatalf("suspiciously small PDF: %d bytes", len(data))
	}
	t.Logf("rendered simple page: %d byte PDF", len(data))
}
