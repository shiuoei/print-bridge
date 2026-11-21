package main

import (
	"context"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/agent"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/printer"
	"github.com/shiuoei/print-bridge/internal/store"
)

// TestMeasureRealTestPrint measures the real spooler path end to end. The reported
// symptom was the button staying on "printing…", which means the command did not
// return promptly; this measures how long it actually takes.
func TestMeasureRealTestPrint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real spooler test in short mode")
	}
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := s.Config()
	cfg.Printing.DefaultPrinter = "HPRT N41"
	cfg.Printing.DefaultPaper = &domain.Paper{WidthMM: 100, HeightMM: 150}
	cfg.Service.Port = 17899
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	a := agent.New(s, printer.New())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Skipf("agent cannot start (port busy): %v", err)
	}
	defer a.Shutdown(context.Background())

	svc := command.Service{Store: s, Printer: printer.New(), Agent: a}
	start := time.Now()
	r := svc.Execute(ctx, command.Request{Command: "test.print", Name: "HPRT N41"})
	elapsed := time.Since(start)
	t.Logf("test.print took %v err=%q", elapsed, r.Error)
	if elapsed > 25*time.Second {
		t.Fatalf("test.print took %v, longer than the UI timeout; the button would stay stuck", elapsed)
	}
}
