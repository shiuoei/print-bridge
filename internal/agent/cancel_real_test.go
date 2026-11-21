package agent

import (
	"context"
	"encoding/base64"
	"os"
	"testing"
	"time"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// A local cancellation must interrupt the active Chromium conversion, not
// merely record a cancelled status while the serial worker stays occupied.
func TestCancelActiveHTMLReleasesWorker(t *testing.T) {
	if os.Getenv("PRINTBRIDGE_REAL_CHROMIUM") != "1" {
		t.Skip("set PRINTBRIDGE_REAL_CHROMIUM=1 to test cancellation of a running browser")
	}
	for _, batch := range []bool{false, true} {
		name := "single"
		if batch {
			name = "batch"
		}
		t.Run(name, func(t *testing.T) {
			printed := make(chan []byte, 1)
			a, s := newTestAgent(t, &fakePrinter{printed: printed})
			htmlJob := domain.Job{JobID: "slow-html", Format: "raw-html", HTML: "<!doctype html><h1>Cancellation test</h1>", WaitMS: 30000}
			rawJob := domain.Job{JobID: "next-raw", Format: "raw", DataBase64: base64.StdEncoding.EncodeToString([]byte("NEXT"))}
			if batch {
				if _, _, err := a.submitBatch(t.Context(), "cancel-batch", "request", []domain.Job{htmlJob, rawJob}, nil, nil); err != nil {
					t.Fatal(err)
				}
			} else if _, _, err := a.submit(t.Context(), htmlJob, "request", nil, nil); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				st, err := s.Latest(t.Context(), htmlJob.JobID)
				if err == nil && st.Status == "downloading" {
					break
				}
				if err == nil && (st.Status == "failed" || st.Status == "submitted") {
					t.Fatalf("HTML job finished before cancellation: %#v", st)
				}
				if time.Now().After(deadline) {
					t.Fatalf("HTML conversion did not start: %#v, %v", st, err)
				}
				time.Sleep(20 * time.Millisecond)
			}
			// Give Chromium time to enter its wait, so this exercises an active
			// conversion rather than a job that is merely queued.
			time.Sleep(time.Second)
			started := time.Now()
			st, err := a.CancelJob(context.Background(), htmlJob.JobID)
			if err != nil || st.Status != "cancelled" {
				t.Fatalf("cancel: %#v, %v", st, err)
			}
			if !batch {
				if _, _, err := a.submit(t.Context(), rawJob, "next", nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case b := <-printed:
				if string(b) != "NEXT" {
					t.Fatalf("next payload = %q", b)
				}
			case <-time.After(12 * time.Second):
				t.Fatal("cancelled HTML conversion still occupies the serial worker")
			}
			if elapsed := time.Since(started); elapsed >= 12*time.Second {
				t.Fatalf("worker released after %s", elapsed)
			}
			latest, err := s.Latest(t.Context(), htmlJob.JobID)
			if err != nil || latest.Status != "cancelled" {
				t.Fatalf("cancelled job history: %#v, %v", latest, err)
			}
		})
	}
}
