// Package llmproxytest runs a test elitea.gateway.v1.GatewayService over real
// mutual TLS. Each call runs an http.Handler, the way the gateway runs its own
// /llm router, so a test writes its gateway as a plain handler and still
// crosses the real gRPC hop.
//
// The identity metadata reaches the handler as the X-Elitea-* request
// headers, exactly as elitea-main signed them.
//
// Realtime: the handler answers the handshake. When it answers 101 the server
// pings the caller once (id 7) and reports the pong as the message "pong 7".
// It echoes every message; a caller close is echoed back and ends the session.
// The message CloseRequest makes the server close with status 1008, "budget".
package llmproxytest

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	gatewayv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/gateway/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth/grpcauthtest"
)

// identityMetadata are the identity header names, as gRPC metadata keys.
var identityMetadata = []string{
	"X-Elitea-Project-Id",
	"X-Elitea-User-Id",
	"X-Elitea-Tenant-Id",
	"X-Elitea-Execution-Id",
	"X-Elitea-Identity-Signature",
}

// CloseRequest is the realtime message that makes the server close the session.
const CloseRequest = "close please"

// Server is a running test gateway.
type Server struct {
	// Target is the LLM_GATEWAY_URL of the server.
	Target string
	// PKI issued the server certificate.
	PKI *grpcauthtest.PKI

	handler http.Handler
	grpc    *grpc.Server
}

// Start runs h behind a GatewayService on a loopback port. The server is
// stopped when the test ends.
func Start(t testing.TB, h http.Handler) *Server {
	t.Helper()
	pki := grpcauthtest.NewPKI(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Target:  "https://" + lis.Addr().String(),
		PKI:     pki,
		handler: h,
		grpc:    grpc.NewServer(grpc.Creds(credentials.NewTLS(pki.ServerTLS(t)))),
	}
	gatewayv1.RegisterGatewayServiceServer(s.grpc, &service{h: h})
	go func() { _ = s.grpc.Serve(lis) }()
	t.Cleanup(s.grpc.Stop)
	return s
}

// Close stops the server: every later call fails as a dead gateway does.
func (s *Server) Close() { s.grpc.Stop() }

// ClientTLS is a client profile the server admits.
func (s *Server) ClientTLS(t testing.TB) *tls.Config {
	t.Helper()
	return s.PKI.ClientTLS(t, "elitea-main")
}

// WriteClientFiles writes the CA, a client certificate and its key under
// dir, for code that takes file paths.
func (s *Server) WriteClientFiles(t testing.TB, dir string) (caPath, certPath, keyPath string) {
	t.Helper()
	return s.PKI.WriteFiles(t, dir, "elitea-main")
}

type service struct {
	gatewayv1.UnimplementedGatewayServiceServer
	h http.Handler
}

func request(ctx context.Context, head *gatewayv1.HttpRequestHead, body io.Reader) (*http.Request, error) {
	if head == nil {
		return nil, status.Error(codes.InvalidArgument, "no request head")
	}
	if body == nil {
		body = http.NoBody
	}
	r, err := http.NewRequestWithContext(ctx, head.GetMethod(), (&url.URL{Path: head.GetPath(), RawQuery: head.GetRawQuery()}).String(), body)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	for _, hdr := range head.GetHeaders() {
		for _, v := range hdr.GetValues() {
			r.Header.Add(hdr.GetName(), v)
		}
	}
	for _, name := range identityMetadata {
		r.Header.Del(name)
	}
	md, _ := metadata.FromIncomingContext(ctx)
	for _, name := range identityMetadata {
		for _, v := range md.Get(strings.ToLower(name)) {
			r.Header.Add(name, v)
		}
	}
	if a := md.Get(":authority"); len(a) > 0 {
		r.Host = a[0]
	}
	return r, nil
}

// writer turns what a handler writes into frames.
type writer struct {
	mu     sync.Mutex
	header http.Header
	sent   bool
	send   func(*gatewayv1.ResponseFrame) error
	err    error
}

