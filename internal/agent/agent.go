package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/shiuoei/print-bridge/internal/convert"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/printer"
	"github.com/shiuoei/print-bridge/internal/security"
	"github.com/shiuoei/print-bridge/internal/store"
)

// errShuttingDown is returned once Shutdown has begun so a caller does not
// believe a job was accepted after the worker stopped draining.
var errShuttingDown = domain.NewProtocolError(domain.CodeInternalError, "agent is shutting down")

// connection is one upgraded WebSocket client. The registry exists so Shutdown
// can close upgraded connections: http.Server.Shutdown does not track hijacked
// connections, which previously left clients hanging against a stopped agent.
type connection struct {
	conn   *websocket.Conn
	sendMu sync.Mutex
	closed bool
}

func (c *connection) write(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	return c.conn.Write(ctx, websocket.MessageText, data)
}

func (c *connection) close(code websocket.StatusCode, reason string) {
	c.sendMu.Lock()
	if c.closed {
		c.sendMu.Unlock()
		return
	}
	c.closed = true
	c.sendMu.Unlock()

	// websocket.Conn.Close performs the closing handshake and blocks until the
	// peer answers or a full write timeout expires (~5 s). A peer that stopped
	// reading (a background browser tab, a killed CLI) must never stall shutdown,
	// so the polite close frame is emitted concurrently and the socket is torn
	// down immediately. CloseNow also unblocks the pending Close.
	go func() { _ = c.conn.Close(code, reason) }()
	_ = c.conn.CloseNow()
}

// activeJob tracks one accepted job that has not reached a terminal state.
//
// gate enforces the protocol ordering guarantee: the `queued` acknowledgement
// must reach the client before any later state for the same request, otherwise a
// fast job can emit `printing` first and the SDK resolves the wrong pending
// request. The worker waits on gate before its first push.
type activeJob struct {
	stateMu    sync.Mutex
	status     domain.Status
	seq        uint64
	batchID    string
	source     string
	printer    string
	paperName  string
	copies     int
	ctx        context.Context
	cancel     context.CancelFunc
	conn       *connection
	remotePush func(domain.Status)
	gate       chan struct{}
	gateOnce   sync.Once
	gated      bool
	cancelled  bool
}

func (j *activeJob) release() {
	if !j.gated {
		return
	}
	j.gateOnce.Do(func() { close(j.gate) })
}

// waitGate blocks until the `queued` acknowledgement was written. The timeout
// keeps a vanished client from stalling the serial worker forever.
func (j *activeJob) waitGate() {
	if !j.gated {
		return
	}
	select {
	case <-j.gate:
	case <-time.After(10 * time.Second):
	}
}

type task struct {
	job       domain.Job
	requestID string
	conn      *connection
	// htmlLocalOrigin is the exact loopback origin this task may render from,
	// derived from the WebSocket handshake when the page URL is same-origin.
	htmlLocalOrigin string
}

type Agent struct {
	store   *store.Store
	printer printer.Backend
	queue   chan task

	mu      sync.Mutex
	active  map[string]*activeJob
	batches map[string]struct{}
	seq     uint64
	closing bool

	conns   map[*connection]struct{}
	connsMu sync.Mutex

	server   *http.Server
	listener net.Listener
	cancel   context.CancelFunc
	done     chan struct{}
	loopDone chan struct{}

	maxConns int
}

func New(s *store.Store, p printer.Backend) *Agent {
	return &Agent{
		store:    s,
		printer:  p,
		queue:    make(chan task, 128),
		active:   make(map[string]*activeJob),
		batches:  make(map[string]struct{}),
		conns:    make(map[*connection]struct{}),
		done:     make(chan struct{}),
		loopDone: make(chan struct{}),
		maxConns: 32,
	}
}

func (a *Agent) Start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	cfg := a.store.Config()
	ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.Service.Port)))
	if err != nil {
		cancel()
		return domain.NewProtocolErrorf(domain.CodeServicePortInUse, "port %d unavailable: %v", cfg.Service.Port, err).WithCause(err)
	}
	a.cancel, a.listener = cancel, ln
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", a.webSocket)
	a.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go a.worker(ctx)
	go a.remoteLoop(ctx)
	go func() {
		defer close(a.loopDone)
		if err := a.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("websocket server stopped", "error", err)
		}
	}()
	return nil
}

