package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/store"

	"github.com/coder/websocket"
)

// Some business front-ends speak a slightly different dialect from the
// documented protocol: they ask for the printer list with type "printers" and
// expect {type:"printers", list:[...]}, and they submit label bytes as
// data_base64 with copies on every job. These tests pin that compatibility so a
// future rewrite of the dispatcher cannot silently drop it and break such a
// site's printing.
const bizOrigin = "http://localhost:1818"

// bizHarness starts an agent with a stub printer and returns a connected socket.
func bizHarness(t *testing.T) (ctx context.Context, conn *websocket.Conn) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	cfg := s.Config()
	cfg.Printing.DefaultPrinter = "Test Printer"
	cfg.Printing.DefaultPaper = &domain.Paper{WidthMM: 100, HeightMM: 150}
	cfg.Security.AllowedOrigins = []string{bizOrigin}
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	a := New(s, &fakePrinter{printed: make(chan []byte, 8)})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.worker(ctx)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := httptest.NewUnstartedServer(http.HandlerFunc(a.webSocket))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	conn, _, err = websocket.Dial(ctx, "ws://127.0.0.1:"+strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)+"/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{bizOrigin}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return ctx, conn
}

// readUntil returns the first frame whose "type" matches want, skipping status
// pushes that belong to other jobs.
func readUntil(t *testing.T, ctx context.Context, conn *websocket.Conn, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rctx, cancel := context.WithTimeout(ctx, time.Second)
		_, data, err := conn.Read(rctx)
		cancel()
		if err != nil {
			t.Fatalf("waiting for %q: %v", want, err)
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("bad frame %s: %v", data, err)
		}
		if msg["type"] == want {
			return msg
		}
	}
	t.Fatalf("no %q frame arrived", want)
	return nil
}

func sendJSON(t *testing.T, ctx context.Context, conn *websocket.Conn, payload string) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// The front-end's printer picker reads msg.list and camelCase isDefault. The
// printers_list shape would leave it empty with no visible error.
func TestShortPrintersMessage(t *testing.T) {
	ctx, conn := bizHarness(t)
	sendJSON(t, ctx, conn, `{"type":"printers"}`)
	msg := readUntil(t, ctx, conn, "printers")
	list, ok := msg["list"].([]any)
	if !ok {
		t.Fatalf("printers response has no list array: %v", msg)
	}
	if len(list) != 1 {
		t.Fatalf("list = %v, want one printer", list)
	}
	first, _ := list[0].(map[string]any)
	if first["name"] != "Test Printer" {
		t.Fatalf("name = %v", first["name"])
	}
	// The front-end reads camelCase isDefault, not the snake_case is_default.
	if first["isDefault"] != true {
		t.Fatalf("isDefault = %v, want true (camelCase, as the front-end reads it)", first["isDefault"])
	}
}

// The documented spelling must keep working alongside it.
func TestUpstreamPrintersMessageStillWorks(t *testing.T) {
	ctx, conn := bizHarness(t)
	sendJSON(t, ctx, conn, `{"type":"get_printers_list","request_id":"r1"}`)
	msg := readUntil(t, ctx, conn, "printers_list")
	if msg["request_id"] != "r1" {
		t.Fatalf("request_id not echoed: %v", msg)
	}
	list, _ := msg["printers"].([]any)
	if len(list) != 1 {
		t.Fatalf("printers = %v", list)
	}
	first, _ := list[0].(map[string]any)
	if first["is_default"] != true {
		t.Fatalf("is_default = %v, want true (snake_case, documented shape)", first["is_default"])
	}
}

// A label PDF sent as data_base64 must be accepted and queued, exactly as the
// front-end's printPdfBase64ViaAgent expects.
func TestInlineBase64PDFIsQueued(t *testing.T) {
	ctx, conn := bizHarness(t)
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\n%%EOF\n"))
	sendJSON(t, ctx, conn, `{"type":"print","job_id":"compat-pdf","format":"pdf","data_base64":"`+pdf+`","copies":1,"scale_mode":"fit_width","direction":"portrait","center":true}`)
	msg := readUntil(t, ctx, conn, "job_status")
	if msg["job_id"] != "compat-pdf" || msg["status"] != "queued" {
		t.Fatalf("unexpected ack: %v", msg)
	}
}

// The front-end puts copies on every job, raw included; failing that job would
// stop TSPL labels from printing at all.
func TestRawWithCopiesIsQueued(t *testing.T) {
	ctx, conn := bizHarness(t)
	sendJSON(t, ctx, conn, `{"type":"print","job_id":"compat-raw","format":"raw","data_base64":"AAECAw==","copies":1}`)
	msg := readUntil(t, ctx, conn, "job_status")
	if msg["job_id"] != "compat-raw" {
		t.Fatalf("raw job not accepted: %v", msg)
	}
	if msg["status"] != "queued" {
		t.Fatalf("status = %v, want queued", msg["status"])
	}
}

// A bad scale_mode is reported, not ignored: the caller asked for a specific
// crop/fit and must not be left believing it took effect.
func TestUnknownScaleModeIsRejected(t *testing.T) {
	ctx, conn := bizHarness(t)
	sendJSON(t, ctx, conn, `{"type":"print","job_id":"compat-bad-scale","format":"raw-html","html":"<p>x</p>","scale_mode":"bogus"}`)
	msg := readUntil(t, ctx, conn, "error")
	if msg["error_code"] != string(domain.CodeInvalidMessage) {
		t.Fatalf("error_code = %v", msg["error_code"])
	}
}

// Sending both a URL and inline bytes is ambiguous; it must fail rather than
// silently pick one.
func TestBothSourcesRejected(t *testing.T) {
	ctx, conn := bizHarness(t)
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4"))
	sendJSON(t, ctx, conn, `{"type":"print","job_id":"compat-both","format":"pdf","file_url":"https://example.com/a.pdf","data_base64":"`+pdf+`"}`)
	msg := readUntil(t, ctx, conn, "error")
	if msg["error_code"] != string(domain.CodeInvalidMessage) {
		t.Fatalf("error_code = %v", msg["error_code"])
	}
}
