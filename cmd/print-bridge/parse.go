package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
)

// parseCommand turns argv into a typed request plus the online/offline policy.
// Every path validates its own arity so `exitUsage` is reported instead of a
// panic or a silently ignored argument.
func parseCommand(args []string) (command.Request, offlinePolicy, error) {
	req := command.Request{ProtocolVersion: 1, RequestID: newRequestID()}
	fail := func(format string, a ...any) (command.Request, offlinePolicy, error) {
		return req, onlineOnly, domain.NewProtocolErrorf(domain.CodeArgumentInvalid, format, a...)
	}

	switch args[0] {
	case "status":
		req.Command = "status"
		return req, onlineOnly, nil

	case "config":
		if len(args) == 1 {
			req.Command = "config.get"
			return req, onlinePreferred, nil
		}
		switch args[1] {
		case "validate":
			// `config validate [path]` must actually VALIDATE. Mapping it to
			// config.get made it a no-op that printed the live config and exited
			// 0 even for a missing or corrupt file. With a path it validates that
			// file offline; without one it validates the live configuration.
			if len(args) > 3 {
				return fail("usage: config validate [<file>]")
			}
			req.Command = "config.validate"
			if len(args) == 3 {
				req.Text = args[2]
			}
			return req, offlineAllowed, nil
		case "export", "import":
			// Config transfer is handled outside the command service because it
			// needs the encryption password, not just typed config values.
			return req, offlineAllowed, errConfigTransferRedirect{action: args[1], rest: args[2:]}
		default:
			return fail("unknown config subcommand: %s", args[1])
		}

	case "printer":
		switch {
		case len(args) == 1:
			req.Command = "printers.list"
			return req, onlinePreferred, nil
		case len(args) == 2 && args[1] == "set-default":
			req.Command = "printers.default"
			return req, onlinePreferred, nil
		case len(args) == 3 && args[1] == "set-default":
			req.Command = "printers.default"
			req.Name = args[2]
			return req, onlinePreferred, nil
		case len(args) == 2:
			req.Command = "printers.show"
			req.Name = args[1]
			return req, onlinePreferred, nil
		default:
			return fail("usage: printer [<name> | set-default <name>]")
		}

	case "paper":
		switch {
		case len(args) == 1:
			req.Command = "config.get"
			return req, onlinePreferred, nil
		case len(args) == 4 && args[1] == "set":
			w, err := strconv.ParseFloat(args[2], 64)
			if err != nil {
				return fail("invalid width: %s", args[2])
			}
			h, err := strconv.ParseFloat(args[3], 64)
			if err != nil {
				return fail("invalid height: %s", args[3])
			}
			paper := domain.Paper{WidthMM: w, HeightMM: h}
			if !paper.Valid() {
				return fail("paper dimensions must be positive and at most 2000 mm")
			}
			req.Command = "config.set-default-paper"
			req.Paper = &paper
			return req, onlinePreferred, nil
		default:
			return fail("usage: paper | paper set <width_mm> <height_mm>")
		}

	case "test-print":
		if len(args) != 2 {
			return fail("usage: test-print \"<printer name>\"")
		}
		req.Command = "test.print"
		req.Name = args[1]
		return req, onlineOnly, nil

	case "service":
		switch {
		case len(args) == 1:
			req.Command = "config.get"
			return req, onlinePreferred, nil
		case len(args) == 3 && args[1] == "set-port":
			port, err := strconv.Atoi(args[2])
			// The floor matches the CLI. Below it the chosen port sits in
			// the dynamic range, where the listener competes with outgoing
			// connections instead of staying predictable.
			if err != nil || port < domain.MinServicePort || port > domain.MaxServicePort {
				return fail("invalid port: %s (must be %d..%d)", args[2], domain.MinServicePort, domain.MaxServicePort)
			}
			req.Command = "service.set-port"
			req.Port = port
			return req, onlinePreferred, nil
		default:
			return fail("usage: service | service set-port <port>")
		}

	case "origin", "ip":
		field := "allowed_origins"
		if args[0] == "ip" {
			field = "allowed_ips"
		}
		switch {
		case len(args) == 1:
			req.Command = "config.get"
			return req, onlinePreferred, nil
		case len(args) == 3 && (args[1] == "add" || args[1] == "delete"):
			req.Command = "config." + args[1] + "-" + field
			req.Entry = args[2]
			return req, onlinePreferred, nil
		default:
			return fail("usage: %s [add|delete \"<value>\"]", args[0])
		}

	case "task":
		switch {
		case len(args) == 1:
			req.Command = "task.list"
			req.Limit = 50
			return req, onlinePreferred, nil
		case len(args) == 2 && args[1] == "clear":
			req.Command = "task.clear"
			return req, onlinePreferred, nil
		case len(args) == 3 && args[1] == "cancel":
			req.Command = "task.cancel"
			req.JobID = args[2]
			return req, onlineOnly, nil
		case len(args) == 2:
			req.Command = "task.show"
			req.JobID = args[1]
			return req, onlinePreferred, nil
		default:
			return fail("usage: task | task \"<job_id>\" | task cancel \"<job_id>\" | task clear")
		}

	case "history":
		if len(args) >= 2 && args[1] == "prune" {
			req.Command = "history.prune"
			fs := flag.NewFlagSet("history prune", flag.ContinueOnError)
			fs.SetOutput(os.Stderr)
			days := fs.Int("days", 0, "retention window in days")
			if err := fs.Parse(args[2:]); err != nil {
				return fail("%v", err)
			}
			req.Days = *days
			return req, onlinePreferred, nil
		}
		if len(args) >= 2 && args[1] == "export" {
			return parseHistoryExport(req, args, fail)
		}
		return fail("usage: history prune [--days N] | history export <file> [--format csv|json] [--days N] [--status S] [--query Q]")

	case "doctor":
		req.Command = "doctor"
		// Doctor is explicitly offline-capable: it must work when the agent is
		// stopped, which is exactly when users need it.
		return req, onlinePreferred, nil

	case "logs":
		req.Command = "logs.tail"
		fs := flag.NewFlagSet("logs", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		level := fs.String("level", "", "minimum level: DEBUG, INFO, WARN, ERROR")
		since := fs.String("since", "", "RFC3339 timestamp")
		limit := fs.Int("limit", 100, "maximum recent entries (1..200)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return fail("usage: logs [--level LEVEL] [--since RFC3339] [--limit N]")
		}
		if *limit < 1 || *limit > 200 {
			return fail("logs limit must be 1..200")
		}
		if *since != "" {
			if _, err := time.Parse(time.RFC3339, *since); err != nil {
				return fail("logs since must be RFC3339")
			}
		}
		req.Level, req.Since, req.Limit = *level, *since, *limit
		return req, onlineOnly, nil

	case "remote":
		return parseRemote(req, args, fail)

	case "test-remote":
		if len(args) != 1 {
			return fail("usage: test-remote")
		}
		req.Command = "remote.test"
		return req, onlineOnly, nil

	default:
		return fail("unknown command: %s (run `print-bridge help`)", strings.Join(args, " "))
	}
}

type failFn func(format string, a ...any) (command.Request, offlinePolicy, error)

// parseHistoryExport handles `history export <file> [flags]`.
//
// The path is positional and the filters are flags, so `history export out.csv
// --days 7` reads naturally. The format defaults to csv and is validated here
// rather than at write time, so a typo fails before the agent does any work.
func parseHistoryExport(req command.Request, args []string, fail failFn) (command.Request, offlinePolicy, error) {
	fs := flag.NewFlagSet("history export", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	format := fs.String("format", "csv", "export format: csv or json")
	days := fs.Int("days", 0, "export only jobs updated in the last N days")
	status := fs.String("status", "", "export only jobs whose latest status matches")
	query := fs.String("query", "", "export only jobs whose id contains this text")
	limit := fs.Int("limit", 0, "export at most N rows")
	positional, err := splitFlags(args[2:])
	if err != nil {
		return fail("%v", err)
	}
	if err := fs.Parse(positional.flagArgs); err != nil {
		return fail("%v", err)
	}
	if len(positional.files) != 1 {
		return fail("usage: history export <file> [--format csv|json] [--days N] [--status S] [--query Q] [--limit N]")
	}
	normalized := strings.ToLower(strings.TrimSpace(*format))
	if normalized != "csv" && normalized != "json" {
		return fail("history export format must be csv or json")
	}
	if *days < 0 {
		return fail("history export --days must not be negative")
	}
	if *limit < 0 || *limit > 50000 {
		return fail("history export --limit must be 0..50000")
	}
	req.Command = "history.export"
	req.Format = normalized
	req.Days = *days
	req.Status = *status
	req.Query = *query
	req.Limit = *limit
	return req, onlinePreferred, errHistoryExportRedirect{path: positional.files[0]}
}

// errHistoryExportRedirect marks a command whose response bytes the CLI writes to a
// path. The command service produces the content; only the CLI knows the file.
type errHistoryExportRedirect struct {
	path string
}

func (e errHistoryExportRedirect) Error() string { return "history export writes " + e.path }

func parseRemote(req command.Request, args []string, fail failFn) (command.Request, offlinePolicy, error) {
	req.Command = "config.get"
	if len(args) == 1 {
		return req, onlinePreferred, nil
	}
	setter := func(cmd string, value string) (command.Request, offlinePolicy, error) {
		req.Command = cmd
		req.Text = value
		return req, onlinePreferred, nil
	}
	switch args[1] {
	case "enable":
		req.Command = "remote.set-enabled"
		req.Enable = true
		return req, onlinePreferred, nil
	case "disable":
		req.Command = "remote.set-enabled"
		req.Enable = false
		return req, onlinePreferred, nil
	case "generate-device-id":
		req.Command = "remote.generate-device-id"
		return req, onlinePreferred, nil
	}
	if len(args) != 3 {
		return fail("usage: remote [enable|disable|set-url|set-token|set-device-id|set-device-name|set-interval|set-max-retries|generate-device-id|dead-letters]")
	}
	switch args[1] {
	case "set-url":
		if !strings.HasPrefix(args[2], "https://") {
			return fail("remote endpoint must be an https:// URL")
		}
		return setter("remote.set-url", args[2])
	case "set-token":
		return setter("remote.set-token", args[2])
	case "set-device-id":
		return setter("remote.set-device-id", args[2])
	case "set-device-name":
		return setter("remote.set-device-name", args[2])
	case "set-interval":
		n, err := strconv.Atoi(args[2])
		if err != nil || n < domain.MinRemotePollIntervalSeconds {
			return fail("invalid interval: %s (must be at least %d seconds)", args[2], domain.MinRemotePollIntervalSeconds)
		}
		req.Command = "remote.set-interval"
		req.Limit = n
		return req, onlinePreferred, nil
	case "set-max-retries":
		n, err := strconv.Atoi(args[2])
		if err != nil || n < domain.MinRemoteMaxReportRetries {
			return fail("invalid retry count: %s (must be at least %d)", args[2], domain.MinRemoteMaxReportRetries)
		}
		req.Command = "remote.set-max-retries"
		req.Limit = n
		return req, onlinePreferred, nil
	case "dead-letters":
		return parseDeadLetters(req, args, fail)
	default:
		return fail("unknown remote subcommand: %s", args[1])
	}
}

// parseDeadLetters handles `remote dead-letters [list|retry|delete|purge]`.
//
// The destructive "all" form is a separate subcommand so it can never be reached by
// omitting an argument.
func parseDeadLetters(req command.Request, args []string, fail failFn) (command.Request, offlinePolicy, error) {
	if len(args) == 2 {
		req.Command = "remote.dead-letters"
		req.Limit = 50
		return req, onlinePreferred, nil
	}
	switch args[2] {
	case "list":
		req.Command = "remote.dead-letters"
		req.Limit = 50
		if len(args) == 5 && args[3] == "--limit" {
			n, err := strconv.Atoi(args[4])
			if err != nil || n < 1 || n > 1000 {
				return fail("dead-letters limit must be 1..1000")
			}
			req.Limit = n
		} else if len(args) != 3 {
			return fail("usage: remote dead-letters list [--limit N]")
		}
		return req, onlinePreferred, nil
	case "retry":
		req.Command = "remote.retry-dead-letters"
		if len(args) < 4 {
			return fail("usage: remote dead-letters retry <event_id>...")
		}
		req.EventIDs = append([]string{}, args[3:]...)
		return req, onlinePreferred, nil
	case "delete":
		req.Command = "remote.delete-dead-letters"
		if len(args) < 4 {
			return fail("usage: remote dead-letters delete <event_id>...")
		}
		req.EventIDs = append([]string{}, args[3:]...)
		return req, onlinePreferred, nil
	case "purge":
		if len(args) != 3 {
			return fail("usage: remote dead-letters purge")
		}
		req.Command = "remote.purge-dead-letters"
		return req, onlinePreferred, nil
	default:
		return fail("unknown dead-letters subcommand: %s", args[2])
	}
}

// errConfigTransferRedirect marks a command handled outside the command service.
type errConfigTransferRedirect struct {
	action string
	rest   []string
}

func (e errConfigTransferRedirect) Error() string { return "config " + e.action }

func isConfigTransferRedirect(err error) (errConfigTransferRedirect, bool) {
	var target errConfigTransferRedirect
	if errors.As(err, &target) {
		return target, true
	}
	return target, false
}

// isHistoryExportRedirect reports whether the parse result carries an export path.
func isHistoryExportRedirect(err error) (errHistoryExportRedirect, bool) {
	var target errHistoryExportRedirect
	if errors.As(err, &target) {
		return target, true
	}
	return target, false
}

func newRequestID() string { return globalUUID() }

var _ = fmt.Sprintf
