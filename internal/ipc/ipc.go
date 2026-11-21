package ipc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/shiuoei/print-bridge/internal/command"
)

const maxFrame = 8 << 20

func SocketPath(dir string) string { return filepath.Join(dir, "run", "agent.sock") }

func Listen(dir string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0700); err != nil {
		return nil, err
	}
	return platformListen(SocketPath(dir))
}

func Serve(ctx context.Context, ln net.Listener, svc command.Service) {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handle(ctx, conn, svc)
	}
}

func handle(ctx context.Context, conn net.Conn, svc command.Service) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	var req command.Request
	if err := readFrame(conn, &req); err != nil {
		return
	}
	if req.ProtocolVersion != 1 {
		_ = writeFrame(conn, command.Response{ProtocolVersion: 1, RequestID: req.RequestID, Error: "unsupported protocol version"})
		return
	}
	resp := svc.Execute(ctx, req)
	_ = writeFrame(conn, resp)
}

func Call(ctx context.Context, dir string, req command.Request) (command.Response, error) {
	conn, err := platformDial(ctx, SocketPath(dir))
	if err != nil {
		return command.Response{}, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	req.ProtocolVersion = 1
	if err := writeFrame(conn, req); err != nil {
		return command.Response{}, err
	}
	var resp command.Response
	if err := readFrame(conn, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

func readFrame(r io.Reader, v any) error {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return err
	}
	if n == 0 || n > maxFrame {
		return errors.New("invalid IPC frame length")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxFrame {
		return errors.New("IPC frame too large")
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(b))); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