// ListeningPort reports the port bound at Start. Configuration may change
// before the process restarts, so it need not match the current config port.
func (a *Agent) ListeningPort() int {
	if a.listener == nil {
		return 0
	}
	if addr, ok := a.listener.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// Shutdown stops accepting connections, closes upgraded WebSocket clients and
// waits for the worker to finish its current job. Queued-but-unstarted jobs are
// recorded as cancelled: they were never submitted to the system queue, so that
// state is accurate, and a restart still never replays them.
//
// The HTTP server is closed with Close rather than Shutdown: an upgraded
// WebSocket handler stays inside ServeHTTP until its read unblocks, and
// http.Server.Shutdown waits for that handler, which held shutdown for a full
// HTTP idle timeout (5 s) after every client was already closed.
func (a *Agent) Shutdown(ctx context.Context) error {
	if a.cancel == nil {
		return nil
	}
	a.mu.Lock()
	a.closing = true
	a.mu.Unlock()

	a.connsMu.Lock()
	conns := make([]*connection, 0, len(a.conns))
	for c := range a.conns {
		conns = append(conns, c)
	}
	a.connsMu.Unlock()
	for _, c := range conns {
		c.close(websocket.StatusGoingAway, "agent shutting down")
	}

	a.cancel()
	serverErr := a.server.Close()
	if errors.Is(serverErr, http.ErrServerClosed) {
		serverErr = nil
	}

	select {
	case <-a.loopDone:
	case <-ctx.Done():
	}
	select {
	case <-a.done:
	case <-ctx.Done():
		if serverErr == nil {
			serverErr = ctx.Err()
		}
	}
	a.drainQueue()
	return serverErr
}

// drainQueue marks every not-yet-started job as cancelled so no job is left in a
// nonterminal state after shutdown.
func (a *Agent) drainQueue() {
	for {
		select {
		case t := <-a.queue:
			a.finish(t.job.JobID, "cancelled", "cancelled before the agent stopped")
		default:
			return
		}
	}
}

func (a *Agent) register(c *connection) error {
	a.connsMu.Lock()
	defer a.connsMu.Unlock()
	if len(a.conns) >= a.maxConns {
		return domain.NewProtocolError(domain.CodeInternalError, "too many concurrent connections")
	}
	a.conns[c] = struct{}{}
	return nil
}

func (a *Agent) unregister(c *connection) {
	a.connsMu.Lock()
	delete(a.conns, c)
	a.connsMu.Unlock()
}

func (a *Agent) applyDefaults(job *domain.Job, cfg domain.Config) error {
	if job.Copies == 0 && job.Format != "raw" {
		job.Copies = cfg.Printing.DefaultCopies
	}
	if job.PrinterName == "" {
		job.PrinterName = cfg.Printing.DefaultPrinter
	}
	if job.Paper == nil && job.Format != "raw" {
		job.Paper = cfg.Printing.DefaultPaper
	}
	if job.PrinterName == "" {
		return domain.NewProtocolError(domain.CodePrinterNotConfigured, "default printer not configured")
	}
	if job.Paper == nil && job.Format != "raw" {
		return domain.NewProtocolError(domain.CodePaperNotConfigured, "default paper not configured")
	}
	return nil
}

// Submit validates and enqueues one job for callers without a live WebSocket
// (remote polling). The returned status is the `queued` acknowledgement.
func (a *Agent) Submit(ctx context.Context, job domain.Job, requestID string, notify func(domain.Status)) (domain.Status, error) {
	return a.SubmitWithSource(ctx, job, requestID, notify, domain.SourceRemote)
}

// SubmitWithSource is Submit with an explicit history source.
func (a *Agent) SubmitWithSource(ctx context.Context, job domain.Job, requestID string, notify func(domain.Status), source string) (domain.Status, error) {
	st, entry, err := a.submitWithMetadata(ctx, job, requestID, nil, notify, "", source)
	if err != nil {
		return domain.Status{}, err
	}
	entry.release()
	return st, nil
}

func (a *Agent) submit(ctx context.Context, job domain.Job, requestID string, conn *connection, notify func(domain.Status)) (domain.Status, *activeJob, error) {
	return a.submitWithOrigin(ctx, job, requestID, conn, notify, "")
}

// submitWithOrigin is submit plus the same-origin loopback allowance derived from
// the connection's handshake Origin.
func (a *Agent) submitWithOrigin(ctx context.Context, job domain.Job, requestID string, conn *connection, notify func(domain.Status), htmlLocalOrigin string) (domain.Status, *activeJob, error) {
	return a.submitWithMetadata(ctx, job, requestID, conn, notify, htmlLocalOrigin, "")
}

// submitWithMetadata is submitWithOrigin plus the history source. Everything the
// job was accepted with is captured once, here, so every later status row can
// record which printer/paper/copies it actually used.
func (a *Agent) submitWithMetadata(ctx context.Context, job domain.Job, requestID string, conn *connection, notify func(domain.Status), htmlLocalOrigin, source string) (domain.Status, *activeJob, error) {
	cfg := a.store.Config()
	if err := job.Validate(cfg); err != nil {
		return domain.Status{}, nil, err
	}
	if err := a.applyDefaults(&job, cfg); err != nil {
		return domain.Status{}, nil, err
	}
	// The allowance is only meaningful for an HTML job whose page lives on the
	// authorised loopback origin; every other format ignores it.
	if job.Format != "html" {
		htmlLocalOrigin = ""
	}
	st := domain.Status{Type: "job_status", RequestID: requestID, JobID: job.JobID, Status: "queued", Message: "queued", At: time.Now().UTC()}

	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return domain.Status{}, nil, errShuttingDown
	}
	if len(a.queue) >= cap(a.queue) {
		a.mu.Unlock()
		return domain.Status{}, nil, domain.NewProtocolError(domain.CodeQueueFull, "print queue full")
	}
	if _, ok := a.active[job.JobID]; ok {
		a.mu.Unlock()
		return domain.Status{}, nil, domain.NewProtocolError(domain.CodeJobDuplicated, "job ID already queued")
	}
	a.mu.Unlock()

	if _, err := a.store.Latest(ctx, job.JobID); err == nil {
		return domain.Status{}, nil, domain.NewProtocolError(domain.CodeJobDuplicated, "job ID already exists")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.Status{}, nil, err
	}

	if err := a.store.AddStatus(ctx, st); err != nil {
		return domain.Status{}, nil, err
	}

	jobCtx, cancel := context.WithCancel(context.Background())

	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		cancel()
		return domain.Status{}, nil, errShuttingDown
	}
	a.seq++
	entry := &activeJob{
		status: st, seq: a.seq, ctx: jobCtx, cancel: cancel, conn: conn, remotePush: notify,
		source: source, printer: job.PrinterName, paperName: paperLabel(job.Paper), copies: job.Copies,
	}
	if conn != nil {
		entry.gate = make(chan struct{})
		entry.gated = true
	}
	a.active[job.JobID] = entry
	a.mu.Unlock()

	a.queue <- task{job: job, requestID: requestID, conn: conn, htmlLocalOrigin: htmlLocalOrigin}
	return st, entry, nil
}

