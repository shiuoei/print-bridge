package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/printer"
	"github.com/shiuoei/print-bridge/internal/store"
)

// fakePrinter records submitted raw payloads and reports deterministic
// capabilities so protocol tests never depend on installed hardware.
type fakePrinter struct {
	printed chan []byte
	// infoErr forces Info to fail, so error mapping can be asserted.
	infoErr  error
	printErr error
}

func (f *fakePrinter) List(context.Context) ([]string, error) {
	return []string{"Test Printer"}, nil
}

func (f *fakePrinter) Info(_ context.Context, name string) (*domain.PrinterInfo, error) {
	if f.infoErr != nil {
		return nil, f.infoErr
	}
	if name != "Test Printer" {
		return nil, printer.ErrPrinterNotFound
	}
	yes := true
	return &domain.PrinterInfo{
		Name:      name,
		IsDefault: &yes,
		Papers: []domain.PrinterPaper{
			{ID: "9", Name: "A4", WidthMM: 210, HeightMM: 297},
			{ID: "257", Name: "76mm * 130mm", WidthMM: 76, HeightMM: 130},
		},
		Trays:      []domain.PrinterTray{{ID: "1", Name: "Roll Paper Feeder"}},
		MediaTypes: []domain.PrinterMediaType{},
	}, nil
}

func (f *fakePrinter) PrintPDF(context.Context, string, string, int, *domain.Paper) (printer.Submission, error) {
	return printer.Submission{Backend: "fake"}, f.printErr
}

func (f *fakePrinter) PrintRaw(_ context.Context, _ string, b []byte) (printer.Submission, error) {
	if f.printed != nil {
		f.printed <- b
	}
	return printer.Submission{Backend: "fake"}, f.printErr
}

func TestSDKWireProtocol(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.Config()
	c.Security.AllowedOrigins = []string{"https://example.com"}
	c.Printing.DefaultPrinter = "Test Printer"
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	f := &fakePrinter{printed: make(chan []byte, 1)}
	a := New(s, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.worker(ctx)
	srv := httptest.NewServer(http.HandlerFunc(a.webSocket))
	defer srv.Close()
	wsURL := "ws" + srv.URL[4:]
	conn, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	send := func(v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		if err := conn.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]any {
		t.Helper()
		readCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, b, err := conn.Read(readCtx)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	send(map[string]any{"type": "ping", "time": 123})
	// "ready" is the protocol wire literal; clients surface it as agentStatus.
	if m := read(); m["type"] != "pong" || m["agent_status"] != "ready" || m["time"] != float64(123) {
		t.Fatalf("pong: %#v", m)
	}
	send(map[string]any{"type": "get_printers_list", "request_id": "r1"})
	if m := read(); m["type"] != "printers_list" || m["request_id"] != "r1" {
		t.Fatalf("printers: %#v", m)
	}
	send(map[string]any{"type": "print", "request_id": "r2", "job_id": "j1", "format": "raw", "data_base64": base64.StdEncoding.EncodeToString([]byte("PRINT 1,1"))})
	first := read()
	if first["status"] != "queued" {
		t.Fatalf("accept: %#v", first)
	}
	for i := 0; i < 3; i++ {
		m := read()
		if m["status"] == "submitted" {
			break
		}
		if i == 2 {
			t.Fatalf("no submitted status: %#v", m)
		}
	}
	select {
	case b := <-f.printed:
		if string(b) != "PRINT 1,1" {
			t.Fatalf("raw bytes: %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("raw job not printed")
	}
	send(map[string]any{"type": "print", "request_id": "r3", "job_id": "j1", "format": "raw", "data_base64": base64.StdEncoding.EncodeToString([]byte("DUP"))})
	if m := read(); m["type"] != "error" || m["error_code"] != "JOB_DUPLICATED" || m["request_id"] != "r3" {
		t.Fatalf("duplicate error: %#v", m)
	}
	send(map[string]any{"type": "print_batch", "request_id": "r4", "batch_id": "b1", "jobs": []map[string]any{{"job_id": "j2", "format": "raw", "data_base64": base64.StdEncoding.EncodeToString([]byte("BATCH"))}}})
	if m := read(); m["status"] != "queued" || m["job_id"] != "b1" {
		t.Fatalf("batch accept first: %#v", m)
	}
	if m := read(); m["status"] != "queued" || m["job_id"] != "j2" {
		t.Fatalf("job accept second: %#v", m)
	}
	for i := 0; i < 3; i++ {
		if m := read(); m["status"] == "submitted" {
			break
		} else if i == 2 {
			t.Fatalf("batch job not submitted: %#v", m)
		}
	}
}

func TestOriginRejected(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := New(s, &fakePrinter{})
	srv := httptest.NewServer(http.HandlerFunc(a.webSocket))
	defer srv.Close()
	_, resp, err := websocket.Dial(context.Background(), "ws"+srv.URL[4:], &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://bad.example"}}})
	if err == nil {
		t.Fatal("untrusted origin accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response: %v", resp)
	}
}

func TestOriginlessNodeSDKIsLoopbackOnly(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := New(s, &fakePrinter{})
	srv := httptest.NewServer(http.HandlerFunc(a.webSocket))
	defer srv.Close()
	conn, _, err := websocket.Dial(context.Background(), "ws"+srv.URL[4:], nil)
	if err != nil {
		t.Fatalf("loopback Node SDK connection rejected: %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "")
	cfg := s.Config()
	cfg.Security.AllowedIPs = append(cfg.Security.AllowedIPs, "192.0.2.1")
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.test/ws", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	resp := httptest.NewRecorder()
	a.webSocket(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("originless remote accepted: HTTP %d", resp.Code)
	}
}

func TestBatchRejectsAtomically(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.Config()
	c.Printing.DefaultPrinter = "Test Printer"
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	a := New(s, &fakePrinter{printed: make(chan []byte, 2)})
	jobs := []domain.Job{{JobID: "good", Format: "raw", DataBase64: base64.StdEncoding.EncodeToString([]byte("A"))}, {JobID: "bad", Format: "raw"}}
	if _, err := a.SubmitBatch(context.Background(), "batch-1", "req-1", jobs, nil); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if _, err := s.Latest(context.Background(), "good"); err == nil {
		t.Fatal("partial batch persisted")
	}
	jobs[1].DataBase64 = base64.StdEncoding.EncodeToString([]byte("B"))
	if _, err := a.SubmitBatch(context.Background(), "batch-1", "req-1", jobs, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitBatch(context.Background(), "batch-1", "req-2", jobs, nil); err == nil {
		t.Fatal("duplicate batch accepted")
	}
}
