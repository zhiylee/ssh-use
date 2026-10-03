package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhiylee/ssh-use/internal/audit"
	"github.com/zhiylee/ssh-use/internal/config"
	"github.com/zhiylee/ssh-use/internal/model"
	"github.com/zhiylee/ssh-use/internal/protocol"
	"github.com/zhiylee/ssh-use/internal/remote"
	"github.com/zhiylee/ssh-use/internal/sshpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type remoteExecutor struct {
	fakeExecutor
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
}

func (f *remoteExecutor) Execute(ctx context.Context, opts sshpool.ExecOptions) (sshpool.ExecResult, error) {
	f.calls.Add(1)
	if f.started != nil {
		close(f.started)
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return sshpool.ExecResult{}, ctx.Err()
		}
	}
	opts.Stdout([]byte("remote output\n"))
	code := 7
	return sshpool.ExecResult{RemoteExitCode: &code}, fmt.Errorf("exit 7")
}

type remoteFixture struct {
	s            *Server
	executor     *remoteExecutor
	dir          string
	admin, agent remote.ClientConfig
	listener     *trackingListener
}

func newRemoteFixture(t *testing.T, setup ...func(*Server)) *remoteFixture {
	t.Helper()
	s := newTestServer(t)
	t.Setenv("SSH_USE_DATA_DIR", t.TempDir())
	store, err := audit.Open(true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s.audit = store
	s.remote = true
	fake := &remoteExecutor{}
	s.pool = fake
	for _, configure := range setup {
		configure(s)
	}
	s.cfg.Hosts["h"] = config.Host{Addr: "10.0.0.1"}
	if err := config.Save(s.cfg); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "server")
	if err := remote.Init(dir, "127.0.0.1", "7443"); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tracked := &trackingListener{Listener: listener}
	listener = tracked
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveRPC(ctx, listener, s, filepath.Join(dir, "auth.yaml"), cert)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	client := func(role string) remote.ClientConfig {
		return remote.ClientConfig{Endpoint: listener.Addr().String(), CAFile: filepath.Join(dir, role, "ca.pem"), TokenFile: filepath.Join(dir, role, "token")}
	}
	return &remoteFixture{s: s, executor: fake, dir: dir, admin: client("admin"), agent: client("agent"), listener: tracked}
}

