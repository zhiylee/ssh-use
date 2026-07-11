package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"agent-ssh/internal/audit"
	"agent-ssh/internal/config"
	"agent-ssh/internal/model"
	"agent-ssh/internal/paths"
	"agent-ssh/internal/policy"
	"agent-ssh/internal/protocol"
	"agent-ssh/internal/redact"
	"agent-ssh/internal/sshpool"
)

type Server struct {
	cfg    *config.Config
	policy *policy.Engine
	audit  *audit.Store
	pool   commandExecutor

	mu          sync.Mutex
	commands    map[string]*commandState
	order       []string
	subscribers map[chan protocol.Message]struct{}
	hostQueues  map[string]*hostQueue
	paused      bool
	resumeCh    chan struct{}
	nextID      atomic.Uint64
}

type commandExecutor interface {
	Execute(context.Context, sshpool.ExecOptions) (sshpool.ExecResult, error)
	Upload(context.Context, sshpool.UploadOptions) (sshpool.TransferResult, error)
	Download(context.Context, sshpool.DownloadOptions) (sshpool.TransferResult, error)
	Statuses() []model.ConnectionStatus
	CloseIdle(time.Duration)
	CloseAllIdle()
	CloseHostIdle(string) bool
}

type commandState struct {
	mu       sync.Mutex
	record   model.CommandRecord
	cancel   context.CancelFunc
	approval chan string
}

type hostQueue struct {
	sem     chan struct{}
	mu      sync.Mutex
	waiters int
}

type limitedBuffer struct {
	mu        sync.Mutex
	data      []byte
	max       int
	truncated bool
}

func Run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	engine, err := policy.New(cfg.Policy)
	if err != nil {
		return err
	}
	store, err := audit.Open(cfg.AuditEnabled())
	if err != nil {
		return err
	}
	defer store.Close()

	server := &Server{
		cfg:         cfg,
		policy:      engine,
		audit:       store,
		pool:        sshpool.New(cfg),
		commands:    map[string]*commandState{},
		subscribers: map[chan protocol.Message]struct{}{},
		hostQueues:  map[string]*hostQueue{},
		resumeCh:    make(chan struct{}),
	}
	return server.listen(ctx)
}

func (s *Server) listen(ctx context.Context) error {
	if err := os.MkdirAll(paths.RuntimeDir(), 0o700); err != nil {
		return err
	}
	socketPath := paths.SocketPath()
	if conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		return fmt.Errorf("daemon already running at %s", socketPath)
	}
	_ = os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socketPath)
	_ = os.Chmod(socketPath, 0o600)

	go s.idleReaper(ctx)
	go s.auditRetention(ctx)

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	dec := protocol.NewDecoder(conn)
	enc := protocol.NewEncoder(conn)

	var msg protocol.Message
	if err := dec.Decode(&msg); err != nil {
		return
	}

	switch msg.Type {
	case "exec":
		s.handleExec(ctx, enc, msg)
	case "file.transfer":
		s.handleTransfer(ctx, conn, enc, dec, msg)
	case "snapshot":
		_ = enc.Encode(s.snapshot())
	case "subscribe_events":
		s.handleSubscribe(conn, enc)
	case "approval.decide":
		_ = enc.Encode(s.approvalDecide(msg.ID, msg.Decision))
	case "command.cancel":
		_ = enc.Encode(s.cancelCommand(msg.ID, "cancelled by user"))
	case "daemon.pause":
		s.setPaused(msg.Paused)
		_ = enc.Encode(protocol.Message{OK: true, Type: "ack", RequestID: msg.RequestID})
	case "daemon.emergency_stop":
		s.emergencyStop()
		_ = enc.Encode(protocol.Message{OK: true, Type: "ack", RequestID: msg.RequestID})
	case "policy.set_mode":
		if !config.ValidMode(msg.Mode) {
			_ = enc.Encode(protocol.Message{OK: false, Type: "ack", Error: "invalid mode"})
			return
		}
		s.policy.SetMode(msg.Mode)
		s.cfg.Policy.Mode = s.policy.Mode()
		if err := config.Save(s.cfg); err != nil {
			_ = enc.Encode(protocol.Message{OK: false, Type: "ack", Error: err.Error()})
			return
		}
		s.broadcast(protocol.Message{Type: "policy.mode_changed", Mode: s.policy.Mode()})
		_ = enc.Encode(protocol.Message{OK: true, Type: "ack", Mode: s.policy.Mode(), RequestID: msg.RequestID})
	case "connections.close_idle":
		s.pool.CloseAllIdle()
		s.broadcast(protocol.Message{Type: "connection.updated"})
		_ = enc.Encode(protocol.Message{OK: true, Type: "ack", RequestID: msg.RequestID})
	case "connections.close_host":
		if msg.Host == "" {
			_ = enc.Encode(protocol.Message{OK: false, Type: "ack", Error: "host is required"})
			return
		}
		if !s.pool.CloseHostIdle(msg.Host) {
			_ = enc.Encode(protocol.Message{OK: false, Type: "ack", Error: "connection is active or not found"})
			return
		}
		s.broadcast(protocol.Message{Type: "connection.updated", Host: msg.Host})
		_ = enc.Encode(protocol.Message{OK: true, Type: "ack", RequestID: msg.RequestID})
	default:
		_ = enc.Encode(protocol.Message{Type: "error", Error: "unknown request type", AgentSSHErrorCode: "protocol_error"})
	}
}