// paperLabel renders a paper for history, or empty when the job has none (raw
// jobs never carry one).
func paperLabel(paper *domain.Paper) string {
	if paper == nil {
		return ""
	}
	return strconv.FormatFloat(paper.WidthMM, 'f', -1, 64) + "x" + strconv.FormatFloat(paper.HeightMM, 'f', -1, 64) + "mm"
}

// SubmitBatch validates the whole batch before persisting or enqueueing any of
// it, so a partially invalid batch never leaves orphan jobs behind.
func (a *Agent) SubmitBatch(ctx context.Context, batchID, requestID string, jobs []domain.Job, notify func(domain.Status)) (domain.Status, error) {
	return a.SubmitBatchWithSource(ctx, batchID, requestID, jobs, notify, domain.SourceRemote)
}

// SubmitBatchWithSource is SubmitBatch with an explicit history source.
func (a *Agent) SubmitBatchWithSource(ctx context.Context, batchID, requestID string, jobs []domain.Job, notify func(domain.Status), source string) (domain.Status, error) {
	st, entries, err := a.submitBatchWithMetadata(ctx, batchID, requestID, jobs, nil, notify, "", source)
	if err != nil {
		return domain.Status{}, err
	}
	for _, e := range entries {
		e.release()
	}
	return st, nil
}

func (a *Agent) submitBatch(ctx context.Context, batchID, requestID string, jobs []domain.Job, conn *connection, notify func(domain.Status)) (domain.Status, []*activeJob, error) {
	return a.submitBatchWithOrigin(ctx, batchID, requestID, jobs, conn, notify, "")
}

func (a *Agent) submitBatchWithOrigin(ctx context.Context, batchID, requestID string, jobs []domain.Job, conn *connection, notify func(domain.Status), htmlLocalOrigin string) (domain.Status, []*activeJob, error) {
	return a.submitBatchWithMetadata(ctx, batchID, requestID, jobs, conn, notify, htmlLocalOrigin, "")
}

func (a *Agent) submitBatchWithMetadata(ctx context.Context, batchID, requestID string, jobs []domain.Job, conn *connection, notify func(domain.Status), htmlLocalOrigin, source string) (domain.Status, []*activeJob, error) {
	cfg := a.store.Config()
	if len(jobs) == 0 {
		return domain.Status{}, nil, domain.NewProtocolError(domain.CodeInvalidMessage, "empty batch")
	}
	if len(jobs) > cfg.Limits.MaxBatchJobs {
		return domain.Status{}, nil, domain.NewProtocolErrorf(domain.CodeBatchTooLarge, "batch exceeds %d jobs", cfg.Limits.MaxBatchJobs)
	}

	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return domain.Status{}, nil, errShuttingDown
	}
	if _, ok := a.batches[batchID]; ok {
		a.mu.Unlock()
		return domain.Status{}, nil, domain.NewProtocolError(domain.CodeBatchDuplicated, "batch ID already exists")
	}
	if len(a.queue)+len(jobs) > cap(a.queue) {
		a.mu.Unlock()
		return domain.Status{}, nil, domain.NewProtocolError(domain.CodeQueueFull, "print queue full")
	}
	a.mu.Unlock()

	statuses := make([]domain.Status, 0, len(jobs))
	seen := map[string]bool{}
	for i := range jobs {
		j := &jobs[i]
		if err := j.Validate(cfg); err != nil {
			return domain.Status{}, nil, err
		}
		if err := a.applyDefaults(j, cfg); err != nil {
			return domain.Status{}, nil, err
		}
		if seen[j.JobID] {
			return domain.Status{}, nil, domain.NewProtocolError(domain.CodeJobDuplicated, "duplicate job ID in batch")
		}
		seen[j.JobID] = true
		a.mu.Lock()
		_, isActive := a.active[j.JobID]
		a.mu.Unlock()
		if isActive {
			return domain.Status{}, nil, domain.NewProtocolError(domain.CodeJobDuplicated, "job ID already queued")
		}
		if _, err := a.store.Latest(ctx, j.JobID); err == nil {
			return domain.Status{}, nil, domain.NewProtocolError(domain.CodeJobDuplicated, "job ID already exists")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return domain.Status{}, nil, err
		}
		statuses = append(statuses, domain.Status{Type: "job_status", RequestID: requestID, JobID: j.JobID, Status: "queued", Message: "queued", At: time.Now().UTC()})
	}
	if err := a.store.AddStatuses(ctx, statuses); err != nil {
		return domain.Status{}, nil, err
	}

	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return domain.Status{}, nil, errShuttingDown
	}
	a.batches[batchID] = struct{}{}
	entries := make([]*activeJob, 0, len(jobs))
	for i, j := range jobs {
		a.seq++
		jobCtx, cancel := context.WithCancel(context.Background())
		entry := &activeJob{
			status: statuses[i], seq: a.seq, batchID: batchID, ctx: jobCtx, cancel: cancel, conn: conn, remotePush: notify,
			source: source, printer: j.PrinterName, paperName: paperLabel(j.Paper), copies: j.Copies,
		}
		if conn != nil {
			entry.gate = make(chan struct{})
			entry.gated = true
		}
		a.active[j.JobID] = entry
		entries = append(entries, entry)
	}
	queued := append([]domain.Job(nil), jobs...)
	a.mu.Unlock()

	for _, j := range queued {
		a.queue <- task{job: j, requestID: requestID, conn: conn}
	}
	return domain.Status{Type: "job_status", RequestID: requestID, JobID: batchID, Status: "queued", Message: "queued", At: time.Now().UTC()}, entries, nil
}

