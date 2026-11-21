package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/logging"
)

// Doctor check groups. The order is the reading order: the things that block any
// printing first, then printing itself, then the optional remote integration.
var doctorGroups = []struct {
	key   string
	label string
	ids   []string
}{
	{key: "core", label: "doctor_group_core", ids: []string{"config", "data_dir", "agent", "port"}},
	{key: "printing", label: "doctor_group_printing", ids: []string{"printers", "paper", "renderer", "office.docx", "office.xlsx", "office.pptx"}},
	{key: "access", label: "doctor_group_access", ids: []string{"origins", "ips"}},
	{key: "remote", label: "doctor_group_remote", ids: []string{"remote", "retention"}},
}

// doctorSummary counts the levels across all checks, ignoring skipped ones.
type doctorSummary struct {
	ok, warn, fail int
}

func summarizeDoctor(report *command.DoctorReport) doctorSummary {
	var s doctorSummary
	if report == nil {
		return s
	}
	for _, check := range report.Checks {
		if check.Skipped {
			continue
		}
		switch check.Level {
		case command.DoctorOK:
			s.ok++
		case command.DoctorWarn:
			s.warn++
		case command.DoctorFail:
			s.fail++
		}
	}
	return s
}

// needsAttention reports whether the last doctor run found a failure or warning.
// The header badge uses it so the operator sees a problem without opening the tab.
func (d *desktop) needsAttention() bool {
	s := summarizeDoctor(d.doctor)
	return s.fail > 0 || s.warn > 0
}

// groupOf returns the group key a check id belongs to, or "other".
func groupOf(id string) string {
	for _, group := range doctorGroups {
		for _, candidate := range group.ids {
			if candidate == id {
				return group.key
			}
		}
	}
	return "other"
}

// doctorPage renders the diagnostics tab: summary counters, grouped checks with
// per-check suggestion, and the recent log tail.
func (d *desktop) doctorPage(c *ui.Context) {
	panel(c, d.tr("system_checks"), d.tr("system_checks_detail"), func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			// Doctor probes printers, browsers and Office converters; it can take
			// seconds, so it runs off the UI thread.
			if ui.Button(c, busyLabel(d, &d.doctorTask, "run_checks", "checks_running")).Clicked() {
				d.runAsync(c, &d.doctorTask, 30*time.Second,
					command.Request{Command: "doctor"},
					func(r command.Response) {
						d.doctor = r.Doctor
						d.doctorAt = time.Now()
						if d.doctor == nil && r.Error != "" {
							d.reportError(r.ErrorCode, r.Error)
						}
					})
			}
			if d.doctor != nil && !d.doctorTask.running {
				if ui.Button(c, d.tr("retry_checks")).Clicked() {
					d.runAsync(c, &d.doctorTask, 30*time.Second,
						command.Request{Command: "doctor"},
						func(r command.Response) {
							d.doctor = r.Doctor
							d.doctorAt = time.Now()
							if d.doctor == nil && r.Error != "" {
								d.reportError(r.ErrorCode, r.Error)
							}
						})
				}
			}
			ui.Spacer(c)
			if d.doctor != nil {
				ui.Text(c, d.tr("checks_completed_at")+" "+d.doctorAt.Local().Format("15:04:05")).
					FontSize(11).TextColor(palette.muted)
			}
		})
		if d.doctor == nil {
			ui.Text(c, d.tr("run_checks_prompt")).TextColor(palette.muted)
			return
		}
		s := summarizeDoctor(d.doctor)
		ui.Row(c).Gap(10).Children(func() {
			doctorCounter(c, d.tr("level_ok"), s.ok, palette.accent)
			doctorCounter(c, d.tr("level_warn"), s.warn, palette.warn)
			doctorCounter(c, d.tr("level_fail"), s.fail, palette.warn)
		})
		// Group the checks so a failure is located by subsystem, not by scrolling a
		// flat list. Unknown ids fall into a trailing "other" group.
		for _, group := range append(doctorGroups, struct {
			key   string
			label string
			ids   []string
		}{key: "other", label: "doctor_group_other"}) {
			checks := d.checksInGroup(group.key)
			if len(checks) == 0 {
				continue
			}
			ui.Text(c, d.tr(group.label)).FontSize(12).Bold().TextColor(palette.ink)
			for _, check := range checks {
				doctorCheckRow(c, d, check)
			}
		}
	})
	d.logPanel(c)
}

