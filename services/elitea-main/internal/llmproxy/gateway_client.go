package llmproxy

// gateway_client.go — the Gateway implementation: gRPC over mutual TLS to
// elitea.gateway.v1.GatewayService (#1359, D4).
//
// One call carries one /llm request as an HTTP envelope: the method, path,
// query and end-to-end headers in a head, and the body as bytes. The gateway
// runs its own /llm handler on it in process and answers with the status and
// headers in a head and the body as bytes, so the bytes the browser receives
// are the bytes the handler wrote. The signed identity tuple travels as gRPC
// metadata under the lower-cased X-Elitea-* names.
//
// A non-OK gRPC status is a failed HOP, never a product answer: the gateway
// answers a budget 402, a policy 403 or a provider error as a normal response.
//
// Load balancing: the client resolves the gateway name through DNS and uses
// round_robin over every address. Point LLM_GATEWAY_URL at the headless
// Service so each replica is an address; the gateway's MaxConnectionAge makes
// the client resolve again, so a new replica receives traffic.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	gatewayv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/gateway/v1"
)

// ErrNoGatewayRoute is Do's answer for a request no GatewayService method
// serves. The gateway would answer it 404 or 405 (writeGatewayRouteMiss).
var ErrNoGatewayRoute = errors.New("llmproxy: no gateway route serves this request")

const (
	// maxGatewayRequestBody is the gateway's own JSON body ceiling (32 MiB)
	// plus one byte. A larger body is cut there, and the gateway's
	// MaxBytesReader then refuses it with the same 400 as before.
	maxGatewayRequestBody = 32<<20 + 1
	// maxGatewaySendMessage and maxGatewayRecvMessage match the gateway
	// listener (internal/grpcapi/listener.go).
	maxGatewaySendMessage = 40 << 20
	maxGatewayRecvMessage = 65 << 20
	// gatewayUploadChunk is the body bytes of one client-stream frame.
	gatewayUploadChunk = 256 << 10
)

// GatewayClientConfig configures a GatewayClient.
type GatewayClientConfig struct {
	// Target is LLM_GATEWAY_URL: "https://<host>:<port>" of the gateway's
	// gRPC listener. The port defaults to 443.
	Target string
	// IdentitySecret is GATEWAY_IDENTITY_SECRET, the HMAC key of the identity
	// tuple. An empty secret sends the tuple unsigned.
	IdentitySecret string

	// mTLS material (LLM_GATEWAY_CLIENT_CERT, LLM_GATEWAY_CLIENT_KEY,
	// LLM_GATEWAY_CA_FILE). Used when TLS is nil.
	ClientCertFile string
	ClientKeyFile  string
	CAFile         string
	// ServerName is the gateway certificate name to expect; the Target host
	// by default.
	ServerName string

	// TLS replaces the file material (tests). It must present a client
	// certificate.
	TLS *tls.Config
}

// GatewayClient is the gRPC Gateway. It is safe for concurrent use.
type GatewayClient struct {
	conn   *grpc.ClientConn
	client gatewayv1.GatewayServiceClient
	secret []byte
}

var _ Gateway = (*GatewayClient)(nil)

// NewGatewayClient builds the client. The connection is lazy: an unreachable
// gateway fails the first call, not the start.
func NewGatewayClient(cfg GatewayClientConfig) (*GatewayClient, error) {
	addr, host, err := gatewayAddress(cfg.Target)
	if err != nil {
		return nil, err
	}
	tlsConfig := cfg.TLS
	if tlsConfig == nil {
		serverName := cfg.ServerName
		if serverName == "" {
			serverName = host
		}
		tlsConfig, err = gatewayClientTLS(cfg.ClientCertFile, cfg.ClientKeyFile, cfg.CAFile, serverName)
		if err != nil {
			return nil, fmt.Errorf("llmproxy: gateway client TLS: %w", err)
		}
	} else {
		tlsConfig = tlsConfig.Clone()
		if tlsConfig.ServerName == "" {
			tlsConfig.ServerName = host
		}
	}
	conn, err := grpc.NewClient("dns:///"+addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallSendMsgSize(maxGatewaySendMessage),
			grpc.MaxCallRecvMsgSize(maxGatewayRecvMessage),
		),
		// A chat stream can be silent for minutes while a model thinks; the
		// pings keep the connection, and every proxy on the path, from
		// dropping it. The gateway admits a ping every 10s or slower.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("llmproxy: gateway client: %w", err)
	}
	return &GatewayClient{
		conn:   conn,
		client: gatewayv1.NewGatewayServiceClient(conn),
		secret: []byte(cfg.IdentitySecret),
	}, nil
}

