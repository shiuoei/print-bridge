package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	Service struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"service"`
	Security struct {
		AllowedOrigins []string `json:"allowed_origins"`
		AllowedIPs     []string `json:"allowed_ips"`
	} `json:"security"`
	Printing struct {
		DefaultPrinter string `json:"default_printer"`
		DefaultPaper   *Paper `json:"default_paper"`
		DefaultCopies  int    `json:"default_copies"`
	} `json:"printing"`
	Limits struct {
		MaxFileSizeMB          int `json:"max_file_size_mb"`
		MaxBatchJobs           int `json:"max_batch_jobs"`
		MaxCopies              int `json:"max_copies"`
		DownloadTimeoutSeconds int `json:"download_timeout_seconds"`
	} `json:"limits"`
	App struct {
		Autostart bool   `json:"autostart"`
		Language  string `json:"language"`
		// Theme is the desktop appearance: "system", "light" or "dark". An empty
		// value means "system", so a config written before the field existed keeps
		// following the OS preference.
		Theme          string `json:"theme,omitempty"`
		SetupCompleted bool   `json:"setup_completed"`
	} `json:"app"`
	Remote struct {
		Enabled              bool   `json:"enabled"`
		EndpointURL          string `json:"endpoint_url"`
		BearerToken          string `json:"bearer_token"`
		DeviceID             string `json:"device_id"`
		DeviceName           string `json:"device_name"`
		PollIntervalSeconds  int    `json:"poll_interval_seconds"`
		MaxReportRetries     int    `json:"max_report_retries"`
		HistoryRetentionDays int    `json:"history_retention_days"`
	} `json:"remote"`
}

func DefaultConfig() Config {
	var c Config
	c.Service.Host = "127.0.0.1"
	c.Service.Port = 17890
	c.Security.AllowedOrigins = []string{}
	c.Security.AllowedIPs = []string{RequiredLoopbackIP}
	c.Printing.DefaultCopies = 1
	c.Limits.MaxFileSizeMB = 20
	c.Limits.MaxBatchJobs = 20
	c.Limits.MaxCopies = 100
	c.Limits.DownloadTimeoutSeconds = 30
	// The desktop app ships for a Chinese-speaking operator, so Chinese is the
	// default; en is the other supported choice. A config file that never
	// carried app.language (written before the field existed) inherits this,
	// while an explicit choice keeps the language the operator picked.
	c.App.Language = "zh-CN"
	c.Remote.PollIntervalSeconds = 10
	c.Remote.MaxReportRetries = 10
	c.Remote.HistoryRetentionDays = 3
	return c
}

const (
	// RequiredLoopbackIP is always present in the client allowlist. Loopback
	// clients are additionally exempt from the allowlist at connection time, so
	// removing this entry cannot lock the local UI out — but keeping it in the
	// config makes that guarantee visible to the operator.
	RequiredLoopbackIP = "127.0.0.1"

	// MinServicePort is the documented floor. Below this range the port is
	// reserved or in the ephemeral range, where a listener would collide with
	// outgoing connections instead of staying predictable.
	MinServicePort = 10000
	MaxServicePort = 65535

	// MinRemotePollIntervalSeconds and MinRemoteMaxReportRetries prevent a
	// configuration that hammers the remote endpoint.
	MinRemotePollIntervalSeconds = 3
	MinRemoteMaxReportRetries    = 1
	MinHistoryRetentionDays      = 1
)

// Normalize returns the config with the invariants that must hold regardless of
// what a config file, CLI edit or import file contained:
//
//   - service.host is pinned to loopback (the compatibility field never selects
//     the bind address, and leaving it free invites a false sense of exposure),
//   - the loopback client address is always allowed, trimmed and de-duplicated,
//     so `ip delete 127.0.0.1` cannot lock the local UI out,
//   - any-address allowlist entries are dropped: `0.0.0.0/0` and `::/0` turn the
//     agent into a LAN-wide print service, which is never the intent of a
//     per-client allowlist.
//
// Callers apply Normalize on load and on every save, matching the documented
// `normalized()` contract.
func (c Config) Normalize() Config {
	c.Service.Host = "127.0.0.1"
	c.Security.AllowedIPs = normalizeAllowedIPs(c.Security.AllowedIPs)
	return c
}

// normalizeAllowedIPs re-injects the loopback address, trims, de-duplicates and
// drops any-address entries.
func normalizeAllowedIPs(entries []string) []string {
	out := make([]string, 0, len(entries)+1)
	seen := make(map[string]bool, len(entries)+1)
	add := func(entry string) {
		if seen[entry] {
			return
		}
		seen[entry] = true
		out = append(out, entry)
	}
	// The required loopback entry comes first so the effective floor is always
	// visible at the top of the list.
	add(RequiredLoopbackIP)
	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" || isAnyAddressEntry(trimmed) {
			continue
		}
		add(trimmed)
	}
	return out
}

// isAnyAddressEntry reports whether an allowlist entry selects the whole
// internet, either as a literal wildcard address or as a zero-length prefix.
func isAnyAddressEntry(entry string) bool {
	if ip := net.ParseIP(entry); ip != nil {
		return ip.IsUnspecified()
	}
	_, network, err := net.ParseCIDR(entry)
	if err != nil {
		return false
	}
	ones, bits := network.Mask.Size()
	return ones == 0 && bits > 0
}

func (c Config) Validate() error {
	if c.App.Language != "en" && c.App.Language != "zh-CN" {
		return errors.New("app language must be en or zh-CN")
	}
	switch c.App.Theme {
	case "", "system", "light", "dark":
	default:
		return errors.New("app theme must be system, light or dark")
	}
	if c.Service.Port < 1 || c.Service.Port > MaxServicePort {
		return fmt.Errorf("port must be 1..%d", MaxServicePort)
	}
	if c.Printing.DefaultCopies < 1 || c.Limits.MaxCopies < 1 || c.Printing.DefaultCopies > c.Limits.MaxCopies {
		return errors.New("invalid copy limit")
	}
	if c.Limits.MaxFileSizeMB < 1 || c.Limits.MaxBatchJobs < 1 || c.Limits.DownloadTimeoutSeconds < 1 {
		return errors.New("invalid limits")
	}
	for _, origin := range c.Security.AllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("invalid origin: " + origin)
		}
	}
	for _, entry := range c.Security.AllowedIPs {
		if net.ParseIP(entry) == nil {
			if _, _, err := net.ParseCIDR(entry); err != nil {
				return errors.New("invalid IP: " + entry)
			}
		}
		// Any-address entries would make the per-client allowlist meaningless.
		// Normalize drops them, so seeing one here means a caller bypassed it.
		if isAnyAddressEntry(entry) {
			return fmt.Errorf("any-address entry %q is not allowed in the client allowlist", entry)
		}
	}
	if c.Remote.Enabled {
		u, err := url.Parse(c.Remote.EndpointURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return errors.New("remote URL must be HTTPS (or HTTP for local development)")
		}
	}
	if c.Remote.PollIntervalSeconds < 1 {
		return errors.New("remote poll interval must be at least 1 second")
	}
	if c.Remote.MaxReportRetries < 0 {
		return errors.New("remote max report retries must not be negative")
	}
	// A zero retention window is a deliberate "keep everything", so only a
	// negative value is rejected here. The minimums below apply to values that
	// are actively used, not to the disabled form.
	if c.Remote.HistoryRetentionDays < 0 {
		return errors.New("history retention must not be negative")
	}
	if c.Printing.DefaultPaper != nil && !c.Printing.DefaultPaper.Valid() {
		return errors.New("invalid default paper")
	}
	return nil
}

func (c Config) OriginAllowed(origin string) bool {
	for _, allowed := range c.Security.AllowedOrigins {
		if strings.EqualFold(allowed, origin) {
			return true
		}
	}
	return false
}

func (c Config) IPAllowed(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// Loopback clients are always allowed. The desktop UI and the CLI reach the
	// agent over the loopback interface, so a mis-edited allowlist must not be
	// able to lock the operator out of their own machine. This matches the
	// protocol gate, which exempts loopback before consulting the allowlist.
	if ip.IsLoopback() {
		return true
	}
	for _, entry := range c.Security.AllowedIPs {
		if p := net.ParseIP(entry); p != nil && p.Equal(ip) {
			return true
		}
		if _, n, err := net.ParseCIDR(entry); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// LoadConfigFile reads and validates one config file without touching the live
// data directory. It is what `config validate <path>` and the desktop transfer
// preview use, so a broken file can be diagnosed without being able to open the
// store (a corrupt config.json prevents store.Open entirely).
//
// Fields absent from the file take their defaults, matching the merge semantics
// the transfer importer uses: a partial hand-written file is validated against
// what the agent would actually run, not against an all-zero struct.
func LoadConfigFile(path string) (Config, error) {
	c := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse: %w", err)
	}
	c = c.Normalize()
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}
