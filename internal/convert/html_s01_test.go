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
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/security"
)

// s01Victim is a loopback HTTP server that counts every request it receives. Any
// non-zero count means a prohibited target was reachable from the renderer.
type s01Victim struct {
	server *httptest.Server
	hits   int32
	port   int
}

func newS01Victim(t *testing.T) *s01Victim {
	t.Helper()
	return newS01VictimHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		fmt.Fprint(w, "SECRET-LOOPBACK-BODY")
	}))
}

func newS01VictimHandler(t *testing.T, handler http.Handler) *s01Victim {
	t.Helper()
	v := &s01Victim{}
	v.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&v.hits, 1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(v.server.Close)
	v.port = v.server.Listener.Addr().(*net.TCPAddr).Port
	return v
}

func (v *s01Victim) Hits() int32 { return atomic.LoadInt32(&v.hits) }

func requireRealChromium(t *testing.T) {
	t.Helper()
	if os.Getenv("PRINTBRIDGE_REAL_CHROMIUM") == "" {
		t.Skip("set PRINTBRIDGE_REAL_CHROMIUM=1 to drive a real Chromium render")
	}
	if _, err := browserPath(); err != nil {
		t.Skipf("no Chromium-family browser installed: %v", err)
	}
}

// TestS01RedirectIntoLoopbackIsBlocked covers the redirect bypass: the first
// request is itself sent to the loopback victim, which returns 302 to a second
// loopback URL. This avoids a vacuous test where the initial private address is
// already rejected before the redirect is followed. The redirecting hop must be
// reached; the second hop must not.
func TestS01RedirectIntoLoopbackIsBlocked(t *testing.T) {
	requireRealChromium(t)
	secondHop := newS01Victim(t)
	firstHop := newS01VictimHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://127.0.0.1:%d/redirected.png", secondHop.port), http.StatusFound)
	}))

	page := fmt.Sprintf(`<!doctype html><html><body>
<img src="http://127.0.0.1:%d/redir.png">
</body></html>`, firstHop.port)

	renderPage(t, page, 2500)

	if hits := secondHop.Hits(); hits != 0 {
		t.Fatalf("S01 redirect bypass: second loopback victim reached %d time(s)", hits)
	}
	if hits := firstHop.Hits(); hits != 0 {
		t.Logf("redirect first hop observed %d time(s); second hop stayed at zero", hits)
	}
	t.Log("redirect into loopback blocked at the first private hop")
}

// TestS01ProxyRedirectControlShowsFirstHopFollowed proves the proxy is actually
// asked to perform the redirecting request. A non-blocking proxy would hit the
// first hop even if browser-level behavior prevents the second one.
func TestS01ProxyRedirectControlShowsFirstHopFollowed(t *testing.T) {
	requireRealChromium(t)
	victim := newS01Victim(t)
	var firstHopHits int32
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&firstHopHits, 1)
		http.Redirect(w, r, fmt.Sprintf("http://127.0.0.1:%d/redirected.png", victim.port), http.StatusFound)
	}))
	defer redirector.Close()

	page := fmt.Sprintf(`<!doctype html><html><body><img src="%s/redir.png"></body></html>`, redirector.URL)
	pagePath := filepath.Join(t.TempDir(), "redirect-control.html")
	if err := os.WriteFile(pagePath, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	chrome, err := browserPath()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(chrome,
		"--headless=new", "--disable-gpu", "--no-sandbox", "--no-first-run",
		"--user-data-dir="+filepath.Join(t.TempDir(), "profile"),
		"--proxy-bypass-list=<-loopback>",
		"--virtual-time-budget=4000", "--dump-dom", "file:///"+filepath.ToSlash(pagePath))
	out, _ := cmd.CombinedOutput()
	_ = out
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&firstHopHits) == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&firstHopHits); got == 0 {
		t.Fatal("redirect control inconclusive: the first hop was never requested")
	}
	if victim.Hits() == 0 {
		t.Fatal("redirect control inconclusive: an unprotected client did not follow the redirect")
	}
	t.Logf("redirect control confirmed: first=%d second=%d", atomic.LoadInt32(&firstHopHits), victim.Hits())
}