func remoteConnect(t *testing.T, cfg remote.ClientConfig) (protocol.Connection, *protocol.Encoder, *protocol.Decoder) {
	t.Helper()
	conn, err := remote.Dial(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	return conn, protocol.NewEncoder(conn), protocol.NewDecoder(conn)
}
func remoteRequest(t *testing.T, cfg remote.ClientConfig, req protocol.Message) protocol.Message {
	t.Helper()
	conn, enc, dec := remoteConnect(t, cfg)
	defer conn.Close()
	if err := enc.Encode(req); err != nil {
		t.Fatal(err)
	}
	var resp protocol.Message
	if err := dec.Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRemoteHostCRUDPermissionsAndPersistence(t *testing.T) {
	f := newRemoteFixture(t)
	list := remoteRequest(t, f.admin, protocol.Message{Type: "hosts.list"})
	if !list.OK || len(list.Hosts) != 1 {
		t.Fatalf("list: %#v", list)
	}
	add := protocol.Message{Type: "hosts.add", Host: "prod", ConfigRevision: list.ConfigRevision, HostConfig: &config.Host{Addr: "192.0.2.10", Tags: []string{"prod"}}}
	if got := remoteRequest(t, f.agent, add); got.SSHUseErrorCode != "forbidden" {
		t.Fatalf("agent write: %#v", got)
	}
	if got := remoteRequest(t, f.admin, add); !got.OK {
		t.Fatalf("add: %#v", got)
	}
	add.Host = "other"
	if got := remoteRequest(t, f.admin, add); got.SSHUseErrorCode != "config_conflict" {
		t.Fatalf("stale write: %#v", got)
	}
	get := remoteRequest(t, f.agent, protocol.Message{Type: "hosts.get", Host: "prod"})
	if !get.OK || get.HostConfig.Addr != "192.0.2.10" {
		t.Fatalf("get: %#v", get)
	}
	addr := "192.0.2.11"
	tags := []string{}
	update := remoteRequest(t, f.admin, protocol.Message{Type: "hosts.update", Host: "prod", ConfigRevision: get.ConfigRevision, HostPatch: &protocol.HostPatch{Addr: &addr, Tags: &tags}})
	if !update.OK {
		t.Fatalf("update: %#v", update)
	}
	cfg, err := config.Load()
	if err != nil || cfg.Hosts["prod"].Addr != addr || len(cfg.Hosts["prod"].Tags) != 0 {
		t.Fatalf("persisted: %#v %v", cfg, err)
	}
	if got := remoteRequest(t, f.admin, protocol.Message{Type: "hosts.delete", Host: "prod", ConfigRevision: update.ConfigRevision}); !got.OK {
		t.Fatalf("delete: %#v", got)
	}
	if got := remoteRequest(t, f.admin, protocol.Message{Type: "exec", Host: "prod", Command: "uptime", RequestID: "deleted"}); got.SSHUseErrorCode != "host_not_found" {
		t.Fatalf("deleted host executed: %#v", got)
	}
	for _, kind := range []string{"approval.decide", "policy.set_mode", "daemon.emergency_stop", "snapshot"} {
		if got := remoteRequest(t, f.agent, protocol.Message{Type: kind}); got.SSHUseErrorCode != "forbidden" {
			t.Fatalf("agent %s: %#v", kind, got)
		}
	}
	conn, enc, dec := remoteConnect(t, f.agent)
	defer conn.Close()
	if err := enc.Encode(protocol.Message{Type: "subscribe_events"}); err != nil {
		t.Fatal(err)
	}
	var event protocol.Message
	if err := dec.Decode(&event); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("agent subscription: %v", err)
	}
}

func TestRemoteExecStreamsDeduplicatesAndKeepsIdentity(t *testing.T) {
	f := newRemoteFixture(t)
	conn, enc, dec := remoteConnect(t, f.agent)
	defer conn.Close()
	req := protocol.Message{Type: "exec", Host: "h", Command: "uptime", RequestID: "once", Source: "forged-admin"}
	if err := enc.Encode(req); err != nil {
		t.Fatal(err)
	}
	id := ""
	var output bytes.Buffer
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.ID != "" {
			id = msg.ID
		}
		if msg.Type == "stdout_chunk" {
			data, _ := protocol.DecodeData(msg)
			output.Write(data)
		}
		if msg.Type == "final" {
			if msg.RemoteExitCode == nil || *msg.RemoteExitCode != 7 {
				t.Fatalf("final: %#v", msg)
			}
			break
		}
	}
	if output.String() != "remote output\n" {
		t.Fatal(output.String())
	}
	dup := remoteRequest(t, f.agent, req)
	if !dup.OK || dup.ID != id || f.executor.calls.Load() != 1 {
		t.Fatalf("duplicate: %#v calls=%d", dup, f.executor.calls.Load())
	}
	if dup.Record.Source != "device:agent" {
		t.Fatalf("forged source: %#v", dup.Record)
	}
	req.Command = "whoami"
	if got := remoteRequest(t, f.agent, req); got.SSHUseErrorCode != "request_conflict" {
		t.Fatalf("reused ID: %#v", got)
	}
	get := remoteRequest(t, f.agent, protocol.Message{Type: "command.get", ID: id})
	if !get.OK || get.Record.Status != model.StatusFailed {
		t.Fatalf("get: %#v", get)
	}
	// Simulate a restarted process: the durable request marker still prevents execution.
	f.s.mu.Lock()
	delete(f.s.commands, id)
	f.s.order = nil
	f.s.mu.Unlock()
	req.Command = "uptime"
	if got := remoteRequest(t, f.agent, req); !got.OK || got.ID != id || f.executor.calls.Load() != 1 {
		t.Fatalf("persisted duplicate: %#v", got)
	}
}

func TestRemoteDisconnectDoesNotCancelExecution(t *testing.T) {
	f := newRemoteFixture(t)
	f.executor.started = make(chan struct{})
	f.executor.release = make(chan struct{})
	conn, enc, dec := remoteConnect(t, f.agent)
	_ = enc.Encode(protocol.Message{Type: "exec", Host: "h", Command: "uptime", RequestID: "disconnect"})
	var created protocol.Message
	if err := dec.Decode(&created); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.executor.started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	conn.Close()
	close(f.executor.release)
	waitFor(t, func() bool {
		info := remoteRequest(t, f.agent, protocol.Message{Type: "command.get", ID: created.ID})
		return info.Record != nil && info.Record.Status == model.StatusFailed
	})
}

