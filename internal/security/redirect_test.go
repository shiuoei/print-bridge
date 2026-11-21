package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// stubLookup maps hostnames to fixture addresses for tests. The address checks
// still run on every returned IP, so this seam proves the redirect path
// re-validates instead of skipping the guard.
func stubLookup(t *testing.T, mapping map[string]string) {
	t.Helper()
	previous := lookupIP
	lookupIP = func(ctx context.Context, network, host string) ([]net.IP, error) {
		if target, ok := mapping[host]; ok {
			ip := net.ParseIP(target)
			if ip == nil {
				return nil, fmt.Errorf("stub: bad ip %q", target)
			}
			return []net.IP{ip}, nil
		}
		return previous(ctx, network, host)
	}
	t.Cleanup(func() { lookupIP = previous })
}

// TestRedirectGuardRejectsPrivateAndNonHTTP documents every redirect target the
// guard must refuse. This is the layer that stops a public URL from bouncing the
// downloader into the local network.
func TestRedirectGuardRejectsPrivateAndNonHTTP(t *testing.T) {
	guard := Client(time.Second).CheckRedirect
	if guard == nil {
		t.Fatal("client has no redirect guard")
	}
	bad := []string{
		"http://127.0.0.1/x",
		"http://localhost/x",
		"http://10.1.2.3/x",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/x",
		"http://192.168.1.1/x",
		"http://0.0.0.0/x",
		"file:///etc/passwd",
		"gopher://127.0.0.1/",
		"ftp://example.com/x",
	}
	for _, target := range bad {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			// A URL the parser rejects outright is also a safe outcome.
			continue
		}
		if err := guard(req, []*http.Request{req}); err == nil {
			t.Errorf("redirect guard accepted %q", target)
		}
	}
	// A plain public URL must still be allowed, otherwise downloads break.
	req, err := http.NewRequest(http.MethodGet, "https://example.com/file.pdf", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard(req, []*http.Request{req}); err != nil {
		t.Errorf("redirect guard rejected a public URL: %v", err)
	}
}

// TestRedirectGuardLimitsHops pins the redirect-follow limit.
func TestRedirectGuardLimitsHops(t *testing.T) {
	guard := Client(time.Second).CheckRedirect
	req, err := http.NewRequest(http.MethodGet, "https://example.com/file.pdf", nil)
	if err != nil {
		t.Fatal(err)
	}
	via := make([]*http.Request, 5)
	for i := range via {
		via[i] = req
	}
	if err := guard(req, via); err == nil {
		t.Fatal("redirect guard accepted too many hops")
	}
}

// TestRedirectToLoopbackIsReBlocked proves the second hop of a redirect chain is
// re-validated. The stubbed resolver makes a public-looking hostname resolve to a
// public address so the URL guard accepts the first hop; the server then answers
// 302 to a loopback literal. The dial-time check must still refuse it.
func TestRedirectToLoopbackIsReBlocked(t *testing.T) {
	stubLookup(t, map[string]string{
		// 192.0.2.0/24 is TEST-NET-1: public-looking, never routed.
		"public.example.test": "192.0.2.1",
	})

	guard := Client(time.Second).CheckRedirect
	if guard == nil {
		t.Fatal("client has no redirect guard")
	}

	// First hop is accepted by URL shape.
	first, err := http.NewRequest(http.MethodGet, "http://public.example.test/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard(first, []*http.Request{first}); err != nil {
		t.Fatalf("guard rejected the public first hop: %v", err)
	}

	// Second hop to a loopback literal must be refused.
	second, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9/secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = guard(second, []*http.Request{first, second})
	if err == nil {
		t.Fatal("guard accepted a loopback redirect target")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("guard error = %v, want ErrBlockedAddress", err)
	}
}

// TestDialGuardBlocksHostnameThatResolvesPrivate proves the DNS-time check, not
// just the literal-IP check: a hostname whose only answer is loopback is refused
// before any connection is made.
func TestDialGuardBlocksHostnameThatResolvesPrivate(t *testing.T) {
	stubLookup(t, map[string]string{
		"internal.example.test": "127.0.0.1",
	})

	client := Client(time.Second)
	resp, err := client.Get("http://internal.example.test/x")
	if err == nil {
		resp.Body.Close()
		t.Fatal("client reached a hostname resolving to loopback")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("error = %v, want ErrBlockedAddress", err)
	}
}

// TestDownloadFirstHopLoopbackIsRefused is the end-to-end entry-point check: a
// download whose URL is already loopback never reaches the server.
func TestDownloadFirstHopLoopbackIsRefused(t *testing.T) {
	reached := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer server.Close()

	_, err := Download(context.Background(), server.URL, t.TempDir(), 1<<20, 2*time.Second)
	if err == nil {
		t.Fatal("expected the loopback download to be refused")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("error = %v, want ErrBlockedAddress", err)
	}
	if code := domain.CodeOf(err); code != domain.CodeDownloadFailed {
		t.Fatalf("code = %q, want %q", code, domain.CodeDownloadFailed)
	}
	if reached {
		t.Fatal("loopback server was reached")
	}
}
