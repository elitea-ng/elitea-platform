// Package internaltoolgrpc serves elitea.runtime.v1.InternalToolService on
// elitea-main's platform gRPC listener (#1359 U5):
//
//   - ListTools and CallTool are the agent worker's internal MCP builders,
//     which its MCP client used to reach as streamable HTTP at
//     /app/{project}/mcp/{scope} on the platform listener;
//   - InvokeToolkit is the Inventory engine's `investigate` source call, which
//     was POST /api/v2/elitea_core/test_tool/prompt_lib/{project}/{toolkit}.
//
// Each RPC is that request, run in process through the public handler
// (internal/transport/platformcall): the same authentication, project-access
// check, MCP gate and handler an external MCP client or the toolkit route's
// HTTP caller reaches. An HTTP refusal is a gRPC status mapped from the HTTP
// status with the status and body in the `elitea-internal-tool-refusal-bin`
// trailer (runtimev1.InternalToolRefusalV1); a JSON-RPC error is in band.
// Who may call is decided first, by the platform listener's grpcauth.Policy.
package internaltoolgrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/platformcall"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

// RefusalTrailer is the trailer that carries a runtimev1.InternalToolRefusalV1.
const RefusalTrailer = "elitea-internal-tool-refusal-bin"

// Progress stages.
const (
	StageAdmitted = "admitted"
	StageRunning  = "running"
)

const (
	// maxAnswerBytes caps one JSON answer or SSE event. A tool result
	// travels in one message on the listener, under its 16 MiB cap.
	maxAnswerBytes = runtimegrpc.PlatformMaxMessageBytes - 64<<10
	// maxRefusalBytes caps a refusal body kept for the trailer.
	maxRefusalBytes = 64 << 10
	// maxRequestBytes is the MCP endpoint's own request cap.
	maxRequestBytes = 1 << 20
	maxNameBytes    = 256
)

// keepaliveInterval is how often a waiting call reports it is still
// running. A variable so tests can shorten it.
var keepaliveInterval = 15 * time.Second

// Server implements runtimev1.InternalToolServiceServer over the public
// HTTP handler.
type Server struct {
	runtimev1.UnimplementedInternalToolServiceServer
	handler http.Handler
	logger  *slog.Logger
}

// NewServer builds the service over handler, elitea-main's public handler.
func NewServer(handler http.Handler, logger *slog.Logger) (*Server, error) {
	if handler == nil {
		return nil, errors.New("internaltoolgrpc: the public handler is required")
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
		Name:     runtimev1.InternalToolService_ServiceDesc.ServiceName,
		Clients:  clients,
		Register: func(registrar grpc.ServiceRegistrar) { runtimev1.RegisterInternalToolServiceServer(registrar, s) },
	}
}