func (s *Server) handleTransfer(parent context.Context, conn net.Conn, enc *protocol.Encoder, dec *protocol.Decoder, req protocol.Message) {
	if req.Host == "" || req.LocalPath == "" || req.RemotePath == "" {
		_ = enc.Encode(protocol.Message{Type: "error", Error: "host, local path, and remote path are required", AgentSSHErrorCode: "protocol_error"})
		return
	}
	if req.Direction != "upload" && req.Direction != "download" {
		_ = enc.Encode(protocol.Message{Type: "error", Error: "transfer direction must be upload or download", AgentSSHErrorCode: "protocol_error"})
		return
	}
	if !path.IsAbs(req.RemotePath) {
		_ = enc.Encode(protocol.Message{Type: "error", Error: "remote path must be absolute", AgentSSHErrorCode: "protocol_error"})
		return
	}
	req.RemotePath = path.Clean(req.RemotePath)
	if req.Size < -1 {
		_ = enc.Encode(protocol.Message{Type: "error", Error: "invalid transfer size", AgentSSHErrorCode: "protocol_error"})
		return
	}

	hostCfg, err := s.cfg.ResolveHost(req.Host)
	if err != nil {
		_ = enc.Encode(protocol.Message{Type: "error", Error: err.Error(), AgentSSHErrorCode: "config_error"})
		return
	}

	id := s.newID()
	displayCommand := transferDisplayCommand(req)
	decision := s.policy.Evaluate(displayCommand)
	now := time.Now()
	rec := model.CommandRecord{
		ID:             id,
		CreatedAt:      now,
		Source:         req.Source,
		ClientPID:      req.ClientPID,
		ClientCWD:      req.CWD,
		Host:           req.Host,
		RemoteUser:     hostCfg.User,
		Command:        displayCommand,
		DisplayCommand: displayCommand,
		Mode:           s.policy.Mode(),
		Risk:           decision.Risk,
		PolicyAction:   decision.Action,
		PolicyRule:     decision.RuleName,
		MatchedPattern: decision.MatchedPattern,
		PolicyReason:   decision.Reason,
		DefaultAction:  decision.DefaultActionUsed,
		Status:         model.StatusCreated,
		PolicyDecision: &decision,
	}

	cmdCtx, cancel := context.WithCancel(parent)
	state := &commandState{record: rec, cancel: cancel, approval: make(chan string, 1)}
	s.addCommand(state)
	defer cancel()

	var writeMu sync.Mutex
	safeEncode := func(msg protocol.Message) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return enc.Encode(msg)
	}
	_ = safeEncode(protocol.Message{Type: "command.created", ID: id, Record: cloneRecordPtr(state)})
	s.broadcast(protocol.Message{Type: "command.created", ID: id, Record: cloneRecordPtr(state)})
	s.save(state)

	if !s.waitIfPaused(cmdCtx, state) {
		s.finalAgentSSHError(safeEncode, state, model.StatusCancelled, "cancelled", "transfer cancelled")
		return
	}
	if decision.Action == model.ActionBlock {
		s.finalAgentSSHError(safeEncode, state, model.StatusBlocked, "policy_blocked", "transfer blocked by policy")
		return
	}
	if decision.Action == model.ActionApprove {
		if !s.waitApproval(cmdCtx, safeEncode, state, decision) {
			return
		}
	}

	release, ok := s.acquireHost(cmdCtx, safeEncode, state)
	if !ok {
		s.finalAgentSSHError(safeEncode, state, model.StatusCancelled, "cancelled", "transfer cancelled")
		return
	}
	defer release()

	transferCtx, timeoutCancel := context.WithTimeout(cmdCtx, s.cfg.Defaults.CommandTimeout.Duration)
	defer timeoutCancel()
	state.mu.Lock()
	state.cancel = timeoutCancel
	state.record.Status = model.StatusRunning
	state.record.StartedAt = time.Now()
	state.mu.Unlock()
	s.saveAndBroadcast(state, "command.running")

	stopDeadline := make(chan struct{})
	go func() {
		select {
		case <-transferCtx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-stopDeadline:
		}
	}()

	var result sshpool.TransferResult
	if req.Direction == "upload" {
		if err := safeEncode(protocol.Message{Type: "transfer.ready", ID: id}); err != nil {
			s.finishTransfer(safeEncode, state, result, err)
			close(stopDeadline)
			return
		}
		reader := &transferChunkReader{dec: dec, id: id}
		result, err = s.pool.Upload(transferCtx, sshpool.UploadOptions{
			CommandID:      id,
			HostName:       req.Host,
			RemotePath:     req.RemotePath,
			DisplayCommand: displayCommand,
			Reader:         reader,
			Atomic:         req.Atomic,
			FileMode:       os.FileMode(req.FileMode),
			PreserveMode:   req.PreserveMode,
			Validate: func(result sshpool.TransferResult) error {
				return validateUpload(req, reader, result)
			},
		})
	} else {
		writer := &transferChunkWriter{encode: safeEncode, id: id}
		result, err = s.pool.Download(transferCtx, sshpool.DownloadOptions{
			HostName:       req.Host,
			RemotePath:     req.RemotePath,
			DisplayCommand: displayCommand,
			Writer:         writer,
			Ready: func(info sshpool.RemoteFileInfo) error {
				return safeEncode(protocol.Message{Type: "transfer.ready", ID: id, Size: info.Size, FileMode: uint32(info.Mode.Perm())})
			},
		})
		if err == nil {
			err = safeEncode(protocol.Message{Type: "transfer.eof", ID: id, OK: true, Bytes: result.Bytes, Checksum: result.Checksum})
		}
		if err == nil {
			var commit protocol.Message
			if decodeErr := dec.Decode(&commit); decodeErr != nil {
				err = fmt.Errorf("receive download confirmation: %w", decodeErr)
			} else if commit.Type != "transfer.commit" || commit.ID != id {
				err = fmt.Errorf("invalid download confirmation")
			} else if !commit.OK {
				err = fmt.Errorf("write local destination: %s", commit.Error)
			}
		}
	}

	close(stopDeadline)
	_ = conn.SetDeadline(time.Time{})
	if result.Connection.Host != "" {
		s.broadcast(protocol.Message{Type: "connection.updated", Host: req.Host})
	}
	s.finishTransfer(safeEncode, state, result, err)
}

