// Package grpcapi serves elitea.gateway.v1.GatewayService: the gRPC front of
// the gateway that elitea-main calls for every /llm request (#1359, D4).
//
// Each RPC runs the gateway's own /llm http.Handler in process (bridge.go), so
// the dialect handling, identity verification, budget and policy gates and
// billing are the code the HTTP listener used to serve. The HTTP listener now
// serves health and metrics only.
//
// Who may call: the listener requires a client certificate from the gateway CA
// (GATEWAY_TLS_CA_FILE), and the Allowlist admits only the configured workload
// identities (GATEWAY_GRPC_CLIENTS, elitea-main by default).
//
// Scaling: every call is self-contained. A stream, and a realtime session, live
// on the replica that accepted them; budget counters and rate limits are
// already shared through NATS. elitea-main balances calls across replicas with
// gRPC round_robin over the headless Service, and MaxConnectionAge makes it
// re-resolve so a scaled-out replica receives traffic.
package grpcapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gatewayv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/gateway/v1"
)

// Config wires the service to the gateway's handlers.
type Config struct {
	// LLM is the /llm router (internal/api.NewRouterWithLog).
	LLM http.Handler
	// Status serves GET /governance/status.
	Status http.Handler
	Logger *slog.Logger
}

// Service implements gatewayv1.GatewayServiceServer.
type Service struct {
	// Unsafe, not Unimplemented: a missing RPC is a compile error, never a
	// silent UNIMPLEMENTED answer.
	gatewayv1.UnsafeGatewayServiceServer

	llm    http.Handler
	status http.Handler
	logger *slog.Logger

	// calls counts the calls in flight that are not realtime sessions, so the
	// shutdown sequence can wait for them (Drain) while the sessions keep
	// running until CloseRealtimeSessions ends them.
	calls    sync.WaitGroup
	callsMu  sync.Mutex
	draining atomic.Bool
}

// NewService builds the service. Both handlers are required.
func NewService(cfg Config) (*Service, error) {
	if cfg.LLM == nil || cfg.Status == nil {
		return nil, errors.New("grpcapi: the /llm and status handlers are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{llm: cfg.LLM, status: cfg.Status, logger: logger}, nil
}

// begin registers a call; it refuses once Drain started.
func (s *Service) begin() (func(), error) {
	s.callsMu.Lock()
	defer s.callsMu.Unlock()
	if s.draining.Load() {
		return nil, status.Error(codes.Unavailable, "the gateway is shutting down; retry against another replica")
	}
	s.calls.Add(1)
	return s.calls.Done, nil
}

// Drain refuses new calls and waits, bounded by ctx, for the calls in flight
// that are not realtime sessions.
func (s *Service) Drain(ctx context.Context) error {
	s.callsMu.Lock()
	s.draining.Store(true)
	s.callsMu.Unlock()
	done := make(chan struct{})
	go func() {
		s.calls.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) serveStreaming(ctx context.Context, rt route, in *gatewayv1.HttpRequest, send func(*gatewayv1.ResponseFrame) error) error {
	done, err := s.begin()
	if err != nil {
		return err
	}
	defer done()
	if in == nil {
		return status.Error(codes.InvalidArgument, "the request is required")
	}
	req, err := newRequest(ctx, rt, in.GetHead(), bytes.NewReader(in.GetBody()), int64(len(in.GetBody())))
	if err != nil {
		return err
	}
	w := newStreamWriter(send)
	if err := serveHTTP(s.llm, w, req); err != nil {
		return err
	}
	return w.finish()
}

func (s *Service) serveUnary(ctx context.Context, handler http.Handler, rt route, in *gatewayv1.HttpRequest) (*gatewayv1.HttpResponse, error) {
	done, err := s.begin()
	if err != nil {
		return nil, err
	}
	defer done()
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "the request is required")
	}
	req, err := newRequest(ctx, rt, in.GetHead(), bytes.NewReader(in.GetBody()), int64(len(in.GetBody())))
	if err != nil {
		return nil, err
	}
	w := newBufferWriter()
	if err := serveHTTP(handler, w, req); err != nil {
		return nil, err
	}
	return w.response()
}

func (s *Service) serveClientStream(ctx context.Context, rt route, recv func() (*gatewayv1.RequestFrame, error)) (*gatewayv1.HttpResponse, error) {
	done, err := s.begin()
	if err != nil {
		return nil, err
	}
	defer done()
	first, err := recv()
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "the request head is required")
	}
	head := first.GetHead()
	if head == nil {
		return nil, status.Error(codes.InvalidArgument, "the first request frame must be the head")
	}
	req, err := newRequest(ctx, rt, head, &frameBody{recv: recv}, -1)
	if err != nil {
		return nil, err
	}
	w := newBufferWriter()
	if err := serveHTTP(s.llm, w, req); err != nil {
		return nil, err
	}
	return w.response()
}

