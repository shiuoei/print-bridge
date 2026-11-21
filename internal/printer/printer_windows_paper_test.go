//go:build windows

package printer

import (
	"errors"
	"testing"

	"github.com/shiuoei/print-bridge/internal/domain"
)

func TestDriverPaperNameRejectsMissingPrinter(t *testing.T) {
	_, err := (platformBackend{}).driverPaperName(t.Context(), "PrintBridge-Definitely-Missing-Queue", &domain.Paper{WidthMM: 210, HeightMM: 297})
	if !errors.Is(err, ErrPrinterNotFound) {
		t.Fatalf("expected ErrPrinterNotFound, got %v", err)
	}
}