// CancelJob cancels a job that has not started printing. A job already printing
// or submitted to the system queue is refused: this agent cannot prove the
// physical job was cancelled, so it must never report `cancelled`.
func (a *Agent) CancelJob(ctx context.Context, jobID string) (domain.Status, error) {
	a.mu.Lock()
	job, ok := a.active[jobID]
	if !ok {
		a.mu.Unlock()
		return domain.Status{}, domain.NewProtocolError(domain.CodeJobNotCancellable, "job is not active")
	}
	if job.status.Status == "printing" {
		a.mu.Unlock()
		return domain.Status{}, domain.NewProtocolError(domain.CodeJobNotCancellable, "job is already printing; it cannot be cancelled safely")
	}
	job.cancelled = true
	if job.cancel != nil {
		job.cancel()
	}
	a.mu.Unlock()

	a.finish(jobID, "cancelled", "cancelled by local request")
	st, err := a.store.Latest(ctx, jobID)
	if err != nil {
		return domain.Status{Type: "job_status", JobID: jobID, Status: "cancelled", Message: "cancelled by local request", At: time.Now().UTC()}, nil
	}
	return st, nil
}

func (a *Agent) worker(ctx context.Context) {
	defer close(a.done)
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-a.queue:
			a.execute(ctx, t)
		}
	}
}

func (a *Agent) execute(ctx context.Context, t task) {
	entry := a.gateFor(t.job.JobID)
	if entry == nil {
		// A queued job may already have reached a terminal state after cancellation.
		return
	}
	jobCtx := entry.ctx
	if jobCtx == nil {
		jobCtx = ctx
	}
	if entry.cancel != nil {
		stop := context.AfterFunc(ctx, entry.cancel)
		defer stop()
	}
	if a.isCancelled(t.job.JobID) {
		a.finish(t.job.JobID, "cancelled", "cancelled before printing started")
		return
	}
	if t.job.Format != "raw" {
		if !a.emit(t, "downloading", "downloading") {
			return
		}
	}
	if a.isCancelled(t.job.JobID) {
		return
	}

	dir, err := os.MkdirTemp("", "printbridge-job-*")
	if err != nil {
		a.finishFailed(t.job.JobID, domain.NewProtocolError(domain.CodeInternalError, "cannot create job work directory").WithCause(err))
		return
	}
	defer os.RemoveAll(dir)

	pdf, raw, err := convert.Prepare(jobCtx, t.job, a.store.Config(), dir, t.htmlLocalOrigin)
	if err != nil {
		if a.isCancelled(t.job.JobID) || errors.Is(err, context.Canceled) {
			a.finish(t.job.JobID, "cancelled", "cancelled during preparation")
			return
		}
		a.finishFailed(t.job.JobID, err)
		return
	}
	if !a.emit(t, "printing", "printing") {
		return
	}
	if a.isCancelled(t.job.JobID) {
		return
	}
	var submission printer.Submission
	if t.job.Format == "raw" {
		submission, err = a.printer.PrintRaw(jobCtx, t.job.PrinterName, raw)
	} else if optioned, ok := a.printer.(printer.OptionedPdfPrinter); ok {
		// The page-fitting choices the job was accepted with reach the backend
		// only when it can honour them; a backend without the optional interface
		// still prints, just with its own defaults.
		scaleMode, direction, center := t.job.PaperOptions()
		submission, err = optioned.PrintPDFWithOptions(jobCtx, pdf, t.job.PrinterName, t.job.Copies, t.job.Paper,
			printer.PrintOptions{ScaleMode: scaleMode, Direction: direction, Center: center})
	} else {
		submission, err = a.printer.PrintPDF(jobCtx, pdf, t.job.PrinterName, t.job.Copies, t.job.Paper)
	}
	if err != nil {
		if a.isCancelled(t.job.JobID) || errors.Is(err, context.Canceled) {
			a.finish(t.job.JobID, "cancelled", "cancelled during printing")
			return
		}
		a.finishFailed(t.job.JobID, classifyPrintError(err))
		return
	}
	// `submitted` means the job reached the system print queue. Physical output
	// cannot be inferred from that, so the job stays `submitted` unless the
	// platform can positively report a verdict for the queue job id.
	a.finish(t.job.JobID, "submitted", "submitted to system print queue")
	a.trackSubmission(t.job.JobID, submission, t.job.PrinterName)
}