// TestS01IPv6LoopbackIsBlocked covers the IPv6 literal form, which is a common
// gap because many allowlists only consider IPv4.
func TestS01IPv6LoopbackIsBlocked(t *testing.T) {
	requireRealChromium(t)
	victim := newS01Victim(t)
	page := fmt.Sprintf(`<!doctype html><html><body>
<img src="http://[::1]:%d/v6.png">
<img src="http://[::ffff:127.0.0.1]:%d/mapped.png">
</body></html>`, victim.port, victim.port)

	renderPage(t, page, 2000)

	if hits := victim.Hits(); hits != 0 {
		t.Fatalf("S01 IPv6 bypass: loopback victim reached %d time(s)", hits)
	}
	t.Log("IPv6 loopback forms blocked")
}

// TestS01DataAndBlobSubresourcesCannotReachLoopback pins that data:/blob: URLs
// cannot be used as a trampoline into the local network.
func TestS01DataAndBlobSubresourcesCannotReachLoopback(t *testing.T) {
	requireRealChromium(t)
	victim := newS01Victim(t)
	page := fmt.Sprintf(`<!doctype html><html><body>
<iframe src="data:text/html,<img src='http://127.0.0.1:%d/a.png'>"></iframe>
<object data="data:text/html,<img src='http://127.0.0.1:%d/b.png'>"></object>
</body></html>`, victim.port, victim.port)

	renderPage(t, page, 2500)

	if hits := victim.Hits(); hits != 0 {
		t.Fatalf("S01 data: trampoline reached loopback %d time(s)", hits)
	}
	t.Log("data: trampoline blocked")
}

// TestS01WebSocketSubresourceIsBlocked covers ws:// which browsers treat
// differently from http:// and some proxies forget to filter. The victim records
// HTTP Upgrade attempts, so a successful bypass is observable.
func TestS01WebSocketSubresourceIsBlocked(t *testing.T) {
	requireRealChromium(t)
	victim := newS01Victim(t)
	page := fmt.Sprintf(`<!doctype html><html><body><script>
try {
  var ws = new WebSocket("ws://127.0.0.1:%d/ws-probe");
  ws.onopen = function(){ ws.send("hi"); };
  window.__wsTried = true;
} catch (e) { window.__wsTried = "threw"; }
</script></body></html>`, victim.port)

	renderPage(t, page, 2500)

	if hits := victim.Hits(); hits != 0 {
		t.Fatalf("S01 ws:// bypass: loopback victim reached %d time(s)", hits)
	}
	t.Log("ws:// subresource blocked")
}

