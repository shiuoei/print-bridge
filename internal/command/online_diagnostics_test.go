package command

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/logging"
	"github.com/shiuoei/print-bridge/internal/printer"
	"github.com/shiuoei/print-bridge/internal/store"
)

type testBackend struct {
	info   *domain.PrinterInfo
	prints int
	paper  *domain.Paper
	pdf    []byte
	fail   error
}
type testAgent struct{}

func (testAgent) ActiveJobs() []domain.Status { return nil }
func (testAgent) CancelJob(context.Context, string) (domain.Status, error) {
	return domain.Status{}, nil
}

func (p *testBackend) List(context.Context) ([]string, error) { return []string{"Test"}, nil }
func (p *testBackend) Info(_ context.Context, name string) (*domain.PrinterInfo, error) {
	if name != "Test" {
		return nil, errors.New("missing printer")
	}
	return p.info, nil
}
func (p *testBackend) PrintPDF(_ context.Context, path, name string, copies int, paper *domain.Paper) (printer.Submission, error) {
	p.prints++
	p.paper = paper
	if copies != 1 || name != "Test" {
		return printer.Submission{}, errors.New("wrong target or copies")
	}
	p.pdf, _ = os.ReadFile(path)
	return printer.Submission{Backend: "test"}, p.fail
}
func (p *testBackend) PrintRaw(context.Context, string, []byte) (printer.Submission, error) {
	return printer.Submission{}, errors.New("unexpected raw print")
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTestPrintSubmitsOnePageWithSelectedPaper(t *testing.T) {
	s := testStore(t)
	c := s.Config()
	c.Printing.DefaultPaper = &domain.Paper{WidthMM: 100, HeightMM: 180}
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	p := &testBackend{info: &domain.PrinterInfo{Name: "Test", Papers: []domain.PrinterPaper{{Name: "100 x 180", WidthMM: 100, HeightMM: 180}}}}
	r := Service{Store: s, Printer: p, Agent: testAgent{}}.Execute(context.Background(), Request{Command: "test.print", Name: "Test"})
	if r.Error != "" || r.Status != "submitted" || p.prints != 1 || p.paper.WidthMM != 100 || !strings.HasPrefix(string(p.pdf), "%PDF-") {
		t.Fatalf("response=%+v prints=%d paper=%+v pdf=%d", r, p.prints, p.paper, len(p.pdf))
	}
	if !strings.Contains(r.Message, "physical output unverified") {
		t.Fatal(r.Message)
	}
}

func TestTestPrintRejectsUnsupportedPaper(t *testing.T) {
	s := testStore(t)
	c := s.Config()
	c.Printing.DefaultPaper = &domain.Paper{WidthMM: 100, HeightMM: 180}
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	p := &testBackend{info: &domain.PrinterInfo{Name: "Test", Papers: []domain.PrinterPaper{{Name: "A4", WidthMM: 210, HeightMM: 297}}}}
	r := Service{Store: s, Printer: p, Agent: testAgent{}}.Execute(context.Background(), Request{Command: "test.print", Name: "Test"})
	if r.ErrorCode != domain.CodePaperNotFound || p.prints != 0 {
		t.Fatalf("response=%+v prints=%d", r, p.prints)
	}
}

func TestRemoteProbeUsesTestHeadersWithoutProcessingJobs(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-PrintBridge-Test") != "true" || r.Header.Get("Authorization") != "Bearer topsecret" || r.Header.Get("X-PrintBridge-Device-Id") != "device-1" || r.Header.Get("X-PrintBridge-Device-Name") != "Desk" {
			t.Errorf("headers: %v", r.Header)
		}
		if r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["event"] != "test" || len(body) != 1 {
				t.Errorf("body: %v %v", body, err)
			}
		}
		mu.Lock()
		got = append(got, r.Method)
		mu.Unlock()
		w.Write([]byte("token=topsecret"))
	}))
	defer srv.Close()
	s := testStore(t)
	c := s.Config()
	c.Remote.EndpointURL = srv.URL
	c.Remote.BearerToken = "topsecret"
	c.Remote.DeviceID = "device-1"
	c.Remote.DeviceName = "Desk"
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	r := Service{Store: s, RemoteClient: srv.Client(), Agent: testAgent{}}.Execute(context.Background(), Request{Command: "remote.test"})
	if r.Error != "" || r.RemoteTest == nil || r.RemoteTest.GET.StatusCode != 200 || r.RemoteTest.POST.StatusCode != 200 {
		t.Fatalf("response=%+v", r)
	}
	if strings.Contains(r.RemoteTest.GET.Response, "topsecret") {
		t.Fatal("token leaked")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "GET" || got[1] != "POST" {
		t.Fatal(got)
	}
}

func TestRemoteProbeCertificateError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	s := testStore(t)
	c := s.Config()
	c.Remote.EndpointURL = srv.URL
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	r := Service{Store: s, Agent: testAgent{}}.Execute(context.Background(), Request{Command: "remote.test"})
	if r.ErrorCode != domain.CodeRemoteUnavailable || r.RemoteTest == nil || r.RemoteTest.GET.Error != "certificate validation failed" {
		t.Fatalf("response=%+v", r)
	}
}

func TestRemoteProbeTimeout(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer srv.Close()
	s := testStore(t)
	c := s.Config()
	c.Remote.EndpointURL = srv.URL
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	r := Service{Store: s, RemoteClient: srv.Client(), Agent: testAgent{}}.Execute(ctx, Request{Command: "remote.test"})
	if r.ErrorCode != domain.CodeRemoteUnavailable || r.RemoteTest == nil || r.RemoteTest.GET.Error != "timeout" {
		t.Fatalf("response=%+v", r)
	}
}

func TestRemoteProbeReportsAuthorizationFailure(t *testing.T) {
	for _, status := range []int{401, 403, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); w.Write([]byte("denied")) }))
			defer srv.Close()
			s := testStore(t)
			c := s.Config()
			c.Remote.EndpointURL = srv.URL
			if err := s.SetConfig(c); err != nil {
				t.Fatal(err)
			}
			r := Service{Store: s, RemoteClient: srv.Client(), Agent: testAgent{}}.Execute(context.Background(), Request{Command: "remote.test"})
			if r.ErrorCode != domain.CodeRemoteUnavailable || r.RemoteTest.GET.StatusCode != status || r.RemoteTest.POST.StatusCode != status || r.RemoteTest.GET.Response != "denied" {
				t.Fatalf("report=%+v", r.RemoteTest)
			}
		})
	}
}

func TestLogsTailFiltersTimeAndLevel(t *testing.T) {
	b := logging.NewBuffer(10)
	logger := slog.New(logging.NewHandler(slog.NewTextHandler(io.Discard, nil), b))
	logger.Info("before")
	since := time.Now().Add(-time.Millisecond).UTC().Format(time.RFC3339)
	logger.Warn("password=hidden")
	r := Service{Store: testStore(t), Agent: testAgent{}, Logs: b}.Execute(context.Background(), Request{Command: "logs.tail", Level: "WARN", Since: since, Limit: 1})
	if r.Error != "" || len(r.Logs) != 1 || strings.Contains(r.Logs[0].Message, "hidden") {
		t.Fatalf("response=%+v", r)
	}
	r = Service{Store: testStore(t), Agent: testAgent{}, Logs: b}.Execute(context.Background(), Request{Command: "logs.tail", Level: "bogus"})
	if r.ErrorCode != domain.CodeArgumentInvalid {
		t.Fatalf("response=%+v", r)
	}
}
