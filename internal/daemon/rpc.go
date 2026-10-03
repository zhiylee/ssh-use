package daemon

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/zhiylee/ssh-use/internal/model"
	"github.com/zhiylee/ssh-use/internal/protocol"
	"github.com/zhiylee/ssh-use/internal/remote"
	"github.com/zhiylee/ssh-use/internal/rpcpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type identity struct{ name, role string }
type identityKey struct{}
type rpcGateway struct {
	rpcpb.UnimplementedGatewayServer
	s         *Server
	ctx       context.Context
	mu        sync.Mutex // serializes submission, journal eviction, and shutdown
	jobs      map[string]*messageJournal
	jobOrder  []string
	wg        sync.WaitGroup
	stopped   bool
	taskSlots chan struct{}
}

func serveRPC(parent context.Context, listener net.Listener, s *Server, authFile string, cert tls.Certificate) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	gateway := &rpcGateway{s: s, ctx: ctx, jobs: map[string]*messageJournal{}, taskSlots: make(chan struct{}, 128)}
	s.mu.Lock()
	s.events = newJournal()
	s.events.next = s.revision.Load()
	s.mu.Unlock()
	slots := make(chan struct{}, 128)
	authenticate := func(ctx context.Context) (context.Context, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		versions := md.Get("ssh-use-version")
		if len(versions) != 1 || versions[0] != fmt.Sprint(remote.Version) {
			return nil, status.Error(codes.FailedPrecondition, "unsupported ssh-use protocol version")
		}
		values := md.Get("authorization")
		if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
			return nil, status.Error(codes.Unauthenticated, "authentication required")
		}
		token := strings.TrimPrefix(values[0], "Bearer ")
		if len(token) < 32 || len(token) > 512 {
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		auth, err := remote.LoadAuth(authFile)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "authentication failed")
		}
		name, role := auth.Authenticate(token)
		if name == "" {
			return nil, status.Error(codes.Unauthenticated, "authentication failed")
		}
		return context.WithValue(ctx, identityKey{}, identity{name, role}), nil
	}
	acquire := func() error {
		select {
		case slots <- struct{}{}:
			return nil
		default:
			return status.Error(codes.ResourceExhausted, "too many active RPCs")
		}
	}
	server := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})),
		grpc.MaxRecvMsgSize(protocol.MaxRequestSize), grpc.MaxSendMsgSize(protocol.MaxMessageSize), grpc.MaxConcurrentStreams(128),
		grpc.ConnectionTimeout(10*time.Second),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: time.Minute}),
		grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			authenticated, err := authenticate(ctx)
			if err != nil {
				return nil, err
			}
			if err = acquire(); err != nil {
				return nil, err
			}
			defer func() { <-slots }()
			return handler(authenticated, req)
		}),
		grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			authenticated, err := authenticate(stream.Context())
			if err != nil {
				return err
			}
			if err = acquire(); err != nil {
				return err
			}
			defer func() { <-slots }()
			return handler(srv, &identityStream{ServerStream: stream, ctx: authenticated})
		}),
	)
	rpcpb.RegisterGatewayServer(server, gateway)
	var background sync.WaitGroup
	background.Add(3)
	go func() { defer background.Done(); s.idleReaper(ctx) }()
	go func() { defer background.Done(); s.auditRetention(ctx) }()
	go func() { defer background.Done(); s.watchConfig(ctx, 250*time.Millisecond) }()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		gateway.mu.Lock()
		gateway.stopped = true
		gateway.mu.Unlock()
		server.Stop()
	}()
	err := server.Serve(listener)
	cancel()
	<-stopped
	gateway.wg.Wait()
	background.Wait()
	s.pool.CloseAllIdle()
	if parent.Err() != nil || err == grpc.ErrServerStopped {
		return nil
	}
	return err
}

type identityStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *identityStream) Context() context.Context { return s.ctx }

