package run_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// The Rust-native engine (ADR-0026) behind THIS host's own sidecar client:
// the real binary on a real Unix socket, the real composition and upload.
// The fake sidecar in engine_test.go proves the client; these prove that
// the Rust engine speaks what the client reads.
//
// The binary is built by ci-deepwiki-engine.yml, which sets
// ELITEA_DEEPWIKI_NATIVE_ENGINE_BIN and ELITEA_REQUIRE_NATIVE_ENGINE=1. The
// second turns the skip below into a failure there, so a broken path to the
// binary cannot pass as a run that never happened. ci-go.yml builds no
// Rust, so the skip is declared in scripts/go/declared-skips.txt.

// nativeEngine starts the Rust engine's fixture runner and answers its
// socket. step is ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS.
func nativeEngine(t *testing.T, step string) string {
	t.Helper()
	return startNativeEngine(t,
		"ELITEA_DEEPWIKI_RUNNER=fixture",
		"ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS="+step,
	)
}

// startNativeEngine starts the Rust engine with extra environment and
// answers its socket.
func startNativeEngine(t *testing.T, extra ...string) string {
	t.Helper()
	binary := os.Getenv("ELITEA_DEEPWIKI_NATIVE_ENGINE_BIN")
	if binary == "" {
		if os.Getenv("ELITEA_REQUIRE_NATIVE_ENGINE") == "1" {
			t.Fatal("ELITEA_REQUIRE_NATIVE_ENGINE=1 but ELITEA_DEEPWIKI_NATIVE_ENGINE_BIN is not set")
		}
		t.Skip("ELITEA_DEEPWIKI_NATIVE_ENGINE_BIN is not set; ci-deepwiki-engine.yml builds the Rust engine and runs this")
	}
	// macOS caps a Unix socket path at 104 bytes; t.TempDir is long.
	dir, err := os.MkdirTemp("/tmp", "dwn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "e.sock")
	env := append(append(os.Environ(), extra...),
		"ELITEA_DEEPWIKI_ENGINE_SOCKET="+socket,
	)
	server := exec.Command(binary, "serve")
	server.Env = env
	server.Stderr = os.Stderr
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Process.Kill()
		_ = server.Wait()
	})
	// The image's own probe decides readiness, so the probe is tested too.
	deadline := time.Now().Add(15 * time.Second)
	for {
		probe := exec.Command(binary, "healthcheck")
		probe.Env = env
		if probe.Run() == nil {
			return socket
		}
		if time.Now().After(deadline) {
			t.Fatal("the native engine never answered its healthcheck")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func nativeRunner(socket string, client run.ArtifactClient) *run.Runner {
	settings, _ := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", func(key string) (string, bool) {
		switch key {
		case "ELITEA_DEEPWIKI_GIT_ALLOWLIST":
			return "github.com,*.github.com", true
		case "ELITEA_DEEPWIKI_ENGINE_SOCKET":
			return socket, true
		}
		return "", false
	})
	runner := run.NewNamedEngineRunner(settings, "native")
	runner.Artifacts = func(map[string]any) (run.ArtifactClient, error) { return client, nil }
	return runner
}

func uploadedNames(client *fakeArtifactClient) []string {
	names := make([]string, 0, len(client.uploads))
	for _, u := range client.uploads {
		names = append(names, u.name)
	}
	sort.Strings(names)
	return names
}

func uploadedBody(client *fakeArtifactClient, suffix string) string {
	for _, u := range client.uploads {
		if strings.HasSuffix(u.name, suffix) {
			return u.data
		}
	}
	return ""
}

// documents replaces each result object's JSON-text `data` with the parsed
// document. The Go fixture writes its JSON artifacts with sorted keys (a Go
// map); the Rust engine keeps the Python engine's insertion order, which is
// what the real engine writes. The documents are the contract, not the key
// order inside them.
func documents(objects []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(objects))
	for _, object := range objects {
		copied := map[string]any{}
		for key, value := range object {
			copied[key] = value
		}
		if text, ok := object["data"].(string); ok {
			var parsed map[string]any
			if json.Unmarshal([]byte(text), &parsed) == nil {
				copied["data"] = parsed
			}
		}
		out = append(out, copied)
	}
	return out
}

