package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/agent"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/ipc"
	"github.com/shiuoei/print-bridge/internal/logging"
	"github.com/shiuoei/print-bridge/internal/printer"
	"github.com/shiuoei/print-bridge/internal/store"
)

// Tab indexes, kept named so tray navigation and the page switch cannot drift
// apart.
const (
	tabOverview = iota
	tabPrinters
	tabAccess
	tabRemote
	tabHistory
	tabDiagnostics
	tabSettings
	tabAbout
)

type desktop struct {
	store   *store.Store
	agent   *agent.Agent
	service command.Service
	window  *mygo.Window
	tray    *mygo.Tray

	tab       int
	showSetup bool

	// Form state, mirrored from the config on load and written back on save.
	port, printerName, origins, ips, remoteURL, remoteToken, message string
	remoteDeviceID, remoteDeviceName, remoteInterval, remoteRetries  string
	paperW, paperH                                                   string
	paperPreset, defaultCopies                                       string

	// Whitelist page: the pending entry and its own inline error, so a bad entry
	// is reported next to the field instead of failing the whole form.
	newOrigin, newIP, originError, ipError string

	errorDetail    string
	language       string
	theme          string
	version        string
	diagCopied     bool
	agentError     string
	agentErrorCode string
	remoteEnabled  bool

	history     []domain.Status
	printers    []string
	printerInfo *domain.PrinterInfo

	doctor     *command.DoctorReport
	doctorAt   time.Time
	remoteTest *command.RemoteTestReport
	logEntries []logging.Entry
	logLevel   string
	// Long-running actions run off the UI thread; these track their state.
	testPrintTask  busyTask
	doctorTask     busyTask
	remoteTestTask busyTask
	exportTask     busyTask
	// Remote outbox: backlog counts and the abandoned status reports.
	remoteStats *store.RemoteOutboxStats
	deadLetters []store.DeadLetter

	// History page state.
	historyJobs     []store.JobSummary
	historyTable    ui.ListState
	historyRow      int
	historyFilter   string
	historyQuery    string
	historySelected string
	historyTimeline []domain.Status
	// exportHistoryFormat is the last format chosen in the export dialog.
	exportHistoryFormat string

	// Transfer and modal state.
	exportFields map[string]bool
	exportFlags  map[string]*bool
	modal        modalState
	// importPreview holds the pending import so Confirm applies exactly what was
	// previewed, not a re-read of a file that may have changed.
	importPreview *transferPreview
	// quitting is set when the user chose Quit, so the close handler lets the
	// window actually close instead of hiding it.
	quitting bool
}

