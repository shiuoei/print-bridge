package domain

import (
	"encoding/json"
	"testing"
)

func TestLanguageConfigDefaultsAndValidation(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.App.Language != "en" {
		t.Fatalf("default language = %q", cfg.App.Language)
	}
	// Existing config.json files do not have app.language. The store unmarshals
	// them over DefaultConfig, so the original language remains valid.
	if err := json.Unmarshal([]byte(`{"app":{"autostart":true}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.App.Language != "en" || cfg.Validate() != nil {
		t.Fatalf("old config did not retain a valid default language: %+v", cfg.App)
	}
	cfg.App.Language = "zh-CN"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Chinese language rejected: %v", err)
	}
	cfg.App.Language = "fr"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unsupported language accepted")
	}
}
