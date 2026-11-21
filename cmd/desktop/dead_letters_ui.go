package main

import (
	"context"
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/store"
)

// Dead-letter panel for the remote tab.
//
// A status report that exhausts its retries is kept, not dropped: the operator has
// to know the business server never learned a job finished, and decide whether to
// retry it (server fixed) or discard it (the event is obsolete). The panel shows
// the backlog counts, the abandoned rows with the reason each failed, and the two
// actions.

// deadLettersPanel renders the backlog summary and the abandoned reports.
func (d *desktop) deadLettersPanel(c *ui.Context) {
	panel(c, d.tr("dead_letters"), d.tr("dead_letters_detail"), func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.Button(c, d.tr("refresh_dead_letters")).Clicked() {
				d.refreshRemoteBacklog()
			}
			ui.Spacer(c)
			if d.remoteStats != nil {
				ui.Text(c, fmt.Sprintf(d.tr("outbox_pending"), d.remoteStats.Pending)).
					FontSize(12).TextColor(palette.muted)
				ui.Text(c, fmt.Sprintf(d.tr("outbox_dead"), d.remoteStats.Dead)).
					FontSize(12).Bold().TextColor(deadLetterTone(d.remoteStats.Dead))
			}
		})
		if len(d.deadLetters) == 0 {
			ui.Text(c, d.tr("no_dead_letters")).FontSize(12).TextColor(palette.muted)
			return
		}
		ui.Row(c).Gap(8).Children(func() {
			if ui.Button(c, d.tr("retry_all_dead_letters")).Clicked() {
				d.retryDeadLetters(nil)
			}
			if ui.Button(c, d.tr("purge_dead_letters")).Clicked() {
				d.openModal(modalPurgeDeadLetters)
			}
		})
		for _, letter := range d.deadLetters {
			entry := letter
			ui.Column(c).FillWidth().Padding(8, 12).Gap(3).Radius(7).Background(palette.canvas).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					badge(c, entry.Status)
					ui.Text(c, "job "+entry.JobID).FontSize(12).Bold().TextColor(palette.ink).SingleLine()
					ui.Spacer(c)
					ui.Text(c, entry.DeadAt.Local().Format("01-02 15:04:05")).FontSize(11).TextColor(palette.muted)
				})
				// The reason is what makes the row actionable; without it the
				// operator cannot tell a server outage from a rejected payload.
				if entry.LastError != "" {
					ui.Text(c, d.tr("last_error")+": "+entry.LastError).FontSize(11).TextColor(palette.warn).SingleLine()
				}
				ui.Text(c, fmt.Sprintf(d.tr("dead_letter_attempts"), entry.Attempts)).FontSize(11).TextColor(palette.muted)
				ui.Row(c).Gap(8).Children(func() {
					if ui.Button(c, d.tr("retry_one")).Clicked() {
						d.retryDeadLetters([]string{entry.EventID})
					}
					if ui.Button(c, d.tr("delete_one")).Clicked() {
						d.deleteDeadLetters([]string{entry.EventID})
					}
				})
			})
		}
	})
}

// deadLetterTone highlights the dead count when it is non-zero.
func deadLetterTone(dead int) ui.Color {
	if dead > 0 {
		return palette.warn
	}
	return palette.muted
}

// refreshRemoteBacklog reloads the outbox counts and the dead-letter list.
func (d *desktop) refreshRemoteBacklog() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stats := d.service.Execute(ctx, command.Request{Command: "remote.stats"})
	if stats.Error != "" {
		d.reportError(stats.ErrorCode, stats.Error)
		return
	}
	d.remoteStats = stats.RemoteStats
	list := d.service.Execute(ctx, command.Request{Command: "remote.dead-letters", Limit: 100})
	if list.Error != "" {
		d.reportError(list.ErrorCode, list.Error)
		return
	}
	d.deadLetters = list.DeadLetters
}

// retryDeadLetters revives the selected reports, or all of them when ids is empty.
func (d *desktop) retryDeadLetters(ids []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := command.Request{Command: "remote.retry-dead-letters"}
	if len(ids) > 0 {
		req.EventIDs = ids
	} else {
		// "Retry all" needs the explicit all-form; the service refuses an empty
		// selection for delete but retry-all is a supported operation, so it is
		// expressed by passing every current id.
		for _, letter := range d.deadLetters {
			req.EventIDs = append(req.EventIDs, letter.EventID)
		}
		if len(req.EventIDs) == 0 {
			return
		}
	}
	r := d.service.Execute(ctx, req)
	if r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	d.message = fmt.Sprintf(d.tr("dead_letters_requeued"), r.Removed)
	d.errorDetail = ""
	d.refreshRemoteBacklog()
}

// deleteDeadLetters discards the selected reports.
func (d *desktop) deleteDeadLetters(ids []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := d.service.Execute(ctx, command.Request{Command: "remote.delete-dead-letters", EventIDs: ids})
	if r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	d.message = fmt.Sprintf(d.tr("dead_letters_deleted"), r.Removed)
	d.errorDetail = ""
	d.refreshRemoteBacklog()
}

// purgeDeadLetters discards every abandoned report, after confirmation.
func (d *desktop) purgeDeadLetters() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := d.service.Execute(ctx, command.Request{Command: "remote.purge-dead-letters"})
	d.closeModal()
	if r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	d.message = fmt.Sprintf(d.tr("dead_letters_deleted"), r.Removed)
	d.errorDetail = ""
	d.refreshRemoteBacklog()
}

// purgeDeadLettersModal is the confirmation for discarding every dead letter.
func (d *desktop) purgeDeadLettersModal(c *ui.Context) {
	if d.modal.kind != modalPurgeDeadLetters {
		return
	}
	open := d.modal.open
	ui.DialogBase(c, &open, func(_, _ ui.Element) {
		ui.Column(c).Gap(10).Padding(16).Width(400).Children(func() {
			ui.Text(c, d.tr("purge_dead_letters_title")).FontSize(16).Bold().TextColor(palette.ink)
			ui.Text(c, d.tr("purge_dead_letters_body")).FontSize(13).TextColor(palette.muted)
			ui.Row(c).Gap(8).Children(func() {
				if ui.Button(c, d.tr("cancel")).Clicked() {
					d.closeModal()
				}
				if ui.PrimaryButton(c, d.tr("purge_dead_letters_confirm")).Clicked() {
					d.purgeDeadLetters()
				}
			})
		})
	})
	if !open {
		d.closeModal()
	}
}

// countDeadLetters is used by the overview to surface a backlog without opening
// the remote tab.
func (d *desktop) deadLetterCount() int {
	if d.remoteStats == nil {
		return 0
	}
	return d.remoteStats.Dead
}

var _ = store.DeadLetter{}