func (d *desktop) load() {
	c := d.store.Config()
	d.port = strconv.Itoa(c.Service.Port)
	d.printerName = c.Printing.DefaultPrinter
	d.origins = strings.Join(c.Security.AllowedOrigins, "\n")
	d.ips = strings.Join(c.Security.AllowedIPs, "\n")
	d.remoteURL = c.Remote.EndpointURL
	d.remoteToken = c.Remote.BearerToken
	d.remoteEnabled = c.Remote.Enabled
	d.remoteDeviceID = c.Remote.DeviceID
	d.remoteDeviceName = c.Remote.DeviceName
	d.remoteInterval = strconv.Itoa(c.Remote.PollIntervalSeconds)
	d.remoteRetries = strconv.Itoa(c.Remote.MaxReportRetries)
	d.defaultCopies = strconv.Itoa(c.Printing.DefaultCopies)
	d.language = c.App.Language
	d.theme = c.App.Theme
	if d.theme == "" {
		d.theme = themeSystem
	}
	d.showSetup = needsSetup(c)
	d.refreshTrayMenu()
	if c.Printing.DefaultPaper != nil {
		d.paperW = strconv.FormatFloat(c.Printing.DefaultPaper.WidthMM, 'f', -1, 64)
		d.paperH = strconv.FormatFloat(c.Printing.DefaultPaper.HeightMM, 'f', -1, 64)
	} else {
		d.paperW, d.paperH = "210", "297"
	}
	d.refresh()
}
func (d *desktop) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := d.service.Execute(ctx, command.Request{Command: "printers.list"})
	if r.ErrorCode == "" && r.Error == "" {
		d.printers = r.Printers
	} else {
		d.reportError(r.ErrorCode, r.Error)
	}
	h := d.service.Execute(ctx, command.Request{Command: "history.list", Limit: 50})
	if h.Error == "" {
		d.history = h.History
	} else {
		d.reportError(h.ErrorCode, h.Error)
	}
	// The remote backlog is cheap to read and is what tells the operator whether a
	// status report never reached the server.
	if s := d.service.Execute(ctx, command.Request{Command: "remote.stats"}); s.Error == "" {
		d.remoteStats = s.RemoteStats
	}
	if l := d.service.Execute(ctx, command.Request{Command: "remote.dead-letters", Limit: 100}); l.Error == "" {
		d.deadLetters = l.DeadLetters
	}
}
func (d *desktop) refreshPrinterInfo() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	name := strings.TrimSpace(d.printerName)
	if name == "" {
		d.printerInfo, d.message = nil, d.tr("select_printer")
		d.errorDetail = ""
		return
	}
	r := d.service.Execute(ctx, command.Request{Command: "printers.show", Name: name})
	if r.Error != "" {
		d.printerInfo = nil
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	d.printerInfo = r.Printer
	d.message, d.errorDetail = "", ""
}
func lines(s string) []string {
	out := []string{}
	for _, line := range strings.Split(s, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			out = append(out, v)
		}
	}
	return out
}
func (d *desktop) save() {
	c := d.store.Config()
	port, err := strconv.Atoi(strings.TrimSpace(d.port))
	if err != nil {
		d.message = d.tr("invalid_port")
		d.errorDetail = ""
		return
	}
	c.Service.Port = port
	// The dropdown shows a "not selected" placeholder; it must not be persisted as
	// a printer name.
	if d.printerName == d.tr("printer_not_selected") {
		c.Printing.DefaultPrinter = ""
	} else {
		c.Printing.DefaultPrinter = strings.TrimSpace(d.printerName)
	}
	c.Security.AllowedOrigins = lines(d.origins)
	c.Security.AllowedIPs = lines(d.ips)
	c.Remote.EndpointURL = strings.TrimSpace(d.remoteURL)
	c.Remote.BearerToken = d.remoteToken
	c.Remote.Enabled = d.remoteEnabled
	c.Remote.DeviceID = strings.TrimSpace(d.remoteDeviceID)
	c.Remote.DeviceName = strings.TrimSpace(d.remoteDeviceName)
	// The interval and retry count are validated by the command service; an
	// unparsable value is reported here rather than silently kept.
	if v, ok := parsePositiveInt(d.remoteInterval); ok {
		c.Remote.PollIntervalSeconds = v
	} else {
		d.message = d.tr("invalid_remote_interval")
		d.errorDetail = ""
		return
	}
	if v, ok := parseNonNegativeInt(d.remoteRetries); ok {
		c.Remote.MaxReportRetries = v
	} else {
		d.message = d.tr("invalid_remote_retries")
		d.errorDetail = ""
		return
	}
	w, okW := parseMM(d.paperW)
	h, okH := parseMM(d.paperH)
	if !okW || !okH {
		d.message = d.tr("invalid_paper")
		d.errorDetail = ""
		return
	}
	c.Printing.DefaultPaper = &domain.Paper{WidthMM: w, HeightMM: h}
	// Copies are part of the default print settings; an invalid value is reported
	// instead of being silently replaced by the previous one.
	if v, ok := parsePositiveInt(d.defaultCopies); ok {
		c.Printing.DefaultCopies = v
	} else {
		d.message = d.tr("invalid_copies")
		d.errorDetail = ""
		return
	}
	r := d.service.Execute(context.Background(), command.Request{Command: "config.set", Config: &c})
	if r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
	} else {
		d.message = d.tr("saved")
		d.errorDetail = ""
	}
}

func (d *desktop) setLanguage(language string) {
	if language == d.language {
		return
	}
	c := d.store.Config()
	c.App.Language = language
	r := d.service.Execute(context.Background(), command.Request{Command: "config.set", Config: &c})
	if r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	d.language = language
	d.message = d.tr("language_changed")
	d.errorDetail = ""
	d.refreshTrayMenu()
}

// parseMM validates a paper dimension typed into the UI.
func parseMM(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v <= 0 || v > 2000 {
		return 0, false
	}
	return v, true
}

// parsePositiveInt accepts a whole number of at least 1.
func parsePositiveInt(s string) (int, bool) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v < 1 {
		return 0, false
	}
	return v, true
}

