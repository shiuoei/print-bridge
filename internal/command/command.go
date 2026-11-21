package command

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/logging"
	"github.com/shiuoei/print-bridge/internal/printer"
	"github.com/shiuoei/print-bridge/internal/store"
)

// Request is the local management envelope shared by the in-process desktop UI
// and the CLI over IPC. It is not the WebSocket protocol: browser clients can
// never reach these commands.
type Request struct {
	ProtocolVersion int            `json:"protocol_version"`
	RequestID       string         `json:"request_id"`
	Command         string         `json:"command"`
	Config          *domain.Config `json:"config,omitempty"`
	Limit           int            `json:"limit,omitempty"`
	Offset          int            `json:"offset,omitempty"`

	// Name selects a printer for printer/show, printer/default, test-print.
	Name string `json:"name,omitempty"`
	// JobID selects a job for task/show and task/cancel.
	JobID string `json:"job_id,omitempty"`
	// Status filters task/list by latest status.
	Status string `json:"status,omitempty"`
	// Query filters task/list by job_id substring.
	Query string `json:"query,omitempty"`
	// Paper sets the default paper for paper/set.
	Paper *domain.Paper `json:"paper,omitempty"`
	// Copies sets the default copy count for config.set-defaults.
	Copies int `json:"copies,omitempty"`
	// Entry is the allowlist entry for origin/ip add and delete.
	Entry string `json:"entry,omitempty"`
	// Port is the listener port for service/set-port.
	Port int `json:"port,omitempty"`
	// Days is the retention window for history/prune.
	Days int `json:"days,omitempty"`
	// Enable toggles remote polling for remote/set-enabled.
	Enable bool `json:"enable,omitempty"`
	// Text carries a scalar value for remote/* and app/* setters.
	Text string `json:"text,omitempty"`
	// Level and Since filter logs.tail (Since is RFC3339).
	Level string `json:"level,omitempty"`
	Since string `json:"since,omitempty"`
	// EventIDs selects specific dead letters for retry/delete. An empty slice
	// means every dead letter, which is the "purge all" form.
	EventIDs []string `json:"event_ids,omitempty"`
	// Format selects the history export serialization: "csv" (default) or "json".
	Format string `json:"format,omitempty"`
	// Rows carries exported history content back over IPC. It is a byte slice so
	// the CLI writes exactly what the agent produced.
	Rows []byte `json:"rows,omitempty"`
}

