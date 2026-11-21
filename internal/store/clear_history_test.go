package store

import (
	"context"
	"testing"
	"time"
)

// TestClearHistoryKeepsInFlightJobs is the safety property of `task clear`: a job
// whose latest status is still non-terminal may be about to receive another
// status write, so its rows must survive.
func TestClearHistoryKeepsInFlightJobs(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	mustInsert(t, s, "done-1", "queued", "queued", now.Add(-3*time.Minute))
	mustInsert(t, s, "done-1", "submitted", "submitted to system print queue", now.Add(-2*time.Minute))
	mustInsert(t, s, "failed-1", "queued", "queued", now.Add(-2*time.Minute))
	mustInsert(t, s, "failed-1", "failed", "download failed", now.Add(-time.Minute))
	mustInsert(t, s, "live-1", "queued", "queued", now.Add(-time.Minute))
	mustInsert(t, s, "live-1", "printing", "printing", now)

	removed, err := s.ClearHistory(ctx)
	if err != nil {
		t.Fatalf("ClearHistory: %v", err)
	}
	if removed == 0 {
		t.Fatal("ClearHistory deleted nothing")
	}

	if st, err := s.Latest(ctx, "done-1"); err == nil {
		t.Fatalf("terminal job survived the clear: %#v", st)
	}
	if st, err := s.Latest(ctx, "failed-1"); err == nil {
		t.Fatalf("failed job survived the clear: %#v", st)
	}
	st, err := s.Latest(ctx, "live-1")
	if err != nil {
		t.Fatalf("in-flight job was deleted: %v", err)
	}
	if st.Status != "printing" {
		t.Fatalf("in-flight job status = %q, want printing", st.Status)
	}

	timeline, err := s.JobHistory(ctx, "live-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline) != 2 {
		t.Fatalf("in-flight timeline length = %d, want 2", len(timeline))
	}
}

// TestClearHistoryIsIdempotent keeps a second invocation from failing on an
// already-empty history.
func TestClearHistoryIsIdempotent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := s.ClearHistory(ctx); err != nil {
		t.Fatalf("first clear: %v", err)
	}
	if _, err := s.ClearHistory(ctx); err != nil {
		t.Fatalf("second clear: %v", err)
	}
}

// TestClearHistoryRejectedOnReadOnly keeps the CLI from mutating a live agent's
// database through the read-only handle.
func TestClearHistoryRejectedOnReadOnly(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustInsert(t, w, "j", "submitted", "done", time.Now().UTC())
	w.Close()

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.ClearHistory(context.Background()); err == nil {
		t.Fatal("ClearHistory succeeded on a read-only store")
	}
}