// parseNonNegativeInt accepts a whole number of at least 0.
func parseNonNegativeInt(s string) (int, bool) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// paletteType is the page colour set. The custom panels are drawn by hand, so they
// cannot inherit mygo's theme and must switch with it; without this a dark theme
// produced dark controls on white cards.
type paletteType struct{ canvas, surface, ink, muted, line, accent, accentPale, warn, warnPale ui.Color }

var lightPalette = paletteType{
	ui.Hex("#EEF3F5"), ui.Hex("#FFFFFF"), ui.Hex("#173047"), ui.Hex("#627787"),
	ui.Hex("#D8E2E7"), ui.Hex("#087F78"), ui.Hex("#DDF2EE"), ui.Hex("#AF681D"), ui.Hex("#FFF0D9"),
}

var darkPalette = paletteType{
	ui.Hex("#12181D"), ui.Hex("#1B232A"), ui.Hex("#E6EDF2"), ui.Hex("#93A4B1"),
	ui.Hex("#2C3944"), ui.Hex("#4FD1C5"), ui.Hex("#173430"), ui.Hex("#E0A45C"), ui.Hex("#3A2C18"),
}

// palette is the colour set for the frame currently being built. It is a package
// variable because the page builders take no theme argument; view sets it once per
// frame before building anything.
var palette = lightPalette

// usePaletteFor selects the palette matching the resolved theme.
func (d *desktop) usePaletteFor(c *ui.Context) {
	if c.Theme().Dark {
		palette = darkPalette
		return
	}
	palette = lightPalette
}

func panel(c *ui.Context, title, description string, body func()) {
	ui.Column(c).FillWidth().Padding(20).Gap(12).Radius(12).Background(palette.surface).Border(1, palette.line).Children(func() {
		ui.Text(c, title).FontSize(17).Bold().TextColor(palette.ink)
		if description != "" {
			ui.Text(c, description).FontSize(13).TextColor(palette.muted)
		}
		body()
	})
}

func fieldLabel(c *ui.Context, label string) {
	ui.Text(c, label).FontSize(13).Bold().TextColor(palette.ink)
}

func (d *desktop) view(c *ui.Context) {
	// The theme is applied at the root so every control inherits it. A nil theme
	// asks mygo to follow the OS preference, which is what "system" means.
	if theme := themeFor(d.theme); theme != nil {
		ui.Themed(c, theme).Fill().Children(func() {
			d.usePaletteFor(c)
			d.pageBody(c)
		})
		return
	}
	d.usePaletteFor(c)
	d.pageBody(c)
}

