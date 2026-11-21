package main

import (
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/store"
)

// seedDeadLetter inserts one abandoned status report into the desktop's store.
func seedDeadLetter(t *testing.T, d *desktop, eventID, jobID, lastError string) {
	t.Helper()
	ctx := t.Context()
	if err := d.store.AddRemoteEvent(ctx, store.RemoteEvent{
		EventID: eventID, JobID: jobID, RequestID: "REQ-" + jobID,
		Status: "success", OccurredAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.FailRemoteEvent(ctx, eventID, 10, lastError); err != nil {
		t.Fatal(err)
	}
}

// TestDeadLettersPanelRendersEmptyState covers the "all delivered" case.
func TestDeadLettersPanelRendersEmptyState(t *testing.T) {
	d := newTestDesktop(t)
	d.refreshRemoteBacklog()
	texts := textsOf(t, func(c *ui.Context) { d.deadLettersPanel(c) })
	for _, want := range []string{d.tr("dead_letters"), d.tr("no_dead_letters")} {
		if !containsText(texts, want) {
			t.Errorf("dead-letter panel does not render %q; got %v", want, texts)
		}
	}
}

// TestDeadLettersPanelRendersEntries checks the row, the reason and both actions.
func TestDeadLettersPanelRendersEntries(t *testing.T) {
	d := newTestDesktop(t)
	seedDeadLetter(t, d, "evt-1", "JOB-DEAD", "connection refused")
	d.refreshRemoteBacklog()

	texts := textsOf(t, func(c *ui.Context) { d.deadLettersPanel(c) })
	for _, want := range []string{
		"JOB-DEAD",
		"connection refused",
		d.tr("last_error"),
		d.tr("retry_one"),
		d.tr("delete_one"),
		d.tr("retry_all_dead_letters"),
		d.tr("purge_dead_letters"),
	} {
		if !containsText(texts, want) {
			t.Errorf("dead-letter panel does not render %q; got %v", want, texts)
		}
	}
}

// TestDeadLetterCountTracksBacklog feeds the overview badge.
func TestDeadLetterCountTracksBacklog(t *testing.T) {
	d := newTestDesktop(t)
	if got := d.deadLetterCount(); got != 0 {
		t.Fatalf("initial dead count = %d, want 0", got)
	}
	seedDeadLetter(t, d, "evt-1", "JOB-1", "HTTP 500")
	d.refreshRemoteBacklog()
	if got := d.deadLetterCount(); got != 1 {
		t.Fatalf("dead count = %d, want 1", got)
	}
}

// TestRetryDeadLetterFromUIRefreshesList keeps the panel in step with the store.
func TestRetryDeadLetterFromUIRefreshesList(t *testing.T) {
	d := newTestDesktop(t)
	// The retry command requires polling to be enabled.
	c := d.store.Config()
	c.Remote.Enabled = true
	c.Remote.EndpointURL = "https://example.com/jobs"
	if err := d.store.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	seedDeadLetter(t, d, "evt-1", "JOB-1", "HTTP 500")
	d.refreshRemoteBacklog()

	d.retryDeadLetters([]string{"evt-1"})
	if len(d.deadLetters) != 0 {
		t.Fatalf("dead letters after retry = %d, want 0", len(d.deadLetters))
	}
	if d.remoteStats == nil || d.remoteStats.Pending != 1 {
		t.Fatalf("pending after retry = %+v, want 1", d.remoteStats)
	}
}

// TestDeleteDeadLetterFromUIRefreshesList covers the discard path.
func TestDeleteDeadLetterFromUIRefreshesList(t *testing.T) {
	d := newTestDesktop(t)
	seedDeadLetter(t, d, "evt-1", "JOB-1", "HTTP 500")
	d.refreshRemoteBacklog()

	d.deleteDeadLetters([]string{"evt-1"})
	if len(d.deadLetters) != 0 {
		t.Fatalf("dead letters after delete = %d, want 0", len(d.deadLetters))
	}
}

// TestPurgeDeadLettersModalRendersConfirm keeps the destructive all-form behind a
// confirmation.
func TestPurgeDeadLettersModalRendersConfirm(t *testing.T) {
	d := newTestDesktop(t)
	d.openModal(modalPurgeDeadLetters)
	texts := textsOf(t, func(c *ui.Context) { d.purgeDeadLettersModal(c) })
	for _, want := range []string{
		d.tr("purge_dead_letters_title"),
		d.tr("purge_dead_letters_body"),
		d.tr("purge_dead_letters_confirm"),
		d.tr("cancel"),
	} {
		if !containsText(texts, want) {
			t.Errorf("purge dialog does not render %q; got %v", want, texts)
		}
	}
}

// TestRemotePageIncludesDeadLetterPanel keeps the panel reachable from the tab.
func TestRemotePageIncludesDeadLetterPanel(t *testing.T) {
	d := newTestDesktop(t)
	seedDeadLetter(t, d, "evt-1", "JOB-DEAD", "timeout")
	d.refreshRemoteBacklog()
	texts := textsOf(t, func(c *ui.Context) { d.remote(c) })
	if !containsText(texts, "JOB-DEAD") {
		t.Errorf("remote page does not show the dead letter; got %v", texts)
	}
}