func (g *rpcGateway) prepare(ctx context.Context, msg *protocol.Message) *protocol.Message {
	who, _ := ctx.Value(identityKey{}).(identity)
	if !g.s.authorized(who.role, who.name, *msg) {
		return &protocol.Message{Type: "error", Error: "permission denied", SSHUseErrorCode: "forbidden"}
	}
	msg.Source = "device:" + who.name
	if msg.Type == "exec" || msg.Type == "file.transfer" {
		cfg, _ := g.s.currentConfig()
		if _, ok := cfg.Hosts[msg.Host]; !ok {
			return &protocol.Message{Type: "error", Error: "remote execution requires a registered host", SSHUseErrorCode: "host_not_found"}
		}
	}
	return nil
}
func (g *rpcGateway) Request(ctx context.Context, p *rpcpb.Message) (*rpcpb.Message, error) {
	msg := protocol.FromWireMessage(p)
	if reply := g.prepare(ctx, msg); reply != nil {
		return protocol.ToWireMessage(reply), nil
	}
	switch msg.Type {
	case "exec", "file.transfer", "subscribe_events":
		return nil, status.Error(codes.InvalidArgument, "use the job or streaming RPC for this operation")
	}
	reply := g.s.request(ctx, *msg)
	if reply.Type == "snapshot" {
		previewSnapshot(&reply)
	}
	return protocol.ToWireMessage(&reply), nil
}
func (g *rpcGateway) SubmitJob(ctx context.Context, p *rpcpb.Message) (*rpcpb.Message, error) {
	msg := protocol.FromWireMessage(p)
	msg.Type = "exec"
	if reply := g.prepare(ctx, msg); reply != nil {
		return protocol.ToWireMessage(reply), nil
	}
	if msg.Host == "" || msg.Command == "" {
		return nil, status.Error(codes.InvalidArgument, "host and command are required")
	}
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "server stopping")
	}
	select {
	case g.taskSlots <- struct{}{}:
	default:
		g.mu.Unlock()
		return nil, status.Error(codes.ResourceExhausted, "too many accepted jobs")
	}
	if fresh, reply := g.s.reserveExec(g.ctx, msg); !fresh {
		<-g.taskSlots
		existing := g.jobs[msg.ID]
		g.mu.Unlock()
		if reply.SSHUseErrorCode == "request_already_accepted" && existing != nil {
			for {
				messages, _, done, changed := existing.read(0)
				if len(messages) > 0 || done {
					reply = g.s.commandInfo(ctx, msg.ID)
					break
				}
				select {
				case <-changed:
				case <-ctx.Done():
					return nil, status.FromContextError(ctx.Err()).Err()
				}
			}
		}
		return protocol.ToWireMessage(&reply), nil
	}
	journal := newJournal()
	g.jobs[msg.ID] = journal
	g.jobOrder = append(g.jobOrder, msg.ID)
	g.pruneJournals()
	ready := make(chan protocol.Message, 1)
	var once sync.Once
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer func() { <-g.taskSlots }()
		defer func() { journal.finish(); g.mu.Lock(); g.pruneJournals(); g.mu.Unlock() }()
		enc := protocol.NewMessageEncoder(func(m protocol.Message) error { journal.publish(m); once.Do(func() { ready <- m }); return nil })
		g.s.handleExec(g.ctx, enc, *msg)
	}()
	g.mu.Unlock()
	select {
	case reply := <-ready:
		reply.OK = reply.Type == "command.created"
		return protocol.ToWireMessage(&reply), nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}
func (g *rpcGateway) WatchJob(req *rpcpb.WatchRequest, stream grpc.ServerStreamingServer[rpcpb.Message]) error {
	msg := protocol.Message{Type: "command.watch", ID: req.Id}
	if reply := g.prepare(stream.Context(), &msg); reply != nil {
		return status.Error(codes.PermissionDenied, reply.Error)
	}
	g.mu.Lock()
	journal := g.jobs[req.Id]
	g.mu.Unlock()
	if journal == nil {
		return status.Error(codes.OutOfRange, "job output no longer retained; query the task record")
	}
	writer := newRPCWriter(stream.Context(), stream.Send)
	defer writer.close()
	cursor := req.Cursor
	for {
		messages, gap, done, changed := journal.read(cursor)
		if gap {
			return status.Error(codes.OutOfRange, "job output cursor expired; query the task record")
		}
		for _, m := range messages {
			if err := writer.write(protocol.ToWireMessage(&m)); err != nil {
				return err
			}
			cursor = m.Cursor
		}
		if done {
			return nil
		}
		select {
		case <-changed:
		case <-stream.Context().Done():
			return status.FromContextError(stream.Context().Err()).Err()
		}
	}
}

// The TUI receives bounded output previews; full live bytes belong to WatchJob.
func previewRecord(r *model.CommandRecord) {
	if len(r.Stdout) > 4096 {
		r.Stdout = strings.Clone(r.Stdout[len(r.Stdout)-4096:])
		r.StdoutTruncated = true
	}
	if len(r.Stderr) > 4096 {
		r.Stderr = strings.Clone(r.Stderr[len(r.Stderr)-4096:])
		r.StderrTruncated = true
	}
}
func previewSnapshot(m *protocol.Message) {
	for i := range m.Commands {
		previewRecord(&m.Commands[i])
	}
}

