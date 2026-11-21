package convert

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/security"
)

// browserPath resolves the Chromium-compatible binary used to render HTML. It
// is a seam so tests can exercise the "no browser installed" classification on
// a machine that happens to have one, and vice versa.
var browserPath = discoverChromePath

func discoverChromePath() (string, error) {
	names := []string{"chrome", "chromium", "chromium-browser"}
	if runtime.GOOS == "windows" {
		names = []string{"msedge.exe", "chrome.exe", "chromium.exe"}
	}
	if runtime.GOOS == "linux" {
		// A Google Chrome install on Linux normally ships only these names. The
		// the locator probes them first; missing them reports a working
		// install as "renderer-unavailable".
		names = []string{"google-chrome-stable", "google-chrome", "chromium", "chromium-browser", "chrome"}
	}
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	if runtime.GOOS == "windows" {
		for _, root := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
			if root == "" {
				continue
			}
			for _, rel := range []string{
				`Microsoft\Edge\Application\msedge.exe`,
				`Google\Chrome\Application\chrome.exe`,
				`Chromium\Application\chrome.exe`,
			} {
				p := filepath.Join(root, rel)
				if _, err := os.Stat(p); err == nil {
					return p, nil
				}
			}
		}
	}
	if runtime.GOOS == "darwin" {
		roots := []string{"/Applications"}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			roots = append(roots, filepath.Join(home, "Applications"))
		}
		for _, root := range roots {
			for _, rel := range []string{"Google Chrome.app/Contents/MacOS/Google Chrome", "Chromium.app/Contents/MacOS/Chromium"} {
				if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
					return filepath.Join(root, rel), nil
				}
			}
		}
	}
	// Clients carry no renderer-specific code, so a missing browser is
	// reported as INTERNAL_ERROR. The "renderer-unavailable: " prefix is part
	// of the contract with the agent layer and the docs, so it stays verbatim.
	return "", domain.NewProtocolError(domain.CodeInternalError, "renderer-unavailable: Chromium browser not installed")
}

// rendererUnavailablePrefix marks the message of a missing-browser failure. The
// agent layer must keep recognizing it even though the wire code is
// INTERNAL_ERROR.
const rendererUnavailablePrefix = "renderer-unavailable: "

// rendererUnavailable builds the classified missing-browser error. Anything the
// browser probe reports (including a bare "not found" from a stubbed or
// platform-specific lookup) is normalized here so the prefix and the code stay
// stable for callers.
func rendererUnavailable(cause error) error {
	return domain.NewProtocolError(domain.CodeInternalError, rendererUnavailablePrefix+"Chromium browser not installed").WithCause(cause)
}

// IsRendererUnavailable reports whether err means "no Chromium/Edge installed".
func IsRendererUnavailable(err error) bool {
	return err != nil && strings.HasPrefix(domain.MessageOf(err), rendererUnavailablePrefix)
}

// DefaultHTMLWaitMS is the settle delay applied when a job omits wait_ms.
//
// Zero is the wrong default: a page whose content is drawn by script, web fonts
// or lazy-loaded images finishes its load event before that content exists, so
// the PDF comes out blank in those regions. The earlier implementation used the
// same one-second default for the same reason.
const DefaultHTMLWaitMS = 1000

// MaxHTMLWaitMS bounds wait_ms. The protocol rejects anything above it.
const MaxHTMLWaitMS = 30_000

