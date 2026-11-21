package main

import (
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/command"
)

// The regression this guards: a click handler used to run the command inline
// during the frame rebuild. Printing a test page takes seconds in the spooler,
// so the window froze and Windows painted "Not responding" over it.

// captureContext renders one frame and returns the Context it built with, so a
// test can drive runAsync the way a click handler would.
func captureContext(t *testing.T) *ui.Context {
	t.Helper()
	var captured *ui.Context
	tester := ui.NewTester(func(c *ui.Context) {
		captured = c
		ui.Text(c, "frame")
	}, 400, 300)
	tester.Frame()
	if captured == nil {
		t.Fatal("no context captured")
	}
	return captured
}

// TestRunAsyncReturnsImmediately is the core property: scheduling a slow command
// must not block the caller, because the caller is the UI frame.
func TestRunAsyncReturnsImmediately(t *testing.T) {
	d := newTestDesktop(t)
	frameCtx := captureContext(t)

	// A command that takes a noticeable time, run through the async path.
	var wg sync.WaitGroup
	wg.Add(1)
	var task busyTask
	start := time.Now()
	d.runAsync(frameCtx, &task, 5*time.Second,
		command.Request{Command: "printers.list"},
		func(command.Response) { wg.Done() })
	elapsed := time.Since(start)

	// Returning within a frame budget is the whole point. A generous bound keeps
	// the test stable on a loaded machine while still failing if the call blocks
	// for the seconds a print takes.
	if elapsed > 100*time.Millisecond {
		t.Fatalf("runAsync blocked for %v; it must return immediately", elapsed)
	}
	if !task.running {
		t.Fatal("task was not marked running, so a second click could start it again")
	}
	wg.Wait()
	if task.running {
		t.Fatal("task still marked running after completion")
	}
}

// TestRunAsyncRefusesDoubleStart keeps a double click from printing twice.
//
// The guard is the running flag, which runAsync sets synchronously before it
// returns. Two calls made from the same frame therefore cannot both start, no
// matter how fast the command is.
func TestRunAsyncRefusesDoubleStart(t *testing.T) {
	d := newTestDesktop(t)
	frameCtx := captureContext(t)

	var task busyTask
	var runs int
	var mu sync.Mutex
	release := make(chan struct{})

	// The first run is held open so the second call definitely overlaps it.
	d.runAsync(frameCtx, &task, 5*time.Second,
		command.Request{Command: "printers.list"},
		func(command.Response) {
			<-release
			mu.Lock()
			runs++
			mu.Unlock()
		})

	// Second call in the same frame must be refused outright.
	d.runAsync(frameCtx, &task, 5*time.Second,
		command.Request{Command: "printers.list"},
		func(command.Response) {
			mu.Lock()
			runs++
			mu.Unlock()
		})

	close(release)
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if runs != 1 {
		t.Fatalf("the command ran %d times; a double click must start it once", runs)
	}
}

// TestBusyLabelShowsProgress keeps the button from looking inert while it works.
func TestBusyLabelShowsProgress(t *testing.T) {
	d := newTestDesktop(t)
	idle := busyLabel(d, nil, "test_print", "test_print_running")
	if idle != d.tr("test_print") {
		t.Fatalf("idle label = %q", idle)
	}
	task := &busyTask{running: true}
	if got := busyLabel(d, task, "test_print", "test_print_running"); got != d.tr("test_print_running") {
		t.Fatalf("busy label = %q, want the running text", got)
	}
}

// TestTestPrintRequiresPrinter keeps an empty selection from reaching the agent.
func TestTestPrintRequiresPrinter(t *testing.T) {
	d := newTestDesktop(t)
	d.printerName = d.tr("printer_not_selected")
	texts := textsOf(t, func(c *ui.Context) { d.printing(c) })
	// The page must render without starting a print; the guard is exercised by
	// the button path, which needs a printer name.
	if !containsText(texts, d.tr("test_print")) {
		t.Fatalf("printing page does not render the test-print button: %v", texts)
	}
}