func (g *rpcGateway) WatchEvents(req *rpcpb.WatchRequest, stream grpc.ServerStreamingServer[rpcpb.Message]) error {
	msg := protocol.Message{Type: "subscribe_events"}
	if reply := g.prepare(stream.Context(), &msg); reply != nil {
		return status.Error(codes.PermissionDenied, reply.Error)
	}
	// Existing approval status checks count RPC subscribers too.
	subscriber := make(chan protocol.Message)
	g.s.mu.Lock()
	g.s.subscribers[subscriber] = struct{}{}
	g.s.mu.Unlock()
	defer func() { g.s.mu.Lock(); delete(g.s.subscribers, subscriber); g.s.mu.Unlock() }()
	writer := newRPCWriter(stream.Context(), stream.Send)
	defer writer.close()
	cursor := req.Cursor
	announced := false
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	refresh := cursor == 0
	for {
		messages, gap, _, changed := g.s.events.read(cursor)
		if refresh || gap {
			snapshot := g.s.snapshot()
			previewSnapshot(&snapshot)
			snapshot.Cursor = snapshot.Revision
			if err := writer.write(protocol.ToWireMessage(&snapshot)); err != nil {
				return err
			}
			cursor = snapshot.Cursor
			announced = true
			refresh = false
			continue
		}
		if !announced && len(messages) == 0 {
			heartbeat := protocol.Message{Type: "heartbeat", Cursor: cursor}
			if err := writer.write(protocol.ToWireMessage(&heartbeat)); err != nil {
				return err
			}
		}
		announced = true
		for _, m := range messages {
			if err := writer.write(protocol.ToWireMessage(&m)); err != nil {
				return err
			}
			cursor = m.Cursor
		}
		select {
		case <-changed:
		case <-ticker.C:
			heartbeat := protocol.Message{Type: "heartbeat", Cursor: cursor}
			if err := writer.write(protocol.ToWireMessage(&heartbeat)); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return status.FromContextError(stream.Context().Err()).Err()
		}
	}
}

// A transfer timeout terminates its RPC; it does not close other multiplexed RPCs.
type transferConnection struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	timer  *time.Timer
}

func (c *transferConnection) Close() error { c.cancel(); return nil }
func (c *transferConnection) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
	}
	if !t.IsZero() {
		c.timer = time.AfterFunc(time.Until(t), c.cancel)
	}
	return nil
}
func (g *rpcGateway) Transfer(stream grpc.BidiStreamingServer[rpcpb.Message, rpcpb.Message]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	msg := protocol.FromWireMessage(first)
	if msg.Type != "file.transfer" {
		return status.Error(codes.InvalidArgument, "first message must be file.transfer")
	}
	if reply := g.prepare(stream.Context(), msg); reply != nil {
		return stream.Send(protocol.ToWireMessage(reply))
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	conn := &transferConnection{cancel: cancel}
	defer conn.SetDeadline(time.Time{})
	enc := protocol.NewMessageEncoder(func(m protocol.Message) error { return stream.Send(protocol.ToWireMessage(&m)) })
	dec := protocol.NewMessageDecoder(func() (protocol.Message, error) {
		p, err := stream.Recv()
		if err != nil {
			return protocol.Message{}, err
		}
		if len(p.Data) > protocol.ChunkSize {
			return protocol.Message{}, status.Error(codes.ResourceExhausted, "transfer chunk too large")
		}
		return *protocol.FromWireMessage(p), nil
	})
	finished := make(chan struct{})
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return status.Error(codes.Unavailable, "server stopping")
	}
	g.wg.Add(1)
	g.mu.Unlock()
	go func() { defer g.wg.Done(); defer close(finished); g.s.handleTransfer(ctx, conn, enc, dec, *msg) }()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

// Called with g.mu held. Active jobs retain their own bounded replay window.
func (g *rpcGateway) pruneJournals() {
	total := 0
	for _, j := range g.jobs {
		j.mu.Lock()
		total += j.bytes
		j.mu.Unlock()
	}
	kept := g.jobOrder[:0]
	for _, id := range g.jobOrder {
		j := g.jobs[id]
		j.mu.Lock()
		done, size := j.done, j.bytes
		j.mu.Unlock()
		if done && (len(g.jobs) > 200 || total > 32*1024*1024) {
			delete(g.jobs, id)
			total -= size
		} else {
			kept = append(kept, id)
		}
	}
	g.jobOrder = kept
}
