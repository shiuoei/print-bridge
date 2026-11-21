package logging

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestTailFiltersAndRedacts(t *testing.T) {
	b := NewBuffer(2)
	h := NewHandler(slog.NewTextHandler(io.Discard, nil), b)
	log := slog.New(h)
	log.Info("old")
	cutoff := time.Now()
	log.Warn("Authorization: Bearer secret123", "token", "abc123", "error", "password=hunter2")
	log.Error("last")
	got := b.Tail(cutoff, slog.LevelWarn, 10)
	if len(got) != 2 || got[0].Level != "WARN" || got[1].Level != "ERROR" {
		t.Fatalf("entries=%+v", got)
	}
	if strings.Contains(got[0].Message, "secret123") || strings.Contains(got[0].Message, "abc123") || strings.Contains(got[0].Message, "hunter2") {
		t.Fatal(got[0].Message)
	}
	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelDebug, "debug", 0)); err != nil {
		t.Fatal(err)
	}
	if len(b.Tail(time.Time{}, slog.LevelError, 10)) != 1 {
		t.Fatal("level filter")
	}
}

func TestForwardedConsoleRedactsGroupedCredentials(t *testing.T) {
	var console bytes.Buffer
	logger := slog.New(NewHandler(slog.NewTextHandler(&console, nil), NewBuffer(5)))
	logger.Warn("Authorization: Bearer secret123", slog.Group("credentials", slog.String("token", "nested-secret")), "password", "hunter2")
	output := console.String()
	for _, secret := range []string{"secret123", "nested-secret", "hunter2"} {
		if strings.Contains(output, secret) {
			t.Fatalf("secret %q in console output: %s", secret, output)
		}
	}
}
