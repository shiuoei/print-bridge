package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/transfer"
)

// transferPreview is the outcome of decrypting an import file, held so the
// confirm step applies exactly what the operator reviewed.
type transferPreview struct {
	path     string
	password string
	merged   domain.Config
	changes  []string
}

// exportModal renders the encrypted export dialog: field checkboxes, password,
// and the empty-password warning that the CLI also requires.
func (d *desktop) exportModal(c *ui.Context) {
	if d.modal.kind != modalExport {
		return
	}
	open := d.modal.open
	ui.DialogBase(c, &open, func(_, _ ui.Element) {
		ui.Column(c).Gap(10).Padding(16).Width(420).Children(func() {
			ui.Text(c, d.tr("export_title")).FontSize(16).Bold().TextColor(palette.ink)
			ui.Text(c, d.tr("export_body")).FontSize(12).TextColor(palette.muted)

			ui.Text(c, d.tr("export_fields")).FontSize(12).Bold().TextColor(palette.ink)
			// Two columns keep the eight fields visible without scrolling. Each
			// checkbox binds to its own bool field: a map index is not addressable.
			for i := 0; i < len(transferFieldOrder); i += 2 {
				ui.Row(c).Gap(16).Children(func() {
					for j := i; j < i+2 && j < len(transferFieldOrder); j++ {
						name := transferFieldOrder[j]
						ui.Checkbox(c, d.exportFieldFlag(name), d.tr(fieldLabelKey[name]))
					}
				})
			}

			ui.Text(c, d.tr("export_password")).FontSize(12).Bold().TextColor(palette.ink)
			ui.TextInput(c, &d.modal.password).Password().FillWidth()
			ui.Text(c, d.tr("export_password_hint")).FontSize(11).TextColor(palette.muted)

			// The token is the one field where an empty password materially
			// weakens the file, so the dialog says so before writing it.
			needsWarning := d.modal.password == "" && d.exportFields["remote-token"] &&
				d.store.Config().Remote.BearerToken != ""
			if needsWarning {
				ui.Text(c, d.tr("export_empty_password_warning")).FontSize(12).TextColor(palette.warn)
				ui.Checkbox(c, &d.modal.confirmed, d.tr("export_empty_password_confirm"))
			}
			if d.modal.err != "" {
				ui.Text(c, d.modal.err).FontSize(12).TextColor(palette.warn)
			}
			ui.Row(c).Gap(8).Children(func() {
				if ui.Button(c, d.tr("cancel")).Clicked() {
					d.closeModal()
				}
				disabled := needsWarning && !d.modal.confirmed
				if disabled {
					ui.Button(c, d.tr("export_confirm"))
				} else if ui.PrimaryButton(c, d.tr("export_confirm")).Clicked() {
					d.runExport()
				}
			})
		})
	})
	if !open {
		d.closeModal()
	}
}

// runExport writes the encrypted config file the operator asked for.
func (d *desktop) runExport() {
	fields := d.selectedTransferFields()
	if len(fields) == 0 {
		d.modal.err = d.tr("export_no_fields")
		return
	}
	path, err := d.pickSaveFile(d.tr("export_title"), "printbridge-config.json")
	if err != nil || path == "" {
		return
	}
	c := d.store.Config()
	selection := transfer.SelectTailored(c, fields)
	data, err := transfer.Export(selection, d.modal.password)
	if err != nil {
		d.modal.err = err.Error()
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		d.modal.err = err.Error()
		return
	}
	d.closeModal()
	d.message = d.tr("config_exported")
	d.errorDetail = ""
}

// importModal renders the import dialog: file picker, password, and the change
// preview the operator must confirm.
func (d *desktop) importModal(c *ui.Context) {
	if d.modal.kind != modalImport && d.modal.kind != modalImportPreview {
		return
	}
	open := d.modal.open
	ui.DialogBase(c, &open, func(_, _ ui.Element) {
		ui.Column(c).Gap(10).Padding(16).Width(460).Children(func() {
			ui.Text(c, d.tr("import_title")).FontSize(16).Bold().TextColor(palette.ink)
			ui.Text(c, d.tr("import_body")).FontSize(12).TextColor(palette.muted)

			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, d.tr("import_file")).FontSize(12).Bold().TextColor(palette.ink)
				if ui.Button(c, d.tr("choose_file")).Clicked() {
					if path, err := d.pickOpenFile(d.tr("import_title")); err == nil {
						d.modal.path = path
						// A different file invalidates any previous preview.
						d.importPreview = nil
						d.modal.err = ""
					}
				}
				shown := d.modal.path
				if shown == "" {
					shown = d.tr("no_file_chosen")
				}
				ui.Text(c, shown).FontSize(12).TextColor(palette.muted).SingleLine()
			})

			ui.Text(c, d.tr("import_password")).FontSize(12).Bold().TextColor(palette.ink)
			// Changing the password invalidates the preview: the same file with a
			// different password decrypts to different content or fails.
			ui.TextInput(c, &d.modal.password).Password().FillWidth()
			ui.Text(c, d.tr("import_password_hint")).FontSize(11).TextColor(palette.muted)

			if d.importPreview != nil {
				ui.Text(c, d.tr("import_changes")).FontSize(12).Bold().TextColor(palette.ink)
				if len(d.importPreview.changes) == 0 {
					ui.Text(c, d.tr("no_import_changes")).FontSize(12).TextColor(palette.muted)
				}
				ui.Scroll(c).Height(150).Children(func() {
					ui.Column(c).FillWidth().Gap(4).Children(func() {
						for _, line := range d.importPreview.changes {
							ui.Text(c, line).FontSize(12).TextColor(palette.ink).SingleLine()
						}
					})
				})
			}
			if d.modal.err != "" {
				ui.Text(c, d.modal.err).FontSize(12).TextColor(palette.warn)
			}
			ui.Row(c).Gap(8).Children(func() {
				if ui.Button(c, d.tr("cancel")).Clicked() {
					d.closeModal()
				}
				if d.importPreview == nil {
					if ui.Button(c, d.tr("preview_import")).Clicked() {
						d.runImportPreview()
					}
				} else if ui.PrimaryButton(c, d.tr("confirm_import")).Clicked() {
					d.runImportApply()
				}
			})
		})
	})
	if !open {
		d.closeModal()
	}
}