func (s *Server) finishTransfer(encode func(protocol.Message) error, state *commandState, result sshpool.TransferResult, err error) {
	state.mu.Lock()
	state.record.FinishedAt = time.Now()
	state.record.DurationMS = state.record.FinishedAt.Sub(state.record.StartedAt).Milliseconds()
	if err == nil {
		state.record.Status = model.StatusDone
	} else if errors.Is(err, sshpool.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
		state.record.Status = model.StatusTimeout
		state.record.AgentSSHErrorCode = "command_timeout"
		state.record.Error = "transfer timeout"
	} else if errors.Is(err, sshpool.ErrCancelled) || errors.Is(err, context.Canceled) {
		state.record.Status = model.StatusCancelled
		state.record.AgentSSHErrorCode = "cancelled"
		state.record.Error = "transfer cancelled"
	} else {
		state.record.Status = model.StatusFailed
		state.record.AgentSSHErrorCode = "transfer_failed"
		state.record.Error = err.Error()
	}
	final := state.record
	state.mu.Unlock()

	s.saveAndBroadcast(state, eventForStatus(final.Status))
	msg := protocol.Message{Type: "final", ID: final.ID, OK: err == nil, Status: string(final.Status), AgentSSHErrorCode: final.AgentSSHErrorCode, Error: final.Error, Bytes: result.Bytes, Checksum: result.Checksum}
	_ = encode(msg)
}

func transferDisplayCommand(req protocol.Message) string {
	remote := req.Host + ":" + req.RemotePath
	if req.Direction == "upload" {
		if req.Atomic {
			return fmt.Sprintf("cp --atomic %q %q", req.LocalPath, remote)
		}
		return fmt.Sprintf("cp %q %q", req.LocalPath, remote)
	}
	if req.Atomic {
		return fmt.Sprintf("cp --atomic %q %q", remote, req.LocalPath)
	}
	return fmt.Sprintf("cp %q %q", remote, req.LocalPath)
}

