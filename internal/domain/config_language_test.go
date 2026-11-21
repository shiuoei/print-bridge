package domain

import (
	"encoding/json"
	"testing"
)

func TestLanguageConfigDefaultsAndValidation(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.App.Language != "zh-CN" {
		t.Fatalf("default language = %q", cfg.App.Language)
	}
	// A config.json written before app.language existed has no such key. The
	// store unmarshals it over DefaultConfig, so it inherits the default, which
	// is Chinese.
	if err := json.Unmarshal([]byte(`{"app":{"autostart":true}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.App.Language != "zh-CN" || cfg.Validate() != nil {
		t.Fatalf("old config did not retain a valid default language: %+v", cfg.App)
	}
	// A config that stored a choice keeps it, whichever language it is.
	if err := json.Unmarshal([]byte(`{"app":{"language":"en"}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.App.Language != "en" || cfg.Validate() != nil {
		t.Fatalf("stored language was not kept: %+v", cfg.App)
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