// trackSubmission asks the backend for the platform's verdict on a job it just
// submitted, and records `completed`/`unknown` only when the platform answered.
//
// This is deliberately conservative: a backend without a tracker, or one that
// cannot find the job in the completed list, leaves the job at `submitted`. The
// agent must never claim `completed` from an absence of evidence.
func (a *Agent) trackSubmission(jobID string, submission printer.Submission, printerName string) {
	if !submission.Tracking {
		return
	}
	tracker, ok := a.printer.(printer.Tracker)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	outcome := tracker.Track(ctx, submission, printerName)
	switch outcome.State {
	case printer.TrackingCompleted:
		a.finish(jobID, "completed", outcome.Message)
	case printer.TrackingFailed:
		a.finish(jobID, "failed", outcome.Message)
	case printer.TrackingUnknown:
		a.finish(jobID, "unknown", outcome.Message)
	}
}

func (a *Agent) gateFor(jobID string) *activeJob {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active[jobID]
}

// classifyPrintError maps a print-backend failure onto the documented code. A
// failed spool submission is PRINT_FAILED; anything already classified (for
// example a printer that vanished) keeps its own code.
func classifyPrintError(err error) error {
	if errors.Is(err, printer.ErrPrinterNotFound) {
		return domain.NewProtocolError(domain.CodePrinterNotFound, domain.MessageOf(err)).WithCause(err)
	}
	if code := domain.CodeOf(err); code != "" && code != domain.CodeInternalError {
		return err
	}
	return domain.NewProtocolError(domain.CodePrintFailed, domain.MessageOf(err)).WithCause(err)
}

func (a *Agent) isCancelled(jobID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	job, ok := a.active[jobID]
	return ok && job.cancelled
}

// emit records and publishes an intermediate state. It returns false when the
// job disappeared or was cancelled, telling the worker to stop executing.
func (a *Agent) emit(t task, state, message string) bool {
	a.mu.Lock()
	job, ok := a.active[t.job.JobID]
	a.mu.Unlock()
	if !ok {
		return false
	}
	job.stateMu.Lock()
	defer job.stateMu.Unlock()
	a.mu.Lock()
	if a.active[t.job.JobID] != job || job.cancelled {
		a.mu.Unlock()
		return false
	}
	job.status = domain.Status{Type: "job_status", RequestID: t.requestID, JobID: t.job.JobID, Status: state, Message: message, At: time.Now().UTC()}
	conn := job.conn
	remotePush := job.remotePush
	a.mu.Unlock()

	job.waitGate()
	a.persistAndNotify(t.job.JobID, t.requestID, state, message, "", conn)
	if remotePush != nil {
		// Only queued maps to `accepted`; intermediate states are filtered by the
		// remote mapper, matching the protocol contract.
		remotePush(domain.Status{Type: "job_status", RequestID: t.requestID, JobID: t.job.JobID, Status: state, Message: message, At: time.Now().UTC()})
	}
	return true
}

// jobMetadata reads the history metadata captured when the job was accepted.
func (a *Agent) jobMetadata(jobID string) (source, printer, paperName string, copies int, batchID string) {
	a.mu.Lock()
	job, ok := a.active[jobID]
	a.mu.Unlock()
	if !ok {
		return "", "", "", 0, ""
	}
	return job.source, job.printer, job.paperName, job.copies, job.batchID
}

func (a *Agent) persistAndNotify(jobID, requestID, state, message, code string, conn *connection) {
	source, printer, paperName, copies, batchID := a.jobMetadata(jobID)
	st := domain.Status{
		Type: "job_status", RequestID: requestID, JobID: jobID, Status: state, Message: message,
		ErrorCode: code, At: time.Now().UTC(),
		Source: source, PrinterName: printer, PaperName: paperName, Copies: copies, BatchID: batchID,
	}
	if err := a.store.AddStatus(context.Background(), st); err != nil {
		// A persistence failure must not silently hide the state change, so it is
		// logged with the job identity instead of being dropped.
		slog.Error("save status", "job_id", jobID, "status", state, "error", err)
	}
	if conn != nil {
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := conn.write(writeCtx, st); err != nil {
			slog.Debug("push status failed", "job_id", jobID, "error", err)
		}
	}
}

// finishFailed records a failed terminal state carrying the classified error so
// the client receives error_code=DOWNLOAD_FAILED / FORMAT_MISMATCH / etc. instead
// of an untyped `failed` message.
func (a *Agent) finishFailed(jobID string, err error) bool {
	message := domain.MessageOf(err)
	code := domain.CodeOf(err)
	if code == "" {
		code = domain.CodeInternalError
	}
	return a.finishWithCode(jobID, "failed", message, code)
}

