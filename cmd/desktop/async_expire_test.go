package main

import (
	"testing"
	"time"
)

// The reported bug: after one click the button stayed on "printing…" forever.
// These tests pin the recovery guarantees so that can never happen again.

// TestBusyTaskExpiresWhenCompletionIsLost simulates a lost completion
// notification: the flag is set, nothing ever clears it, and the rebuild path
// must recover once the deadline has passed.
func TestBusyTaskExpiresWhenCompletionIsLost(t *testing.T) {
	task := &busyTask{
		running:   true,
		startedAt: time.Now().Add(-1 * time.Minute),
		timeout:   2 * time.Second,
	}
	task.expireIfOverdue(time.Now())
	if task.running {
		t.Fatal("an overdue task still claims to be running; the button would stay on 'printing…'")
	}
	if task.err == "" {
		t.Fatal("the expiry did not explain itself")
	}
}

// TestBusyTaskStaysRunningBeforeDeadline keeps a genuinely slow command from being
// cut short by the safety net.
func TestBusyTaskStaysRunningBeforeDeadline(t *testing.T) {
	task := &busyTask{
		running:   true,
		startedAt: time.Now(),
		timeout:   30 * time.Second,
	}
	task.expireIfOverdue(time.Now())
	if !task.running {
		t.Fatal("a task inside its deadline was cleared early")
	}
}

// TestBusyLabelRecoversFromLostCompletion is the UI-level guarantee: even when
// the completion is never delivered, rendering the button again shows the idle
// label once the deadline passes.
func TestBusyLabelRecoversFromLostCompletion(t *testing.T) {
	d := newTestDesktop(t)
	task := &busyTask{
		running:   true,
		startedAt: time.Now().Add(-5 * time.Minute),
		timeout:   30 * time.Second,
	}
	if got := busyLabel(d, task, "test_print", "test_print_running"); got != d.tr("test_print") {
		t.Fatalf("label = %q, want the idle label once the task is overdue", got)
	}
}

// TestBusyLabelHandlesNilTask keeps the helper safe for buttons that never run a
// background command.
func TestBusyLabelHandlesNilTask(t *testing.T) {
	d := newTestDesktop(t)
	if got := busyLabel(d, nil, "test_print", "test_print_running"); got != d.tr("test_print") {
		t.Fatalf("label = %q", got)
	}
}
