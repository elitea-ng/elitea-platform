package grpcapi

// realtime.go — the Realtime RPC: the /llm/v1/realtime session over a
// bidirectional gRPC stream.
//
// The caller's WebSocket ends at elitea-main. The handshake request arrives as
// the first frame and runs the gateway's realtime handler unchanged: every
// admission step, and a refusal is an HTTP status and body exactly as before.
// At the accept the handler gets a RealtimeSocket backed by this stream instead
// of a hijacked connection (llmproxy.WithRealtimeAcceptor). The handshake and
// Origin checks are websocket.Accept's (llmproxy.CheckRealtimeHandshake).
//
// Liveness: the session's keepalive pings the caller through a RealtimePing
// frame; elitea-main pings the browser and answers with a RealtimePong. A
// browser that stopped answering therefore still ends the session.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gatewayv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/gateway/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
)

// Realtime serves one realtime session. It is not counted by Drain: a session
// is ended by the handler's CloseRealtimeSessions, in its own shutdown phase.
func (s *Service) Realtime(stream gatewayv1.GatewayService_RealtimeServer) error {
	if s.draining.Load() {
		return status.Error(codes.Unavailable, "the gateway is shutting down; retry against another replica")
	}
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "the handshake frame is required")
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "the first frame must be the handshake")
	}
	b := &realtimeBridge{stream: stream}
	w := newStreamWriter(func(f *gatewayv1.ResponseFrame) error {
		switch frame := f.GetFrame().(type) {
		case *gatewayv1.ResponseFrame_Head:
			return b.send(&gatewayv1.RealtimeResponse{Frame: &gatewayv1.RealtimeResponse_Head{Head: frame.Head}})
		case *gatewayv1.ResponseFrame_Data:
			return b.send(&gatewayv1.RealtimeResponse{Frame: &gatewayv1.RealtimeResponse_Data{Data: frame.Data}})
		}
		return nil
	})
	w.upgrade = true
	b.w = w
	ctx := llmproxy.WithRealtimeAcceptor(stream.Context(), b.accept)
	req, err := newRequest(ctx, routeRealtime, open, nil, 0)
	if err != nil {
		return err
	}
	if err := serveHTTP(s.llm, w, req); err != nil {
		return err
	}
	return w.finish()
}

type realtimeBridge struct {
	stream gatewayv1.GatewayService_RealtimeServer
	w      *streamWriter

	sendMu sync.Mutex
}

// send serialises Send: the session writes, pings and closes from different
// goroutines, and a gRPC stream allows one sender at a time.
func (b *realtimeBridge) send(frame *gatewayv1.RealtimeResponse) error {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	return b.stream.Send(frame)
}

// accept is the llmproxy.RealtimeAcceptor of this stream.
func (b *realtimeBridge) accept(w http.ResponseWriter, r *http.Request, opts llmproxy.RealtimeAcceptOptions) (llmproxy.RealtimeSocket, string, error) {
	subprotocol, err := llmproxy.CheckRealtimeHandshake(w, r, opts)
	if err != nil {
		return nil, "", err
	}
	// Through the request log's recorder, so the session is logged as 101.
	w.WriteHeader(http.StatusSwitchingProtocols)
	b.w.mu.Lock()
	upgraded, werr := b.w.upgraded, b.w.err
	b.w.mu.Unlock()
	if !upgraded || werr != nil {
		if werr == nil {
			werr = errors.New("grpcapi: the session head was not sent")
		}
		return nil, "", werr
	}
	sock := &realtimeSocket{
		b:         b,
		readLimit: opts.ReadLimit,
		messages:  make(chan []byte),
		done:      make(chan struct{}),
		pings:     map[uint64]chan struct{}{},
	}
	go sock.receive(b.stream.Context())
	return sock, subprotocol, nil
}

// realtimeSocket is the caller's side of the session as llmproxy sees it.
type realtimeSocket struct {
	b         *realtimeBridge
	readLimit int64

	messages chan []byte
	done     chan struct{}
	err      error // set before done closes

	pingMu   sync.Mutex
	pings    map[uint64]chan struct{}
	nextPing uint64

	closeOnce sync.Once
}

func (s *realtimeSocket) receive(ctx context.Context) {
	defer close(s.done)
	for {
		in, err := s.b.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("the caller ended the session")
			}
			s.err = err
			return
		}
		switch frame := in.GetFrame().(type) {
		case *gatewayv1.RealtimeRequest_Message:
			data := frame.Message.GetData()
			if s.readLimit > 0 && int64(len(data)) > s.readLimit {
				s.err = fmt.Errorf("a caller message of %d bytes exceeds the %d byte limit", len(data), s.readLimit)
				return
			}
			select {
			case s.messages <- data:
			case <-ctx.Done():
				s.err = ctx.Err()
				return
			}
		case *gatewayv1.RealtimeRequest_Pong:
			s.pingMu.Lock()
			if waiter, ok := s.pings[frame.Pong.GetId()]; ok {
				close(waiter)
				delete(s.pings, frame.Pong.GetId())
			}
			s.pingMu.Unlock()
		case *gatewayv1.RealtimeRequest_Close:
			s.err = fmt.Errorf("the caller closed the session (status %d)", frame.Close.GetCode())
			return
		default:
			s.err = errors.New("the caller sent a frame a live session does not take")
			return
		}
	}
}

// Read returns the next caller message.
func (s *realtimeSocket) Read(ctx context.Context) ([]byte, error) {
	select {
	case data := <-s.messages:
		return data, nil
	case <-s.done:
		return nil, s.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Write sends one text message to the caller.
func (s *realtimeSocket) Write(ctx context.Context, frame []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.b.send(&gatewayv1.RealtimeResponse{Frame: &gatewayv1.RealtimeResponse_Message{
		Message: &gatewayv1.RealtimeMessage{Data: frame},
	}})
}

// Ping asks elitea-main to ping the browser and waits for the answer.
func (s *realtimeSocket) Ping(ctx context.Context) error {
	waiter := make(chan struct{})
	s.pingMu.Lock()
	s.nextPing++
	id := s.nextPing
	s.pings[id] = waiter
	s.pingMu.Unlock()
	defer func() {
		s.pingMu.Lock()
		delete(s.pings, id)
		s.pingMu.Unlock()
	}()
	if err := s.b.send(&gatewayv1.RealtimeResponse{Frame: &gatewayv1.RealtimeResponse_Ping{
		Ping: &gatewayv1.RealtimePing{Id: id},
	}}); err != nil {
		return err
	}
	select {
	case <-waiter:
		return nil
	case <-s.done:
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close sends the close status once. elitea-main closes the browser's
// WebSocket with it; the stream itself ends when the handler returns.
func (s *realtimeSocket) Close(code llmproxy.RealtimeCloseCode, reason string) {
	s.closeOnce.Do(func() {
		_ = s.b.send(&gatewayv1.RealtimeResponse{Frame: &gatewayv1.RealtimeResponse_Close{
			Close: &gatewayv1.RealtimeClose{Code: int32(code), Reason: reason}, //nolint:gosec // a close status fits
		}})
	})
}

var _ llmproxy.RealtimeSocket = (*realtimeSocket)(nil)