type transferChunkReader struct {
	dec      *protocol.Decoder
	id       string
	buffer   []byte
	nextSeq  int64
	received int64
	eof      bool
	bytes    int64
	checksum string
}

func (r *transferChunkReader) Read(data []byte) (int, error) {
	if len(r.buffer) > 0 {
		n := copy(data, r.buffer)
		r.buffer = r.buffer[n:]
		return n, nil
	}
	if r.eof {
		return 0, io.EOF
	}

	for {
		var msg protocol.Message
		if err := r.dec.Decode(&msg); err != nil {
			return 0, err
		}
		if msg.ID != r.id {
			return 0, fmt.Errorf("transfer message has unexpected id %q", msg.ID)
		}
		switch msg.Type {
		case "transfer.chunk":
			if msg.Seq != r.nextSeq+1 {
				return 0, fmt.Errorf("transfer chunk sequence %d, expected %d", msg.Seq, r.nextSeq+1)
			}
			chunk, err := protocol.DecodeData(msg)
			if err != nil {
				return 0, fmt.Errorf("decode transfer chunk: %w", err)
			}
			r.nextSeq = msg.Seq
			r.received += int64(len(chunk))
			if len(chunk) == 0 {
				continue
			}
			n := copy(data, chunk)
			if n < len(chunk) {
				r.buffer = append(r.buffer[:0], chunk[n:]...)
			}
			return n, nil
		case "transfer.eof":
			if msg.Bytes != r.received {
				return 0, fmt.Errorf("upload byte count mismatch: received %d, sender reported %d", r.received, msg.Bytes)
			}
			r.eof = true
			r.bytes = msg.Bytes
			r.checksum = msg.Checksum
			return 0, io.EOF
		case "transfer.abort":
			if msg.Error == "" {
				msg.Error = "client aborted upload"
			}
			return 0, errors.New(msg.Error)
		default:
			return 0, fmt.Errorf("unexpected upload message type %q", msg.Type)
		}
	}
}

type transferChunkWriter struct {
	encode func(protocol.Message) error
	id     string
	seq    int64
}

func (w *transferChunkWriter) Write(data []byte) (int, error) {
	w.seq++
	if err := w.encode(protocol.Chunk("transfer.chunk", w.id, w.seq, data)); err != nil {
		return 0, err
	}
	return len(data), nil
}

func validateUpload(req protocol.Message, reader *transferChunkReader, result sshpool.TransferResult) error {
	if !reader.eof {
		return fmt.Errorf("upload ended before transfer.eof")
	}
	if req.Size >= 0 && result.Bytes != req.Size {
		return fmt.Errorf("upload size mismatch: source reported %d, remote received %d", req.Size, result.Bytes)
	}
	if reader.bytes != result.Bytes {
		return fmt.Errorf("upload byte count mismatch: sender reported %d, remote received %d", reader.bytes, result.Bytes)
	}
	if reader.checksum == "" || reader.checksum != result.Checksum {
		return fmt.Errorf("upload checksum mismatch")
	}
	return nil
}

