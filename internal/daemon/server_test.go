package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-ssh/internal/audit"
	"agent-ssh/internal/config"
	"agent-ssh/internal/model"
	"agent-ssh/internal/policy"
	"agent-ssh/internal/protocol"
	"agent-ssh/internal/sshpool"
)

func TestApprovalDecideAndCancel(t *testing.T) {
	s := newTestServer(t)
	state := &commandState{record: model.CommandRecord{ID: "cmd", Status: model.StatusPendingApproval}, approval: make(chan string, 1)}
	s.addCommand(state)
	if resp := s.approvalDecide("missing", "approve"); resp.OK {
		t.Fatal("expected missing command error")
	}
	if resp := s.approvalDecide("cmd", "bad"); resp.OK {
		t.Fatal("expected bad decision error")
	}
	if resp := s.approvalDecide("cmd", "approve"); !resp.OK {
		t.Fatalf("approve failed: %#v", resp)
	}
	if got := <-state.approval; got != "approve" {
		t.Fatalf("approval = %q", got)
	}
	if resp := s.cancelCommand("cmd", "cancel"); !resp.OK {
		t.Fatalf("cancel pending failed: %#v", resp)
	}
}

func TestPauseResumeAndEmergencyStop(t *testing.T) {
	s := newTestServer(t)
	s.setPaused(true)
	if !s.paused {
		t.Fatal("server not paused")
	}

	cancelled := &commandState{record: model.CommandRecord{ID: "pending", Status: model.StatusPendingApproval}, approval: make(chan string, 1)}
	running := &commandState{record: model.CommandRecord{ID: "running", Status: model.StatusRunning}, approval: make(chan string, 1)}
	s.addCommand(cancelled)
	s.addCommand(running)
	s.emergencyStop()
	select {
	case got := <-cancelled.approval:
		if got != "cancel" {
			t.Fatalf("approval = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("pending command not cancelled")
	}
	if running.record.Status != model.StatusRunning {
		t.Fatal("running command should not be directly cancelled by emergency stop")
	}

	resumeCh := s.resumeCh
	s.setPaused(false)
	select {
	case <-resumeCh:
	case <-time.After(time.Second):
		t.Fatal("resume channel not closed")
	}
}

func TestSnapshotAndPolicyRules(t *testing.T) {
	s := newTestServer(t)
	state := &commandState{record: model.CommandRecord{ID: "cmd", CreatedAt: time.Now(), Status: model.StatusDone, Host: "h", Command: "uptime", DisplayCommand: "uptime", PolicyRule: "readonly_inspection"}, approval: make(chan string, 1)}
	s.addCommand(state)
	snap := s.snapshot()
	if snap.Type != "snapshot" || !snap.OK || len(snap.Commands) != 1 {
		t.Fatalf("snapshot = %#v", snap)
	}
	if snap.RuntimeSettings == nil || snap.RuntimeSettings.Generation != 1 || snap.RuntimeSettings.Mode != config.ModeSensitive || len(snap.RuntimeSettings.ConfirmRisks) != 2 {
		t.Fatalf("runtime settings = %#v", snap.RuntimeSettings)
	}
	found := false
	for _, rule := range snap.PolicyRules {
		if rule.Name == "readonly_inspection" && rule.Matches == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("policy rules missing match count: %#v", snap.PolicyRules)
	}
}

func TestReloadConfigAddsHostAndUpdatesPolicy(t *testing.T) {
	s := newTestServer(t)
	fake := &fakeExecutor{}
	s.pool = fake
	falseValue := false
	cfg := config.Default()
	cfg.Hosts["new-server"] = config.Host{Addr: "10.20.30.40", User: "deploy", Port: 2222, Key: "/tmp/new-key"}
	cfg.Policy.BuiltinRules = &falseValue
	cfg.Policy.Rules = []config.RuleConfig{{Name: "block_deploy", Action: "block", Risk: "critical", Patterns: []string{`^deploy$`}}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.reloadConfig(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	s.handleExec(context.Background(), protocol.NewEncoder(&output), protocol.Message{Type: "exec", Host: "new-server", Command: "uptime"})
	if fake.executions != 1 || fake.lastExecHost.Addr != "10.20.30.40" || fake.lastExecHost.User != "deploy" || fake.lastExecHost.Port != 2222 {
		t.Fatalf("new host was not applied: executions=%d host=%#v", fake.executions, fake.lastExecHost)
	}

	output.Reset()
	s.handleExec(context.Background(), protocol.NewEncoder(&output), protocol.Message{Type: "exec", Host: "new-server", Command: "deploy"})
	if fake.executions != 1 || !bytes.Contains(output.Bytes(), []byte("policy_blocked")) {
		t.Fatalf("reloaded policy was not applied: executions=%d output=%s", fake.executions, output.String())
	}
}

func TestReloadConfigFailureKeepsPreviousGeneration(t *testing.T) {
	s := newTestServer(t)
	previousConfig, previousPolicy := s.currentConfig()
	path := os.Getenv("AGENT_SSH_CONFIG_PATH")
	if err := os.WriteFile(path, []byte("hosts: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.reloadConfig(); err == nil {
		t.Fatal("expected malformed config to fail")
	}
	currentConfig, currentPolicy := s.currentConfig()
	if currentConfig != previousConfig || currentPolicy != previousPolicy {
		t.Fatal("malformed config replaced the active generation")
	}

	falseValue := false
	cfg := config.Default()
	cfg.Policy.BuiltinRules = &falseValue
	cfg.Policy.Rules = []config.RuleConfig{{Name: "bad_regex", Action: "block", Risk: "critical", Patterns: []string{"("}}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.reloadConfig(); err == nil {
		t.Fatal("expected invalid policy regex to fail")
	}
	currentConfig, currentPolicy = s.currentConfig()
	if currentConfig != previousConfig || currentPolicy != previousPolicy {
		t.Fatal("invalid policy replaced the active generation")
	}
}

func TestReloadConfigRejectsBytesFromSupersededFile(t *testing.T) {
	s := newTestServer(t)
	previousConfig, previousPolicy := s.currentConfig()
	path := os.Getenv("AGENT_SSH_CONFIG_PATH")
	oldData := []byte("policy:\n  mode: auto\n")
	if err := os.WriteFile(path, oldData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("policy:\n  mode: approval\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.reloadConfigData(oldData); !errors.Is(err, errConfigChanged) {
		t.Fatalf("reload error = %v", err)
	}
	currentConfig, currentPolicy := s.currentConfig()
	if currentConfig != previousConfig || currentPolicy != previousPolicy {
		t.Fatal("superseded bytes replaced the active generation")
	}
}

func TestWatchConfigReloadsChangedFile(t *testing.T) {
	s := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.watchConfig(ctx, 5*time.Millisecond)

	cfg := config.Default()
	cfg.Hosts["watched"] = config.Host{Addr: "10.0.0.1"}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		current, _ := s.currentConfig()
		return current.Hosts["watched"].Addr == "10.0.0.1"
	})

	path := os.Getenv("AGENT_SSH_CONFIG_PATH")
	if err := os.WriteFile(path, []byte("policy:\n  mode: invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	current, _ := s.currentConfig()
	if current.Hosts["watched"].Addr != "10.0.0.1" {
		t.Fatal("invalid watched config replaced active config")
	}

	cfg.Hosts["watched"] = config.Host{Addr: "10.0.0.2"}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		current, _ := s.currentConfig()
		return current.Hosts["watched"].Addr == "10.0.0.2"
	})
}

func TestWatchConfigAllowsInitiallyMissingFile(t *testing.T) {
	s := newTestServer(t)
	events := make(chan protocol.Message, 1)
	s.mu.Lock()
	s.subscribers[events] = struct{}{}
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.watchConfig(ctx, 5*time.Millisecond)

	select {
	case event := <-events:
		t.Fatalf("missing optional config emitted event: %#v", event)
	case <-time.After(30 * time.Millisecond):
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{max: 5}
	b.Append([]byte("abc"))
	b.Append([]byte("def"))
	if b.String() != "bcdef" || !b.Truncated() {
		t.Fatalf("buffer = %q truncated=%v", b.String(), b.Truncated())
	}
	zero := &limitedBuffer{}
	zero.Append([]byte("abc"))
	if zero.String() != "" || zero.Truncated() {
		t.Fatalf("zero buffer = %q truncated=%v", zero.String(), zero.Truncated())
	}
}

func TestEventForStatus(t *testing.T) {
	tests := map[model.Status]string{
		model.StatusDone:         "command.done",
		model.StatusFailed:       "command.failed",
		model.StatusTimeout:      "command.timeout",
		model.StatusBlocked:      "command.blocked",
		model.StatusRejected:     "command.rejected",
		model.StatusCancelled:    "command.cancelled",
		model.StatusCancelFailed: "command.cancel_failed",
		model.StatusRunning:      "command.updated",
	}
	for status, want := range tests {
		if got := eventForStatus(status); got != want {
			t.Fatalf("eventForStatus(%s)=%q want=%q", status, got, want)
		}
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("AGENT_SSH_CONFIG_PATH", filepath.Join(t.TempDir(), "config.yaml"))
	cfg := config.Default()
	engine, err := policy.New(cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		cfg:              cfg,
		policy:           engine,
		configGeneration: 1,
		audit:            &audit.Store{},
		pool:             sshpool.New(),
		commands:         map[string]*commandState{},
		subscribers:      map[chan protocol.Message]struct{}{},
		hostQueues:       map[string]*hostQueue{},
		resumeCh:         make(chan struct{}),
	}
}

func TestAcquireHostCancellation(t *testing.T) {
	s := newTestServer(t)
	q := s.hostQueue("h")
	q.sem <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state := &commandState{record: model.CommandRecord{ID: "cmd", Host: "h"}, approval: make(chan string, 1)}
	_, ok := s.acquireHost(ctx, func(protocol.Message) error { return nil }, state)
	if ok {
		t.Fatal("expected acquire to fail after cancellation")
	}
}

func TestFinalAgentSSHErrorAndUpdateOutput(t *testing.T) {
	s := newTestServer(t)
	state := &commandState{record: model.CommandRecord{ID: "cmd", StartedAt: time.Now().Add(-time.Second)}, approval: make(chan string, 1)}
	s.addCommand(state)
	var sent []protocol.Message
	s.finalAgentSSHError(func(msg protocol.Message) error {
		sent = append(sent, msg)
		return nil
	}, state, model.StatusRejected, "approval_rejected", "no")
	if len(sent) != 1 || sent[0].Type != "final" || sent[0].AgentSSHErrorCode != "approval_rejected" {
		t.Fatalf("sent=%#v", sent)
	}
	rec := cloneRecord(state)
	if rec.Status != model.StatusRejected || rec.ApprovalStatus != "rejected" || rec.DurationMS <= 0 {
		t.Fatalf("record=%#v", rec)
	}
	out := &limitedBuffer{max: 100}
	out.Append([]byte("stdout"))
	errb := &limitedBuffer{max: 100}
	errb.Append([]byte("stderr"))
	s.updateOutput(state, out, errb)
	rec = cloneRecord(state)
	if rec.Stdout != "stdout" || rec.Stderr != "stderr" {
		t.Fatalf("output=%#v", rec)
	}
}

func TestWaitIfPaused(t *testing.T) {
	s := newTestServer(t)
	state := &commandState{record: model.CommandRecord{ID: "cmd"}, approval: make(chan string, 1)}
	if !s.waitIfPaused(context.Background(), state) {
		t.Fatal("unexpected paused wait failure")
	}
	s.setPaused(true)
	done := make(chan bool, 1)
	go func() { done <- s.waitIfPaused(context.Background(), state) }()
	time.Sleep(10 * time.Millisecond)
	s.setPaused(false)
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("wait returned false")
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not resume")
	}
}

func TestWaitApprovalApproveRejectTimeout(t *testing.T) {
	s := newTestServer(t)
	state := &commandState{record: model.CommandRecord{ID: "cmd"}, approval: make(chan string, 1)}
	go func() { state.approval <- "approve" }()
	if !s.waitApproval(context.Background(), func(protocol.Message) error { return nil }, state, model.PolicyDecision{}) {
		t.Fatal("approve should continue")
	}

	state = &commandState{record: model.CommandRecord{ID: "reject"}, approval: make(chan string, 1)}
	go func() { state.approval <- "reject" }()
	var final protocol.Message
	if s.waitApproval(context.Background(), func(msg protocol.Message) error { final = msg; return nil }, state, model.PolicyDecision{}) {
		t.Fatal("reject should stop")
	}
	if final.AgentSSHErrorCode != "approval_rejected" {
		t.Fatalf("final=%#v", final)
	}

	s.cfg.Approval.Timeout.Duration = time.Millisecond
	state = &commandState{record: model.CommandRecord{ID: "timeout"}, approval: make(chan string, 1)}
	if s.waitApproval(context.Background(), func(protocol.Message) error { return nil }, state, model.PolicyDecision{}) {
		t.Fatal("timeout should stop")
	}
	if cloneRecord(state).AgentSSHErrorCode != "approval_timeout" {
		t.Fatalf("record=%#v", cloneRecord(state))
	}
}

func TestHandleConnSimpleRequests(t *testing.T) {
	s := newTestServer(t)
	client, server := netPipe(t)
	go s.handleConn(context.Background(), server)
	enc := protocol.NewEncoder(client)
	dec := protocol.NewDecoder(client)
	if err := enc.Encode(protocol.Message{Type: "snapshot"}); err != nil {
		t.Fatal(err)
	}
	var resp protocol.Message
	if err := dec.Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "snapshot" || !resp.OK {
		t.Fatalf("resp=%#v", resp)
	}
	client.Close()

	client, server = netPipe(t)
	go s.handleConn(context.Background(), server)
	enc = protocol.NewEncoder(client)
	dec = protocol.NewDecoder(client)
	if err := enc.Encode(protocol.Message{Type: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "error" || resp.AgentSSHErrorCode != "protocol_error" {
		t.Fatalf("resp=%#v", resp)
	}
	client.Close()
}

func TestHandleConnControlRequests(t *testing.T) {
	s := newTestServer(t)
	s.pool = &fakeExecutor{}
	state := &commandState{record: model.CommandRecord{ID: "cmd", Status: model.StatusPendingApproval}, approval: make(chan string, 1)}
	s.addCommand(state)

	resp := roundTripHandleConn(t, s, protocol.Message{Type: "approval.decide", ID: "cmd", Decision: "approve"})
	if !resp.OK {
		t.Fatalf("approval resp=%#v", resp)
	}
	if got := <-state.approval; got != "approve" {
		t.Fatalf("approval=%q", got)
	}

	resp = roundTripHandleConn(t, s, protocol.Message{Type: "daemon.pause", Paused: true})
	if !resp.OK || !s.paused {
		t.Fatalf("pause resp=%#v paused=%v", resp, s.paused)
	}
	resp = roundTripHandleConn(t, s, protocol.Message{Type: "policy.set_mode", Mode: config.ModeApproval})
	if !resp.OK || s.policy.Mode() != config.ModeApproval {
		t.Fatalf("mode resp=%#v mode=%s", resp, s.policy.Mode())
	}
	resp = roundTripHandleConn(t, s, protocol.Message{Type: "policy.set_mode", Mode: "bad"})
	if resp.OK {
		t.Fatalf("bad mode resp=%#v", resp)
	}
	resp = roundTripHandleConn(t, s, protocol.Message{Type: "connections.close_idle"})
	if !resp.OK {
		t.Fatalf("close idle resp=%#v", resp)
	}
	resp = roundTripHandleConn(t, s, protocol.Message{Type: "connections.close_host", Host: "h"})
	if !resp.OK {
		t.Fatalf("close host resp=%#v", resp)
	}
	resp = roundTripHandleConn(t, s, protocol.Message{Type: "connections.close_host"})
	if resp.OK {
		t.Fatalf("empty close host resp=%#v", resp)
	}
}

func TestHandleSubscribeReceivesSnapshotAndBroadcast(t *testing.T) {
	s := newTestServer(t)
	client, server := netPipe(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleConn(context.Background(), server)
	}()
	enc := protocol.NewEncoder(client)
	dec := protocol.NewDecoder(client)
	if err := enc.Encode(protocol.Message{Type: "subscribe_events"}); err != nil {
		t.Fatal(err)
	}
	var snap protocol.Message
	if err := dec.Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap.Type != "snapshot" {
		t.Fatalf("snap=%#v", snap)
	}
	s.broadcast(protocol.Message{Type: "daemon.paused", Paused: true})
	var event protocol.Message
	if err := dec.Decode(&event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "daemon.paused" || !event.Paused {
		t.Fatalf("event=%#v", event)
	}
	client.Close()
	s.broadcast(protocol.Message{Type: "daemon.paused", Paused: false})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subscribe did not exit")
	}
}

func TestCancelCommandBranches(t *testing.T) {
	s := newTestServer(t)
	_, cancel := context.WithCancel(context.Background())
	queued := &commandState{record: model.CommandRecord{ID: "queued", Status: model.StatusQueued}, cancel: cancel, approval: make(chan string, 1)}
	done := &commandState{record: model.CommandRecord{ID: "done", Status: model.StatusDone}, approval: make(chan string, 1)}
	s.addCommand(queued)
	s.addCommand(done)
	if resp := s.cancelCommand("queued", "stop"); !resp.OK {
		t.Fatalf("queued cancel resp=%#v", resp)
	}
	if cloneRecord(queued).Status != model.StatusCancelled {
		t.Fatalf("queued record=%#v", cloneRecord(queued))
	}
	if resp := s.cancelCommand("done", "stop"); resp.OK {
		t.Fatalf("done cancel resp=%#v", resp)
	}
}

func TestAuditRetention(t *testing.T) {
	s := newTestServer(t)
	s.cfg.Audit.RetentionDays = 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.auditRetention(ctx)
	s.cfg.Audit.RetentionDays = 1
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	s.auditRetention(ctx)
}

func TestHandleExecBlockAndAllowWithFakePool(t *testing.T) {
	s := newTestServer(t)
	fake := &fakeExecutor{}
	s.pool = fake
	var buf bytes.Buffer
	enc := protocol.NewEncoder(&buf)
	s.handleExec(context.Background(), enc, protocol.Message{Type: "exec", Host: "h", Command: "rm -rf /"})
	if !bytes.Contains(buf.Bytes(), []byte("policy_blocked")) {
		t.Fatalf("blocked output=%s", buf.String())
	}

	buf.Reset()
	fake.stdout = []byte("hello")
	fake.stderr = []byte("warn")
	fake.exit = 0
	s.handleExec(context.Background(), enc, protocol.Message{Type: "exec", Host: "h", Command: "uptime"})
	out := buf.String()
	for _, want := range []string{"stdout_chunk", "stderr_chunk", `"ok":true`} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
}

func TestHandleExecSSHFailure(t *testing.T) {
	s := newTestServer(t)
	s.pool = &fakeExecutor{err: errors.New("boom"), noRemoteExit: true}
	var buf bytes.Buffer
	s.handleExec(context.Background(), protocol.NewEncoder(&buf), protocol.Message{Type: "exec", Host: "h", Command: "uptime"})
	if !bytes.Contains(buf.Bytes(), []byte("ssh_failed")) {
		t.Fatalf("output=%s", buf.String())
	}
}

func TestHandleTransferUploadAndDownload(t *testing.T) {
	s := newTestServer(t)
	s.policy.SetMode(config.ModeAuto)
	fake := &fakeExecutor{}
	s.pool = fake
	data := append([]byte{0, 1, 255}, bytes.Repeat([]byte("transfer"), 20000)...)

	client, server := netPipe(t)
	go s.handleConn(context.Background(), server)
	enc := protocol.NewEncoder(client)
	dec := protocol.NewDecoder(client)
	if err := enc.Encode(protocol.Message{
		Type:         "file.transfer",
		Host:         "h",
		Direction:    "upload",
		LocalPath:    "local.bin",
		RemotePath:   "/remote.bin",
		Size:         int64(len(data)),
		FileMode:     0o640,
		PreserveMode: true,
		Atomic:       true,
	}); err != nil {
		t.Fatal(err)
	}
	var ready protocol.Message
	for ready.Type != "transfer.ready" {
		if err := dec.Decode(&ready); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Encode(protocol.Chunk("transfer.chunk", ready.ID, 1, data)); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(protocol.Message{Type: "transfer.eof", ID: ready.ID, Bytes: int64(len(data)), Checksum: testChecksum(data)}); err != nil {
		t.Fatal(err)
	}
	var final protocol.Message
	if err := dec.Decode(&final); err != nil {
		t.Fatal(err)
	}
	if final.Type != "final" || !final.OK || final.Bytes != int64(len(data)) || final.Checksum != testChecksum(data) {
		t.Fatalf("upload final=%#v", final)
	}
	client.Close()

	fake.stdout = data
	client, server = netPipe(t)
	go s.handleConn(context.Background(), server)
	enc = protocol.NewEncoder(client)
	dec = protocol.NewDecoder(client)
	if err := enc.Encode(protocol.Message{Type: "file.transfer", Host: "h", Direction: "download", LocalPath: "local.bin", RemotePath: "/remote.bin", Size: -1}); err != nil {
		t.Fatal(err)
	}
	var downloaded []byte
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		switch msg.Type {
		case "transfer.chunk":
			chunk, err := protocol.DecodeData(msg)
			if err != nil {
				t.Fatal(err)
			}
			downloaded = append(downloaded, chunk...)
		case "transfer.eof":
			if msg.Bytes != int64(len(data)) || msg.Checksum != testChecksum(data) {
				t.Fatalf("download eof=%#v", msg)
			}
			if err := enc.Encode(protocol.Message{Type: "transfer.commit", ID: msg.ID, OK: true}); err != nil {
				t.Fatal(err)
			}
		case "final":
			if !msg.OK || !bytes.Equal(downloaded, data) {
				t.Fatalf("download final=%#v bytes=%d", msg, len(downloaded))
			}
			client.Close()
			return
		}
	}
}

func TestHandleTransferRejectsFailedLocalCommit(t *testing.T) {
	s := newTestServer(t)
	s.policy.SetMode(config.ModeAuto)
	s.pool = &fakeExecutor{stdout: []byte("data")}
	client, server := netPipe(t)
	go s.handleConn(context.Background(), server)
	enc := protocol.NewEncoder(client)
	dec := protocol.NewDecoder(client)
	if err := enc.Encode(protocol.Message{Type: "file.transfer", Host: "h", Direction: "download", LocalPath: "local", RemotePath: "/remote", Size: -1}); err != nil {
		t.Fatal(err)
	}
	for {
		var msg protocol.Message
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "transfer.eof" {
			if err := enc.Encode(protocol.Message{Type: "transfer.commit", ID: msg.ID, OK: false, Error: "disk full"}); err != nil {
				t.Fatal(err)
			}
		}
		if msg.Type == "final" {
			if msg.OK || msg.AgentSSHErrorCode != "transfer_failed" || !bytes.Contains([]byte(msg.Error), []byte("disk full")) {
				t.Fatalf("final=%#v", msg)
			}
			client.Close()
			return
		}
	}
}

func testChecksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type fakeExecutor struct {
	stdout       []byte
	stderr       []byte
	exit         int
	err          error
	noRemoteExit bool
	executions   int
	lastExecHost config.Host
}

func (f *fakeExecutor) Execute(ctx context.Context, opts sshpool.ExecOptions) (sshpool.ExecResult, error) {
	f.executions++
	f.lastExecHost = opts.ResolvedHost
	if len(f.stdout) > 0 {
		opts.Stdout(f.stdout)
	}
	if len(f.stderr) > 0 {
		opts.Stderr(f.stderr)
	}
	code := f.exit
	if f.noRemoteExit {
		return sshpool.ExecResult{Connection: model.ConnectionStatus{Host: opts.HostName}}, f.err
	}
	return sshpool.ExecResult{RemoteExitCode: &code, Connection: model.ConnectionStatus{Host: opts.HostName}}, f.err
}

func (f *fakeExecutor) Upload(ctx context.Context, opts sshpool.UploadOptions) (sshpool.TransferResult, error) {
	data, err := io.ReadAll(opts.Reader)
	if err != nil {
		return sshpool.TransferResult{}, err
	}
	sum := sha256.Sum256(data)
	result := sshpool.TransferResult{
		Bytes:      int64(len(data)),
		Checksum:   hex.EncodeToString(sum[:]),
		Connection: model.ConnectionStatus{Host: opts.HostName},
	}
	if opts.Validate != nil {
		if err := opts.Validate(result); err != nil {
			return result, err
		}
	}
	return result, f.err
}

func (f *fakeExecutor) Download(ctx context.Context, opts sshpool.DownloadOptions) (sshpool.TransferResult, error) {
	if opts.Ready != nil {
		if err := opts.Ready(sshpool.RemoteFileInfo{Size: int64(len(f.stdout)), Mode: 0o644}); err != nil {
			return sshpool.TransferResult{}, err
		}
	}
	if len(f.stdout) > 0 {
		if _, err := opts.Writer.Write(f.stdout); err != nil {
			return sshpool.TransferResult{}, err
		}
	}
	sum := sha256.Sum256(f.stdout)
	return sshpool.TransferResult{
		Bytes:      int64(len(f.stdout)),
		Size:       int64(len(f.stdout)),
		Checksum:   hex.EncodeToString(sum[:]),
		FileMode:   0o644,
		Connection: model.ConnectionStatus{Host: opts.HostName},
	}, f.err
}

func (f *fakeExecutor) Statuses() []model.ConnectionStatus {
	return []model.ConnectionStatus{{Host: "h"}}
}
func (f *fakeExecutor) CloseIdle(time.Duration)        {}
func (f *fakeExecutor) CloseAllIdle()                  {}
func (f *fakeExecutor) CloseHostIdle(host string) bool { return host == "h" }
func (f *fakeExecutor) ApplyConfig(*config.Config)     {}

func netPipe(t *testing.T) (client, server net.Conn) {
	t.Helper()
	return net.Pipe()
}

func roundTripHandleConn(t *testing.T, s *Server, req protocol.Message) protocol.Message {
	t.Helper()
	client, server := netPipe(t)
	defer client.Close()
	go s.handleConn(context.Background(), server)
	enc := protocol.NewEncoder(client)
	dec := protocol.NewDecoder(client)
	if err := enc.Encode(req); err != nil {
		t.Fatal(err)
	}
	var resp protocol.Message
	if err := dec.Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}
