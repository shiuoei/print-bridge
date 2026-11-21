package transfer

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/shiuoei/print-bridge/internal/domain"
	"golang.org/x/crypto/argon2"
)

type envelope struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	Crypto  struct {
		KDF         string `json:"kdf"`
		MemoryKiB   int    `json:"memory_kib"`
		Iterations  int    `json:"iterations"`
		Parallelism int    `json:"parallelism"`
		Cipher      string `json:"cipher"`
		TagBytes    int    `json:"tag_bytes"`
		Salt        string `json:"salt"`
		Nonce       string `json:"nonce"`
	} `json:"crypto"`
	Payload string `json:"payload"`
}

// PartialConfig is the transferable subset of the agent configuration.
//
// Every field is a pointer so an absent key and a present-but-empty value stay
// distinguishable on the wire. The format encodes the same subset with
// serde `skip_serializing_if = "Option::is_none"`, and its importer only
// overwrites the keys the file actually contains. Using the full Config here
// would make a selective export indistinguishable from a full one on re-import
// and silently reset every unselected setting.
type PartialConfig struct {
	Service  *PartialService  `json:"service,omitempty"`
	Security *PartialSecurity `json:"security,omitempty"`
	Remote   *PartialRemote   `json:"remote,omitempty"`
}

type PartialService struct {
	Port *int `json:"port,omitempty"`
}

type PartialSecurity struct {
	AllowedOrigins *[]string `json:"allowed_origins,omitempty"`
	AllowedIPs     *[]string `json:"allowed_ips,omitempty"`
}

type PartialRemote struct {
	Enabled              *bool   `json:"enabled,omitempty"`
	EndpointURL          *string `json:"endpoint_url,omitempty"`
	BearerToken          *string `json:"bearer_token,omitempty"`
	PollIntervalSeconds  *int    `json:"poll_interval_seconds,omitempty"`
	MaxReportRetries     *int    `json:"max_report_retries,omitempty"`
	HistoryRetentionDays *int    `json:"history_retention_days,omitempty"`
}

// TransferFields is the closed set of selectable keys, mirroring the documented
// `--only` vocabulary. An empty selection means "every transferable field".
var TransferFields = []string{
	"service-port",
	"allowed-origins",
	"allowed-ips",
	"remote-enabled",
	"remote-url",
	"remote-token",
	"remote-interval",
	"remote-max-retries",
}

// FieldKeys maps a CLI field name to the JSON key it controls.
var FieldKeys = map[string]string{
	"service-port":       "service.port",
	"allowed-origins":    "security.allowed_origins",
	"allowed-ips":        "security.allowed_ips",
	"remote-enabled":     "remote.enabled",
	"remote-url":         "remote.endpoint_url",
	"remote-token":       "remote.bearer_token",
	"remote-interval":    "remote.poll_interval_seconds",
	"remote-max-retries": "remote.max_report_retries",
}

func boolPtr(v bool) *bool         { return &v }
func intPtr(v int) *int            { return &v }
func strPtr(v string) *string      { return &v }
func strsPtr(v []string) *[]string { out := append([]string{}, v...); return &out }

// SelectTailored builds the payload for an explicit field selection. Selecting
// nothing returns nil; callers should use SelectAll for a full export.
func SelectTailored(c domain.Config, only []string) *PartialConfig {
	if len(only) == 0 {
		return nil
	}
	keep := map[string]bool{}
	for _, name := range only {
		keep[FieldKeys[name]] = true
	}
	var p PartialConfig
	if keep["service.port"] {
		p.Service = &PartialService{Port: intPtr(c.Service.Port)}
	}
	var sec PartialSecurity
	if keep["security.allowed_origins"] {
		sec.AllowedOrigins = strsPtr(c.Security.AllowedOrigins)
	}
	if keep["security.allowed_ips"] {
		sec.AllowedIPs = strsPtr(c.Security.AllowedIPs)
	}
	if sec.AllowedOrigins != nil || sec.AllowedIPs != nil {
		p.Security = &sec
	}
	var rem PartialRemote
	if keep["remote.enabled"] {
		rem.Enabled = boolPtr(c.Remote.Enabled)
	}
	if keep["remote.endpoint_url"] {
		rem.EndpointURL = strPtr(c.Remote.EndpointURL)
	}
	if keep["remote.bearer_token"] {
		rem.BearerToken = strPtr(c.Remote.BearerToken)
	}
	if keep["remote.poll_interval_seconds"] {
		rem.PollIntervalSeconds = intPtr(c.Remote.PollIntervalSeconds)
	}
	if keep["remote.max_report_retries"] {
		rem.MaxReportRetries = intPtr(c.Remote.MaxReportRetries)
	}
	if rem.EndpointURL != nil || rem.Enabled != nil || rem.BearerToken != nil ||
		rem.PollIntervalSeconds != nil || rem.MaxReportRetries != nil || rem.HistoryRetentionDays != nil {
		p.Remote = &rem
	}
	return &p
}

