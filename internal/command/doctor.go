package command

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// Doctor levels mirror the exit-code contract in docs/FEATURES.md C03:
// FAIL means the agent cannot work, WARN means degraded but usable.
const (
	DoctorOK   = "ok"
	DoctorWarn = "warn"
	DoctorFail = "fail"
)

type DoctorCheck struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Level   string `json:"level"`
	Detail  string `json:"detail"`
	Hint    string `json:"hint,omitempty"`
	Skipped bool   `json:"skipped,omitempty"`
}

type DoctorReport struct {
	Version    string        `json:"version"`
	OS         string        `json:"os"`
	Arch       string        `json:"arch"`
	DataDir    string        `json:"data_dir"`
	CheckedAt  string        `json:"checked_at"`
	Checks     []DoctorCheck `json:"checks"`
	WorstLevel string        `json:"worst_level"`
}

// doctor performs read-only local checks. It never activates Office, never
// contacts the remote task server, never submits a print job and never mutates
// configuration.
func (s Service) doctor(ctx context.Context, r *Response) error {
	report := DoctorReport{
		Version:   s.version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		DataDir:   s.Store.Dir(),
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
		Checks:    []DoctorCheck{},
	}
	add := func(id, name, level, detail, hint string) {
		report.Checks = append(report.Checks, DoctorCheck{ID: id, Name: name, Level: level, Detail: detail, Hint: hint})
	}
	skip := func(id, name, detail string) {
		report.Checks = append(report.Checks, DoctorCheck{ID: id, Name: name, Level: DoctorOK, Detail: detail, Skipped: true})
	}

	cfg := s.Store.Config()

	// Configuration validity.
	if err := cfg.Validate(); err != nil {
		add("config", "Configuration", DoctorFail, err.Error(), "run `print-bridge config validate` after fixing config.json")
	} else {
		add("config", "Configuration", DoctorOK, "configuration is valid", "")
	}

	// Data directory writability: the agent writes config and SQLite here.
	if err := ensureWritable(s.Store.Dir()); err != nil {
		add("data_dir", "Data directory", DoctorFail, err.Error(), "grant the running user write access to the data directory")
	} else {
		add("data_dir", "Data directory", DoctorOK, "writable: "+s.Store.Dir(), "")
	}

	// Listener port availability. A port already used by this agent is normal,
	// so only report a conflict when the port is bound by something else.
	if portInUse(cfg.Service.Port) {
		if agent, ok := s.Agent.(interface{ ListeningPort() int }); ok && agent.ListeningPort() == cfg.Service.Port {
			add("port", "Listener port", DoctorOK, fmt.Sprintf("port %d is served by the running agent", cfg.Service.Port), "")
		} else {
			add("port", "Listener port", DoctorWarn,
				fmt.Sprintf("port %d is already in use", cfg.Service.Port),
				"pick another port with `print-bridge service set-port <port>`; the agent will not stop the other process")
		}
	} else {
		add("port", "Listener port", DoctorOK, fmt.Sprintf("port %d is free", cfg.Service.Port), "")
	}

	// Default printer must exist in the system printer list.
	names, err := s.Printer.List(ctx)
	switch {
	case err != nil:
		add("printers", "Printers", DoctorFail, "cannot enumerate printers: "+err.Error(), "check the print spooler / CUPS service and permissions")
	case len(names) == 0:
		add("printers", "Printers", DoctorFail, "no printers are installed", "install at least one printer before sending jobs")
	case cfg.Printing.DefaultPrinter == "":
		add("printers", "Printers", DoctorWarn,
			fmt.Sprintf("%d printer(s) found, no default configured", len(names)),
			"set one with `print-bridge printer set-default \"<name>\"`")
	default:
		found := false
		for _, n := range names {
			if n == cfg.Printing.DefaultPrinter {
				found = true
				break
			}
		}
		if found {
			add("printers", "Printers", DoctorOK, fmt.Sprintf("default printer %q is available", cfg.Printing.DefaultPrinter), "")
		} else {
			add("printers", "Printers", DoctorFail,
				fmt.Sprintf("configured default printer %q is not installed", cfg.Printing.DefaultPrinter),
				"choose an installed printer with `print-bridge printer set-default`")
		}
	}

	// Default paper.
	if cfg.Printing.DefaultPaper == nil {
		add("paper", "Default paper", DoctorWarn, "no default paper configured",
			"set one with `print-bridge paper set <width_mm> <height_mm>`; file jobs without paper will be rejected")
	} else if !cfg.Printing.DefaultPaper.Valid() {
		add("paper", "Default paper", DoctorFail, "default paper dimensions are invalid", "set valid dimensions with `print-bridge paper set`")
	} else {
		add("paper", "Default paper", DoctorOK,
			fmt.Sprintf("%.1f x %.1f mm", cfg.Printing.DefaultPaper.WidthMM, cfg.Printing.DefaultPaper.HeightMM), "")
	}

	// Security allowlists. Empty origins means browsers cannot connect at all.
	if len(cfg.Security.AllowedOrigins) == 0 {
		add("origins", "Allowed origins", DoctorWarn, "no website origin is allowed; browser clients will be rejected",
			"add the business site with `print-bridge origin add https://example.com`")
	} else {
		add("origins", "Allowed origins", DoctorOK, fmt.Sprintf("%d origin(s) allowed", len(cfg.Security.AllowedOrigins)), "")
	}
	if len(cfg.Security.AllowedIPs) == 0 {
		add("ips", "Allowed client IPs", DoctorFail, "no client IP is allowed; every WebSocket handshake will be rejected",
			"add at least 127.0.0.1 with `print-bridge ip add 127.0.0.1`")
	} else {
		add("ips", "Allowed client IPs", DoctorOK, fmt.Sprintf("%d entr(y/ies) allowed", len(cfg.Security.AllowedIPs)), "")
	}

	// HTML renderer. Only discovery: launching a browser would violate doctor's
	// read-only contract.
	if path, err := FindChromium(); err != nil {
		add("renderer", "HTML renderer", DoctorWarn, err.Error(),
			"install Microsoft Edge, Google Chrome or Chromium; HTML and raw-html jobs will fail")
	} else {
		add("renderer", "HTML renderer", DoctorOK, path, "")
	}

	// Office converters: report the candidate order without activating COM.
	for _, format := range []string{"docx", "xlsx", "pptx"} {
		level, detail, hint := officeProbe(format)
		add("office."+format, "Office "+format, level, detail, hint)
	}

	// Remote configuration completeness.
	if cfg.Remote.Enabled {
		if cfg.Remote.EndpointURL == "" {
			add("remote", "Remote polling", DoctorFail, "remote polling is enabled without an endpoint",
				"set the endpoint or disable remote polling")
		} else {
			add("remote", "Remote polling", DoctorOK, "configured for "+cfg.Remote.EndpointURL,
				"doctor does not contact the remote server; use `print-bridge test-remote`")
		}
	} else {
		add("remote", "Remote polling", DoctorOK, "disabled", "")
	}
	if cfg.Remote.HistoryRetentionDays <= 0 {
		add("retention", "History retention", DoctorWarn, "history retention is not configured; history will grow without bound",
			"set remote.history_retention_days to a positive number of days")
	} else {
		add("retention", "History retention", DoctorOK, fmt.Sprintf("%d day(s)", cfg.Remote.HistoryRetentionDays), "")
	}

	// Offline mode reporting: doctor can run while the agent is stopped.
	if s.Agent == nil {
		skip("agent", "Agent", "not running; doctor ran offline checks only")
	} else {
		add("agent", "Agent", DoctorOK, fmt.Sprintf("%d job(s) in the in-memory queue", len(s.Agent.ActiveJobs())), "")
	}

	report.WorstLevel = worstLevel(report.Checks)
	r.Doctor = &report
	r.Status = report.WorstLevel
	if report.WorstLevel == DoctorFail {
		return domain.NewProtocolError(domain.CodeDoctorFailed, "doctor found failing checks")
	}
	return nil
}