// RenderHTML renders one HTML job to a PDF.
//
// allowedLoopbackOrigin carries the exact loopback origin this render may reach,
// when the caller established that the page URL and the WebSocket handshake
// Origin agree. An empty value means "no loopback access", which is the default
// for every job whose source is not a matching same-origin local page.
func RenderHTML(ctx context.Context, job domain.Job, cfg domain.Config, dir string, allowedLoopbackOrigin string) (string, []byte, error) {
	// Validate the request before probing for a browser: an oversized body or
	// a blocked URL is a client error that must not depend on what is
	// installed on the host, and it must not launch Chromium.
	var url string
	if job.Format == "raw-html" {
		if len(job.HTML) > cfg.Limits.MaxFileSizeMB*1024*1024 {
			return "", nil, domain.NewProtocolErrorf(domain.CodeFileTooLarge, "HTML document exceeds the %d MB limit", cfg.Limits.MaxFileSizeMB)
		}
		url = "data:text/html;charset=utf-8;base64," + base64.StdEncoding.EncodeToString([]byte(job.HTML))
	} else {
		if _, err := security.ValidateURLWithLoopback(job.FileURL, allowedLoopbackOrigin); err != nil {
			return "", nil, security.DownloadFailure(err)
		}
		url = job.FileURL
	}
	chrome, err := browserPath()
	if err != nil {
		var pe *domain.ProtocolError
		if errors.As(err, &pe) {
			return "", nil, err
		}
		return "", nil, rendererUnavailable(err)
	}
	proxy, err := newSafeProxy(allowedLoopbackOrigin)
	if err != nil {
		return "", nil, domain.NewProtocolErrorf(domain.CodeInternalError, "start HTML renderer proxy: %v", err).WithCause(err)
	}
	defer proxy.Close()
	profile := filepath.Join(dir, "chrome-profile")
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx,
		chromedp.ExecPath(chrome), chromedp.UserDataDir(profile), chromedp.Headless,
		chromedp.ProxyServer("http://"+proxy.Addr()),
		// Chromium bypasses the proxy for loopback by default. That implicit
		// bypass is what lets a same-origin loopback page load its own images:
		// forcing loopback through the proxy with "<-loopback>" makes Chromium
		// drop loopback sub-resources entirely, producing a PDF that is silently
		// missing them. Loopback is therefore dialled directly and the policy is
		// enforced on the dial itself (see isAllowedLoopbackDial), which refuses
		// every loopback target except the one authorised origin.
		chromedp.Flag("proxy-bypass-list", "localhost,127.0.0.1,::1"),
		chromedp.Flag("disable-quic", true), chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("no-first-run", true))
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()
	limit, cancel := context.WithTimeout(browserCtx, 90*time.Second)
	defer cancel()
	// A job that omits wait_ms must still settle: see DefaultHTMLWaitMS.
	waitMS := job.WaitMS
	if waitMS <= 0 {
		waitMS = DefaultHTMLWaitMS
	}
	if waitMS > MaxHTMLWaitMS {
		waitMS = MaxHTMLWaitMS
	}
	// Reset the block record immediately before navigation so browser background
	// traffic that preceded the page cannot be attributed to it.
	proxy.resetBlocks()
	// The guard enforces the loopback policy on the requests Chromium makes
	// directly, since those bypass the proxy.
	guard := newLoopbackGuard(allowedLoopbackOrigin, proxy)
	stopGuard, err := guard.start(limit)
	if err != nil {
		return "", nil, renderFailure("install request guard", err)
	}
	defer stopGuard()
	if err := chromedp.Do(limit, chromedp.Navigate(url), chromedp.Sleep(time.Duration(waitMS)*time.Millisecond)); err != nil {
		return "", nil, renderFailure("load page", err)
	}
	paper := chromedp.PaperA4
	if job.Paper != nil {
		paper = chromedp.PaperSize{Width: job.Paper.WidthMM / 25.4, Height: job.Paper.HeightMM / 25.4}
	}
	b, err := chromedp.Run(limit, chromedp.PrintToPDF(chromedp.PDFPaper(paper), chromedp.PDFMargin(0), chromedp.PDFPrintBackground()))
	if err != nil {
		return "", nil, renderFailure("print to PDF", err)
	}
	// A page that exceeded the sub-resource budget produced a PDF missing content.
	// Reporting success would hand the caller a silently incomplete printout, so
	// the exhaustion is surfaced as a failure instead.
	if proxy.exhaustedNow() {
		return "", nil, domain.NewProtocolErrorf(
			domain.CodeFileTooLarge,
			"HTML page exceeded the resource budget (%d sub-resources, %d bytes) and was refused",
			maxPageResources, maxPageBytes)
	}
	// Same reasoning for a policy refusal: the block itself is the security
	// outcome we want, but shipping a PDF that silently lacks the refused asset
	// would report success for an incomplete printout.
	if target := guard.blockedURL(); target != "" {
		return "", nil, domain.NewProtocolErrorf(
			domain.CodeDownloadFailed,
			"HTML page requested a blocked sub-resource (%s); the render was refused rather than printed incomplete",
			target)
	}
	if target := proxy.blockedResource(); target != "" {
		return "", nil, domain.NewProtocolErrorf(
			domain.CodeDownloadFailed,
			"HTML page requested a blocked sub-resource (%s); the render was refused rather than printed incomplete",
			target)
	}
	p := filepath.Join(dir, "html.pdf")
	if err := os.WriteFile(p, b, 0600); err != nil {
		return "", nil, domain.NewProtocolErrorf(domain.CodeInternalError, "write rendered PDF: %v", err).WithCause(err)
	}
	if err := VerifyPDF(p); err != nil {
		return "", nil, err
	}
	return p, nil, nil
}