func (s *Service) ChatCompletions(in *gatewayv1.ChatCompletionsRequest, stream gatewayv1.GatewayService_ChatCompletionsServer) error {
	return s.serveStreaming(stream.Context(), routeChat, in.GetRequest(), func(f *gatewayv1.ResponseFrame) error {
		return stream.Send(&gatewayv1.ChatCompletionsResponse{Frame: f})
	})
}

func (s *Service) Completions(in *gatewayv1.CompletionsRequest, stream gatewayv1.GatewayService_CompletionsServer) error {
	return s.serveStreaming(stream.Context(), routeCompletions, in.GetRequest(), func(f *gatewayv1.ResponseFrame) error {
		return stream.Send(&gatewayv1.CompletionsResponse{Frame: f})
	})
}

func (s *Service) Messages(in *gatewayv1.MessagesRequest, stream gatewayv1.GatewayService_MessagesServer) error {
	return s.serveStreaming(stream.Context(), routeMessages, in.GetRequest(), func(f *gatewayv1.ResponseFrame) error {
		return stream.Send(&gatewayv1.MessagesResponse{Frame: f})
	})
}

func (s *Service) Responses(in *gatewayv1.ResponsesRequest, stream gatewayv1.GatewayService_ResponsesServer) error {
	return s.serveStreaming(stream.Context(), routeResponses, in.GetRequest(), func(f *gatewayv1.ResponseFrame) error {
		return stream.Send(&gatewayv1.ResponsesResponse{Frame: f})
	})
}

func (s *Service) Speech(in *gatewayv1.SpeechRequest, stream gatewayv1.GatewayService_SpeechServer) error {
	return s.serveStreaming(stream.Context(), routeSpeech, in.GetRequest(), func(f *gatewayv1.ResponseFrame) error {
		return stream.Send(&gatewayv1.SpeechResponse{Frame: f})
	})
}

func (s *Service) CountTokens(ctx context.Context, in *gatewayv1.CountTokensRequest) (*gatewayv1.CountTokensResponse, error) {
	resp, err := s.serveUnary(ctx, s.llm, routeCountTokens, in.GetRequest())
	if err != nil {
		return nil, err
	}
	return &gatewayv1.CountTokensResponse{Response: resp}, nil
}

func (s *Service) Embeddings(ctx context.Context, in *gatewayv1.EmbeddingsRequest) (*gatewayv1.EmbeddingsResponse, error) {
	resp, err := s.serveUnary(ctx, s.llm, routeEmbeddings, in.GetRequest())
	if err != nil {
		return nil, err
	}
	return &gatewayv1.EmbeddingsResponse{Response: resp}, nil
}

func (s *Service) ImageGenerate(ctx context.Context, in *gatewayv1.ImageGenerateRequest) (*gatewayv1.ImageGenerateResponse, error) {
	resp, err := s.serveUnary(ctx, s.llm, routeImageGenerate, in.GetRequest())
	if err != nil {
		return nil, err
	}
	return &gatewayv1.ImageGenerateResponse{Response: resp}, nil
}

func (s *Service) ListModels(ctx context.Context, in *gatewayv1.ListModelsRequest) (*gatewayv1.ListModelsResponse, error) {
	resp, err := s.serveUnary(ctx, s.llm, routeListModels, in.GetRequest())
	if err != nil {
		return nil, err
	}
	return &gatewayv1.ListModelsResponse{Response: resp}, nil
}

