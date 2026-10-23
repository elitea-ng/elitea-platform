package modelgrpc_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	llmv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/llm/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy/gatewaytest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth/grpcauthtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/modelgrpc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

const (
	workerName = "elitea-worker"
	goodToken  = "claim-token"
	secret     = "identity-secret"
)

// sseFrames is what the fake gateway streams for a chat completion: several
// frames, each flushed on its own.
var sseFrames = []string{
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n",
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\n",
	"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n",
	"data: [DONE]\n\n",
}

// bigAnswerBytes is a unary answer over 16 MiB.
const bigAnswerBytes = 20 << 20

const budgetBody = `{"error":{"message":"member budget exhausted","type":"budget_exceeded","code":"budget_exceeded","scope":"member"}}`

// fakeGateway is elitea-llm-gateway as the test sees it: an http.Handler
// served over the real GatewayService hop (gatewaytest).
type fakeGateway struct {
	mu        sync.Mutex
	seen      []*http.Request
	bodies    []string
	cancelled chan struct{}
	started   chan struct{}
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{cancelled: make(chan struct{}, 1), started: make(chan struct{}, 1)}
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	g.seen = append(g.seen, r.Clone(context.Background()))
	g.bodies = append(g.bodies, string(body))
	g.mu.Unlock()
	switch {
	case strings.Contains(string(body), `"budget"`):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, budgetBody)
	case strings.Contains(string(body), `"big"`):
		// A b64 image answer past the 16 MiB a platform message used to cap.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"`+strings.Repeat("A", bigAnswerBytes)+`"}]}`)
	case strings.Contains(string(body), `"hang"`):
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseFrames[0])
		w.(http.Flusher).Flush()
		g.started <- struct{}{}
		<-r.Context().Done()
		g.cancelled <- struct{}{}
	case r.URL.Path == "/llm/v1/chat/completions" || r.URL.Path == "/llm/v1/messages":
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range sseFrames {
			_, _ = io.WriteString(w, frame)
			w.(http.Flusher).Flush()
		}
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"path":%q,"ct":%q,"bytes":%d}`, r.URL.Path, r.Header.Get("Content-Type"), len(body))
	}
}

func (g *fakeGateway) last() (*http.Request, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.seen[len(g.seen)-1], g.bodies[len(g.bodies)-1]
}

// pipeline is a minimal stand-in for elitea-main's public handler: a bearer
// check in front of the real /llm proxy over the real gateway hop.
func pipeline(t *testing.T, gateway http.Handler) http.Handler {
	t.Helper()
	proxy, err := llmproxy.New(llmproxy.Config{Gateway: gatewaytest.Client(t, gateway, secret)})
	if err != nil {
		t.Fatal(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid token"}}`)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/llm/") {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func start(t *testing.T, pki *grpcauthtest.PKI, handler http.Handler) string {
	t.Helper()
	service, err := modelgrpc.NewServer(handler, nil)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := grpcauth.NewAllowlist([]string{"dns:" + workerName})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := runtimegrpc.NewPlatformServer(runtimegrpc.PlatformServerConfig{
		Address: listener.Addr().String(),
		TLS:     pki.ServerTLS(t),
		Policy:  runtimegrpc.DefaultPlatformGRPCPolicy(),
	}, service.PlatformService(clients))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return listener.Addr().String()
}

func client(t *testing.T, address string, tlsConfig *tls.Config) llmv1.ModelServiceClient {
	t.Helper()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return llmv1.NewModelServiceClient(connection)
}

func bearer(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// collect reads a chat stream: its head's content type and the body bytes.
func collect(stream grpc.ServerStreamingClient[llmv1.ChatCompletionsResponse]) (string, []byte, error) {
	var contentType string
	var body bytes.Buffer
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return contentType, body.Bytes(), nil
		}
		if err != nil {
			return contentType, body.Bytes(), err
		}
		switch f := frame.GetFrame().(type) {
		case *llmv1.ChatCompletionsResponse_Head:
			contentType = f.Head.GetContentType()
		case *llmv1.ChatCompletionsResponse_Data:
			body.Write(f.Data)
		}
	}
}