func (s *Server) handleExec(parent context.Context, enc *protocol.Encoder, req protocol.Message) {
	if req.Host == "" || req.Command == "" {
		_ = enc.Encode(protocol.Message{Type: "error", Error: "host and command are required", AgentSSHErrorCode: "protocol_error"})
		return
	}

	hostCfg, err := s.cfg.ResolveHost(req.Host)
	if err != nil {
		_ = enc.Encode(protocol.Message{Type: "error", Error: err.Error(), AgentSSHErrorCode: "config_error"})
		return
	}

	id := s.newID()
	decision := s.policy.Evaluate(req.Command)
	now := time.Now()
	rec := model.CommandRecord{
		ID:             id,
		CreatedAt:      now,
		Source:         req.Source,
		ClientPID:      req.ClientPID,
		ClientCWD:      req.CWD,
		Host:           req.Host,
		RemoteUser:     hostCfg.User,
		Command:        req.Command,
		DisplayCommand: req.Command,
		Mode:           s.policy.Mode(),
		Risk:           decision.Risk,
		PolicyAction:   decision.Action,
		PolicyRule:     decision.RuleName,
		MatchedPattern: decision.MatchedPattern,
		PolicyReason:   decision.Reason,
		DefaultAction:  decision.DefaultActionUsed,
		Status:         model.StatusCreated,
		PolicyDecision: &decision,
	}

	cmdCtx, cancel := context.WithCancel(parent)
	state := &commandState{record: rec, cancel: cancel, approval: make(chan string, 1)}
	s.addCommand(state)
	defer cancel()

	var writeMu sync.Mutex
	safeEncode := func(msg protocol.Message) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return enc.Encode(msg)
	}
	_ = safeEncode(protocol.Message{Type: "command.created", ID: id, Record: cloneRecordPtr(state)})
	s.broadcast(protocol.Message{Type: "command.created", ID: id, Record: cloneRecordPtr(state)})
	s.save(state)

	if !s.waitIfPaused(cmdCtx, state) {
		s.finalAgentSSHError(safeEncode, state, model.StatusCancelled, "cancelled", "command cancelled")
		return
	}

	if decision.Action == model.ActionBlock {
		s.finalAgentSSHError(safeEncode, state, model.StatusBlocked, "policy_blocked", "command blocked by policy")
		return
	}

	if decision.Action == model.ActionApprove {
		if !s.waitApproval(cmdCtx, safeEncode, state, decision) {
			return
		}
	}

	release, ok := s.acquireHost(cmdCtx, safeEncode, state)
	if !ok {
		s.finalAgentSSHError(safeEncode, state, model.StatusCancelled, "cancelled", "command cancelled")
		return
	}
	defer release()

	commandCtx, timeoutCancel := context.WithTimeout(cmdCtx, s.cfg.Defaults.CommandTimeout.Duration)
	defer timeoutCancel()
	state.mu.Lock()
	state.cancel = timeoutCancel
	state.record.Status = model.StatusRunning
	state.record.StartedAt = time.Now()
	state.mu.Unlock()
	s.saveAndBroadcast(state, "command.running")

	stdoutBuf := &limitedBuffer{max: s.cfg.Audit.MaxOutputBytes}
	stderrBuf := &limitedBuffer{max: s.cfg.Audit.MaxOutputBytes}
	var stdoutSeq atomic.Int64
	var stderrSeq atomic.Int64

	emitStdout := func(data []byte) {
		stdoutBuf.Append(data)
		s.updateOutput(state, stdoutBuf, nil)
		_ = safeEncode(protocol.Chunk("stdout_chunk", id, stdoutSeq.Add(1), data))
		s.broadcast(protocol.Message{Type: "command.stdout", ID: id, Data: string(data), Record: cloneRecordPtr(state)})
	}
	emitStderr := func(data []byte) {
		stderrBuf.Append(data)
		s.updateOutput(state, nil, stderrBuf)
		_ = safeEncode(protocol.Chunk("stderr_chunk", id, stderrSeq.Add(1), data))
		s.broadcast(protocol.Message{Type: "command.stderr", ID: id, Data: string(data), Record: cloneRecordPtr(state)})
	}

	result, err := s.pool.Execute(commandCtx, sshpool.ExecOptions{
		CommandID: id,
		HostName:  req.Host,
		Command:   req.Command,
		Stdout:    emitStdout,
		Stderr:    emitStderr,
	})
	s.updateOutput(state, stdoutBuf, stderrBuf)
	if result.Connection.Host != "" {
		s.broadcast(protocol.Message{Type: "connection.updated", Host: req.Host})
	}

	state.mu.Lock()
	state.record.FinishedAt = time.Now()
	state.record.DurationMS = state.record.FinishedAt.Sub(state.record.StartedAt).Milliseconds()
	state.record.RemoteExitCode = result.RemoteExitCode
	state.record.StdoutTruncated = stdoutBuf.Truncated()
	state.record.StderrTruncated = stderrBuf.Truncated()
	if err == nil {
		state.record.Status = model.StatusDone
	} else if result.RemoteExitCode != nil {
		state.record.Status = model.StatusFailed
		state.record.Error = err.Error()
	} else if errors.Is(err, sshpool.ErrTimeout) {
		state.record.Status = model.StatusTimeout
		state.record.AgentSSHErrorCode = "command_timeout"
		state.record.Error = "command timeout"
	} else if errors.Is(err, sshpool.ErrCancelled) || errors.Is(commandCtx.Err(), context.Canceled) {
		state.record.Status = model.StatusCancelled
		state.record.AgentSSHErrorCode = "cancelled"
		state.record.Error = "command cancelled"
	} else {
		state.record.Status = model.StatusFailed
		state.record.AgentSSHErrorCode = "ssh_failed"
		state.record.Error = err.Error()
	}
	final := state.record
	state.mu.Unlock()

	s.saveAndBroadcast(state, eventForStatus(final.Status))
	if final.AgentSSHErrorCode != "" {
		_ = safeEncode(protocol.Message{Type: "final", ID: id, OK: false, Status: string(final.Status), AgentSSHErrorCode: final.AgentSSHErrorCode, Error: final.Error, RemoteExitCode: final.RemoteExitCode})
		return
	}
	_ = safeEncode(protocol.Message{Type: "final", ID: id, OK: true, Status: string(final.Status), RemoteExitCode: final.RemoteExitCode})
}