// rpcResponse is the JSON-RPC response the MCP endpoint answers.
type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int32  `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ListTools is tools/list.
func (s *Server) ListTools(ctx context.Context, request *runtimev1.InternalToolListRequestV1) (*runtimev1.InternalToolListResponseV1, error) {
	path, err := mcpPath(request.GetScope())
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	httpRequest, err := s.request(ctx, path, body, "application/json")
	if err != nil {
		return nil, err
	}
	writer := platformcall.NewBuffer(maxAnswerBytes)
	if err := s.serve(ctx, writer, httpRequest); err != nil {
		return nil, err
	}
	if !platformcall.Success(writer.Status()) {
		return nil, refuse(ctx, writer.Status(), writer.Body())
	}
	if writer.TooLarge() {
		return nil, status.Errorf(codes.ResourceExhausted, "the tool listing exceeds %d bytes", maxAnswerBytes)
	}
	answer, err := decodeRPC(writer.Body())
	if err != nil {
		return nil, err
	}
	if answer.Error != nil {
		return &runtimev1.InternalToolListResponseV1{Outcome: &runtimev1.InternalToolListResponseV1_Error{Error: rpcError(answer)}}, nil
	}
	return &runtimev1.InternalToolListResponseV1{Outcome: &runtimev1.InternalToolListResponseV1_ResultJson{ResultJson: answer.Result}}, nil
}

// CallTool is tools/call. The endpoint is asked for its event stream: a call
// that admits a run primes the stream (a progress frame here) and answers when
// the run settles; a quick call answers JSON at once.
func (s *Server) CallTool(request *runtimev1.InternalToolCallRequestV1, stream grpc.ServerStreamingServer[runtimev1.InternalToolCallFrameV1]) error {
	ctx := stream.Context()
	path, err := mcpPath(request.GetScope())
	if err != nil {
		return err
	}
	name := request.GetName()
	if name == "" || len(name) > maxNameBytes || !printable(name) {
		return status.Error(codes.InvalidArgument, "name is not a tool name")
	}
	arguments := json.RawMessage(request.GetArgumentsJson())
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &object); err != nil || object == nil {
		return status.Error(codes.InvalidArgument, "arguments_json is not a JSON object")
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": arguments}})
	if err != nil || len(body) > maxRequestBytes {
		return status.Error(codes.InvalidArgument, "the tool call is too large")
	}
	httpRequest, err := s.request(ctx, path, body, "application/json, text/event-stream")
	if err != nil {
		return err
	}
	sender := newSender(func(stage string) error {
		return stream.Send(&runtimev1.InternalToolCallFrameV1{Frame: &runtimev1.InternalToolCallFrameV1_Progress{Progress: &runtimev1.InternalToolProgressV1{Stage: stage}}})
	})
	writer := &eventWriter{header: http.Header{}, admitted: sender.admitted}
	err = s.serveWithKeepalive(ctx, sender, writer, httpRequest)
	if err != nil {
		return err
	}
	if sendErr := sender.err(); sendErr != nil {
		return sendErr
	}
	if !platformcall.Success(writer.status()) {
		return refuse(ctx, writer.status(), writer.body)
	}
	if writer.tooLarge {
		return status.Errorf(codes.ResourceExhausted, "the tool result exceeds %d bytes", maxAnswerBytes)
	}
	raw := writer.message
	if raw == nil {
		if writer.streamed {
			// The stream ended without its answer: the run's observation was
			// cut short (the endpoint writes nothing then).
			return status.Error(codes.Unavailable, "the tool call ended without a result")
		}
		raw = writer.body
	}
	answer, err := decodeRPC(raw)
	if err != nil {
		return err
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if answer.Error != nil {
		return stream.Send(&runtimev1.InternalToolCallFrameV1{Frame: &runtimev1.InternalToolCallFrameV1_Error{Error: rpcError(answer)}})
	}
	return stream.Send(&runtimev1.InternalToolCallFrameV1{Frame: &runtimev1.InternalToolCallFrameV1_ResultJson{ResultJson: answer.Result}})
}

// InvokeToolkit is POST /api/v2/elitea_core/test_tool/prompt_lib/{project}/{toolkit}
// with the body shape the investigate grant admits, and nothing more.
func (s *Server) InvokeToolkit(request *runtimev1.InternalToolInvokeRequestV1, stream grpc.ServerStreamingServer[runtimev1.InternalToolInvokeFrameV1]) error {
	ctx := stream.Context()
	if request.GetProjectId() <= 0 {
		return status.Error(codes.InvalidArgument, "project_id must be positive")
	}
	toolkit := request.GetToolkitId()
	if id, err := strconv.ParseInt(toolkit, 10, 64); err != nil || id <= 0 || strconv.FormatInt(id, 10) != toolkit {
		return status.Error(codes.InvalidArgument, "toolkit_id is not a toolkit id")
	}
	if name := request.GetToolName(); name == "" || len(name) > maxNameBytes || !printable(name) {
		return status.Error(codes.InvalidArgument, "tool_name is not a tool name")
	}
	if len(request.GetRequestId()) > maxNameBytes || !printable(request.GetRequestId()) {
		return status.Error(codes.InvalidArgument, "request_id is not a label")
	}
	params := json.RawMessage(request.GetToolParamsJson())
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(params, &object); err != nil || object == nil {
		return status.Error(codes.InvalidArgument, "tool_params_json is not a JSON object")
	}
	body, err := json.Marshal(map[string]any{
		"request_id":     request.GetRequestId(),
		"tool_name":      request.GetToolName(),
		"tool_params":    params,
		"toolkit_config": map[string]any{"toolkit_id": toolkit},
	})
	if err != nil || len(body) > maxRequestBytes {
		return status.Error(codes.InvalidArgument, "the toolkit call is too large")
	}
	path := "/api/v2/elitea_core/test_tool/prompt_lib/" + strconv.FormatInt(request.GetProjectId(), 10) + "/" + toolkit
	httpRequest, err := s.request(ctx, path, body, "application/json")
	if err != nil {
		return err
	}
	sender := newSender(func(stage string) error {
		return stream.Send(&runtimev1.InternalToolInvokeFrameV1{Frame: &runtimev1.InternalToolInvokeFrameV1_Progress{Progress: &runtimev1.InternalToolProgressV1{Stage: stage}}})
	})
	// The route answers once, when the tool is done: the call is admitted as
	// soon as it is handed over, and keeps reporting while it waits.
	if err := sender.admitted(); err != nil {
		return err
	}
	writer := platformcall.NewBuffer(maxAnswerBytes)
	if err := s.serveWithKeepalive(ctx, sender, writer, httpRequest); err != nil {
		return err
	}
	if sendErr := sender.err(); sendErr != nil {
		return sendErr
	}
	if !platformcall.Success(writer.Status()) {
		return refuse(ctx, writer.Status(), writer.Body())
	}
	if writer.TooLarge() {
		return status.Errorf(codes.ResourceExhausted, "the toolkit answer exceeds %d bytes", maxAnswerBytes)
	}
	if !json.Valid(writer.Body()) {
		return status.Error(codes.Internal, "the toolkit answer is not JSON")
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	return stream.Send(&runtimev1.InternalToolInvokeFrameV1{Frame: &runtimev1.InternalToolInvokeFrameV1_ResultJson{ResultJson: writer.Body()}})
}

// request builds one in-process POST.
func (s *Server) request(ctx context.Context, path string, body []byte, accept string) (*http.Request, error) {
	request, err := platformcall.NewRequest(ctx, http.MethodPost, path, nil, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.ContentLength = int64(len(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", accept)
	return request, nil
}

func (s *Server) serve(ctx context.Context, writer http.ResponseWriter, request *http.Request) error {
	if aborted := platformcall.Serve(s.handler, writer, request, s.logger); aborted {
		if err := platformcall.ContextStatus(ctx); err != nil {
			return err
		}
		return status.Error(codes.Unavailable, "the tool call broke")
	}
	return platformcall.ContextStatus(ctx)
}

// serveWithKeepalive serves the request while, once the call is admitted, a
// "running" progress frame goes out every keepaliveInterval.
func (s *Server) serveWithKeepalive(ctx context.Context, sender *sender, writer http.ResponseWriter, request *http.Request) error {
	done := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				sender.running()
			}
		}
	}()
	err := s.serve(ctx, writer, request)
	close(done)
	wait.Wait()
	return err
}

// sender serialises a stream's progress frames with its final frame.
type sender struct {
	mu       sync.Mutex
	send     func(stage string) error
	started  bool
	firstErr error
}

func newSender(send func(string) error) *sender { return &sender{send: send} }

func (s *sender) admitted() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.firstErr != nil {
		return s.firstErr
	}
	s.started = true
	s.firstErr = s.send(StageAdmitted)
	return s.firstErr
}

func (s *sender) running() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.firstErr != nil {
		return
	}
	s.firstErr = s.send(StageRunning)
}

func (s *sender) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstErr
}

// eventWriter reads the MCP endpoint's answer: JSON, or an event stream
// whose priming event admits the call and whose `message` event is the
// answer.
type eventWriter struct {
	header   http.Header
	admitted func() error

	code     int
	streamed bool
	pending  []byte
	body     []byte
	message  []byte
	tooLarge bool
}

func (w *eventWriter) Header() http.Header { return w.header }

func (w *eventWriter) WriteHeader(code int) {
	if w.code != 0 || (code >= 100 && code <= 199) {
		return
	}
	w.code = code
	w.streamed = platformcall.Success(code) && strings.HasPrefix(strings.ToLower(w.header.Get("Content-Type")), "text/event-stream")
}

func (w *eventWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}

func (w *eventWriter) Write(data []byte) (int, error) {
	if w.code == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if !w.streamed {
		limit := maxAnswerBytes
		if !platformcall.Success(w.code) {
			limit = maxRefusalBytes
		}
		room := limit - len(w.body)
		if len(data) > room {
			if platformcall.Success(w.code) {
				w.tooLarge = true
				return 0, errors.New("internaltoolgrpc: the answer is too large")
			}
			w.body = append(w.body, data[:max(room, 0)]...)
			return len(data), nil
		}
		w.body = append(w.body, data...)
		return len(data), nil
	}
	w.pending = append(w.pending, data...)
	if len(w.pending) > maxAnswerBytes {
		w.tooLarge = true
		return 0, errors.New("internaltoolgrpc: the event is too large")
	}
	for {
		end := bytes.Index(w.pending, []byte("\n\n"))
		if end < 0 {
			return len(data), nil
		}
		event := w.pending[:end]
		w.pending = w.pending[end+2:]
		if err := w.event(event); err != nil {
			return 0, err
		}
	}
}

// event handles one server-sent event.
func (w *eventWriter) event(event []byte) error {
	var kind string
	var data [][]byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			kind = string(value)
		case "data":
			data = append(data, value)
		}
	}
	payload := bytes.Join(data, []byte("\n"))
	if kind == "message" && len(payload) > 0 {
		w.message = append([]byte(nil), payload...)
		return nil
	}
	// The priming event (a resume cursor and no data): the run is admitted
	// and being observed.
	return w.admitted()
}

// Flush is a no-op: events are handled as they are written.
func (w *eventWriter) Flush() {}

func decodeRPC(raw []byte) (rpcResponse, error) {
	var answer rpcResponse
	if err := json.Unmarshal(raw, &answer); err != nil {
		return rpcResponse{}, status.Error(codes.Internal, "the MCP answer is not JSON-RPC")
	}
	if answer.Error == nil && len(answer.Result) == 0 {
		return rpcResponse{}, status.Error(codes.Internal, "the MCP answer carries neither a result nor an error")
	}
	return answer, nil
}

func rpcError(answer rpcResponse) *runtimev1.InternalToolRpcErrorV1 {
	return &runtimev1.InternalToolRpcErrorV1{Code: answer.Error.Code, Message: answer.Error.Message}
}

func refuse(ctx context.Context, httpStatus int, body []byte) error {
	if len(body) > maxRefusalBytes {
		body = body[:maxRefusalBytes]
	}
	refusal := &runtimev1.InternalToolRefusalV1{HttpStatus: int32(httpStatus), Body: body} //nolint:gosec // an HTTP status fits
	return platformcall.Refuse(ctx, RefusalTrailer, httpStatus, refusal, "internal tool call")
}

// mcpPath is /app/{project}/mcp[/{scope}]. The scope stays a relative path of
// non-empty segments; the endpoint applies its own category rules.
func mcpPath(scope *runtimev1.InternalToolScopeV1) (string, error) {
	if scope.GetProjectId() <= 0 {
		return "", status.Error(codes.InvalidArgument, "scope.project_id must be positive")
	}
	path := "/app/" + strconv.FormatInt(scope.GetProjectId(), 10) + "/mcp"
	tail := scope.GetScope()
	if tail == "" {
		return path, nil
	}
	if len(tail) > maxNameBytes || !printable(tail) || strings.ContainsAny(tail, "\\?#%") {
		return "", status.Error(codes.InvalidArgument, "scope.scope is not an MCP scope")
	}
	for _, segment := range strings.Split(tail, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", status.Error(codes.InvalidArgument, "scope.scope is not an MCP scope")
		}
	}
	return path + "/" + tail, nil
}

func printable(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