const chatBody = `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func TestAuthorizedWorkerStreamsTheHTTPBytes(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	gateway := newFakeGateway()
	handler := pipeline(t, gateway)
	address := start(t, pki, handler)
	models := client(t, address, pki.ClientTLS(t, workerName))

	stream, err := models.ChatCompletions(bearer(context.Background(), goodToken), &llmv1.ChatCompletionsRequest{
		Call: &llmv1.ModelCall{ProjectId: "7", ExecutionId: "0123456789abcdef0123456789abcdef"},
		Body: []byte(chatBody),
	})
	if err != nil {
		t.Fatal(err)
	}
	contentType, overGRPC, err := collect(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if contentType != "text/event-stream" {
		t.Errorf("content type %q", contentType)
	}
	seen, body := gateway.last()
	if body != chatBody {
		t.Errorf("the gateway got body %q", body)
	}
	if seen.URL.Path != "/llm/v1/chat/completions" {
		t.Errorf("the gateway got path %q", seen.URL.Path)
	}

	// The same pipeline over HTTP answers the same bytes.
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	request, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/llm/v1/chat/completions", strings.NewReader(chatBody))
	request.Header.Set("Authorization", "Bearer "+goodToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	overHTTP, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !bytes.Equal(overGRPC, overHTTP) {
		t.Fatalf("gRPC bytes differ from HTTP bytes:\n%q\n%q", overGRPC, overHTTP)
	}
	if want := strings.Join(sseFrames, ""); string(overGRPC) != want {
		t.Fatalf("stream = %q, want %q", overGRPC, want)
	}
}

func TestUnaryAndClientStreamRoutes(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	gateway := newFakeGateway()
	address := start(t, pki, pipeline(t, gateway))
	models := client(t, address, pki.ClientTLS(t, workerName))
	ctx := bearer(context.Background(), goodToken)

	embedded, err := models.Embeddings(ctx, &llmv1.EmbeddingsRequest{Body: []byte(`{"input":["a"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(embedded.GetBody()); !strings.Contains(got, `"/llm/v1/embeddings"`) {
		t.Errorf("embeddings answer %s", got)
	}
	generated, err := models.ImageGenerate(ctx, &llmv1.ImageGenerateRequest{Body: []byte(`{"prompt":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(generated.GetBody()); !strings.Contains(got, `"/llm/v1/images/generations"`) {
		t.Errorf("images answer %s", got)
	}

	edit, err := models.ImageEdit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	contentType := "multipart/form-data; boundary=xyz"
	if err := edit.Send(&llmv1.ImageEditRequest{Frame: &llmv1.ImageEditRequest_Head{Head: &llmv1.ImageEditHead{ContentType: contentType}}}); err != nil {
		t.Fatal(err)
	}
	multipart := "--xyz\r\nContent-Disposition: form-data; name=\"prompt\"\r\n\r\nedit\r\n--xyz--\r\n"
	for _, chunk := range []string{multipart[:10], multipart[10:]} {
		if err := edit.Send(&llmv1.ImageEditRequest{Frame: &llmv1.ImageEditRequest_Data{Data: []byte(chunk)}}); err != nil {
			t.Fatal(err)
		}
	}
	edited, err := edit.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"path":"/llm/v1/images/edits","ct":%q,"bytes":%d}`, contentType, len(multipart))
	if string(edited.GetBody()) != want {
		t.Errorf("image edit answer %s, want %s", edited.GetBody(), want)
	}
	if _, body := gateway.last(); body != multipart {
		t.Errorf("the gateway got multipart %q", body)
	}
}

// A unary answer past 16 MiB (a b64 image) reaches a client that reads up to
// the platform listener's send cap: the gateway's 64 MiB unary cap holds end
// to end, not the 16 MiB a request message is capped at.
func TestAUnaryAnswerOver16MiBPasses(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	address := start(t, pki, pipeline(t, newFakeGateway()))
	connection, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(credentials.NewTLS(pki.ClientTLS(t, workerName))),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(runtimegrpc.PlatformMaxSendMessageBytes)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	models := llmv1.NewModelServiceClient(connection)
	generated, err := models.ImageGenerate(bearer(context.Background(), goodToken),
		&llmv1.ImageGenerateRequest{Body: []byte(`{"prompt":"big"}`)})
	if err != nil {
		t.Fatalf("ImageGenerate over 16 MiB: %v", err)
	}
	if got := len(generated.GetBody()); got <= bigAnswerBytes {
		t.Fatalf("answer of %d bytes, want over %d", got, bigAnswerBytes)
	}
}

func TestCallersOutsideTheAllowlistAreRefused(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	gateway := newFakeGateway()
	address := start(t, pki, pipeline(t, gateway))
	request := &llmv1.EmbeddingsRequest{Body: []byte(`{}`)}
	ctx := bearer(context.Background(), goodToken)

	_, err := client(t, address, pki.ClientTLS(t, "elitea-scheduler")).Embeddings(ctx, request)
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("wrong identity: %v", err)
	}
	_, err = client(t, address, pki.AnonymousTLS()).Embeddings(ctx, request)
	if err == nil || status.Code(err) == codes.OK {
		t.Errorf("no certificate was admitted")
	}
	foreign := grpcauthtest.NewPKI(t).ClientTLS(t, workerName)
	foreign.RootCAs = pki.Roots()
	_, err = client(t, address, foreign).Embeddings(ctx, request)
	if err == nil {
		t.Errorf("a certificate from another CA was admitted")
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if len(gateway.seen) != 0 {
		t.Errorf("a refused caller reached the gateway %d times", len(gateway.seen))
	}
}

func refusalOf(t *testing.T, trailer metadata.MD) *llmv1.ModelRefusal {
	t.Helper()
	values := trailer.Get(modelgrpc.RefusalTrailer)
	if len(values) != 1 {
		t.Fatalf("refusal trailer: %v", trailer)
	}
	refusal := &llmv1.ModelRefusal{}
	if err := proto.Unmarshal([]byte(values[0]), refusal); err != nil {
		t.Fatal(err)
	}
	return refusal
}

func TestABadTokenIsTheHTTPRefusal(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	gateway := newFakeGateway()
	address := start(t, pki, pipeline(t, gateway))
	models := client(t, address, pki.ClientTLS(t, workerName))

	for name, ctx := range map[string]context.Context{
		"bad token": bearer(context.Background(), "forged"),
		"no token":  context.Background(),
	} {
		var trailer metadata.MD
		_, err := models.Embeddings(ctx, &llmv1.EmbeddingsRequest{Body: []byte(`{}`)}, grpc.Trailer(&trailer))
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("%s: %v", name, err)
		}
		if refusal := refusalOf(t, trailer); refusal.GetHttpStatus() != http.StatusUnauthorized || !strings.Contains(string(refusal.GetBody()), "invalid token") {
			t.Errorf("%s: refusal %v", name, refusal)
		}
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if len(gateway.seen) != 0 {
		t.Errorf("an unauthenticated call reached the gateway")
	}
}

func TestABudgetRefusalIsResourceExhaustedWithTheBody(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	address := start(t, pki, pipeline(t, newFakeGateway()))
	models := client(t, address, pki.ClientTLS(t, workerName))

	stream, err := models.ChatCompletions(bearer(context.Background(), goodToken), &llmv1.ChatCompletionsRequest{
		Body: []byte(`{"model":"budget"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, data, err := collect(stream)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("budget refusal: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("a refusal streamed data %q", data)
	}
	refusal := refusalOf(t, stream.Trailer())
	if refusal.GetHttpStatus() != http.StatusPaymentRequired || string(refusal.GetBody()) != budgetBody ||
		refusal.GetContentType() != "application/json" {
		t.Errorf("refusal %v", refusal)
	}
}

func TestClientCancelReachesTheGateway(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	gateway := newFakeGateway()
	address := start(t, pki, pipeline(t, gateway))
	models := client(t, address, pki.ClientTLS(t, workerName))

	ctx, cancel := context.WithCancel(bearer(context.Background(), goodToken))
	defer cancel()
	stream, err := models.ChatCompletions(ctx, &llmv1.ChatCompletionsRequest{Body: []byte(`{"model":"hang"}`)})
	if err != nil {
		t.Fatal(err)
	}
	// The head and the first frame arrive before the gateway hangs.
	for range 2 {
		if _, err := stream.Recv(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-gateway.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway never started")
	}
	cancel()
	select {
	case <-gateway.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the client's cancel did not reach the gateway")
	}
}

func TestMalformedCallsAreRefusedBeforeThePipeline(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	gateway := newFakeGateway()
	address := start(t, pki, pipeline(t, gateway))
	models := client(t, address, pki.ClientTLS(t, workerName))
	ctx := bearer(context.Background(), goodToken)

	_, err := models.Embeddings(ctx, &llmv1.EmbeddingsRequest{Call: &llmv1.ModelCall{ProjectId: "7\r\nX-Evil: 1"}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("header-splitting project id: %v", err)
	}
	edit, err := models.ImageEdit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = edit.Send(&llmv1.ImageEditRequest{Frame: &llmv1.ImageEditRequest_Head{Head: &llmv1.ImageEditHead{ContentType: "application/json"}}})
	if _, err := edit.CloseAndRecv(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("non-multipart image edit: %v", err)
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if len(gateway.seen) != 0 {
		t.Errorf("a malformed call reached the gateway")
	}
}

func TestCodeForHTTPStatus(t *testing.T) {
	for httpStatus, want := range map[int]codes.Code{
		400: codes.InvalidArgument, 401: codes.Unauthenticated, 402: codes.ResourceExhausted,
		403: codes.PermissionDenied, 404: codes.NotFound, 408: codes.DeadlineExceeded,
		409: codes.Aborted, 418: codes.FailedPrecondition, 429: codes.ResourceExhausted,
		500: codes.Internal, 502: codes.Unavailable, 503: codes.Unavailable, 504: codes.DeadlineExceeded,
	} {
		if got := modelgrpc.CodeForHTTPStatus(httpStatus); got != want {
			t.Errorf("%d -> %v, want %v", httpStatus, got, want)
		}
	}
}

// editStream is an ImageEdit server stream that records when Recv runs
// relative to the handler: grpc-go forbids Recv after the handler returned.
// After the head, Recv blocks until the test releases it, as a client that
// has not sent its body yet would.
type editStream struct {
	grpc.ServerStream
	ctx      context.Context
	frames   []*llmv1.ImageEditRequest
	release  chan struct{}
	mu       sync.Mutex
	returned bool
	inRecv   int
	late     int
	answered *llmv1.ImageEditResponse
}

func (s *editStream) Context() context.Context { return s.ctx }

func (s *editStream) Recv() (*llmv1.ImageEditRequest, error) {
	s.mu.Lock()
	if s.returned {
		s.late++
	}
	s.inRecv++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inRecv--
		s.mu.Unlock()
	}()
	if len(s.frames) == 0 {
		<-s.release
		return nil, io.EOF
	}
	frame := s.frames[0]
	s.frames = s.frames[1:]
	return frame, nil
}

func (s *editStream) SendAndClose(answer *llmv1.ImageEditResponse) error {
	s.answered = answer
	return nil
}

// handlerReturned marks the handler done and reports Recv calls still
// running at that moment.
func (s *editStream) handlerReturned() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.returned = true
	return s.inRecv
}

func multipartHead() *llmv1.ImageEditRequest {
	return &llmv1.ImageEditRequest{Frame: &llmv1.ImageEditRequest_Head{Head: &llmv1.ImageEditHead{ContentType: "multipart/form-data; boundary=x"}}}
}

// The pipeline refuses before it reads the body (a bad bearer here): the
// handler must not leave a Recv running behind it.
func TestImageEditEarlyRefusalLeavesNoRecvBehind(t *testing.T) {
	refusing := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	service, err := modelgrpc.NewServer(refusing, nil)
	if err != nil {
		t.Fatal(err)
	}
	stream := &editStream{
		ctx:     context.Background(),
		frames:  []*llmv1.ImageEditRequest{multipartHead()},
		release: make(chan struct{}),
	}
	err = service.ImageEdit(stream)
	running := stream.handlerReturned()
	close(stream.release)
	time.Sleep(20 * time.Millisecond) // a leaked reader would Recv again by now
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("ImageEdit = %v, want Unauthenticated", err)
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if running != 0 || stream.late != 0 {
		t.Fatalf("Recv ran after the handler returned: %d in flight, %d late", running, stream.late)
	}
}

// The body is read as the pipeline asks for it, and a second head is the
// caller's protocol error, not the pipeline's.
func TestImageEditReadsTheBodyOnDemandAndRefusesASecondHead(t *testing.T) {
	var read string
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		read = string(body)
		_, _ = io.WriteString(w, `{}`)
	})
	service, err := modelgrpc.NewServer(echo, nil)
	if err != nil {
		t.Fatal(err)
	}
	data := func(s string) *llmv1.ImageEditRequest {
		return &llmv1.ImageEditRequest{Frame: &llmv1.ImageEditRequest_Data{Data: []byte(s)}}
	}
	released := make(chan struct{})
	close(released)
	stream := &editStream{ctx: context.Background(), frames: []*llmv1.ImageEditRequest{multipartHead(), data("ab"), data("cd")}, release: released}
	if err := service.ImageEdit(stream); err != nil || read != "abcd" || stream.answered == nil {
		t.Fatalf("ImageEdit = %v, read %q, answered %v", err, read, stream.answered)
	}

	stream = &editStream{ctx: context.Background(), frames: []*llmv1.ImageEditRequest{multipartHead(), data("ab"), multipartHead()}, release: released}
	if err := service.ImageEdit(stream); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a second head = %v, want InvalidArgument", err)
	}
}