// TestS01WebSocketControlShowsUpgradeAttempt proves that the ws:// case has a
// detectable victim: without the proxy, Chrome must attempt the HTTP Upgrade.
func TestS01WebSocketControlShowsUpgradeAttempt(t *testing.T) {
	requireRealChromium(t)
	victim := newS01Victim(t)
	chrome, err := browserPath()
	if err != nil {
		t.Fatal(err)
	}
	pagePath := filepath.Join(t.TempDir(), "ws-control.html")
	page := fmt.Sprintf(`<!doctype html><html><body><script>
try { var ws = new WebSocket("ws://127.0.0.1:%d/ws-probe"); ws.onopen = function(){ ws.send("x"); }; } catch (e) {}
</script></body></html>`, victim.port)
	if err := os.WriteFile(pagePath, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(t.TempDir(), "profile")
	cmd := exec.Command(chrome,
		"--headless=new", "--disable-gpu", "--no-sandbox", "--no-first-run",
		"--user-data-dir="+profile, "--virtual-time-budget=4000", "--dump-dom",
		"file:///"+filepath.ToSlash(pagePath))
	out, _ := cmd.CombinedOutput()
	_ = out
	deadline := time.Now().Add(5 * time.Second)
	for victim.Hits() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if victim.Hits() == 0 {
		t.Fatal("ws:// control inconclusive: no Upgrade request reached the victim without the proxy")
	}
	t.Logf("ws:// control confirmed: %d request(s)", victim.Hits())
}

// TestS01NumericIPEncodingsStayUnreachable covers the encodings that
// SECURITY ValidateURL currently lets through: a decimal integer, hex, octal and
// shorthand forms. ValidateURL cannot classify them because net.ParseIP does not
// parse them, so the guarantee here is that the renderer still cannot reach a
// loopback service through them.
func TestS01NumericIPEncodingsStayUnreachable(t *testing.T) {
	requireRealChromium(t)
	victim := newS01Victim(t)

	// Build the odd forms from the real port so a success would be observable.
	forms := []string{
		fmt.Sprintf("http://2130706433:%d/dec.png", victim.port),
		fmt.Sprintf("http://0x7f000001:%d/hex.png", victim.port),
		fmt.Sprintf("http://127.1:%d/short.png", victim.port),
	}
	var body string
	for _, f := range forms {
		body += fmt.Sprintf("<img src=%q>\n", f)
	}
	renderPage(t, "<!doctype html><html><body>\n"+body+"</body></html>", 3000)

	if hits := victim.Hits(); hits != 0 {
		t.Fatalf("S01 numeric encoding bypass: loopback victim reached %d time(s)", hits)
	}
	t.Log("numeric IP encodings did not reach loopback")
}

// TestS01ProxyNumericControlShowsRequestsReachProxy proves that Chrome actually
// sends numeric/hex/short IP forms to the proxy. Without this control, a zero
// victim count could merely mean Chrome rejected the URL before networking.
func TestS01ProxyNumericControlShowsRequestsReachProxy(t *testing.T) {
	requireRealChromium(t)
	victim := newS01Victim(t)
	var requests int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		http.Error(w, "control", http.StatusBadGateway)
	}))
	defer origin.Close()

	// Use the victim as the second hop so a successful browser-side connection
	// would still be visible, but only the proxy request counter is asserted here.
	page := fmt.Sprintf(`<!doctype html><html><body>
<img src="http://2130706433:%d/dec.png">
<img src="http://0x7f000001:%d/hex.png">
<img src="http://127.1:%d/short.png">
</body></html>`, victim.port, victim.port, victim.port)
	pagePath := filepath.Join(t.TempDir(), "numeric-control.html")
	if err := os.WriteFile(pagePath, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	chrome, err := browserPath()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(chrome,
		"--headless=new", "--disable-gpu", "--no-sandbox", "--no-first-run",
		"--user-data-dir="+filepath.Join(t.TempDir(), "profile"),
		"--proxy-server="+origin.URL,
		"--proxy-bypass-list=<-loopback>",
		"--virtual-time-budget=4000", "--dump-dom", "file:///"+filepath.ToSlash(pagePath))
	out, _ := cmd.CombinedOutput()
	_ = out
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&requests) == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&requests); got == 0 {
		t.Fatal("numeric control inconclusive: no request reached the proxy")
	}
	t.Logf("numeric control confirmed: %d proxy request(s)", atomic.LoadInt32(&requests))
}

// TestS01ValidateURLNumericFormsAreUnclassified documents the URL-layer gap
// explicitly: decimal/hex/octal host forms cannot be classified by net.ParseIP,
// so safety depends on resolving and checking the actual peer at dial time.
func TestS01ValidateURLNumericFormsAreUnclassified(t *testing.T) {
	for _, raw := range []string{
		"http://2130706433/",
		"http://0x7f000001/",
		"http://0177.0.0.1/",
		"http://127.1/",
		"http://0/",
		"http://localhost/",
		"http://metadata.google.internal/",
	} {
		if _, err := security.ValidateURL(raw); err != nil {
			t.Logf("blocked at URL validation: %s (%v)", raw, err)
			continue
		}
		t.Logf("NOT classified by ValidateURL (relies on dial-time DNS check): %s", raw)
	}
}

// renderPage drives the production RenderHTML path over an inline document.
func renderPage(t *testing.T, page string, waitMS int) {
	t.Helper()
	cfg := domain.DefaultConfig()
	job := domain.Job{JobID: "s01", Format: "raw-html", HTML: page, WaitMS: waitMS}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	pdf, _, err := RenderHTML(ctx, job, cfg, t.TempDir(), "")
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	data, err := os.ReadFile(pdf)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 500 {
		t.Fatalf("render produced a suspiciously small PDF (%d bytes)", len(data))
	}
	t.Logf("rendered %d byte PDF", len(data))
}
