package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/printer"
	"github.com/shiuoei/print-bridge/internal/store"
)

// wireClient is a minimal test client for the raw protocol so assertions stay
// independent of any SDK.
type wireClient struct {
	t    *testing.T
	conn *websocket.Conn
}

func newWireClient(t *testing.T, handler http.HandlerFunc, origin string) *wireClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	opts := &websocket.DialOptions{}
	if origin != "" {
		opts.HTTPHeader = http.Header{"Origin": []string{origin}}
	}
	conn, _, err := websocket.Dial(context.Background(), "ws"+srv.URL[4:], opts)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return &wireClient{t: t, conn: conn}
}

func (c *wireClient) send(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *wireClient) read() map[string]any {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.conn.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		c.t.Fatal(err)
	}
	return m
}

// readUntil returns the first message whose status equals want.
func (c *wireClient) readUntilStatus(want string) map[string]any {
	c.t.Helper()
	for i := 0; i < 8; i++ {
		m := c.read()
		if m["status"] == want {
			return m
		}
	}
	c.t.Fatalf("status %q not observed", want)
	return nil
}

func newTestAgent(t *testing.T, p printer.Backend) (*Agent, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := s.Config()
	c.Printing.DefaultPrinter = "Test Printer"
	c.Printing.DefaultPaper = &domain.Paper{WidthMM: 210, HeightMM: 297}
	c.Security.AllowedOrigins = []string{"https://example.com"}
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	a := New(s, p)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.worker(ctx)
	return a, s
}

// TestPrinterInfoReturnsRealCapabilities is the A07/P09 regression test: the
// endpoint used to return hard-coded empty arrays.
func TestPrinterInfoReturnsRealCapabilities(t *testing.T) {
	a, _ := newTestAgent(t, &fakePrinter{})
	c := newWireClient(t, a.webSocket, "https://example.com")

	c.send(map[string]any{"type": "get_printer_info", "request_id": "i1", "printer_name": "Test Printer"})
	m := c.read()
	if m["type"] != "printer_info" {
		t.Fatalf("type: %#v", m)
	}
	info, _ := m["printer"].(map[string]any)
	papers, _ := info["papers"].([]any)
	if len(papers) != 2 {
		t.Fatalf("papers: %#v", info["papers"])
	}
	first, _ := papers[0].(map[string]any)
	if first["width_mm"] != float64(210) || first["height_mm"] != float64(297) || first["name"] != "A4" {
		t.Fatalf("paper[0]: %#v", first)
	}
	trays, _ := info["trays"].([]any)
	if len(trays) != 1 {
		t.Fatalf("trays: %#v", info["trays"])
	}
	// media_types must serialize as [] rather than null when the backend has none.
	mediaTypes, ok := info["media_types"].([]any)
	if !ok || mediaTypes == nil {
		t.Fatalf("media_types must be an empty array, got %#v", info["media_types"])
	}
	if len(mediaTypes) != 0 {
		t.Fatalf("media_types: %#v", mediaTypes)
	}
}

// TestPrinterInfoUnknownPrinterVerifiesErrorCode guards the documented
// PRINTER_NOT_FOUND mapping instead of leaking INTERNAL_ERROR.
func TestPrinterInfoUnknownPrinterUsesNotFoundCode(t *testing.T) {
	a, _ := newTestAgent(t, &fakePrinter{})
	c := newWireClient(t, a.webSocket, "https://example.com")

	c.send(map[string]any{"type": "get_printer_info", "request_id": "i2", "printer_name": "No Such Printer"})
	m := c.read()
	if m["type"] != "error" || m["error_code"] != "PRINTER_NOT_FOUND" || m["request_id"] != "i2" {
		t.Fatalf("error frame: %#v", m)
	}
}

