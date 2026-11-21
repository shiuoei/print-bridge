package logging

import (
	"log"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// The reported bug, in its production form: the desktop was started with
//
//	logBuffer := logging.NewBuffer(500)
//	slog.SetDefault(slog.New(logging.NewHandler(slog.Default().Handler(), logBuffer)))
//
// slog.SetDefault points the standard `log` package at the installed logger, and
// the handler it was given was slog's own defaultHandler, which writes through
// `log`. The next record therefore reached log's output mutex while the same
// goroutine already held it, and the goroutine never came back. test.print hit
// this in its success log, after the page had already been printed, so the
// button stayed on "printing…" until the desktop's overdue net fired.
//
// The regression to keep: logging must never be able to wedge a goroutine.
func TestWrappedDefaultHandlerCannotDeadlock(t *testing.T) {
	// Restore whatever a previous test installed before measuring our own.
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })

	// The old call, verbatim. It must be harmless: NewHandler refuses slog's
	// defaultHandler and uses a plain console handler instead.
	buffer := NewBuffer(50)
	slog.SetDefault(slog.New(NewHandler(slog.Default().Handler(), buffer)))

	done := make(chan struct{})
	go func() {
		defer close(done)
		slog.Info("structured record", "printer", "HPRT N41")
		log.Print("standard package record")
		log.Printf("formatted %s", "record")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging deadlocked; this is the bug that froze test printing")
	}

	entries := buffer.Tail(time.Time{}, slog.LevelInfo, 50)
	if len(entries) == 0 {
		t.Fatal("no record reached the buffer the UI reads")
	}
	var sawStructured bool
	for _, e := range entries {
		if e.Message == "structured record printer=HPRT N41" {
			sawStructured = true
		}
	}
	if !sawStructured {
		t.Fatalf("the structured record did not reach the buffer: %+v", entries)
	}
}

// TestConcurrentLoggingDuringInstall covers the real shape of the failure: the
// print worker logs from its own goroutine while the desktop logs from the UI.
func TestConcurrentLoggingDuringInstall(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	slog.SetDefault(slog.New(NewHandler(slog.Default().Handler(), NewBuffer(10))))

	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				slog.Info("worker record", "worker", n)
				log.Printf("worker %d line %d", n, j)
			}
		}(i)
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent logging deadlocked")
	}
}

// TestInstallKeepsRecordsInBuffer proves the supported entry point fills the
// buffer the log panel reads.
func TestInstallKeepsRecordsInBuffer(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })

	buffer := NewBuffer(5)
	Install(buffer)
	slog.Warn("installed handler", "job_id", "j-1")
	if got := buffer.Tail(time.Time{}, slog.LevelWarn, 10); len(got) != 1 || got[0].Message != "installed handler job_id=j-1" {
		t.Fatalf("Tail() = %+v", got)
	}
}
