package security

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// loopbackClient speaks to an httptest server on 127.0.0.1. It exists only for
// tests: production downloads always go through Client, which refuses
// loopback/private peers at dial time.
func loopbackClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}

func TestBlockedAddressKeepsSentinelAndCode(t *testing.T) {
	_, err := Download(context.Background(), "http://127.0.0.1/", t.TempDir(), 1024, time.Second)
	if err == nil {
		t.Fatal("expected an error for a loopback URL")
	}
	// Both properties matter: the sentinel keeps the local guard testable,
	// and the wire code must be DOWNLOAD_FAILED because the SDK has no
	// address-specific code.
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("expected errors.Is(err, ErrBlockedAddress), got %v", err)
	}
	if code := domain.CodeOf(err); code != domain.CodeDownloadFailed {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeDownloadFailed, err)
	}
}

func TestInvalidURLIsDownloadFailed(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "http://user:pass@example.com/", ""} {
		_, err := Download(context.Background(), raw, t.TempDir(), 1024, time.Second)
		if code := domain.CodeOf(err); code != domain.CodeDownloadFailed {
			t.Fatalf("%q: code = %q, want %q", raw, code, domain.CodeDownloadFailed)
		}
	}
}

func TestDotLocalHostIsDownloadFailed(t *testing.T) {
	_, err := Download(context.Background(), "http://printer.local/file.pdf", t.TempDir(), 1024, time.Second)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("expected blocked address sentinel, got %v", err)
	}
	if code := domain.CodeOf(err); code != domain.CodeDownloadFailed {
		t.Fatalf("code = %q, want %q", code, domain.CodeDownloadFailed)
	}
}

func TestOversizedContentLengthIsFileTooLarge(t *testing.T) {
	const maxBytes int64 = 1024
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Declare a length far above the limit; the guard must reject before
		// any of the body is written.
		w.Header().Set("Content-Length", "104857600")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	_, err := transfer(context.Background(), loopbackClient(), server.URL, t.TempDir(), maxBytes)
	if err == nil {
		t.Fatal("expected a size error")
	}
	if code := domain.CodeOf(err); code != domain.CodeFileTooLarge {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeFileTooLarge, err)
	}
	if !strings.Contains(err.Error(), "1024") {
		t.Fatalf("error should name the limit: %v", err)
	}
}

func TestOversizedStreamIsFileTooLarge(t *testing.T) {
	const maxBytes int64 = 64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Content-Length (chunked): only the running total can catch this.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("A", 4096)))
	}))
	defer server.Close()

	dir := t.TempDir()
	_, err := transfer(context.Background(), loopbackClient(), server.URL, dir, maxBytes)
	if code := domain.CodeOf(err); code != domain.CodeFileTooLarge {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeFileTooLarge, err)
	}
	// The partial file must not survive the rejection.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read temp dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("partial download left behind: %v", entries)
	}
}

func TestDownloadTimeoutCleansPartialFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := loopbackClient()
	client.Timeout = 100 * time.Millisecond
	dir := t.TempDir()
	_, err := transfer(context.Background(), client, server.URL, dir, 1024)
	if code := domain.CodeOf(err); code != domain.CodeDownloadFailed {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeDownloadFailed, err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("partial download remained: %v, read error: %v", entries, readErr)
	}
}

func TestNon2xxIsDownloadFailed(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusForbidden} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		_, err := transfer(context.Background(), loopbackClient(), server.URL, t.TempDir(), 1024)
		server.Close()
		if code := domain.CodeOf(err); code != domain.CodeDownloadFailed {
			t.Fatalf("status %d: code = %q, want %q", status, code, domain.CodeDownloadFailed)
		}
		if !strings.Contains(err.Error(), http.StatusText(status)) && !strings.Contains(err.Error(), strconv.Itoa(status)) {
			t.Fatalf("status %d: message should carry the status: %v", status, err)
		}
	}
}

func TestSuccessfulDownloadWritesFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("%PDF-1.4\npayload"))
	}))
	defer server.Close()

	path, err := transfer(context.Background(), loopbackClient(), server.URL, t.TempDir(), 1<<20)
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	body, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read downloaded file: %v", readErr)
	}
	if string(body) != "%PDF-1.4\npayload" {
		t.Fatalf("unexpected body %q", string(body))
	}
}

func TestWriteFailureIsInternalError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data"))
	}))
	defer server.Close()

	// A path that does not exist makes os.CreateTemp fail.
	_, err := transfer(context.Background(), loopbackClient(), server.URL, filepath.Join("no-such-dir-printbridge", "nested"), 1<<20)
	if code := domain.CodeOf(err); code != domain.CodeInternalError {
		t.Fatalf("code = %q, want %q (err: %v)", code, domain.CodeInternalError, err)
	}
}

func TestDownloadFailurePreservesExistingClassification(t *testing.T) {
	// A classified error must not be re-wrapped into a different code by the
	// helpers used at the call sites.
	original := domain.NewProtocolError(domain.CodeFileTooLarge, "already classified")
	if got := DownloadFailure(original); got != error(original) {
		t.Fatalf("expected the original error to pass through, got %v", got)
	}
	if code := domain.CodeOf(DownloadFailure(original)); code != domain.CodeFileTooLarge {
		t.Fatalf("code = %q, want %q", code, domain.CodeFileTooLarge)
	}
}

func TestPublicIPGuard(t *testing.T) {
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !PublicIP(net.ParseIP(raw)) {
			t.Errorf("rejected public address %s", raw)
		}
	}
}
