//go:build windows

package ipc

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

const pipeName = `\\.\pipe\print-bridge-agent`

func platformListen(string) (net.Listener, error) {
	return winio.ListenPipe(pipeName, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;OW)", MessageMode: false, InputBufferSize: 65536, OutputBufferSize: 65536})
}
func platformDial(ctx context.Context, _ string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, pipeName)
}
