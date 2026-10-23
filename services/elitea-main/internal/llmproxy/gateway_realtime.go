package llmproxy

// gateway_realtime.go — the Realtime method of GatewayClient: one realtime
// session over the bidirectional GatewayService Realtime stream.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	gatewayv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/gateway/v1"
)

// Realtime opens a session. The handshake headers travel with the
// connection headers kept: the gateway runs the WebSocket handshake checks on
// them.
func (c *GatewayClient) Realtime(ctx context.Context, req *Request) (RealtimeSession, *Response, error) {
	ctx, cancel := context.WithCancel(c.callContext(ctx, req.Identity))
	stream, err := c.client.Realtime(ctx)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	open := requestHead(req, http.MethodGet, "/llm/v1/realtime", true)
	if err := stream.Send(&gatewayv1.RealtimeRequest{Frame: &gatewayv1.RealtimeRequest_Open{Open: open}}); err != nil {
		// io.EOF: the call ended; Recv returns its status.
		if !errors.Is(err, io.EOF) {
			cancel()
			return nil, nil, err
		}
	}
	recv := func() (*gatewayv1.ResponseFrame, error) {
		in, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		switch frame := in.GetFrame().(type) {
		case *gatewayv1.RealtimeResponse_Head:
			return &gatewayv1.ResponseFrame{Frame: &gatewayv1.ResponseFrame_Head{Head: frame.Head}}, nil
		case *gatewayv1.RealtimeResponse_Data:
			return &gatewayv1.ResponseFrame{Frame: &gatewayv1.ResponseFrame_Data{Data: frame.Data}}, nil
		}
		return nil, errors.New("llmproxy: the gateway sent a session frame before the upgrade")
	}
	resp, err := streamResponse(recv, cancel)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		// A refusal: the head and body are the answer.
		return nil, resp, nil
	}
	resp.Body = http.NoBody
	resp.ContentLength = 0
	return &realtimeSession{stream: stream, cancel: cancel}, resp, nil
}

type realtimeSession struct {
	stream interface {
		Send(*gatewayv1.RealtimeRequest) error
		Recv() (*gatewayv1.RealtimeResponse, error)
		CloseSend() error
	}
	cancel context.CancelFunc
	sendMu sync.Mutex
}

func (s *realtimeSession) send(frame *gatewayv1.RealtimeRequest) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(frame)
}

func (s *realtimeSession) Recv() (RealtimeEvent, error) {
	in, err := s.stream.Recv()
	if err != nil {
		return RealtimeEvent{}, err
	}
	switch frame := in.GetFrame().(type) {
	case *gatewayv1.RealtimeResponse_Message:
		return RealtimeEvent{Message: frame.Message.GetData(), Binary: frame.Message.GetBinary()}, nil
	case *gatewayv1.RealtimeResponse_Ping:
		return RealtimeEvent{PingID: frame.Ping.GetId()}, nil
	case *gatewayv1.RealtimeResponse_Close:
		return RealtimeEvent{Closed: true, CloseCode: int(frame.Close.GetCode()), CloseReason: frame.Close.GetReason()}, nil
	}
	return RealtimeEvent{}, errors.New("llmproxy: the gateway sent an HTTP frame inside a live session")
}

func (s *realtimeSession) Send(data []byte, binary bool) error {
	return s.send(&gatewayv1.RealtimeRequest{Frame: &gatewayv1.RealtimeRequest_Message{
		Message: &gatewayv1.RealtimeMessage{Data: data, Binary: binary},
	}})
}

func (s *realtimeSession) Pong(id uint64) error {
	return s.send(&gatewayv1.RealtimeRequest{Frame: &gatewayv1.RealtimeRequest_Pong{Pong: &gatewayv1.RealtimePong{Id: id}}})
}

func (s *realtimeSession) Close(code int, reason string) error {
	err := s.send(&gatewayv1.RealtimeRequest{Frame: &gatewayv1.RealtimeRequest_Close{
		Close: &gatewayv1.RealtimeClose{Code: int32(code), Reason: reason}, //nolint:gosec // a close status fits
	}})
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return errors.Join(err, s.stream.CloseSend())
}

func (s *realtimeSession) End() { s.cancel() }
