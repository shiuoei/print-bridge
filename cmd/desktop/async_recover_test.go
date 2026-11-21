package main

import (
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/command"
)

// The reported symptom: the button stayed on "printing…" forever after one
// click. That means task.running never went back to false, so either the
// command never returned or the reset was lost.

// TestRunAsyncResetsRunningOnTimeout proves the running flag is cleared even when
// the command does not finish in time.
func TestRunAsyncResetsRunningOnTimeout(t *testing.T) {
	d := newTestDesktop(t)
	frameCtx := captureContext(t)

	var task busyTask
	// A 1ns timeout makes the context expire before the command can finish, which
	// is the same code path a hung spooler call takes.
	d.runAsync(frameCtx, &task, time.Nanosecond,
		command.Request{Command: "printers.list"},
		func(command.Response) {})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !task.running {
			return // reset as expected
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task.running stayed true after the command timed out; the button would show 'printing…' forever")
}

// TestRunAsyncResetsRunningOnUnknownCommand covers a command that fails fast: the
// flag must still clear.
func TestRunAsyncResetsRunningOnUnknownCommand(t *testing.T) {
	d := newTestDesktop(t)
	frameCtx := captureContext(t)

	var task busyTask
	d.runAsync(frameCtx, &task, 5*time.Second,
		command.Request{Command: "no.such.command"},
		func(command.Response) {})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !task.running {
			if task.err == "" {
				t.Fatal("a failing command did not record an error")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task.running stayed true after a failing command")
}

// TestBusyLabelRecoversAfterCompletion is the UI-level property: once the task
// finishes, the button must show the idle label again.
func TestBusyLabelRecoversAfterCompletion(t *testing.T) {
	d := newTestDesktop(t)
	frameCtx := captureContext(t)

	var task busyTask
	if got := busyLabel(d, &task, "test_print", "test_print_running"); got != d.tr("test_print") {
		t.Fatalf("before start: %q", got)
	}
	d.runAsync(frameCtx, &task, 5*time.Second,
		command.Request{Command: "printers.list"},
		func(command.Response) {})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !task.running {
			if got := busyLabel(d, &task, "test_print", "test_print_running"); got != d.tr("test_print") {
				t.Fatalf("after completion: %q, want the idle label back", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task never completed")
}