// finish records a terminal state without an error classification.
func (a *Agent) finish(jobID, state, message string) bool {
	return a.finishWithCode(jobID, state, message, "")
}

// finishWithCode records a terminal state, removes the job from the active set,
// notifies the owning connection and forwards the state to the remote outbox only
// when the job came from the remote poller. Local WebSocket jobs must not create
// remote report events, otherwise every browser print would be POSTed back.
func (a *Agent) finishWithCode(jobID, state, message, code string) bool {
	a.mu.Lock()
	job, ok := a.active[jobID]
	a.mu.Unlock()
	if !ok {
		return false
	}
	job.stateMu.Lock()
	defer job.stateMu.Unlock()
	a.mu.Lock()
	if a.active[jobID] != job {
		a.mu.Unlock()
		return false
	}
	requestID := job.status.RequestID
	conn := job.conn
	remotePush := job.remotePush
	batchID := job.batchID
	delete(a.active, jobID)
	a.mu.Unlock()
	if job.cancel != nil {
		job.cancel()
	}
	job.waitGate()
	a.persistAndNotify(jobID, requestID, state, message, code, conn)
	if remotePush != nil {
		remotePush(domain.Status{Type: "job_status", RequestID: requestID, JobID: jobID, BatchID: batchID, Status: state, Message: message, ErrorCode: code, At: time.Now().UTC()})
	}
	return true
}

type wireMessage struct {
	Type      string       `json:"type"`
	RequestID string       `json:"request_id"`
	JobID     string       `json:"job_id"`
	BatchID   string       `json:"batch_id"`
	Time      int64        `json:"time"`
	Jobs      []domain.Job `json:"jobs"`
	domain.Job
}

func (a *Agent) webSocket(w http.ResponseWriter, r *http.Request) {
	addr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "invalid client address", http.StatusForbidden)
		return
	}
	cfg := a.store.Config()
	ip := net.ParseIP(addr)
	origin := r.Header.Get("Origin")
	// Node-based clients using the ws package send no Origin header. Allow that case
	// only from loopback; browser connections still require an exact allowlist hit.
	if !cfg.IPAllowed(ip) || !(cfg.OriginAllowed(origin) || (origin == "" && ip.IsLoopback())) {
		http.Error(w, "client not allowed", http.StatusForbidden)
		return
	}

	a.mu.Lock()
	closing := a.closing
	a.mu.Unlock()
	if closing {
		http.Error(w, "agent is shutting down", http.StatusServiceUnavailable)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c := &connection{conn: conn}
	if err := a.register(c); err != nil {
		c.close(websocket.StatusTryAgainLater, "too many connections")
		return
	}
	defer a.unregister(c)
	defer c.close(websocket.StatusNormalClosure, "")

	conn.SetReadLimit(int64(cfg.Limits.MaxFileSizeMB) * 1024 * 1024 * 2)

	send := func(v any) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return c.write(ctx, v)
	}

	for {
		_, data, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		var msg wireMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			_ = send(errorFrame("", domain.NewProtocolError(domain.CodeInvalidMessage, "invalid JSON")))
			continue
		}
		switch msg.Type {
		case "ping":
			// The literal is part of the wire contract: the agent
			// answers "ready" and the SDK surfaces it verbatim as agentStatus.
			_ = send(map[string]any{"type": "pong", "time": msg.Time, "agent_status": "ready"})
		case "print":
			var job domain.Job
			if err := json.Unmarshal(data, &job); err != nil {
				_ = send(errorFrame(msg.RequestID, domain.NewProtocolError(domain.CodeInvalidMessage, err.Error())))
				continue
			}
			loopbackOrigin, originErr := htmlLocalOriginFor([]domain.Job{job}, origin)
			if originErr != nil {
				_ = send(errorFrame(msg.RequestID, originErr))
				continue
			}
			st, entry, err := a.submitWithMetadata(context.Background(), job, msg.RequestID, c, nil, loopbackOrigin, domain.SourceWebSocket)
			if err != nil {
				_ = send(errorFrame(msg.RequestID, err))
				continue
			}
			_ = send(st)
			entry.release()
		case "print_batch":
			loopbackOrigin, originErr := htmlLocalOriginFor(msg.Jobs, origin)
			if originErr != nil {
				_ = send(errorFrame(msg.RequestID, originErr))
				continue
			}
			st, entries, err := a.submitBatchWithMetadata(context.Background(), msg.BatchID, msg.RequestID, msg.Jobs, c, nil, loopbackOrigin, domain.SourceWebSocket)
			if err != nil {
				_ = send(errorFrame(msg.RequestID, err))
				continue
			}
			_ = send(st)
			for _, job := range msg.Jobs {
				_ = send(domain.Status{Type: "job_status", RequestID: msg.RequestID, JobID: job.JobID, Status: "queued", Message: "queued"})
			}
			for _, entry := range entries {
				entry.release()
			}
		case "get_printers_list":
			a.handlePrintersList(send, msg.RequestID, cfg)
		case "printers":
			// Alternative spelling of get_printers_list used by some business front-ends.
			// `{type:"printers", list:[{name, isDefault}]}` — a different shape
			// from the get_printers_list one above, so it is answered on its own
			// branch rather than by reusing handlePrintersList and confusing
			// both callers.
			a.handleShortPrinters(send, cfg)
		case "get_printer_info":
			a.handlePrinterInfo(send, msg.RequestID, msg.PrinterName, cfg)
		case "get_print_queue":
			// The protocol response lists jobs that have not started executing
			// yet, all reported as `queued`. Reporting in-flight jobs with live
			// status would make "waiting jobs" wrong for a client that renders
			// the queue.
			_ = send(map[string]any{"type": "print_queue", "request_id": msg.RequestID, "jobs": a.pendingQueueSnapshot()})
		case "query":
			st, err := a.store.Latest(context.Background(), msg.JobID)
			if err != nil {
				_ = send(domain.Status{Type: "job_status", RequestID: msg.RequestID, JobID: msg.JobID, Status: "unknown", Message: "unknown"})
			} else {
				st.RequestID = msg.RequestID
				_ = send(st)
			}
		default:
			_ = send(errorFrame(msg.RequestID, domain.NewProtocolError(domain.CodeInvalidMessage, "unknown message type")))
		}
	}
}