// Close releases the connection.
func (c *GatewayClient) Close() error { return c.conn.Close() }

// gatewayAddress turns LLM_GATEWAY_URL into "host:port".
func gatewayAddress(raw string) (addr, host string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("llmproxy: LLM_GATEWAY_URL could not be parsed: %w", err)
	}
	if u.Scheme != "https" || u.Hostname() == "" {
		return "", "", fmt.Errorf("llmproxy: LLM_GATEWAY_URL must be https://<host>:<port> of the gateway gRPC listener, got %q", raw)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.User != nil {
		return "", "", fmt.Errorf("llmproxy: LLM_GATEWAY_URL must carry no path, query or userinfo, got %q", raw)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port), u.Hostname(), nil
}

// gatewayClientTLS is the client side of the gateway's mTLS listener.
func gatewayClientTLS(certFile, keyFile, caFile, serverName string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" || caFile == "" {
		return nil, errors.New("LLM_GATEWAY_CLIENT_CERT, LLM_GATEWAY_CLIENT_KEY and LLM_GATEWAY_CA_FILE are required")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	caPEM, err := os.ReadFile(caFile) //nolint:gosec // operator-configured path
	if err != nil {
		return nil, fmt.Errorf("read ca bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates parsed from ca bundle %q", caFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ---- request building ----

// hopByHopHeaders describe the caller's connection, not the request.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// requestHeaders are the end-to-end headers of h as the gateway may see
// them: no caller credentials or identity (stripIdentityHeaders), and no
// hop-by-hop headers unless keepUpgrade (the realtime handshake).
func requestHeaders(h http.Header, keepUpgrade bool) []*gatewayv1.HttpHeader {
	out := h.Clone()
	if out == nil {
		out = http.Header{}
	}
	stripIdentityHeaders(out)
	if !keepUpgrade {
		for _, v := range out.Values("Connection") {
			for _, name := range strings.Split(v, ",") {
				if name = strings.TrimSpace(name); name != "" {
					out.Del(name)
				}
			}
		}
		for _, name := range hopByHopHeaders {
			out.Del(name)
		}
	}
	out.Del("Host")
	out.Del("Content-Length")
	return toWireHeaders(out)
}

func toWireHeaders(h http.Header) []*gatewayv1.HttpHeader {
	headers := make([]*gatewayv1.HttpHeader, 0, len(h))
	for name, values := range h {
		headers = append(headers, &gatewayv1.HttpHeader{Name: name, Values: values})
	}
	return headers
}

func fromWireHeaders(headers []*gatewayv1.HttpHeader) http.Header {
	h := make(http.Header, len(headers))
	for _, hdr := range headers {
		name := http.CanonicalHeaderKey(hdr.GetName())
		h[name] = append(h[name], hdr.GetValues()...)
	}
	return h
}

// callContext adds the identity tuple to ctx as metadata, signed under the
// client's secret by the same scheme as the X-Elitea-* headers.
func (c *GatewayClient) callContext(ctx context.Context, id Identity) context.Context {
	h := http.Header{}
	SignIdentityHeaders(h, c.secret, id.ProjectID, id.UserID, id.TenantID, id.ExecutionID)
	pairs := make([]string, 0, 2*len(h))
	for name, values := range h {
		for _, v := range values {
			pairs = append(pairs, strings.ToLower(name), v)
		}
	}
	if len(pairs) == 0 {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, pairs...)
}

func requestHead(req *Request, method, path string, keepUpgrade bool) *gatewayv1.HttpRequestHead {
	if req.Method != "" {
		method = req.Method
	}
	if req.Path != "" {
		path = req.Path
	}
	return &gatewayv1.HttpRequestHead{
		Method:   method,
		Path:     path,
		RawQuery: req.RawQuery,
		Headers:  requestHeaders(req.Header, keepUpgrade),
	}
}

// readBody reads the request body up to the gateway's ceiling plus one byte.
// A read error (an edge ceiling, a cancelled caller) is returned as is.
func readBody(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(body, maxGatewayRequestBody))
}

func (c *GatewayClient) httpRequest(req *Request, method, path string) (*gatewayv1.HttpRequest, error) {
	body, err := readBody(req.Body)
	if err != nil {
		return nil, err
	}
	return &gatewayv1.HttpRequest{Head: requestHead(req, method, path, false), Body: body}, nil
}

// ---- response reading ----

// errGatewayNoHead is a call that ended without a response head.
var errGatewayNoHead = errors.New("llmproxy: the gateway answered without a response head")

func unaryResponse(resp *gatewayv1.HttpResponse, err error) (*Response, error) {
	if err != nil {
		return nil, err
	}
	head := resp.GetHead()
	if head == nil {
		return nil, errGatewayNoHead
	}
	return &Response{
		StatusCode:    int(head.GetStatusCode()),
		Header:        fromWireHeaders(head.GetHeaders()),
		Body:          io.NopCloser(bytes.NewReader(resp.GetBody())),
		ContentLength: int64(len(resp.GetBody())),
	}, nil
}

// streamResponse reads the head of a response stream and returns the body
// reader over the data frames. cancel ends the call; Body.Close calls it.
func streamResponse(recv func() (*gatewayv1.ResponseFrame, error), cancel context.CancelFunc) (*Response, error) {
	first, err := recv()
	if err != nil {
		cancel()
		if errors.Is(err, io.EOF) {
			err = errGatewayNoHead
		}
		return nil, err
	}
	head := first.GetHead()
	if head == nil {
		cancel()
		return nil, errGatewayNoHead
	}
	return &Response{
		StatusCode:    int(head.GetStatusCode()),
		Header:        fromWireHeaders(head.GetHeaders()),
		Body:          &frameBody{recv: recv, cancel: cancel},
		ContentLength: -1,
	}, nil
}

// frameBody is a response body over data frames.
type frameBody struct {
	recv   func() (*gatewayv1.ResponseFrame, error)
	cancel context.CancelFunc
	buf    []byte
	err    error
	once   sync.Once
}

func (b *frameBody) Read(p []byte) (int, error) {
	for len(b.buf) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		frame, err := b.recv()
		if err != nil {
			b.err = err
			continue
		}
		if frame.GetHead() != nil {
			b.err = errors.New("llmproxy: the gateway sent a second response head")
			continue
		}
		b.buf = frame.GetData()
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

func (b *frameBody) Close() error {
	b.once.Do(b.cancel)
	return nil
}

// serverStream runs a server-streaming RPC.
func serverStream[Resp any, PResp interface {
	*Resp
	GetFrame() *gatewayv1.ResponseFrame
}](
	ctx context.Context,
	open func(ctx context.Context) (grpc.ServerStreamingClient[Resp], error),
) (*Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	stream, err := open(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	return streamResponse(func() (*gatewayv1.ResponseFrame, error) {
		msg, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		return PResp(msg).GetFrame(), nil
	}, cancel)
}

// clientStream runs a client-streaming RPC: the head frame, then the body in
// chunks. A body read error ends the call and is returned as is.
func clientStream[Req any, Resp any, PResp interface {
	*Resp
	GetResponse() *gatewayv1.HttpResponse
}](
	ctx context.Context,
	open func(ctx context.Context, opts ...grpc.CallOption) (grpc.ClientStreamingClient[Req, Resp], error),
	wrap func(*gatewayv1.RequestFrame) *Req,
	head *gatewayv1.HttpRequestHead,
	body io.Reader,
) (*Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := open(ctx)
	if err != nil {
		return nil, err
	}
	finish := func() (*Response, error) {
		resp, err := stream.CloseAndRecv()
		if err != nil {
			return nil, err
		}
		return unaryResponse(PResp(resp).GetResponse(), nil)
	}
	if err := stream.Send(wrap(&gatewayv1.RequestFrame{Frame: &gatewayv1.RequestFrame_Head{Head: head}})); err != nil {
		// io.EOF: the gateway ended the call; CloseAndRecv has its status.
		return finish()
	}
	if body != nil {
		buf := make([]byte, gatewayUploadChunk)
		for {
			n, rerr := body.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				if err := stream.Send(wrap(&gatewayv1.RequestFrame{Frame: &gatewayv1.RequestFrame_Data{Data: chunk}})); err != nil {
					// The gateway answered before it read the whole body
					// (a refusal); its answer is the response.
					return finish()
				}
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				return nil, rerr
			}
		}
	}
	return finish()
}

// ---- the Gateway methods ----

func (c *GatewayClient) ChatCompletions(ctx context.Context, req *Request) (*Response, error) {
	in, err := c.httpRequest(req, http.MethodPost, "/llm/v1/chat/completions")
	if err != nil {
		return nil, err
	}
	return serverStream(c.callContext(ctx, req.Identity), func(ctx context.Context) (grpc.ServerStreamingClient[gatewayv1.ChatCompletionsResponse], error) {
		return c.client.ChatCompletions(ctx, &gatewayv1.ChatCompletionsRequest{Request: in})
	})
}

func (c *GatewayClient) Completions(ctx context.Context, req *Request) (*Response, error) {
	in, err := c.httpRequest(req, http.MethodPost, "/llm/v1/completions")
	if err != nil {
		return nil, err
	}
	return serverStream(c.callContext(ctx, req.Identity), func(ctx context.Context) (grpc.ServerStreamingClient[gatewayv1.CompletionsResponse], error) {
		return c.client.Completions(ctx, &gatewayv1.CompletionsRequest{Request: in})
	})
}

func (c *GatewayClient) Messages(ctx context.Context, req *Request) (*Response, error) {
	in, err := c.httpRequest(req, http.MethodPost, "/llm/v1/messages")
	if err != nil {
		return nil, err
	}
	return serverStream(c.callContext(ctx, req.Identity), func(ctx context.Context) (grpc.ServerStreamingClient[gatewayv1.MessagesResponse], error) {
		return c.client.Messages(ctx, &gatewayv1.MessagesRequest{Request: in})
	})
}

func (c *GatewayClient) Responses(ctx context.Context, req *Request) (*Response, error) {
	in, err := c.httpRequest(req, http.MethodPost, "/llm/v1/responses")
	if err != nil {
		return nil, err
	}
	return serverStream(c.callContext(ctx, req.Identity), func(ctx context.Context) (grpc.ServerStreamingClient[gatewayv1.ResponsesResponse], error) {
		return c.client.Responses(ctx, &gatewayv1.ResponsesRequest{Request: in})
	})
}

func (c *GatewayClient) Speech(ctx context.Context, req *Request) (*Response, error) {
	in, err := c.httpRequest(req, http.MethodPost, "/llm/v1/audio/speech")
	if err != nil {
		return nil, err
	}
	return serverStream(c.callContext(ctx, req.Identity), func(ctx context.Context) (grpc.ServerStreamingClient[gatewayv1.SpeechResponse], error) {
		return c.client.Speech(ctx, &gatewayv1.SpeechRequest{Request: in})
	})
}

func (c *GatewayClient) CountTokens(ctx context.Context, req *Request) (*Response, error) {
	in, err := c.httpRequest(req, http.MethodPost, "/llm/v1/messages/count_tokens")
	if err != nil {
		return nil, err
	}
	resp, err := c.client.CountTokens(c.callContext(ctx, req.Identity), &gatewayv1.CountTokensRequest{Request: in})
	return unaryResponse(resp.GetResponse(), err)
}

func (c *GatewayClient) Embeddings(ctx context.Context, req *Request) (*Response, error) {
	in, err := c.httpRequest(req, http.MethodPost, "/llm/v1/embeddings")
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Embeddings(c.callContext(ctx, req.Identity), &gatewayv1.EmbeddingsRequest{Request: in})
	return unaryResponse(resp.GetResponse(), err)
}

func (c *GatewayClient) ImageGenerate(ctx context.Context, req *Request) (*Response, error) {
	in, err := c.httpRequest(req, http.MethodPost, "/llm/v1/images/generations")
	if err != nil {
		return nil, err
	}
	resp, err := c.client.ImageGenerate(c.callContext(ctx, req.Identity), &gatewayv1.ImageGenerateRequest{Request: in})
	return unaryResponse(resp.GetResponse(), err)
}

func (c *GatewayClient) ListModels(ctx context.Context, req *Request) (*Response, error) {
	in, err := c.httpRequest(req, http.MethodGet, "/llm/v1/models")
	if err != nil {
		return nil, err
	}
	resp, err := c.client.ListModels(c.callContext(ctx, req.Identity), &gatewayv1.ListModelsRequest{Request: in})
	return unaryResponse(resp.GetResponse(), err)
}

func (c *GatewayClient) GetModel(ctx context.Context, req *Request) (*Response, error) {
	in, err := c.httpRequest(req, http.MethodGet, "")
	if err != nil {
		return nil, err
	}
	resp, err := c.client.GetModel(c.callContext(ctx, req.Identity), &gatewayv1.GetModelRequest{Request: in})
	return unaryResponse(resp.GetResponse(), err)
}

func (c *GatewayClient) CheckConnection(ctx context.Context, req *Request) (*Response, error) {
	in, err := c.httpRequest(req, http.MethodPost, "/llm/v1/check_connection")
	if err != nil {
		return nil, err
	}
	resp, err := c.client.CheckConnection(c.callContext(ctx, req.Identity), &gatewayv1.CheckConnectionRequest{Request: in})
	return unaryResponse(resp.GetResponse(), err)
}

func (c *GatewayClient) ListProviderModels(ctx context.Context, req *Request) (*Response, error) {
	in, err := c.httpRequest(req, http.MethodPost, "/llm/v1/list_provider_models")
	if err != nil {
		return nil, err
	}
	resp, err := c.client.ListProviderModels(c.callContext(ctx, req.Identity), &gatewayv1.ListProviderModelsRequest{Request: in})
	return unaryResponse(resp.GetResponse(), err)
}

func (c *GatewayClient) ListProviderVoices(ctx context.Context, req *Request) (*Response, error) {
	in, err := c.httpRequest(req, http.MethodPost, "/llm/v1/list_provider_voices")
	if err != nil {
		return nil, err
	}
	resp, err := c.client.ListProviderVoices(c.callContext(ctx, req.Identity), &gatewayv1.ListProviderVoicesRequest{Request: in})
	return unaryResponse(resp.GetResponse(), err)
}

func (c *GatewayClient) GovernanceStatus(ctx context.Context) (*Response, error) {
	resp, err := c.client.GovernanceStatus(ctx, &gatewayv1.GovernanceStatusRequest{})
	return unaryResponse(resp.GetResponse(), err)
}

func (c *GatewayClient) ImageEdit(ctx context.Context, req *Request) (*Response, error) {
	return clientStream(c.callContext(ctx, req.Identity), c.client.ImageEdit,
		func(f *gatewayv1.RequestFrame) *gatewayv1.ImageEditRequest {
			return &gatewayv1.ImageEditRequest{Frame: f}
		},
		requestHead(req, http.MethodPost, "/llm/v1/images/edits", false), req.Body)
}

func (c *GatewayClient) Transcribe(ctx context.Context, req *Request) (*Response, error) {
	return clientStream(c.callContext(ctx, req.Identity), c.client.Transcribe,
		func(f *gatewayv1.RequestFrame) *gatewayv1.TranscribeRequest {
			return &gatewayv1.TranscribeRequest{Frame: f}
		},
		requestHead(req, http.MethodPost, "/llm/v1/audio/transcriptions", false), req.Body)
}
