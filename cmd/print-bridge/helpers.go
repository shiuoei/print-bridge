package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/domain"
	"github.com/shiuoei/print-bridge/internal/ipc"
	"github.com/shiuoei/print-bridge/internal/printer"
	"github.com/shiuoei/print-bridge/internal/store"
	"github.com/shiuoei/print-bridge/internal/transfer"
)

// ipcCall is the single IPC entry point so tests can substitute a fake.
func ipcCall(ctx context.Context, dir string, req command.Request) (command.Response, error) {
	req.ProtocolVersion = 1
	if req.RequestID == "" {
		req.RequestID = uuid.NewString()
	}
	return ipc.Call(ctx, dir, req)
}

// offlineConfig reads configuration without triggering the agent's crash
// recovery. It is only reachable once the agent is provably absent.
func offlineConfig(_ context.Context, dir string) (domain.Config, error) {
	s, err := store.Open(dir)
	if err != nil {
		return domain.Config{}, err
	}
	defer s.Close()
	return s.Config(), nil
}

func offlineSetConfig(_ context.Context, dir string, c domain.Config) error {
	s, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.SetConfig(c); err != nil {
		return domain.NewProtocolError(domain.CodeConfigInvalid, err.Error()).WithCause(err)
	}
	return nil
}

// globalUUID keeps request-id generation in one place.
func globalUUID() string { return uuid.NewString() }

// offlineService builds a command service for the offline executor.
func offlineService(s *store.Store) command.Service {
	return command.Service{Store: s, Printer: printer.New()}
}

var (
	_ = offlineService
	_ = transfer.Export
)
