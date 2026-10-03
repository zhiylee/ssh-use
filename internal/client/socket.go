package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/zhiylee/ssh-use/internal/paths"
	"github.com/zhiylee/ssh-use/internal/protocol"
	"github.com/zhiylee/ssh-use/internal/remote"
)

var (
	dialUnix      = func(path string) (net.Conn, error) { return net.DialTimeout("unix", path, time.Second) }
	startDaemonFn = startDaemon
)

func Connect() (protocol.Connection, error) {
	cfg, err := remote.LoadClient()
	if err != nil {
		return nil, err
	}
	if cfg.Endpoint != "" {
		return remote.Dial(cfg)
	}
	return dialUnix(paths.SocketPath())
}

func EnsureDaemon(ctx context.Context) error {
	cfg, err := remote.LoadClient()
	if err != nil {
		return err
	}
	if cfg.Endpoint != "" {
		return nil
	} // Connect authenticates; never spawn a local fallback.
	conn, err := Connect()
	if err == nil {
		_ = conn.Close()
		return nil
	}
	if err := startDaemonFn(); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := Connect()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("daemon did not become ready; see %s", paths.LogPath())
}

func IsRemote() bool {
	cfg, err := remote.LoadClient()
	return err != nil || cfg.Endpoint != ""
}

func Endpoint() string {
	cfg, err := remote.LoadClient()
	if err != nil {
		return "invalid remote configuration"
	}
	if cfg.Endpoint != "" {
		return cfg.Endpoint
	}
	return paths.SocketPath()
}

func Request(ctx context.Context, msg protocol.Message) (protocol.Message, error) {
	if err := EnsureDaemon(ctx); err != nil {
		return protocol.Message{}, err
	}
	cfg, err := remote.LoadClient()
	if err != nil {
		return protocol.Message{}, err
	}
	if cfg.Endpoint != "" {
		return remote.Request(ctx, cfg, msg)
	}
	conn, err := Connect()
	if err != nil {
		return protocol.Message{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	enc := protocol.NewEncoder(conn)
	dec := protocol.NewDecoder(conn)
	if err := enc.Encode(msg); err != nil {
		return protocol.Message{}, err
	}
	var resp protocol.Message
	if err := dec.Decode(&resp); err != nil {
		return protocol.Message{}, err
	}
	if !resp.OK && resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

func startDaemon() error {
	if err := os.MkdirAll(paths.RuntimeDir(), 0o700); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(paths.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.Dir = filepath.Dir(exe)
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}
	return cmd.Process.Release()
}
