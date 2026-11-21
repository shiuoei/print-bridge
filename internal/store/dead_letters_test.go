package store

import (
	"context"
	"testing"
	"time"
)

// seedDeadLetter inserts one abandoned status report.
func seedDeadLetter(t *testing.T, s *Store, eventID, jobID, lastError string, deadAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := s.AddRemoteEvent(ctx, RemoteEvent{
		EventID: eventID, JobID: jobID, RequestID: "REQ-" + jobID,
		Status: "success", Message: "submitted", OccurredAt: deadAt.Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FailRemoteEvent(ctx, eventID, 10, lastError); err != nil {
		t.Fatal(err)
	}
	// FailRemoteEvent stamps "now"; pin the time so ordering is deterministic.
	if _, err := s.db.Exec(`UPDATE remote_outbox SET dead_at=? WHERE event_id=?`,
		deadAt.Format(time.RFC3339Nano), eventID); err != nil {
		t.Fatal(err)
	}
}

// TestListDeadLetters returns only abandoned events, newest first, with the reason.
func TestListDeadLetters(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	seedDeadLetter(t, s, "dead-old", "job-a", "HTTP 500", now.Add(-2*time.Hour))
	seedDeadLetter(t, s, "dead-new", "job-b", "connection refused", now.Add(-time.Minute))
	// A still-pending event must not appear in the dead-letter list.
	if err := s.AddRemoteEvent(ctx, RemoteEvent{EventID: "pending", JobID: "job-c", OccurredAt: now.Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}

	letters, err := s.ListDeadLetters(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(letters) != 2 {
		t.Fatalf("dead letters = %d, want 2", len(letters))
	}
	if letters[0].EventID != "dead-new" {
		t.Fatalf("newest first expected, got %q first", letters[0].EventID)
	}
	if letters[0].LastError != "connection refused" {
		t.Fatalf("last error = %q", letters[0].LastError)
	}
	if letters[0].JobID != "job-b" {
		t.Fatalf("job id = %q", letters[0].JobID)
	}
}

// TestRemoteStatsCountsPendingAndDead covers the two numbers the UI shows.
func TestRemoteStatsCountsPendingAndDead(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	seedDeadLetter(t, s, "dead-1", "job-a", "HTTP 500", now.Add(-time.Hour))
	for _, id := range []string{"p1", "p2"} {
		if err := s.AddRemoteEvent(ctx, RemoteEvent{EventID: id, JobID: "job-" + id, OccurredAt: now.Format(time.RFC3339Nano)}); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := s.RemoteStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Dead != 1 || stats.Pending != 2 {
		t.Fatalf("stats = %+v, want 1 dead and 2 pending", stats)
	}
}

// TestRetryDeadLettersRevivesOnlySelection checks a targeted retry clears the dead
// flag and resets attempts, leaving the other rows alone.
func TestRetryDeadLettersRevivesOnlySelection(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	seedDeadLetter(t, s, "keep-dead", "job-a", "HTTP 500", now.Add(-2*time.Hour))
	seedDeadLetter(t, s, "revive", "job-b", "HTTP 502", now.Add(-time.Hour))

	n, err := s.RetryDeadLetters(ctx, []string{"revive"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("revived %d, want 1", n)
	}
	stats, err := s.RemoteStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Dead != 1 || stats.Pending != 1 {
		t.Fatalf("stats after retry = %+v, want 1 dead and 1 pending", stats)
	}
	// The revived event must be due again, so the next report pass picks it up.
	due, err := s.DueRemoteEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range due {
		if e.EventID == "revive" {
			found = true
			if e.Attempts != 0 {
				t.Fatalf("revived event attempts = %d, want 0", e.Attempts)
			}
		}
	}
	if !found {
		t.Fatal("the revived event is not due")
	}
}

// TestDeleteDeadLettersRequiresSelection keeps the destructive path explicit.
func TestDeleteDeadLettersRequiresSelection(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	seedDeadLetter(t, s, "dead-1", "job-a", "HTTP 500", time.Now().UTC())

	if _, err := s.DeleteDeadLetters(ctx, []string{"nope"}); err != nil {
		t.Fatal(err)
	}
	stats, _ := s.RemoteStats(ctx)
	if stats.Dead != 1 {
		t.Fatalf("an unmatched id deleted a row: %+v", stats)
	}

	n, err := s.DeleteDeadLetters(ctx, []string{"dead-1"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted %d, want 1", n)
	}
	stats, _ = s.RemoteStats(ctx)
	if stats.Dead != 0 {
		t.Fatalf("dead letters remain: %+v", stats)
	}
}

// TestDeleteAllDeadLettersWithEmptySelection is the purge form used by the
// dedicated purge command.
func TestDeleteAllDeadLettersWithEmptySelection(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	seedDeadLetter(t, s, "dead-1", "job-a", "HTTP 500", now.Add(-time.Hour))
	seedDeadLetter(t, s, "dead-2", "job-b", "HTTP 501", now)
	if err := s.AddRemoteEvent(ctx, RemoteEvent{EventID: "pending", JobID: "job-c", OccurredAt: now.Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}

	n, err := s.DeleteDeadLetters(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("purged %d, want 2", n)
	}
	// Pending reports must survive a dead-letter purge.
	stats, _ := s.RemoteStats(ctx)
	if stats.Dead != 0 || stats.Pending != 1 {
		t.Fatalf("stats = %+v, want 0 dead and 1 pending", stats)
	}
}

// TestDeadLetterActionsRejectedOnReadOnly keeps the CLI from mutating a live
// agent's database through the read-only handle.
func TestDeadLetterActionsRejectedOnReadOnly(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	ctx := context.Background()
	if _, err := ro.RetryDeadLetters(ctx, []string{"x"}); err == nil {
		t.Fatal("RetryDeadLetters succeeded on a read-only store")
	}
	if _, err := ro.DeleteDeadLetters(ctx, []string{"x"}); err == nil {
		t.Fatal("DeleteDeadLetters succeeded on a read-only store")
	}
}
