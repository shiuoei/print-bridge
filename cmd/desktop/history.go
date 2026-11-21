package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/store"
)

// History page state lives on the desktop struct; see historyState.

// statusOptions is the filter list: the empty entry means "any status".
func (d *desktop) statusOptions() []string {
	return []string{
		d.tr("filter_any"),
		d.tr("status_queued"),
		d.tr("status_downloading"),
		d.tr("status_printing"),
		d.tr("status_submitted"),
		d.tr("status_completed"),
		d.tr("status_failed"),
		d.tr("status_unknown"),
		d.tr("status_cancelled"),
	}
}

// statusFromLabel maps the localized filter label back to a wire status.
func (d *desktop) statusFromLabel(label string) string {
	switch label {
	case d.tr("status_queued"):
		return "queued"
	case d.tr("status_downloading"):
		return "downloading"
	case d.tr("status_printing"):
		return "printing"
	case d.tr("status_submitted"):
		return "submitted"
	case d.tr("status_completed"):
		return "completed"
	case d.tr("status_failed"):
		return "failed"
	case d.tr("status_unknown"):
		return "unknown"
	case d.tr("status_cancelled"):
		return "cancelled"
	}
	return ""
}

// refreshHistory reloads the aggregated job list with the current filter.
func (d *desktop) refreshHistory() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status := d.statusFromLabel(d.historyFilter)
	r := d.service.Execute(ctx, command.Request{
		Command: "task.list",
		Limit:   200,
		Status:  status,
		Query:   d.historyQuery,
	})
	if r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	d.historyJobs = r.Jobs
	// Keep the selection inside the new result set; a filter can remove the
	// previously chosen row.
	if d.historyRow >= len(d.historyJobs) {
		d.historyRow = len(d.historyJobs) - 1
	}
	if d.historyRow < 0 {
		d.historyRow = 0
	}
	d.historyTimeline = nil
	d.historySelected = ""
}

// loadTimeline fetches one job's status events for the detail pane.
func (d *desktop) loadTimeline(jobID string) {
	if jobID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := d.service.Execute(ctx, command.Request{Command: "task.show", JobID: jobID})
	if r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	d.historyTimeline = r.History
	d.historySelected = jobID
}

// historyPage renders the task history: filter row, sortable table, and the
// selected job's timeline.
func (d *desktop) historyPage(c *ui.Context) {
	if d.historyFilter == "" {
		d.historyFilter = d.tr("filter_any")
	}
	panel(c, d.tr("recent_jobs"), d.tr("recent_jobs_detail"), func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			fieldLabel(c, d.tr("filter_status"))
			ui.Select(c, &d.historyFilter, d.statusOptions()).Width(150)
			fieldLabel(c, d.tr("filter_query"))
			ui.TextInput(c, &d.historyQuery).Width(200).Placeholder(d.tr("filter_query_placeholder"))
			if ui.Button(c, d.tr("apply_filter")).Clicked() {
				d.refreshHistory()
			}
			ui.Spacer(c)
			if ui.Button(c, d.tr("refresh_history")).Clicked() {
				d.refreshHistory()
			}
			if ui.Button(c, d.tr("export_history")).Clicked() {
				d.openModal(modalExportHistory)
			}
			clearLabel := d.tr("clear_history")
			if ui.Button(c, clearLabel).Clicked() {
				d.openModal(modalClearHistory)
			}
		})
		ui.Text(c, fmt.Sprintf(d.tr("jobs_count"), len(d.historyJobs))).FontSize(12).TextColor(palette.muted)
		if len(d.historyJobs) == 0 {
			// Still render the table header so the columns are discoverable before
			// the first job arrives; an empty table body alone would look broken.
			columns := d.historyColumns()
			ui.Table(c, &d.historyTable, columns, 0, func(int, int) {}).Height(60)
			ui.Text(c, d.tr("no_jobs")).TextColor(palette.muted)
			return
		}
		columns := d.historyColumns()
		jobs := d.historyJobs
		// The column widths fit the panel, so no horizontal scroll is needed; the
		// table's own vertical scrolling bounds the height and keeps the detail
		// pane visible below it.
		table := ui.Table(c, &d.historyTable, columns, len(jobs), func(row, col int) {
			job := jobs[row]
			switch col {
			case 0:
				ui.Text(c, job.UpdatedAt.Local().Format("01-02 15:04:05")).SingleLine()
			case 1:
				badge(c, d.statusLabel(job.LatestStatus))
			case 2:
				ui.Text(c, d.sourceLabel(job.Source)).SingleLine()
			case 3:
				ui.Text(c, job.JobID).SingleLine()
			case 4:
				ui.Text(c, job.PrinterName).SingleLine()
			case 5:
				ui.Text(c, job.PaperName).SingleLine()
			case 6:
				ui.Text(c, copiesLabel(job.Copies)).SingleLine()
			case 7:
				ui.Text(c, job.Message).SingleLine()
			}
		}).Height(200)
		if table.Submitted() && d.historyTable.Selected != nil {
			row := *d.historyTable.Selected
			if row >= 0 && row < len(jobs) {
				d.historyRow = row
				d.loadTimeline(jobs[row].JobID)
			}
		}
	})
	d.historyDetail(c)
}

