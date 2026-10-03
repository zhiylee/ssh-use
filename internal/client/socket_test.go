package client

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/zhiylee/ssh-use/internal/protocol"
)

func TestConnectUsesDialer(t *testing.T) {
	origDial := dialUnix
	origStart := startDaemonFn
	t.Cleanup(func() { dialUnix = origDial; startDaemonFn = origStart })
	called := false
	dialUnix = func(path string) (net.Conn, error) {
		called = true
		c1, c2 := net.Pipe()
		go c2.Close()
		return c1, nil
	}
	conn, err := Connect()
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if !called {
		t.Fatal("dialer not called")
	}
}

func TestEnsureDaemonStartsWhenMissing(t *testing.T) {
	origDial := dialUnix
	origStart := startDaemonFn
	t.Cleanup(func() { dialUnix = origDial; startDaemonFn = origStart })
	attempts := 0
	started := false
	dialUnix = func(path string) (net.Conn, error) {
		attempts++
		if !started {
			return nil, net.ErrClosed
		}
		c1, c2 := net.Pipe()
		go c2.Close()
		return c1, nil
	}
	startDaemonFn = func() error {
		started = true
		return nil
	}
	if err := EnsureDaemon(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts < 2 || !started {
		t.Fatalf("attempts=%d started=%v", attempts, started)
	}
}

func TestRequestRoundTrip(t *testing.T) {
	origDial := dialUnix
	origStart := startDaemonFn
	t.Cleanup(func() { dialUnix = origDial; startDaemonFn = origStart })
	dialUnix = func(path string) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		go func() {
			defer serverConn.Close()
			dec := protocol.NewDecoder(serverConn)
			enc := protocol.NewEncoder(serverConn)
			var req protocol.Message
			_ = dec.Decode(&req)
			_ = enc.Encode(protocol.Message{Type: "ack", OK: true, ID: req.ID})
		}()
		return clientConn, nil
	}
	startDaemonFn = func() error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := Request(ctx, protocol.Message{Type: "snapshot", ID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID != "x" {
		t.Fatalf("resp=%#v", resp)
	}
}

func TestEnsureDaemonStartError(t *testing.T) {
	origDial := dialUnix
	origStart := startDaemonFn
	t.Cleanup(func() { dialUnix = origDial; startDaemonFn = origStart })
	dialUnix = func(path string) (net.Conn, error) { return nil, net.ErrClosed }
	startDaemonFn = func() error { return errors.New("start failed") }
	if err := EnsureDaemon(context.Background()); err == nil || err.Error() != "start failed" {
		t.Fatalf("err=%v", err)
	}
}

func TestRequestErrorResponse(t *testing.T) {
	origDial := dialUnix
	origStart := startDaemonFn
	t.Cleanup(func() { dialUnix = origDial; startDaemonFn = origStart })
	dialUnix = func(path string) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		go func() {
			defer serverConn.Close()
			dec := protocol.NewDecoder(serverConn)
			enc := protocol.NewEncoder(serverConn)
			var req protocol.Message
			_ = dec.Decode(&req)
			_ = enc.Encode(protocol.Message{Type: "ack", OK: false, Error: "bad"})
		}()
		return clientConn, nil
	}
	startDaemonFn = func() error { return nil }
	_, err := Request(context.Background(), protocol.Message{Type: "snapshot"})
	if err == nil || err.Error() != "bad" {
		t.Fatalf("err=%v", err)
	}
}

func TestRemoteNeverStartsLocalDaemon(t *testing.T) {
	t.Setenv("SSH_USE_CLIENT_CONFIG", "")
	t.Setenv("SSH_USE_CONFIG_PATH", t.TempDir()+"/config.yaml")
	t.Setenv("SSH_USE_SERVER", "127.0.0.1:1")
	t.Setenv("SSH_USE_TOKEN_FILE", t.TempDir()+"/missing-token")
	old := startDaemonFn
	t.Cleanup(func() { startDaemonFn = old })
	startDaemonFn = func() error { t.Fatal("remote failure spawned local daemon"); return nil }
	if _, err := Request(context.Background(), protocol.Message{Type: "ping"}); err == nil {
		t.Fatal("missing token accepted")
	}
}
