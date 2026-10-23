// Package modelgrpc serves elitea.llm.v1.ModelService on elitea-main's
// platform gRPC listener (#1359 U2): the agent worker's and the
// DeepWiki/Inventory engines' model calls.
//
// Each RPC is one /llm request, and it runs through the SAME pipeline the
// public /llm route runs, in process: the public handler is handed a request
// for the RPC's fixed /llm/v1 path, carrying the caller's bearer, project
// selector and execution id as the HTTP caller sent them. Authentication, the
// project selection and membership check, the execution-id verification and
// the hand-off to elitea-llm-gateway (llmproxy.Proxy over llmproxy.Gateway),
// whose budget and policy gates run there, are therefore one implementation,
// not two that could drift. Nothing crosses a socket between this server and
// that pipeline.
//
// The answer goes back unchanged: a success is a head (the Content-Type) and
// the body bytes as the pipeline wrote them, SSE framing included; a refusal
// is a gRPC status mapped from the HTTP status, with the HTTP status, body
// and Retry-After in the `elitea-model-refusal-bin` trailer (llmv1.ModelRefusal),
// so a caller classifies it exactly as it classified the HTTP answer.
//
// Who may call is decided before any of this: the platform listener's
// grpcauth.Policy admits only the identities configured for this service.
package modelgrpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	llmv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/llm/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

// The /llm routes each RPC serves. They are fixed: no caller chooses a path.
const (
	chatCompletionsPath = "/llm/v1/chat/completions"
	messagesPath        = "/llm/v1/messages"
	embeddingsPath      = "/llm/v1/embeddings"
	imageGeneratePath   = "/llm/v1/images/generations"
	imageEditPath       = "/llm/v1/images/edits"
)

// RefusalTrailer is the trailer that carries an llmv1.ModelRefusal.
const RefusalTrailer = "elitea-model-refusal-bin"

// maxRefusalBodyBytes caps the refusal body carried in the trailer.
const maxRefusalBodyBytes = 64 << 10

// maxUnaryBodyBytes caps a unary answer at the gateway's own unary cap
// (64 MiB), so what the gateway answers passes this hop unchanged. One
// message of it fits runtimegrpc.PlatformMaxSendMessageBytes with room for
// the head.
const maxUnaryBodyBytes = 64 << 20

// A unary answer and its head fit one message on the platform listener.
var _ = [runtimegrpc.PlatformMaxSendMessageBytes - maxUnaryBodyBytes - 64<<10]struct{}{}

// Request header names the pipeline reads (the /llm caller contract).
const (
	headerProjectSelector  = "X-Project-Id"
	headerExecutionID      = "X-Elitea-Execution-Id"
	headerAnthropicVersion = "Anthropic-Version"
	headerAnthropicBeta    = "Anthropic-Beta"
)

// maxSelectorBytes bounds the project selector and the execution id; the
// pipeline applies its own shape rules to both.
const maxSelectorBytes = 256

// Server implements llmv1.ModelServiceServer over an http.Handler that serves
// /llm (elitea-main's public handler).
type Server struct {
	llmv1.UnimplementedModelServiceServer
	handler http.Handler
	logger  *slog.Logger
}

