package convert

import (
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// proxyClient routes requests through p so tests exercise the same handler the
// renderer uses.
func proxyClient(p *safeProxy) *http.Client {
	proxyURL, _ := url.Parse("http://" + p.Addr())
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   5 * time.Second,
	}
}

// TestProxyResourceBudgetEnforcesCount proves the per-page request budget is
// enforced: after the limit, further sub-resources are refused instead of
// letting a hostile page fan out unbounded requests.
func TestProxyResourceBudgetEnforcesCount(t *testing.T) {
	p, err := newSafeProxy("")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for i := 0; i < maxPageResources; i++ {
		if !p.chargeResource() {
			t.Fatalf("chargeResource refused within budget at %d", i)
		}
	}
	if p.chargeResource() {
		t.Fatal("chargeResource accepted more than maxPageResources")
	}
	if !p.exhaustedNow() {
		t.Fatal("budget exhaustion was not recorded")
	}
}

// TestProxyResourceBudgetEnforcesBytes proves the byte budget is enforced.
func TestProxyResourceBudgetEnforcesBytes(t *testing.T) {
	p, err := newSafeProxy("")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if !p.chargeBytes(maxPageBytes) {
		t.Fatal("full-budget charge was refused")
	}
	if p.chargeBytes(1) {
		t.Fatal("chargeBytes accepted a payload over maxPageBytes")
	}
	if !p.exhaustedNow() {
		t.Fatal("byte exhaustion was not recorded")
	}
}

// TestProxyValidatesBeforeCharging pins the ordering that keeps blocked requests
// from consuming the page budget, and checks the 403 the renderer relies on.
func TestProxyValidatesBeforeCharging(t *testing.T) {
	p, err := newSafeProxy("")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	client := proxyClient(p)
	for _, target := range []string{
		"http://127.0.0.1/x",
		"http://localhost/x",
		"http://10.0.0.1/x",
		"http://169.254.169.254/latest/meta-data",
	} {
		resp, err := client.Get(target)
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", target, resp.StatusCode)
		}
	}
	if resources, bytes := p.Stats(); resources != 0 || bytes != 0 {
		t.Fatalf("blocked requests charged budget: (%d, %d)", resources, bytes)
	}
}

// TestProxyLimitsAreSane guards against a misconfigured huge or zero cap.
func TestProxyLimitsAreSane(t *testing.T) {
	if maxSingleResourceBytes <= 0 || maxSingleResourceBytes > 1<<30 {
		t.Fatalf("maxSingleResourceBytes = %d, want a sane positive bound", maxSingleResourceBytes)
	}
	if maxPageBytes < maxSingleResourceBytes {
		t.Fatal("page budget must allow at least one resource")
	}
	if maxPageResources <= 0 || pageResourceTimeout <= 0 {
		t.Fatal("resource count and timeout bounds must be positive")
	}
}

// TestProxyBudgetAllowsManySmallResources exercises the common case: many small
// resources stay inside the budget.
func TestProxyBudgetAllowsManySmallResources(t *testing.T) {
	p, err := newSafeProxy("")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	const n = 100
	const each = 1024
	for i := 0; i < n; i++ {
		if !p.chargeResource() {
			t.Fatalf("resource %d refused", i)
		}
		if !p.chargeBytes(each) {
			t.Fatalf("bytes at %d refused", i)
		}
	}
	resources, bytes := p.Stats()
	if resources != n || bytes != n*each {
		t.Fatalf("stats = (%d,%d), want (%d,%d)", resources, bytes, n, n*each)
	}
	if p.exhaustedNow() {
		t.Fatal("a within-budget render was marked exhausted")
	}
}
