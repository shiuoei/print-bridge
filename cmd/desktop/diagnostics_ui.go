package main

import (
	"fmt"
	"strings"

	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
)

var errorTextKeys = map[string]string{
	domain.CodeServicePortInUse:      "error_port_in_use",
	domain.CodePrinterNotConfigured:  "error_printer_not_configured",
	domain.CodePrinterNotFound:       "error_printer_not_found",
	domain.CodePaperNotConfigured:    "error_paper_not_configured",
	domain.CodePaperNotFound:         "error_paper_not_found",
	domain.CodeConfigInvalid:         "error_config_invalid",
	domain.CodePermission:            "error_permission",
	domain.CodeReadOnly:              "error_read_only",
	domain.CodePrinterBackendFailure: "error_printer_backend",
	domain.CodeAgentNotRunning:       "error_agent_not_running",
	domain.CodeConflict:              "error_conflict",
	domain.CodeDoctorFailed:          "error_doctor",
}

func (d *desktop) errorSummary(code string) string {
	if key := errorTextKeys[code]; key != "" {
		return d.tr(key)
	}
	return d.tr("error_generic")
}

func (d *desktop) reportError(code, raw string) {
	d.message = d.errorSummary(code)
	d.errorDetail = raw
}

// Doctor's machine-readable IDs and levels drive the localized explanation.
// Keep the original detail for unexpected failures so a support engineer can
// still see the exact printer, OS, or converter error.
func (d *desktop) doctorDetail(check command.DoctorCheck) string {
	if d.language != "zh-CN" {
		return check.Detail
	}
	cfg := d.store.Config()
	switch check.ID {
	case "config":
		if check.Level == command.DoctorOK {
			return d.tr("doctor_detail_config_ok")
		}
	case "data_dir":
		if check.Level == command.DoctorOK {
			return fmt.Sprintf(d.tr("doctor_detail_data_ok"), d.store.Dir())
		}
	case "port":
		if check.Level == command.DoctorWarn {
			return fmt.Sprintf(d.tr("doctor_detail_port_busy"), cfg.Service.Port)
		}
		return fmt.Sprintf(d.tr("doctor_detail_port_ok"), cfg.Service.Port)
	case "printers":
		if strings.HasPrefix(check.Detail, "cannot enumerate printers:") {
			return fmt.Sprintf(d.tr("doctor_detail_raw"), check.Detail)
		}
		if len(d.printers) == 0 && check.Level == command.DoctorFail {
			return d.tr("doctor_detail_no_printers")
		}
		if cfg.Printing.DefaultPrinter == "" {
			return fmt.Sprintf(d.tr("doctor_detail_printer_missing_default"), len(d.printers))
		}
		if check.Level == command.DoctorOK {
			return fmt.Sprintf(d.tr("doctor_detail_printer_ok"), cfg.Printing.DefaultPrinter)
		}
		return fmt.Sprintf(d.tr("doctor_detail_printer_unavailable"), cfg.Printing.DefaultPrinter)
	case "paper":
		if cfg.Printing.DefaultPaper == nil {
			return d.tr("doctor_detail_no_paper")
		}
		if check.Level == command.DoctorOK {
			return fmt.Sprintf("%.1f × %.1f mm", cfg.Printing.DefaultPaper.WidthMM, cfg.Printing.DefaultPaper.HeightMM)
		}
	case "origins":
		if len(cfg.Security.AllowedOrigins) == 0 {
			return d.tr("doctor_detail_no_origins")
		}
		return fmt.Sprintf(d.tr("doctor_detail_origins_ok"), len(cfg.Security.AllowedOrigins))
	case "ips":
		if len(cfg.Security.AllowedIPs) == 0 {
			return d.tr("doctor_detail_no_ips")
		}
		return fmt.Sprintf(d.tr("doctor_detail_ips_ok"), len(cfg.Security.AllowedIPs))
	case "renderer":
		if check.Level == command.DoctorOK {
			return fmt.Sprintf(d.tr("doctor_detail_renderer_ok"), check.Detail)
		}
	case "office.docx", "office.xlsx", "office.pptx":
		if check.Level == command.DoctorOK {
			return fmt.Sprintf(d.tr("doctor_detail_office_ok"), check.Detail)
		}
	case "remote":
		if !cfg.Remote.Enabled {
			return d.tr("doctor_detail_remote_off")
		}
		if cfg.Remote.EndpointURL == "" {
			return d.tr("doctor_detail_remote_missing_url")
		}
		return fmt.Sprintf(d.tr("doctor_detail_remote_ok"), cfg.Remote.EndpointURL)
	case "retention":
		if cfg.Remote.HistoryRetentionDays <= 0 {
			return d.tr("doctor_detail_no_retention")
		}
		return fmt.Sprintf(d.tr("doctor_detail_retention_ok"), cfg.Remote.HistoryRetentionDays)
	case "agent":
		if check.Skipped {
			return d.tr("doctor_detail_agent_off")
		}
		if d.agent != nil {
			return fmt.Sprintf(d.tr("doctor_detail_agent_jobs"), len(d.agent.ActiveJobs()))
		}
	}
	if strings.TrimSpace(check.Detail) == "" {
		return d.tr("doctor_detail_unknown")
	}
	return fmt.Sprintf(d.tr("doctor_detail_raw"), check.Detail)
}
