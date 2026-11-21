package store

// Tests for the read-only handle, history retention and history queries.
// The original Open/RecoverInterrupted tests live in store_test.go and must
// keep passing unchanged.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// mustInsert writes one status through a writable handle using an explicit
// timestamp so retention tests can age rows deterministically.
func mustInsert(t *testing.T, s *Store, job, status, message string, at time.Time) {
	t.Helper()
	if err := s.AddStatus(context.Background(), domain.Status{
		JobID: job, RequestID: "req-" + job, Status: status, Message: message, At: at,
	}); err != nil {
		t.Fatalf("AddStatus(%s,%s): %v", job, status, err)
	}
}

// countRows is a direct read used to prove a read-only open changed nothing.
func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestOpenReadOnlyDoesNotRecover is the regression test for the P0 bug: a
// second process opening the store while the Agent runs must not mark live jobs
// as failed.
func TestOpenReadOnlyDoesNotRecover(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx := context.Background()

	// A job mid-flight plus a job that is still queued: both nonterminal.
	for _, st := range []domain.Status{
		{JobID: "live-1", Status: "queued", Message: "waiting"},
		{JobID: "live-2", Status: "printing", Message: "spooling"},
		{JobID: "done-1", Status: "completed", Message: "ok"},
	} {
		if err := w.AddStatus(ctx, st); err != nil {
			t.Fatal(err)
		}
	}
	before := countRows(t, w, "job_status")

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if !ro.ReadOnly() {
		t.Fatal("OpenReadOnly returned a writable store")
	}

	// The critical assertion: live state is untouched.
	for job, want := range map[string]string{"live-1": "queued", "live-2": "printing", "done-1": "completed"} {
		st, err := ro.Latest(ctx, job)
		if err != nil {
			t.Fatalf("Latest(%s): %v", job, err)
		}
		if st.Status != want {
			t.Errorf("job %s: status = %q, want %q (read-only open must not recover)", job, st.Status, want)
		}
	}
	if got := countRows(t, ro, "job_status"); got != before {
		t.Errorf("read-only open changed row count: %d -> %d (RecoverInterrupted must not run)", before, got)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-reading through the writable handle shows the writer's view is intact.
	for job, want := range map[string]string{"live-1": "queued", "live-2": "printing"} {
		st, err := w.Latest(ctx, job)
		if err != nil || st.Status != want {
			t.Fatalf("after read-only close, %s = %#v %v, want %q", job, st, err, want)
		}
	}

	// Sanity: the exclusive path still recovers, so the split did not disable
	// recovery where it is required.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	st, err := w2.Latest(ctx, "live-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "failed" || st.Message != "interrupted by previous exit" {
		t.Errorf("Open() no longer recovers: %#v", st)
	}
}

// TestOpenReadOnlyRejectsWrites proves the readOnly guard covers every mutating
// method, including the ones that touch SQLite rather than config.json.
func TestOpenReadOnlyRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustInsert(t, w, "j1", "queued", "queued", time.Now().UTC())
	if err := w.AddRemoteEvent(context.Background(), RemoteEvent{EventID: "e1", JobID: "j1", OccurredAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	ctx := context.Background()

	cfg := ro.Config()
	now := time.Now().UTC()
	cases := []struct {
		name string
		call func() error
	}{
		{"SetConfig", func() error { return ro.SetConfig(cfg) }},
		{"AddStatus", func() error { return ro.AddStatus(ctx, domain.Status{JobID: "j1", Status: "failed"}) }},
		{"AddStatuses", func() error {
			return ro.AddStatuses(ctx, []domain.Status{{JobID: "j1", Status: "failed"}})
		}},
		{"AddRemoteEvent", func() error {
			return ro.AddRemoteEvent(ctx, RemoteEvent{EventID: "e2", JobID: "j1", OccurredAt: now.Format(time.RFC3339)})
		}},
		{"AckRemoteEvent", func() error { return ro.AckRemoteEvent(ctx, "e1") }},
		{"RetryRemoteEvent", func() error { return ro.RetryRemoteEvent(ctx, "e1", 3, now) }},
		{"FailRemoteEvent", func() error { return ro.FailRemoteEvent(ctx, "e1", 3, "HTTP 500") }},
		{"PruneHistory", func() error { _, err := ro.PruneHistory(ctx, 1); return err }},
		{"RecoverInterrupted", func() error { return ro.RecoverInterrupted(ctx) }},
	}
	for _, c := range cases {
		err := c.call()
		if err == nil {
			t.Errorf("%s succeeded on a read-only store", c.name)
			continue
		}
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s error = %v, want errors.Is(err, ErrReadOnly)", c.name, err)
		}
	}

	// Nothing leaked into the database or the config file.
	var events int
	if err := ro.db.QueryRow(`SELECT COUNT(*) FROM remote_outbox`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Errorf("remote_outbox rows = %d, want 1 (write leaked)", events)
	}
	st, err := ro.Latest(ctx, "j1")
	if err != nil || st.Status != "queued" {
		t.Errorf("job state changed through read-only store: %#v %v", st, err)
	}
}