func worstLevel(checks []DoctorCheck) string {
	worst := DoctorOK
	for _, c := range checks {
		switch c.Level {
		case DoctorFail:
			return DoctorFail
		case DoctorWarn:
			worst = DoctorWarn
		}
	}
	return worst
}

func ensureWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// portInUse reports whether the listener port can still be bound.
func portInUse(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", fmt.Sprint(port)))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

// version is what the doctor report names: the version the caller stamped, else
// PRINTBRIDGE_VERSION, else a development build.
func (s Service) version() string {
	if s.Version != "" {
		return s.Version
	}
	return version()
}

func version() string {
	if v := os.Getenv("PRINTBRIDGE_VERSION"); v != "" {
		return v
	}
	return "dev"
}

// FindChromium mirrors the browser discovery order used for HTML rendering, so
// doctor reports the same renderer a real job would pick.
func FindChromium() (string, error) {
	var names []string
	if runtime.GOOS == "windows" {
		names = []string{"msedge.exe", "chrome.exe", "chromium.exe"}
	} else {
		names = []string{"chrome", "chromium", "chromium-browser"}
	}
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	switch runtime.GOOS {
	case "windows":
		for _, root := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
			if root == "" {
				continue
			}
			for _, rel := range []string{`Microsoft\Edge\Application\msedge.exe`, `Google\Chrome\Application\chrome.exe`} {
				p := filepath.Join(root, rel)
				if _, err := os.Stat(p); err == nil {
					return p, nil
				}
			}
		}
	case "darwin":
		for _, p := range []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		} {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("no Chromium-family browser found")
}

// officeProbe reports which converter would be tried first. It is deliberately
// passive: it checks for installed executables or registry-registered ProgIDs
// without launching Office, WPS or LibreOffice.
func officeProbe(format string) (level, detail, hint string) {
	if runtime.GOOS == "windows" {
		if progID := firstOfficeProgID(format); progID != "" {
			return DoctorOK, "COM converter registered: " + progID, ""
		}
	}
	if tool := findLibreOffice(); tool != "" {
		return DoctorOK, "LibreOffice: " + tool, ""
	}
	if runtime.GOOS == "windows" {
		return DoctorWarn, "no Microsoft Office, WPS Office or LibreOffice converter detected",
			"install one of them; " + format + " jobs will fail with OFFICE_CONVERT_FAILED"
	}
	return DoctorWarn, "LibreOffice is not installed",
		"install LibreOffice; " + format + " jobs will fail with OFFICE_CONVERT_FAILED"
}

func findLibreOffice() string {
	for _, name := range []string{"libreoffice", "soffice"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	if runtime.GOOS == "windows" {
		for _, root := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)")} {
			if root == "" {
				continue
			}
			p := filepath.Join(root, "LibreOffice", "program", "soffice.exe")
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// officeProgramNames returns the whole converter order for a format so CLI help
// and doctor output stay consistent with the conversion implementation.
func officeProgramNames(format string) []string {
	switch strings.ToLower(format) {
	case "docx":
		return []string{"Word.Application", "KWPS.Application"}
	case "xlsx":
		return []string{"Excel.Application", "KET.Application"}
	case "pptx":
		return []string{"PowerPoint.Application", "KWPP.Application"}
	}
	return nil
}