func TestRemoteApprovalFromAnotherConnection(t *testing.T) {
	f := newRemoteFixture(t)
	if err := f.s.setPolicyMode(config.ModeApproval); err != nil {
		t.Fatal(err)
	}
	conn, enc, dec := remoteConnect(t, f.agent)
	defer conn.Close()
	_ = enc.Encode(protocol.Message{Type: "exec", Host: "h", Command: "uptime", RequestID: "approve"})
	id := ""
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "approval.waiting" {
			id = msg.ID
			break
		}
	}
	if got := remoteRequest(t, f.agent, protocol.Message{Type: "approval.decide", ID: id, Decision: "approve"}); got.OK {
		t.Fatal("agent approved own task")
	}
	if got := remoteRequest(t, f.admin, protocol.Message{Type: "approval.decide", ID: id, Decision: "approve"}); !got.OK {
		t.Fatalf("admin approve: %#v", got)
	}
	if got := remoteRequest(t, f.admin, protocol.Message{Type: "approval.decide", ID: id, Decision: "approve"}); got.OK {
		t.Fatal("second approval accepted")
	}
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "final" {
			break
		}
	}
	if f.executor.calls.Load() != 1 {
		t.Fatal("incorrect execution count")
	}
}

func TestRemoteUploadStreamsMultipleFrames(t *testing.T) {
	f := newRemoteFixture(t)
	if err := f.s.setPolicyMode(config.ModeAuto); err != nil {
		t.Fatal(err)
	}
	conn, enc, dec := remoteConnect(t, f.agent)
	defer conn.Close()
	data := bytes.Repeat([]byte{0, 1, 255, 4}, 400000) // total exceeds the per-frame bound
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	_ = enc.Encode(protocol.Message{Type: "file.transfer", Host: "h", Direction: "upload", LocalPath: "/client/only", RemotePath: "/target/file", Size: int64(len(data)), Atomic: true})
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "transfer.ready" {
			seq := int64(0)
			for offset := 0; offset < len(data); offset += 32768 {
				seq++
				end := min(offset+32768, len(data))
				if err := enc.Encode(protocol.Chunk("transfer.chunk", msg.ID, seq, data[offset:end])); err != nil {
					t.Fatal(err)
				}
			}
			_ = enc.Encode(protocol.Message{Type: "transfer.eof", ID: msg.ID, Bytes: int64(len(data)), Checksum: digest})
			break
		}
	}
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "final" {
			if !msg.OK {
				t.Fatalf("transfer: %#v", msg)
			}
			break
		}
	}
}

func TestRemoteTLSAndRevocation(t *testing.T) {
	f := newRemoteFixture(t)
	check := func(cfg remote.ClientConfig) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := remote.Request(ctx, cfg, protocol.Message{Type: "ping"}); err == nil {
			t.Fatal("invalid TLS trust or token accepted")
		}
	}
	bad := f.agent
	bad.CAFile = ""
	check(bad)
	bad = f.agent
	bad.TokenFile = filepath.Join(t.TempDir(), "bad-token")
	_ = os.WriteFile(bad.TokenFile, bytes.Repeat([]byte("x"), 64), 0600)
	check(bad)
	if err := os.WriteFile(filepath.Join(f.dir, "auth.yaml"), []byte("clients: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	check(f.agent)
}

func TestApprovalConfigChangeDoesNotExecuteOldTarget(t *testing.T) {
	f := newRemoteFixture(t)
	if err := f.s.setPolicyMode(config.ModeApproval); err != nil {
		t.Fatal(err)
	}
	conn, enc, dec := remoteConnect(t, f.agent)
	defer conn.Close()
	_ = enc.Encode(protocol.Message{Type: "exec", Host: "h", Command: "uptime", RequestID: "changed-config"})
	id := ""
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "approval.waiting" {
			id = msg.ID
			break
		}
	}
	get := remoteRequest(t, f.admin, protocol.Message{Type: "hosts.get", Host: "h"})
	addr := "192.0.2.100"
	if got := remoteRequest(t, f.admin, protocol.Message{Type: "hosts.update", Host: "h", ConfigRevision: get.ConfigRevision, HostPatch: &protocol.HostPatch{Addr: &addr}}); !got.OK {
		t.Fatal(got)
	}
	if got := remoteRequest(t, f.admin, protocol.Message{Type: "approval.decide", ID: id, Decision: "approve"}); !got.OK {
		t.Fatal(got)
	}
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "final" {
			if msg.SSHUseErrorCode != "config_changed" {
				t.Fatalf("final: %#v", msg)
			}
			break
		}
	}
	if f.executor.calls.Load() != 0 {
		t.Fatal("executed changed target")
	}
}

