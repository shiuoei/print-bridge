package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/ipc"
	"github.com/shiuoei/print-bridge/internal/printer"
	"github.com/shiuoei/print-bridge/internal/store"
	"github.com/shiuoei/print-bridge/internal/transfer"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run maps every outcome onto the documented exit codes instead of collapsing
// all failures into a generic 1.
func run(args []string) int {
	err := dispatch(args)
	if err == nil {
		return exitOK
	}
	fmt.Fprintln(os.Stderr, err)

	var pe *domain.ProtocolError
	if errors.As(err, &pe) {
		switch pe.Code {
		case domain.CodeDoctorFailed:
			return exitDoctorFail
		case domain.CodeRemoteUnavailable, domain.CodePrintFailed:
			return exitDoctorFail
		case domain.CodeAgentNotRunning:
			return exitAgentAbsent
		case domain.CodePermission:
			return exitPermission
		case domain.CodeServicePortInUse, domain.CodeConflict:
			return exitConflict
		case domain.CodeArgumentInvalid, domain.CodeInvalidMessage, domain.CodeUnsupportedCommand:
			return exitUsage
		}
	}
	return exitUsage
}

// offlinePolicy describes how a command may run when the agent is not reachable
// over IPC. A permission or protocol failure must never be treated as "agent not
// running", otherwise the CLI silently writes a second copy of live state.
type offlinePolicy int

const (
	// onlineOnly commands need the live agent and refuse to run offline.
	onlineOnly offlinePolicy = iota
	// onlinePreferred commands use the agent when present and fall back to the
	// offline executor only when the agent is provably absent.
	onlinePreferred
	// offlineAllowed commands are safe against a closed database.
	offlineAllowed
)

func dispatch(args []string) error {
	dir, args, err := resolveDataDir(args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		printUsage(os.Stdout)
		return domain.NewProtocolError(domain.CodeArgumentInvalid, "no command given")
	}

	switch args[0] {
	case "help", "--help", "-h":
		printUsage(os.Stdout)
		return nil
	case "version", "--version":
		fmt.Println(versionString())
		return nil
	}

	req, policy, err := parseCommand(args)
	if err != nil {
		if redirect, ok := isConfigTransferRedirect(err); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			return runConfigTransfer(ctx, dir, redirect.action, redirect.rest)
		}
		if redirect, ok := isHistoryExportRedirect(err); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			return runHistoryExport(ctx, dir, req, redirect.path)
		}
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	switch policy {
	case onlineOnly:
		resp, err := ipc.Call(ctx, dir, req)
		if err != nil {
			return domain.NewProtocolError(domain.CodeAgentNotRunning, "agent is not running; start PrintBridge first").WithCause(err)
		}
		if resp.RemoteTest != nil {
			if err := render(resp, jsonOutput()); err != nil {
				return err
			}
		}
		if rerr := responseError(resp); rerr != nil {
			return rerr
		}
		if resp.RemoteTest != nil {
			return nil
		}
		return render(resp, jsonOutput())
	default:
		resp, err := ipc.Call(ctx, dir, req)
		if err == nil {
			if rerr := responseError(resp); rerr != nil {
				// A validation failure from a live agent is authoritative: falling
				// back would hide the real error and could duplicate a write.
				return rerr
			}
			return render(resp, jsonOutput())
		}
		if !agentAbsent(err) {
			return domain.NewProtocolError(domain.CodeInternalError, "agent IPC failed").WithCause(err)
		}
		return runOffline(ctx, dir, req)
	}
}

// agentAbsent distinguishes "no agent is listening" from real IPC failures such
// as a permission denial or a protocol mismatch.
func agentAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

func resolveDataDir(args []string) (string, []string, error) {
	if len(args) >= 2 && args[0] == "--data-dir" {
		return args[1], args[2:], nil
	}
	dir, err := store.DefaultDir()
	if err != nil {
		return "", nil, err
	}
	return dir, args, nil
}

func jsonOutput() bool {
	switch strings.ToLower(os.Getenv("PRINTBRIDGE_OUTPUT")) {
	case "json":
		return true
	}
	for _, a := range os.Args[1:] {
		if a == "--json" {
			return true
		}
	}
	return false
}

// runOffline executes the commands that are safe without a live agent. Writes go
// through an exclusive Store open, which is only correct because the agent is
// provably absent (see the P0 note in docs/PROGRESS.md).
func runOffline(ctx context.Context, dir string, req command.Request) error {
	s, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer s.Close()
	svc := command.Service{Store: s, Printer: printer.New()}
	resp := svc.Execute(ctx, req)
	if rerr := responseError(resp); rerr != nil {
		return rerr
	}
	return render(resp, jsonOutput())
}

