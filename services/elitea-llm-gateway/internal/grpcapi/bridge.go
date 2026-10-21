package grpcapi

// bridge.go — one GatewayService call becomes one in-process call of the
// gateway's own /llm handler.
//
// The handler is the same http.Handler the HTTP listener used to serve
// (internal/api.NewRouterWithLog): request log, hop guard, identity
// verification, model mapping, budget and policy gates, dialect handling and
// billing all run unchanged. Nothing loops back over the network: the request
// is built from the call's head and body, and the response writer turns what
// the handler writes into response frames.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	gatewayv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/gateway/v1"
)

// Identity metadata: the X-Elitea-* header names, lower-cased. elitea-main
// signs the tuple with the same HMAC it signed the HTTP headers with, and the
// handler verifies it exactly as before (llmproxy/identity.go).
var identityHeaders = []string{
	"X-Elitea-Project-Id",
	"X-Elitea-User-Id",
	"X-Elitea-Tenant-Id",
	"X-Elitea-Execution-Id",
	"X-Elitea-Identity-Signature",
}

// droppedRequestHeaders never travel from the request head to the handler.
// Hop-by-hop headers describe main's connection to the browser, not this
// request; Host and Content-Length are set from the call itself; the identity
// tuple is accepted from metadata only.
var droppedRequestHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Connection":    true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Host":                true,
	"Content-Length":      true,
}

func init() {
	for _, name := range identityHeaders {
		droppedRequestHeaders[name] = true
	}
}

const (
	// maxUnaryResponse bounds a response collected into one message.
	maxUnaryResponse = 64 << 20
	// streamFlushThreshold sends buffered body bytes once this many collect
	// between two Flush calls.
	streamFlushThreshold = 32 << 10
	// maxDataFrame bounds the body bytes of one response frame.
	maxDataFrame = 1 << 20
)

// route names the HTTP requests one RPC serves.
type route struct {
	methods []string
	paths   []string
	// prefix admits any path below it ("/llm/v1/models/" for GetModel).
	prefix string
	// upgrade keeps Connection and Upgrade (the realtime handshake).
	upgrade bool
}

func (rt route) admits(head *gatewayv1.HttpRequestHead) bool {
	methodOK := false
	for _, m := range rt.methods {
		if head.GetMethod() == m {
			methodOK = true
		}
	}
	if !methodOK {
		return false
	}
	p := head.GetPath()
	if p == "" || !strings.HasPrefix(p, "/") || strings.Contains(p, "..") || strings.ContainsAny(p, "?#\r\n\x00") {
		return false
	}
	for _, allowed := range rt.paths {
		if p == allowed {
			return true
		}
	}
	return rt.prefix != "" && strings.HasPrefix(p, rt.prefix) && len(p) > len(rt.prefix)
}

var (
	routeChat               = route{methods: []string{http.MethodPost}, paths: []string{"/llm/v1/chat/completions"}}
	routeCompletions        = route{methods: []string{http.MethodPost}, paths: []string{"/llm/v1/completions"}}
	routeMessages           = route{methods: []string{http.MethodPost}, paths: []string{"/llm/v1/messages"}}
	routeCountTokens        = route{methods: []string{http.MethodPost}, paths: []string{"/llm/v1/messages/count_tokens"}}
	routeResponses          = route{methods: []string{http.MethodPost}, paths: []string{"/llm/v1/responses"}}
	routeEmbeddings         = route{methods: []string{http.MethodPost}, paths: []string{"/llm/v1/embeddings"}}
	routeImageGenerate      = route{methods: []string{http.MethodPost}, paths: []string{"/llm/v1/images/generations"}}
	routeImageEdit          = route{methods: []string{http.MethodPost}, paths: []string{"/llm/v1/images/edits", "/llm/v1/images/variations"}}
	routeSpeech             = route{methods: []string{http.MethodPost}, paths: []string{"/llm/v1/audio/speech"}}
	routeTranscribe         = route{methods: []string{http.MethodPost}, paths: []string{"/llm/v1/audio/transcriptions", "/llm/v1/audio/translations"}}
	routeRealtime           = route{methods: []string{http.MethodGet, http.MethodPost}, paths: []string{"/llm/v1/realtime"}, upgrade: true}
	routeListModels         = route{methods: []string{http.MethodGet}, paths: []string{"/llm/v1/models"}}
	routeGetModel           = route{methods: []string{http.MethodGet}, prefix: "/llm/v1/models/"}
	routeCheckConnection    = route{methods: []string{http.MethodPost}, paths: []string{"/llm/v1/check_connection"}}
	routeListProviderModels = route{methods: []string{http.MethodPost}, paths: []string{"/llm/v1/list_provider_models"}}
	routeListProviderVoices = route{methods: []string{http.MethodPost}, paths: []string{"/llm/v1/list_provider_voices"}}
)