// pageBody builds the page chrome and content, inside whatever theme the root
// established.
func (d *desktop) pageBody(c *ui.Context) {
	ui.Column(c).Fill().Background(palette.canvas).Children(func() {
		ui.Column(c).FillWidth().Padding(24, 28, 18).Gap(15).Background(palette.surface).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(12).Children(func() {
				ui.Box(c).Width(5).Height(40).Radius(3).Background(palette.accent)
				ui.Column(c).Gap(2).Children(func() {
					ui.Text(c, "PrintBridge").FontSize(24).Bold().TextColor(palette.ink)
					ui.Text(c, d.tr("subtitle")).FontSize(12).TextColor(palette.muted)
				})
				ui.Spacer(c)
				// The badge reflects both the agent state and the last doctor run, so
				// a configuration problem is visible without opening Diagnostics. It
				// is a button so the click jumps straight to the checks.
				state, tone := d.tr("agent_online"), palette.accent
				if d.agentError != "" {
					state, tone = d.tr("agent_offline"), palette.warn
				} else if d.needsAttention() {
					state, tone = d.tr("needs_attention"), palette.warn
				}
				if ui.Button(c, "●  "+state).TextColor(tone).Clicked() {
					d.tab = tabDiagnostics
				}
			})
			if !d.showSetup {
				ui.Tabs(c, &d.tab, d.tr("tab_overview"), d.tr("tab_printers"), d.tr("tab_access"), d.tr("tab_remote"), d.tr("tab_history"), d.tr("tab_diagnostics"), d.tr("tab_settings"), d.tr("tab_about"))
			}
		})
		scrollKey := fmt.Sprintf("tab-scroll-%d", d.tab)
		if d.showSetup {
			scrollKey = "setup-scroll"
		}
		ui.Scroll(c.Key(scrollKey)).Grow(1).Padding(24, 28).Children(func() {
			ui.Column(c).FillWidth().Gap(16).Children(func() {
				if d.errorDetail != "" {
					panel(c, d.message, d.tr("technical_detail"), func() {
						ui.Text(c, d.errorDetail).FontSize(12).TextColor(palette.muted).Selectable()
					})
				}
				if d.showSetup {
					d.setup(c)
					return
				}
				switch d.tab {
				case tabOverview:
					d.overview(c)
				case tabPrinters:
					d.printing(c)
				case tabAccess:
					d.security(c)
				case tabRemote:
					d.remote(c)
				case tabHistory:
					d.historyPage(c)
				case tabDiagnostics:
					d.diagnostics(c)
				case tabSettings:
					d.settings(c)
				case tabAbout:
					d.aboutPage(c)
				}
			})
		})
		// Modals draw over the page, so they are built after the page content.
		d.exportModal(c)
		d.importModal(c)
		d.clearHistoryModal(c)
		d.purgeDeadLettersModal(c)
		d.exportHistoryModal(c)
		ui.Row(c).FillWidth().Padding(14, 28).Gap(10).AlignItems(ui.Center).Background(palette.surface).Children(func() {
			if d.showSetup {
				if ui.PrimaryButton(c, d.tr("setup_finish")).Clicked() {
					d.finishSetup()
				}
				if ui.Button(c, d.tr("setup_skip")).Clicked() {
					d.skipSetup()
				}
				ui.Spacer(c)
				if d.message != "" && d.errorDetail == "" {
					ui.Text(c, d.message).FontSize(12).TextColor(palette.muted).SingleLine()
				}
				return
			}
			if ui.PrimaryButton(c, d.tr("save_changes")).Clicked() {
				d.save()
			}
			if ui.Button(c, d.tr("discard_changes")).Clicked() {
				d.load()
				d.message = d.tr("changes_discarded")
				d.errorDetail = ""
			}
			ui.Spacer(c)
			if d.message != "" && d.errorDetail == "" {
				ui.Text(c, d.message).FontSize(12).TextColor(palette.muted).SingleLine()
			}
		})
	})
}

func (d *desktop) overview(c *ui.Context) {
	tone, pale, title, detail := palette.accent, palette.accentPale, d.tr("agent_listening"), d.tr("agent_listening_detail")
	cfg := d.store.Config()
	if cfg.Printing.DefaultPrinter != "" && cfg.Printing.DefaultPaper != nil && len(cfg.Security.AllowedOrigins) > 0 {
		detail = d.tr("agent_ready_detail")
	}
	if d.agentError != "" {
		tone, pale, title, detail = palette.warn, palette.warnPale, d.tr("agent_start_failed"), d.errorSummary(d.agentErrorCode)
	}
	ui.Column(c).FillWidth().Padding(24).Gap(8).Radius(12).Background(pale).Children(func() {
		ui.Text(c, d.tr("local_agent")).FontSize(11).Bold().TextColor(tone)
		ui.Text(c, title).FontSize(22).Bold().TextColor(palette.ink)
		ui.Text(c, detail).FontSize(13).TextColor(palette.muted)
		if d.agentError != "" {
			ui.Text(c, d.agentError).FontSize(12).TextColor(palette.muted).Selectable()
		}
	})
	panel(c, d.tr("at_glance"), d.tr("at_glance_detail"), func() {
		ui.Text(c, fmt.Sprintf(d.tr("listening_port"), d.port)).TextColor(palette.ink)
		ui.Text(c, fmt.Sprintf(d.tr("printers_found"), len(d.printers))).TextColor(palette.ink)
		name := d.printerName
		if name == "" {
			name = d.tr("not_configured")
		}
		ui.Text(c, fmt.Sprintf(d.tr("default_printer_value"), name)).TextColor(palette.ink)
		ui.Row(c).Children(func() {
			if ui.Button(c, d.tr("refresh_devices")).Clicked() {
				d.refresh()
				d.message = d.tr("devices_refreshed")
			}
		})
	})
	panel(c, d.tr("data_location"), d.tr("data_location_detail"), func() {
		ui.Text(c, d.store.Dir()).FontSize(12).TextColor(palette.muted).Selectable()
	})
}

