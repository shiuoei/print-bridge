package security

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// ErrBlockedAddress reports that a URL resolved to a private, loopback or
// otherwise non-public address. Callers keep matching it with errors.Is even
// though the protocol-visible classification of that failure is
// DOWNLOAD_FAILED, because the SDK has no address-specific code.
var ErrBlockedAddress = errors.New("blocked private or local address")

// downloadChunkBytes bounds the working buffer of the streaming copy, not the
// download itself. Files larger than maxBytes are rejected while copying.
const downloadChunkBytes = 32 * 1024

// lookupIP is a test seam only. Production uses the system resolver; a test can
// map a public-looking hostname to a local fixture without weakening the address
// checks that run on every result.
var lookupIP = net.DefaultResolver.LookupIP

// blockedHostNames are hostnames that are known to resolve to local or internal
// endpoints, or that serve cloud instance metadata. They are refused at the URL
// layer so the decision does not depend on how a particular resolver answers at
// dial time. DNS is still checked for every other hostname.
var blockedHostNames = map[string]bool{
	"localhost":                true,
	"localhost.localdomain":    true,
	"ip6-localhost":            true,
	"ip6-loopback":             true,
	"metadata":                 true,
	"metadata.google.internal": true,
	"metadata.goog":            true,
	"instance-data":            true,
}

// blockedHostSuffixes cover resolution services that deliberately map arbitrary
// names onto internal addresses (for example 127.0.0.1.nip.io) plus mDNS names.
var blockedHostSuffixes = []string{
	".localhost",
	".local",
	".localdomain",
	".internal",
	".home.arpa",
}

// isBlockedHostName reports whether a hostname is known-local by name alone.
// It is case-insensitive and tolerates the trailing dot of a fully qualified
// name, so "LocalHost." cannot slip through.
func isBlockedHostName(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if name == "" {
		return true
	}
	if blockedHostNames[name] {
		return true
	}
	for _, suffix := range blockedHostSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func PublicIP(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified() && !ip.IsInterfaceLocalMulticast() && ip.IsGlobalUnicast()
}

// ValidateURL rejects anything that is not a plain public HTTP(S) URL. A host
// that is a literal private/loopback address is rejected here rather than at
// dial time so callers get a stable DOWNLOAD_FAILED classification before any
// connection is attempted; hostnames still have to be resolved at dial time.
func ValidateURL(raw string) (*url.URL, error) {
	return ValidateURLWithLoopback(raw, "")
}

// ValidateURLWithLoopback is ValidateURL plus a same-origin loopback exception.
//
// The business system may be a local web app served from http://127.0.0.1:PORT,
// and it must be able to print its own pages. Allowing every loopback target
// would reopen the SSRF hole this package exists to close, so the exception is
// narrow: the URL's origin must equal allowedOrigin exactly, and that origin
// must itself be an exact loopback origin supplied by the caller from a trusted
// source (the WebSocket handshake Origin, already validated against the
// allowlist). Sub-resources of such a page are permitted only when they share
// that exact origin.
func ValidateURLWithLoopback(raw, allowedOrigin string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("only public HTTP(S) URLs are allowed")
	}
	if allowedOrigin != "" && sameLoopbackOrigin(u, allowedOrigin) {
		return u, nil
	}
	// Known-local names are refused by name so the verdict does not depend on a
	// resolver answer at dial time.
	if isBlockedHostName(u.Hostname()) {
		return nil, ErrBlockedAddress
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !PublicIP(ip) {
		return nil, ErrBlockedAddress
	}
	return u, nil
}

// sameLoopbackOrigin reports whether u has exactly the origin named by
// allowedOrigin, and that origin is itself an exact loopback origin.
//
// The second condition is what keeps this exception from becoming a general
// SSRF bypass: without it, a caller could pass "http://10.0.0.5" or
// "http://169.254.169.254" as the "allowed origin" and reach the private network.
func sameLoopbackOrigin(u *url.URL, allowedOrigin string) bool {
	if !IsExactLoopbackOrigin(allowedOrigin) {
		return false
	}
	allowed, err := url.Parse(allowedOrigin)
	if err != nil || allowed == nil {
		return false
	}
	return strings.EqualFold(u.Scheme, allowed.Scheme) &&
		strings.EqualFold(u.Host, allowed.Host)
}

// SameLoopbackOrigin reports whether raw targets exactly the authorised loopback
// origin. It is exported so the HTML renderer's request guard and the downloader
// share one definition of "same origin" instead of each re-deriving it.
func SameLoopbackOrigin(raw, allowedOrigin string) bool {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return false
	}
	return sameLoopbackOrigin(u, allowedOrigin)
}

// IsExactLoopbackOrigin reports whether raw is an http(s) origin whose host is
// one of the two canonical loopback addresses (127.0.0.1 or ::1) and which has no
// path, query or fragment. Only such an origin may be used as the loopback
// exception.
//
// The canonical forms are matched literally rather than via net.IP.IsLoopback:
// the whole 127.0.0.0/8 range is loopback, but only 127.0.0.1 is what a local web
// app is actually served from, and accepting the rest would widen the exception
// for no benefit.
func IsExactLoopbackOrigin(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Path != "" && u.Path != "/" {
		return false
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "::1"
}

func Client(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			return DialPublic(ctx, network, host, port, timeout)
		},
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		_, err := ValidateURL(req.URL.String())
		return err
	}}
}