// NewServer builds the service over handler, the public HTTP handler whose
// /llm route is the pipeline every call runs through.
func NewServer(handler http.Handler, logger *slog.Logger) (*Server, error) {
	if handler == nil {
		return nil, errors.New("modelgrpc: the /llm handler is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{handler: handler, logger: logger}, nil
}

// PlatformService declares the service for the platform listener, admitting
// clients only.
func (s *Server) PlatformService(clients grpcauth.Allowlist) runtimegrpc.PlatformService {
	return runtimegrpc.PlatformService{
		Name:     llmv1.ModelService_ServiceDesc.ServiceName,
		Clients:  clients,
		Register: func(registrar grpc.ServiceRegistrar) { llmv1.RegisterModelServiceServer(registrar, s) },
	}
}

// ChatCompletions serves POST /llm/v1/chat/completions.
func (s *Server) ChatCompletions(request *llmv1.ChatCompletionsRequest, stream grpc.ServerStreamingServer[llmv1.ChatCompletionsResponse]) error {
	return s.serveStream(stream.Context(), stream, chatCompletionsPath, request.GetCall(), request.GetBody(), nil,
		func(head *llmv1.ResponseHead) error {
			return stream.Send(&llmv1.ChatCompletionsResponse{Frame: &llmv1.ChatCompletionsResponse_Head{Head: head}})
		},
		func(data []byte) error {
			return stream.Send(&llmv1.ChatCompletionsResponse{Frame: &llmv1.ChatCompletionsResponse_Data{Data: data}})
		})
}

// Messages serves POST /llm/v1/messages.
func (s *Server) Messages(request *llmv1.MessagesRequest, stream grpc.ServerStreamingServer[llmv1.MessagesResponse]) error {
	extra := http.Header{}
	if version := request.GetAnthropicVersion(); version != "" {
		if !HeaderSafe(version) {
			return status.Error(codes.InvalidArgument, "anthropic_version is not a header value")
		}
		extra.Set(headerAnthropicVersion, version)
	}
	for _, beta := range request.GetAnthropicBeta() {
		if !HeaderSafe(beta) {
			return status.Error(codes.InvalidArgument, "anthropic_beta is not a header value")
		}
		extra.Add(headerAnthropicBeta, beta)
	}
	return s.serveStream(stream.Context(), stream, messagesPath, request.GetCall(), request.GetBody(), extra,
		func(head *llmv1.ResponseHead) error {
			return stream.Send(&llmv1.MessagesResponse{Frame: &llmv1.MessagesResponse_Head{Head: head}})
		},
		func(data []byte) error {
			return stream.Send(&llmv1.MessagesResponse{Frame: &llmv1.MessagesResponse_Data{Data: data}})
		})
}

// Embeddings serves POST /llm/v1/embeddings.
func (s *Server) Embeddings(ctx context.Context, request *llmv1.EmbeddingsRequest) (*llmv1.EmbeddingsResponse, error) {
	head, body, err := s.serveUnary(ctx, embeddingsPath, request.GetCall(), "application/json", bytes.NewReader(request.GetBody()))
	if err != nil {
		return nil, err
	}
	return &llmv1.EmbeddingsResponse{Head: head, Body: body}, nil
}

// ImageGenerate serves POST /llm/v1/images/generations.
func (s *Server) ImageGenerate(ctx context.Context, request *llmv1.ImageGenerateRequest) (*llmv1.ImageGenerateResponse, error) {
	head, body, err := s.serveUnary(ctx, imageGeneratePath, request.GetCall(), "application/json", bytes.NewReader(request.GetBody()))
	if err != nil {
		return nil, err
	}
	return &llmv1.ImageGenerateResponse{Head: head, Body: body}, nil
}

// ImageEdit serves POST /llm/v1/images/edits. The multipart body streams in
// from the client as the pipeline reads it.
func (s *Server) ImageEdit(stream grpc.ClientStreamingServer[llmv1.ImageEditRequest, llmv1.ImageEditResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "an ImageEdit call starts with its head")
		}
		return err
	}
	open := first.GetHead()
	if open == nil {
		return status.Error(codes.InvalidArgument, "an ImageEdit call starts with its head")
	}
	contentType := open.GetContentType()
	if !strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data") || !HeaderSafe(contentType) {
		return status.Error(codes.InvalidArgument, "an ImageEdit body is multipart/form-data")
	}
	// The body is pulled from the stream as the pipeline reads it, on the
	// pipeline's own goroutine, so stream.Recv never runs after this handler
	// returns, which grpc-go forbids. A pipeline that refuses before reading
	// (a bad bearer, a budget) never calls Recv at all.
	body := &editBody{stream: stream}
	head, answer, err := s.serveUnary(stream.Context(), imageEditPath, open.GetCall(), contentType, body)
	if violation := body.close(); violation != nil {
		return violation
	}
	if err != nil {
		return err
	}
	return stream.SendAndClose(&llmv1.ImageEditResponse{Head: head, Body: answer})
}

// editBody is an ImageEdit's multipart body: the data frames after the head,
// read from the stream on demand.
type editBody struct {
	stream grpc.ClientStreamingServer[llmv1.ImageEditRequest, llmv1.ImageEditResponse]

	mu        sync.Mutex
	pending   []byte
	err       error
	violation error
	closed    bool
}

func (b *editBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.pending) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		if b.closed {
			return 0, io.ErrClosedPipe
		}
		frame, err := b.stream.Recv()
		if err != nil {
			b.err = err
			continue
		}
		data, ok := frame.GetFrame().(*llmv1.ImageEditRequest_Data)
		if !ok {
			b.violation = status.Error(codes.InvalidArgument, "an ImageEdit call has one head")
			b.err = b.violation
			continue
		}
		b.pending = data.Data
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

// close ends the body: no Read after it reaches the stream. It returns the
// protocol violation a Read met, if any, which is the call's answer.
func (b *editBody) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return b.violation
}

