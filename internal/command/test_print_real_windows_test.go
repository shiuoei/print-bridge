//go:build windows

package command

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/printer"
)

// Explicit opt-in: this test submits exactly one A4 page to a real printer.
func TestRealTestPrintSubmission(t *testing.T) {
	name := os.Getenv("PRINTBRIDGE_REAL_TEST_PRINT")
	if name == "" {
		t.Skip("set PRINTBRIDGE_REAL_TEST_PRINT to a real A4 printer queue")
	}
	s := testStore(t)
	c := s.Config()
	c.Printing.DefaultPaper = &domain.Paper{WidthMM: 210, HeightMM: 297}
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	r := Service{Store: s, Printer: printer.New(), Agent: testAgent{}}.Execute(ctx, Request{Command: "test.print", Name: name})
	if r.Error != "" || r.Status != "submitted" {
		t.Fatalf("test print: %s (%s)", r.Error, r.ErrorCode)
	}
	t.Log(r.Message)
}