// DialPublic validates every DNS answer before connecting, then tries each
// public address. This keeps the private-address guard while tolerating a host
// whose first IPv6 or IPv4 address is unreachable on the current network.
func DialPublic(ctx context.Context, network, host, port string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialer := &net.Dialer{Timeout: min(timeout, 3*time.Second)}
	return dialPublicWith(ctx, network, host, port, timeout, dialer.DialContext)
}

func dialPublicWith(ctx context.Context, network, host, port string, timeout time.Duration, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	limit, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ips, err := lookupIP(limit, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, ErrBlockedAddress
	}
	for _, ip := range ips {
		if !PublicIP(ip) {
			return nil, ErrBlockedAddress
		}
	}
	// Interleave address families so several unreachable IPv6 answers cannot
	// consume the whole deadline before trying an available IPv4 endpoint.
	var v4, v6 []net.IP
	for _, ip := range ips {
		if ip.To4() != nil {
			v4 = append(v4, ip)
		} else {
			v6 = append(v6, ip)
		}
	}
	ordered := make([]net.IP, 0, len(ips))
	for i := 0; i < len(ips); i++ {
		if i < len(v4) {
			ordered = append(ordered, v4[i])
		}
		if i < len(v6) {
			ordered = append(ordered, v6[i])
		}
	}
	var lastErr error
	for _, ip := range ordered {
		conn, err := dial(limit, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if limit.Err() != nil {
			return nil, limit.Err()
		}
	}
	return nil, fmt.Errorf("no public address reachable for %s: %w", host, lastErr)
}

// Download fetches raw into dir and returns the temporary file path. Every
// failure is returned as a classified protocol error so the job status and the
// error frame carry a stable code instead of a generic INTERNAL_ERROR.
func Download(ctx context.Context, raw, dir string, maxBytes int64, timeout time.Duration) (string, error) {
	// Validate before dialing: the scheme/host guard is cheap and refuses
	// loopback and .local literals without relying on DNS.
	if _, err := ValidateURL(raw); err != nil {
		return "", DownloadFailure(err)
	}
	return transfer(ctx, Client(timeout), raw, dir, maxBytes)
}

// transfer performs the HTTP transfer and streaming size check without
// re-validating the URL. Download owns the address guard; keeping the transfer
// separate lets tests drive a local httptest server while production traffic
// still goes through Client, which refuses non-public peers at dial time.
func transfer(ctx context.Context, client *http.Client, raw, dir string, maxBytes int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", DownloadFailure(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", DownloadFailure(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", domain.NewProtocolErrorf(domain.CodeDownloadFailed, "download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return "", TooLargeFailure(maxBytes)
	}
	f, err := os.CreateTemp(dir, "download-*")
	if err != nil {
		return "", writeFailure(err)
	}
	name := f.Name()
	saved := false
	defer func() {
		if !saved {
			_ = f.Close()
			_ = os.Remove(name)
		}
	}()
	buf := make([]byte, downloadChunkBytes)
	var written int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > maxBytes {
				return "", TooLargeFailure(maxBytes)
			}
			if _, writeErr := f.Write(buf[:n]); writeErr != nil {
				return "", writeFailure(writeErr)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", DownloadFailure(readErr)
		}
	}
	if err := f.Close(); err != nil {
		return "", writeFailure(err)
	}
	saved = true
	return filepath.Clean(name), nil
}

// DownloadFailure classifies a transfer failure as DOWNLOAD_FAILED. The cause
// chain is preserved, so errors.Is(err, ErrBlockedAddress) still reports a
// blocked address while the wire code stays DOWNLOAD_FAILED.
func DownloadFailure(err error) error {
	if err == nil {
		return nil
	}
	if already := classified(err); already != nil {
		return already
	}
	if errors.Is(err, ErrBlockedAddress) {
		return domain.NewProtocolErrorf(domain.CodeDownloadFailed, "download blocked: private or local address (%v)", err).WithCause(err)
	}
	return domain.NewProtocolErrorf(domain.CodeDownloadFailed, "download failed: %v", err).WithCause(err)
}

// TooLargeFailure reports a payload that exceeds the configured byte limit.
func TooLargeFailure(maxBytes int64) error {
	return domain.NewProtocolErrorf(domain.CodeFileTooLarge, "file exceeds the %d byte download limit", maxBytes)
}

func writeFailure(err error) error {
	return domain.NewProtocolErrorf(domain.CodeInternalError, "write download file: %v", err).WithCause(err)
}

// classified returns err when a lower layer already attached a protocol code.
func classified(err error) error {
	var pe *domain.ProtocolError
	if errors.As(err, &pe) {
		return err
	}
	return nil
}
