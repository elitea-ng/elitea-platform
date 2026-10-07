package run_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// The Rust-native Inventory engine (ADR-0027) behind THIS host's own sidecar
// client: the real binary on a real Unix socket, the real composition and
// upload, answering what this host's own fixture runner answers.
//
// The binary is built by ci-inventory-engine.yml, which sets
// ELITEA_INVENTORY_NATIVE_ENGINE_BIN and ELITEA_REQUIRE_NATIVE_ENGINE=1. The
// second turns the skip below into a failure there, so a broken path to the
// binary cannot pass as a run that never happened. ci-go.yml builds no Rust,
// so the skip is declared in scripts/go/declared-skips.txt.

func startInventoryEngine(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("ELITEA_INVENTORY_NATIVE_ENGINE_BIN")
	if binary == "" {
		if os.Getenv("ELITEA_REQUIRE_NATIVE_ENGINE") == "1" {
			t.Fatal("ELITEA_REQUIRE_NATIVE_ENGINE=1 but ELITEA_INVENTORY_NATIVE_ENGINE_BIN is not set")
		}
		t.Skip("ELITEA_INVENTORY_NATIVE_ENGINE_BIN is not set; ci-inventory-engine.yml builds the Rust engine and runs this")
	}
	// macOS caps a Unix socket path at 104 bytes; t.TempDir is long.
	dir, err := os.MkdirTemp("/tmp", "inv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "e.sock")
	env := append(os.Environ(),
		"ELITEA_INVENTORY_RUNNER=fixture",
		"ELITEA_INVENTORY_ENGINE_SOCKET="+socket,
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
			t.Fatal("the native Inventory engine never answered its healthcheck")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runners are the two runners under test: the host's engine runner over the
// Rust sidecar, and the host's own fixture runner, each with its own fake
// artifact store.
func runners(t *testing.T, socket string) (native, golden *run.Runner, nativeUploads, goldenUploads *fakeArtifacts) {
	t.Helper()
	settings, err := spi.SettingsFromEnv("ELITEA_INVENTORY_", func(key string) (string, bool) {
		if key == "ELITEA_INVENTORY_ENGINE_SOCKET" {
			return socket, true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	nativeUploads = &fakeArtifacts{fail: map[string]error{}}
	goldenUploads = &fakeArtifacts{fail: map[string]error{}}
	native = run.NewEngineRunner(settings)
	native.Artifacts = nativeUploads.factory()
	golden = run.NewFixtureRunner(settings, 0)
	golden.Artifacts = goldenUploads.factory()
	return native, golden, nativeUploads, goldenUploads
}

// invokeOn drives one call through the real invocation manager to its
// terminal body, as harness.invoke does for a hand-made runner.
func invokeOn(t *testing.T, runner *run.Runner, toolkit, tool string, request map[string]any) map[string]any {
	t.Helper()
	resolved, err := inventory.Toolkits.Resolve(toolkit)
	if err != nil {
		t.Fatal(err)
	}
	manager := spi.NewManager(nil, time.Hour, nil)
	manager.Start(context.Background())
	defer manager.Stop()
	ctx := context.Background()
	invocation, err := manager.Submit(ctx, toolkit, tool, func(ctx context.Context, tc *spi.Context) (map[string]any, error) {
		return runner.Invoke(ctx, spi.Invoke{Family: resolved, Toolkit: toolkit, Tool: tool, Request: request}, tc)
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		body, err := manager.Poll(ctx, toolkit, tool, invocation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if status := body["status"]; status == "Completed" || status == "Error" {
			return body
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the invocation never settled")
	return nil
}

// parsed decodes a JSON text so two writers' documents compare as documents:
// the host's fixture writes Go maps (sorted keys), the engines keep the
// Python engine's insertion order. The document is the contract.
func parsed(t *testing.T, text string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, text)
	}
	return value
}

// composed decodes a composed result — a JSON list of objects whose `data`
// may itself be a JSON text — with every nested document parsed.
func composed(t *testing.T, text string) any {
	t.Helper()
	var objects []map[string]any
	if err := json.Unmarshal([]byte(text), &objects); err != nil {
		return parsed(t, text)
	}
	for _, object := range objects {
		if data, ok := object["data"].(string); ok {
			var document any
			if json.Unmarshal([]byte(data), &document) == nil {
				object["data"] = document
			}
		}
	}
	return objects
}

func uploadsByName(t *testing.T, uploads *fakeArtifacts) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, u := range uploads.uploads {
		out[u.Bucket+"/"+u.Name] = parsed(t, u.Data)
	}
	return out
}

func TestTheNativeEngineIngestsWhatTheGoFixtureDoes(t *testing.T) {
	native, golden, nativeUploads, goldenUploads := runners(t, startInventoryEngine(t))
	request := map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{
			"bucket": "code-graphs", "llm_settings": llmSettings(),
			"source": map[string]any{"type": "github", "id": "acme-widgets", "settings": map[string]any{}},
		}},
	}
	nativeBody := invokeOn(t, native, "inventory", "run_ingestion", request)
	goldenBody := invokeOn(t, golden, "inventory", "run_ingestion", request)
	if nativeBody["status"] != "Completed" || goldenBody["status"] != "Completed" {
		t.Fatalf("native %v, go %v", nativeBody, goldenBody)
	}
	got, want := uploadsByName(t, nativeUploads), uploadsByName(t, goldenUploads)
	if len(got) == 0 || !reflect.DeepEqual(got, want) {
		names := func(m map[string]any) []string {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return keys
		}
		t.Fatalf("uploads differ:\nnative %v\ngo     %v\n%v\n%v", names(got), names(want), got, want)
	}
	if !reflect.DeepEqual(composed(t, nativeBody["result"].(string)), composed(t, goldenBody["result"].(string))) {
		t.Fatalf("composed results differ:\nnative %v\ngo     %v", nativeBody["result"], goldenBody["result"])
	}
}

// Every retrieval golden family, answered through the socket, is the Go
// fixture's answer: the same JSON document.
func TestTheNativeEngineAnswersRetrievalAsTheGoFixtureDoes(t *testing.T) {
	native, golden, _, _ := runners(t, startInventoryEngine(t))
	for _, tc := range []struct {
		toolkit, tool string
		parameters    map[string]any
	}{
		{"inventory", "get_stats", nil},
		{"inventory", "list_ingested_sources", nil},
		{"inventory", "get_sources_status", nil},
		{"inventory", "impact_analysis", map[string]any{"entity_id": "code:payment-client"}},
		{"inventory", "get_related_entities", map[string]any{"entity_id": "code:payment-client"}},
		{"inventory", "get_cross_source_relations", nil},
		{"inventory", "search_graph", map[string]any{"query": "payment"}},
		{"inventory", "get_entity", map[string]any{"entity_id": "docs:checkout-guide"}},
		{"inventory", "list_presets", nil},
		{"inventory_search", "list_entity_types", nil},
		{"inventory_search", "investigate", map[string]any{"question": "checkout"}},
	} {
		t.Run(tc.toolkit+"/"+tc.tool, func(t *testing.T) {
			parameters := map[string]any{"output_format": "json"}
			for k, v := range tc.parameters {
				parameters[k] = v
			}
			request := map[string]any{"parameters": parameters}
			nativeBody := invokeOn(t, native, tc.toolkit, tc.tool, request)
			goldenBody := invokeOn(t, golden, tc.toolkit, tc.tool, request)
			if nativeBody["status"] != goldenBody["status"] {
				t.Fatalf("status: native %v, go %v", nativeBody, goldenBody)
			}
			left := composed(t, fmt.Sprint(nativeBody["result"]))
			right := composed(t, fmt.Sprint(goldenBody["result"]))
			if !reflect.DeepEqual(left, right) {
				t.Fatalf("answers differ:\nnative %v\ngo     %v", left, right)
			}
		})
	}
}
