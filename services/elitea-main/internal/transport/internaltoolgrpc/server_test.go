package internaltoolgrpc_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/grpcauth/grpcauthtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/internaltoolgrpc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
)

const (
	workerName = "elitea-worker"
	otherName  = "elitea-intruder"
	token7     = "actor-token-of-project-7"
)

// fakePlatform stands in for the public handler's MCP endpoint and the
// toolkit test_tool route: a bearer of project 7 only, the endpoint's JSON
// and event-stream answers, and the route's synchronous JSON.
type fakePlatform struct {
	mu       sync.Mutex
	bodies   []string
	scopes   []string
	started  chan struct{}
	canceled chan struct{}
}

func newFake() *fakePlatform {
	return &fakePlatform{started: make(chan struct{}, 1), canceled: make(chan struct{}, 1)}
}

func (f *fakePlatform) handler() http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token7 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"authentication required"}`)
				return
			}
			if chi.URLParam(r, "projectID") != "" && chi.URLParam(r, "projectID") != "7" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	project := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if chi.URLParam(r, "projectID") != "7" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"ok":false,"error":"no access to this project"}`)
				return
			}
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.bodies = append(f.bodies, string(body))
			f.scopes = append(f.scopes, chi.URLParam(r, "*"))
			f.mu.Unlock()
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			next(w, r)
		}
	}
	r.Post("/app/{projectID}/mcp/*", project(f.mcp))
	r.Post("/api/v2/elitea_core/test_tool/prompt_lib/{projectID}/{toolID}", project(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":"rows of toolkit %s","truncated":false}`, chi.URLParam(r, "toolID"))
	}))
	return r
}

func (f *fakePlatform) mcp(w http.ResponseWriter, r *http.Request) {
	var message struct {
		Method string `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&message)
	writeJSON := func(body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
	switch {
	case message.Method == "tools/list":
		writeJSON(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"list_agents","inputSchema":{"type":"object"}}]}}`)
	case message.Params.Name == "quick":
		writeJSON(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":%q}],"isError":false}}`, string(message.Params.Arguments)))
	case message.Params.Name == "unknown":
		writeJSON(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"unknown tool"}}`)
	case message.Params.Name == "run" && strings.Contains(r.Header.Get("Accept"), "text/event-stream"):
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "id: cursor-1\ndata:\nretry: 1000\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(60 * time.Millisecond)
		_, _ = io.WriteString(w, "id: cursor-2\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"done\"}]}}\n\n")
	case message.Params.Name == "hang":
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "id: cursor-1\ndata:\nretry: 1000\n\n")
		w.(http.Flusher).Flush()
		f.started <- struct{}{}
		<-r.Context().Done()
		f.canceled <- struct{}{}
	default:
		writeJSON(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`)
	}
}

func start(t *testing.T, pki *grpcauthtest.PKI, handler http.Handler) string {
	t.Helper()
	service, err := internaltoolgrpc.NewServer(handler, nil)
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

func client(t *testing.T, address string, tlsConfig *tls.Config) runtimev1.InternalToolServiceClient {
	t.Helper()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return runtimev1.NewInternalToolServiceClient(connection)
}

func bearer(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

var scope = &runtimev1.InternalToolScopeV1{ProjectId: 7, Scope: "applications"}

// frames reads a CallTool stream: the progress stages, then the final frame.
func frames(stream grpc.ServerStreamingClient[runtimev1.InternalToolCallFrameV1]) ([]string, *runtimev1.InternalToolCallFrameV1, error) {
	var stages []string
	var last *runtimev1.InternalToolCallFrameV1
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stages, last, nil
		}
		if err != nil {
			return stages, last, err
		}
		if progress := frame.GetProgress(); progress != nil {
			if last != nil {
				return stages, last, errors.New("progress after the result")
			}
			stages = append(stages, progress.GetStage())
			continue
		}
		if last != nil {
			return stages, last, errors.New("two final frames")
		}
		last = frame
	}
}