// newRequest builds the /llm request one RPC makes.
func newRequest(ctx context.Context, path string, call *llmv1.ModelCall, contentType, accept string, body io.Reader, extra http.Header) (*http.Request, error) {
	projectID := call.GetProjectId()
	executionID := call.GetExecutionId()
	if len(projectID) > maxSelectorBytes || !HeaderSafe(projectID) {
		return nil, status.Error(codes.InvalidArgument, "project_id is not a project selector")
	}
	if len(executionID) > maxSelectorBytes || !HeaderSafe(executionID) {
		return nil, status.Error(codes.InvalidArgument, "execution_id is not an execution id")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, status.Error(codes.Internal, "the model request could not be built")
	}
	request.Host = "elitea-main"
	request.RequestURI = path
	request.Proto, request.ProtoMajor, request.ProtoMinor = "HTTP/2.0", 2, 0
	if address, ok := peer.FromContext(ctx); ok && address.Addr != nil {
		request.RemoteAddr = address.Addr.String()
	} else {
		request.RemoteAddr = net.JoinHostPort("127.0.0.1", "0")
	}
	for name, values := range extra {
		request.Header[name] = values
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", accept)
	// The bearer the HTTP caller sent, unchanged. A missing or malformed one
	// is not refused here: the pipeline answers it, as it answered the HTTP
	// caller (401), so the refusal reads the same.
	if token, err := grpcauth.BearerToken(ctx); err == nil {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if projectID != "" {
		request.Header.Set(headerProjectSelector, projectID)
	}
	if executionID != "" {
		request.Header.Set(headerExecutionID, executionID)
	}
	return request, nil
}

// HeaderSafe reports whether value can stand as one header value: printable
// ASCII and spaces/tabs, nothing a header could be split on. It is the one
// copy; internal/transport/platformcall re-exports it.
func HeaderSafe(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < 0x20 && c != '\t') || c >= 0x7f {
			return false
		}
	}
	return true
}

// serveStream runs one streaming RPC.
func (s *Server) serveStream(
	ctx context.Context,
	stream grpc.ServerStream,
	path string,
	call *llmv1.ModelCall,
	body []byte,
	extra http.Header,
	sendHead func(*llmv1.ResponseHead) error,
	sendData func([]byte) error,
) error {
	request, err := newRequest(ctx, path, call, "application/json", "text/event-stream", bytes.NewReader(body), extra)
	if err != nil {
		return err
	}
	writer := &streamWriter{header: http.Header{}, sendHead: sendHead, sendData: sendData}
	if aborted := s.serve(writer, request); aborted {
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		return status.Error(codes.Unavailable, "the model stream broke")
	}
	if writer.sendErr != nil {
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		return writer.sendErr
	}
	if writer.refusal != nil {
		stream.SetTrailer(writer.refusal.trailer())
		return writer.refusal.status()
	}
	if !writer.headSent {
		// The pipeline wrote nothing: a success with an empty body.
		return sendHead(&llmv1.ResponseHead{ContentType: writer.header.Get("Content-Type")})
	}
	return nil
}

// serveUnary runs one RPC whose answer is one message.
func (s *Server) serveUnary(ctx context.Context, path string, call *llmv1.ModelCall, contentType string, body io.Reader) (*llmv1.ResponseHead, []byte, error) {
	request, err := newRequest(ctx, path, call, contentType, "application/json", body, nil)
	if err != nil {
		return nil, nil, err
	}
	writer := &bufferWriter{header: http.Header{}}
	if aborted := s.serve(writer, request); aborted {
		if ctx.Err() != nil {
			return nil, nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, nil, status.Error(codes.Unavailable, "the model answer broke")
	}
	if ctx.Err() != nil {
		return nil, nil, status.FromContextError(ctx.Err()).Err()
	}
	statusCode := writer.statusCode()
	if statusCode < 200 || statusCode > 299 {
		refused := newRefusal(statusCode, writer.header, writer.body)
		_ = grpc.SetTrailer(ctx, refused.trailer())
		return nil, nil, refused.status()
	}
	if writer.tooLarge {
		return nil, nil, status.Errorf(codes.ResourceExhausted, "the model answer exceeds %d bytes", maxUnaryBodyBytes)
	}
	return &llmv1.ResponseHead{ContentType: writer.header.Get("Content-Type")}, writer.body, nil
}

// serve runs the handler and reports whether it aborted the response
// (http.ErrAbortHandler: the gateway stream broke after the head).
func (s *Server) serve(writer http.ResponseWriter, request *http.Request) (aborted bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered != http.ErrAbortHandler { //nolint:errorlint // the sentinel is panicked by value
				s.logger.Error("modelgrpc: the /llm pipeline panicked", "path", request.URL.Path, "panic", fmt.Sprint(recovered))
			}
			aborted = true
		}
	}()
	s.handler.ServeHTTP(writer, request)
	return false
}

