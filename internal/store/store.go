package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
	_ "modernc.org/sqlite"
)

// ErrNoDatabase reports that a data directory holds no history database yet.
// Read-only callers (CLI, desktop UI) use it to degrade gracefully instead of
// creating or migrating a database behind the running Agent's back.
var ErrNoDatabase = errors.New("store: no history database")

// ErrReadOnly reports that a write was attempted through a read-only Store.
// OpenReadOnly returns such a Store so that online readers can never mutate
// database state or config while another process owns the data directory.
var ErrReadOnly = errors.New("store: read-only handle")

const historyFile = "history.sqlite3"
const configFile = "config.json"

type Store struct {
	dir    string
	db     *sql.DB
	mu     sync.RWMutex
	config domain.Config
	// readOnly is true for handles created by OpenReadOnly. It guards every
	// mutating method so that a mistaken call site fails loudly instead of
	// silently writing to a database another process owns.
	readOnly bool
}

func DefaultDir() (string, error) {
	if p := os.Getenv("PRINTBRIDGE_DATA_DIR"); p != "" {
		return filepath.Abs(p)
	}
	p, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(p, "print-bridge"), nil
}

// Open opens the data directory for exclusive use by the Agent (or the desktop
// process that hosts it). It creates the directory, migrates the schema and
// runs RecoverInterrupted, which marks a previous process's nonterminal jobs as
// failed. Because of that write, Open must not be used for online management
// while an Agent is running; use OpenReadOnly for that.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, config: domain.DefaultConfig()}
	if err := s.loadConfig(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, historyFile))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	s.db = db
	if err = s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.RecoverInterrupted(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an existing data directory for reading only, which is safe
// while another process (the Agent) holds the same database.
//
// It never migrates the schema and never calls RecoverInterrupted: doing so
// would rewrite live job state from a second process and mark queued or
// printing jobs as failed. The SQLite connection is opened with the driver's
// read-only URI mode and PRAGMA query_only so that even an accidental write
// fails at the database layer, on top of the readOnly guard in this package.
//
// A missing history database yields ErrNoDatabase so callers can fall back to
// an empty view. The data directory itself is never created.
func OpenReadOnly(dir string) (*Store, error) {
	info, err := os.Stat(filepath.Join(dir, historyFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNoDatabase, filepath.Join(dir, historyFile))
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: %s is a directory", ErrNoDatabase, filepath.Join(dir, historyFile))
	}
	s := &Store{dir: dir, config: domain.DefaultConfig(), readOnly: true}
	if err := s.loadConfig(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", readOnlyDSN(filepath.Join(dir, historyFile)))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// Fail fast on an unusable file (for example a non-SQLite or truncated
	// file) so callers get the error here instead of on the first query.
	var one int
	if err := db.QueryRow(`SELECT 1`).Scan(&one); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s read-only: %w", historyFile, err)
	}
	// A zero-byte history file (left behind by an interrupted first start)
	// opens as an empty SQLite database and every query would fail with "no
	// such table". Report it as a missing database so readers degrade instead
	// of surfacing a confusing error. This is a read, never a migration.
	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='job_status'`).Scan(&tables); err != nil {
		db.Close()
		return nil, fmt.Errorf("inspect %s: %w", historyFile, err)
	}
	if tables == 0 {
		db.Close()
		return nil, fmt.Errorf("%w: %s has no schema", ErrNoDatabase, filepath.Join(dir, historyFile))
	}
	s.db = db
	return s, nil
}

// readOnlyDSN builds a SQLite URI that opens path for reading only.
//
// The path must be percent-escaped: a data directory under the user's profile
// can legitimately contain spaces, '%' or '#', and a naive "file:"+path
// concatenation silently points SQLite at a different file (verified: such a
// path opens an empty database and loses every row). The leading slash is
// required so that a Windows drive letter is parsed as a path and not as a URI
// authority.
func readOnlyDSN(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	q := u.Query()
	q.Set("mode", "ro")
	// busy_timeout keeps a reader from failing immediately while the Agent's
	// writer holds a lock; query_only is a second, connection-level barrier.
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Store) loadConfig() error {
	data, err := os.ReadFile(filepath.Join(s.dir, configFile))
	if err == nil {
		if err := json.Unmarshal(data, &s.config); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Normalize before validating: a config edited by hand (or written by an
	// older build) may be missing the mandatory loopback entry, and rejecting it
	// outright would strand the operator with an unreadable config.
	s.config = s.config.Normalize()
	return s.config.Validate()
}

const schemaVersion = 6

func (s *Store) migrate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, schemaVersion)
	}
	if version < 1 {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS job_status (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 job_id TEXT NOT NULL, request_id TEXT NOT NULL, status TEXT NOT NULL,
 message TEXT NOT NULL, at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS job_status_job_id ON job_status(job_id, id DESC);
CREATE TABLE IF NOT EXISTS remote_outbox (
 event_id TEXT PRIMARY KEY, request_id TEXT NOT NULL, job_id TEXT NOT NULL,
 status TEXT NOT NULL, message TEXT NOT NULL, occurred_at TEXT NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0, next_at TEXT NOT NULL
);`); err != nil {
			return err
		}
		version = 1
	}
	if version < 2 {
		if _, err := tx.Exec(`ALTER TABLE remote_outbox ADD COLUMN dead_at TEXT`); err != nil {
			return err
		}
		version = 2
	}
	if version < 3 {
		// error_code carries the classified failure (DOWNLOAD_FAILED, ...) so the
		// CLI and UI can explain a failure without parsing the message text.
		if _, err := tx.Exec(`ALTER TABLE job_status ADD COLUMN error_code TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
		version = 3
	}
	if version < 4 {
		// Remote dedup lives in its own tables so it survives a history prune: a
		// job the remote server already delivered must not be printed again just
		// because its history rows were cleaned up. batch_id on the outbox lets a
		// server correlate a report with the batch it belongs to.
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS remote_jobs (
 job_id TEXT PRIMARY KEY,
 seen_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS remote_batches (
 batch_id TEXT PRIMARY KEY,
 seen_at TEXT NOT NULL
);
ALTER TABLE remote_outbox ADD COLUMN batch_id TEXT NOT NULL DEFAULT '';`); err != nil {
			return err
		}
		version = 4
	}
	if version < 5 {
		// History must be able to answer "which printer/paper/copies, from which
		// source, in which batch, and when did it finish" without consulting the
		// current configuration, which may have changed since.
		for _, col := range []string{
			`ALTER TABLE job_status ADD COLUMN batch_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE job_status ADD COLUMN source TEXT NOT NULL DEFAULT 'web_socket'`,
			`ALTER TABLE job_status ADD COLUMN printer_name TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE job_status ADD COLUMN paper_name TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE job_status ADD COLUMN copies INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE job_status ADD COLUMN finished_at TEXT`,
		} {
			if _, err := tx.Exec(col); err != nil {
				return err
			}
		}
		version = 5
	}
	if version < 6 {
		// last_error records why a status report was finally abandoned, so the
		// dead-letter list can explain the failure instead of showing only a count.
		if _, err := tx.Exec(`ALTER TABLE remote_outbox ADD COLUMN last_error TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
		version = 6
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, version)); err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverInterrupted makes a previous process's nonterminal jobs explicit.
// They are not replayed: printing may have reached the system queue before the crash.
//
// Only Open (the Agent's exclusive-start path) calls this. It is a write and is
// therefore rejected by read-only handles.
func (s *Store) RecoverInterrupted(ctx context.Context) error {
	if s.readOnly {
		return fmt.Errorf("recover interrupted: %w", ErrReadOnly)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_status(job_id,request_id,status,message,at)
SELECT latest.job_id, latest.request_id, 'failed', 'interrupted by previous exit', ?
FROM job_status AS latest
WHERE latest.id = (SELECT MAX(id) FROM job_status WHERE job_id=latest.job_id)
AND latest.status IN ('queued','downloading','printing')`, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}
func (s *Store) Dir() string    { return s.dir }
func (s *Store) ReadOnly() bool { return s.readOnly }
func (s *Store) Config() domain.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.config)
}

