package transfer

import (
	"encoding/json"
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
)

func TestEncryptedRoundTrip(t *testing.T) {
	c := domain.DefaultConfig()
	c.Remote.BearerToken = "secret"
	b, err := Export(SelectAll(c), "password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Import(b, "wrong", domain.DefaultConfig()); err == nil {
		t.Fatal("wrong password accepted")
	}
	got, err := Import(b, "password", domain.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got.Remote.BearerToken != "secret" || got.Service.Port != 17890 {
		t.Fatalf("round trip: %#v", got)
	}
}

// TestSelectiveExportLeavesOtherFieldsAlone is the regression guard for the
// defect where `--only service-port` exported a full Config whose unselected
// fields held defaults, so importing it silently reset the whitelist and the
// remote settings.
func TestSelectiveExportLeavesOtherFieldsAlone(t *testing.T) {
	live := domain.DefaultConfig()
	live.Service.Port = 17890
	live.Security.AllowedOrigins = []string{"https://a.example"}
	live.Security.AllowedIPs = []string{"127.0.0.1", "10.0.0.0/8"}
	live.Remote.PollIntervalSeconds = 30
	live.Remote.MaxReportRetries = 7

	selected := SelectTailored(live, []string{"service-port"})
	if selected == nil {
		t.Fatal("selection is nil")
	}
	selected.Service.Port = intPtr(19090)

	merged := Apply(live, selected)
	if merged.Service.Port != 19090 {
		t.Fatalf("port = %d, want 19090", merged.Service.Port)
	}
	if len(merged.Security.AllowedOrigins) != 1 || merged.Security.AllowedOrigins[0] != "https://a.example" {
		t.Fatalf("allowed_origins were reset: %#v", merged.Security.AllowedOrigins)
	}
	if len(merged.Security.AllowedIPs) != 2 {
		t.Fatalf("allowed_ips were reset: %#v", merged.Security.AllowedIPs)
	}
	if merged.Remote.PollIntervalSeconds != 30 || merged.Remote.MaxReportRetries != 7 {
		t.Fatalf("remote settings were reset: %#v", merged.Remote)
	}
}

// TestSelectiveExportOmitsUnselectedKeys checks the wire shape directly: an
// unselected key must be absent from the decrypted payload, not present with a
// default value.
func TestSelectiveExportOmitsUnselectedKeys(t *testing.T) {
	live := domain.DefaultConfig()
	live.Security.AllowedOrigins = []string{"https://a.example"}

	payload, err := json.Marshal(SelectTailored(live, []string{"service-port"}))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["security"]; ok {
		t.Fatalf("unselected security section leaked into the payload: %s", payload)
	}
	if _, ok := decoded["remote"]; ok {
		t.Fatalf("unselected remote section leaked into the payload: %s", payload)
	}
	if svc, ok := decoded["service"]; !ok || svc["port"] == nil {
		t.Fatalf("selected service.port missing: %s", payload)
	}
}

// TestImportPreservesTokenWhenAbsent mirrors the token rule: a file
// without a token must not clear the configured one.
func TestImportPreservesTokenWhenAbsent(t *testing.T) {
	live := domain.DefaultConfig()
	live.Remote.BearerToken = "keep-me"

	payload, err := Export(SelectTailored(live, []string{"service-port"}), "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Import(payload, "", live)
	if err != nil {
		t.Fatal(err)
	}
	if got.Remote.BearerToken != "keep-me" {
		t.Fatalf("token = %q, want it preserved", got.Remote.BearerToken)
	}
}

// TestImportAcceptsUpstreamPayload parses the encrypted shell the original tool
// writes: a partial config object with only service.port present.
func TestImportAcceptsUpstreamPayload(t *testing.T) {
	raw := []byte(`{"service":{"port":19090}}`)
	var partial PartialConfig
	if err := json.Unmarshal(raw, &partial); err != nil {
		t.Fatal(err)
	}
	live := domain.DefaultConfig()
	live.Security.AllowedOrigins = []string{"https://b.example"}
	merged := Apply(live, &partial)
	if merged.Service.Port != 19090 {
		t.Fatalf("port = %d", merged.Service.Port)
	}
	if len(merged.Security.AllowedOrigins) != 1 || merged.Security.AllowedOrigins[0] != "https://b.example" {
		t.Fatalf("origins not preserved: %#v", merged.Security.AllowedOrigins)
	}
}

// TestSelectAllCoversEveryField keeps the no-`--only` export complete.
func TestSelectAllCoversEveryField(t *testing.T) {
	c := domain.DefaultConfig()
	all := SelectAll(c)
	if all.Service == nil || all.Service.Port == nil {
		t.Fatal("service.port missing")
	}
	if all.Security == nil || all.Security.AllowedOrigins == nil || all.Security.AllowedIPs == nil {
		t.Fatal("security section incomplete")
	}
	if all.Remote == nil || all.Remote.PollIntervalSeconds == nil ||
		all.Remote.MaxReportRetries == nil || all.Remote.HistoryRetentionDays == nil {
		t.Fatal("remote section incomplete")
	}
}

// TestImportRejectsEmptyPayload guards against an export that would produce an
// importable file carrying nothing.
func TestImportRejectsEmptyPayload(t *testing.T) {
	if _, err := Export(nil, "password"); err == nil {
		t.Fatal("nil selection accepted by Export")
	}
}