func responseError(resp command.Response) error {
	if resp.Error == "" {
		return nil
	}
	code := resp.ErrorCode
	if code == "" {
		code = domain.CodeInternalError
	}
	return domain.NewProtocolError(code, resp.Error)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func render(resp command.Response, asJSON bool) error {
	if asJSON {
		return printJSON(resp)
	}
	switch {
	case resp.Doctor != nil:
		printDoctor(resp.Doctor)
	case resp.RemoteTest != nil:
		for _, p := range []command.RemoteProbe{resp.RemoteTest.GET, resp.RemoteTest.POST} {
			fmt.Printf("%s: HTTP %d (%d ms)", p.Method, p.StatusCode, p.DurationMS)
			if p.Error != "" {
				fmt.Printf(" - %s", p.Error)
			}
			if p.Response != "" {
				fmt.Printf(" - %s", p.Response)
			}
			fmt.Println()
		}
	case resp.Logs != nil:
		for _, e := range resp.Logs {
			fmt.Printf("%s  %-5s %s\n", e.Time.Local().Format("2006-01-02 15:04:05"), e.Level, e.Message)
		}
	case resp.DeadLetters != nil:
		printDeadLetters(resp.DeadLetters)
	case resp.RemoteStats != nil:
		fmt.Printf("pending=%d dead=%d\n", resp.RemoteStats.Pending, resp.RemoteStats.Dead)
	case resp.Printer != nil:
		printPrinterInfo(resp.Printer)
	case resp.Printers != nil:
		for _, name := range resp.Printers {
			fmt.Println(name)
		}
	case resp.Jobs != nil:
		printJobs(resp.Jobs, resp.Active)
	case resp.History != nil:
		printHistory(resp.History)
	case resp.Config != nil:
		return printConfig(resp.Config)
	case resp.Removed != 0:
		fmt.Printf("removed %d row(s)\n", resp.Removed)
	case resp.Value != "":
		fmt.Println(resp.Value)
	case resp.Message != "":
		fmt.Println(resp.Message)
	case resp.Status != "":
		fmt.Println(resp.Status)
	}
	return nil
}

func printConfig(c *domain.Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// printDeadLetters renders the abandoned status reports in a compact table. The
// event id is printed in full because it is the argument retry and delete take.
func printDeadLetters(letters []store.DeadLetter) {
	if len(letters) == 0 {
		fmt.Println("no dead letters")
		return
	}
	for _, d := range letters {
		fmt.Printf("%s  job=%s  status=%s  attempts=%d  dead_at=%s\n",
			d.EventID, d.JobID, d.Status, d.Attempts, d.DeadAt.Local().Format("2006-01-02 15:04:05"))
		if d.LastError != "" {
			fmt.Printf("    last error: %s\n", d.LastError)
		}
		if d.Message != "" {
			fmt.Printf("    message: %s\n", d.Message)
		}
	}
	fmt.Printf("\n%d dead letter(s); retry with `remote dead-letters retry <event_id>`\n", len(letters))
}

func printHistory(history []domain.Status) {
	for _, st := range history {
		at := st.At.Local().Format("2006-01-02 15:04:05")
		fmt.Printf("%s  %-12s %-14s %s\n", at, st.Status, st.JobID, st.Message)
	}
}

func printJobs(jobs []store.JobSummary, active []domain.Status) {
	if len(active) > 0 {
		fmt.Printf("Agent queue (%d active):\n", len(active))
		for _, st := range active {
			fmt.Printf("  %-12s %-14s %s\n", st.Status, st.JobID, st.Message)
		}
		fmt.Println()
	}
	if len(jobs) == 0 {
		fmt.Println("no history")
		return
	}
	fmt.Printf("%-14s %-12s %-20s %s\n", "JOB ID", "STATUS", "UPDATED", "EVENTS")
	for _, j := range jobs {
		fmt.Printf("%-14s %-12s %-20s %d\n", j.JobID, j.LatestStatus, j.UpdatedAt.Local().Format("2006-01-02 15:04:05"), j.Events)
	}
}

func printDoctor(report *command.DoctorReport) {
	fmt.Printf("PrintBridge %s  (%s/%s)\n", report.Version, report.OS, report.Arch)
	fmt.Printf("data directory: %s\n\n", report.DataDir)
	for _, c := range report.Checks {
		mark := map[string]string{command.DoctorOK: "OK  ", command.DoctorWarn: "WARN", command.DoctorFail: "FAIL"}[c.Level]
		if c.Skipped {
			mark = "SKIP"
		}
		fmt.Printf("[%s] %-18s %s\n", mark, c.Name, c.Detail)
		if c.Hint != "" {
			fmt.Printf("       hint: %s\n", c.Hint)
		}
	}
	fmt.Printf("\nresult: %s\n", strings.ToUpper(report.WorstLevel))
}

func printPrinterInfo(info *domain.PrinterInfo) {
	fmt.Printf("%s\n", info.Name)
	if info.IsDefault != nil && *info.IsDefault {
		fmt.Println("  default: yes")
	}
	if info.Port != nil && *info.Port != "" {
		fmt.Printf("  port: %s\n", *info.Port)
	}
	if info.DPI != nil {
		fmt.Printf("  dpi: %d\n", *info.DPI)
	}
	fmt.Printf("  papers: %d\n", len(info.Papers))
	for _, p := range info.Papers {
		fmt.Printf("    %-8s %-38s %.1f x %.1f mm\n", p.ID, p.Name, p.WidthMM, p.HeightMM)
	}
	if len(info.Trays) > 0 {
		fmt.Printf("  trays: %d\n", len(info.Trays))
		for _, t := range info.Trays {
			fmt.Printf("    %-8s %s\n", t.ID, t.Name)
		}
	}
	if len(info.MediaTypes) > 0 {
		fmt.Printf("  media types: %d\n", len(info.MediaTypes))
		for _, m := range info.MediaTypes {
			fmt.Printf("    %-8s %s\n", m.ID, m.Name)
		}
	}
}

// printUsage documents every command together with the online/offline rules, so
// users are not surprised by which commands need a running agent.
func printUsage(w io.Writer) {
	fmt.Fprint(w, `PrintBridge CLI

Usage: print-bridge [--data-dir <dir>] [--json] <command> [args]

Printer and paper
  printer                      list system printers
  printer "<name>"             show printer capabilities
  printer set-default "<name>" set the printer used when a job omits one
  paper                        show the default paper
  paper set <width_mm> <height_mm>
                               set the default paper
  test-print "<name>"          submit a test page            (agent required)

Service and security
  service                      show the listener port
  service set-port <port>      change the listener port
  origin                       list allowed website origins
  origin add "<origin>"        allow a browser origin
  origin delete "<origin>"     remove an allowed origin
  ip                           list allowed client IPs/CIDRs
  ip add "<ip|cidr>"           allow a client address
  ip delete "<ip|cidr>"        remove an allowed address
  config                       print the effective configuration
  config validate              validate the configuration file
  config export <file> --password-file <file> [--only <field> ...]
  config import <file> --password-file <file> [--preview|--yes]

Tasks and diagnostics
  status                       agent status                 (agent required)
  task                         list history and active jobs
  task "<job_id>"              show one job timeline
  task cancel "<job_id>"       cancel a queued job          (agent required)
  history prune [--days N]     delete history older than N days
  history export <file>        export job history as csv (default) or json
                               [--format csv|json] [--days N] [--status S]
                               [--query Q] [--limit N]
  doctor [--json]              read-only environment check
  logs [--level LEVEL] [--since RFC3339] [--limit N]
                               recent agent log lines       (agent required)

Remote
  remote                       show remote polling settings
  remote enable|disable        toggle remote polling
  remote set-url "<url>"       set the HTTPS endpoint
  remote set-token "<token>"   set the bearer token
  remote set-device-id "<id>"  set the device identifier
  remote set-device-name "<n>" set the device name
  remote set-interval <sec>    set the poll interval
  remote set-max-retries <n>   set the report retry count
  remote generate-device-id    generate and store a UUID v4 device ID
  remote dead-letters [list]   list status reports that never reached the server
  remote dead-letters retry <event_id>...   queue them for another attempt
  remote dead-letters delete <event_id>...  discard the selected reports
  remote dead-letters purge    discard every dead letter
  test-remote                  probe GET and POST connectivity (agent required)

Exit codes
  0 success                          3 agent not running
  1 doctor reported FAIL             4 permission denied
  2 invalid arguments                5 port or resource conflict
`)
}

func versionString() string {
	if v := os.Getenv("PRINTBRIDGE_VERSION"); v != "" {
		return v
	}
	return "print-bridge dev"
}

var _ = strconv.Atoi
var _ = uuid.NewString
var _ = transfer.Export
var _ = strings.TrimSpace