// The Rust fixture lands the same wiki as this host's own fixture runner:
// the same object keys, the same page bodies and the same composed result.
func TestTheNativeEngineGeneratesTheWikiTheGoFixtureDoes(t *testing.T) {
	socket := nativeEngine(t, "0")
	native, golden := &fakeArtifactClient{}, &fakeArtifactClient{}
	runner := nativeRunner(socket, native)
	if runner.Name() != "native" {
		t.Fatalf("runner name %q", runner.Name())
	}
	body, events, err := invokeWithEvents(t, runner, spi.Family{Name: "main"}, "generate_wiki", fixtureRequest("GO", transport), "")
	if err != nil {
		t.Fatal(err)
	}
	reference, _, err := invokeWithEvents(t, fixtureRunner(golden, 0), spi.Family{Name: "main"}, "generate_wiki", fixtureRequest("GO", transport), "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := uploadedNames(native), uploadedNames(golden); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("uploads differ:\nnative %v\ngo     %v", got, want)
	}
	for _, page := range []string{"getting-started.md", "request-flow.md", "storage.md"} {
		if uploadedBody(native, page) != uploadedBody(golden, page) {
			t.Errorf("%s differs:\nnative %q\ngo     %q", page, uploadedBody(native, page), uploadedBody(golden, page))
		}
	}
	// The JSON artifacts carry the same document; their whitespace is each
	// writer's own (the Rust engine writes Python's json.dumps bytes).
	for _, suffix := range []string{"wiki_manifest_fixture-1.json", "wiki_structure_fixture.json"} {
		var a, b any
		if json.Unmarshal([]byte(uploadedBody(native, suffix)), &a) != nil || json.Unmarshal([]byte(uploadedBody(golden, suffix)), &b) != nil {
			t.Fatalf("%s is not JSON", suffix)
		}
		left, _ := json.Marshal(a)
		right, _ := json.Marshal(b)
		if string(left) != string(right) {
			t.Errorf("%s differs:\nnative %s\ngo     %s", suffix, left, right)
		}
	}
	nativeObjects, goObjects := documents(objectsOf(t, body)), documents(objectsOf(t, reference))
	left, _ := json.Marshal(nativeObjects)
	right, _ := json.Marshal(goObjects)
	if string(left) != string(right) {
		t.Errorf("composed results differ:\nnative %s\ngo     %s", left, right)
	}
	text := strings.Join(events, "\n")
	uploaded := fmt.Sprintf("Uploaded %d wiki objects", len(golden.uploads))
	for _, want := range []string{"Cloning the repository", "Indexing 12 files", "Planning the wiki structure", "Writing 3 pages", "Assembling the manifest", uploaded} {
		if !strings.Contains(text, want) {
			t.Errorf("events lack %q: %q", want, text)
		}
	}
}