// paperLabelOf renders a driver paper as a dropdown label: the driver name plus its
// size, so two differently named papers of the same size stay distinguishable.
func paperLabelOf(paper domain.PrinterPaper) string {
	return fmt.Sprintf("%s  ·  %.1f × %.1f mm", paper.Name, paper.WidthMM, paper.HeightMM)
}

// findPaper resolves a dropdown label back to the driver paper it came from.
func findPaper(papers []domain.PrinterPaper, label string) *domain.PrinterPaper {
	for i := range papers {
		if paperLabelOf(papers[i]) == label {
			return &papers[i]
		}
	}
	return nil
}

func (d *desktop) printing(c *ui.Context) {
	panel(c, d.tr("default_print_setup"), d.tr("default_print_setup_detail"), func() {
		// The printer is chosen from what the system actually reports, with the
		// free-text field kept as a fallback for a queue that is installed but
		// currently unreachable. Typing an exact name used to be the only option.
		fieldLabel(c, d.tr("printer_name"))
		if len(d.printers) > 0 {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				// A leading "not selected" label keeps the trigger readable when no
				// default printer is configured yet.
				options := append([]string{d.tr("printer_not_selected")}, d.printers...)
				if d.printerName == "" {
					d.printerName = d.tr("printer_not_selected")
				}
				ui.Select(c, &d.printerName, options).Width(320)
				if ui.Button(c, d.tr("refresh_devices")).Clicked() {
					d.refresh()
				}
			})
		} else {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.TextInput(c, &d.printerName).Placeholder(d.tr("printer_name_placeholder")).Width(320)
				if ui.Button(c, d.tr("refresh_devices")).Clicked() {
					d.refresh()
				}
			})
			ui.Text(c, d.tr("no_printers_hint")).FontSize(12).TextColor(palette.muted)
		}

		fieldLabel(c, d.tr("paper_size"))
		// A paper the driver advertises is offered as a preset; the millimetre
		// fields stay editable for a custom size the driver does not list.
		if d.printerInfo != nil && len(d.printerInfo.Papers) > 0 {
			labels := make([]string, 0, len(d.printerInfo.Papers)+1)
			labels = append(labels, "")
			for _, paper := range d.printerInfo.Papers {
				labels = append(labels, paperLabelOf(paper))
			}
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Select(c, &d.paperPreset, labels).Width(320)
			})
			if d.paperPreset != "" {
				if chosen := findPaper(d.printerInfo.Papers, d.paperPreset); chosen != nil {
					d.paperW = strconv.FormatFloat(chosen.WidthMM, 'f', -1, 64)
					d.paperH = strconv.FormatFloat(chosen.HeightMM, 'f', -1, 64)
				}
			}
		}
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.TextInput(c, &d.paperW).Width(110).Placeholder(d.tr("width"))
			ui.Text(c, "×").TextColor(palette.muted)
			ui.TextInput(c, &d.paperH).Width(110).Placeholder(d.tr("height"))
			if ui.Button(c, d.tr("view_capabilities")).Clicked() {
				d.refreshPrinterInfo()
			}
			// Printing spends seconds in the spooler; running it inline froze the
			// window. It now runs on a goroutine and reports back.
			if ui.Button(c, busyLabel(d, &d.testPrintTask, "test_print", "test_print_running")).Clicked() {

				name := strings.TrimSpace(d.printerName)
				if name == "" || name == d.tr("printer_not_selected") {
					d.message, d.errorDetail = d.tr("printer_required"), ""
				} else {
					d.runAsync(c, &d.testPrintTask, 30*time.Second,
						command.Request{Command: "test.print", Name: name},
						func(r command.Response) {
							if r.Error != "" {
								d.reportError(r.ErrorCode, r.Error)
							} else {
								d.message, d.errorDetail = d.tr("test_print_submitted"), ""
							}
						})
				}
			}
			if d.testPrintTask.err != "" {
				ui.Text(c, d.testPrintTask.err).FontSize(12).TextColor(palette.warn)
			}
		})
		// Copies are part of the default print settings;
		// leaving them out meant the operator had to change them per job.
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			fieldLabel(c, d.tr("default_copies"))
			ui.TextInput(c, &d.defaultCopies).Width(90)
			ui.Text(c, d.tr("default_copies_hint")).FontSize(12).TextColor(palette.muted)
		})
	})
	panel(c, fmt.Sprintf(d.tr("available_printers"), len(d.printers)), d.tr("available_printers_detail"), func() {
		if len(d.printers) == 0 {
			ui.Text(c, d.tr("no_printers")).TextColor(palette.muted)
		}
		for _, name := range d.printers {
			ui.Text(c, "•  "+name).TextColor(palette.ink).SingleLine()
		}
	})
	if d.printerInfo != nil {
		panel(c, d.printerInfo.Name, fmt.Sprintf(d.tr("printer_capabilities"), len(d.printerInfo.Papers), len(d.printerInfo.Trays), len(d.printerInfo.MediaTypes)), func() {
			shown := d.printerInfo.Papers
			if len(shown) > 12 {
				shown = shown[:12]
			}
			for _, p := range shown {
				ui.Text(c, fmt.Sprintf("%s  ·  %.1f × %.1f mm", p.Name, p.WidthMM, p.HeightMM)).FontSize(12).TextColor(palette.ink).SingleLine()
			}
			if extra := len(d.printerInfo.Papers) - len(shown); extra > 0 {
				ui.Text(c, fmt.Sprintf(d.tr("more_papers"), extra)).FontSize(12).TextColor(palette.muted)
			}
		})
	}
}

