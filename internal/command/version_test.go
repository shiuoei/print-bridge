package command

import "testing"

// The doctor report names the version the caller stamped, so the desktop build
// reports mygo.App.Version() instead of the development fallback; without one
// the environment stamp still works for the CLI.
func TestDoctorVersion(t *testing.T) {
	t.Setenv("PRINTBRIDGE_VERSION", "9.9.9")
	if got := (Service{}).version(); got != "9.9.9" {
		t.Fatalf("unstamped service version = %q, want the environment override", got)
	}
	if got := (Service{Version: "0.2.0"}).version(); got != "0.2.0" {
		t.Fatalf("stamped service version = %q, want 0.2.0", got)
	}
	t.Setenv("PRINTBRIDGE_VERSION", "")
	if got := (Service{}).version(); got != "dev" {
		t.Fatalf("versionless service = %q, want dev", got)
	}
}
