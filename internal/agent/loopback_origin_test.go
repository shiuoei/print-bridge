package agent

import (
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// TestHTMLLocalOriginForMatchingSameOrigin is the happy path for the local web
// app use case: a page served from the same loopback origin as the WebSocket
// handshake may be printed.
func TestHTMLLocalOriginForMatchingSameOrigin(t *testing.T) {
	jobs := []domain.Job{{JobID: "j", Format: "html", FileURL: "http://127.0.0.1:6688/invoice/1"}}
	got, err := htmlLocalOriginFor(jobs, "http://127.0.0.1:6688")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "http://127.0.0.1:6688" {
		t.Fatalf("origin = %q, want http://127.0.0.1:6688", got)
	}
}

// TestHTMLLocalOriginForRejectsMismatch covers every way the origin can fail to
// authorise the page. Each must be an explicit error, not a silent downgrade,
// because a silent downgrade turns a local print into a confusing DOWNLOAD_FAILED.
func TestHTMLLocalOriginForRejectsMismatch(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		origin string
	}{
		{"different port", "http://127.0.0.1:6689/x", "http://127.0.0.1:6688"},
		{"different scheme", "https://127.0.0.1:6688/x", "http://127.0.0.1:6688"},
		{"no origin", "http://127.0.0.1:6688/x", ""},
		{"public origin", "http://127.0.0.1:6688/x", "https://example.com"},
		{"localhost origin", "http://127.0.0.1:6688/x", "http://localhost:6688"},
		{"non-loopback origin", "http://127.0.0.1:6688/x", "http://10.0.0.5:6688"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			jobs := []domain.Job{{JobID: "j", Format: "html", FileURL: c.url}}
			if _, err := htmlLocalOriginFor(jobs, c.origin); err == nil {
				t.Fatalf("%s was authorised", c.name)
			}
		})
	}
}

// TestHTMLLocalOriginForIgnoresPublicJobs keeps the exception from being applied
// to a batch that only prints public pages.
func TestHTMLLocalOriginForIgnoresPublicJobs(t *testing.T) {
	jobs := []domain.Job{
		{JobID: "a", Format: "html", FileURL: "https://example.com/x"},
		{JobID: "b", Format: "pdf", FileURL: "https://example.com/y.pdf"},
	}
	got, err := htmlLocalOriginFor(jobs, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("origin = %q, want empty for a public-only batch", got)
	}
}

// TestHTMLLocalOriginForMixedBatchIsRejected matches the protocol rule: one
// mismatch in a batch refuses the whole batch rather than granting loopback
// access to the public jobs too.
func TestHTMLLocalOriginForMixedBatchIsRejected(t *testing.T) {
	jobs := []domain.Job{
		{JobID: "local", Format: "html", FileURL: "http://127.0.0.1:6688/x"},
		{JobID: "other", Format: "html", FileURL: "http://127.0.0.1:7777/y"},
	}
	if _, err := htmlLocalOriginFor(jobs, "http://127.0.0.1:6688"); err == nil {
		t.Fatal("a batch mixing two loopback origins was authorised")
	}
}

// TestIsExactLoopbackHost accepts only the canonical literal loopback addresses.
func TestIsExactLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"127.0.0.2", false},
		{"localhost", false},
		{"example.com", false},
		{"10.0.0.5", false},
	}
	for _, c := range cases {
		if got := isExactLoopbackHost(c.host); got != c.want {
			t.Errorf("isExactLoopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// TestNonHTMLJobNeverGetsLoopbackOrigin keeps a PDF job from carrying an HTML
// allowance it has no use for.
func TestNonHTMLJobNeverGetsLoopbackOrigin(t *testing.T) {
	a := newTrackingTestAgent(t, &fakePrinter{})
	// A PDF job with a loopback URL is rejected by the download guard, and the
	// allowance must not be consulted for it at all.
	jobs := []domain.Job{{JobID: "p", Format: "pdf", FileURL: "http://127.0.0.1:6688/x.pdf"}}
	got, err := htmlLocalOriginFor(jobs, "http://127.0.0.1:6688")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("a non-HTML job produced loopback origin %q", got)
	}
	_ = a
}