func newWriter(send func(*gatewayv1.ResponseFrame) error) *writer {
	return &writer{header: http.Header{}, send: send}
}

func (w *writer) Header() http.Header { return w.header }

func (w *writer) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sent {
		return
	}
	w.sent = true
	headers := make([]*gatewayv1.HttpHeader, 0, len(w.header))
	for name, values := range w.header {
		headers = append(headers, &gatewayv1.HttpHeader{Name: name, Values: append([]string(nil), values...)})
	}
	if err := w.send(&gatewayv1.ResponseFrame{Frame: &gatewayv1.ResponseFrame_Head{
		Head: &gatewayv1.HttpResponseHead{StatusCode: int32(code), Headers: headers}, //nolint:gosec // an HTTP status fits
	}}); err != nil {
		w.err = err
	}
}

func (w *writer) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	if err := w.send(&gatewayv1.ResponseFrame{Frame: &gatewayv1.ResponseFrame_Data{Data: append([]byte(nil), p...)}}); err != nil {
		w.err = err
		return 0, err
	}
	return len(p), nil
}

func (w *writer) Flush() {}

func (s *service) stream(ctx context.Context, in *gatewayv1.HttpRequest, send func(*gatewayv1.ResponseFrame) error) error {
	r, err := request(ctx, in.GetHead(), bytes.NewReader(in.GetBody()))
	if err != nil {
		return err
	}
	w := newWriter(send)
	s.h.ServeHTTP(w, r)
	w.WriteHeader(http.StatusOK)
	return w.err
}

func (s *service) unary(ctx context.Context, head *gatewayv1.HttpRequestHead, body io.Reader) (*gatewayv1.HttpResponse, error) {
	r, err := request(ctx, head, body)
	if err != nil {
		return nil, err
	}
	out := &gatewayv1.HttpResponse{}
	w := newWriter(func(f *gatewayv1.ResponseFrame) error {
		if h := f.GetHead(); h != nil {
			out.Head = h
		}
		out.Body = append(out.Body, f.GetData()...)
		return nil
	})
	s.h.ServeHTTP(w, r)
	w.WriteHeader(http.StatusOK)
	return out, nil
}

// clientStream reads the head frame and serves the data frames as the body.
func (s *service) clientStream(ctx context.Context, recv func() (*gatewayv1.RequestFrame, error)) (*gatewayv1.HttpResponse, error) {
	first, err := recv()
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		for {
			f, err := recv()
			if errors.Is(err, io.EOF) {
				_ = pw.Close()
				return
			}
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(f.GetData()); err != nil {
				return
			}
		}
	}()
	defer func() { _ = pr.Close() }()
	return s.unary(ctx, first.GetHead(), pr)
}

func frameSender[M any](send func(*M) error, wrap func(*gatewayv1.ResponseFrame) *M) func(*gatewayv1.ResponseFrame) error {
	return func(f *gatewayv1.ResponseFrame) error { return send(wrap(f)) }
}

func (s *service) ChatCompletions(in *gatewayv1.ChatCompletionsRequest, st grpc.ServerStreamingServer[gatewayv1.ChatCompletionsResponse]) error {
	return s.stream(st.Context(), in.GetRequest(), frameSender(st.Send, func(f *gatewayv1.ResponseFrame) *gatewayv1.ChatCompletionsResponse {
		return &gatewayv1.ChatCompletionsResponse{Frame: f}
	}))
}

func (s *service) Completions(in *gatewayv1.CompletionsRequest, st grpc.ServerStreamingServer[gatewayv1.CompletionsResponse]) error {
	return s.stream(st.Context(), in.GetRequest(), frameSender(st.Send, func(f *gatewayv1.ResponseFrame) *gatewayv1.CompletionsResponse {
		return &gatewayv1.CompletionsResponse{Frame: f}
	}))
}

func (s *service) Messages(in *gatewayv1.MessagesRequest, st grpc.ServerStreamingServer[gatewayv1.MessagesResponse]) error {
	return s.stream(st.Context(), in.GetRequest(), frameSender(st.Send, func(f *gatewayv1.ResponseFrame) *gatewayv1.MessagesResponse {
		return &gatewayv1.MessagesResponse{Frame: f}
	}))
}