func (s *Server) waitIfPaused(ctx context.Context, state *commandState) bool {
	s.mu.Lock()
	paused := s.paused
	ch := s.resumeCh
	s.mu.Unlock()
	if !paused {
		return true
	}
	state.mu.Lock()
	state.record.Status = model.StatusPaused
	state.mu.Unlock()
	s.saveAndBroadcast(state, "command.paused")
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Server) waitApproval(ctx context.Context, encode func(protocol.Message) error, state *commandState, decision model.PolicyDecision) bool {
	state.mu.Lock()
	state.record.Status = model.StatusPendingApproval
	state.record.ApprovalStatus = "pending"
	state.mu.Unlock()
	s.saveAndBroadcast(state, "command.pending")
	_ = encode(protocol.Message{Type: "approval.waiting", ID: state.record.ID, PolicyDecision: &decision})

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	timeout := time.NewTimer(s.cfg.Approval.Timeout.Duration)
	defer timeout.Stop()
	started := time.Now()

	for {
		select {
		case decision := <-state.approval:
			switch decision {
			case "approve":
				state.mu.Lock()
				state.record.Status = model.StatusApproved
				state.record.ApprovalStatus = "approved"
				state.record.ApprovedAt = time.Now()
				state.mu.Unlock()
				s.saveAndBroadcast(state, "command.approved")
				return true
			case "reject":
				s.finalAgentSSHError(encode, state, model.StatusRejected, "approval_rejected", "command rejected by user")
				return false
			case "cancel":
				s.finalAgentSSHError(encode, state, model.StatusCancelled, "cancelled", "command cancelled")
				return false
			}
		case <-ticker.C:
			s.mu.Lock()
			tuiConnected := len(s.subscribers) > 0
			s.mu.Unlock()
			_ = encode(protocol.Message{Type: "approval.heartbeat", ID: state.record.ID, Elapsed: time.Since(started).Round(time.Second).String(), TUIConnected: tuiConnected})
		case <-timeout.C:
			s.finalAgentSSHError(encode, state, model.StatusTimeout, "approval_timeout", "approval timeout")
			return false
		case <-ctx.Done():
			s.finalAgentSSHError(encode, state, model.StatusCancelled, "cancelled", "command cancelled")
			return false
		}
	}
}

func (s *Server) acquireHost(ctx context.Context, encode func(protocol.Message) error, state *commandState) (func(), bool) {
	q := s.hostQueue(state.record.Host)
	select {
	case q.sem <- struct{}{}:
		return func() { <-q.sem }, true
	default:
	}

	q.mu.Lock()
	q.waiters++
	position := q.waiters + 1
	q.mu.Unlock()

	state.mu.Lock()
	state.record.Status = model.StatusQueued
	state.mu.Unlock()
	s.saveAndBroadcast(state, "command.queued")
	_ = encode(protocol.Message{Type: "command.queued", ID: state.record.ID, Host: state.record.Host, Position: position})

	select {
	case q.sem <- struct{}{}:
		q.mu.Lock()
		if q.waiters > 0 {
			q.waiters--
		}
		q.mu.Unlock()
		return func() { <-q.sem }, true
	case <-ctx.Done():
		q.mu.Lock()
		if q.waiters > 0 {
			q.waiters--
		}
		q.mu.Unlock()
		return nil, false
	}
}

func (s *Server) hostQueue(host string) *hostQueue {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.hostQueues[host]
	if q == nil {
		q = &hostQueue{sem: make(chan struct{}, 1)}
		s.hostQueues[host] = q
	}
	return q
}