// queueSnapshot returns active jobs in acceptance order. Iterating the map
// directly made `get_print_queue` ordering unstable between calls.
//
// Each entry carries the same fields the protocol queue response does, including
// batch_id so a client can group a batch's jobs. batch_id is omitted for jobs
// accepted outside a batch, and omitted in that case.
func (a *Agent) queueSnapshot() []map[string]any {
	a.mu.Lock()
	type row struct {
		seq   uint64
		batch string
		st    domain.Status
	}
	rows := make([]row, 0, len(a.active))
	for _, job := range a.active {
		job.stateMu.Lock()
		st := job.status
		job.stateMu.Unlock()
		rows = append(rows, row{seq: job.seq, batch: job.batchID, st: st})
	}
	a.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].seq < rows[j].seq })
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		item := map[string]any{
			"request_id": r.st.RequestID,
			"job_id":     r.st.JobID,
			"status":     r.st.Status,
			"message":    r.st.Message,
		}
		if r.batch != "" {
			item["batch_id"] = r.batch
		}
		out = append(out, item)
	}
	return out
}

// pendingQueueSnapshot returns only jobs the worker has not started yet, each
// reported as `queued`. This is what `get_print_queue` answers:
// the queue is the backlog, not the in-flight view.
func (a *Agent) pendingQueueSnapshot() []map[string]any {
	rows := a.queueSnapshot()
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if row["status"] != "queued" {
			continue
		}
		item := map[string]any{
			"request_id": row["request_id"],
			"job_id":     row["job_id"],
			"status":     "queued",
			"message":    "queued",
		}
		if batch, ok := row["batch_id"]; ok {
			item["batch_id"] = batch
		}
		out = append(out, item)
	}
	return out
}

// ActiveJobs exposes the in-memory queue for local management commands. It is
// the Agent queue, not the operating system print queue.
func (a *Agent) ActiveJobs() []domain.Status {
	a.mu.Lock()
	type row struct {
		seq uint64
		st  domain.Status
	}
	rows := make([]row, 0, len(a.active))
	for _, job := range a.active {
		job.stateMu.Lock()
		st := job.status
		job.stateMu.Unlock()
		rows = append(rows, row{seq: job.seq, st: st})
	}
	a.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].seq < rows[j].seq })
	out := make([]domain.Status, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.st)
	}
	return out
}

// htmlLocalOriginFor derives the loopback allowance for a batch of jobs from the
// handshake Origin.
//
// A local web app (POS, invoice viewer) is served from http://127.0.0.1:PORT and
// must be able to print its own pages. Allowing every loopback page would reopen
// the SSRF hole the renderer's policy exists to close, so the exception requires
// all of:
//   - the page URL is an exact loopback URL (literal 127.0.0.1 or ::1),
//   - the handshake Origin is an exact loopback origin,
//   - every such page URL shares that exact origin.
//
// Returns ("", nil) when no HTML job targets loopback. Any mismatch is an error,
// because silently dropping the allowance would turn a same-origin local print
// into a confusing DOWNLOAD_FAILED.
func htmlLocalOriginFor(jobs []domain.Job, origin string) (string, error) {
	var loopbackURLs []string
	for _, job := range jobs {
		if job.Format != "html" || job.FileURL == "" {
			continue
		}
		u, err := url.Parse(job.FileURL)
		if err != nil || u == nil {
			continue
		}
		if isExactLoopbackHost(u.Hostname()) {
			loopbackURLs = append(loopbackURLs, job.FileURL)
		}
	}
	if len(loopbackURLs) == 0 {
		return "", nil
	}
	if origin == "" {
		return "", domain.NewProtocolError(domain.CodeInvalidMessage,
			"loopback HTML URLs require a matching loopback WebSocket Origin")
	}
	if !security.IsExactLoopbackOrigin(origin) {
		return "", domain.NewProtocolError(domain.CodeInvalidMessage,
			"loopback HTML URLs require a matching loopback WebSocket Origin")
	}
	allowed, err := url.Parse(origin)
	if err != nil {
		return "", domain.NewProtocolError(domain.CodeInvalidMessage,
			"loopback HTML URLs require a matching loopback WebSocket Origin")
	}
	for _, raw := range loopbackURLs {
		u, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(u.Scheme, allowed.Scheme) || !strings.EqualFold(u.Host, allowed.Host) {
			return "", domain.NewProtocolError(domain.CodeInvalidMessage,
				"loopback HTML URL must match the WebSocket Origin exactly")
		}
	}
	return allowed.Scheme + "://" + allowed.Host, nil
}