func (s *service) Responses(in *gatewayv1.ResponsesRequest, st grpc.ServerStreamingServer[gatewayv1.ResponsesResponse]) error {
	return s.stream(st.Context(), in.GetRequest(), frameSender(st.Send, func(f *gatewayv1.ResponseFrame) *gatewayv1.ResponsesResponse {
		return &gatewayv1.ResponsesResponse{Frame: f}
	}))
}

func (s *service) Speech(in *gatewayv1.SpeechRequest, st grpc.ServerStreamingServer[gatewayv1.SpeechResponse]) error {
	return s.stream(st.Context(), in.GetRequest(), frameSender(st.Send, func(f *gatewayv1.ResponseFrame) *gatewayv1.SpeechResponse {
		return &gatewayv1.SpeechResponse{Frame: f}
	}))
}

func (s *service) CountTokens(ctx context.Context, in *gatewayv1.CountTokensRequest) (*gatewayv1.CountTokensResponse, error) {
	resp, err := s.unary(ctx, in.GetRequest().GetHead(), bytes.NewReader(in.GetRequest().GetBody()))
	return &gatewayv1.CountTokensResponse{Response: resp}, err
}

func (s *service) Embeddings(ctx context.Context, in *gatewayv1.EmbeddingsRequest) (*gatewayv1.EmbeddingsResponse, error) {
	resp, err := s.unary(ctx, in.GetRequest().GetHead(), bytes.NewReader(in.GetRequest().GetBody()))
	return &gatewayv1.EmbeddingsResponse{Response: resp}, err
}

func (s *service) ImageGenerate(ctx context.Context, in *gatewayv1.ImageGenerateRequest) (*gatewayv1.ImageGenerateResponse, error) {
	resp, err := s.unary(ctx, in.GetRequest().GetHead(), bytes.NewReader(in.GetRequest().GetBody()))
	return &gatewayv1.ImageGenerateResponse{Response: resp}, err
}

func (s *service) ListModels(ctx context.Context, in *gatewayv1.ListModelsRequest) (*gatewayv1.ListModelsResponse, error) {
	resp, err := s.unary(ctx, in.GetRequest().GetHead(), bytes.NewReader(in.GetRequest().GetBody()))
	return &gatewayv1.ListModelsResponse{Response: resp}, err
}

func (s *service) GetModel(ctx context.Context, in *gatewayv1.GetModelRequest) (*gatewayv1.GetModelResponse, error) {
	resp, err := s.unary(ctx, in.GetRequest().GetHead(), bytes.NewReader(in.GetRequest().GetBody()))
	return &gatewayv1.GetModelResponse{Response: resp}, err
}

func (s *service) CheckConnection(ctx context.Context, in *gatewayv1.CheckConnectionRequest) (*gatewayv1.CheckConnectionResponse, error) {
	resp, err := s.unary(ctx, in.GetRequest().GetHead(), bytes.NewReader(in.GetRequest().GetBody()))
	return &gatewayv1.CheckConnectionResponse{Response: resp}, err
}

func (s *service) ListProviderModels(ctx context.Context, in *gatewayv1.ListProviderModelsRequest) (*gatewayv1.ListProviderModelsResponse, error) {
	resp, err := s.unary(ctx, in.GetRequest().GetHead(), bytes.NewReader(in.GetRequest().GetBody()))
	return &gatewayv1.ListProviderModelsResponse{Response: resp}, err
}

func (s *service) ListProviderVoices(ctx context.Context, in *gatewayv1.ListProviderVoicesRequest) (*gatewayv1.ListProviderVoicesResponse, error) {
	resp, err := s.unary(ctx, in.GetRequest().GetHead(), bytes.NewReader(in.GetRequest().GetBody()))
	return &gatewayv1.ListProviderVoicesResponse{Response: resp}, err
}

