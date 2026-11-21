package store

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// History export.
//
// The export is generated from the same aggregated rows the UI lists, so an
// operator who filtered the table and then exported gets exactly what they saw.
// CSV is the default because it opens in a spreadsheet; JSON is offered for a
// scripted consumer. Both carry the same columns.

// ExportFormat selects the serialization of a history export.
type ExportFormat string

const (
	ExportCSV  ExportFormat = "csv"
	ExportJSON ExportFormat = "json"
)

// ParseExportFormat validates a user-supplied format name.
func ParseExportFormat(raw string) (ExportFormat, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(ExportCSV):
		return ExportCSV, nil
	case string(ExportJSON):
		return ExportJSON, nil
	}
	return "", fmt.Errorf("unsupported export format %q: use csv or json", raw)
}

// ExportOptions narrows an export. Status and Query mirror ListJobs; Since
// restricts to jobs whose latest update is at or after that instant.
type ExportOptions struct {
	Format ExportFormat
	Status string
	Query  string
	// Since is inclusive; a zero value exports the whole retained history.
	Since time.Time
	// Limit bounds the row count; zero means the export cap.
	Limit int
}

// maxExportRows bounds one export so a large history cannot produce an
// unbounded file from a single click.
const maxExportRows = 50000

// ExportHistory serializes the aggregated job history.
//
// The returned bytes are the complete file content, so the caller decides where
// it goes (CLI path or desktop save dialog) and the same bytes can be asserted in
// tests.
func (s *Store) ExportHistory(ctx context.Context, opts ExportOptions) ([]byte, int, error) {
	format := opts.Format
	if format == "" {
		format = ExportCSV
	}
	limit := opts.Limit
	switch {
	case limit <= 0:
		limit = maxExportRows
	case limit > maxExportRows:
		limit = maxExportRows
	}

	jobs, err := s.exportRows(ctx, opts, limit)
	if err != nil {
		return nil, 0, err
	}
	switch format {
	case ExportJSON:
		b, err := json.MarshalIndent(jobs, "", "  ")
		if err != nil {
			return nil, 0, err
		}
		return append(b, '\n'), len(jobs), nil
	default:
		b, err := exportCSV(jobs)
		if err != nil {
			return nil, 0, err
		}
		return b, len(jobs), nil
	}
}

// exportRows reads the rows an export covers, newest first.
func (s *Store) exportRows(ctx context.Context, opts ExportOptions, limit int) ([]JobSummary, error) {
	filter, args := "", []any{}
	if opts.Status != "" {
		filter += ` AND latest.status=?`
		args = append(args, opts.Status)
	}
	if opts.Query != "" {
		filter += ` AND latest.job_id LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(opts.Query)+"%")
	}
	if !opts.Since.IsZero() {
		filter += ` AND julianday(latest.at) >= julianday(?)`
		args = append(args, opts.Since.UTC().Format(time.RFC3339Nano))
	}
	stmt := `
SELECT latest.job_id, latest.status, latest.message, latest.at, agg.events,
       latest.batch_id, latest.source, latest.printer_name, latest.paper_name, latest.copies,
       COALESCE(latest.finished_at, '')
FROM job_status AS latest
JOIN (SELECT job_id, COUNT(*) AS events, MAX(id) AS max_id FROM job_status GROUP BY job_id) AS agg
  ON latest.id = agg.max_id
WHERE 1=1` + filter + `
ORDER BY julianday(latest.at) DESC, latest.id DESC
LIMIT ?`
	args = append(args, limit)
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

// exportHeader is the CSV column order. It matches the history table so a
// spreadsheet lines up with what the operator saw on screen.
var exportHeader = []string{
	"job_id", "status", "source", "batch_id", "printer", "paper", "copies",
	"events", "updated_at", "finished_at", "message",
}

// exportCSV renders the rows as RFC 4180 CSV.
func exportCSV(jobs []JobSummary) ([]byte, error) {
	var buf bytes.Buffer
	// A UTF-8 BOM makes Excel open the file as UTF-8 instead of mangling non-ASCII
	// printer and paper names.
	buf.WriteString("\ufeff")
	w := csv.NewWriter(&buf)
	if err := w.Write(exportHeader); err != nil {
		return nil, err
	}
	for _, j := range jobs {
		record := []string{
			j.JobID,
			j.LatestStatus,
			j.Source,
			j.BatchID,
			j.PrinterName,
			j.PaperName,
			copiesCell(j.Copies),
			strconv.Itoa(j.Events),
			formatTime(j.UpdatedAt),
			formatTime(j.FinishedAt),
			j.Message,
		}
		if err := w.Write(record); err != nil {
			return nil, err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// copiesCell renders copies, leaving the cell empty for a raw job that carries none.
func copiesCell(copies int) string {
	if copies <= 0 {
		return ""
	}
	return strconv.Itoa(copies)
}

// formatTime renders a timestamp in UTC RFC 3339, or empty when unset.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
