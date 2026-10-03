package remote

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zhiylee/ssh-use/internal/protocol"
	"github.com/zhiylee/ssh-use/internal/rpcpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

// Channels live for the process lifetime and are shared by concurrent calls.
// A logical session only cancels its own RPC when closed.
var channels = struct {
	sync.Mutex
	clients map[string]*grpc.ClientConn
}{clients: map[string]*grpc.ClientConn{}}

type tokenCredentials struct{ file string }

func readToken(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 || len(token) > 512 {
		return "", fmt.Errorf("client token must contain 32-512 characters")
	}
	return token, nil
}
func (c tokenCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	token, err := readToken(c.file)
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + token, "ssh-use-version": fmt.Sprint(Version)}, nil
}
func (tokenCredentials) RequireTransportSecurity() bool { return true }
func rpcClient(cfg ClientConfig) (rpcpb.GatewayClient, error) {
	if _, err := readToken(cfg.TokenFile); err != nil {
		return nil, err
	}
	var pem []byte
	if cfg.CAFile != "" {
		var err error
		pem, err = os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}

	}
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%x", cfg.Endpoint, cfg.CAFile, cfg.TokenFile, sha256.Sum256(pem))
	channels.Lock()
	defer channels.Unlock()
	if conn := channels.clients[key]; conn != nil {
		return rpcpb.NewGatewayClient(conn), nil
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13}
	if cfg.CAFile != "" {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("invalid CA file")
		}
		tlsConfig.RootCAs = roots
	}
	conn, err := grpc.NewClient("passthrough:///"+cfg.Endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithPerRPCCredentials(tokenCredentials{cfg.TokenFile}),
		grpc.WithDisableRetry(), grpc.WithDisableServiceConfig(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(protocol.MaxMessageSize), grpc.MaxCallSendMsgSize(protocol.MaxRequestSize)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: time.Minute, Timeout: 10 * time.Second}),
	)
	if err != nil {
		return nil, err
	}
	channels.clients[key] = conn
	return rpcpb.NewGatewayClient(conn), nil
}
func CloseClients() {
	channels.Lock()
	defer channels.Unlock()
	for key, conn := range channels.clients {
		_ = conn.Close()
		delete(channels.clients, key)
	}
}
func Request(ctx context.Context, cfg ClientConfig, msg protocol.Message) (protocol.Message, error) {
	client, err := rpcClient(cfg)
	if err != nil {
		return protocol.Message{}, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	p, err := client.Request(ctx, protocol.ToWireMessage(&msg))
	if err != nil {
		return protocol.Message{}, rpcError(err)
	}
	reply := *protocol.FromWireMessage(p)
	if !reply.OK && reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

type Session struct {
	client    rpcpb.GatewayClient
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	timer     *time.Timer
	send      func(*rpcpb.Message) error
	receive   func() (*rpcpb.Message, error)
	pending   *protocol.Message
	kind      string
	watchID   string
	completed bool
	cursor    uint64
	reconnect bool
	backoff   time.Duration
}

func Dial(cfg ClientConfig) (protocol.Connection, error) {
	client, err := rpcClient(cfg)
	if err != nil {
		return nil, err
	}
	base, closeSession := context.WithCancel(context.Background())
	return &Session{client: client, ctx: base, cancel: closeSession, backoff: 250 * time.Millisecond}, nil
}
func (s *Session) Close() error {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()
	s.cancel()
	return nil
}
func (s *Session) SetDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
	}
	if !t.IsZero() {
		s.timer = time.AfterFunc(time.Until(t), s.cancel)
	}
	return nil
}
func (s *Session) Send(msg protocol.Message) error {
	if s.kind != "" {
		if s.send == nil {
			return fmt.Errorf("RPC does not accept additional messages")
		}
		return s.send(protocol.ToWireMessage(&msg))
	}
	s.kind = msg.Type
	switch msg.Type {
	case "exec":
		ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
		p, err := s.client.SubmitJob(ctx, protocol.ToWireMessage(&msg))
		cancel()
		if err != nil {
			return err
		}
		reply := protocol.FromWireMessage(p)
		if reply.Type != "command.created" || !reply.OK {
			s.pending = reply
			return nil
		}
		stream, err := s.client.WatchJob(s.ctx, &rpcpb.WatchRequest{Id: reply.ID})
		if err != nil {
			return err
		}
		s.receive = stream.Recv
		s.watchID = reply.ID
	case "command.watch":
		stream, err := s.client.WatchJob(s.ctx, &rpcpb.WatchRequest{Id: msg.ID, Cursor: msg.Cursor})
		if err != nil {
			return err
		}
		s.receive = stream.Recv
		s.watchID = msg.ID
		s.cursor = msg.Cursor
	case "subscribe_events":
		stream, err := s.client.WatchEvents(s.ctx, &rpcpb.WatchRequest{Cursor: msg.Cursor})
		if err != nil {
			return err
		}
		s.receive = stream.Recv
		s.cursor = msg.Cursor
	case "file.transfer":
		stream, err := s.client.Transfer(s.ctx)
		if err != nil {
			return err
		}
		s.send = stream.Send
		s.receive = stream.Recv
		return s.send(protocol.ToWireMessage(&msg))
	default:
		ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
		p, err := s.client.Request(ctx, protocol.ToWireMessage(&msg))
		cancel()
		if err != nil {
			return err
		}
		s.pending = protocol.FromWireMessage(p)
	}
	return nil
}
func (s *Session) Receive() (protocol.Message, error) {
	if s.pending != nil {
		m := *s.pending
		s.pending = nil
		s.track(m)
		return m, nil
	}
	if s.reconnect {
		for {
			timer := time.NewTimer(s.backoff)
			select {
			case <-timer.C:
			case <-s.ctx.Done():
				timer.Stop()
				return protocol.Message{}, s.ctx.Err()
			}
			receive, err := s.resumeStream()
			if err == nil {
				// Receive one message to establish that authentication and subscription succeeded.
				p, recvErr := receive()
				if recvErr == nil {
					s.receive = receive
					s.pending = protocol.FromWireMessage(p)
					s.reconnect = false
					s.backoff = 250 * time.Millisecond
					return protocol.Message{Type: "stream.connected"}, nil
				}
				err = recvErr
			}
			if !retriableStream(err) {
				return protocol.Message{}, err
			}
			s.backoff = min(5*time.Second, s.backoff*2)
		}
	}
	if s.receive == nil {
		return protocol.Message{}, io.EOF
	}
	p, err := s.receive()
	if err != nil {
		if s.watching() && !s.completed && s.ctx.Err() == nil && retriableStream(err) {
			s.reconnect = true
			return protocol.Message{Type: "stream.reconnecting", Error: err.Error()}, nil
		}
		return protocol.Message{}, err
	}
	m := *protocol.FromWireMessage(p)
	s.track(m)
	return m, nil
}
func retriableStream(err error) bool {
	// TLS trust/hostname failures require configuration changes, not reconnect loops.
	if strings.Contains(err.Error(), "authentication handshake failed") || strings.Contains(err.Error(), "x509:") {
		return false
	}
	return err == io.EOF || status.Code(err) == codes.Unavailable || status.Code(err) == codes.DeadlineExceeded
}

func (s *Session) watching() bool { return s.kind == "subscribe_events" || s.watchID != "" }
func (s *Session) track(m protocol.Message) {
	if s.watching() {
		s.cursor = m.Cursor
	}
	if m.Type == "final" {
		s.completed = true
	}
}
func (s *Session) resumeStream() (func() (*rpcpb.Message, error), error) {
	if s.watchID != "" {
		stream, err := s.client.WatchJob(s.ctx, &rpcpb.WatchRequest{Id: s.watchID, Cursor: s.cursor})
		if err != nil {
			return nil, err
		}
		return stream.Recv, nil
	}
	stream, err := s.client.WatchEvents(s.ctx, &rpcpb.WatchRequest{Cursor: s.cursor})
	if err != nil {
		return nil, err
	}
	return stream.Recv, nil
}

func rpcError(err error) error {
	switch status.Code(err) {
	case codes.DeadlineExceeded:
		return errors.Join(context.DeadlineExceeded, err)
	case codes.Canceled:
		return errors.Join(context.Canceled, err)
	}
	return err
}
