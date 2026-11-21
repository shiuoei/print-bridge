package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
)

// Existing installations with complete print settings should open normally.
// Fresh installations can also explicitly skip the guide and reopen it later.
func needsSetup(c domain.Config) bool {
	return !c.App.SetupCompleted &&
		(c.Printing.DefaultPrinter == "" || c.Printing.DefaultPaper == nil || len(c.Security.AllowedOrigins) == 0)
}

func buildSetupConfig(c domain.Config, available []string, printerName, paperW, paperH, origins, port string) (domain.Config, string, error) {
	name := strings.TrimSpace(printerName)
	if name == "" || !slices.Contains(available, name) {
		return c, "setup_select_printer_error", nil
	}
	w, okW := parseMM(paperW)
	h, okH := parseMM(paperH)
	if !okW || !okH {
		return c, "invalid_paper", nil
	}
	allowed := lines(origins)
	if len(allowed) == 0 {
		return c, "setup_origin_error", nil
	}
	parsedPort, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil {
		return c, "invalid_port", nil
	}
	c.Printing.DefaultPrinter = name
	c.Printing.DefaultPaper = &domain.Paper{WidthMM: w, HeightMM: h}
	c.Security.AllowedOrigins = allowed
	c.Service.Port = parsedPort
	c.App.SetupCompleted = true
	if err := c.Validate(); err != nil {
		return c, "setup_invalid_config", err
	}
	return c, "", nil
}

func (d *desktop) finishSetup() {
	c, key, err := buildSetupConfig(d.store.Config(), d.printers, d.printerName, d.paperW, d.paperH, d.origins, d.port)
	if key != "" {
		d.message = d.tr(key)
		d.errorDetail = ""
		if err != nil {
			d.errorDetail = err.Error()
		}
		return
	}
	r := d.service.Execute(context.Background(), command.Request{Command: "config.set", Config: &c})
	if r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	d.showSetup = false
	d.message = d.tr("setup_done")
	if d.agentError != "" {
		d.message = d.tr("setup_done_restart")
	}
	d.errorDetail = ""
	d.tab = 0
}

func (d *desktop) skipSetup() {
	c := d.store.Config()
	c.App.SetupCompleted = true
	r := d.service.Execute(context.Background(), command.Request{Command: "config.set", Config: &c})
	if r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	d.showSetup = false
	d.message, d.errorDetail = d.tr("setup_skipped"), ""
	d.tab = 0
}

func (d *desktop) setup(c *ui.Context) {
	ui.Column(c).FillWidth().Padding(24).Gap(8).Radius(12).Background(palette.accentPale).Children(func() {
		ui.Text(c, d.tr("setup_heading")).FontSize(22).Bold().TextColor(palette.ink)
		ui.Text(c, d.tr("setup_intro")).FontSize(13).TextColor(palette.muted)
	})
	if d.agentError != "" {
		panel(c, d.tr("setup_connection_problem"), d.errorSummary(d.agentErrorCode), func() {
			ui.Text(c, d.agentError).FontSize(12).TextColor(palette.muted).Selectable()
			if d.agentErrorCode == domain.CodeServicePortInUse {
				ui.Text(c, d.tr("setup_port_conflict_hint")).FontSize(12).TextColor(palette.warn)
			}
		})
	}
	panel(c, d.tr("setup_printer_title"), d.tr("setup_printer_detail"), func() {
		if len(d.printers) == 0 {
			ui.Text(c, d.tr("no_printers")).TextColor(palette.muted)
		} else {
			ui.Select(c, &d.printerName, d.printers).FillWidth()
		}
		ui.Row(c).Children(func() {
			if ui.Button(c, d.tr("refresh_devices")).Clicked() {
				d.refresh()
				d.message = d.tr("devices_refreshed")
			}
		})
	})
	panel(c, d.tr("setup_paper_title"), d.tr("setup_paper_detail"), func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.TextInput(c, &d.paperW).Width(110).Placeholder(d.tr("width"))
			ui.Text(c, "×").TextColor(palette.muted)
			ui.TextInput(c, &d.paperH).Width(110).Placeholder(d.tr("height"))
			ui.Text(c, "mm").TextColor(palette.muted)
			if ui.Button(c, d.tr("view_capabilities")).Clicked() {
				d.refreshPrinterInfo()
			}
		})
		if d.printerInfo != nil && d.printerInfo.Name == d.printerName {
			ui.Text(c, fmt.Sprintf(d.tr("printer_capabilities"), len(d.printerInfo.Papers), len(d.printerInfo.Trays), len(d.printerInfo.MediaTypes))).FontSize(12).TextColor(palette.muted)
		}
	})
	panel(c, d.tr("setup_origin_title"), d.tr("setup_origin_detail"), func() {
		ui.TextArea(c, &d.origins).Placeholder("https://example.com").FillWidth().Height(78)
	})
	panel(c, d.tr("setup_connection_title"), d.tr("setup_connection_detail"), func() {
		fieldLabel(c, d.tr("websocket_port"))
		ui.TextInput(c, &d.port).Width(140)
		ui.Text(c, d.tr("restart_port")).FontSize(12).TextColor(palette.muted)
		fieldLabel(c, d.tr("data_location"))
		ui.Text(c, d.store.Dir()).FontSize(12).TextColor(palette.muted).Selectable()
	})
}