// historyColumns is the table schema, shared by the populated and empty states so
// the header does not change shape when the first job arrives.
//
// The widths fit the default 880 px window: the six decision-relevant columns stay
// readable and the message is the flexible last column. Keeping the fixed widths
// inside the panel avoids forcing a horizontal scroll on first open, which would
// hide the right-hand columns behind a scrollbar the operator does not expect.
func (d *desktop) historyColumns() []ui.TableColumn {
	return []ui.TableColumn{
		{Title: d.tr("col_time"), Width: 118, Sortable: true},
		{Title: d.tr("col_status"), Width: 92, Sortable: true},
		{Title: d.tr("col_source"), Width: 92, Sortable: true},
		{Title: d.tr("col_job"), Width: 132, Sortable: true},
		{Title: d.tr("col_printer"), Width: 120},
		{Title: d.tr("col_paper"), Width: 88},
		{Title: d.tr("col_copies"), Width: 66, Align: ui.End},
		{Title: d.tr("col_message")},
	}
}

// historyDetail renders the selected job's metadata and status timeline.
func (d *desktop) historyDetail(c *ui.Context) {
	panel(c, d.tr("task_details"), d.tr("task_details_detail"), func() {
		if d.historySelected == "" {
			ui.Text(c, d.tr("select_task_hint")).TextColor(palette.muted)
			return
		}
		var job *store.JobSummary
		for i := range d.historyJobs {
			if d.historyJobs[i].JobID == d.historySelected {
				job = &d.historyJobs[i]
				break
			}
		}
		if job != nil {
			summaryRow(c, d.tr("col_job"), job.JobID)
			summaryRow(c, d.tr("col_status"), d.statusLabel(job.LatestStatus))
			summaryRow(c, d.tr("col_source"), d.sourceLabel(job.Source))
			if job.BatchID != "" {
				summaryRow(c, d.tr("col_batch"), job.BatchID)
			}
			summaryRow(c, d.tr("col_printer"), job.PrinterName)
			summaryRow(c, d.tr("col_paper"), job.PaperName)
			summaryRow(c, d.tr("col_copies"), copiesLabel(job.Copies))
			if !job.FinishedAt.IsZero() {
				summaryRow(c, d.tr("col_finished"), job.FinishedAt.Local().Format(time.RFC3339))
			}
		}
		if len(d.historyTimeline) == 0 {
			ui.Text(c, d.tr("no_status_records")).FontSize(12).TextColor(palette.muted)
			return
		}
		ui.Scroll(c).Height(200).Children(func() {
			ui.Column(c).FillWidth().Gap(6).Children(func() {
				for _, st := range d.historyTimeline {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						badge(c, d.statusLabel(st.Status))
						ui.Text(c, st.At.Local().Format("15:04:05")).FontSize(12).TextColor(palette.muted)
						ui.Text(c, st.Message).FontSize(12).TextColor(palette.ink).SingleLine()
					})
				}
			})
		})
	})
}

// clearHistoryModal renders the two-stage confirm for `task clear`.
func (d *desktop) clearHistoryModal(c *ui.Context) {
	if d.modal.kind != modalClearHistory {
		return
	}
	open := d.modal.open
	ui.DialogBase(c, &open, func(_, _ ui.Element) {
		ui.Column(c).Gap(10).Padding(16).Width(380).Children(func() {
			ui.Text(c, d.tr("clear_history_title")).FontSize(16).Bold().TextColor(palette.ink)
			ui.Text(c, d.tr("clear_history_body")).FontSize(13).TextColor(palette.muted)
			ui.Row(c).Gap(8).Children(func() {
				if ui.Button(c, d.tr("cancel")).Clicked() {
					d.closeModal()
				}
				if ui.PrimaryButton(c, d.tr("clear_history_confirm")).Clicked() {
					d.runClearHistory()
				}
			})
		})
	})
	if !open {
		d.closeModal()
	}
}

// runClearHistory executes `task clear` and refreshes the list.
func (d *desktop) runClearHistory() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := d.service.Execute(ctx, command.Request{Command: "task.clear"})
	d.closeModal()
	if r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	d.message = fmt.Sprintf(d.tr("history_cleared"), r.Removed)
	d.errorDetail = ""
	d.refreshHistory()
}

// statusLabel localizes a wire status, falling back to the upper-case token so an
// unknown status is still visible rather than blank.
func (d *desktop) statusLabel(status string) string {
	if key := "status_" + status; d.tr(key) != key {
		return d.tr(key)
	}
	return status
}

// sourceLabel localizes a history source token.
func (d *desktop) sourceLabel(source string) string {
	if source == "" {
		return ""
	}
	key := "source_" + source
	if d.tr(key) != key {
		return d.tr(key)
	}
	return source
}

// copiesLabel renders the copies column; raw jobs carry none.
func copiesLabel(copies int) string {
	if copies <= 0 {
		return "-"
	}
	return strconv.Itoa(copies)
}

var _ = domain.SourceWebSocket