func (s *Server) finalAgentSSHError(encode func(protocol.Message) error, state *commandState, status model.Status, code, message string) {
	state.mu.Lock()
	now := time.Now()
	state.record.Status = status
	state.record.FinishedAt = now
	if !state.record.StartedAt.IsZero() {
		state.record.DurationMS = now.Sub(state.record.StartedAt).Milliseconds()
	}
	state.record.AgentSSHErrorCode = code
	state.record.Error = message
	if status == model.StatusRejected {
		state.record.ApprovalStatus = "rejected"
	}
	final := state.record
	state.mu.Unlock()
	s.saveAndBroadcast(state, eventForStatus(status))
	_ = encode(protocol.Message{Type: "final", ID: final.ID, OK: false, Status: string(status), AgentSSHErrorCode: code, Error: message})
}

func (s *Server) approvalDecide(id, decision string) protocol.Message {
	state := s.getCommand(id)
	if state == nil {
		return protocol.Message{Type: "ack", OK: false, Error: "unknown command"}
	}
	if decision != "approve" && decision != "reject" {
		return protocol.Message{Type: "ack", OK: false, Error: "decision must be approve or reject"}
	}
	state.mu.Lock()
	status := state.record.Status
	state.mu.Unlock()
	if status != model.StatusPendingApproval {
		return protocol.Message{Type: "ack", OK: false, Error: "command is not pending approval"}
	}
	select {
	case state.approval <- decision:
	default:
	}
	return protocol.Message{Type: "ack", OK: true}
}

func (s *Server) cancelCommand(id, reason string) protocol.Message {
	state := s.getCommand(id)
	if state == nil {
		return protocol.Message{Type: "ack", OK: false, Error: "unknown command"}
	}
	state.mu.Lock()
	status := state.record.Status
	cancel := state.cancel
	state.mu.Unlock()

	switch status {
	case model.StatusPendingApproval:
		select {
		case state.approval <- "cancel":
		default:
		}
		if cancel != nil {
			cancel()
		}
		return protocol.Message{Type: "ack", OK: true}
	case model.StatusPaused, model.StatusQueued, model.StatusRunning, model.StatusCreated, model.StatusApproved:
		if cancel != nil {
			cancel()
		}
		if status != model.StatusRunning {
			state.mu.Lock()
			state.record.Status = model.StatusCancelled
			state.record.AgentSSHErrorCode = "cancelled"
			state.record.Error = reason
			state.record.FinishedAt = time.Now()
			state.mu.Unlock()
			s.saveAndBroadcast(state, "command.cancelled")
		}
		return protocol.Message{Type: "ack", OK: true}
	default:
		return protocol.Message{Type: "ack", OK: false, Error: "command cannot be cancelled"}
	}
}

func (s *Server) setPaused(paused bool) {
	s.mu.Lock()
	if s.paused == paused {
		s.mu.Unlock()
		return
	}
	if paused {
		s.paused = true
	} else {
		s.paused = false
		close(s.resumeCh)
		s.resumeCh = make(chan struct{})
	}
	s.mu.Unlock()
	s.broadcast(protocol.Message{Type: "daemon.paused", Paused: paused})
}

func (s *Server) emergencyStop() {
	s.setPaused(true)
	s.mu.Lock()
	ids := make([]string, 0, len(s.commands))
	for id, state := range s.commands {
		state.mu.Lock()
		status := state.record.Status
		state.mu.Unlock()
		if status == model.StatusPendingApproval || status == model.StatusQueued || status == model.StatusPaused {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.cancelCommand(id, "cancelled by emergency stop")
	}
}

func (s *Server) snapshot() protocol.Message {
	s.mu.Lock()
	paused := s.paused
	mode := s.policy.Mode()
	memory := make([]model.CommandRecord, 0, len(s.order))
	seen := map[string]struct{}{}
	for _, id := range s.order {
		if state := s.commands[id]; state != nil {
			rec := cloneRecord(state)
			memory = append(memory, rec)
			seen[rec.ID] = struct{}{}
		}
	}
	s.mu.Unlock()

	recent, _ := s.audit.Recent(context.Background(), 200)
	commands := make([]model.CommandRecord, 0, len(memory)+len(recent))
	commands = append(commands, memory...)
	for _, rec := range recent {
		if _, ok := seen[rec.ID]; !ok {
			commands = append(commands, rec)
		}
	}
	sort.Slice(commands, func(i, j int) bool {
		return commands[i].CreatedAt.After(commands[j].CreatedAt)
	})
	return protocol.Message{Type: "snapshot", OK: true, Paused: paused, Mode: mode, Commands: commands, Connections: s.pool.Statuses(), PolicyRules: s.policyRules(commands)}
}

func (s *Server) policyRules(commands []model.CommandRecord) []model.PolicyRuleView {
	counts := map[string]int{}
	for _, rec := range commands {
		if rec.PolicyRule != "" {
			counts[rec.PolicyRule]++
		}
	}
	rules := s.policy.Rules()
	views := make([]model.PolicyRuleView, 0, len(rules))
	for _, rule := range rules {
		patterns := make([]string, len(rule.Patterns))
		copy(patterns, rule.Patterns)
		views = append(views, model.PolicyRuleView{Name: rule.Name, Action: rule.Action, Risk: rule.Risk, Patterns: patterns, Matches: counts[rule.Name]})
	}
	return views
}

func (s *Server) handleSubscribe(conn net.Conn, enc *protocol.Encoder) {
	ch := make(chan protocol.Message, 100)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subscribers, ch)
		s.mu.Unlock()
		close(ch)
	}()

	_ = enc.Encode(s.snapshot())
	for msg := range ch {
		if err := enc.Encode(msg); err != nil {
			return
		}
	}
}

