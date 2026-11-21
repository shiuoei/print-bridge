package main

import (
	"errors"
	"log"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// This file holds the shared modal/dialog plumbing for the desktop pages. mygo's
// dialogs block, so they are only opened from a click handler, never while
// building the page: a blocking call inside a build would freeze the frame.

// modalState is one open dialog's state. Only one modal is open at a time, which
// keeps the escape/focus behaviour predictable.
type modalState struct {
	// kind identifies which dialog the page should draw.
	kind string
	// open is the flag the dialog base binds to.
	open bool
	// password and note are the shared text fields.
	password string
	note     string
	// confirmed is the explicit acknowledgement checkbox, used by the export
	// dialog when an empty password would leave the token effectively
	// unprotected.
	confirmed bool
	// path is the chosen file for import/export.
	path string
	// busy disables the confirm button while an operation runs.
	busy bool
	// err holds the last failure shown inside the dialog.
	err string
}

// Modal kinds.
const (
	modalNone             = ""
	modalExport           = "export"
	modalImport           = "import"
	modalClearHistory     = "clear-history"
	modalImportPreview    = "import-preview"
	modalPurgeDeadLetters = "purge-dead-letters"
	modalExportHistory    = "export-history"
)

// openModal switches to a modal and resets its transient state.
func (d *desktop) openModal(kind string) {
	d.modal = modalState{kind: kind, open: true}
}

// closeModal dismisses the current modal.
func (d *desktop) closeModal() {
	d.modal = modalState{}
}

// pickSaveFile asks for a destination path with the native save dialog.
func (d *desktop) pickSaveFile(title, defaultName string) (string, error) {
	return saveFileDialog(d.window, title, defaultName,
		fileFilter{Name: "PrintBridge config", Extensions: []string{"json"}})
}

// fileFilter is the subset of a mygo FileFilter this package needs, so callers do
// not have to import mygo just to describe a filter.
type fileFilter struct {
	Name       string
	Extensions []string
}

// saveFileDialog opens the native save dialog with one filter.
func saveFileDialog(parent *mygo.Window, title, defaultName string, filter fileFilter) (string, error) {
	return mygo.Dialog.Save(mygo.SaveDialogOptions{
		Parent:      parent,
		Title:       title,
		DefaultPath: defaultName,
		Filters:     []mygo.FileFilter{{Name: filter.Name, Extensions: filter.Extensions}},
	})
}

// pickOpenFile asks for an existing file with the native open dialog.
func (d *desktop) pickOpenFile(title string) (string, error) {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent:  d.window,
		Title:   title,
		Filters: []mygo.FileFilter{{Name: "PrintBridge config", Extensions: []string{"json"}}},
	})
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", errors.New("no file chosen")
	}
	return paths[0], nil
}

// confirm runs a blocking yes/no native dialog. It is only called from a click
// handler, never during build.
func (d *desktop) confirm(title, message, confirmLabel string) bool {
	res, err := mygo.Dialog.Message(mygo.MessageOptions{
		Parent:             d.window,
		Type:               mygo.MessageWarning,
		Title:              title,
		Message:            message,
		Buttons:            []string{d.tr("cancel"), confirmLabel},
		DefaultButton:      1,
		CancelButton:       0,
		DestructiveButtons: []int{1},
	})
	if err != nil {
		log.Print(err)
		return false
	}
	return res.Button == 1
}

// errorAlert reports a failure to the operator without leaving the page.
func (d *desktop) errorAlert(title, message string) {
	_, _ = mygo.Dialog.Message(mygo.MessageOptions{
		Parent:        d.window,
		Type:          mygo.MessageError,
		Title:         title,
		Message:       message,
		Buttons:       []string{d.tr("close")},
		DefaultButton: 0,
		CancelButton:  0,
	})
}

// transferFields lists the selectable export fields with their localized labels.
// The order matches the established settings order so an operator moving between the
// CLI and the UI sees the same list.
var transferFieldOrder = []string{
	"service-port",
	"allowed-origins",
	"allowed-ips",
	"remote-enabled",
	"remote-url",
	"remote-token",
	"remote-interval",
	"remote-max-retries",
}

// fieldLabelKey maps an export field to its i18n key.
var fieldLabelKey = map[string]string{
	"service-port":       "field_service_port",
	"allowed-origins":    "field_allowed_origins",
	"allowed-ips":        "field_allowed_ips",
	"remote-enabled":     "field_remote_enabled",
	"remote-url":         "field_remote_url",
	"remote-token":       "field_remote_token",
	"remote-interval":    "field_remote_interval",
	"remote-max-retries": "field_remote_max_retries",
}

// selectedTransferFields returns the fields currently ticked, in stable order.
func (d *desktop) selectedTransferFields() []string {
	d.syncExportFields()
	out := make([]string, 0, len(transferFieldOrder))
	for _, name := range transferFieldOrder {
		if d.exportFields[name] {
			out = append(out, name)
		}
	}
	return out
}

// exportFieldFlag returns the addressable bool for one export field, creating it
// on first use. mygo checkboxes bind to a *bool, and a map index is not
// addressable, so each field keeps its own named flag.
func (d *desktop) exportFieldFlag(name string) *bool {
	if d.exportFlags == nil {
		d.exportFlags = map[string]*bool{}
	}
	if flag, ok := d.exportFlags[name]; ok {
		return flag
	}
	flag := new(bool)
	*flag = true
	d.exportFlags[name] = flag
	return flag
}

// syncExportFields copies the checkbox flags into the map read by the export
// path, so the two representations cannot disagree.
func (d *desktop) syncExportFields() {
	for name, flag := range d.exportFlags {
		d.exportFields[name] = *flag
	}
}

// initExportFields selects every field by default, matching the export dialog.
func (d *desktop) initExportFields() {
	if d.exportFields == nil {
		d.exportFields = map[string]bool{}
	}
	for _, name := range transferFieldOrder {
		d.exportFields[name] = true
		d.exportFieldFlag(name)
	}
}

// badge renders a short status label with the palette's accent.
func badge(c *ui.Context, text string) {
	ui.Badge(c, text)
}

// summaryRow renders one "label: value" line used by the history detail pane.
func summaryRow(c *ui.Context, label, value string) {
	ui.Row(c).Gap(8).Children(func() {
		ui.Text(c, label).FontSize(12).TextColor(palette.muted)
		ui.Text(c, value).FontSize(12).TextColor(palette.ink).SingleLine()
	})
}

// joinNonEmpty joins the non-empty strings with a separator.
func joinNonEmpty(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}
