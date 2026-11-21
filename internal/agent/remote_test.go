package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/store"
)

func TestRemoteAuthFailurePausesWithoutConsumingOutboxRetry(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	cfg := s.Config()
	cfg.Remote.EndpointURL = server.URL
	ctx := context.Background()
	a := New(s, &fakePrinter{})
	var pause remotePauseError
	if err := a.pollRemote(ctx, server.Client(), cfg); !errors.As(err, &pause) || pause.status != http.StatusUnauthorized {
		t.Fatalf("poll error: %v", err)
	}
	if err := s.AddRemoteEvent(ctx, store.RemoteEvent{EventID: "e1", JobID: "j1", OccurredAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if err := a.reportRemote(ctx, server.Client(), cfg); !errors.As(err, &pause) || pause.status != http.StatusUnauthorized {
		t.Fatalf("report error: %v", err)
	}
	events, err := s.DueRemoteEvents(ctx, 10)
	if err != nil || len(events) != 1 || events[0].Attempts != 0 {
		t.Fatalf("outbox retry consumed: %#v %v", events, err)
	}
}
