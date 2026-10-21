package run_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	enginev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/engine/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/engine/enginetest"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// A fake sidecar on a Unix socket: it records the request, streams the
// scripted lines (one event each) with a pause between them, and honours a
// stop by ending the stream early.
type fakeSidecar struct {
	enginev1.UnimplementedEngineServiceServer
	socket   string
	lines    []string
	pause    time.Duration
	mu       sync.Mutex
	requests []map[string]any
	stops    []string
	// byTool scripts a tool's own stream; a tool not in it streams lines.
	byTool map[string][]string
	// tools is what Health lists; nil lists none, as an engine that does not
	// list its tools answers.
	tools []string
	// healthCalls counts Health.
	healthCalls int
}

func newFakeSidecar(t *testing.T, lines []string, pause time.Duration) *fakeSidecar {
	t.Helper()
	f := &fakeSidecar{lines: lines, pause: pause}
	f.socket = enginetest.Serve(t, f)
	return f
}

func (f *fakeSidecar) Stop(_ context.Context, request *enginev1.StopRequest) (*enginev1.StopResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, request.GetInvocationId())
	return &enginev1.StopResponse{Stopped: true}, nil
}

func (f *fakeSidecar) Health(context.Context, *enginev1.HealthRequest) (*enginev1.HealthResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthCalls++
	return &enginev1.HealthResponse{Status: "UP", Runner: "native", Tools: append([]string(nil), f.tools...)}, nil
}

func (f *fakeSidecar) Invoke(request *enginev1.InvokeRequest, stream enginev1.EngineService_InvokeServer) error {
	var arguments map[string]any
	_ = json.Unmarshal(request.GetArgumentsJson(), &arguments)
	recorded := map[string]any{"invocation_id": request.GetInvocationId(), "tool": request.GetTool(), "arguments": arguments}
	f.mu.Lock()
	f.requests = append(f.requests, recorded)
	lines := f.lines
	if scripted, ok := f.byTool[request.GetTool()]; ok {
		lines = scripted
	}
	f.mu.Unlock()
	for _, line := range lines {
		f.mu.Lock()
		stopped := false
		for _, id := range f.stops {
			stopped = stopped || id == request.GetInvocationId()
		}
		f.mu.Unlock()
		if stopped {
			return nil
		}
		event, err := enginetest.Event(line)
		if err != nil {
			return err
		}
		if err := stream.Send(event); err != nil {
			return err
		}
		if f.pause > 0 {
			time.Sleep(f.pause)
		}
	}
	return nil
}

func engineRunner(f *fakeSidecar, client run.ArtifactClient) *run.Runner {
	settings, _ := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", func(key string) (string, bool) {
		switch key {
		case "ELITEA_DEEPWIKI_GIT_ALLOWLIST":
			return "github.com,*.github.com", true
		case "ELITEA_DEEPWIKI_ENGINE_SOCKET":
			return f.socket, true
		}
		return "", false
	})
	runner := run.NewEngineRunner(settings)
	runner.Artifacts = func(map[string]any) (run.ArtifactClient, error) { return client, nil }
	return runner
}

func TestTheEngineRunnerStreamsProgressAndComposesTheResult(t *testing.T) {
	result := map[string]any{
		"success": true, "result": "Wiki generated: 1 pages", "wiki_id": "acme--e2e-service--main",
		"artifacts":          []any{map[string]any{"name": "acme--e2e-service--main/wiki_pages/a.md", "type": "text/markdown", "data": "# a"}},
		"repository_context": "repository: acme/e2e-service\n",
	}
	encoded, _ := json.Marshal(map[string]any{"result": result})
	sidecar := newFakeSidecar(t, []string{`{"thinking": "Cloning the repository"}`, `{"thinking": "Indexing 12 files"}`, string(encoded)}, 0)
	client := &fakeArtifactClient{}
	body, events, err := invokeWithEvents(t, engineRunner(sidecar, client), spi.Family{Name: "main"}, "generate_wiki", fixtureRequest("GO", transport), "")
	if err != nil {
		t.Fatal(err)
	}
	objects := objectsOf(t, body)
	if objects[0]["data"] != "Wiki generated: 1 pages" || len(client.uploads) != 2 {
		t.Fatalf("objects %v uploads %d", objects, len(client.uploads))
	}
	text := strings.Join(events, "\n")
	for _, want := range []string{"Starting generate_wiki", "Cloning the repository", "Indexing 12 files", "Uploaded 2 wiki objects"} {
		if !strings.Contains(text, want) {
			t.Fatalf("events lack %q: %q", want, text)
		}
	}
	// The sidecar received the legacy keyword set for this invocation.
	if len(sidecar.requests) != 1 || sidecar.requests[0]["tool"] != "generate_wiki" || !strings.HasPrefix(sidecar.requests[0]["invocation_id"].(string), "invocation_") {
		t.Fatalf("%v", sidecar.requests)
	}
	arguments := sidecar.requests[0]["arguments"].(map[string]any)
	for _, key := range []string{"query", "repo_config", "llm_settings", "active_branch", "run_in_subprocess", "planner_mode", "exclude_tests", "indexing_method", "force_rebuild_index", "embedding_model"} {
		if _, ok := arguments[key]; !ok {
			t.Fatalf("arguments lack %s: %v", key, arguments)
		}
	}
}