func (d *desktop) security(c *ui.Context) {
	d.whitelistPage(c)
}

func (d *desktop) remote(c *ui.Context) {
	panel(c, d.tr("remote_polling"), d.tr("remote_polling_detail"), func() {
		fieldLabel(c, d.tr("enable_remote"))
		ui.Switch(c, &d.remoteEnabled)
		fieldLabel(c, d.tr("https_endpoint"))
		ui.TextInput(c, &d.remoteURL).Placeholder("https://example.com/jobs").FillWidth()
		fieldLabel(c, d.tr("bearer_token"))
		ui.TextInput(c, &d.remoteToken).Password().FillWidth()
		ui.Text(c, d.tr("token_detail")).FontSize(12).TextColor(palette.muted)
	})
	panel(c, d.tr("remote_identity"), d.tr("remote_identity_detail"), func() {
		// Device identity is what the business server uses to attribute jobs, so
		// it needs its own editable fields and a generator rather than being
		// reachable only from the CLI.
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			fieldLabel(c, d.tr("device_id"))
			ui.TextInput(c, &d.remoteDeviceID).Placeholder("uuid v4").Width(280)
			if ui.Button(c, d.tr("generate_device_id")).Clicked() {
				r := d.service.Execute(context.Background(), command.Request{Command: "remote.generate-device-id"})
				if r.Error != "" {
					d.reportError(r.ErrorCode, r.Error)
				} else {
					d.remoteDeviceID = r.Value
					d.message, d.errorDetail = d.tr("device_id_generated"), ""
				}
			}
		})
		fieldLabel(c, d.tr("device_name"))
		ui.TextInput(c, &d.remoteDeviceName).Placeholder("packing-station-01").Width(280)
		ui.Row(c).Gap(12).Children(func() {
			fieldLabel(c, d.tr("poll_interval"))
			ui.TextInput(c, &d.remoteInterval).Width(90)
			fieldLabel(c, d.tr("max_report_retries"))
			ui.TextInput(c, &d.remoteRetries).Width(90)
		})
		ui.Text(c, d.tr("remote_limits_hint")).FontSize(12).TextColor(palette.muted)
	})
	panel(c, d.tr("remote_test_panel"), d.tr("remote_test_detail"), func() {
		// Probing the endpoint does two HTTP round trips; it must not block the
		// frame.
		if ui.Button(c, busyLabel(d, &d.remoteTestTask, "test_remote", "test_remote_running")).Clicked() {
			d.runAsync(c, &d.remoteTestTask, 20*time.Second,
				command.Request{Command: "remote.test"},
				func(r command.Response) {
					d.remoteTest = r.RemoteTest
					if r.Error != "" {
						d.reportError(r.ErrorCode, r.Error)
					} else {
						d.message, d.errorDetail = d.tr("test_remote_ok"), ""
					}
				})
		}
		if d.remoteTestTask.err != "" {
			ui.Text(c, d.remoteTestTask.err).FontSize(12).TextColor(palette.warn)
		}
		if d.remoteTest != nil {
			for _, p := range []command.RemoteProbe{d.remoteTest.GET, d.remoteTest.POST} {
				ui.Text(c, fmt.Sprintf("%s  HTTP %d  %d ms  %s %s", p.Method, p.StatusCode, p.DurationMS, p.Error, p.Response)).FontSize(12).TextColor(palette.muted)
			}
		}
	})
	d.deadLettersPanel(c)
}