// refusal is a non-2xx answer of the pipeline.
type refusal struct {
	httpStatus  int
	contentType string
	retryAfter  string
	body        []byte
}

func newRefusal(httpStatus int, header http.Header, body []byte) *refusal {
	if len(body) > maxRefusalBodyBytes {
		body = body[:maxRefusalBodyBytes]
	}
	return &refusal{
		httpStatus:  httpStatus,
		contentType: header.Get("Content-Type"),
		retryAfter:  header.Get("Retry-After"),
		body:        append([]byte(nil), body...),
	}
}

func (r *refusal) trailer() metadata.MD {
	encoded, err := proto.Marshal(&llmv1.ModelRefusal{
		HttpStatus:  int32(r.httpStatus), //nolint:gosec // an HTTP status fits
		ContentType: r.contentType,
		Body:        r.body,
		RetryAfter:  r.retryAfter,
	})
	if err != nil {
		return metadata.MD{}
	}
	return metadata.Pairs(RefusalTrailer, string(encoded))
}

func (r *refusal) status() error {
	return status.Errorf(CodeForHTTPStatus(r.httpStatus), "the model call was refused (HTTP %d)", r.httpStatus)
}

// CodeForHTTPStatus maps a refusal's HTTP status to its gRPC code, as
// llmv1.ModelRefusal documents.
func CodeForHTTPStatus(httpStatus int) codes.Code {
	switch httpStatus {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusPaymentRequired, http.StatusTooManyRequests:
		return codes.ResourceExhausted
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return codes.DeadlineExceeded
	case http.StatusConflict:
		return codes.Aborted
	case http.StatusNotImplemented:
		return codes.Unimplemented
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return codes.Unavailable
	}
	if httpStatus >= 500 {
		return codes.Internal
	}
	return codes.FailedPrecondition
}

// streamWriter is the http.ResponseWriter of a streaming RPC. A 2xx head is
// sent at once and every Write is one data frame, so a stream is never
// re-buffered on this hop; a refusal's body is kept for the trailer.
type streamWriter struct {
	header   http.Header
	sendHead func(*llmv1.ResponseHead) error
	sendData func([]byte) error

	wroteHeader bool
	headSent    bool
	refusal     *refusal
	sendErr     error
}

func (w *streamWriter) Header() http.Header { return w.header }

func (w *streamWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	if code >= 100 && code <= 199 {
		return
	}
	w.wroteHeader = true
	if code < 200 || code > 299 {
		w.refusal = newRefusal(code, w.header, nil)
		return
	}
	w.headSent = true
	if err := w.sendHead(&llmv1.ResponseHead{ContentType: w.header.Get("Content-Type")}); err != nil {
		w.sendErr = err
	}
}

func (w *streamWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.sendErr != nil {
		return 0, w.sendErr
	}
	if w.refusal != nil {
		room := maxRefusalBodyBytes - len(w.refusal.body)
		if room > 0 {
			w.refusal.body = append(w.refusal.body, data[:min(room, len(data))]...)
		}
		return len(data), nil
	}
	if len(data) == 0 {
		return 0, nil
	}
	// The handler may reuse its buffer once Write returns; the frame owns a
	// copy.
	if err := w.sendData(append([]byte(nil), data...)); err != nil {
		w.sendErr = err
		return 0, err
	}
	return len(data), nil
}

// Flush is a no-op: every Write is already sent.
func (w *streamWriter) Flush() {}

// bufferWriter is the http.ResponseWriter of a unary RPC.
type bufferWriter struct {
	header   http.Header
	code     int
	body     []byte
	tooLarge bool
}

func (w *bufferWriter) Header() http.Header { return w.header }

func (w *bufferWriter) WriteHeader(code int) {
	if w.code != 0 || (code >= 100 && code <= 199) {
		return
	}
	w.code = code
}

func (w *bufferWriter) Write(data []byte) (int, error) {
	if w.code == 0 {
		w.WriteHeader(http.StatusOK)
	}
	limit := maxUnaryBodyBytes
	if w.code < 200 || w.code > 299 {
		limit = maxRefusalBodyBytes
	}
	room := limit - len(w.body)
	if len(data) > room {
		if w.code >= 200 && w.code <= 299 {
			w.tooLarge = true
			return 0, errors.New("modelgrpc: the answer exceeds the unary limit")
		}
		w.body = append(w.body, data[:max(room, 0)]...)
		return len(data), nil
	}
	w.body = append(w.body, data...)
	return len(data), nil
}

func (w *bufferWriter) Flush() {}

func (w *bufferWriter) statusCode() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}