func TestListAndCallTools(t *testing.T) {
	restore := internaltoolgrpc.SetKeepaliveInterval(10 * time.Millisecond)
	defer restore()
	pki := grpcauthtest.NewPKI(t)
	fake := newFake()
	tools := client(t, start(t, pki, fake.handler()), pki.ClientTLS(t, workerName))
	ctx := bearer(context.Background(), token7)

	listed, err := tools.ListTools(ctx, &runtimev1.InternalToolListRequestV1{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(listed.GetResultJson()), `"list_agents"`) {
		t.Errorf("listing %s", listed.GetResultJson())
	}

	quick, err := tools.CallTool(ctx, &runtimev1.InternalToolCallRequestV1{Scope: scope, Name: "quick", ArgumentsJson: []byte(`{"a":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	stages, final, err := frames(quick)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 0 || !strings.Contains(string(final.GetResultJson()), `{\"a\":1}`) {
		t.Errorf("quick: stages %v, final %v", stages, final)
	}

	// A run: the stream is primed (admitted), keeps reporting, then answers.
	run, err := tools.CallTool(ctx, &runtimev1.InternalToolCallRequestV1{Scope: scope, Name: "run"})
	if err != nil {
		t.Fatal(err)
	}
	stages, final, err = frames(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) < 2 || stages[0] != internaltoolgrpc.StageAdmitted || stages[1] != internaltoolgrpc.StageRunning {
		t.Errorf("run stages %v", stages)
	}
	if !strings.Contains(string(final.GetResultJson()), `"done"`) {
		t.Errorf("run final %v", final)
	}

	unknown, err := tools.CallTool(ctx, &runtimev1.InternalToolCallRequestV1{Scope: scope, Name: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	_, final, err = frames(unknown)
	if err != nil || final.GetError().GetCode() != -32602 {
		t.Errorf("a JSON-RPC error is in band: %v, %v", final, err)
	}

	fake.mu.Lock()
	scopes := append([]string(nil), fake.scopes...)
	fake.mu.Unlock()
	for _, seen := range scopes {
		if seen != "applications" {
			t.Errorf("the endpoint saw scope %q", seen)
		}
	}
}

func TestInvokeToolkitStreamsProgressThenTheAnswer(t *testing.T) {
	restore := internaltoolgrpc.SetKeepaliveInterval(5 * time.Millisecond)
	defer restore()
	pki := grpcauthtest.NewPKI(t)
	fake := newFake()
	tools := client(t, start(t, pki, fake.handler()), pki.ClientTLS(t, workerName))

	stream, err := tools.InvokeToolkit(bearer(context.Background(), token7), &runtimev1.InternalToolInvokeRequestV1{
		ProjectId: 7, ToolkitId: "42", ToolName: "read_file", ToolParamsJson: []byte(`{"path":"a.go"}`), RequestId: "investigate-42-read_file",
	})
	if err != nil {
		t.Fatal(err)
	}
	var stages []string
	var result []byte
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if progress := frame.GetProgress(); progress != nil {
			stages = append(stages, progress.GetStage())
			continue
		}
		result = frame.GetResultJson()
	}
	if len(stages) == 0 || stages[0] != internaltoolgrpc.StageAdmitted {
		t.Errorf("stages %v", stages)
	}
	if string(result) != `{"ok":true,"result":"rows of toolkit 42","truncated":false}` {
		t.Errorf("result %s", result)
	}
	fake.mu.Lock()
	body := fake.bodies[len(fake.bodies)-1]
	fake.mu.Unlock()
	var sent map[string]any
	_ = json.Unmarshal([]byte(body), &sent)
	if len(sent) != 4 || sent["tool_name"] != "read_file" || sent["toolkit_config"].(map[string]any)["toolkit_id"] != "42" {
		t.Errorf("the route got %s", body)
	}
}

func TestRefusals(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	fake := newFake()
	address := start(t, pki, fake.handler())

	t.Run("wrong identity", func(t *testing.T) {
		tools := client(t, address, pki.ClientTLS(t, otherName))
		_, err := tools.ListTools(bearer(context.Background(), token7), &runtimev1.InternalToolListRequestV1{Scope: scope})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("an unlisted workload: %v", err)
		}
	})
	t.Run("no certificate", func(t *testing.T) {
		tools := client(t, address, pki.AnonymousTLS())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := tools.ListTools(bearer(ctx, token7), &runtimev1.InternalToolListRequestV1{Scope: scope})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("no client certificate: %v", err)
		}
	})
	t.Run("bad token", func(t *testing.T) {
		tools := client(t, address, pki.ClientTLS(t, workerName))
		var trailer metadata.MD
		_, err := tools.ListTools(bearer(context.Background(), "forged"), &runtimev1.InternalToolListRequestV1{Scope: scope}, grpc.Trailer(&trailer))
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("a bad bearer: %v", err)
		}
		values := trailer.Get(internaltoolgrpc.RefusalTrailer)
		refusal := &runtimev1.InternalToolRefusalV1{}
		if len(values) != 1 || proto.Unmarshal([]byte(values[0]), refusal) != nil || refusal.GetHttpStatus() != http.StatusUnauthorized {
			t.Fatalf("refusal trailer %v", values)
		}
	})
	t.Run("cross-project toolkit", func(t *testing.T) {
		tools := client(t, address, pki.ClientTLS(t, workerName))
		stream, err := tools.InvokeToolkit(bearer(context.Background(), token7), &runtimev1.InternalToolInvokeRequestV1{ProjectId: 8, ToolkitId: "42", ToolName: "read_file"})
		if err != nil {
			t.Fatal(err)
		}
		for {
			_, err = stream.Recv()
			if err != nil {
				break
			}
		}
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("project 7's bearer invoked project 8's toolkit: %v", err)
		}
		values := stream.Trailer().Get(internaltoolgrpc.RefusalTrailer)
		refusal := &runtimev1.InternalToolRefusalV1{}
		if len(values) != 1 || proto.Unmarshal([]byte(values[0]), refusal) != nil || !strings.Contains(string(refusal.GetBody()), "no access") {
			t.Fatalf("refusal trailer %v", values)
		}
	})
	t.Run("a scope that would change the route", func(t *testing.T) {
		tools := client(t, address, pki.ClientTLS(t, workerName))
		for _, bad := range []string{"../../api/v2/x", "a//b", "a?b"} {
			_, err := tools.ListTools(bearer(context.Background(), token7), &runtimev1.InternalToolListRequestV1{Scope: &runtimev1.InternalToolScopeV1{ProjectId: 7, Scope: bad}})
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("scope %q: %v", bad, err)
			}
		}
	})
}

func TestCancellingCallToolCancelsTheHandler(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	fake := newFake()
	tools := client(t, start(t, pki, fake.handler()), pki.ClientTLS(t, workerName))
	ctx, cancel := context.WithCancel(bearer(context.Background(), token7))
	defer cancel()
	stream, err := tools.CallTool(ctx, &runtimev1.InternalToolCallRequestV1{Scope: scope, Name: "hang"})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := stream.Recv()
	if err != nil || frame.GetProgress().GetStage() != internaltoolgrpc.StageAdmitted {
		t.Fatalf("first frame %v, %v", frame, err)
	}
	<-fake.started
	cancel()
	select {
	case <-fake.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the call did not cancel the handler")
	}
}