// newRequest builds the handler's request from a call. body is the request
// body; contentLength is its size, or -1 when it streams.
func newRequest(ctx context.Context, rt route, head *gatewayv1.HttpRequestHead, body io.Reader, contentLength int64) (*http.Request, error) {
	if head == nil || !rt.admits(head) {
		return nil, status.Error(codes.InvalidArgument, "the request head names a method or path this RPC does not serve")
	}
	if strings.ContainsAny(head.GetRawQuery(), "#\r\n\x00") {
		return nil, status.Error(codes.InvalidArgument, "the request query is malformed")
	}
	target := &url.URL{Path: head.GetPath(), RawQuery: head.GetRawQuery()}
	if body == nil {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, head.GetMethod(), target.RequestURI(), body)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "the request head is malformed")
	}
	req.RequestURI = target.RequestURI()
	req.Proto, req.ProtoMajor, req.ProtoMinor = "HTTP/1.1", 1, 1
	req.ContentLength = contentLength
	if contentLength == 0 {
		req.Body = http.NoBody
	}
	for _, h := range head.GetHeaders() {
		name := http.CanonicalHeaderKey(h.GetName())
		if name == "" || strings.ContainsAny(name, " :\r\n\x00") {
			return nil, status.Error(codes.InvalidArgument, "the request head carries a malformed header name")
		}
		upgradeHeader := rt.upgrade && (name == "Connection" || name == "Upgrade")
		if droppedRequestHeaders[name] && !upgradeHeader {
			continue
		}
		for _, v := range h.GetValues() {
			if strings.ContainsAny(v, "\r\n\x00") {
				return nil, status.Error(codes.InvalidArgument, "the request head carries a malformed header value")
			}
			req.Header.Add(name, v)
		}
	}
	incoming, _ := metadata.FromIncomingContext(ctx)
	for _, name := range identityHeaders {
		values := incoming.Get(strings.ToLower(name))
		if len(values) > 1 {
			return nil, status.Error(codes.InvalidArgument, "an identity metadata entry is repeated")
		}
		if len(values) == 1 && values[0] != "" {
			if strings.ContainsAny(values[0], "\r\n\x00") {
				return nil, status.Error(codes.InvalidArgument, "an identity metadata entry is malformed")
			}
			req.Header.Set(name, values[0])
		}
	}
	// Host is the name the caller dialled, as it was on the HTTP hop: the
	// realtime Origin rule compares against it.
	if authority := incoming.Get(":authority"); len(authority) == 1 {
		req.Host = authority[0]
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		req.RemoteAddr = p.Addr.String()
	}
	return req, nil
}

// serveHTTP runs handler and turns a handler panic into a call error. net/http
// recovers a panicking handler per connection; a gRPC server does not, so
// without this one bad request would end the process.
func serveHTTP(handler http.Handler, w http.ResponseWriter, req *http.Request) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler { //nolint:errorlint // the sentinel is panicked by value
				err = status.Error(codes.Aborted, "the handler aborted the response")
				return
			}
			err = status.Errorf(codes.Internal, "the handler failed: %v", p)
		}
	}()
	handler.ServeHTTP(w, req)
	return nil
}

func headFromHeader(statusCode int, header http.Header) *gatewayv1.HttpResponseHead {
	head := &gatewayv1.HttpResponseHead{StatusCode: int32(statusCode)} //nolint:gosec // an HTTP status fits
	for name, values := range header {
		head.Headers = append(head.Headers, &gatewayv1.HttpHeader{Name: name, Values: append([]string(nil), values...)})
	}
	return head
}