type Response struct {
	ProtocolVersion int    `json:"protocol_version"`
	RequestID       string `json:"request_id"`
	ErrorCode       string `json:"error_code,omitempty"`
	Error           string `json:"error,omitempty"`

	Config     *domain.Config      `json:"config,omitempty"`
	Printers   []string            `json:"printers,omitempty"`
	Printer    *domain.PrinterInfo `json:"printer,omitempty"`
	History    []domain.Status     `json:"history,omitempty"`
	Jobs       []store.JobSummary  `json:"jobs,omitempty"`
	Active     []domain.Status     `json:"active,omitempty"`
	Doctor     *DoctorReport       `json:"doctor,omitempty"`
	RemoteTest *RemoteTestReport   `json:"remote_test,omitempty"`
	Logs       []logging.Entry     `json:"logs,omitempty"`
	// DeadLetters is the abandoned status-report list; RemoteStats reports the
	// outbox backlog so the UI can show both numbers at once.
	DeadLetters []store.DeadLetter       `json:"dead_letters,omitempty"`
	RemoteStats *store.RemoteOutboxStats `json:"remote_stats,omitempty"`
	// Rows is exported history content; RowsCount is how many jobs it covers.
	Rows      []byte `json:"rows,omitempty"`
	RowsCount int    `json:"rows_count,omitempty"`

	Status  string `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
	Value   string `json:"value,omitempty"`
	Removed int64  `json:"removed,omitempty"`
}

// AgentState is the subset of live Agent behaviour the command service needs.
// It is an interface so the desktop UI can pass the in-process Agent and the CLI
// can pass a remote proxy over IPC, without command importing agent.
type AgentState interface {
	ActiveJobs() []domain.Status
	CancelJob(ctx context.Context, jobID string) (domain.Status, error)
}

// Service executes typed management commands. Both the MyGo desktop UI (in
// process) and the CLI (over IPC) call exactly this code, so the two entry
// points cannot drift.
type Service struct {
	Store        *store.Store
	Printer      printer.Backend
	Agent        AgentState
	Logs         *logging.Buffer
	RemoteClient *http.Client
}

func (s Service) Execute(ctx context.Context, req Request) Response {
	r := Response{ProtocolVersion: 1, RequestID: req.RequestID}
	err := s.dispatch(ctx, req, &r)
	if err != nil {
		r.ErrorCode = domain.CodeOf(err)
		r.Error = domain.MessageOf(err)
	}
	return r
}

func (s Service) dispatch(ctx context.Context, req Request, r *Response) error {
	switch req.Command {
	case "status":
		r.Status = "running"
		if s.Agent != nil {
			r.Active = s.Agent.ActiveJobs()
		}
		return nil
	case "config.get":
		c := s.Store.Config()
		r.Config = &c
		return nil
	case "config.set":
		if req.Config == nil {
			return domain.NewProtocolError(domain.CodeInvalidMessage, "config required")
		}
		if err := s.Store.SetConfig(*req.Config); err != nil {
			return domain.NewProtocolError(domain.CodeConfigInvalid, err.Error()).WithCause(err)
		}
		return nil
	case "config.validate":
		// No path validates the live configuration; a path validates that file
		// without touching the running config. Reporting a file's problems is
		// the whole point: a corrupt config.json prevents the store from opening,
		// so `doctor` and `config get` cannot diagnose it.
		if req.Text == "" {
			c := s.Store.Config()
			if err := c.Validate(); err != nil {
				return domain.NewProtocolError(domain.CodeConfigInvalid, err.Error()).WithCause(err)
			}
			c = c.Normalize()
			r.Config = &c
			r.Message = "configuration is valid"
			return nil
		}
		c, err := domain.LoadConfigFile(req.Text)
		if err != nil {
			return domain.NewProtocolErrorf(domain.CodeConfigInvalid, "%s: %v", req.Text, err).WithCause(err)
		}
		r.Config = &c
		r.Message = req.Text + ": configuration is valid"
		return nil
	case "printers.list":
		if s.Printer == nil {
			return domain.NewProtocolError(domain.CodePrintFailed, "no print backend is configured")
		}
		names, err := s.Printer.List(ctx)
		if err != nil {
			return domain.NewProtocolError(domain.CodePrintFailed, "failed to enumerate printers").WithCause(err)
		}
		r.Printers = names
		return nil
	case "printers.show":
		info, err := s.Printer.Info(ctx, req.Name)
		if err != nil {
			if errors.Is(err, printer.ErrPrinterNotFound) {
				return domain.NewProtocolErrorf(domain.CodePrinterNotFound, "printer not found: %s", req.Name)
			}
			return domain.NewProtocolError(domain.CodePrintFailed, "failed to read printer capabilities").WithCause(err)
		}
		r.Printer = info
		return nil
	case "printers.default":
		if req.Name == "" {
			r.Value = s.Store.Config().Printing.DefaultPrinter
			return nil
		}
		names, err := s.Printer.List(ctx)
		if err != nil {
			return domain.NewProtocolError(domain.CodePrintFailed, "failed to enumerate printers").WithCause(err)
		}
		found := false
		for _, name := range names {
			if name == req.Name {
				found = true
				break
			}
		}
		if !found {
			return domain.NewProtocolErrorf(domain.CodePrinterNotFound, "printer not found: %s", req.Name)
		}
		c := s.Store.Config()
		c.Printing.DefaultPrinter = req.Name
		if err := s.Store.SetConfig(c); err != nil {
			return domain.NewProtocolError(domain.CodeConfigInvalid, err.Error()).WithCause(err)
		}
		r.Value = req.Name
		return nil
	case "history.list":
		limit := req.Limit
		if limit <= 0 {
			limit = 100
		}
		history, err := s.Store.Recent(ctx, limit)
		if err != nil {
			return err
		}
		r.History = history
		return nil
	case "task.list":
		jobs, err := s.Store.ListJobs(ctx, req.Limit, req.Offset, req.Status, req.Query)
		if err != nil {
			return err
		}
		r.Jobs = jobs
		if s.Agent != nil {
			r.Active = s.Agent.ActiveJobs()
		}
		return nil
	case "task.show":
		if req.JobID == "" {
			return domain.NewProtocolError(domain.CodeInvalidMessage, "job_id required")
		}
		timeline, err := s.Store.JobHistory(ctx, req.JobID)
		if err != nil {
			return err
		}
		if len(timeline) == 0 {
			return domain.NewProtocolErrorf(domain.CodeJobNotFound, "job not found: %s", req.JobID)
		}
		r.History = timeline
		return nil
	case "task.cancel":
		if s.Agent == nil {
			return domain.NewProtocolError(domain.CodeAgentNotRunning, "task cancellation requires a running agent")
		}
		if req.JobID == "" {
			return domain.NewProtocolError(domain.CodeInvalidMessage, "job_id required")
		}
		st, err := s.Agent.CancelJob(ctx, req.JobID)
		if err != nil {
			return err
		}
		r.Status = st.Status
		r.Message = st.Message
		return nil
	case "task.clear":
		// Mirrors ClearTaskHistory: jobs whose latest status is
		// still non-terminal are left alone, because clearing them would erase
		// the only record of a job the queue may still be working on.
		removed, err := s.Store.ClearHistory(ctx)
		if err != nil {
			return err
		}
		r.Removed = removed
		return nil
	case "history.export":
		return s.exportHistory(ctx, req, r)
	case "history.prune":
		days := req.Days
		if days <= 0 {
			days = s.Store.Config().Remote.HistoryRetentionDays
		}
		removed, err := s.Store.PruneHistory(ctx, days)
		if err != nil {
			return err
		}
		r.Removed = removed
		return nil
	case "config.set-default-paper":
		c := s.Store.Config()
		c.Printing.DefaultPaper = req.Paper
		if err := s.Store.SetConfig(c); err != nil {
			return domain.NewProtocolError(domain.CodeConfigInvalid, err.Error()).WithCause(err)
		}
		r.Config = &c
		return nil
	case "service.set-port":
		c := s.Store.Config()
		c.Service.Port = req.Port
		if err := s.Store.SetConfig(c); err != nil {
			return domain.NewProtocolError(domain.CodeConfigInvalid, err.Error()).WithCause(err)
		}
		r.Config = &c
		r.Message = "saved; restart the agent to apply the new listener port"
		return nil
	case "config.add-allowed_origins", "config.delete-allowed_origins",
		"config.add-allowed_ips", "config.delete-allowed_ips":
		c := s.Store.Config()
		add := strings.HasPrefix(req.Command, "config.add-")
		isOrigin := strings.HasSuffix(req.Command, "allowed_origins")
		var updated domain.Config
		var err error
		if isOrigin {
			updated, err = withEntry(c, c.Security.AllowedOrigins, req.Entry, add, true)
			if err == nil {
				updated.Security.AllowedOrigins = appendOrRemove(updated.Security.AllowedOrigins, req.Entry, add)
			}
		} else {
			updated, err = withEntry(c, c.Security.AllowedIPs, req.Entry, add, false)
			if err == nil {
				updated.Security.AllowedIPs = appendOrRemove(updated.Security.AllowedIPs, req.Entry, add)
			}
		}
		if err != nil {
			return err
		}
		// Normalize here too: SetConfig normalizes what it persists, but the
		// response must report the same effective value, otherwise `ip delete
		// 127.0.0.1` would echo a list without the mandatory loopback entry.
		updated = updated.Normalize()
		if err := s.Store.SetConfig(updated); err != nil {
			return domain.NewProtocolError(domain.CodeConfigInvalid, err.Error()).WithCause(err)
		}
		r.Config = &updated
		return nil
	case "remote.set-enabled", "remote.set-url", "remote.set-token",
		"remote.set-device-id", "remote.set-device-name",
		"remote.set-interval", "remote.set-max-retries", "remote.generate-device-id":
		c := s.Store.Config()
		switch req.Command {
		case "remote.set-enabled":
			c.Remote.Enabled = req.Enable
		case "remote.set-url":
			c.Remote.EndpointURL = req.Text
		case "remote.set-token":
			c.Remote.BearerToken = req.Text
		case "remote.set-device-id":
			c.Remote.DeviceID = req.Text
		case "remote.set-device-name":
			c.Remote.DeviceName = req.Text
		case "remote.set-interval":
			c.Remote.PollIntervalSeconds = req.Limit
		case "remote.set-max-retries":
			c.Remote.MaxReportRetries = req.Limit
		case "remote.generate-device-id":
			c.Remote.DeviceID = uuid.NewString()
		}
		if err := c.Validate(); err != nil {
			return domain.NewProtocolError(domain.CodeConfigInvalid, err.Error()).WithCause(err)
		}
		if err := s.Store.SetConfig(c); err != nil {
			return domain.NewProtocolError(domain.CodeConfigInvalid, err.Error()).WithCause(err)
		}
		r.Config = &c
		if req.Command == "remote.generate-device-id" {
			r.Value = c.Remote.DeviceID
		}
		return nil
	case "doctor":
		return s.doctor(ctx, r)
	case "test.print":
		if s.Agent == nil {
			return domain.NewProtocolError(domain.CodeAgentNotRunning, "test printing requires a running agent")
		}
		return s.testPrint(ctx, req.Name, r)
	case "remote.test":
		if s.Agent == nil {
			return domain.NewProtocolError(domain.CodeAgentNotRunning, "remote testing requires a running agent")
		}
		return s.testRemote(ctx, r)
	case "logs.tail":
		if s.Agent == nil {
			return domain.NewProtocolError(domain.CodeAgentNotRunning, "logs require a running agent")
		}
		return s.tailLogs(req, r)
	case "remote.stats":
		return s.remoteStats(ctx, r)
	case "remote.dead-letters":
		return s.listDeadLetters(ctx, req, r)
	case "remote.retry-dead-letters":
		return s.retryDeadLetters(ctx, req, r)
	case "remote.delete-dead-letters":
		return s.deleteDeadLetters(ctx, req, r)
	case "remote.purge-dead-letters":
		return s.purgeDeadLetters(ctx, r)
	default:
		return domain.NewProtocolErrorf(domain.CodeUnsupportedCommand, "unknown command: %s", req.Command)
	}
}

