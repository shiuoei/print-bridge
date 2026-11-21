package command

import (
	"context"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/store"
)

// History export.
//
// The bytes are produced here rather than in the CLI so the desktop UI and the CLI
// export identical content: both call the same command, and the CLI only writes the
// returned bytes to a path. Generating it in two places would let the two formats
// drift.

// exportHistory serializes the job history according to the request.
func (s Service) exportHistory(ctx context.Context, req Request, r *Response) error {
	format, err := store.ParseExportFormat(req.Format)
	if err != nil {
		return domain.NewProtocolError(domain.CodeArgumentInvalid, err.Error())
	}
	opts := store.ExportOptions{
		Format: format,
		Status: req.Status,
		Query:  req.Query,
		Limit:  req.Limit,
	}
	// Days reuses the retention-window field: 0 exports everything retained.
	if req.Days > 0 {
		opts.Since = time.Now().UTC().AddDate(0, 0, -req.Days)
	}
	rows, count, err := s.Store.ExportHistory(ctx, opts)
	if err != nil {
		return err
	}
	r.Rows = rows
	r.RowsCount = count
	r.Value = string(format)
	return nil
}