func TestDataDirectoryHasSingleOwner(t *testing.T) {
	t.Setenv("SSH_USE_DATA_DIR", t.TempDir())
	unlock, err := lockInstance()
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockInstance(); err == nil {
		second()
		unlock()
		t.Fatal("second process acquired data directory")
	}
	unlock()
	third, err := lockInstance()
	if err != nil {
		t.Fatal(err)
	}
	third()
}

// Track physical connections to verify multiplexing and force transport failures.
type trackingListener struct {
	net.Listener
	count       atomic.Int64
	mu          sync.Mutex
	connections []net.Conn
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.count.Add(1)
		l.mu.Lock()
		l.connections = append(l.connections, c)
		l.mu.Unlock()
	}
	return c, err
}
func (l *trackingListener) disconnect() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.connections {
		_ = c.Close()
	}
	l.connections = nil
}
func TestRemoteMultiplexesAndRevokesExistingChannel(t *testing.T) {
	f := newRemoteFixture(t)
	conn, enc, dec := remoteConnect(t, f.admin)
	defer conn.Close()
	if err := enc.Encode(protocol.Message{Type: "subscribe_events"}); err != nil {
		t.Fatal(err)
	}
	var initial protocol.Message
	if err := dec.Decode(&initial); err != nil || initial.Type != "snapshot" {
		t.Fatalf("subscribe: %#v %v", initial, err)
	}
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		reply, err := remote.Request(ctx, f.admin, protocol.Message{Type: "hosts.list"})
		cancel()
		if err != nil || !reply.OK {
			t.Fatalf("multiplexed request: %#v %v", reply, err)
		}
	}
	if f.listener.count.Load() != 1 {
		t.Fatalf("RPCs created %d physical connections", f.listener.count.Load())
	}
	if err := os.WriteFile(filepath.Join(f.dir, "auth.yaml"), []byte("clients: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := remote.Request(ctx, f.admin, protocol.Message{Type: "hosts.list"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("revoked token on existing channel: %v", err)
	}
}
func TestRemoteEventSubscriptionReconnectsWithCursor(t *testing.T) {
	f := newRemoteFixture(t)
	conn, enc, dec := remoteConnect(t, f.admin)
	defer conn.Close()
	if err := enc.Encode(protocol.Message{Type: "subscribe_events"}); err != nil {
		t.Fatal(err)
	}
	var m protocol.Message
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	f.s.setPaused(true)
	for m.Type != "daemon.paused" {
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
	}
	cursor := m.Cursor
	f.listener.disconnect()
	f.s.setPaused(false)
	reconnected := false
	for {
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		if m.Type == "stream.connected" {
			reconnected = true
		}
		if m.Type == "daemon.paused" && !m.Paused && m.Cursor > cursor {
			break
		}
	}
	if !reconnected {
		t.Fatal("subscription did not reconnect")
	}
}
func TestRemoteEventCursorGapRefreshesSnapshot(t *testing.T) {
	f := newRemoteFixture(t)
	// Wait for RPC initialization before publishing.
	_ = remoteRequest(t, f.admin, protocol.Message{Type: "ping"})
	for i := 0; i < 1050; i++ {
		f.s.broadcast(protocol.Message{Type: "connection.updated"})
	}
	conn, enc, dec := remoteConnect(t, f.admin)
	defer conn.Close()
	if err := enc.Encode(protocol.Message{Type: "subscribe_events", Cursor: 1}); err != nil {
		t.Fatal(err)
	}
	var m protocol.Message
	if err := dec.Decode(&m); err != nil || m.Type != "snapshot" || m.Cursor < 1050 {
		t.Fatalf("cursor gap: %#v %v", m, err)
	}
}
func TestRemoteBinaryDownloadWaitsForCommit(t *testing.T) {
	f := newRemoteFixture(t)
	if err := f.s.setPolicyMode(config.ModeAuto); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{0, 255, 128, 10}, 400000)
	f.executor.stdout = data
	conn, enc, dec := remoteConnect(t, f.agent)
	defer conn.Close()
	if err := enc.Encode(protocol.Message{Type: "file.transfer", Host: "h", Direction: "download", LocalPath: "/client/file", RemotePath: "/remote/file"}); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	var seq int64
	for {
		var m protocol.Message
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		switch m.Type {
		case "transfer.chunk":
			if m.Encoding != "" || len(m.Payload) > protocol.ChunkSize || m.Seq != seq+1 {
				t.Fatalf("binary frame: %#v", m)
			}
			seq = m.Seq
			got.Write(m.Payload)
		case "transfer.eof":
			sum := sha256.Sum256(got.Bytes())
			if !bytes.Equal(got.Bytes(), data) || m.Checksum != hex.EncodeToString(sum[:]) {
				t.Fatal("corrupt binary download")
			}
			if err := enc.Encode(protocol.Message{Type: "transfer.commit", ID: m.ID, OK: true}); err != nil {
				t.Fatal(err)
			}
		case "final":
			if !m.OK {
				t.Fatalf("download failed: %#v", m)
			}
			return
		}
	}
}
func TestRemoteBinaryOutputPreservesArbitraryBytes(t *testing.T) {
	f := newRemoteFixture(t)
	data := bytes.Repeat([]byte{0, 255, 128, 10}, 60000)
	f.s.pool = &fakeExecutor{stdout: data}
	conn, enc, dec := remoteConnect(t, f.agent)
	defer conn.Close()
	if err := enc.Encode(protocol.Message{Type: "exec", Host: "h", Command: "uptime", RequestID: "binary"}); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	for {
		var m protocol.Message
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		if m.Type == "stdout_chunk" {
			if m.Encoding != "" || len(m.Payload) > protocol.ChunkSize {
				t.Fatal("output was encoded as text or oversized")
			}
			got.Write(m.Payload)
		}
		if m.Type == "final" {
			if !m.OK || !bytes.Equal(data, got.Bytes()) {
				t.Fatal("binary command output corrupted")
			}
			return
		}
	}
}