// ask streams its answer down the token channel; the poll shows it as
// llm_chunk events that join back to the answer the result carries.
func TestTheNativeEngineStreamsTheAnswer(t *testing.T) {
	socket := nativeEngine(t, "0")
	request := fixtureRequest("", transport)
	request["parameters"] = map[string]any{"question": "where do pages live?"}
	body, events, err := invokeWithEvents(t, nativeRunner(socket, &fakeArtifactClient{}), spi.Family{Name: "main"}, "ask", request, "")
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	for _, event := range events {
		var chunk struct {
			Event string `json:"event"`
			Data  struct {
				Text string `json:"text"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(event), &chunk) == nil && chunk.Event == "llm_chunk" {
			streamed.WriteString(chunk.Data.Text)
		}
	}
	if streamed.String() != "Fixture answer to: where do pages live?" {
		t.Fatalf("streamed %q; events %q", streamed.String(), events)
	}
	if !strings.Contains(str(body["result"]), "Fixture answer to: where do pages live?") {
		t.Fatalf("result %v", body["result"])
	}
}

// deep_research publishes its plan as a structured todo_update event.
func TestTheNativeEnginePublishesTheResearchPlan(t *testing.T) {
	socket := nativeEngine(t, "0")
	request := fixtureRequest("", transport)
	request["parameters"] = map[string]any{"question": "how is storage organised?"}
	_, events, err := invokeWithEvents(t, nativeRunner(socket, &fakeArtifactClient{}), spi.Family{Name: "main"}, "deep_research", request, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(events, "\n"), `"event": "todo_update"`) {
		t.Fatalf("no plan event: %q", events)
	}
}

// A stop from the host ends the Rust engine's run mid-step: nothing is
// uploaded and the invocation is not completed.
func TestAStopEndsTheNativeEnginesRun(t *testing.T) {
	socket := nativeEngine(t, "30")
	client := &fakeArtifactClient{}
	started := time.Now()
	body, _, err := invokeWithEvents(t, nativeRunner(socket, client), spi.Family{Name: "main"}, "generate_wiki", fixtureRequest("GO", transport), "Cloning the repository")
	if err == nil {
		t.Fatalf("a stopped run completed: %v", body)
	}
	if len(client.uploads) != 0 {
		t.Fatalf("a stopped run uploaded %v", uploadedNames(client))
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the stop took %v; the engine waited out its step", elapsed)
	}
}

// The native runner (ELITEA_DEEPWIKI_RUNNER=native): generate_wiki runs in
// the engine's worker CHILD process, and the engine checks the clone host
// against ITS OWN git allowlist again, after the host's check passed. Here
// the host admits github.com and the engine admits only another host, so
// the run ends at the engine's check: through the child, the NDJSON relay
// and the host's error mapping, with nothing uploaded.
//
// A whole native generation is not run here: it needs a model gateway, a
// git host and PostgreSQL, which this job does not stand up for the Go
// tests. The crate's own tests/native_generate.rs runs it end to end (a
// git http-backend, a mock gateway, the CI PostgreSQL service). The database
// URL below is never connected to: the run fails before it opens a build.
func TestTheNativeRunnerRechecksTheCloneHostInItsWorker(t *testing.T) {
	scratch, err := os.MkdirTemp("/tmp", "dwns")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	socket := startNativeEngine(t,
		"ELITEA_DEEPWIKI_RUNNER=native",
		"ELITEA_DEEPWIKI_DATABASE_URL=postgresql://deepwiki:deepwiki@127.0.0.1:1/deepwiki",
		"ELITEA_DEEPWIKI_BUILD_OWNER=native-go-test",
		"ELITEA_DEEPWIKI_GIT_ALLOWLIST=git.example.com",
		"ELITEA_DEEPWIKI_SCRATCH_PATH="+scratch,
		// The platform listener the native runner requires; never reached:
		// the run ends at the engine's clone-host check.
		"ELITEA_DEEPWIKI_MODEL_SERVICE_URL=http://127.0.0.1:1",
	)
	client := &fakeArtifactClient{}
	runner := nativeRunner(socket, client)
	llm := map[string]any{}
	for key, value := range transport {
		llm[key] = value
	}
	llm["model_name"] = "gpt-4o"
	request := map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{
			"code_toolkit": map[string]any{
				"github_configuration": map[string]any{"url": "https://github.com"},
				"repository":           "acme/e2e-service",
				"active_branch":        "main",
			},
			"llm_settings":    llm,
			"embedding_model": "text-embedding-3-small",
		}},
		"parameters": map[string]any{"query": "Document it"},
	}
	_, events, err := invokeWithEvents(t, runner, spi.Family{Name: "main"}, "generate_wiki", request, "")
	if err == nil {
		t.Fatal("a clone host off the engine's allowlist was admitted")
	}
	if !strings.Contains(err.Error(), "not on the git-host allowlist") {
		t.Fatalf("error %v", err)
	}
	if len(client.uploads) != 0 {
		t.Fatalf("a refused run uploaded %v", uploadedNames(client))
	}
	if text := strings.Join(events, "\n"); !strings.Contains(text, "DeepWiki worker started") {
		t.Fatalf("the run never reached the worker child: %q", text)
	}
	entries, _ := os.ReadDir(filepath.Join(scratch, "jobs"))
	if len(entries) != 0 {
		t.Fatalf("the worker's scratch directory was left: %v", entries)
	}
}

// An ARTIFACT-FOLDER source through the native runner: the host skips the
// git allowlist (CheckEgress), and the engine's worker CHILD lists and
// downloads the folder from the platform's ArtifactObjectService (on the
// platform listener, ELITEA_DEEPWIKI_MODEL_SERVICE_URL) with the
// invocation's callback bearer, although the engine's git
// allowlist names another host. The host stops the run once the folder is
// read (the next step needs PostgreSQL, which this job does not stand up):
// nothing is uploaded and the job directory is removed. The crate's
// tests/native_generate.rs runs a whole folder generation.
func TestTheNativeRunnerReadsAnArtifactFolderInItsWorker(t *testing.T) {
	objects := map[string]string{
		"docs/README.md":    "# Handbook\n\nHow the team works.\n",
		"docs/guide/run.py": "def run():\n    return 1\n",
	}
	platform := serveArtifactObjects(t, objects)

	scratch, err := os.MkdirTemp("/tmp", "dwna")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	socket := startNativeEngine(t,
		"ELITEA_DEEPWIKI_RUNNER=native",
		"ELITEA_DEEPWIKI_DATABASE_URL=postgresql://deepwiki:deepwiki@127.0.0.1:1/deepwiki",
		"ELITEA_DEEPWIKI_BUILD_OWNER=native-go-test",
		"ELITEA_DEEPWIKI_GIT_ALLOWLIST=git.example.com",
		"ELITEA_DEEPWIKI_SCRATCH_PATH="+scratch,
		"ELITEA_DEEPWIKI_MODEL_SERVICE_URL=http://"+platform.address,
	)
	client := &fakeArtifactClient{}
	runner := nativeRunner(socket, client)
	llm := map[string]any{
		"api_base":     "https://elitea-main:9446/llm/v1",
		"api_key":      "minted",
		"organization": "90200",
		"model_name":   "gpt-4o",
	}
	request := map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{
			"code_toolkit": map[string]any{
				"artifact_configuration": map[string]any{"bucket": "handbook", "prefix": "docs"},
				"active_branch":          "main",
			},
			"llm_settings":    llm,
			"embedding_model": "text-embedding-3-small",
		}},
		"parameters": map[string]any{"query": "Document it"},
	}
	_, events, err := invokeWithEvents(t, runner, spi.Family{Name: "main"}, "generate_wiki", request, "Materialised artifact://")
	if err == nil {
		t.Fatal("a stopped run completed")
	}
	if strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("the folder met the git allowlist: %v", err)
	}
	text := strings.Join(events, "\n")
	if !strings.Contains(text, "Materialised artifact://handbook/docs: 2 objects") {
		t.Fatalf("the folder was not read: %v\n%s", err, text)
	}
	got := platform.calls()
	sort.Strings(got)
	want := []string{
		"Bearer minted GetObject 90200/handbook/docs/README.md",
		"Bearer minted GetObject 90200/handbook/docs/guide/run.py",
		"Bearer minted ListObjects 90200/handbook?prefix=docs/",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ArtifactObjectService calls %q, want %q", got, want)
	}
	if len(client.uploads) != 0 {
		t.Fatalf("a stopped run uploaded %v", uploadedNames(client))
	}
	// The stopped child is reaped and its job directory removed shortly
	// after the host reports the stop.
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, _ := os.ReadDir(filepath.Join(scratch, "jobs"))
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worker's scratch directory was left: %v", entries)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// fakeArtifactObjects is elitea-main's ArtifactObjectService as the native
// engine's worker reaches it: plaintext gRPC on loopback (the engine admits
// http:// only there), one bucket's objects in project 90200, and a record
// of every call with the bearer it carried.
type fakeArtifactObjects struct {
	runtimev1.UnimplementedArtifactObjectServiceServer
	objects map[string]string
	address string
	mu      sync.Mutex
	seen    []string
}

func serveArtifactObjects(t *testing.T, objects map[string]string) *fakeArtifactObjects {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeArtifactObjects{objects: objects, address: listener.Addr().String()}
	server := grpc.NewServer()
	runtimev1.RegisterArtifactObjectServiceServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return fake
}

func (f *fakeArtifactObjects) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *fakeArtifactObjects) record(ctx context.Context, call string) {
	bearer := ""
	if values := metadata.ValueFromIncomingContext(ctx, "authorization"); len(values) > 0 {
		bearer = values[0]
	}
	f.mu.Lock()
	f.seen = append(f.seen, bearer+" "+call)
	f.mu.Unlock()
}

func (f *fakeArtifactObjects) ListObjects(request *runtimev1.ArtifactListObjectsRequestV1, stream grpc.ServerStreamingServer[runtimev1.ArtifactListObjectsResponseV1]) error {
	f.record(stream.Context(), fmt.Sprintf("ListObjects %d/%s?prefix=%s", request.GetProjectId(), request.GetBucket(), request.GetPrefix()))
	if request.GetProjectId() != 90200 || request.GetBucket() != "handbook" {
		return status.Error(codes.NotFound, "no such bucket")
	}
	listed := []*runtimev1.ArtifactObjectInfoV1{{Key: "docs/"}}
	for key, body := range f.objects {
		if strings.HasPrefix(key, request.GetPrefix()) {
			listed = append(listed, &runtimev1.ArtifactObjectInfoV1{Key: key, SizeBytes: int64(len(body)), ModifiedAt: "2026-10-01T10:00:00Z"})
		}
	}
	return stream.Send(&runtimev1.ArtifactListObjectsResponseV1{Objects: listed})
}

func (f *fakeArtifactObjects) GetObject(request *runtimev1.ArtifactGetObjectRequestV1, stream grpc.ServerStreamingServer[runtimev1.ArtifactGetObjectResponseV1]) error {
	object := request.GetObject()
	f.record(stream.Context(), fmt.Sprintf("GetObject %d/%s/%s", object.GetProjectId(), object.GetBucket(), object.GetKey()))
	body, ok := f.objects[object.GetKey()]
	if object.GetProjectId() != 90200 || object.GetBucket() != "handbook" || !ok {
		return status.Error(codes.NotFound, "no such object")
	}
	info := &runtimev1.ArtifactObjectInfoV1{Key: object.GetKey(), SizeBytes: int64(len(body)), ModifiedAt: "2026-10-01T10:00:00Z"}
	if err := stream.Send(&runtimev1.ArtifactGetObjectResponseV1{Frame: &runtimev1.ArtifactGetObjectResponseV1_Info{Info: info}}); err != nil {
		return err
	}
	return stream.Send(&runtimev1.ArtifactGetObjectResponseV1{Frame: &runtimev1.ArtifactGetObjectResponseV1_Data{Data: []byte(body)}})
}