func clone(c domain.Config) domain.Config {
	c.Security.AllowedIPs = append([]string(nil), c.Security.AllowedIPs...)
	c.Security.AllowedOrigins = append([]string(nil), c.Security.AllowedOrigins...)
	if c.Printing.DefaultPaper != nil {
		p := *c.Printing.DefaultPaper
		c.Printing.DefaultPaper = &p
	}
	return c
}

func (s *Store) SetConfig(c domain.Config) error {
	if s.readOnly {
		return fmt.Errorf("set config: %w", ErrReadOnly)
	}
	// Normalize first so no path (CLI edit, desktop save, config import) can
	// persist a config that drops the loopback client or allows any address.
	c = c.Normalize()
	if err := c.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(s.dir, configFile)); err != nil {
		return err
	}
	s.config = clone(c)
	return nil
}

func (s *Store) AddStatus(ctx context.Context, st domain.Status) error {
	if s.readOnly {
		return fmt.Errorf("add status: %w", ErrReadOnly)
	}
	if st.At.IsZero() {
		st.At = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_status(job_id,request_id,status,message,error_code,at,batch_id,source,printer_name,paper_name,copies,finished_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		st.JobID, st.RequestID, st.Status, st.Message, st.ErrorCode, st.At.Format(time.RFC3339Nano),
		st.BatchID, statusSource(st), st.PrinterName, st.PaperName, st.Copies, finishedAt(st))
	return err
}

func (s *Store) AddStatuses(ctx context.Context, statuses []domain.Status) error {
	if s.readOnly {
		return fmt.Errorf("add statuses: %w", ErrReadOnly)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, st := range statuses {
		if st.At.IsZero() {
			st.At = time.Now().UTC()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_status(job_id,request_id,status,message,error_code,at,batch_id,source,printer_name,paper_name,copies,finished_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			st.JobID, st.RequestID, st.Status, st.Message, st.ErrorCode, st.At.Format(time.RFC3339Nano),
			st.BatchID, statusSource(st), st.PrinterName, st.PaperName, st.Copies, finishedAt(st)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// statusSource normalizes the recorded source. An unset source is a WebSocket
// job, which is what every caller before this field existed meant.
func statusSource(st domain.Status) string {
	if st.Source == "" {
		return domain.SourceWebSocket
	}
	return st.Source
}

// finishedAt records when a job reached a terminal state, and stays NULL while it
// is still running. A later terminal status overwrites it, matching the
// rule that the last terminal transition wins.
func finishedAt(st domain.Status) any {
	if !domain.IsTerminalStatus(st.Status) {
		return nil
	}
	return st.At.UTC().Format(time.RFC3339Nano)
}

// statusColumns is the projection every status read shares, so a new history
// column is added in exactly one place.
const statusColumns = `job_id,request_id,status,message,error_code,at,batch_id,source,printer_name,paper_name,copies`

// scanStatus fills a Status from a row projected with statusColumns.
func scanStatus(scan func(...any) error) (domain.Status, error) {
	var st domain.Status
	var raw string
	if err := scan(&st.JobID, &st.RequestID, &st.Status, &st.Message, &st.ErrorCode, &raw,
		&st.BatchID, &st.Source, &st.PrinterName, &st.PaperName, &st.Copies); err != nil {
		return st, err
	}
	st.Type = "job_status"
	st.At, _ = time.Parse(time.RFC3339Nano, raw)
	return st, nil
}

func (s *Store) Latest(ctx context.Context, jobID string) (domain.Status, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+statusColumns+` FROM job_status WHERE job_id=? ORDER BY id DESC LIMIT 1`, jobID)
	return scanStatus(row.Scan)
}

func (s *Store) Recent(ctx context.Context, limit int) ([]domain.Status, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+statusColumns+` FROM job_status ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Status{}
	for rows.Next() {
		st, err := scanStatus(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// JobSummary is one aggregated row per job for list views.
type JobSummary struct {
	JobID        string    `json:"job_id"`
	LatestStatus string    `json:"latest_status"`
	Message      string    `json:"message"`
	UpdatedAt    time.Time `json:"updated_at"`
	Events       int       `json:"events"`
	// BatchID, Source, PrinterName, PaperName and Copies come from the latest
	// status row, so a list view can answer "which printer/paper/copies, from
	// which source, in which batch" without a second query per row.
	BatchID     string    `json:"batch_id,omitempty"`
	Source      string    `json:"source,omitempty"`
	PrinterName string    `json:"printer_name,omitempty"`
	PaperName   string    `json:"paper_name,omitempty"`
	Copies      int       `json:"copies,omitempty"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
}

// ListJobs returns one row per job, ordered by newest status first. limit and
// offset page the result; status filters on the latest status; query matches a
// job_id substring. A non-positive limit means the default (100) and an
// over-large limit is capped, so a caller cannot accidentally load everything.
func (s *Store) ListJobs(ctx context.Context, limit, offset int, status, query string) ([]JobSummary, error) {
	switch {
	case limit <= 0:
		limit = 100
	case limit > 1000:
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	filter, filterArgs := "", []any{}
	if status != "" {
		filter += ` AND latest.status=?`
		filterArgs = append(filterArgs, status)
	}
	if query != "" {
		filter += ` AND latest.job_id LIKE ? ESCAPE '\'`
		filterArgs = append(filterArgs, "%"+escapeLike(query)+"%")
	}
	// The subquery pins one row per job_id (its highest id) together with the
	// event count, so the summary cannot drift if a status is inserted between
	// reads. Ties on the timestamp break on id to keep paging deterministic.
	stmt := `
SELECT latest.job_id, latest.status, latest.message, latest.at, agg.events,
       latest.batch_id, latest.source, latest.printer_name, latest.paper_name, latest.copies,
       COALESCE(latest.finished_at, '')
FROM job_status AS latest
JOIN (SELECT job_id, COUNT(*) AS events, MAX(id) AS max_id FROM job_status GROUP BY job_id) AS agg
  ON latest.id = agg.max_id
WHERE 1=1` + filter + `
ORDER BY julianday(latest.at) DESC, latest.id DESC
LIMIT ? OFFSET ?`
	args := append(filterArgs, limit, offset)
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JobSummary{}
	for rows.Next() {
		var j JobSummary
		var raw, finished string
		if err := rows.Scan(&j.JobID, &j.LatestStatus, &j.Message, &raw, &j.Events,
			&j.BatchID, &j.Source, &j.PrinterName, &j.PaperName, &j.Copies, &finished); err != nil {
			return nil, err
		}
		j.UpdatedAt, _ = time.Parse(time.RFC3339Nano, raw)
		if finished != "" {
			j.FinishedAt, _ = time.Parse(time.RFC3339Nano, finished)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobHistory returns one job's full timeline in ascending id order (oldest
// first). An unknown job yields an empty slice, not an error.
func (s *Store) JobHistory(ctx context.Context, jobID string) ([]domain.Status, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+statusColumns+` FROM job_status WHERE job_id=? ORDER BY id ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Status{}
	for rows.Next() {
		st, err := scanStatus(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// escapeLike neutralises LIKE wildcards so a user-supplied query is matched
// literally. The caller must pair it with ESCAPE '\'.
func escapeLike(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '%' || r == '_' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// PruneHistory deletes history older than retentionDays and returns the number
// of deleted rows (job status rows plus dead remote outbox rows).
//
// The newest row of every job is always kept, even when it is older than the
// window: Latest() and Recovery depend on it, and dropping it would lose a
// job's terminal state. A non-positive retentionDays falls back to the
// configured Remote.HistoryRetentionDays. Because the Agent owns the database,
// this method is rejected on read-only handles; the CLI prunes via IPC.
func (s *Store) PruneHistory(ctx context.Context, retentionDays int) (int64, error) {
	if s.readOnly {
		return 0, fmt.Errorf("prune history: %w", ErrReadOnly)
	}
	if retentionDays <= 0 {
		retentionDays = s.Config().Remote.HistoryRetentionDays
	}
	if retentionDays <= 0 {
		// Never treat a missing retention setting as "delete everything".
		return 0, fmt.Errorf("prune history: refusal: retention window is %d day(s)", retentionDays)
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.RFC3339Nano)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Rows with an unparseable `at` are excluded by the julianday() IS NOT NULL
	// guard instead of being silently deleted.
	statusRes, err := tx.ExecContext(ctx, `DELETE FROM job_status
WHERE julianday(at) IS NOT NULL
  AND julianday(at) < julianday(?)
  AND id NOT IN (SELECT MAX(id) FROM job_status GROUP BY job_id)`, cutoff)
	if err != nil {
		return 0, err
	}
	statusDeleted, err := statusRes.RowsAffected()
	if err != nil {
		return 0, err
	}

	outboxRes, err := tx.ExecContext(ctx, `DELETE FROM remote_outbox
WHERE dead_at IS NOT NULL
  AND julianday(dead_at) IS NOT NULL
  AND julianday(dead_at) < julianday(?)`, cutoff)
	if err != nil {
		return 0, err
	}
	outboxDeleted, err := outboxRes.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return statusDeleted + outboxDeleted, nil
}

// RecordRemoteJob marks a remote job id as seen and reports whether this call was
// the first to see it.
//
// Remote dedup is intentionally independent of job_status: pruning history must
// not make an already-delivered remote job eligible for printing again.
func (s *Store) RecordRemoteJob(ctx context.Context, jobID string) (bool, error) {
	if s.readOnly {
		return false, fmt.Errorf("record remote job: %w", ErrReadOnly)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO remote_jobs (job_id, seen_at) VALUES (?, ?)`,
		jobID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// RecordRemoteBatch marks a remote batch id as seen and reports whether this call
// was the first to see it. Recording the batch is what makes a re-delivered batch
// a no-op instead of a partial re-print.
func (s *Store) RecordRemoteBatch(ctx context.Context, batchID string) (bool, error) {
	if s.readOnly {
		return false, fmt.Errorf("record remote batch: %w", ErrReadOnly)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO remote_batches (batch_id, seen_at) VALUES (?, ?)`,
		batchID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ClearHistory removes every terminal job record and its status events, plus
// dead-letter remote events. Jobs whose latest status is still non-terminal are
// kept: the queue may be about to write their next state, and deleting the row
// would leave that write with no history to append to.
//
// Returns the number of deleted rows.
func (s *Store) ClearHistory(ctx context.Context) (int64, error) {
	if s.readOnly {
		return 0, fmt.Errorf("clear history: %w", ErrReadOnly)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// A job is terminal when its most recent status is one of the five terminal
	// states. Anything else (queued/downloading/printing) is in flight.
	const terminalLatest = `SELECT job_id FROM job_status AS outer_status
 WHERE outer_status.id = (SELECT MAX(id) FROM job_status WHERE job_id = outer_status.job_id)
   AND outer_status.status NOT IN ('queued','downloading','printing')`

	statusRes, err := tx.ExecContext(ctx,
		`DELETE FROM job_status WHERE job_id IN (`+terminalLatest+`)`)
	if err != nil {
		return 0, err
	}
	deleted, err := statusRes.RowsAffected()
	if err != nil {
		return 0, err
	}

	// Delivered and dead-letter events are safe to drop; pending ones are the
	// outbox backlog and must survive a history clear.
	outboxRes, err := tx.ExecContext(ctx,
		`DELETE FROM remote_outbox WHERE attempts > 0 OR dead_at IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	outboxDeleted, err := outboxRes.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted + outboxDeleted, nil
}

type RemoteEvent struct {
	EventID, RequestID, JobID, BatchID, Status, Message, OccurredAt string
	Attempts                                                        int
}

func (s *Store) AddRemoteEvent(ctx context.Context, e RemoteEvent) error {
	if s.readOnly {
		return fmt.Errorf("add remote event: %w", ErrReadOnly)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO remote_outbox(event_id,request_id,job_id,status,message,occurred_at,next_at,batch_id) VALUES(?,?,?,?,?,?,?,?)`, e.EventID, e.RequestID, e.JobID, e.Status, e.Message, e.OccurredAt, time.Now().UTC().Format(time.RFC3339Nano), e.BatchID)
	return err
}
func (s *Store) DueRemoteEvents(ctx context.Context, limit int) ([]RemoteEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event_id,request_id,job_id,status,message,occurred_at,attempts,COALESCE(batch_id,'') FROM remote_outbox WHERE dead_at IS NULL AND next_at<=? ORDER BY next_at LIMIT ?`, time.Now().UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RemoteEvent{}
	for rows.Next() {
		var e RemoteEvent
		if err := rows.Scan(&e.EventID, &e.RequestID, &e.JobID, &e.Status, &e.Message, &e.OccurredAt, &e.Attempts, &e.BatchID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) AckRemoteEvent(ctx context.Context, id string) error {
	if s.readOnly {
		return fmt.Errorf("ack remote event: %w", ErrReadOnly)
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM remote_outbox WHERE event_id=?`, id)
	return err
}
func (s *Store) RetryRemoteEvent(ctx context.Context, id string, attempts int, next time.Time) error {
	if s.readOnly {
		return fmt.Errorf("retry remote event: %w", ErrReadOnly)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE remote_outbox SET attempts=?,next_at=? WHERE event_id=?`, attempts, next.UTC().Format(time.RFC3339Nano), id)
	return err
}

// FailRemoteEvent abandons one status report after its retries are exhausted and
// records why, so the dead-letter list can explain the failure.
func (s *Store) FailRemoteEvent(ctx context.Context, id string, attempts int, lastError string) error {
	if s.readOnly {
		return fmt.Errorf("fail remote event: %w", ErrReadOnly)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE remote_outbox SET attempts=?,dead_at=?,last_error=? WHERE event_id=?`,
		attempts, time.Now().UTC().Format(time.RFC3339Nano), lastError, id)
	return err
}

// DeadLetter is one status report that exhausted its retries. It is retained so an
// operator can see what never reached the server and choose to retry or discard it.
type DeadLetter struct {
	RemoteEvent
	// DeadAt is when the event was abandoned, and LastError is the failure that
	// caused the final attempt to be counted.
	DeadAt    time.Time `json:"dead_at"`
	LastError string    `json:"last_error"`
}

// RemoteOutboxStats reports the outbox backlog. Pending counts events still being
// retried; Dead counts events that exhausted their retries and need a decision.
type RemoteOutboxStats struct {
	Pending int `json:"pending"`
	Dead    int `json:"dead"`
}

// RemoteStats summarizes the remote outbox for the UI and CLI.
func (s *Store) RemoteStats(ctx context.Context) (RemoteOutboxStats, error) {
	var st RemoteOutboxStats
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM remote_outbox WHERE dead_at IS NULL`).Scan(&st.Pending); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM remote_outbox WHERE dead_at IS NOT NULL`).Scan(&st.Dead); err != nil {
		return st, err
	}
	return st, nil
}

// ListDeadLetters returns the dead-letter events, newest first.
func (s *Store) ListDeadLetters(ctx context.Context, limit int) ([]DeadLetter, error) {
	switch {
	case limit <= 0:
		limit = 100
	case limit > 1000:
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id,request_id,job_id,status,message,occurred_at,attempts,
       COALESCE(batch_id,''), COALESCE(dead_at,''), COALESCE(last_error,'')
FROM remote_outbox WHERE dead_at IS NOT NULL ORDER BY julianday(dead_at) DESC, event_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeadLetter{}
	for rows.Next() {
		var d DeadLetter
		var deadAt string
		if err := rows.Scan(&d.EventID, &d.RequestID, &d.JobID, &d.Status, &d.Message, &d.OccurredAt,
			&d.Attempts, &d.BatchID, &deadAt, &d.LastError); err != nil {
			return nil, err
		}
		d.DeadAt, _ = time.Parse(time.RFC3339Nano, deadAt)
		out = append(out, d)
	}
	return out, rows.Err()
}

// RetryDeadLetters puts dead-letter events back in the queue with a cleared attempt
// count, so the next report pass tries them again. An empty ids slice retries every
// dead letter. Returns the number of events revived.
func (s *Store) RetryDeadLetters(ctx context.Context, ids []string) (int64, error) {
	if s.readOnly {
		return 0, fmt.Errorf("retry dead letters: %w", ErrReadOnly)
	}
	query := `UPDATE remote_outbox SET attempts=0, dead_at=NULL, next_at=? WHERE dead_at IS NOT NULL`
	args := []any{time.Now().UTC().Format(time.RFC3339Nano)}
	if len(ids) > 0 {
		query += ` AND event_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteDeadLetters discards dead-letter events. An empty ids slice discards every
// dead letter. Returns the number of events removed.
func (s *Store) DeleteDeadLetters(ctx context.Context, ids []string) (int64, error) {
	if s.readOnly {
		return 0, fmt.Errorf("delete dead letters: %w", ErrReadOnly)
	}
	query := `DELETE FROM remote_outbox WHERE dead_at IS NOT NULL`
	args := []any{}
	if len(ids) > 0 {
		query += ` AND event_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// placeholders builds "?,?,?" for an IN clause with n values.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}
