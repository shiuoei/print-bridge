package security

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestDialPublicFallsBackAcrossAddressFamilies(t *testing.T) {
	previous := lookupIP
	lookupIP = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("2001:4860:4860::8888"), net.ParseIP("2001:4860:4860::8844"), net.ParseIP("8.8.8.8")}, nil
	}
	t.Cleanup(func() { lookupIP = previous })
	var attempts []string
	conn, err := dialPublicWith(context.Background(), "tcp", "example.com", "443", time.Second,
		func(_ context.Context, _, address string) (net.Conn, error) {
			attempts = append(attempts, address)
			if address != "[2001:4860:4860::8888]:443" {
				return nil, errors.New("unreachable")
			}
			client, peer := net.Pipe()
			peer.Close()
			return client, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if want := []string{"8.8.8.8:443", "[2001:4860:4860::8888]:443"}; !reflect.DeepEqual(attempts, want) {
		t.Fatalf("attempts = %v, want %v", attempts, want)
	}
}

func TestDialPublicRejectsMixedPrivateAnswerBeforeConnect(t *testing.T) {
	previous := lookupIP
	lookupIP = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("127.0.0.1")}, nil
	}
	t.Cleanup(func() { lookupIP = previous })
	dialed := false
	_, err := dialPublicWith(context.Background(), "tcp", "example.com", "443", time.Second,
		func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, nil
		})
	if !errors.Is(err, ErrBlockedAddress) || dialed {
		t.Fatalf("err = %v, dialed = %v; want blocked before dialing", err, dialed)
	}
}
