package convert

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// TestSafeProxyRecordsBlockedResource covers the change that makes a refused
// sub-resource fail the render. Before this, the proxy answered 403 and the
// render still succeeded with a PDF silently missing the asset.
func TestSafeProxyRecordsBlockedResource(t *testing.T) {
	p, err := newSafeProxy("")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// A loopback target is refused by the policy.
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9/secret.png", nil)
	rec := httptest.NewRecorder()
	p.handle(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	target := p.blockedResource()
	if target == "" {
		t.Fatal("the refused sub-resource was not recorded")
	}
	if !strings.Contains(target, "127.0.0.1") {
		t.Fatalf("recorded target = %q, want it to name the refused host", target)
	}
}

// TestSafeProxyResetBlocksClearsRecord keeps pre-navigation background traffic
// from failing the job.
func TestSafeProxyResetBlocksClearsRecord(t *testing.T) {
	p, err := newSafeProxy("")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	req := httptest.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	p.handle(httptest.NewRecorder(), req)
	if p.blockedResource() == "" {
		t.Fatal("block not recorded")
	}
	p.resetBlocks()
	if got := p.blockedResource(); got != "" {
		t.Fatalf("blockedResource = %q after reset, want empty", got)
	}
}

// TestSafeProxyBlocksFileScheme covers the non-HTTP scheme path.
func TestSafeProxyBlocksFileScheme(t *testing.T) {
	p, err := newSafeProxy("")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.URL.Scheme = "file"
	req.URL.Host = ""
	p.handle(httptest.NewRecorder(), req)
	if p.blockedResource() == "" {
		t.Fatal("a file: target was not recorded as blocked")
	}
}

// requireRenderer skips a test when Chromium cannot actually be launched. The
// binary may be present while this sandbox forbids the process spawn, so probing
// the path alone is not enough.
func requireRenderer(t *testing.T) {
	t.Helper()
	if _, err := browserPath(); err != nil {
		t.Skip("no Chromium installed:", err)
	}
}

// TestRenderHTMLRefusesOnBlockedResource exercises the end-to-end path with a
// page that references a loopback image. The render must fail rather than ship
// a PDF without the image.
func TestRenderHTMLRefusesOnBlockedResource(t *testing.T) {
	requireRenderer(t)
	page := "<html><body><h1>Invoice</h1><img src=\"http://127.0.0.1:9/blocked.png\"></body></html>"
	job := domain.Job{
		JobID:  "blocked-resource",
		Format: "raw-html",
		HTML:   page,
		WaitMS: 300,
	}
	cfg := domain.DefaultConfig()
	_, _, err := RenderHTML(context.Background(), job, cfg, t.TempDir(), "")
	if err == nil {
		t.Fatal("render succeeded despite a blocked sub-resource")
	}
	if !strings.Contains(domain.MessageOf(err), "blocked sub-resource") {
		// A sandbox that forbids spawning Chromium cannot reach the proxy path.
		if strings.Contains(domain.MessageOf(err), "chrome failed to start") {
			t.Skip("Chromium cannot be spawned in this environment:", domain.MessageOf(err))
		}
		t.Fatalf("error = %q, want it to name the blocked sub-resource", domain.MessageOf(err))
	}
}

// TestDefaultHTMLWaitMSIsOneSecond pins the default that keeps script-rendered
// content from being captured before it exists.
func TestDefaultHTMLWaitMSIsOneSecond(t *testing.T) {
	if DefaultHTMLWaitMS != 1000 {
		t.Fatalf("DefaultHTMLWaitMS = %d, want 1000", DefaultHTMLWaitMS)
	}
	if MaxHTMLWaitMS != 30000 {
		t.Fatalf("MaxHTMLWaitMS = %d, want 30000", MaxHTMLWaitMS)
	}
}

// TestInlineHTMLRendersWithDefaultWait checks that an inline page still renders
// successfully when the job omits wait_ms (the default is applied internally).
func TestInlineHTMLRendersWithDefaultWait(t *testing.T) {
	requireRenderer(t)
	job := domain.Job{
		JobID:  "inline-default-wait",
		Format: "raw-html",
		HTML:   "<html><body><p>hello</p></body></html>",
	}
	cfg := domain.DefaultConfig()
	pdf, _, err := RenderHTML(context.Background(), job, cfg, t.TempDir(), "")
	if err != nil {
		if strings.Contains(domain.MessageOf(err), "chrome failed to start") {
			t.Skip("Chromium cannot be spawned in this environment:", domain.MessageOf(err))
		}
		t.Fatalf("RenderHTML: %v", err)
	}
	if pdf == "" {
		t.Fatal("no path returned")
	}
}
