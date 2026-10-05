package run_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

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
	env := append(os.Environ(),
		"ELITEA_DEEPWIKI_RUNNER=fixture",
		"ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS="+step,
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