// checksInGroup returns the checks belonging to one group.
func (d *desktop) checksInGroup(group string) []command.DoctorCheck {
	out := []command.DoctorCheck{}
	if d.doctor == nil {
		return out
	}
	for _, check := range d.doctor.Checks {
		if groupOf(check.ID) == group {
			out = append(out, check)
		}
	}
	return out
}

// runDoctor executes the checks and records when they ran.
// runDoctor executes the checks synchronously. The UI path uses runAsync instead
// so the window keeps responding; this stays for tests and one-shot callers.
func (d *desktop) runDoctor() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := d.service.Execute(ctx, command.Request{Command: "doctor"})
	d.doctor = r.Doctor
	d.doctorAt = time.Now()
	if d.doctor == nil && r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
	}
}

// doctorCounter renders one PASS/WARN/FAIL tally.
func doctorCounter(c *ui.Context, label string, n int, tone ui.Color) {
	ui.Column(c).Gap(1).Padding(8, 12).Radius(8).Background(palette.canvas).Children(func() {
		ui.Text(c, fmt.Sprintf("%d", n)).FontSize(16).Bold().TextColor(tone)
		ui.Text(c, label).FontSize(11).TextColor(palette.muted)
	})
}

// doctorCheckRow renders one check with its level, localized title, result and —
// for a non-OK result — the suggestion the backend attached.
func doctorCheckRow(c *ui.Context, d *desktop, check command.DoctorCheck) {
	level := check.Level
	if check.Skipped {
		level = "skip"
	}
	tone := palette.accent
	if level == command.DoctorWarn || level == command.DoctorFail {
		tone = palette.warn
	}
	ui.Column(c).FillWidth().Padding(8, 12).Gap(2).Radius(7).Background(palette.canvas).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, d.tr("level_"+level)).FontSize(11).Bold().TextColor(tone)
			ui.Text(c, d.tr("doctor_"+check.ID)).FontSize(12).Bold().TextColor(palette.ink)
		})
		ui.Text(c, d.doctorDetail(check)).FontSize(12).TextColor(palette.muted)
		// The suggestion is what makes a warning actionable; it was previously
		// dropped entirely.
		if check.Hint != "" && level != command.DoctorOK && level != "skip" {
			ui.Text(c, d.tr("suggestion")+": "+d.doctorHint(check)).FontSize(11).TextColor(palette.muted)
		}
	})
}

// doctorHint localizes a backend hint when a translation exists.
func (d *desktop) doctorHint(check command.DoctorCheck) string {
	key := "hint_" + check.ID
	if d.tr(key) != key {
		return d.tr(key)
	}
	return check.Hint
}

// logPanel renders the in-memory log tail with a level filter.
func (d *desktop) logPanel(c *ui.Context) {
	panel(c, d.tr("recent_logs"), d.tr("recent_logs_detail"), func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.Button(c, d.tr("refresh_logs")).Clicked() {
				d.refreshLogs()
			}
			fieldLabel(c, d.tr("log_level"))
			// The filter starts at "any" so the dropdown shows a value instead of
			// rendering blank.
			if d.logLevel == "" {
				d.logLevel = d.tr("filter_any")
			}
			ui.Select(c, &d.logLevel, []string{d.tr("filter_any"), "INFO", "WARN", "ERROR"}).Width(120)
		})
		shown := d.filteredLogs()
		if len(shown) == 0 {
			ui.Text(c, d.tr("no_logs")).FontSize(12).TextColor(palette.muted)
			return
		}
		ui.Scroll(c).Height(180).Children(func() {
			ui.Column(c).FillWidth().Gap(2).Children(func() {
				for _, e := range shown {
					ui.Text(c, e.Time.Local().Format("15:04:05")+"  "+e.Level+"  "+e.Message).
						FontSize(11).TextColor(palette.muted).SingleLine()
				}
			})
		})
	})
}

// refreshLogs reloads the in-memory log tail.
func (d *desktop) refreshLogs() {
	r := d.service.Execute(context.Background(), command.Request{Command: "logs.tail", Limit: 200})
	if r.Error != "" {
		d.reportError(r.ErrorCode, r.Error)
		return
	}
	d.logEntries = r.Logs
}

// filteredLogs applies the level filter to the loaded entries.
func (d *desktop) filteredLogs() []logging.Entry {
	want := strings.ToUpper(strings.TrimSpace(d.logLevel))
	if want == "" || d.logLevel == d.tr("filter_any") {
		return d.logEntries
	}
	out := make([]logging.Entry, 0, len(d.logEntries))
	for _, e := range d.logEntries {
		if strings.EqualFold(e.Level, want) {
			out = append(out, e)
		}
	}
	return out
}