func (s *Service) GetModel(ctx context.Context, in *gatewayv1.GetModelRequest) (*gatewayv1.GetModelResponse, error) {
	resp, err := s.serveUnary(ctx, s.llm, routeGetModel, in.GetRequest())
	if err != nil {
		return nil, err
	}
	return &gatewayv1.GetModelResponse{Response: resp}, nil
}

func (s *Service) CheckConnection(ctx context.Context, in *gatewayv1.CheckConnectionRequest) (*gatewayv1.CheckConnectionResponse, error) {
	resp, err := s.serveUnary(ctx, s.llm, routeCheckConnection, in.GetRequest())
	if err != nil {
		return nil, err
	}
	return &gatewayv1.CheckConnectionResponse{Response: resp}, nil
}

func (s *Service) ListProviderModels(ctx context.Context, in *gatewayv1.ListProviderModelsRequest) (*gatewayv1.ListProviderModelsResponse, error) {
	resp, err := s.serveUnary(ctx, s.llm, routeListProviderModels, in.GetRequest())
	if err != nil {
		return nil, err
	}
	return &gatewayv1.ListProviderModelsResponse{Response: resp}, nil
}

func (s *Service) ListProviderVoices(ctx context.Context, in *gatewayv1.ListProviderVoicesRequest) (*gatewayv1.ListProviderVoicesResponse, error) {
	resp, err := s.serveUnary(ctx, s.llm, routeListProviderVoices, in.GetRequest())
	if err != nil {
		return nil, err
	}
	return &gatewayv1.ListProviderVoicesResponse{Response: resp}, nil
}

func (s *Service) MeteredAdmit(ctx context.Context, in *gatewayv1.MeteredAdmitRequest) (*gatewayv1.MeteredAdmitResponse, error) {
	resp, err := s.serveUnary(ctx, s.llm, routeMeteredAdmit, in.GetRequest())
	if err != nil {
		return nil, err
	}
	return &gatewayv1.MeteredAdmitResponse{Response: resp}, nil
}

func (s *Service) MeteredSettle(ctx context.Context, in *gatewayv1.MeteredSettleRequest) (*gatewayv1.MeteredSettleResponse, error) {
	resp, err := s.serveUnary(ctx, s.llm, routeMeteredSettle, in.GetRequest())
	if err != nil {
		return nil, err
	}
	return &gatewayv1.MeteredSettleResponse{Response: resp}, nil
}

// routeGovernanceStatus is the one route of the status handler.
var routeGovernanceStatus = route{methods: []string{http.MethodGet}, paths: []string{"/governance/status"}}

func (s *Service) GovernanceStatus(ctx context.Context, _ *gatewayv1.GovernanceStatusRequest) (*gatewayv1.GovernanceStatusResponse, error) {
	resp, err := s.serveUnary(ctx, s.status, routeGovernanceStatus, &gatewayv1.HttpRequest{
		Head: &gatewayv1.HttpRequestHead{Method: http.MethodGet, Path: "/governance/status"},
	})
	if err != nil {
		return nil, err
	}
	return &gatewayv1.GovernanceStatusResponse{Response: resp}, nil
}

func (s *Service) ImageEdit(stream gatewayv1.GatewayService_ImageEditServer) error {
	resp, err := s.serveClientStream(stream.Context(), routeImageEdit, func() (*gatewayv1.RequestFrame, error) {
		in, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		return in.GetFrame(), nil
	})
	if err != nil {
		return err
	}
	return stream.SendAndClose(&gatewayv1.ImageEditResponse{Response: resp})
}

func (s *Service) Transcribe(stream gatewayv1.GatewayService_TranscribeServer) error {
	resp, err := s.serveClientStream(stream.Context(), routeTranscribe, func() (*gatewayv1.RequestFrame, error) {
		in, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		return in.GetFrame(), nil
	})
	if err != nil {
		return err
	}
	return stream.SendAndClose(&gatewayv1.TranscribeResponse{Response: resp})
}

// compile-time proofs of the shapes the handler relies on.
var (
	_ gatewayv1.GatewayServiceServer = (*Service)(nil)
	_ io.ReadCloser                  = (*frameBody)(nil)
	_ http.Flusher                   = (*streamWriter)(nil)
	_ http.Flusher                   = (*bufferWriter)(nil)
)