// renderFailure classifies a Chromium failure. A timed-out or crashed render is
// an internal failure for protocol purposes because the SDK has no dedicated
// render code; any classification already present on the cause is preserved. A
// cancelled parent context stays a plain cancellation so the agent can report
// `cancelled` instead of `failed`.
func renderFailure(stage string, err error) error {
	var pe *domain.ProtocolError
	if errors.As(err, &pe) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return domain.NewProtocolErrorf(domain.CodeInternalError, "renderer %s failed: %v", stage, err).WithCause(err)
}

// Resource bounds for one rendered page. Without them a hostile page can make the
// renderer fetch thousands of sub-resources or hold connections open until the
// whole render times out, which is a local resource-consumption attack even when
// every individual address is public.
const (
	// maxPageResources caps how many sub-resource requests one render may proxy.
	maxPageResources = 512
	// maxPageBytes caps the total sub-resource payload for one render.
	maxPageBytes = 64 << 20
	// maxSingleResourceBytes caps one proxied response body.
	maxSingleResourceBytes = 20 << 20
	// pageResourceTimeout bounds a single sub-resource transfer.
	pageResourceTimeout = 20 * time.Second
)

type safeProxy struct {
	listener net.Listener
	server   *http.Server

	// mu guards the budget counters; they are shared by concurrent sub-resource
	// requests belonging to the same rendered page.
	mu        sync.Mutex
	resources int
	bytes     int64
	exhausted bool

	// blocked records the first sub-resource the policy refused. Serving a 403
	// to Chromium is not enough: the render would still succeed and produce a
	// PDF silently missing that resource, so the render is failed instead.
	blocked string

	// allowedLoopbackOrigin is the one loopback origin this render may reach, or
	// empty when no same-origin local page was authorised for this job.
	allowedLoopbackOrigin string
}

func (p *safeProxy) Addr() string { return p.listener.Addr().String() }
func (p *safeProxy) Close()       { _ = p.server.Close(); _ = p.listener.Close() }

// chargeResource reserves one sub-resource request against the page budget.
func (p *safeProxy) chargeResource() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exhausted {
		return false
	}
	p.resources++
	if p.resources > maxPageResources {
		p.exhausted = true
		return false
	}
	return true
}

// chargeBytes adds a delivered payload to the page budget.
func (p *safeProxy) chargeBytes(n int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exhausted {
		return false
	}
	p.bytes += n
	if p.bytes > maxPageBytes {
		p.exhausted = true
		return false
	}
	return true
}

// Stats reports the observed resource count and byte total for diagnostics and
// for tests asserting that a bound was actually exercised.
func (p *safeProxy) Stats() (resources int, bytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resources, p.bytes
}

// exhaustedNow reports whether a budget refused a sub-resource during this render.
func (p *safeProxy) exhaustedNow() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exhausted
}

// recordBlock remembers the first refused sub-resource for this render.
func (p *safeProxy) recordBlock(target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.blocked == "" {
		p.blocked = target
	}
}

// blockedResource returns the first sub-resource the policy refused, if any.
func (p *safeProxy) blockedResource() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.blocked
}

// resetBlocks clears the record before navigation.
func (p *safeProxy) resetBlocks() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocked = ""
}

