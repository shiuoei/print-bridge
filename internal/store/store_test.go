package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
)

func TestFreshDataAndHistory(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Config()
	if c.Service.Port != 17890 || len(c.Security.AllowedOrigins) != 0 {
		t.Fatalf("unexpected fresh config: %#v", c)
	}
	c.Security.AllowedOrigins = []string{"https://example.com"}
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	if err := s.AddStatus(context.Background(), domain.Status{JobID: "j1", Status: "queued", Message: "queued"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if !s.Config().OriginAllowed("https://example.com") {
		t.Fatal("config not persisted")
	}
	st, err := s.Latest(context.Background(), "j1")
	if err != nil || st.Status != "failed" || st.Message != "interrupted by previous exit" {
		t.Fatalf("history: %#v %v", st, err)
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version: %d, %v", version, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err = s.Latest(context.Background(), "j1")
	if err != nil || st.Status != "failed" {
		t.Fatalf("recovered status changed on second open: %#v %v", st, err)
	}
}

func TestRemoteEventDeadLetter(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.AddRemoteEvent(ctx, RemoteEvent{EventID: "e1", JobID: "j1", OccurredAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if err := s.FailRemoteEvent(ctx, "e1", 2, "HTTP 503"); err != nil {
		t.Fatal(err)
	}
	items, err := s.DueRemoteEvents(ctx, 10)
	if err != nil || len(items) != 0 {
		t.Fatalf("dead event still due: %#v %v", items, err)
	}
	var deadAt sql.NullString
	if err := s.db.QueryRow(`SELECT dead_at FROM remote_outbox WHERE event_id='e1'`).Scan(&deadAt); err != nil || !deadAt.Valid {
		t.Fatalf("dead event not retained: %#v %v", deadAt, err)
	}
}

func TestUpgradeUnversionedDevelopmentDatabase(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE job_status (
 id INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT NOT NULL, request_id TEXT NOT NULL,
 status TEXT NOT NULL, message TEXT NOT NULL, at TEXT NOT NULL
);
CREATE TABLE remote_outbox (
 event_id TEXT PRIMARY KEY, request_id TEXT NOT NULL, job_id TEXT NOT NULL,
 status TEXT NOT NULL, message TEXT NOT NULL, occurred_at TEXT NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0, next_at TEXT NOT NULL
);
INSERT INTO job_status(job_id,request_id,status,message,at) VALUES('old','req','submitted','submitted','2026-01-01T00:00:00Z');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("upgraded version: %d %v", version, err)
	}
	st, err := s.Latest(context.Background(), "old")
	if err != nil || st.Status != "submitted" {
		t.Fatalf("old history lost: %#v %v", st, err)
	}
}