func TestAnEngineFailureKeepsItsTypeAndCategory(t *testing.T) {
	cases := []struct {
		line     string
		category string
		errType  string
	}{
		{`{"error": {"message": "Wiki not found for repository", "error_type": "FileNotFoundError", "error_category": "resource_not_found"}}`, "resource_not_found", "FileNotFoundError"},
		{`{"error": {"message": "query must not be empty", "error_type": "ValueError", "error_category": "invalid_input"}}`, "invalid_input", "ValueError"},
		{`{"result": {"success": false, "error": "[SERVICE_BUSY] too many jobs"}}`, "runtime_error", "RuntimeError"},
		{`{"result": {"success": false, "error": "bad query", "error_category": "invalid_input"}}`, "invalid_input", "ValueError"},
	}
	for _, tc := range cases {
		sidecar := newFakeSidecar(t, []string{tc.line}, 0)
		request := fixtureRequest("", transport)
		request["parameters"] = map[string]any{"question": "?"}
		body, _, err := invokeWithEvents(t, engineRunner(sidecar, &fakeArtifactClient{}), spi.Family{Name: "main"}, "ask", request, "")
		if err == nil || body["error_category"] != tc.category || body["error_type"] != tc.errType {
			t.Errorf("%s: %v %v", tc.line, body, err)
		}
	}
}

func TestAStopReachesTheSidecarAndEndsTheRunAsCancelled(t *testing.T) {
	lines := []string{`{"thinking": "Cloning the repository"}`}
	for i := 0; i < 40; i++ {
		lines = append(lines, fmt.Sprintf(`{"thinking": "step %d"}`, i))
	}
	lines = append(lines, `{"result": {"success": true, "result": "never"}}`)
	sidecar := newFakeSidecar(t, lines, 50*time.Millisecond)
	client := &fakeArtifactClient{}
	body, events, err := invokeWithEvents(t, engineRunner(sidecar, client), spi.Family{Name: "main"}, "generate_wiki", fixtureRequest("GO", transport), "Cloning the repository")
	if err == nil {
		t.Fatalf("a stopped run completed: %v", body)
	}
	if len(client.uploads) != 0 || strings.Contains(strings.Join(events, "\n"), "never") {
		t.Fatal("a stopped run produced output")
	}
	sidecar.mu.Lock()
	stops := append([]string(nil), sidecar.stops...)
	sidecar.mu.Unlock()
	if len(stops) != 1 || !strings.HasPrefix(stops[0], "invocation_") {
		t.Fatalf("stops %v", stops)
	}
}

func TestAnUnreachableEngineIsARuntimeErrorNotAHang(t *testing.T) {
	settings, _ := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", func(key string) (string, bool) {
		switch key {
		case "ELITEA_DEEPWIKI_GIT_ALLOWLIST":
			return "*", true
		case "ELITEA_DEEPWIKI_ENGINE_SOCKET":
			return "/nonexistent/engine.sock", true
		}
		return "", false
	})
	runner := run.NewEngineRunner(settings)
	// The facade's llm_settings carry the project an ask is scoped to
	// (project.go); without one the ask is refused before the socket.
	request := fixtureRequest("", transport)
	request["parameters"] = map[string]any{"question": "?"}
	body, _, err := invokeWithEvents(t, runner, spi.Family{Name: "main"}, "ask", request, "")
	if err == nil || body["error_category"] != "runtime_error" || !strings.Contains(str(body["result"]), "not reachable") {
		t.Fatalf("%v %v", body, err)
	}
}

func TestAStreamThatEndsWithoutAResultIsAnError(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{`{"thinking": "Cloning"}`}, 0)
	request := fixtureRequest("", transport)
	request["parameters"] = map[string]any{"question": "?"}
	body, _, err := invokeWithEvents(t, engineRunner(sidecar, &fakeArtifactClient{}), spi.Family{Name: "main"}, "ask", request, "")
	if err == nil || !strings.Contains(str(body["result"]), "without a result") {
		t.Fatalf("%v %v", body, err)
	}
}