func newSafeProxy(allowedLoopbackOrigin string) (*safeProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &safeProxy{listener: ln, allowedLoopbackOrigin: allowedLoopbackOrigin}
	p.server = &http.Server{Handler: http.HandlerFunc(p.handle), ReadHeaderTimeout: 5 * time.Second}
	go p.server.Serve(ln)
	return p, nil
}
func (p *safeProxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if _, err := security.ValidateURLWithLoopback(r.URL.String(), p.allowedLoopbackOrigin); err != nil {
		p.recordBlock(r.URL.String())
		http.Error(w, "blocked resource", http.StatusForbidden)
		return
	}
	if !p.chargeResource() {
		http.Error(w, "resource budget exhausted", http.StatusTooManyRequests)
		return
	}
	req := r.Clone(r.Context())
	req.RequestURI = ""
	// Hop-by-hop headers are meaningful only for one connection; forwarding them
	// to the origin produces a malformed request or a broken response.
	for _, h := range []string{"Proxy-Connection", "Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		req.Header.Del(h)
	}
	ctx, cancel := context.WithTimeout(req.Context(), pageResourceTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := security.Client(pageResourceTimeout).Do(req)
	if err != nil {
		http.Error(w, "resource unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Read one byte past the per-resource cap so an over-size body is detected
	// instead of being written half-way and reported as a complete resource.
	limited := io.LimitReader(resp.Body, maxSingleResourceBytes+1)
	copied, _ := io.Copy(w, limited)
	if copied > maxSingleResourceBytes {
		// The body was already partly written, so the resource cannot be saved;
		// record it as a failed render rather than shipping a truncated asset.
		p.recordBlock(r.URL.String() + " (exceeds the per-resource byte limit)")
		p.chargeBytes(copied)
		return
	}
	p.chargeBytes(copied)
}

// isLoopbackHost reports whether host is a literal loopback address or the
// well-known localhost name.
func (p *safeProxy) isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isAllowedLoopbackDial reports whether dialing this exact host:port is the
// authorised same-origin loopback target.
func (p *safeProxy) isAllowedLoopbackDial(host, port string) bool {
	if p.allowedLoopbackOrigin == "" {
		return false
	}
	allowed, err := url.Parse(p.allowedLoopbackOrigin)
	if err != nil || allowed == nil {
		return false
	}
	allowedPort := allowed.Port()
	if allowedPort == "" {
		if allowed.Scheme == "https" {
			allowedPort = "443"
		} else {
			allowedPort = "80"
		}
	}
	return strings.EqualFold(allowed.Hostname(), host) && allowedPort == port
}

func (p *safeProxy) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || port != "443" {
		// No port in the authority is normal for CONNECT (implied 443).
		if err == nil && r.Host != "" && !strings.Contains(r.Host, ":") {
			host, port = r.Host, "443"
		} else {
			p.recordBlock("CONNECT " + r.Host)
			http.Error(w, "blocked CONNECT", http.StatusForbidden)
			return
		}
	}
	// CONNECT carries nearly every HTTPS sub-resource, so it must be charged
	// against the same page budget as plain HTTP. Without this, the resource
	// count and byte caps only govern the minority of pages served over http.
	if !p.chargeResource() {
		http.Error(w, "resource budget exhausted", http.StatusTooManyRequests)
		return
	}
	// A same-origin loopback page may pull HTTPS assets from that same origin.
	// Dialing those through DialPublic would be refused by design, so the exact
	// authorised origin is dialled directly. Every other loopback target stays
	// blocked, and every public target still goes through the address guard.
	if p.isLoopbackHost(host) && !p.isAllowedLoopbackDial(host, port) {
		p.recordBlock("https://" + net.JoinHostPort(host, port))
		http.Error(w, "blocked address", http.StatusForbidden)
		return
	}
	var remote net.Conn
	if p.isAllowedLoopbackDial(host, port) {
		remote, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(r.Context(), "tcp", net.JoinHostPort(host, port))
	} else {
		remote, err = security.DialPublic(r.Context(), "tcp", host, port, 10*time.Second)
	}
	if err != nil {
		if errors.Is(err, security.ErrBlockedAddress) {
			p.recordBlock("https://" + net.JoinHostPort(host, port))
			http.Error(w, "blocked address", http.StatusForbidden)
			return
		}
		http.Error(w, "connect failed", http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		remote.Close()
		http.Error(w, "proxy unavailable", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		remote.Close()
		return
	}
	_, _ = fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	// The tunnel carries TLS, so the payload cannot be inspected or truncated
	// here. Charge the bytes that actually flow through it rather than reserving
	// the per-resource cap up front: Chromium opens several background HTTPS
	// connections (telemetry, component updates, favicons) that carry almost no
	// data, and charging each one 20 MiB exhausted the whole page budget before
	// the page's own content was even requested.
	go func() { _, _ = io.Copy(remote, client); remote.Close() }()
	copied, _ := io.Copy(client, remote)
	p.chargeBytes(copied)
	client.Close()
}