// TestErrorFramesCarryStableCodes covers A09 for the validation paths that used
// to be reported as INTERNAL_ERROR.
func TestErrorFramesCarryStableCodes(t *testing.T) {
	a, _ := newTestAgent(t, &fakePrinter{})
	c := newWireClient(t, a.webSocket, "https://example.com")

	cases := []struct {
		name string
		msg  map[string]any
		want string
	}{
		{
			name: "unsupported format",
			msg:  map[string]any{"type": "print", "request_id": "e1", "job_id": "j-e1", "format": "exe"},
			want: "UNSUPPORTED_FORMAT",
		},
		{
			name: "invalid copies",
			msg: map[string]any{"type": "print", "request_id": "e2", "job_id": "j-e2", "format": "raw",
				"data_base64": base64.StdEncoding.EncodeToString([]byte("X")), "copies": 500},
			// A raw job may not carry copies at all, but the copy range is
			// validated first and gives the more specific documented code.
			want: "COPIES_OUT_OF_RANGE",
		},
		{
			name: "raw rejects paper",
			msg: map[string]any{"type": "print", "request_id": "e3", "job_id": "j-e3", "format": "raw",
				"data_base64": base64.StdEncoding.EncodeToString([]byte("X")),
				"paper":       map[string]any{"width_mm": 60, "height_mm": 40}},
			want: "INVALID_MESSAGE",
		},
		{
			name: "missing file_url",
			msg:  map[string]any{"type": "print", "request_id": "e4", "job_id": "j-e4", "format": "pdf"},
			want: "INVALID_MESSAGE",
		},
		{
			name: "unknown message type",
			msg:  map[string]any{"type": "definitely-not-a-request", "request_id": "e5"},
			want: "INVALID_MESSAGE",
		},
		{
			name: "batch too large",
			msg: map[string]any{"type": "print_batch", "request_id": "e6", "batch_id": "b-e6",
				"jobs": manyRawJobs(30)},
			want: "BATCH_TOO_LARGE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.send(tc.msg)
			m := c.read()
			if m["type"] != "error" {
				t.Fatalf("expected error frame, got %#v", m)
			}
			if m["error_code"] != tc.want {
				t.Fatalf("error_code = %v, want %s (%#v)", m["error_code"], tc.want, m)
			}
			if m["request_id"] != tc.msg["request_id"] {
				t.Fatalf("request_id = %v, want %v", m["request_id"], tc.msg["request_id"])
			}
		})
	}
}

