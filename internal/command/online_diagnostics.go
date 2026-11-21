package command

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/logging"
)

type RemoteProbe struct {
	Method     string `json:"method"`
	StatusCode int    `json:"status_code,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Response   string `json:"response,omitempty"`
	Error      string `json:"error,omitempty"`
}
type RemoteTestReport struct {
	GET  RemoteProbe `json:"get"`
	POST RemoteProbe `json:"post"`
}

func (s Service) testRemote(ctx context.Context, r *Response) error {
	c := s.Store.Config()
	u, err := url.Parse(c.Remote.EndpointURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return domain.NewProtocolError(domain.CodeConfigInvalid, "configure a valid HTTPS remote endpoint first")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := s.RemoteClient
	if client == nil {
		client = &http.Client{Timeout: 7 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	report := &RemoteTestReport{}
	report.GET = probeRemote(ctx, &copyClient, c, http.MethodGet)
	report.POST = probeRemote(ctx, &copyClient, c, http.MethodPost)
	r.RemoteTest = report
	slog.Info("remote connection test", "get_status", report.GET.StatusCode, "post_status", report.POST.StatusCode, "get_error", report.GET.Error, "post_error", report.POST.Error)
	if !probeOK(report.GET) || !probeOK(report.POST) {
		return domain.NewProtocolError(domain.CodeRemoteUnavailable, "remote connection test failed; inspect GET and POST results")
	}
	r.Status = "ok"
	r.Message = "remote GET and POST test requests succeeded; no print jobs or normal status reports were processed"
	return nil
}

func probeOK(p RemoteProbe) bool { return p.Error == "" && p.StatusCode >= 200 && p.StatusCode < 300 }

func probeRemote(ctx context.Context, client *http.Client, c domain.Config, method string) RemoteProbe {
	p := RemoteProbe{Method: method}
	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewBufferString(`{"event":"test"}`)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Remote.EndpointURL, body)
	if err != nil {
		p.Error = "invalid endpoint"
		return p
	}
	req.Header.Set("X-PrintBridge-Test", "true")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Remote.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.Remote.BearerToken)
	}
	if c.Remote.DeviceID != "" {
		req.Header.Set("X-PrintBridge-Device-Id", c.Remote.DeviceID)
	}
	if c.Remote.DeviceName != "" {
		req.Header.Set("X-PrintBridge-Device-Name", c.Remote.DeviceName)
	}
	start := time.Now()
	resp, err := client.Do(req)
	p.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		p.Error = remoteError(err)
		return p
	}
	defer resp.Body.Close()
	p.StatusCode = resp.StatusCode
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1025))
	if err != nil {
		p.Error = "response read failed"
		return p
	}
	if len(b) > 1024 {
		b = b[:1024]
	}
	response := strings.TrimSpace(string(b))
	if c.Remote.BearerToken != "" {
		response = strings.ReplaceAll(response, c.Remote.BearerToken, "[REDACTED]")
	}
	p.Response = logging.Redact(response)
	return p
}

func remoteError(err error) string {
	var netErr net.Error
	var certErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidErr x509.CertificateInvalidError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.As(err, &certErr), errors.As(err, &hostErr), errors.As(err, &invalidErr):
		return "certificate validation failed"
	default:
		return "connection failed"
	}
}

func (s Service) tailLogs(req Request, r *Response) error {
	if s.Logs == nil {
		return domain.NewProtocolError(domain.CodeInternalError, "agent log buffer unavailable")
	}
	var since time.Time
	if req.Since != "" {
		parsed, err := time.Parse(time.RFC3339, req.Since)
		if err != nil {
			return domain.NewProtocolError(domain.CodeArgumentInvalid, "since must be RFC3339")
		}
		since = parsed
	}
	level := slog.LevelDebug
	if req.Level != "" {
		if err := level.UnmarshalText([]byte(strings.ToUpper(req.Level))); err != nil {
			return domain.NewProtocolError(domain.CodeArgumentInvalid, "level must be DEBUG, INFO, WARN, or ERROR")
		}
	}
	r.Logs = s.Logs.Tail(since, level, req.Limit)
	if r.Logs == nil {
		r.Logs = []logging.Entry{}
	}
	return nil
}