// withEntry validates an allowlist change and returns the config scaffold the
// caller then applies with appendOrRemove. It validates the single new entry in
// isolation so a bad value is rejected before the list is touched.
func withEntry(c domain.Config, list []string, entry string, add, isOrigin bool) (domain.Config, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return c, domain.NewProtocolError(domain.CodeArgumentInvalid, "empty allowlist entry")
	}
	probe := c
	if isOrigin {
		probe.Security.AllowedOrigins = []string{entry}
		probe.Security.AllowedIPs = nil
	} else {
		probe.Security.AllowedOrigins = nil
		probe.Security.AllowedIPs = []string{entry}
	}
	if err := probe.Validate(); err != nil {
		return c, domain.NewProtocolError(domain.CodeArgumentInvalid, err.Error()).WithCause(err)
	}
	found := false
	for _, existing := range list {
		if existing == entry {
			found = true
			break
		}
	}
	if add && found {
		return c, domain.NewProtocolErrorf(domain.CodeConflict, "entry already present: %s", entry)
	}
	if !add && !found {
		return c, domain.NewProtocolErrorf(domain.CodeConflict, "entry not found: %s", entry)
	}
	return c, nil
}

// appendOrRemove produces a new slice so the caller's config value is replaced
// rather than aliasing the slice header that Go copies by value.
func appendOrRemove(list []string, entry string, add bool) []string {
	entry = strings.TrimSpace(entry)
	if add {
		out := make([]string, 0, len(list)+1)
		out = append(out, list...)
		return append(out, entry)
	}
	out := make([]string, 0, len(list))
	for _, existing := range list {
		if existing != entry {
			out = append(out, existing)
		}
	}
	return out
}
