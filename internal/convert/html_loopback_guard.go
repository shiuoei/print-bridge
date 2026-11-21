package convert

import (
	"context"
	"net"
	"net/url"
	"strings"
	"sync"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/shiuoei/print-bridge/internal/security"
)

// Loopback request policy for the HTML renderer.
//
// Why this exists
//
// Chromium has two ways to keep a page away from loopback: route loopback
// through the proxy, or let it connect directly.
//
// Routing it through the proxy (the "<-loopback>" bypass rule) makes Chromium
// drop loopback sub-resources entirely: a page served from 127.0.0.1 printed as
// a PDF with no images at all, and reported success. Letting loopback connect
// directly keeps the page intact but removes every policy check, so a page could
// read any local port.
//
// Neither is acceptable, so the policy is enforced one level lower. Chromium
// dials loopback directly, and every request it makes is paused with the Fetch
// domain: a loopback request is continued only when it targets the one
// authorised origin, and failed otherwise. Non-loopback traffic is continued
// here and still goes through the sub-resource proxy, which applies the public
// URL rules.

// loopbackGuard intercepts requests and enforces the loopback policy.
type loopbackGuard struct {
	// allowedOrigin is the exact loopback origin this render may reach, or empty
	// when the job may not reach loopback at all.
	allowedOrigin string
	// proxy is the sub-resource proxy, consulted for the page budget.
	proxy *safeProxy

	mu sync.Mutex
	// blocked records the first refused loopback URL, so the caller can fail the
	// render instead of printing a page that silently lacks content.
	blocked string
	// paused carries intercepted requests to the handler goroutine. Listen's
	// callback must not run commands, so it only forwards.
	paused chan *fetch.EventRequestPaused
}

// newLoopbackGuard builds the guard for one render.
func newLoopbackGuard(allowedOrigin string, proxy *safeProxy) *loopbackGuard {
	return &loopbackGuard{
		allowedOrigin: allowedOrigin,
		proxy:         proxy,
		paused:        make(chan *fetch.EventRequestPaused, 256),
	}
}

// blockedURL returns the first loopback URL the policy refused.
func (g *loopbackGuard) blockedURL() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.blocked
}

// recordBlocked remembers the first refused URL.
func (g *loopbackGuard) recordBlocked(target string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blocked == "" {
		g.blocked = target
	}
}

// start enables interception and begins answering paused requests. The caller
// must call stop when the render finishes.
func (g *loopbackGuard) start(ctx context.Context) (func(), error) {
	// Only the request stage needs interception; responses pass through.
	patterns := []*fetch.RequestPattern{{
		URLPattern:   "*",
		RequestStage: fetch.RequestStageRequest,
	}}
	if _, err := chromedp.Call(ctx, fetch.Enable, fetch.EnableParams{Patterns: patterns}); err != nil {
		return nil, err
	}

	// Listen's callback must not run commands, so it only forwards each paused
	// request to the handler goroutine below.
	if err := chromedp.Listen(ctx, func(ev any) {
		paused, ok := ev.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		select {
		case g.paused <- paused:
		case <-ctx.Done():
		}
	}); err != nil {
		return nil, err
	}

	workerCtx, cancelWorker := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-workerCtx.Done():
				return
			case paused := <-g.paused:
				g.handle(workerCtx, paused)
			}
		}
	}()
	stop := func() {
		cancelWorker()
		<-done
	}
	return stop, nil
}

// handle decides one intercepted request.
func (g *loopbackGuard) handle(ctx context.Context, paused *fetch.EventRequestPaused) {
	rawURL := paused.Request.URL
	if !isLoopbackURL(rawURL) {
		// Public traffic continues to the proxy, which applies its own rules.
		_, _ = chromedp.Call(ctx, fetch.ContinueRequest, fetch.ContinueRequestParams{RequestID: paused.RequestID})
		return
	}

	// Loopback bypassed the proxy, so this is the only check it gets.
	if g.allowedOrigin == "" || !security.SameLoopbackOrigin(rawURL, g.allowedOrigin) {
		g.recordBlocked(rawURL)
		g.fail(ctx, paused.RequestID)
		return
	}
	// The authorised origin is charged against the page budget too, so a page
	// cannot use its own origin to pull an unbounded amount of data.
	if g.proxy != nil && !g.proxy.chargeResource() {
		g.recordBlocked(rawURL + " (page resource budget exhausted)")
		g.fail(ctx, paused.RequestID)
		return
	}
	_, _ = chromedp.Call(ctx, fetch.ContinueRequest, fetch.ContinueRequestParams{RequestID: paused.RequestID})
}

// fail refuses one intercepted request.
func (g *loopbackGuard) fail(ctx context.Context, id fetch.RequestID) {
	_, _ = chromedp.Call(ctx, fetch.FailRequest, fetch.FailRequestParams{
		RequestID:   id,
		ErrorReason: network.ErrorReasonBlockedByClient,
	})
}

// isLoopbackURL reports whether a URL targets a loopback host.
func isLoopbackURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
