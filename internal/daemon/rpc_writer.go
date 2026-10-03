package daemon

import (
	"context"
	"github.com/zhiylee/ssh-use/internal/rpcpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"time"
)

// A stalled observer is detached after a bounded write wait. Returning from its
// RPC handler cancels the underlying gRPC Send; accepted jobs keep executing.
type rpcWriter struct {
	ctx    context.Context
	close  context.CancelFunc
	input  chan *rpcpb.Message
	result chan error
	err    error
}

func newRPCWriter(parent context.Context, send func(*rpcpb.Message) error) *rpcWriter {
	ctx, cancel := context.WithCancel(parent)
	w := &rpcWriter{ctx: ctx, close: cancel, input: make(chan *rpcpb.Message), result: make(chan error, 1)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-w.input:
				err := send(m)
				select {
				case w.result <- err:
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}
	}()
	return w
}
func (w *rpcWriter) write(m *rpcpb.Message) error {
	if w.err != nil {
		return w.err
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case w.input <- m:
	case <-w.ctx.Done():
		w.err = status.FromContextError(w.ctx.Err()).Err()
		return w.err
	case <-timer.C:
		w.err = status.Error(codes.DeadlineExceeded, "slow observer")
		return w.err
	}
	select {
	case w.err = <-w.result:
	case <-w.ctx.Done():
		w.err = status.FromContextError(w.ctx.Err()).Err()
	case <-timer.C:
		w.err = status.Error(codes.DeadlineExceeded, "slow observer")
	}
	return w.err
}