func manyRawJobs(n int) []map[string]any {
	jobs := make([]map[string]any, 0, n)
	payload := base64.StdEncoding.EncodeToString([]byte("X"))
	for i := 0; i < n; i++ {
		jobs = append(jobs, map[string]any{
			"job_id":      "bulk-" + string(rune('a'+i%26)) + "-" + itoa(i),
			"format":      "raw",
			"data_base64": payload,
		})
	}
	return jobs
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// TestQueuedAckPrecedesLaterStatuses pins the ordering guarantee that a fast job
// must not push `printing` before the `queued` acknowledgement.
func TestQueuedAckPrecedesLaterStatuses(t *testing.T) {
	a, _ := newTestAgent(t, &fakePrinter{printed: make(chan []byte, 4)})
	c := newWireClient(t, a.webSocket, "https://example.com")

	c.send(map[string]any{"type": "print", "request_id": "o1", "job_id": "order-1", "format": "raw",
		"data_base64": base64.StdEncoding.EncodeToString([]byte("ORDER"))})
	first := c.read()
	if first["status"] != "queued" || first["request_id"] != "o1" {
		t.Fatalf("first frame must be the queued ack, got %#v", first)
	}
	terminal := c.readUntilStatus("submitted")
	if terminal["job_id"] != "order-1" {
		t.Fatalf("terminal frame: %#v", terminal)
	}
}

// TestPrintQueueOrderIsStable verifies get_print_queue no longer reflects random
// Go map iteration order.
func TestPrintQueueOrderIsStable(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.Config()
	c.Printing.DefaultPrinter = "Test Printer"
	c.Printing.DefaultPaper = &domain.Paper{WidthMM: 210, HeightMM: 297}
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	a := New(s, &fakePrinter{})
	// Insert active jobs directly: the worker is intentionally not started so
	// the queue stays populated for inspection.
	for _, id := range []string{"q1", "q2", "q3", "q4", "q5"} {
		a.seq++
		a.active[id] = &activeJob{
			status: domain.Status{Type: "job_status", RequestID: "r-" + id, JobID: id, Status: "queued", Message: "queued"},
			seq:    a.seq,
		}
	}
	first := a.ActiveJobs()
	if len(first) != 5 {
		t.Fatalf("active jobs: %d", len(first))
	}
	for i, want := range []string{"q1", "q2", "q3", "q4", "q5"} {
		if first[i].JobID != want {
			t.Fatalf("order[%d] = %s, want %s", i, first[i].JobID, want)
		}
	}
	for attempt := 0; attempt < 20; attempt++ {
		again := a.ActiveJobs()
		for i := range first {
			if again[i].JobID != first[i].JobID {
				t.Fatalf("order changed on attempt %d: %v vs %v", attempt, ids(again), ids(first))
			}
		}
	}
}

func ids(sts []domain.Status) []string {
	out := make([]string, 0, len(sts))
	for _, st := range sts {
		out = append(out, st.JobID)
	}
	return out
}

// TestCancelQueuedJobReportsCancelled covers the new cancellation path for jobs
// that have not started printing.
func TestCancelQueuedJobReportsCancelled(t *testing.T) {
	a, s := newTestAgent(t, &fakePrinter{printed: make(chan []byte, 1)})
	// Register an active job without starting the worker so it stays queued.
	a.seq++
	a.active["cancel-me"] = &activeJob{
		status: domain.Status{Type: "job_status", RequestID: "rc", JobID: "cancel-me", Status: "queued", Message: "queued"},
		seq:    a.seq,
	}
	st, err := a.CancelJob(context.Background(), "cancel-me")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "cancelled" {
		t.Fatalf("status = %q", st.Status)
	}
	latest, err := s.Latest(context.Background(), "cancel-me")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Status != "cancelled" {
		t.Fatalf("persisted status = %q", latest.Status)
	}
}

// TestCancelPrintingJobIsRefused proves the agent never claims `cancelled` for a
// job that may already be in the system queue.
func TestCancelPrintingJobIsRefused(t *testing.T) {
	a, _ := newTestAgent(t, &fakePrinter{})
	a.seq++
	a.active["printing-job"] = &activeJob{
		status: domain.Status{Type: "job_status", RequestID: "rp", JobID: "printing-job", Status: "printing", Message: "printing"},
		seq:    a.seq,
	}
	_, err := a.CancelJob(context.Background(), "printing-job")
	if err == nil {
		t.Fatal("cancelling a printing job must be refused")
	}
	if !domain.HasCode(err, domain.CodeJobNotCancellable) {
		t.Fatalf("code = %s", domain.CodeOf(err))
	}
}

func TestCancelledStatusFollowsInFlightStatus(t *testing.T) {
	a, s := newTestAgent(t, &fakePrinter{})
	job := &activeJob{
		status: domain.Status{Type: "job_status", JobID: "in-flight", Status: "queued"},
		gate:   make(chan struct{}),
		gated:  true,
	}
	a.mu.Lock()
	a.active["in-flight"] = job
	a.mu.Unlock()
	emitted := make(chan bool, 1)
	go func() {
		emitted <- a.emit(task{job: domain.Job{JobID: "in-flight"}}, "downloading", "downloading")
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.mu.Lock()
		status := job.status.Status
		a.mu.Unlock()
		if status == "downloading" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("intermediate status never entered the acknowledgement gate")
		}
		time.Sleep(time.Millisecond)
	}
	cancelled := make(chan error, 1)
	go func() {
		_, err := a.CancelJob(context.Background(), "in-flight")
		cancelled <- err
	}()
	for {
		a.mu.Lock()
		marked := job.cancelled
		a.mu.Unlock()
		if marked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancellation was not recorded")
		}
		time.Sleep(time.Millisecond)
	}
	job.release()
	select {
	case ok := <-emitted:
		if !ok {
			t.Fatal("in-flight status was unexpectedly discarded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("intermediate status did not finish")
	}
	select {
	case err := <-cancelled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not finish")
	}
	latest, err := s.Latest(t.Context(), "in-flight")
	if err != nil || latest.Status != "cancelled" {
		t.Fatalf("latest history must be cancelled: %#v, %v", latest, err)
	}
}

// TestShutdownClosesUpgradedConnections covers A03: http.Server.Shutdown does not
// track hijacked WebSocket connections, so exiting used to leave clients hanging.
func TestShutdownClosesUpgradedConnections(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.Config()
	c.Service.Port = freePort(t)
	c.Security.AllowedOrigins = []string{"https://example.com"}
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	a := New(s, &fakePrinter{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	client := newWireClient(t, a.webSocket, "https://example.com")
	client.send(map[string]any{"type": "ping", "time": 1})
	if m := client.read(); m["type"] != "pong" {
		t.Fatalf("pong: %#v", m)
	}

	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	shutdownStart := time.Now()
	if err := a.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// Shutdown must not wait on the peer's closing handshake: a browser tab that
	// stopped reading used to stall it for a full HTTP idle timeout.
	if elapsed := time.Since(shutdownStart); elapsed > 3*time.Second {
		t.Fatalf("Shutdown took %v; it must not wait for the client to finish the closing handshake", elapsed)
	}

	// The client must observe closure rather than hang until its own timeout.
	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelRead()
	if _, _, err := client.conn.Read(readCtx); err == nil {
		t.Fatal("connection still open after shutdown")
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("connection was not closed by Shutdown; client had to time out")
	}
}

// TestSubmitAfterShutdownIsRejected ensures a job is never accepted once the
// worker has stopped draining.
func TestSubmitAfterShutdownIsRejected(t *testing.T) {
	a, _ := newTestAgent(t, &fakePrinter{})
	a.mu.Lock()
	a.closing = true
	a.mu.Unlock()
	_, err := a.Submit(context.Background(), domain.Job{
		JobID:      "late",
		Format:     "raw",
		DataBase64: base64.StdEncoding.EncodeToString([]byte("LATE")),
	}, "r-late", nil)
	if err == nil {
		t.Fatal("submission after shutdown must be rejected")
	}
}

// TestShutdownCancelsQueuedJobs proves no job is left nonterminal after a clean
// stop, and that such jobs are not reported as failures.
func TestShutdownCancelsQueuedJobs(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.Config()
	c.Service.Port = freePort(t)
	c.Printing.DefaultPrinter = "Test Printer"
	c.Printing.DefaultPaper = &domain.Paper{WidthMM: 210, HeightMM: 297}
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	a := New(s, &fakePrinter{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Submit(context.Background(), domain.Job{
		JobID:      "queued-at-shutdown",
		Format:     "raw",
		DataBase64: base64.StdEncoding.EncodeToString([]byte("Q")),
	}, "r-q", nil); err != nil {
		t.Fatal(err)
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := a.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	latest, err := s.Latest(context.Background(), "queued-at-shutdown")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Status != "cancelled" && latest.Status != "submitted" {
		t.Fatalf("status after shutdown = %q, want cancelled or submitted", latest.Status)
	}
}

// TestFailedStatusCarriesErrorCode proves A09 is wired through the *job status*
// path: a conversion failure must reach the client with a documented error_code,
// not just an untyped `failed` message.
func TestFailedStatusCarriesErrorCode(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.Config()
	c.Printing.DefaultPrinter = "Test Printer"
	c.Printing.DefaultPaper = &domain.Paper{WidthMM: 210, HeightMM: 297}
	c.Security.AllowedOrigins = []string{"https://example.com"}
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	a := New(s, &fakePrinter{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.worker(ctx)
	client := newWireClient(t, a.webSocket, "https://example.com")

	// A pdf job whose file_url is a data URL with a broken payload fails during
	// preparation with FORMAT_MISMATCH.
	client.send(map[string]any{
		"type": "print", "request_id": "f1", "job_id": "fail-1", "format": "pdf",
		"file_url": "data:application/pdf;base64,bm90LWEtcGRm",
	})
	if m := client.read(); m["status"] != "queued" {
		t.Fatalf("ack: %#v", m)
	}
	failed := client.readUntilStatus("failed")
	if failed["error_code"] != "FORMAT_MISMATCH" {
		t.Fatalf("error_code = %v, want FORMAT_MISMATCH (%#v)", failed["error_code"], failed)
	}

	// The same classification must be visible in the persisted history.
	latest, err := s.Latest(context.Background(), "fail-1")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Status != "failed" || latest.ErrorCode != "FORMAT_MISMATCH" {
		t.Fatalf("persisted = %#v", latest)
	}
}

func TestBackendSubmissionFailureIsNotReportedAsSubmitted(t *testing.T) {
	for _, format := range []string{"raw", "pdf"} {
		t.Run(format, func(t *testing.T) {
			a, s := newTestAgent(t, &fakePrinter{printErr: errors.New("spooler unavailable")})
			client := newWireClient(t, a.webSocket, "https://example.com")
			job := map[string]any{"type": "print", "request_id": "backend-failure", "job_id": "fail-" + format, "format": format}
			if format == "raw" {
				job["data_base64"] = base64.StdEncoding.EncodeToString([]byte("PRINT"))
			} else {
				job["file_url"] = "data:application/pdf;base64," + base64.StdEncoding.EncodeToString([]byte("%PDF-1.4\n%%EOF\n"))
			}
			client.send(job)
			if ack := client.read(); ack["status"] != "queued" {
				t.Fatalf("ack: %#v", ack)
			}
			failed := client.readUntilStatus("failed")
			if failed["error_code"] != domain.CodePrintFailed {
				t.Fatalf("failure: %#v", failed)
			}
			latest, err := s.Latest(context.Background(), "fail-"+format)
			if err != nil || latest.Status != "failed" || latest.ErrorCode != domain.CodePrintFailed {
				t.Fatalf("history: %#v, err: %v", latest, err)
			}
		})
	}
}

func TestBackendMissingPrinterUsesNotFoundCode(t *testing.T) {
	for _, format := range []string{"raw", "pdf"} {
		t.Run(format, func(t *testing.T) {
			a, s := newTestAgent(t, &fakePrinter{printErr: printer.ErrPrinterNotFound})
			client := newWireClient(t, a.webSocket, "https://example.com")
			job := map[string]any{"type": "print", "request_id": "missing-printer", "job_id": "missing-" + format, "format": format}
			if format == "raw" {
				job["data_base64"] = base64.StdEncoding.EncodeToString([]byte("PRINT"))
			} else {
				job["file_url"] = "data:application/pdf;base64," + base64.StdEncoding.EncodeToString([]byte("%PDF-1.4\n%%EOF\n"))
			}
			client.send(job)
			if ack := client.read(); ack["status"] != "queued" {
				t.Fatalf("ack: %#v", ack)
			}
			failed := client.readUntilStatus("failed")
			if failed["error_code"] != domain.CodePrinterNotFound {
				t.Fatalf("failure: %#v", failed)
			}
			latest, err := s.Latest(context.Background(), "missing-"+format)
			if err != nil || latest.ErrorCode != domain.CodePrinterNotFound {
				t.Fatalf("history: %#v, err: %v", latest, err)
			}
		})
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

var _ = printer.ErrPrinterNotFound
var _ = errors.Is