func TestRemoteJobWatcherResumesWithoutExecutingAgain(t *testing.T) {
	f := newRemoteFixture(t)
	f.executor.started = make(chan struct{})
	f.executor.release = make(chan struct{})
	conn, enc, dec := remoteConnect(t, f.agent)
	defer conn.Close()
	if err := enc.Encode(protocol.Message{Type: "exec", Host: "h", Command: "uptime", RequestID: "resume"}); err != nil {
		t.Fatal(err)
	}
	var m protocol.Message
	if err := dec.Decode(&m); err != nil || m.Type != "command.created" {
		t.Fatalf("submit: %#v %v", m, err)
	}
	select {
	case <-f.executor.started:
	case <-time.After(time.Second):
		t.Fatal("job did not start")
	}
	f.listener.disconnect()
	close(f.executor.release)
	var output bytes.Buffer
	reconnected := false
	for {
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		if m.Type == "stream.connected" {
			reconnected = true
		}
		if m.Type == "stdout_chunk" {
			output.Write(m.Payload)
		}
		if m.Type == "final" {
			break
		}
	}
	if !reconnected || output.String() != "remote output\n" || f.executor.calls.Load() != 1 {
		t.Fatalf("resume: connected=%v output=%q calls=%d", reconnected, output.String(), f.executor.calls.Load())
	}
}
func TestConcurrentRemoteSubmissionsAcceptOnce(t *testing.T) {
	f := newRemoteFixture(t)
	var wg sync.WaitGroup
	errors := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := remote.Dial(f.agent)
			if err != nil {
				errors <- err
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			enc, dec := protocol.NewEncoder(conn), protocol.NewDecoder(conn)
			if err := enc.Encode(protocol.Message{Type: "exec", Host: "h", Command: "uptime", RequestID: "concurrent"}); err != nil {
				errors <- err
				return
			}
			var m protocol.Message
			if err := dec.Decode(&m); err != nil {
				errors <- err
				return
			}
			if m.Type != "command.created" && m.Type != "command" {
				errors <- fmt.Errorf("unexpected submission: %#v", m)
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	waitFor(t, func() bool { return f.executor.calls.Load() == 1 })
	if f.executor.calls.Load() != 1 {
		t.Fatal("duplicate remote execution")
	}
}

func TestRemoteRejectsOversizedBinaryChunk(t *testing.T) {
	f := newRemoteFixture(t)
	if err := f.s.setPolicyMode(config.ModeAuto); err != nil {
		t.Fatal(err)
	}
	conn, enc, dec := remoteConnect(t, f.agent)
	defer conn.Close()
	if err := enc.Encode(protocol.Message{Type: "file.transfer", Host: "h", Direction: "upload", LocalPath: "/client/file", RemotePath: "/remote/file", Size: -1}); err != nil {
		t.Fatal(err)
	}
	for {
		var m protocol.Message
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		if m.Type == "transfer.ready" {
			if err := enc.Encode(protocol.Chunk("transfer.chunk", m.ID, 1, make([]byte, protocol.ChunkSize+1))); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	for {
		var m protocol.Message
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		if m.Type == "final" {
			if m.OK || m.SSHUseErrorCode != "transfer_failed" {
				t.Fatalf("oversized chunk accepted: %#v", m)
			}
			return
		}
	}
}
