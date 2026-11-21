package store

import (
	"context"
	"testing"
	"time"
)

// TestRecordRemoteJobIsIdempotent is the core of remote job dedup: the first call
// records, every later call reports "already seen".
func TestRecordRemoteJobIsIdempotent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	fresh, err := s.RecordRemoteJob(ctx, "JOB-1")
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatal("first record was not reported as fresh")
	}
	for i := 0; i < 3; i++ {
		fresh, err := s.RecordRemoteJob(ctx, "JOB-1")
		if err != nil {
			t.Fatal(err)
		}
		if fresh {
			t.Fatalf("repeat %d was reported as fresh", i+1)
		}
	}
}

// TestRecordRemoteBatchIsIdempotent covers whole-batch idempotency: a re-delivered
// batch must be recognised as already processed.
func TestRecordRemoteBatchIsIdempotent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	fresh, err := s.RecordRemoteBatch(ctx, "BATCH-1")
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatal("first batch record was not fresh")
	}
	fresh, err = s.RecordRemoteBatch(ctx, "BATCH-1")
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatal("re-delivered batch was reported as fresh")
	}
}

// TestRemoteDedupSurvivesHistoryPrune is the reason dedup lives in its own table:
// cleaning history must not make an already-printed remote job printable again.
func TestRemoteDedupSurvivesHistoryPrune(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if _, err := s.RecordRemoteJob(ctx, "JOB-PRUNE"); err != nil {
		t.Fatal(err)
	}
	// Insert and clear history rows for the same job.
	mustInsert(t, s, "JOB-PRUNE", "submitted", "done", time.Now().UTC())
	if _, err := s.ClearHistory(ctx); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.RecordRemoteJob(ctx, "JOB-PRUNE")
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatal("pruning history resurrected remote dedup")
	}
}

// TestRemoteDedupRejectedOnReadOnly keeps the CLI from writing dedup rows through
// a read-only handle.
func TestRemoteDedupRejectedOnReadOnly(t *testing.T) {
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
	if _, err := ro.RecordRemoteJob(context.Background(), "x"); err == nil {
		t.Fatal("RecordRemoteJob succeeded on a read-only store")
	}
	if _, err := ro.RecordRemoteBatch(context.Background(), "y"); err == nil {
		t.Fatal("RecordRemoteBatch succeeded on a read-only store")
	}
}
