package command

import (
	"context"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/store"
)

// deadLetterService builds a service over a throwaway store with remote polling
// enabled, which the retry command requires.
func deadLetterService(t *testing.T) (Service, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := s.Config()
	c.Remote.Enabled = true
	c.Remote.EndpointURL = "https://example.com/jobs"
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	return Service{Store: s}, s
}

// seedCommandDeadLetter inserts one abandoned report.
func seedCommandDeadLetter(t *testing.T, s *store.Store, eventID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.AddRemoteEvent(ctx, store.RemoteEvent{
		EventID: eventID, JobID: "job-" + eventID, Status: "success",
		OccurredAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FailRemoteEvent(ctx, eventID, 10, "HTTP 500"); err != nil {
		t.Fatal(err)
	}
}

// TestListDeadLettersCommand covers the read path the UI and CLI both use.
func TestListDeadLettersCommand(t *testing.T) {
	svc, s := deadLetterService(t)
	seedCommandDeadLetter(t, s, "dead-1")

	var r Response
	if err := svc.listDeadLetters(context.Background(), Request{Limit: 10}, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.DeadLetters) != 1 || r.DeadLetters[0].EventID != "dead-1" {
		t.Fatalf("dead letters = %+v", r.DeadLetters)
	}
}

// TestRemoteStatsCommand covers the backlog counters.
func TestRemoteStatsCommand(t *testing.T) {
	svc, s := deadLetterService(t)
	seedCommandDeadLetter(t, s, "dead-1")

	var r Response
	if err := svc.remoteStats(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	if r.RemoteStats == nil || r.RemoteStats.Dead != 1 {
		t.Fatalf("stats = %+v", r.RemoteStats)
	}
}

// TestRetryDeadLettersRefusedWhenPollingDisabled keeps a "successful" retry from
// being reported when nothing would ever send the revived rows.
func TestRetryDeadLettersRefusedWhenPollingDisabled(t *testing.T) {
	svc, s := deadLetterService(t)
	seedCommandDeadLetter(t, s, "dead-1")
	c := s.Config()
	c.Remote.Enabled = false
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}

	var r Response
	err := svc.retryDeadLetters(context.Background(), Request{EventIDs: []string{"dead-1"}}, &r)
	if err == nil {
		t.Fatal("retry succeeded with polling disabled")
	}
	if domain.CodeOf(err) != domain.CodeRemotePaused {
		t.Fatalf("code = %q, want %q", domain.CodeOf(err), domain.CodeRemotePaused)
	}
}

// TestRetryDeadLettersUnknownIdReportsNotFound keeps a typo from looking like a
// successful retry.
func TestRetryDeadLettersUnknownIdReportsNotFound(t *testing.T) {
	svc, _ := deadLetterService(t)
	var r Response
	err := svc.retryDeadLetters(context.Background(), Request{EventIDs: []string{"missing"}}, &r)
	if err == nil {
		t.Fatal("an unknown event id was accepted")
	}
	if domain.CodeOf(err) != domain.CodeJobNotFound {
		t.Fatalf("code = %q", domain.CodeOf(err))
	}
}

// TestRetryDeadLettersSucceeds checks the happy path returns a count.
func TestRetryDeadLettersSucceeds(t *testing.T) {
	svc, s := deadLetterService(t)
	seedCommandDeadLetter(t, s, "dead-1")

	var r Response
	if err := svc.retryDeadLetters(context.Background(), Request{EventIDs: []string{"dead-1"}}, &r); err != nil {
		t.Fatal(err)
	}
	if r.Removed != 1 {
		t.Fatalf("removed = %d, want 1", r.Removed)
	}
}

// TestDeleteDeadLettersRefusesEmptySelection is the safety property: the
// destructive all-form must be unreachable by omitting the selection.
func TestDeleteDeadLettersRefusesEmptySelection(t *testing.T) {
	svc, s := deadLetterService(t)
	seedCommandDeadLetter(t, s, "dead-1")

	var r Response
	err := svc.deleteDeadLetters(context.Background(), Request{}, &r)
	if err == nil {
		t.Fatal("an empty selection deleted rows")
	}
	if domain.CodeOf(err) != domain.CodeInvalidMessage {
		t.Fatalf("code = %q", domain.CodeOf(err))
	}
	stats, _ := s.RemoteStats(context.Background())
	if stats.Dead != 1 {
		t.Fatalf("a row was deleted anyway: %+v", stats)
	}
}

// TestDeleteDeadLettersBySelection removes exactly the named rows.
func TestDeleteDeadLettersBySelection(t *testing.T) {
	svc, s := deadLetterService(t)
	seedCommandDeadLetter(t, s, "dead-1")
	seedCommandDeadLetter(t, s, "dead-2")

	var r Response
	if err := svc.deleteDeadLetters(context.Background(), Request{EventIDs: []string{"dead-1"}}, &r); err != nil {
		t.Fatal(err)
	}
	if r.Removed != 1 {
		t.Fatalf("removed = %d, want 1", r.Removed)
	}
	stats, _ := s.RemoteStats(context.Background())
	if stats.Dead != 1 {
		t.Fatalf("stats = %+v, want 1 remaining", stats)
	}
}

// TestPurgeDeadLettersRemovesAll is the explicit all-form.
func TestPurgeDeadLettersRemovesAll(t *testing.T) {
	svc, s := deadLetterService(t)
	seedCommandDeadLetter(t, s, "dead-1")
	seedCommandDeadLetter(t, s, "dead-2")

	var r Response
	if err := svc.purgeDeadLetters(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Removed != 2 {
		t.Fatalf("removed = %d, want 2", r.Removed)
	}
	stats, _ := s.RemoteStats(context.Background())
	if stats.Dead != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}
