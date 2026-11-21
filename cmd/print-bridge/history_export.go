package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
)

// runHistoryExport asks the running agent for the exported bytes and writes them to
// the requested path.
//
// The content is produced by the command service so the CLI and the desktop export
// identical files; only the destination differs. The file is written through a
// temporary file and renamed, so an interrupted export never leaves a truncated
// file that looks complete.
func runHistoryExport(ctx context.Context, dir string, req command.Request, path string) error {
	resp, err := ipcCall(ctx, dir, req)
	if err != nil {
		if !agentAbsent(err) {
			return domain.NewProtocolError(domain.CodeInternalError, "agent IPC failed").WithCause(err)
		}
		// Exporting needs the agent: it owns the database. Reading the file
		// offline would race the agent's writer.
		return domain.NewProtocolError(domain.CodeAgentNotRunning,
			"agent is not running; start PrintBridge first").WithCause(err)
	}
	if resp.Error != "" {
		return domain.NewProtocolError(domain.CodeOf(nil), resp.Error)
	}
	if len(resp.Rows) == 0 {
		return domain.NewProtocolError(domain.CodeInternalError, "agent returned an empty export")
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".printbridge-export-*")
	if err != nil {
		return domain.NewProtocolError(domain.CodePermission, "cannot create the export file").WithCause(err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(resp.Rows); err != nil {
		tmp.Close()
		cleanup()
		return domain.NewProtocolError(domain.CodePermission, "cannot write the export file").WithCause(err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return domain.NewProtocolError(domain.CodePermission, "cannot write the export file").WithCause(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return domain.NewProtocolError(domain.CodePermission, "cannot finalize the export file").WithCause(err)
	}
	fmt.Printf("exported %d job(s) as %s to %s\n", resp.RowsCount, resp.Value, path)
	return nil
}