// runImportPreview decrypts the file and shows the redacted change list.
func (d *desktop) runImportPreview() {
	if d.modal.path == "" {
		d.modal.err = d.tr("no_file_chosen")
		return
	}
	data, err := os.ReadFile(d.modal.path)
	if err != nil {
		d.modal.err = err.Error()
		return
	}
	current := d.store.Config()
	merged, err := transfer.Import(data, d.modal.password, current)
	if err != nil {
		// A wrong password and a damaged file are indistinguishable here, which
		// is exactly what the encrypted format is supposed to guarantee.
		d.modal.err = d.tr("import_failed")
		return
	}
	d.importPreview = &transferPreview{
		path:     d.modal.path,
		password: d.modal.password,
		merged:   merged,
		changes:  diffForUI(current, merged),
	}
	d.modal.err = ""
	d.modal.kind = modalImportPreview
}

// runImportApply saves exactly the configuration the operator previewed.
func (d *desktop) runImportApply() {
	if d.importPreview == nil {
		return
	}
	merged := d.importPreview.merged
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := d.service.Execute(ctx, command.Request{Command: "config.set", Config: &merged})
	if r.Error != "" {
		d.modal.err = r.Error
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	changes := len(d.importPreview.changes)
	d.closeModal()
	d.importPreview = nil
	d.load()
	d.message = strings.ReplaceAll(d.tr("config_imported"), "%d", strconv.Itoa(changes))
	d.errorDetail = ""
}

// diffForUI produces the redacted change list shown in the preview. Secrets never
// appear in clear text.
func diffForUI(before, after domain.Config) []string {
	out := []string{}
	if before.Service.Port != after.Service.Port {
		out = append(out, lineChange("service.port", strconv.Itoa(before.Service.Port), strconv.Itoa(after.Service.Port)))
	}
	if !sameStrings(before.Security.AllowedOrigins, after.Security.AllowedOrigins) {
		out = append(out, lineChange("security.allowed_origins",
			countLabel(len(before.Security.AllowedOrigins)), countLabel(len(after.Security.AllowedOrigins))))
	}
	if !sameStrings(before.Security.AllowedIPs, after.Security.AllowedIPs) {
		out = append(out, lineChange("security.allowed_ips",
			countLabel(len(before.Security.AllowedIPs)), countLabel(len(after.Security.AllowedIPs))))
	}
	if before.Remote.Enabled != after.Remote.Enabled {
		out = append(out, lineChange("remote.enabled", boolLabel(before.Remote.Enabled), boolLabel(after.Remote.Enabled)))
	}
	if before.Remote.EndpointURL != after.Remote.EndpointURL {
		out = append(out, lineChange("remote.endpoint_url", emptyLabel(before.Remote.EndpointURL), emptyLabel(after.Remote.EndpointURL)))
	}
	if before.Remote.BearerToken != after.Remote.BearerToken {
		out = append(out, lineChange("remote.bearer_token", secretLabel(before.Remote.BearerToken), secretLabel(after.Remote.BearerToken)))
	}
	if before.Remote.PollIntervalSeconds != after.Remote.PollIntervalSeconds {
		out = append(out, lineChange("remote.poll_interval_seconds",
			strconv.Itoa(before.Remote.PollIntervalSeconds), strconv.Itoa(after.Remote.PollIntervalSeconds)))
	}
	if before.Remote.MaxReportRetries != after.Remote.MaxReportRetries {
		out = append(out, lineChange("remote.max_report_retries",
			strconv.Itoa(before.Remote.MaxReportRetries), strconv.Itoa(after.Remote.MaxReportRetries)))
	}
	return out
}

func lineChange(key, before, after string) string {
	return key + ": " + before + " -> " + after
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func countLabel(n int) string {
	return strconv.Itoa(n) + " entries"
}

func boolLabel(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func emptyLabel(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(empty)"
	}
	return v
}

func secretLabel(v string) string {
	if v == "" {
		return "(empty)"
	}
	return "********"
}
