package run_test

// The small decisions the invoke path makes on its way through, tested
// directly because each of them is reachable only from a body shape a whole
// invocation cannot produce on demand — an engine that answers a bare string,
// a runner composed with no name, an artifact whose key carries no extension.

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// Truthy is Python's truth, because the legacy merge branches on `if value`.
// Every JSON kind is listed: a type this returns `true` for by falling through
// the default arm would make an explicit empty value override a configured one.
func TestTruthyIsPythonsTruthForEveryJSONKind(t *testing.T) {
	for _, falsy := range []any{nil, false, "", 0, float64(0), int64(0), []any{}, map[string]any{}} {
		if run.Truthy(falsy) {
			t.Errorf("%#v reads as truthy", falsy)
		}
	}
	for _, truthy := range []any{true, "x", 1, float64(0.5), int64(2), []any{nil}, map[string]any{"k": nil}} {
		if !run.Truthy(truthy) {
			t.Errorf("%#v reads as falsy", truthy)
		}
	}
	// A kind the switch does not name: a struct is neither empty nor a
	// container, so it is truthy, and an empty one of a kind with a length is
	// not. The reflect arm is what a JSON decoder never reaches and a caller
	// passing a Go value does.
	if !run.Truthy(struct{}{}) {
		t.Error("a value of an unlisted kind reads as falsy")
	}
	if run.Truthy([0]int{}) {
		t.Error("an empty array reads as truthy")
	}
}

// The engine's failure dict becomes the category a caller can act on. The
// mapping is what turns "not found" into a 404-shaped answer instead of a
// generic runtime error, and the empty-message arms are what a caller sees
// when the engine failed without saying why.
func TestEveryEngineFailureShapeMapsToItsCategory(t *testing.T) {
	cases := []struct {
		name   string
		result map[string]any
		kind   spi.Kind
		text   string
	}{
		{"category invalid_input", map[string]any{"error_category": "invalid_input", "error": "bad id"}, spi.KindValue, "bad id"},
		{"ValueError", map[string]any{"error_type": "ValueError"}, spi.KindValue, "Invalid input"},
		{"category resource_not_found", map[string]any{"error_category": "resource_not_found", "error": "no graph"}, spi.KindNotFound, "no graph"},
		{"FileNotFoundError", map[string]any{"error_type": "FileNotFoundError"}, spi.KindNotFound, "Not found"},
		{"anything else", map[string]any{"error": "boom"}, spi.KindRuntime, "boom"},
		{"nothing at all", map[string]any{}, spi.KindRuntime, "Unknown error"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := run.EngineError(testCase.result)
			var failure *spi.Failure
			if !errors.As(err, &failure) {
				t.Fatalf("%v is not an SPI failure", err)
			}
			if failure.Kind != testCase.kind {
				t.Errorf("kind %v, want %v", failure.Kind, testCase.kind)
			}
			if err.Error() != testCase.text {
				t.Errorf("message %q, want %q", err.Error(), testCase.text)
			}
		})
	}
}

// The stored object's Content-Type is derived from the KEY's extension by
// elitea-main when the multipart part carries none, so an artifact whose key
// has no extension is one the graph browser cannot read. Each branch is a
// different way that goes wrong.
func TestAnArtifactsExtensionComesFromItsKeyThenItsType(t *testing.T) {
	cases := []struct {
		name        string
		key         string
		contentType string
		want        string
	}{
		{"the key wins", "graph.json", "text/markdown", "json"},
		{"json with no extension", "graph", "application/json", "json"},
		{"markdown with no extension", "summary", "text/markdown", "md"},
		{"neither", "summary", "application/octet-stream", "txt"},
		{"a trailing dot is not an extension", "graph.", "application/json", "json"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			composed := run.ComposeResultObjects(map[string]any{
				"result": "done",
				"artifacts": []any{map[string]any{
					"name": testCase.key, "type": testCase.contentType, "data": "{}",
				}},
			}, "graphs")
			if len(composed) != 2 {
				t.Fatalf("composed %d objects, want the message and the artifact", len(composed))
			}
			if composed[1].ResultExtension != testCase.want {
				t.Fatalf("extension %q, want %q", composed[1].ResultExtension, testCase.want)
			}
		})
	}
}