// TestOpenReadOnlyMissingDatabase covers graceful degradation before the Agent
// has ever run: no database file means ErrNoDatabase and no file created.
func TestOpenReadOnlyMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	_, err := OpenReadOnly(dir)
	if err == nil {
		t.Fatal("OpenReadOnly on an empty directory succeeded")
	}
	if !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("error = %v, want errors.Is(err, ErrNoDatabase)", err)
	}

	// The directory must not gain a database (or anything else) from the probe.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("read-only open created %v in an empty directory", names)
	}

	// A path that does not exist at all is also ErrNoDatabase, not a stat error.
	missing := filepath.Join(dir, "nope")
	if _, err := OpenReadOnly(missing); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("missing dir error = %v, want ErrNoDatabase", err)
	}
}

// TestPruneHistoryKeepsLatestPerJob is the retention-safety test: a job whose
// newest status is already older than the window must keep that row, otherwise
// Latest() would lose the terminal state.
func TestPruneHistoryKeepsLatestPerJob(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-90 * 24 * time.Hour)

	// job-old: entirely outside the window, terminal state must survive.
	mustInsert(t, s, "job-old", "queued", "queued", old)
	mustInsert(t, s, "job-old", "downloading", "downloading", old.Add(time.Minute))
	mustInsert(t, s, "job-old", "completed", "done", old.Add(2*time.Minute))
	// job-mixed: one stale row that is safe to delete, one recent row.
	mustInsert(t, s, "job-mixed", "queued", "queued", old)
	mustInsert(t, s, "job-mixed", "completed", "done", now.Add(-time.Hour))
	// job-fresh: untouched.
	mustInsert(t, s, "job-fresh", "queued", "queued", now.Add(-2*time.Hour))

	// A dead outbox row past the window and a live one inside it.
	if err := s.AddRemoteEvent(ctx, RemoteEvent{EventID: "dead-old", JobID: "job-old", OccurredAt: old.Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	if err := s.FailRemoteEvent(ctx, "dead-old", 3, "HTTP 500"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE remote_outbox SET dead_at=? WHERE event_id='dead-old'`, old.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRemoteEvent(ctx, RemoteEvent{EventID: "pending", JobID: "job-fresh", OccurredAt: now.Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}

	before := countRows(t, s, "job_status")
	removed, err := s.PruneHistory(ctx, 30)
	if err != nil {
		t.Fatalf("PruneHistory: %v", err)
	}
	// Expect job-old:queued + job-old:downloading + job-mixed:queued + dead outbox.
	if removed != 4 {
		t.Errorf("removed = %d, want 4", removed)
	}
	if after := countRows(t, s, "job_status"); after != before-3 {
		t.Errorf("job_status rows = %d, want %d", after, before-3)
	}

	// The whole point: Latest() still returns the terminal state of the stale job.
	st, err := s.Latest(ctx, "job-old")
	if err != nil {
		t.Fatalf("Latest(job-old) after prune: %v", err)
	}
	if st.Status != "completed" || st.Message != "done" {
		t.Errorf("Latest(job-old) = %#v, want completed/done", st)
	}
	// Its older rows are gone, so the timeline is a single retained row.
	hist, err := s.JobHistory(ctx, "job-old")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Status != "completed" {
		t.Errorf("job-old timeline = %#v, want only the retained terminal row", hist)
	}

	// Recent jobs are intact, including their full timeline.
	hist, err = s.JobHistory(ctx, "job-mixed")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Status != "completed" {
		t.Errorf("job-mixed timeline = %#v, want the single recent row", hist)
	}
	if st, err := s.Latest(ctx, "job-fresh"); err != nil || st.Status != "queued" {
		t.Errorf("Latest(job-fresh) = %#v %v", st, err)
	}

	// Dead outbox pruning: the stale dead letter is gone, the pending one stays.
	var deadLeft, pendingLeft int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM remote_outbox WHERE event_id='dead-old'`).Scan(&deadLeft); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM remote_outbox WHERE event_id='pending'`).Scan(&pendingLeft); err != nil {
		t.Fatal(err)
	}
	if deadLeft != 0 || pendingLeft != 1 {
		t.Errorf("outbox after prune: dead-old=%d pending=%d, want 0 and 1", deadLeft, pendingLeft)
	}

	// A second prune is a no-op: the retained rows are all protected.
	again, err := s.PruneHistory(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("second prune removed %d rows, want 0", again)
	}
}

// TestPruneHistoryRetentionFallback checks the defensive paths: retention comes
// from config when the argument is not positive, and an unusable window deletes
// nothing rather than everything.
func TestPruneHistoryRetentionFallback(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	stale := now.Add(-90 * 24 * time.Hour)
	mustInsert(t, s, "j1", "queued", "queued", stale)
	mustInsert(t, s, "j2", "completed", "done", now)

	cfg := s.Config()
	if cfg.Remote.HistoryRetentionDays <= 0 {
		t.Fatalf("test precondition: default retention = %d", cfg.Remote.HistoryRetentionDays)
	}
	// A non-positive argument falls back to the configured window.
	if _, err := s.PruneHistory(ctx, 0); err != nil {
		t.Fatalf("PruneHistory(0): %v", err)
	}
	if st, err := s.Latest(ctx, "j2"); err != nil || st.Status != "completed" {
		t.Fatalf("recent job lost: %#v %v", st, err)
	}

	// With the window disabled entirely, delete nothing instead of everything.
	cfg.Remote.HistoryRetentionDays = 0
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, s, "job_status")
	if _, err := s.PruneHistory(ctx, 0); err == nil {
		t.Fatal("PruneHistory with a disabled window succeeded")
	}
	if after := countRows(t, s, "job_status"); after != before {
		t.Errorf("rows = %d, want %d (a disabled window must not prune)", after, before)
	}
}

// TestListJobsAndJobHistory covers aggregation, ordering, filtering and the
// per-job timeline.
func TestListJobsAndJobHistory(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	mustInsert(t, s, "job-a", "queued", "queued", now.Add(-3*time.Hour))
	mustInsert(t, s, "job-a", "printing", "spooling", now.Add(-2*time.Hour))
	mustInsert(t, s, "job-a", "completed", "printed", now.Add(-1*time.Hour))
	mustInsert(t, s, "job-b", "failed", "printer offline", now.Add(-30*time.Minute))
	mustInsert(t, s, "job-c", "completed", "ok", now.Add(-90*time.Minute))

	jobs, err := s.ListJobs(ctx, 10, 0, "", "")
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("got %d jobs, want 3: %#v", len(jobs), jobs)
	}
	// Newest status first: job-b (30m), job-a (1h), job-c (1.5h).
	wantOrder := []string{"job-b", "job-a", "job-c"}
	for i, want := range wantOrder {
		if jobs[i].JobID != want {
			t.Errorf("jobs[%d].JobID = %q, want %q (ordering)", i, jobs[i].JobID, want)
		}
	}
	// The summary carries the LATEST status, not the first one.
	if jobs[1].LatestStatus != "completed" || jobs[1].Message != "printed" || jobs[1].Events != 3 {
		t.Errorf("job-a summary = %#v, want completed/printed/3 events", jobs[1])
	}
	if jobs[1].UpdatedAt.IsZero() || !jobs[1].UpdatedAt.Equal(now.Add(-1*time.Hour).Truncate(time.Nanosecond)) {
		if got := jobs[1].UpdatedAt; got.Sub(now.Add(-1*time.Hour)).Abs() > time.Second {
			t.Errorf("job-a UpdatedAt = %v, want about %v", got, now.Add(-time.Hour))
		}
	}

	// Status filter operates on the latest status.
	failed, err := s.ListJobs(ctx, 10, 0, "failed", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].JobID != "job-b" {
		t.Errorf("status filter = %#v, want only job-b", failed)
	}
	// An unmatched filter yields an empty slice, not nil and not an error.
	none, err := s.ListJobs(ctx, 10, 0, "queued", "")
	if err != nil {
		t.Fatal(err)
	}
	if none == nil || len(none) != 0 {
		t.Errorf("unmatched status filter = %#v, want empty non-nil slice", none)
	}

	// Query filter is a literal substring match on job_id.
	byQuery, err := s.ListJobs(ctx, 10, 0, "", "job-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(byQuery) != 1 || byQuery[0].JobID != "job-a" {
		t.Errorf("query filter = %#v, want job-a only", byQuery)
	}
	// LIKE wildcards in user input must not act as wildcards.
	for _, wildcard := range []string{"%", "_", "job-%"} {
		got, err := s.ListJobs(ctx, 10, 0, "", wildcard)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("query %q matched %d jobs, want 0 (wildcards must be escaped)", wildcard, len(got))
		}
	}

	// Paging is deterministic and stable across calls.
	page1, err := s.ListJobs(ctx, 2, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	page2, err := s.ListJobs(ctx, 2, 2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || len(page2) != 1 {
		t.Fatalf("paging = %d/%d rows, want 2/1", len(page1), len(page2))
	}
	if page1[0].JobID != "job-b" || page1[1].JobID != "job-a" || page2[0].JobID != "job-c" {
		t.Errorf("paging order = %q,%q | %q", page1[0].JobID, page1[1].JobID, page2[0].JobID)
	}
	// Paging past the end is empty, not an error.
	past, err := s.ListJobs(ctx, 2, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(past) != 0 {
		t.Errorf("page past end = %#v, want empty", past)
	}

	// JobHistory is ascending (oldest first) and complete.
	timeline, err := s.JobHistory(ctx, "job-a")
	if err != nil {
		t.Fatalf("JobHistory: %v", err)
	}
	if len(timeline) != 3 {
		t.Fatalf("job-a timeline = %d entries, want 3: %#v", len(timeline), timeline)
	}
	for i, want := range []string{"queued", "printing", "completed"} {
		if timeline[i].Status != want {
			t.Errorf("timeline[%d] = %q, want %q", i, timeline[i].Status, want)
		}
		if timeline[i].Type != "job_status" {
			t.Errorf("timeline[%d].Type = %q, want job_status", i, timeline[i].Type)
		}
	}
	// Ascending id order implies ascending time here.
	if !timeline[0].At.Before(timeline[1].At) || !timeline[1].At.Before(timeline[2].At) {
		t.Errorf("timeline not ascending: %v", []time.Time{timeline[0].At, timeline[1].At, timeline[2].At})
	}
	// An unknown job yields an empty slice rather than an error.
	empty, err := s.JobHistory(ctx, "no-such-job")
	if err != nil {
		t.Fatalf("JobHistory(unknown): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("JobHistory(unknown) = %#v, want empty non-nil slice", empty)
	}

	// The read-only handle exposes the same read API.
	ro, err := OpenReadOnly(s.Dir())
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	roJobs, err := ro.ListJobs(ctx, 10, 0, "", "")
	if err != nil {
		t.Fatalf("read-only ListJobs: %v", err)
	}
	if len(roJobs) != len(jobs) {
		t.Errorf("read-only ListJobs = %d rows, writable = %d", len(roJobs), len(jobs))
	}
	roTimeline, err := ro.JobHistory(ctx, "job-a")
	if err != nil || len(roTimeline) != 3 {
		t.Errorf("read-only JobHistory = %d rows %v, want 3", len(roTimeline), err)
	}
}

// TestOpenReadOnlyUnusableFile covers the leftovers of an interrupted first
// start: a zero-byte history file is reported as missing, and a file that is
// not a database at all returns a real error rather than a writable handle.
func TestOpenReadOnlyUnusableFile(t *testing.T) {
	t.Run("zero-byte file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "history.sqlite3"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := OpenReadOnly(dir)
		if !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("error = %v, want ErrNoDatabase", err)
		}
		// The read-only probe must not have written anything into the file.
		info, statErr := os.Stat(filepath.Join(dir, "history.sqlite3"))
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Size() != 0 {
			t.Errorf("file size = %d, want 0 (read-only open must not initialize)", info.Size())
		}
	})
	t.Run("not a database", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "history.sqlite3"), []byte("this is not sqlite"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenReadOnly(dir); err == nil {
			t.Fatal("OpenReadOnly accepted a non-database file")
		}
	})
}

// TestOpenReadOnlyConcurrentWriter is the cross-process safety proof: a
// writable handle stays open (as the Agent would be) while a read-only handle
// is opened, read and closed in the same process. The queued row must survive.
func TestOpenReadOnlyConcurrentWriter(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx := context.Background()
	if err := w.AddStatus(ctx, domain.Status{JobID: "live", Status: "queued", Message: "waiting"}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		ro, err := OpenReadOnly(dir)
		if err != nil {
			t.Fatalf("round %d OpenReadOnly: %v", i, err)
		}
		st, err := ro.Latest(ctx, "live")
		if err != nil {
			t.Fatalf("round %d Latest: %v", i, err)
		}
		if st.Status != "queued" {
			t.Fatalf("round %d: status = %q, want queued", i, st.Status)
		}
		if err := ro.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// The writer keeps working after readers opened and closed the database.
	if err := w.AddStatus(ctx, domain.Status{JobID: "live", Status: "printing", Message: "spooling"}); err != nil {
		t.Fatalf("writer blocked by read-only handles: %v", err)
	}
	st, err := w.Latest(ctx, "live")
	if err != nil || st.Status != "printing" {
		t.Fatalf("writer state = %#v %v, want printing", st, err)
	}
}

// TestOpenReadOnlyDSNIsEscaped guards the path-escaping requirement: a data
// directory containing URI-significant characters must still resolve to the
// real database. A naive "file:"+path DSN silently opens a different file.
func TestOpenReadOnlyDSNIsEscaped(t *testing.T) {
	// The temp root is process-wide, so build the awkward name inside it.
	root := t.TempDir()
	dir := filepath.Join(root, "data dir %41 #x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := w.AddStatus(ctx, domain.Status{JobID: "escaped", Status: "queued", Message: "queued"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("OpenReadOnly(%q): %v", dir, err)
	}
	defer ro.Close()
	st, err := ro.Latest(ctx, "escaped")
	if err != nil {
		t.Fatalf("read escaped path: %v", err)
	}
	if st.Status != "queued" {
		t.Fatalf("status = %q, want queued (DSN path was not escaped correctly)", st.Status)
	}
}