func (d *desktop) historyView(c *ui.Context) {
	panel(c, d.tr("recent_jobs"), d.tr("recent_jobs_detail"), func() {
		if ui.Button(c, d.tr("refresh_history")).Clicked() {
			d.refresh()
		}
		if len(d.history) == 0 {
			ui.Text(c, d.tr("no_jobs")).TextColor(palette.muted)
		}
		for _, st := range d.history {
			ui.Column(c).FillWidth().Padding(10, 12).Gap(3).Radius(7).Background(palette.canvas).Children(func() {
				status := d.tr("status_" + st.Status)
				if status == "status_"+st.Status {
					status = strings.ToUpper(st.Status)
				}
				ui.Text(c, st.JobID+"   ·   "+status).FontSize(12).Bold().TextColor(palette.ink).SingleLine()
				ui.Text(c, st.Message).FontSize(12).TextColor(palette.muted).SingleLine()
			})
		}
	})
}

func (d *desktop) diagnostics(c *ui.Context) {
	d.doctorPage(c)
}

func (d *desktop) settings(c *ui.Context) {
	d.themePanel(c)
	panel(c, d.tr("config_transfer"), d.tr("config_transfer_detail"), func() {
		ui.Text(c, d.tr("config_transfer_note")).FontSize(12).TextColor(palette.muted)
		ui.Row(c).Gap(8).Children(func() {
			if ui.Button(c, d.tr("export_config")).Clicked() {
				d.initExportFields()
				d.openModal(modalExport)
			}
			if ui.Button(c, d.tr("import_config")).Clicked() {
				d.importPreview = nil
				d.openModal(modalImport)
			}
		})
	})
}

func (d *desktop) refreshTrayMenu() {
	if d.tray == nil {
		return
	}
	d.tray.SetMenu(mygo.NewMenu([]*mygo.MenuItem{
		{Label: d.tr("tray_open"), Click: func(*mygo.MenuItem, *mygo.Window) { d.showWindow() }},
		{Label: d.tr("tray_test_print"), Click: func(*mygo.MenuItem, *mygo.Window) { d.trayTestPrint() }},
		{Label: d.tr("tray_view_logs"), Click: func(*mygo.MenuItem, *mygo.Window) {
			d.tab = tabDiagnostics
			d.showWindow()
		}},
		{Label: d.tr("tray_restart"), Click: func(*mygo.MenuItem, *mygo.Window) { mygo.App.Relaunch() }},
		{Label: d.tr("tray_autostart"), Type: mygo.MenuItemCheckbox, Checked: mygo.App.OpenAtLogin(), Click: func(it *mygo.MenuItem, _ *mygo.Window) {
			if err := mygo.App.SetOpenAtLogin(it.Checked); err != nil {
				log.Print(err)
				return
			}
			// The config field is what the settings page shows, so it has to
			// follow the OS state; otherwise the two disagree and the UI lies.
			d.setAutostart(it.Checked)
		}},
		mygo.Separator(),
		{Label: d.tr("tray_quit"), Click: func(*mygo.MenuItem, *mygo.Window) {
			d.quitting = true
			mygo.App.Quit()
		}},
	}))
}

// showWindow restores the UI from the tray. A hidden window still has its native
// handle, so Show is enough; the guard covers the startup window.
func (d *desktop) showWindow() {
	if d.window == nil {
		return
	}
	d.window.Show()
	d.window.Focus()
}

