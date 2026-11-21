package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/command"
)

// Asynchronous command execution for the desktop UI.
//
// The UI is rebuilt on the main thread, and a click handler runs inside that
// rebuild. Any command executed there blocks the frame: printing a test page
// spends seconds in the Windows spooler, so the window stopped responding and
// Windows painted "Not responding" over it. Every command that can take longer
// than a frame therefore runs on a goroutine and reports back through the
// context's Invalidate, which mygo documents as safe from any goroutine.

// busyTask is one command in flight, so the UI can show progress and refuse a
// second click on the same action.
type busyTask struct {
	// running is true while the goroutine is still working.
	running bool
	// err holds the failure of the last run, for the panel to display.
	err string
	// startedAt lets the UI recover from a lost completion notification. If a
	// frame arrives after the command's own timeout has passed and the task still
	// claims to be running, the flag is cleared rather than leaving the button
	// stuck on its progress label forever.
	startedAt time.Time
	// timeout is how long the command was allowed to take.
	timeout time.Duration
}

// expireIfOverdue clears the running flag when the deadline has passed.
//
// This is a safety net for the reported bug: the button stayed on "printing…"
// after a print that did complete. A lost Invalidate or a panic in a completion
// callback must not be able to freeze the button permanently, so the rebuild path
// itself checks the deadline.
func (t *busyTask) expireIfOverdue(now time.Time) {
	if t == nil || !t.running || t.timeout <= 0 || t.startedAt.IsZero() {
		return
	}
	if now.Sub(t.startedAt) > t.timeout+2*time.Second {
		t.running = false
		if t.err == "" {
			t.err = "the operation timed out"
		}
	}
}

// asyncResult is what a background command reports back.
type asyncResult struct {
	// resp is the command response, valid when err is empty.
	resp command.Response
	// err is the transport-level failure, if any.
	err error
}

// runAsync executes req on a goroutine and applies the result on the main
// thread, then asks for a new frame.
//
// done runs through Window.Update, so it touches UI state on the main thread and
// the window is told to rebuild right after it returns. That is stronger than
// calling Context.Invalidate from the goroutine: Invalidate resolves the engine
// through a weak pointer, so a collected engine would silently drop the frame and
// leave the button stuck on its progress label.
func (d *desktop) runAsync(c *ui.Context, task *busyTask, timeout time.Duration, req command.Request, done func(command.Response)) {
	if task != nil {
		if task.running {
			return
		}
		task.running = true
		task.err = ""
		task.startedAt = time.Now()
		task.timeout = timeout
	}
	window := d.window
	services := c.Services()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		resp := d.service.Execute(ctx, req)
		apply := func() {
			if task != nil {
				task.running = false
				if resp.Error != "" {
					task.err = resp.Error
				}
			}
			if done != nil {
				done(resp)
			}
		}
		if window != nil {
			// Runs on the main thread and schedules the rebuild.
			window.Update(apply)
			return
		}
		// No window (tests): apply directly and still ask for a frame.
		apply()
		services.Invalidate()
	}()
}

// debugLog is retained for ad-hoc diagnosis of a stuck UI, which a GUI build
// cannot report on stdout. Enable with PRINTBRIDGE_DEBUG=1.
func (d *desktop) debugLog(format string, args ...any) {
	if os.Getenv("PRINTBRIDGE_DEBUG") == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(d.store.Dir(), "async.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

// busyLabel renders the button label for a task, showing progress while it runs.
// It also applies the overdue safety net, so a lost completion cannot leave the
// button on its progress label forever.
func busyLabel(d *desktop, task *busyTask, idleKey, busyKey string) string {
	if task == nil {
		return d.tr(idleKey)
	}
	task.expireIfOverdue(time.Now())
	if task.running {
		return d.tr(busyKey)
	}
	return d.tr(idleKey)
}
