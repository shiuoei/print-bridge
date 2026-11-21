package domain

import (
	"net"
	"os"
	"testing"
)

// TestNormalizeKeepsLoopback guards the self-lockout defect: deleting the
// loopback entry used to persist a list without it, so the local UI and CLI
// could no longer connect to their own agent.
func TestNormalizeKeepsLoopback(t *testing.T) {
	c := DefaultConfig()
	c.Security.AllowedIPs = []string{"::1", "10.0.0.1"}
	got := c.Normalize()
	if len(got.Security.AllowedIPs) != 3 {
		t.Fatalf("allowed_ips = %#v, want loopback re-injected", got.Security.AllowedIPs)
	}
	if got.Security.AllowedIPs[0] != RequiredLoopbackIP {
		t.Fatalf("loopback must come first, got %#v", got.Security.AllowedIPs)
	}
}

// TestNormalizeDropsAnyAddressEntries covers the other half: an any-address
// entry turns the per-client allowlist into a LAN-wide print service.
func TestNormalizeDropsAnyAddressEntries(t *testing.T) {
	for _, entry := range []string{"0.0.0.0/0", "::/0", "0.0.0.0", "::"} {
		c := DefaultConfig()
		c.Security.AllowedIPs = []string{entry, "10.0.0.1"}
		got := c.Normalize()
		for _, kept := range got.Security.AllowedIPs {
			if kept == entry {
				t.Fatalf("%q survived normalization: %#v", entry, got.Security.AllowedIPs)
			}
		}
		if !contains(got.Security.AllowedIPs, "10.0.0.1") {
			t.Fatalf("a legitimate entry was dropped: %#v", got.Security.AllowedIPs)
		}
	}
}

func TestNormalizeTrimsAndDeduplicates(t *testing.T) {
	c := DefaultConfig()
	c.Security.AllowedIPs = []string{"  10.0.0.1  ", "10.0.0.1", "", "10.0.0.2"}
	got := c.Normalize()
	want := []string{RequiredLoopbackIP, "10.0.0.1", "10.0.0.2"}
	if len(got.Security.AllowedIPs) != len(want) {
		t.Fatalf("allowed_ips = %#v, want %#v", got.Security.AllowedIPs, want)
	}
	for i := range want {
		if got.Security.AllowedIPs[i] != want[i] {
			t.Fatalf("allowed_ips = %#v, want %#v", got.Security.AllowedIPs, want)
		}
	}
}

// TestValidateRejectsAnyAddress keeps a direct caller of Validate from bypassing
// the normalization the store performs.
func TestValidateRejectsAnyAddress(t *testing.T) {
	c := DefaultConfig()
	c.Security.AllowedIPs = []string{RequiredLoopbackIP, "0.0.0.0/0"}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted an any-address entry")
	}
}

// TestIPAllowedAlwaysPermitsLoopback is the connection-time half of the
// self-lockout guard.
func TestIPAllowedAlwaysPermitsLoopback(t *testing.T) {
	c := DefaultConfig()
	c.Security.AllowedIPs = []string{"10.0.0.1"} // loopback deliberately absent
	if !c.IPAllowed(net.ParseIP("127.0.0.1")) {
		t.Fatal("loopback client was rejected")
	}
	if !c.IPAllowed(net.ParseIP("::1")) {
		t.Fatal("IPv6 loopback client was rejected")
	}
	if c.IPAllowed(net.ParseIP("192.168.1.5")) {
		t.Fatal("a non-allowed LAN client was accepted")
	}
}

// TestPortFloorIsNotALoadTimeInvariant records the intended layering: the CLI
// rejects ports below MinServicePort, but loading a config must not, or an
// existing installation could become unopenable after an upgrade.
func TestPortFloorIsNotALoadTimeInvariant(t *testing.T) {
	c := DefaultConfig()
	c.Service.Port = 5000
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected an out-of-floor port: %v", err)
	}
	if MinServicePort != 10000 {
		t.Fatalf("MinServicePort = %d, want 10000", MinServicePort)
	}
}

func TestValidateAcceptsHTTPRemoteEndpoint(t *testing.T) {
	c := DefaultConfig()
	c.Remote.Enabled = true
	c.Remote.EndpointURL = "http://127.0.0.1:18080/jobs"
	if err := c.Validate(); err != nil {
		t.Fatalf("HTTP endpoint rejected: %v", err)
	}
}

func TestLoadConfigFileReportsBrokenFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/broken.json"
	if err := writeFile(path, `{"service":{"port":"oops"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigFile(path); err == nil {
		t.Fatal("LoadConfigFile accepted a file with a wrong type")
	}
	if _, err := LoadConfigFile(dir + "/missing.json"); err == nil {
		t.Fatal("LoadConfigFile accepted a missing file")
	}
}

func TestLoadConfigFileNormalizes(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/partial.json"
	if err := writeFile(path, `{"security":{"allowed_ips":["0.0.0.0/0"]}}`); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Security.AllowedIPs) != 1 || got.Security.AllowedIPs[0] != RequiredLoopbackIP {
		t.Fatalf("allowed_ips = %#v, want only loopback", got.Security.AllowedIPs)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