// trayTestPrint submits a test page for the configured printer without opening the
// window, matching the tray action.
func (d *desktop) trayTestPrint() {
	name := strings.TrimSpace(d.printerName)
	if name == "" {
		name = d.store.Config().Printing.DefaultPrinter
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := d.service.Execute(ctx, command.Request{Command: "test.print", Name: name})
	if r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	d.message, d.errorDetail = d.tr("test_print_submitted"), ""
}

// setAutostart persists the autostart choice so the settings page and the OS agree.
func (d *desktop) setAutostart(enabled bool) {
	c := d.store.Config()
	if c.App.Autostart == enabled {
		return
	}
	c.App.Autostart = enabled
	if err := d.store.SetConfig(c); err != nil {
		log.Print(err)
	}
}

func main() {
	if !mygo.App.RequestSingleInstanceLock() {
		return
	}
	logBuffer := logging.NewBuffer(500)
	// Install wraps slog's console handler rather than slog's default handler.
	// Reading slog.Default().Handler() here and handing it back would make
	// slog's defaultHandler forward into the logger that replaced it, so the
	// first record takes log's output mutex twice and every later log call in
	// the process blocks forever — the bug that froze test printing.
	logging.Install(logBuffer)
	dir, err := store.DefaultDir()
	if err != nil {
		log.Fatal(err)
	}
	s, err := store.Open(dir)
	if err != nil {
		log.Fatal(err)
	}
	p := printer.New()
	a := agent.New(s, p)
	ctx, cancel := context.WithCancel(context.Background())
	startErr := a.Start(ctx)
	if startErr != nil {
		log.Print(startErr)
	}
	var liveAgent command.AgentState
	var desktopAgent *agent.Agent
	if startErr == nil {
		liveAgent = a
		desktopAgent = a
	}
	ln, err := ipc.Listen(dir)
	if err == nil {
		go ipc.Serve(ctx, ln, command.Service{Store: s, Printer: p, Agent: liveAgent, Logs: logBuffer})
	} else {
		log.Print(err)
	}
	d := &desktop{store: s, agent: desktopAgent, service: command.Service{Store: s, Printer: p, Agent: liveAgent, Logs: logBuffer}}
	// The version is stamped at build time; a dev build says so rather than
	// claiming a release number it does not have.
	d.version = "dev"
	if v := os.Getenv("PRINTBRIDGE_VERSION"); v != "" {
		d.version = v
	}
	if startErr != nil {
		d.agentError = startErr.Error()
		d.agentErrorCode = domain.CodeOf(startErr)
	}
	d.load()
	mygo.App.SetName("PrintBridge")
	mygo.App.OnWindowAllClosed(func() {})
	mygo.App.OnSecondInstance(func([]string, string) {
		if d.window != nil {
			d.window.Show()
		}
	})
	// Both icon sizes come from the one embedded asset: the tray takes the
	// PNG's own dimensions as the icon size, while the window lets Windows
	// scale. A failure leaves the executable's icon resource in charge.
	windowPNG, windowIconErr := iconAt(windowIconSize)
	if windowIconErr != nil {
		log.Print(windowIconErr)
	}
	trayPNG, trayIconErr := iconAt(trayIconSize)
	if trayIconErr != nil {
		log.Print(trayIconErr)
	}
	mygo.App.WhenReady(func() {
		d.window = mygo.NewWindow(mygo.WindowOptions{Title: "PrintBridge", Width: 880, Height: 680, Content: ui.View(d.view)})
		// mygo gives the window the executable's icon resource, which `mygo
		// build` writes from mygo.json's icon; setting it from the embedded
		// asset as well keeps the title bar and taskbar icon right in builds
		// that carry no such resource, such as `go run ./cmd/desktop`.
		if windowPNG != nil {
			if err := d.window.SetIcon(windowPNG); err != nil {
				log.Print(err)
			}
		}
		// Closing the window must hide it, not destroy it. mygo clears the native
		// handle on close, so a destroyed window cannot be shown again from the
		// tray: the process would keep running with no way back. Preventing the
		// close and hiding instead keeps the agent alive and the tray able to
		// restore the UI, which is the documented tray-app contract.
		d.window.OnClose(func(e *mygo.CloseEvent) {
			if d.quitting {
				return
			}
			e.PreventDefault()
			d.window.Hide()
		})
		tray, err := mygo.NewTray(mygo.TrayOptions{Icon: trayPNG, ToolTip: "PrintBridge"})
		if err == nil {
			d.tray = tray
			d.refreshTrayMenu()
		} else {
			// Without a tray icon the window still hides on close, so a
			// failure here would leave the app running with no way back
			// except Task Manager; say so in the log.
			log.Print("tray: ", err)
		}
	})
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) {
		cancel()
		shutdown, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		_ = a.Shutdown(shutdown)
		if ln != nil {
			ln.Close()
		}
		_ = s.Close()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