func (s *Server) broadcast(msg protocol.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (s *Server) addCommand(state *commandState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands[state.record.ID] = state
	s.order = append(s.order, state.record.ID)
}

func (s *Server) getCommand(id string) *commandState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commands[id]
}

func (s *Server) saveAndBroadcast(state *commandState, event string) {
	s.save(state)
	s.broadcast(protocol.Message{Type: event, ID: state.record.ID, Record: cloneRecordPtr(state)})
}

func (s *Server) save(state *commandState) {
	rec := cloneRecord(state)
	if s.cfg.RedactSecrets() {
		rec.Command = redact.Redact(rec.Command)
		rec.DisplayCommand = redact.Redact(rec.DisplayCommand)
		rec.Stdout = redact.Redact(rec.Stdout)
		rec.Stderr = redact.Redact(rec.Stderr)
	}
	rec.Stdout, rec.StdoutTruncated = audit.LimitOutput(rec.Stdout, s.cfg.Audit.MaxOutputBytes)
	rec.Stderr, rec.StderrTruncated = audit.LimitOutput(rec.Stderr, s.cfg.Audit.MaxOutputBytes)
	_ = s.audit.Save(context.Background(), rec)
}

func (s *Server) updateOutput(state *commandState, stdout, stderr *limitedBuffer) {
	state.mu.Lock()
	if stdout != nil {
		state.record.Stdout = stdout.String()
		state.record.StdoutTruncated = stdout.Truncated()
	}
	if stderr != nil {
		state.record.Stderr = stderr.String()
		state.record.StderrTruncated = stderr.Truncated()
	}
	state.mu.Unlock()
}

func (s *Server) idleReaper(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.pool.CloseIdle(s.cfg.Defaults.IdleTimeout.Duration)
			s.broadcast(protocol.Message{Type: "connection.updated"})
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) auditRetention(ctx context.Context) {
	if s.cfg.Audit.RetentionDays <= 0 {
		return
	}
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()
	for {
		cutoff := time.Now().Add(-time.Duration(s.cfg.Audit.RetentionDays) * 24 * time.Hour)
		_, _ = s.audit.DeleteOlderThan(ctx, cutoff)
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) newID() string {
	n := s.nextID.Add(1)
	return fmt.Sprintf("cmd_%d_%06d", time.Now().UnixMilli(), n)
}

func cloneRecord(state *commandState) model.CommandRecord {
	state.mu.Lock()
	defer state.mu.Unlock()
	rec := state.record
	if rec.PolicyDecision != nil {
		decision := *rec.PolicyDecision
		rec.PolicyDecision = &decision
	}
	return rec
}

func cloneRecordPtr(state *commandState) *model.CommandRecord {
	rec := cloneRecord(state)
	return &rec
}

func eventForStatus(status model.Status) string {
	switch status {
	case model.StatusDone:
		return "command.done"
	case model.StatusFailed:
		return "command.failed"
	case model.StatusTimeout:
		return "command.timeout"
	case model.StatusBlocked:
		return "command.blocked"
	case model.StatusRejected:
		return "command.rejected"
	case model.StatusCancelled:
		return "command.cancelled"
	case model.StatusCancelFailed:
		return "command.cancel_failed"
	default:
		return "command.updated"
	}
}

func (b *limitedBuffer) Append(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.max <= 0 {
		return
	}
	b.data = append(b.data, data...)
	if len(b.data) > b.max {
		trim := len(b.data) - b.max
		b.data = append([]byte(nil), b.data[trim:]...)
		b.truncated = true
	}
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

func (b *limitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
