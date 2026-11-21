package convert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrepareLibreOfficeProfilePinsMacroSecurity is the regression guard for the
// defect where the isolated LibreOffice profile was created empty, so documents
// opened under LibreOffice's default (lower) macro security level.
func TestPrepareLibreOfficeProfilePinsMacroSecurity(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "lo-profile")
	if err := prepareLibreOfficeProfile(profile); err != nil {
		t.Fatalf("prepareLibreOfficeProfile: %v", err)
	}
	xcu := filepath.Join(profile, "user", "registrymodifications.xcu")
	data, err := os.ReadFile(xcu)
	if err != nil {
		t.Fatalf("macro security config was not written: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "MacroSecurityLevel") {
		t.Fatalf("config does not set MacroSecurityLevel:\n%s", text)
	}
	if !strings.Contains(text, "<value>3</value>") {
		t.Fatalf("macro security level is not the maximum (3):\n%s", text)
	}
	// Level 3 means "very high". No trusted location may be configured, or the
	// high level would be bypassed for files under it.
	if strings.Contains(text, "SecureURL") || strings.Contains(text, "TrustedLocation") {
		t.Fatalf("the profile must not declare trusted locations:\n%s", text)
	}
	if !strings.Contains(text, "/org.openoffice.Office.Common/Security/Scripting") {
		t.Fatalf("config targets the wrong registry path:\n%s", text)
	}
}

// TestPrepareLibreOfficeProfileIsIdempotent keeps a retried conversion from
// failing on an existing profile directory.
func TestPrepareLibreOfficeProfileIsIdempotent(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "lo-profile")
	for i := 0; i < 3; i++ {
		if err := prepareLibreOfficeProfile(profile); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if _, err := os.Stat(filepath.Join(profile, "user", "registrymodifications.xcu")); err != nil {
		t.Fatalf("config missing after repeats: %v", err)
	}
}
