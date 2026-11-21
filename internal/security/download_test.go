package security

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestPrivateAddressDenied(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "::1", "10.1.2.3", "192.168.0.1", "169.254.169.254", "fc00::1"} {
		if PublicIP(net.ParseIP(raw)) {
			t.Errorf("allowed %s", raw)
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "http://user:pass@example.com/", "http://x.local/"} {
		if _, err := ValidateURL(raw); err == nil {
			t.Errorf("allowed %s", raw)
		}
	}
	_, err := Download(context.Background(), "http://127.0.0.1/", t.TempDir(), 1024, time.Second)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("expected blocked address, got %v", err)
	}
}