// isExactLoopbackHost reports whether host is one of the two canonical loopback
// addresses. Only the literal forms count: `localhost` depends on resolver
// configuration, and the rest of 127.0.0.0/8 is never where a local web app is
// served from, so accepting it would widen the exception for no benefit.
func isExactLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "::1"
}

func (a *Agent) handlePrintersList(send func(any) error, requestID string, cfg domain.Config) {
	names, err := a.printer.List(context.Background())
	if err != nil {
		_ = send(errorFrame(requestID, domain.NewProtocolError(domain.CodePrintFailed, "failed to enumerate printers").WithCause(err)))
		return
	}
	defaultName := defaultPrinterName(a.printer, cfg)
	items := make([]map[string]any, 0, len(names))
	for _, name := range names {
		item := map[string]any{"name": name, "is_default": name == defaultName}
		// The protocol response carries the device attributes alongside the name.
		// They are optional and the SDK models them, so a client that renders a
		// printer list with port/dpi/locality keeps working. A queue whose
		// capabilities cannot be read still appears, just without attributes.
		if info, err := a.printer.Info(context.Background(), name); err == nil {
			if info.DPI != nil {
				item["dpi"] = *info.DPI
			}
			if info.Port != nil {
				item["port"] = *info.Port
			}
			if info.IsLocal != nil {
				item["is_local"] = *info.IsLocal
			}
			if info.IsNetwork != nil {
				item["is_network"] = *info.IsNetwork
			}
			if info.IsVirtual != nil {
				item["is_virtual"] = *info.IsVirtual
			}
		}
		items = append(items, item)
	}
	_ = send(map[string]any{"type": "printers_list", "request_id": requestID, "printers": items})
}

// handleShortPrinters answers the `{"type":"printers"}` spelling.
//
// That client reads `msg.list` and expects camelCase `isDefault`, so answering
// with the `printers_list` frame above would leave its printer picker empty with
// no error shown. The list itself comes from the same source as that frame, so
// both spellings report the same devices.
func (a *Agent) handleShortPrinters(send func(any) error, cfg domain.Config) {
	names, err := a.printer.List(context.Background())
	if err != nil {
		_ = send(errorFrame("", domain.NewProtocolError(domain.CodePrintFailed, "failed to enumerate printers").WithCause(err)))
		return
	}
	defaultName := defaultPrinterName(a.printer, cfg)
	items := make([]map[string]any, 0, len(names))
	for _, name := range names {
		items = append(items, map[string]any{"name": name, "isDefault": name == defaultName})
	}
	_ = send(map[string]any{"type": "printers", "list": items})
}

// defaultPrinterName is the printer reported as `is_default`. The configured
// default comes first because it is what a job without `printer_name` actually
// uses; the OS default is only a fallback for display. Reporting the OS default
// while jobs use the configured one would mislead the business system.
func defaultPrinterName(p printer.Backend, cfg domain.Config) string {
	if cfg.Printing.DefaultPrinter != "" {
		return cfg.Printing.DefaultPrinter
	}
	return systemDefaultPrinterName(p)
}

// systemDefaultPrinterName reports the operating system's default printer.
func systemDefaultPrinterName(p printer.Backend) string {
	if sys, ok := p.(printer.SystemDefaulter); ok {
		if name, err := sys.SystemDefault(context.Background()); err == nil {
			return name
		}
	}
	return ""
}

func (a *Agent) handlePrinterInfo(send func(any) error, requestID, printerName string, cfg domain.Config) {
	if printerName == "" {
		printerName = cfg.Printing.DefaultPrinter
		if printerName == "" {
			printerName = systemDefaultPrinterName(a.printer)
		}
		if printerName == "" {
			_ = send(errorFrame(requestID, domain.NewProtocolError(domain.CodePrinterNotConfigured, "no printer selected")))
			return
		}
	}
	info, err := a.printer.Info(context.Background(), printerName)
	if err != nil {
		if errors.Is(err, printer.ErrPrinterNotFound) {
			_ = send(errorFrame(requestID, domain.NewProtocolErrorf(domain.CodePrinterNotFound, "printer not found: %s", printerName)))
			return
		}
		_ = send(errorFrame(requestID, domain.NewProtocolErrorf(domain.CodePrintFailed, "failed to read capabilities for %s", printerName).WithCause(err)))
		return
	}
	_ = send(map[string]any{"type": "printer_info", "request_id": requestID, "printer": info})
}

func errorFrame(requestID string, err error) map[string]any {
	code := domain.CodeOf(err)
	if code == "" {
		code = domain.CodeInternalError
	}
	return map[string]any{"type": "error", "request_id": requestID, "error_code": code, "message": domain.MessageOf(err)}
}
