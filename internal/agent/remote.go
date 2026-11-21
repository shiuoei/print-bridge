package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/store"
)

type remoteEnvelope struct {
	Type      string       `json:"type"`
	RequestID string       `json:"request_id"`
	BatchID   string       `json:"batch_id"`
	JobID     string       `json:"job_id"`
	Jobs      []domain.Job `json:"jobs"`
	domain.Job
}

type remotePauseError struct{ status int }

func (e remotePauseError) Error() string {
	return fmt.Sprintf("remote HTTP %d; polling paused until configuration changes", e.status)
}

func pauseStatus(code int) error {
	if code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusNotFound {
		return remotePauseError{status: code}
	}
	return nil
}

func (a *Agent) remoteLoop(ctx context.Context) {
	client := &http.Client{Timeout: 20 * time.Second}
	var paused *domain.Config
	for {
		cfg := a.store.Config()
		if paused != nil && paused.Remote != cfg.Remote {
			paused = nil
		}
		if !cfg.Remote.Enabled {
			paused = nil
		} else if paused == nil {
			if err := a.pollRemote(ctx, client, cfg); err != nil {
				slog.Warn("remote poll", "error", err)
				var stop remotePauseError
				if errors.As(err, &stop) {
					paused = &cfg
				}
			}
			if paused == nil {
				if err := a.reportRemote(ctx, client, cfg); err != nil {
					slog.Warn("remote report", "error", err)
					var stop remotePauseError
					if errors.As(err, &stop) {
						paused = &cfg
					}
				}
			}
		}
		interval := time.Duration(cfg.Remote.PollIntervalSeconds) * time.Second
		if interval < time.Second {
			interval = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func remoteHeaders(req *http.Request, c domain.Config) {
	if c.Remote.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.Remote.BearerToken)
	}
	if c.Remote.DeviceID != "" {
		req.Header.Set("X-PrintBridge-Device-Id", c.Remote.DeviceID)
	}
	if c.Remote.DeviceName != "" {
		req.Header.Set("X-PrintBridge-Device-Name", c.Remote.DeviceName)
	}
}

func (a *Agent) pollRemote(ctx context.Context, client *http.Client, c domain.Config) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Remote.EndpointURL, nil)
	if err != nil {
		return err
	}
	remoteHeaders(req, c)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if err := pauseStatus(resp.StatusCode); err != nil {
			return err
		}
		return fmt.Errorf("remote GET: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(b)) == 0 || string(bytes.TrimSpace(b)) == "null" {
		return nil
	}
	var raw []json.RawMessage
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte("[")) {
		if err := json.Unmarshal(b, &raw); err != nil {
			return err
		}
	} else {
		raw = []json.RawMessage{b}
	}
	for _, item := range raw {
		var env remoteEnvelope
		if err := json.Unmarshal(item, &env); err != nil {
			continue
		}
		switch env.Type {
		case "print":
			var j domain.Job
			if err := json.Unmarshal(item, &j); err != nil {
				continue
			}
			a.enqueueRemoteJobs(ctx, env.RequestID, "", []domain.Job{j})
		case "print_batch":
			if len(env.Jobs) == 0 {
				continue
			}
			// A batch is idempotent as a whole: the batch id is recorded once, and
			// a re-delivered batch is skipped entirely instead of re-submitting
			// whichever member jobs happen not to be in history yet.
			a.enqueueRemoteJobs(ctx, env.RequestID, env.BatchID, env.Jobs)
		default:
			// An unknown envelope is not silently accepted as a single job: the
			// payload shape differs, and guessing would enqueue garbage.
			continue
		}
	}
	return nil
}

