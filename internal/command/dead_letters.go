package command

import (
	"context"

	"github.com/shiuoei/print-bridge/internal/domain"
)

// Dead-letter management.
//
// A status report is abandoned after its retry budget is spent. It is kept rather
// than dropped, because "the business server never learned this job finished" is a
// fact an operator needs to see and act on: retry it once the server is fixed, or
// discard it deliberately. Without these commands the rows were retained but
// unreachable, which is the same as losing them.

// remoteStats reports the outbox backlog: reports still being retried and reports
// that gave up.
func (s Service) remoteStats(ctx context.Context, r *Response) error {
	stats, err := s.Store.RemoteStats(ctx)
	if err != nil {
		return err
	}
	r.RemoteStats = &stats
	return nil
}

// listDeadLetters returns the abandoned reports, newest first.
func (s Service) listDeadLetters(ctx context.Context, req Request, r *Response) error {
	letters, err := s.Store.ListDeadLetters(ctx, req.Limit)
	if err != nil {
		return err
	}
	r.DeadLetters = letters
	return nil
}

// retryDeadLetters puts abandoned reports back in the queue. An empty selection
// retries every dead letter, which is the "server is fixed now" case.
func (s Service) retryDeadLetters(ctx context.Context, req Request, r *Response) error {
	// Retrying is only meaningful while polling is enabled: with polling off the
	// revived rows would sit in the queue and quietly age, which looks like the
	// retry failed.
	if !s.Store.Config().Remote.Enabled {
		return domain.NewProtocolError(domain.CodeRemotePaused,
			"remote polling is disabled; enable it before retrying dead letters")
	}
	n, err := s.Store.RetryDeadLetters(ctx, req.EventIDs)
	if err != nil {
		return err
	}
	if n == 0 && len(req.EventIDs) > 0 {
		return domain.NewProtocolErrorf(domain.CodeJobNotFound,
			"no dead letter matched the given event id(s)")
	}
	r.Removed = n
	r.Message = "dead letters queued for retry"
	return nil
}

// deleteDeadLetters discards abandoned reports. Deleting is irreversible, so the
// caller must say which ones or explicitly ask for all of them.
func (s Service) deleteDeadLetters(ctx context.Context, req Request, r *Response) error {
	if len(req.EventIDs) == 0 {
		return domain.NewProtocolError(domain.CodeInvalidMessage,
			"refusing to delete every dead letter without an explicit selection")
	}
	n, err := s.Store.DeleteDeadLetters(ctx, req.EventIDs)
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.NewProtocolErrorf(domain.CodeJobNotFound,
			"no dead letter matched the given event id(s)")
	}
	r.Removed = n
	r.Message = "dead letters deleted"
	return nil
}

// purgeDeadLetters removes every dead letter. It exists as a separate command so
// the destructive "all" form is never reachable by omitting an argument.
func (s Service) purgeDeadLetters(ctx context.Context, r *Response) error {
	n, err := s.Store.DeleteDeadLetters(ctx, nil)
	if err != nil {
		return err
	}
	r.Removed = n
	r.Message = "all dead letters deleted"
	return nil
}