// GovernanceStatus serves GET /governance/status through the handler.
func (s *service) GovernanceStatus(ctx context.Context, _ *gatewayv1.GovernanceStatusRequest) (*gatewayv1.GovernanceStatusResponse, error) {
	resp, err := s.unary(ctx, &gatewayv1.HttpRequestHead{Method: http.MethodGet, Path: "/governance/status"}, nil)
	return &gatewayv1.GovernanceStatusResponse{Response: resp}, err
}

func (s *service) ImageEdit(st grpc.ClientStreamingServer[gatewayv1.ImageEditRequest, gatewayv1.ImageEditResponse]) error {
	resp, err := s.clientStream(st.Context(), func() (*gatewayv1.RequestFrame, error) {
		m, err := st.Recv()
		return m.GetFrame(), err
	})
	if err != nil {
		return err
	}
	return st.SendAndClose(&gatewayv1.ImageEditResponse{Response: resp})
}

func (s *service) Transcribe(st grpc.ClientStreamingServer[gatewayv1.TranscribeRequest, gatewayv1.TranscribeResponse]) error {
	resp, err := s.clientStream(st.Context(), func() (*gatewayv1.RequestFrame, error) {
		m, err := st.Recv()
		return m.GetFrame(), err
	})
	if err != nil {
		return err
	}
	return st.SendAndClose(&gatewayv1.TranscribeResponse{Response: resp})
}

// Realtime runs the handshake through the handler, then echoes.
func (s *service) Realtime(st grpc.BidiStreamingServer[gatewayv1.RealtimeRequest, gatewayv1.RealtimeResponse]) error {
	first, err := st.Recv()
	if err != nil {
		return err
	}
	r, err := request(st.Context(), first.GetOpen(), nil)
	if err != nil {
		return err
	}
	var code int32
	w := newWriter(func(f *gatewayv1.ResponseFrame) error {
		switch frame := f.GetFrame().(type) {
		case *gatewayv1.ResponseFrame_Head:
			code = frame.Head.GetStatusCode()
			return st.Send(&gatewayv1.RealtimeResponse{Frame: &gatewayv1.RealtimeResponse_Head{Head: frame.Head}})
		case *gatewayv1.ResponseFrame_Data:
			return st.Send(&gatewayv1.RealtimeResponse{Frame: &gatewayv1.RealtimeResponse_Data{Data: frame.Data}})
		}
		return nil
	})
	s.h.ServeHTTP(w, r)
	w.WriteHeader(http.StatusOK)
	if w.err != nil || code != http.StatusSwitchingProtocols {
		return w.err
	}
	if err := st.Send(&gatewayv1.RealtimeResponse{Frame: &gatewayv1.RealtimeResponse_Ping{Ping: &gatewayv1.RealtimePing{Id: 7}}}); err != nil {
		return err
	}
	for {
		in, err := st.Recv()
		if err != nil {
			return err
		}
		switch frame := in.GetFrame().(type) {
		case *gatewayv1.RealtimeRequest_Pong:
			// Echo the pong as a message, so a test sees it arrived.
			if err := st.Send(&gatewayv1.RealtimeResponse{Frame: &gatewayv1.RealtimeResponse_Message{Message: &gatewayv1.RealtimeMessage{Data: []byte(fmt.Sprintf("pong %d", frame.Pong.GetId()))}}}); err != nil {
				return err
			}
		case *gatewayv1.RealtimeRequest_Message:
			if string(frame.Message.GetData()) == CloseRequest {
				return st.Send(&gatewayv1.RealtimeResponse{Frame: &gatewayv1.RealtimeResponse_Close{Close: &gatewayv1.RealtimeClose{Code: 1008, Reason: "budget"}}})
			}
			if err := st.Send(&gatewayv1.RealtimeResponse{Frame: &gatewayv1.RealtimeResponse_Message{Message: frame.Message}}); err != nil {
				return err
			}
		case *gatewayv1.RealtimeRequest_Close:
			return st.Send(&gatewayv1.RealtimeResponse{Frame: &gatewayv1.RealtimeResponse_Close{Close: frame.Close}})
		}
	}
}
