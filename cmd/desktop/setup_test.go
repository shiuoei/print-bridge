package main

import (
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
)

func TestFirstRunSetupAndExistingConfig(t *testing.T) {
	c := domain.DefaultConfig()
	if !needsSetup(c) {
		t.Fatal("fresh config should open setup")
	}
	c.App.SetupCompleted = true
	if needsSetup(c) {
		t.Fatal("skipped or completed setup should stay dismissed")
	}
	c.App.SetupCompleted = false
	c.Printing.DefaultPrinter = "Printer A"
	c.Printing.DefaultPaper = &domain.Paper{WidthMM: 210, HeightMM: 297}
	c.Security.AllowedOrigins = []string{"https://example.com"}
	if needsSetup(c) {
		t.Fatal("an existing complete config should open normally")
	}
}

func TestBuildSetupConfigRequiresUsableInputs(t *testing.T) {
	base := domain.DefaultConfig()
	available := []string{"Printer A"}
	tests := []struct {
		name, printer, width, height, origins, port, wantKey string
	}{
		{"missing printer", "", "210", "297", "https://example.com", "17890", "setup_select_printer_error"},
		{"unknown printer", "Other", "210", "297", "https://example.com", "17890", "setup_select_printer_error"},
		{"bad paper", "Printer A", "0", "297", "https://example.com", "17890", "invalid_paper"},
		{"missing origin", "Printer A", "210", "297", "", "17890", "setup_origin_error"},
		{"invalid origin", "Printer A", "210", "297", "file://local", "17890", "setup_invalid_config"},
		{"valid", "Printer A", "210", "297", "https://example.com", "17890", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, key, _ := buildSetupConfig(base, available, tc.printer, tc.width, tc.height, tc.origins, tc.port)
			if key != tc.wantKey {
				t.Fatalf("error key = %q, want %q", key, tc.wantKey)
			}
			if key == "" && (!got.App.SetupCompleted || got.Printing.DefaultPrinter != "Printer A" || !got.OriginAllowed("https://example.com")) {
				t.Fatalf("valid setup not applied: %+v", got)
			}
		})
	}
}