// Every object type the composer names, from the key alone. The classification
// is what a reader with only the composed list has to go on.
func TestTheComposerNamesWhatEachArtifactIs(t *testing.T) {
	composed := run.ComposeResultObjects(map[string]any{
		"result": "done",
		"artifacts": []any{
			map[string]any{"name": "graph.json"},
			map[string]any{"name": "sources_status.json"},
			map[string]any{"name": ".ingestion-checkpoint-github:1.json"},
			map[string]any{"name": "notes.md"},
		},
	}, "")
	want := []string{"message", "knowledge_graph", "sources_status", "ingestion_checkpoint", "inventory_artifact"}
	if len(composed) != len(want) {
		t.Fatalf("composed %d objects, want %d", len(composed), len(want))
	}
	for index, expected := range want {
		if composed[index].ObjectType != expected {
			t.Errorf("object %d is %q, want %q", index, composed[index].ObjectType, expected)
		}
	}
	// No bucket configured: the artifacts still name one, because an object
	// with no bucket is an upload with nowhere to go.
	for _, object := range composed[1:] {
		if object.ResultBucket != run.DefaultBucket {
			t.Errorf("%s has bucket %q", object.NameString(), object.ResultBucket)
		}
	}
	// The message carries no key and no bucket — it is not bound for one.
	if composed[0].NameString() != "" || composed[0].IsArtifact() {
		t.Errorf("the message object looks like an artifact: %+v", composed[0])
	}
}

// The terminal body is the frozen contract: a JSON STRING under `result`, and
// `[]` rather than `null` when there is nothing. A null there is what a
// browser renders as a crash instead of an empty list.
func TestTheTerminalBodyCarriesAnEncodedListEvenWhenEmpty(t *testing.T) {
	body := run.CompletedBody("inv-1", nil)
	if body["status"] != "Completed" || body["result_type"] != "String" {
		t.Fatalf("%v", body)
	}
	encoded, isString := body["result"].(string)
	if !isString {
		t.Fatalf("result is %T, not the encoded string", body["result"])
	}
	if encoded != "[]" {
		t.Fatalf("an empty result encoded as %q", encoded)
	}
	var decoded []map[string]any
	if err := json.Unmarshal([]byte(run.CompletedBody("inv-1",
		[]run.Object{run.Message("hello")})["result"].(string)), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || decoded[0]["data"] != "hello" {
		t.Fatalf("%v", decoded)
	}
}

// A runner composed with no name still answers one. /health reports it, and an
// empty string there reads as "no runner" when the truth is "a runner nobody
// named".
func TestARunnerWithNoNameStillReportsOne(t *testing.T) {
	if name := (&run.Runner{}).Name(); name != "tools" {
		t.Fatalf("an unnamed runner reports %q", name)
	}
	if name := (&run.Runner{RunnerName: "fixture"}).Name(); name != "fixture" {
		t.Fatalf("a named runner reports %q", name)
	}
}

// The two constructors, from settings alone. They are the composition root's
// only entry points, and a nil runner here is a host that comes up and cannot
// serve — the failure ADR-0023's registry test cannot see, because it only
// asks whether a factory returned an error.
func TestBothConstructorsProduceARunnerThatServesTheEngineTable(t *testing.T) {
	settings := spi.Settings{Prefix: "ELITEA_INVENTORY_", EngineSocket: "/run/inventory/engine.sock"}
	for name, runner := range map[string]*run.Runner{
		"fixture": run.NewFixtureRunner(settings, 0),
		"legacy":  run.NewEngineRunner(settings),
	} {
		if runner == nil {
			t.Fatalf("%s composed as nil", name)
		}
		if runner.Name() != name {
			t.Errorf("%s reports %q", name, runner.Name())
		}
		if runner.Artifacts == nil {
			t.Errorf("%s has no artifact transport: every upload would be skipped", name)
		}
		for _, tool := range run.EngineTools() {
			if _, served := runner.Tools[tool]; !served {
				t.Errorf("%s does not serve the admitted tool %s", name, tool)
			}
		}
	}
}

// The artifact factory answers nil — not an error — when the request carried
// no transport, because a direct SPI call has none and must still run.
func TestTheArtifactFactoryIsNilWithoutBothHalvesOfTheTransport(t *testing.T) {
	factory := run.ArtifactClientFrom("")
	for _, incomplete := range []map[string]any{
		{},
		{"api_base": "https://elitea.example"},
		{"api_key": "bearer"},
	} {
		client, err := factory(incomplete)
		if err != nil || client != nil {
			t.Errorf("%v produced a client (%v, %v)", incomplete, client, err)
		}
	}
	client, err := factory(map[string]any{
		"api_base": "https://elitea.example/llm/v1", "api_key": "bearer", "organization": "7"})
	if err != nil || client == nil {
		t.Fatalf("a complete transport produced no client: %v %v", client, err)
	}
}
