package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/ipc"
	"github.com/shiuoei/print-bridge/internal/logging"
	"github.com/shiuoei/print-bridge/internal/printer"
	"github.com/shiuoei/print-bridge/internal/store"
)

type cliTestAgent struct{}

func (cliTestAgent) ActiveJobs() []domain.Status { return nil }
func (cliTestAgent) CancelJob(context.Context, string) (domain.Status, error) {
	return domain.Status{}, nil
}

func TestOfflineOriginCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PRINTBRIDGE_DATA_DIR", dir)
	if code := run([]string{"origin", "add", "https://example.com"}); code != exitOK {
		t.Fatalf("exit code %d, want %d", code, exitOK)
	}
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Config().OriginAllowed("https://example.com") {
		t.Fatal("origin not saved")
	}
}

func TestOnlineOriginCommandUpdatesRunningStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PRINTBRIDGE_DATA_DIR", dir)
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ln, err := ipc.Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ipc.Serve(ctx, ln, command.Service{Store: s, Printer: printer.New()})
	if code := run([]string{"origin", "add", "https://online.example"}); code != exitOK {
		t.Fatalf("exit code %d, want %d", code, exitOK)
	}
	if !s.Config().OriginAllowed("https://online.example") {
		t.Fatal("running Agent config was not updated")
	}
}

func TestOriginAddDuplicateIsConflict(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PRINTBRIDGE_DATA_DIR", dir)
	if code := run([]string{"origin", "add", "https://example.com"}); code != exitOK {
		t.Fatalf("first add exit code %d", code)
	}
	if code := run([]string{"origin", "add", "https://example.com"}); code != exitConflict {
		t.Fatalf("duplicate add exit code %d, want %d", code, exitConflict)
	}
}

func TestOriginDeleteMissingIsConflict(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PRINTBRIDGE_DATA_DIR", dir)
	if code := run([]string{"origin", "delete", "https://absent.example"}); code != exitConflict {
		t.Fatalf("exit code %d, want %d", code, exitConflict)
	}
}

func TestInvalidArgumentsExitCode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PRINTBRIDGE_DATA_DIR", dir)
	cases := [][]string{
		{"nonsense-command"},
		{"service", "set-port", "abc"},
		{"service", "set-port", "70000"},
		{"paper", "set", "-1", "10"},
		{"remote", "set-url", "http://insecure.example"},
	}
	for _, args := range cases {
		if code := run(args); code != exitUsage {
			t.Errorf("%v exit code %d, want %d", args, code, exitUsage)
		}
	}
}

func TestPaperSetPersists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PRINTBRIDGE_DATA_DIR", dir)
	if code := run([]string{"paper", "set", "76", "130"}); code != exitOK {
		t.Fatalf("exit code %d", code)
	}
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	paper := s.Config().Printing.DefaultPaper
	if paper == nil || paper.WidthMM != 76 || paper.HeightMM != 130 {
		t.Fatalf("paper = %#v", paper)
	}
}

func TestServiceSetPortPersists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PRINTBRIDGE_DATA_DIR", dir)
	if code := run([]string{"service", "set-port", "18099"}); code != exitOK {
		t.Fatalf("exit code %d", code)
	}
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.Config().Service.Port; got != 18099 {
		t.Fatalf("port = %d", got)
	}
}

func TestRemoteDeviceIDGeneration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PRINTBRIDGE_DATA_DIR", dir)
	if code := run([]string{"remote", "generate-device-id"}); code != exitOK {
		t.Fatalf("exit code %d", code)
	}
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := s.Config().Remote.DeviceID
	if len(id) != 36 {
		t.Fatalf("device id = %q, want a UUID", id)
	}
}

// TestDoctorOfflineRunsWithoutAgent proves doctor works with no agent, which is
// exactly when operators need it.
func TestDoctorOfflineRunsWithoutAgent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PRINTBRIDGE_DATA_DIR", dir)
	// An empty data directory has no default printer and no origins, so doctor
	// must report failures; the exit code must be the doctor code, not a crash.
	if code := run([]string{"doctor"}); code != exitDoctorFail && code != exitOK {
		t.Fatalf("doctor exit code %d", code)
	}
}

func TestOnlineDiagnosticCommandParsing(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		command string
	}{
		{[]string{"test-print", "HP"}, "test.print"},
		{[]string{"test-remote"}, "remote.test"},
		{[]string{"logs", "--level", "WARN", "--since", "2026-11-16T10:00:00+08:00", "--limit", "50"}, "logs.tail"},
	} {
		req, policy, err := parseCommand(tc.args)
		if err != nil || policy != onlineOnly || req.Command != tc.command {
			t.Fatalf("args=%v req=%+v policy=%v err=%v", tc.args, req, policy, err)
		}
	}
	for _, args := range [][]string{{"test-print"}, {"test-print", "HP", "extra"}, {"test-remote", "extra"}, {"logs", "--limit", "201"}, {"logs", "--since", "today"}} {
		if _, _, err := parseCommand(args); err == nil {
			t.Fatalf("accepted invalid args %v", args)
		}
	}
}

func TestOnlineRemoteAndLogsUseIPC(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PRINTBRIDGE_DATA_DIR", dir)
	var reject atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-PrintBridge-Test") != "true" {
			t.Errorf("missing test header")
		}
		if reject.Load() {
			w.WriteHeader(http.StatusUnauthorized)
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.Config()
	c.Remote.EndpointURL = srv.URL
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	b := logging.NewBuffer(10)
	slog.New(logging.NewHandler(slog.NewTextHandler(io.Discard, nil), b)).Warn("probe ready")
	ln, err := ipc.Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ipc.Serve(ctx, ln, command.Service{Store: s, Printer: printer.New(), Agent: cliTestAgent{}, Logs: b, RemoteClient: srv.Client()})
	if code := run([]string{"test-remote"}); code != exitOK {
		t.Fatalf("remote exit %d", code)
	}
	if code := run([]string{"logs", "--level", "WARN", "--limit", "1"}); code != exitOK {
		t.Fatalf("logs exit %d", code)
	}
	reject.Store(true)
	if code := run([]string{"test-remote"}); code != exitDoctorFail {
		t.Fatalf("remote failure exit %d", code)
	}
}
