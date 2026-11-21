package convert

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

// This opt-in probe distinguishes an HTTPS tunnel failure from a Chromium
// render failure when checking a public HTML fixture.
func TestHTMLProxyPublicHTTPSFixture(t *testing.T) {
	raw := os.Getenv("PRINTBRIDGE_MATRIX_HTML_URL")
	if raw == "" {
		t.Skip("set PRINTBRIDGE_MATRIX_HTML_URL to a public HTTPS HTML fixture")
	}
	p, err := newSafeProxy("")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	proxyURL, _ := url.Parse("http://" + p.Addr())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 25 * time.Second}
	resp, err := client.Get(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(data) == 0 {
		t.Fatalf("status %d, bytes %d", resp.StatusCode, len(data))
	}
	t.Logf("proxy HTTPS fixture status %d, bytes %d", resp.StatusCode, len(data))
}
