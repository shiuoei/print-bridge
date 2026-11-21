package security

import (
	"errors"
	"net"
	"testing"
)

// TestValidateURLWithLoopbackRequiresExactOrigin is the security core of the
// same-origin loopback feature: a loopback URL is allowed only when the caller
// supplies the exact origin it was authorised for.
func TestValidateURLWithLoopbackRequiresExactOrigin(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		origin  string
		allowed bool
	}{
		{"same loopback origin", "http://127.0.0.1:6688/invoice/1", "http://127.0.0.1:6688", true},
		{"same IPv6 loopback origin", "http://[::1]:6688/invoice/1", "http://[::1]:6688", true},
		{"different port", "http://127.0.0.1:6689/invoice/1", "http://127.0.0.1:6688", false},
		{"different scheme", "https://127.0.0.1:6688/invoice/1", "http://127.0.0.1:6688", false},
		{"no origin supplied", "http://127.0.0.1:6688/invoice/1", "", false},
		{"origin is a public site", "http://127.0.0.1:6688/invoice/1", "https://example.com", false},
		{"other loopback host", "http://127.0.0.2:6688/x", "http://127.0.0.1:6688", false},
		{"link-local metadata", "http://169.254.169.254/latest/meta-data/", "http://169.254.169.254", false},
		{"private network", "http://10.0.0.5/x", "http://10.0.0.5", false},
		{"public URL still works", "https://example.com/x", "", true},
		{"file scheme rejected", "file:///etc/passwd", "", false},
		{"localhost name rejected", "http://localhost:6688/x", "http://localhost:6688", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ValidateURLWithLoopback(c.raw, c.origin)
			if c.allowed && err != nil {
				t.Fatalf("%q with origin %q was rejected: %v", c.raw, c.origin, err)
			}
			if !c.allowed && err == nil {
				t.Fatalf("%q with origin %q was allowed", c.raw, c.origin)
			}
		})
	}
}

// TestValidateURLWithLoopbackStillBlocksEverythingElse makes sure the exception
// did not weaken the default policy.
func TestValidateURLWithLoopbackStillBlocksEverythingElse(t *testing.T) {
	origin := "http://127.0.0.1:6688"
	blocked := []string{
		"http://127.0.0.1:9999/",  // loopback, wrong port
		"http://192.168.1.1/",     // private
		"http://169.254.169.254/", // link-local metadata
		"http://[::1]:9999/",      // IPv6 loopback, wrong port
		"file:///etc/passwd",      // wrong scheme
		"ftp://127.0.0.1:6688/x",  // wrong scheme
	}
	for _, raw := range blocked {
		if _, err := ValidateURLWithLoopback(raw, origin); err == nil {
			t.Errorf("%q was allowed under the loopback exception", raw)
		}
	}
}

// TestIsExactLoopbackOrigin covers what the agent accepts as an authorising
// origin: a literal loopback http(s) origin with no path or credentials.
func TestIsExactLoopbackOrigin(t *testing.T) {
	cases := []struct {
		origin string
		want   bool
	}{
		{"http://127.0.0.1:6688", true},
		{"https://127.0.0.1:6688", true},
		{"http://[::1]:6688", true},
		{"http://127.0.0.1", true},
		{"http://127.0.0.1:6688/", true},
		{"http://127.0.0.1:6688/path", false},
		{"http://127.0.0.1:6688?q=1", false},
		{"http://user@127.0.0.1:6688", false},
		{"http://localhost:6688", false},
		{"http://127.0.0.2:6688", false},
		{"https://example.com", false},
		{"file:///tmp", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsExactLoopbackOrigin(c.origin); got != c.want {
			t.Errorf("IsExactLoopbackOrigin(%q) = %v, want %v", c.origin, got, c.want)
		}
	}
}

// TestLoopbackExceptionDoesNotAffectDialGuard records that the URL-level
// exception is separate from the dial-time address guard: a hostname that
// resolves to loopback is still refused by DialPublic.
func TestLoopbackExceptionDoesNotAffectDialGuard(t *testing.T) {
	_, err := DialPublic(t.Context(), "tcp", "127.0.0.1", "6688", 0)
	if err == nil {
		t.Fatal("DialPublic reached loopback")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("error = %v, want ErrBlockedAddress", err)
	}
	var netErr net.Error
	_ = netErr
}