// enqueueRemoteJobs records and submits the new jobs of one remote item.
//
// Dedup is keyed on the remote job table rather than on task history, so pruning
// history cannot resurrect a job the remote server already delivered. Jobs whose
// id was seen before are skipped; a batch id seen before skips the whole batch.
func (a *Agent) enqueueRemoteJobs(ctx context.Context, requestID, batchID string, jobs []domain.Job) {
	if batchID != "" {
		fresh, err := a.store.RecordRemoteBatch(ctx, batchID)
		if err != nil {
			slog.Error("remote batch dedup", "batch_id", batchID, "error", err)
			return
		}
		if !fresh {
			slog.Info("remote batch already processed", "batch_id", batchID)
			return
		}
	}
	notify := func(st domain.Status) { a.enqueueRemoteStatus(st) }
	if batchID == "" {
		job := jobs[0]
		if job.JobID == "" {
			return
		}
		fresh, err := a.store.RecordRemoteJob(ctx, job.JobID)
		if err != nil {
			slog.Error("remote job dedup", "job_id", job.JobID, "error", err)
			return
		}
		if !fresh {
			return
		}
		if _, err := a.SubmitWithSource(ctx, job, requestID, notify, domain.SourceRemote); err != nil {
			slog.Warn("remote job rejected", "job_id", job.JobID, "error", err)
		}
		return
	}
	var fresh []domain.Job
	for _, job := range jobs {
		if job.JobID == "" {
			continue
		}
		ok, err := a.store.RecordRemoteJob(ctx, job.JobID)
		if err != nil {
			slog.Error("remote job dedup", "job_id", job.JobID, "error", err)
			continue
		}
		if ok {
			fresh = append(fresh, job)
		}
	}
	if len(fresh) == 0 {
		return
	}
	if _, err := a.SubmitBatchWithSource(ctx, batchID, requestID, fresh, notify, domain.SourceRemote); err != nil {
		slog.Warn("remote batch rejected", "batch_id", batchID, "error", err)
	}
}

func (a *Agent) enqueueRemoteStatus(st domain.Status) {
	status := ""
	switch st.Status {
	case "queued":
		status = "accepted"
	case "submitted":
		status = "success"
	case "failed", "cancelled":
		status = "failed"
	default:
		return
	}
	e := store.RemoteEvent{EventID: uuid.NewString(), RequestID: st.RequestID, JobID: st.JobID, BatchID: st.BatchID, Status: status, Message: st.Message, OccurredAt: st.At.UTC().Format(time.RFC3339)}
	if err := a.store.AddRemoteEvent(context.Background(), e); err != nil {
		slog.Error("remote outbox", "error", err)
	}
}

func (a *Agent) reportRemote(ctx context.Context, client *http.Client, c domain.Config) error {
	events, err := a.store.DueRemoteEvents(ctx, 20)
	if err != nil {
		return err
	}
	for _, e := range events {
		if e.Attempts >= c.Remote.MaxReportRetries {
			// The event already exhausted its retries in an earlier pass; record
			// that as the reason so the dead-letter list can explain it.
			if err := a.store.FailRemoteEvent(ctx, e.EventID, e.Attempts, "retry limit already reached"); err != nil {
				return err
			}
			continue
		}
		payload := map[string]any{"event": "status", "event_id": e.EventID, "request_id": e.RequestID, "job_id": e.JobID, "status": e.Status, "message": e.Message, "occurred_at": e.OccurredAt, "device_id": c.Remote.DeviceID, "device_name": c.Remote.DeviceName}
		// A batch member reports its batch id so a server keying on batches can
		// correlate the report; a single job omits the key entirely rather than
		// sending an empty string.
		if e.BatchID != "" {
			payload["batch_id"] = e.BatchID
		}
		b, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Remote.EndpointURL, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		remoteHeaders(req, c)
		resp, err := client.Do(req)
		failure := ""
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				_ = a.store.AckRemoteEvent(ctx, e.EventID)
				continue
			}
			failure = fmt.Sprintf("HTTP %d", resp.StatusCode)
			if err := pauseStatus(resp.StatusCode); err != nil {
				return err
			}
		} else {
			failure = err.Error()
		}
		attempts := e.Attempts + 1
		if attempts >= c.Remote.MaxReportRetries {
			if err := a.store.FailRemoteEvent(ctx, e.EventID, attempts, failure); err != nil {
				return err
			}
			slog.Warn("remote report retries exhausted", "event_id", e.EventID, "job_id", e.JobID, "error", failure)
			continue
		}
		backoff := time.Duration(1<<min(e.Attempts, 8)) * time.Second
		if err := a.store.RetryRemoteEvent(ctx, e.EventID, attempts, time.Now().Add(backoff)); err != nil {
			return err
		}
	}
	return nil
}
