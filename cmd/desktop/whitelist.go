package main

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
)

// Validation sentinels, mapped to localized text by errText.
var (
	errInvalidOrigin = errors.New("invalid origin")
	errInvalidIP     = errors.New("invalid ip")
	errAnyAddress    = errors.New("any address")
)

// This file implements per-entry management for the two allowlists. The original
// product let the operator add one entry at a time, delete a single row, and kept
// the loopback address pinned so it could not be removed by accident. The previous
// textarea made a typo silently disable browser access until the next global save,
// so each action here persists immediately and reports its own error.

// whitelistPage renders both allowlists with per-entry controls.
func (d *desktop) whitelistPage(c *ui.Context) {
	d.originListPanel(c)
	d.ipListPanel(c)
	d.portPanel(c)
}

// originListPanel manages the website Origin allowlist.
func (d *desktop) originListPanel(c *ui.Context) {
	panel(c, d.tr("website_access"), d.tr("website_access_detail"), func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.TextInput(c, &d.newOrigin).Placeholder("https://example.com").Width(320)
			if ui.Button(c, d.tr("add")).Clicked() {
				d.addOrigin()
			}
		})
		if d.originError != "" {
			ui.Text(c, d.originError).FontSize(12).TextColor(palette.warn)
		}
		origins := d.store.Config().Security.AllowedOrigins
		if len(origins) == 0 {
			ui.Text(c, d.tr("no_origins")).FontSize(12).TextColor(palette.muted)
			return
		}
		for _, origin := range origins {
			entry := origin
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Padding(6, 10).Radius(7).Background(palette.canvas).Children(func() {
				ui.Text(c, entry).FontSize(12).TextColor(palette.ink).SingleLine()
				ui.Spacer(c)
				if ui.Button(c, d.tr("delete")).Clicked() {
					d.removeOrigin(entry)
				}
			})
		}
	})
}

// ipListPanel manages the client IP allowlist. The loopback entry is pinned: it is
// shown but cannot be deleted, because the desktop UI and the CLI reach the agent
// over loopback.
func (d *desktop) ipListPanel(c *ui.Context) {
	panel(c, d.tr("client_access"), d.tr("client_access_detail"), func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.TextInput(c, &d.newIP).Placeholder("192.168.1.0/24").Width(320)
			if ui.Button(c, d.tr("add")).Clicked() {
				d.addIP()
			}
		})
		if d.ipError != "" {
			ui.Text(c, d.ipError).FontSize(12).TextColor(palette.warn)
		}
		entries := d.store.Config().Security.AllowedIPs
		if len(entries) == 0 {
			ui.Text(c, d.tr("no_ips")).FontSize(12).TextColor(palette.muted)
			return
		}
		for _, entry := range entries {
			value := entry
			pinned := value == domain.RequiredLoopbackIP
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Padding(6, 10).Radius(7).Background(palette.canvas).Children(func() {
				ui.Text(c, value).FontSize(12).TextColor(palette.ink).SingleLine()
				if pinned {
					ui.Badge(c, d.tr("loopback_fixed"))
				}
				ui.Spacer(c)
				if !pinned && ui.Button(c, d.tr("delete")).Clicked() {
					d.removeIP(value)
				}
			})
		}
	})
}

// portPanel holds the listener port, which changes the same security surface.
func (d *desktop) portPanel(c *ui.Context) {
	panel(c, d.tr("listener"), d.tr("listener_detail"), func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			fieldLabel(c, d.tr("websocket_port"))
			ui.TextInput(c, &d.port).Width(120)
		})
		ui.Text(c, d.tr("restart_port")).FontSize(12).TextColor(palette.muted)
	})
}

// addOrigin validates and persists one Origin.
func (d *desktop) addOrigin() {
	value := strings.TrimSpace(d.newOrigin)
	if err := validateOriginEntry(value); err != nil {
		d.originError = d.errText(err)
		return
	}
	for _, existing := range d.store.Config().Security.AllowedOrigins {
		if strings.EqualFold(existing, value) {
			d.originError = d.tr("entry_duplicate")
			return
		}
	}
	d.mutateConfig(func(c *domain.Config) { c.Security.AllowedOrigins = append(c.Security.AllowedOrigins, value) }, &d.originError)
	d.newOrigin = ""
}

// removeOrigin deletes one Origin.
func (d *desktop) removeOrigin(value string) {
	d.mutateConfig(func(c *domain.Config) {
		c.Security.AllowedOrigins = removeString(c.Security.AllowedOrigins, value)
	}, &d.originError)
}

// addIP validates and persists one IP or CIDR.
func (d *desktop) addIP() {
	value := strings.TrimSpace(d.newIP)
	if err := validateIPEntry(value); err != nil {
		d.ipError = d.errText(err)
		return
	}
	for _, existing := range d.store.Config().Security.AllowedIPs {
		if existing == value {
			d.ipError = d.tr("entry_duplicate")
			return
		}
	}
	d.mutateConfig(func(c *domain.Config) { c.Security.AllowedIPs = append(c.Security.AllowedIPs, value) }, &d.ipError)
	d.newIP = ""
}

// removeIP deletes one IP entry. The loopback entry is refused here as well as in
// the UI, so a future caller cannot bypass the pinning.
func (d *desktop) removeIP(value string) {
	if value == domain.RequiredLoopbackIP {
		d.ipError = d.tr("loopback_pinned")
		return
	}
	d.mutateConfig(func(c *domain.Config) {
		c.Security.AllowedIPs = removeString(c.Security.AllowedIPs, value)
	}, &d.ipError)
}

// mutateConfig applies one change through the command service so a running agent
// and the on-disk file stay consistent, and records any failure for the panel.
func (d *desktop) mutateConfig(change func(*domain.Config), errOut *string) {
	c := d.store.Config()
	change(&c)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := d.service.Execute(ctx, command.Request{Command: "config.set", Config: &c})
	if r.Error != "" {
		*errOut = d.errText(errors.New(r.Error))
		return
	}
	*errOut = ""
	d.message, d.errorDetail = d.tr("saved"), ""
}

// validateOriginEntry enforces the exact-origin rule the agent applies at
// handshake time: scheme, host, and no path, query or fragment.
func validateOriginEntry(value string) error {
	if value == "" {
		return errInvalidOrigin
	}
	u, err := url.Parse(value)
	if err != nil || u == nil {
		return errInvalidOrigin
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errInvalidOrigin
	}
	if u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errInvalidOrigin
	}
	return nil
}

// validateIPEntry accepts an IP literal or a CIDR block, and refuses any-address
// entries that would open the agent to the whole network.
func validateIPEntry(value string) error {
	if value == "" {
		return errInvalidIP
	}
	if ip := net.ParseIP(value); ip != nil {
		if ip.IsUnspecified() {
			return errAnyAddress
		}
		return nil
	}
	_, network, err := net.ParseCIDR(value)
	if err != nil {
		return errInvalidIP
	}
	if ones, bits := network.Mask.Size(); ones == 0 && bits > 0 {
		return errAnyAddress
	}
	return nil
}

// removeString returns list without value.
func removeString(list []string, value string) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		if item != value {
			out = append(out, item)
		}
	}
	return out
}

// errText turns a validation sentinel or protocol error into a localized message.
func (d *desktop) errText(err error) string {
	switch err {
	case errInvalidOrigin:
		return d.tr("invalid_origin")
	case errInvalidIP:
		return d.tr("invalid_ip")
	case errAnyAddress:
		return d.tr("invalid_any_ip")
	}
	return domain.MessageOf(err)
}
