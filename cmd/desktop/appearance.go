package main

import (
	"fmt"
	"net/url"
	"os"
	"runtime"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
)

// Theme choices persisted in config.app.theme. "system" follows the OS setting.
const (
	themeSystem = "system"
	themeLight  = "light"
	themeDark   = "dark"
)

// themeFor resolves the configured choice to a concrete mygo theme.
func themeFor(choice string) *ui.Theme {
	switch choice {
	case themeLight:
		return ui.LightTheme()
	case themeDark:
		return ui.DarkTheme()
	}
	return nil // nil asks mygo to follow the system preference.
}

// themePanel renders the appearance choice: language, theme and the setup guide.
func (d *desktop) themePanel(c *ui.Context) {
	panel(c, d.tr("appearance"), d.tr("appearance_detail"), func() {
		fieldLabel(c, d.tr("language"))
		ui.Row(c).Gap(10).Children(func() {
			d.languageButton(c, "zh-CN", d.tr("language_chinese"))
			d.languageButton(c, "en", d.tr("language_english"))
		})
		ui.Text(c, d.tr("language_note")).FontSize(12).TextColor(palette.muted)

		fieldLabel(c, d.tr("theme"))
		ui.Row(c).Gap(10).Children(func() {
			for _, choice := range []struct{ value, key string }{
				{themeSystem, "theme_system"},
				{themeLight, "theme_light"},
				{themeDark, "theme_dark"},
			} {
				value := choice.value
				label := d.tr(choice.key)
				if d.theme == value {
					if ui.PrimaryButton(c, label).Clicked() {
						d.setTheme(value)
					}
				} else if ui.Button(c, label).Clicked() {
					d.setTheme(value)
				}
			}
		})
		ui.Text(c, d.tr("theme_note")).FontSize(12).TextColor(palette.muted)

		ui.Row(c).Children(func() {
			if ui.Button(c, d.tr("setup_open")).Clicked() {
				d.showSetup = true
				d.message, d.errorDetail = "", ""
			}
		})
	})
}

// languageButton renders one language choice, highlighting the active one.
func (d *desktop) languageButton(c *ui.Context, value, label string) {
	if d.language == value {
		if ui.PrimaryButton(c, label).Clicked() {
			d.setLanguage(value)
		}
		return
	}
	if ui.Button(c, label).Clicked() {
		d.setLanguage(value)
	}
}

// setTheme persists the appearance choice. The theme applies on the next frame
// because the window is rebuilt from config each pass.
func (d *desktop) setTheme(value string) {
	if d.theme == value {
		return
	}
	c := d.store.Config()
	c.App.Theme = value
	r := d.service.Execute(nil, command.Request{Command: "config.set", Config: &c})
	if r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	d.theme = value
	d.message, d.errorDetail = d.tr("theme_changed"), ""
}

// aboutPage renders the version, build identity and data location. It replaces the
// previously missing About tab.
func (d *desktop) aboutPage(c *ui.Context) {
	version := d.version
	if version == "" {
		// A desktop built without a version stamp still says something truthful
		// rather than rendering an empty row.
		version = "dev"
	}
	panel(c, d.tr("about"), d.tr("about_detail"), func() {
		summaryRow(c, d.tr("about_version"), version)
		summaryRow(c, d.tr("about_go"), runtime.Version())
		summaryRow(c, d.tr("about_platform"), runtime.GOOS+"/"+runtime.GOARCH)
		if exe, err := os.Executable(); err == nil {
			summaryRow(c, d.tr("about_executable"), exe)
		}
		summaryRow(c, d.tr("about_data_dir"), d.store.Dir())
	})
	d.updatePanel(c)
	panel(c, d.tr("about_links"), d.tr("about_links_detail"), func() {
		ui.Text(c, d.tr("about_license")).FontSize(12).TextColor(palette.muted)
		ui.Text(c, d.tr("about_license_note")).FontSize(12).TextColor(palette.muted)
	})
	panel(c, d.tr("about_diagnostics"), d.tr("about_diagnostics_detail"), func() {
		if ui.Button(c, d.tr("copy_diagnostics")).Clicked() {
			d.copyDiagnostics()
		}
		if d.diagCopied {
			ui.Text(c, d.tr("diagnostics_copied")).FontSize(12).TextColor(palette.accent)
		}
	})
}

// updatePanel shows the signed auto-update state: whether background checks are
// on, the last check, a newer version when one was found, and the manual check.
// Builds that cannot update themselves still show why.
func (d *desktop) updatePanel(c *ui.Context) {
	panel(c, d.tr("updates"), d.tr("updates_detail"), func() {
		if !d.updatesReady {
			ui.Text(c, d.tr("updates_unavailable")).FontSize(12).TextColor(palette.muted)
			return
		}
		automatic := d.updateAutomatic
		if ui.Checkbox(c, &automatic, d.tr("update_automatic")).Changed() {
			d.setUpdateAutomatic(automatic)
		}
		ui.Row(c).Gap(8).Children(func() {
			if ui.Button(c, d.tr("update_check")).Clicked() {
				d.checkForUpdates()
			}
		})
		switch {
		case d.updateAvailable != "":
			ui.Text(c, fmt.Sprintf(d.tr("update_available"), d.updateAvailable)).FontSize(12).TextColor(palette.accent)
		case !d.updateLastCheck.IsZero():
			ui.Text(c, fmt.Sprintf(d.tr("update_last_check"), d.updateLastCheck.Local().Format("2006-01-02 15:04"))).FontSize(12).TextColor(palette.muted)
		default:
			ui.Text(c, d.tr("update_not_checked")).FontSize(12).TextColor(palette.muted)
		}
	})
}

// copyDiagnostics builds a redacted support summary and puts it on the clipboard.
func (d *desktop) copyDiagnostics() {
	s := summarizeDoctor(d.doctor)
	cfg := d.store.Config()
	version := d.version
	if version == "" {
		version = "dev"
	}
	text := fmt.Sprintf(
		"PrintBridge %s\nGo %s\n%s/%s\nport=%d\ntheme=%s\nlanguage=%s\n"+
			"printers=%d\ndefault_printer=%q\npaper=%sx%s\norigins=%d\nips=%d\n"+
			"remote_enabled=%t\nremote_endpoint=%s\n"+
			"doctor: ok=%d warn=%d fail=%d\n",
		version, runtime.Version(), runtime.GOOS, runtime.GOARCH,
		cfg.Service.Port, d.theme, cfg.App.Language,
		len(d.printers), cfg.Printing.DefaultPrinter, d.paperW, d.paperH,
		len(cfg.Security.AllowedOrigins), len(cfg.Security.AllowedIPs),
		cfg.Remote.Enabled, redactEndpoint(cfg.Remote.EndpointURL),
		s.ok, s.warn, s.fail,
	)
	// The bearer token is deliberately absent: a support bundle must never carry a
	// secret, and the operator can re-enter it if needed.
	if err := mygo.Clipboard.Write(transfer.TextData(text)); err != nil {
		d.reportError(domain.CodeInternalError, err.Error())
		return
	}
	d.diagCopied = true
	d.message, d.errorDetail = d.tr("diagnostics_copied"), ""
}

// redactEndpoint keeps the scheme and host of an endpoint for support purposes but
// drops the path and query, which often carry a tenant identifier.
func redactEndpoint(raw string) string {
	if raw == "" {
		return "(empty)"
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return "(invalid)"
	}
	return u.Scheme + "://" + u.Host
}
