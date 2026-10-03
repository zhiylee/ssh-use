package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"

	"github.com/zhiylee/ssh-use/internal/protocol"
	"github.com/zhiylee/ssh-use/internal/remote"
)

type RemoteOptions struct{ Listen, CertFile, KeyFile, AuthFile string }

func RunRemote(ctx context.Context, opts RemoteOptions) error {
	if opts.CertFile == "" || opts.KeyFile == "" || opts.AuthFile == "" {
		return fmt.Errorf("--tls-cert, --tls-key and --auth-file are required")
	}
	cert, err := tls.LoadX509KeyPair(opts.CertFile, opts.KeyFile)
	if err != nil {
		return err
	}
	if _, err := remote.LoadAuth(opts.AuthFile); err != nil {
		return err
	}
	unlock, err := lockInstance()
	if err != nil {
		return err
	}
	defer unlock()
	s, err := newServer()
	if err != nil {
		return err
	}
	defer s.audit.Close()
	if !s.cfg.AuditEnabled() {
		return fmt.Errorf("server mode requires audit.enabled for durable task records")
	}
	s.remote = true
	if err := s.audit.MarkInterrupted(ctx); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	return serveRPC(ctx, listener, s, opts.AuthFile, cert)
}

func (s *Server) reserveExec(ctx context.Context, req *protocol.Message) (bool, protocol.Message) {
	if len(req.RequestID) < 1 || len(req.RequestID) > 128 {
		return false, protocol.Message{Type: "error", Error: "exec requires a request_id (1-128 characters)", SSHUseErrorCode: "protocol_error"}
	}
	id := fmt.Sprintf("cmd_%x", sha256.Sum256([]byte(req.Source+"\x00"+req.RequestID)))
	data, _ := json.Marshal([]string{req.Host, req.Command})
	fresh, err := s.audit.ClaimRequest(ctx, id, fmt.Sprintf("%x", sha256.Sum256(data)))
	if err != nil {
		return false, protocol.Message{Type: "error", Error: err.Error(), SSHUseErrorCode: "request_conflict"}
	}
	req.ID = id
	if !fresh {
		info := s.commandInfo(ctx, id)
		if !info.OK {
			return false, protocol.Message{Type: "error", ID: id, Error: "request already accepted; status not available, do not resubmit with a new ID", SSHUseErrorCode: "request_already_accepted"}
		}
		return false, info
	}
	return true, protocol.Message{}
}

func (s *Server) authorized(role, name string, req protocol.Message) bool {
	if role == "admin" {
		return true
	}
	switch req.Type {
	case "ping", "exec", "file.transfer", "hosts.list", "hosts.get":
		return true
	case "command.cancel", "command.get", "command.watch":
		if state := s.getCommand(req.ID); state != nil {
			return cloneRecord(state).Source == "device:"+name
		}
		if rec, err := s.audit.Get(context.Background(), req.ID); err == nil && rec != nil {
			return rec.Source == "device:"+name
		}
	}
	return false
}

func (s *Server) commandInfo(ctx context.Context, id string) protocol.Message {
	if state := s.getCommand(id); state != nil {
		return protocol.Message{Type: "command", OK: true, ID: id, Record: cloneRecordPtr(state)}
	}
	rec, err := s.audit.Get(ctx, id)
	if err != nil {
		return protocol.Message{Type: "error", Error: err.Error()}
	}
	if rec == nil {
		return protocol.Message{Type: "error", Error: "unknown command", SSHUseErrorCode: "command_not_found"}
	}
	return protocol.Message{Type: "command", OK: true, ID: id, Record: rec}
}
