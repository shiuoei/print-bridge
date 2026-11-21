//go:build !windows

package ipc

import (
	"context"
	"net"
	"os"
)

func platformListen(path string) (net.Listener, error) {
	if _, err := os.Stat(path); err == nil {
		if c, e := net.Dial("unix", path); e == nil {
			c.Close()
			return nil, os.ErrExist
		}
		os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0660); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
func platformDial(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}