// SelectAll captures every transferable field, which is what a no-`--only`
// export must produce. It is not SelectTailored(TransferFields): history
// retention is not individually selectable but still belongs in a full export.
func SelectAll(c domain.Config) *PartialConfig {
	return &PartialConfig{
		Service: &PartialService{Port: intPtr(c.Service.Port)},
		Security: &PartialSecurity{
			AllowedOrigins: strsPtr(c.Security.AllowedOrigins),
			AllowedIPs:     strsPtr(c.Security.AllowedIPs),
		},
		Remote: &PartialRemote{
			Enabled:              boolPtr(c.Remote.Enabled),
			EndpointURL:          strPtr(c.Remote.EndpointURL),
			BearerToken:          strPtr(c.Remote.BearerToken),
			PollIntervalSeconds:  intPtr(c.Remote.PollIntervalSeconds),
			MaxReportRetries:     intPtr(c.Remote.MaxReportRetries),
			HistoryRetentionDays: intPtr(c.Remote.HistoryRetentionDays),
		},
	}
}

func Export(selection *PartialConfig, password string) ([]byte, error) {
	if selection == nil {
		return nil, errors.New("export selection is required")
	}
	inner, err := json.Marshal(map[string]any{"format": "printbridge-config", "version": 1, "config": selection})
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	key := argon2.IDKey([]byte(password), salt, 2, 19456, 1, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	var e envelope
	e.Format = "printbridge-config-encrypted"
	e.Version = 1
	e.Crypto.KDF = "argon2id13"
	e.Crypto.MemoryKiB = 19456
	e.Crypto.Iterations = 2
	e.Crypto.Parallelism = 1
	e.Crypto.Cipher = "aes-256-gcm"
	e.Crypto.TagBytes = 16
	e.Crypto.Salt = base64.StdEncoding.EncodeToString(salt)
	e.Crypto.Nonce = base64.StdEncoding.EncodeToString(nonce)
	e.Payload = base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, inner, nil))
	return json.MarshalIndent(e, "", "  ")
}

func Import(data []byte, password string, current domain.Config) (domain.Config, error) {
	var e envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return current, err
	}
	if e.Format != "printbridge-config-encrypted" || e.Version != 1 || e.Crypto.KDF != "argon2id13" || e.Crypto.Cipher != "aes-256-gcm" || e.Crypto.MemoryKiB != 19456 || e.Crypto.Iterations != 2 || e.Crypto.Parallelism != 1 || e.Crypto.TagBytes != 16 {
		return current, errors.New("unsupported encrypted config")
	}
	salt, err := base64.StdEncoding.DecodeString(e.Crypto.Salt)
	if err != nil || len(salt) != 16 {
		return current, errors.New("invalid salt")
	}
	nonce, err := base64.StdEncoding.DecodeString(e.Crypto.Nonce)
	if err != nil || len(nonce) != 12 {
		return current, errors.New("invalid nonce")
	}
	payload, err := base64.StdEncoding.DecodeString(e.Payload)
	if err != nil {
		return current, err
	}
	key := argon2.IDKey([]byte(password), salt, 2, 19456, 1, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return current, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return current, err
	}
	plain, err := aead.Open(nil, nonce, payload, nil)
	if err != nil {
		return current, errors.New("incorrect password or damaged config")
	}
	var inner struct {
		Format  string          `json:"format"`
		Version int             `json:"version"`
		Config  json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(plain, &inner); err != nil {
		return current, err
	}
	if inner.Format != "printbridge-config" || inner.Version != 1 {
		return current, errors.New("unsupported config format")
	}
	var incoming PartialConfig
	if len(inner.Config) > 0 {
		if err := json.Unmarshal(inner.Config, &incoming); err != nil {
			return current, err
		}
	}
	merged := Apply(current, &incoming)
	if err := merged.Validate(); err != nil {
		return current, err
	}
	return merged, nil
}

// Apply merges a partial payload onto base. Only keys present in the payload are
// overwritten, so importing a selective export leaves every other setting alone —
// this is the merge rule and the reason the payload must stay partial.
//
// The bearer token follows one extra rule: a missing, null or empty incoming
// token keeps the current one, so re-importing an export that omitted the token
// (or a redacted file) cannot silently disable remote polling auth.
func Apply(base domain.Config, incoming *PartialConfig) domain.Config {
	out := base
	if incoming == nil {
		return out
	}
	if svc := incoming.Service; svc != nil && svc.Port != nil {
		out.Service.Port = *svc.Port
	}
	if sec := incoming.Security; sec != nil {
		if sec.AllowedOrigins != nil {
			out.Security.AllowedOrigins = append([]string{}, (*sec.AllowedOrigins)...)
		}
		if sec.AllowedIPs != nil {
			out.Security.AllowedIPs = append([]string{}, (*sec.AllowedIPs)...)
		}
	}
	if rem := incoming.Remote; rem != nil {
		if rem.Enabled != nil {
			out.Remote.Enabled = *rem.Enabled
		}
		if rem.EndpointURL != nil {
			out.Remote.EndpointURL = *rem.EndpointURL
		}
		if rem.BearerToken != nil && *rem.BearerToken != "" {
			out.Remote.BearerToken = *rem.BearerToken
		}
		if rem.PollIntervalSeconds != nil {
			out.Remote.PollIntervalSeconds = *rem.PollIntervalSeconds
		}
		if rem.MaxReportRetries != nil {
			out.Remote.MaxReportRetries = *rem.MaxReportRetries
		}
		if rem.HistoryRetentionDays != nil {
			out.Remote.HistoryRetentionDays = *rem.HistoryRetentionDays
		}
	}
	return out
}