// streamWriter is the http.ResponseWriter of a streaming response. The head
// is sent with the first flush (an explicit Flush, a full buffer, or the end
// of the handler), as net/http sends it; body bytes follow in frames.
type streamWriter struct {
	mu      sync.Mutex
	header  http.Header
	send    func(*gatewayv1.ResponseFrame) error
	status  int
	headOut bool
	buf     []byte
	done    bool
	err     error
	// upgrade sends a 101 head at once and refuses body writes after it:
	// the realtime session continues as messages.
	upgrade  bool
	upgraded bool
}

func newStreamWriter(send func(*gatewayv1.ResponseFrame) error) *streamWriter {
	return &streamWriter{header: http.Header{}, send: send}
}

func (w *streamWriter) Header() http.Header { return w.header }

func (w *streamWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status != 0 || w.done {
		return
	}
	if code == http.StatusSwitchingProtocols && w.upgrade {
		w.status = code
		w.upgraded = true
		w.flushLocked()
		return
	}
	if code >= 100 && code < 200 {
		// An informational status is not the response; net/http sends it
		// on its own and keeps going. There is nothing to relay.
		return
	}
	w.status = code
}

func (w *streamWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done || w.upgraded {
		return 0, errors.New("grpcapi: the response is closed")
	}
	if w.err != nil {
		return 0, w.err
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.buf = append(w.buf, b...)
	if len(w.buf) >= streamFlushThreshold {
		w.flushLocked()
	}
	if w.err != nil {
		return 0, w.err
	}
	return len(b), nil
}

// Flush implements http.Flusher; the SSE path asserts it directly.
func (w *streamWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.flushLocked()
}

func (w *streamWriter) flushLocked() {
	if w.err != nil {
		return
	}
	if !w.headOut {
		w.headOut = true
		if err := w.send(&gatewayv1.ResponseFrame{Frame: &gatewayv1.ResponseFrame_Head{Head: headFromHeader(w.status, w.header)}}); err != nil {
			w.err = err
			return
		}
	}
	for len(w.buf) > 0 {
		n := min(len(w.buf), maxDataFrame)
		chunk := append([]byte(nil), w.buf[:n]...)
		w.buf = w.buf[n:]
		if err := w.send(&gatewayv1.ResponseFrame{Frame: &gatewayv1.ResponseFrame_Data{Data: chunk}}); err != nil {
			w.err = err
			return
		}
	}
	w.buf = w.buf[:0]
}

// finish sends what is left and closes the writer. A handler that wrote
// nothing answered 200 with an empty body, as net/http answers.
func (w *streamWriter) finish() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return w.err
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if !w.upgraded {
		w.flushLocked()
	}
	w.done = true
	return w.err
}

// bufferWriter is the http.ResponseWriter of a unary response.
type bufferWriter struct {
	mu     sync.Mutex
	header http.Header
	status int
	body   bytes.Buffer
	err    error
}

func newBufferWriter() *bufferWriter { return &bufferWriter{header: http.Header{}} }

func (w *bufferWriter) Header() http.Header { return w.header }

func (w *bufferWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status != 0 || (code >= 100 && code < 200) {
		return
	}
	w.status = code
}

func (w *bufferWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len()+len(b) > maxUnaryResponse {
		w.err = status.Errorf(codes.ResourceExhausted, "the response exceeds %d bytes", maxUnaryResponse)
		return 0, w.err
	}
	return w.body.Write(b)
}

// Flush is a no-op: the response leaves as one message.
func (w *bufferWriter) Flush() {}

func (w *bufferWriter) response() (*gatewayv1.HttpResponse, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return nil, w.err
	}
	code := w.status
	if code == 0 {
		code = http.StatusOK
	}
	return &gatewayv1.HttpResponse{Head: headFromHeader(code, w.header), Body: append([]byte(nil), w.body.Bytes()...)}, nil
}

// frameBody is the body of a client-streamed request: the data frames that
// follow the head, in order.
type frameBody struct {
	recv    func() (*gatewayv1.RequestFrame, error)
	pending []byte
	err     error
}

func (b *frameBody) Read(p []byte) (int, error) {
	for len(b.pending) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		frame, err := b.recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				b.err = io.EOF
			} else {
				b.err = fmt.Errorf("grpcapi: read the request body: %w", err)
			}
			continue
		}
		data, ok := frame.GetFrame().(*gatewayv1.RequestFrame_Data)
		if !ok {
			b.err = errors.New("grpcapi: a request frame after the head carries no body data")
			continue
		}
		b.pending = data.Data
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *frameBody) Close() error { return nil }
