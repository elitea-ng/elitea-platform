package run_test

// The Inventory runner: the deferred refusals, the source check, the merge,
// composition and the upload.
//
// Every case here is about a decision the HOST makes before or after the
// engine, because those are the decisions the engine's own tests cannot see —
// the engine is handed arguments and gives back a dict, and it has no idea
// whether the tool it ran was one this platform ever served.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/artifacts"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// -- harness ------------------------------------------------------------------

type recordedUpload struct{ Bucket, Name, Data string }

type fakeArtifacts struct {
	uploads []recordedUpload
	fail    map[string]error
	// objects are what Download serves, keyed "bucket/key".
	objects map[string][]byte
}

// seededGraph is the graph.json every harness bucket holds, as the Python
// engine left it: what import_graph reads.
const seededGraph = `{"directed": true, "multigraph": false, "graph": {}, "nodes": [{"id": "code:a"}, {"id": "code:b"}], "links": [{"source": "code:a", "target": "code:b"}]}`

func (f *fakeArtifacts) Download(_ context.Context, bucket, key string) ([]byte, error) {
	if err := f.fail["download:"+key]; err != nil {
		return nil, err
	}
	data, ok := f.objects[bucket+"/"+key]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", artifacts.ErrNotFound, bucket, key)
	}
	return data, nil
}

func (f *fakeArtifacts) Upload(_ context.Context, bucket, name string, data []byte) error {
	if err := f.fail[name]; err != nil {
		return err
	}
	f.uploads = append(f.uploads, recordedUpload{bucket, name, string(data)})
	return nil
}

func (f *fakeArtifacts) factory() run.ArtifactClientFactory {
	return func(llmSettings map[string]any) (run.ArtifactClient, error) {
		if !run.Truthy(llmSettings["api_base"]) || !run.Truthy(llmSettings["api_key"]) {
			return nil, nil
		}
		return f, nil
	}
}

// harness runs one invocation through the real invocation manager, so the
// context, the checkpoint, the poll loop and the terminal body are the
// production ones — a runner tested against a hand-made context proves nothing
// about what a caller actually receives.
type harness struct {
	t        *testing.T
	runner   *run.Runner
	uploads  *fakeArtifacts
	lastArgs map[string]any
	// identity is the verified caller the gate hands the runner.
	identity spi.Identity
}

// TestTheVerifiedCallerReachesTheEngine pins ADR-0028 D3: the engine filters
// by document ACL, so it must learn who is calling — and only from the
// identity the gate verified, never from the body the caller writes.
func TestTheVerifiedCallerReachesTheEngine(t *testing.T) {
	h := newHarness(t, map[string]run.Tool{"search_graph": answer(map[string]any{"success": true, "result": "ok"})})
	h.identity = spi.Identity{ProjectID: "7", UserID: "42"}
	request := map[string]any{"project_id": 7, "parameters": map[string]any{
		"application_id": 70, "query": "x", "caller_user_id": "1"}}
	if _, err := h.invoke("inventory", "inventory", "search_graph", request); err != nil {
		t.Fatal(err)
	}
	if got := h.lastArgs["caller_user_id"]; got != "42" {
		t.Errorf("caller_user_id = %v, want the verified 42", got)
	}

	h.identity = spi.Identity{}
	if _, err := h.invoke("inventory", "inventory", "search_graph", request); err != nil {
		t.Fatal(err)
	}
	if _, present := h.lastArgs["caller_user_id"]; present {
		t.Errorf("an unsigned hop named a caller: %v", h.lastArgs)
	}
}

func newHarness(t *testing.T, tools map[string]run.Tool) *harness {
	t.Helper()
	uploads := &fakeArtifacts{fail: map[string]error{}, objects: map[string][]byte{"graphs/graph.json": []byte(seededGraph)}}
	h := &harness{t: t, uploads: uploads}
	wrapped := map[string]run.Tool{}
	for name, tool := range tools {
		inner := tool
		wrapped[name] = func(ctx context.Context, arguments map[string]any, tc *spi.Context) (map[string]any, error) {
			h.lastArgs = arguments
			return inner(ctx, arguments, tc)
		}
	}
	h.runner = &run.Runner{RunnerName: "legacy", Tools: wrapped, Artifacts: uploads.factory()}
	return h
}

// answer is a tool that returns a fixed engine result.
func answer(result map[string]any) run.Tool {
	return func(context.Context, map[string]any, *spi.Context) (map[string]any, error) {
		return result, nil
	}
}

// invoke drives one call to a terminal body. A failed invocation is returned
// as the body AND an error carrying its category, because both are what the
// caller sees: HTTP 200 with status Error is the frozen contract.
func (h *harness) invoke(family, toolkit, tool string, request map[string]any) (map[string]any, error) {
	h.t.Helper()
	resolved, err := inventory.Toolkits.Resolve(toolkit)
	if err != nil {
		h.t.Fatal(err)
	}
	if resolved.Name != family {
		h.t.Fatalf("toolkit %q resolved to family %q, expected %q", toolkit, resolved.Name, family)
	}
	manager := spi.NewManager(nil, time.Hour, nil)
	manager.Start(context.Background())
	defer manager.Stop()
	ctx := context.Background()
	invocation, err := manager.Submit(ctx, toolkit, tool, func(ctx context.Context, tc *spi.Context) (map[string]any, error) {
		return h.runner.Invoke(ctx, spi.Invoke{
			Family: resolved, Toolkit: toolkit, Tool: tool, Request: request, Identity: h.identity,
		}, tc)
	})
	if err != nil {
		h.t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, err := manager.Poll(ctx, toolkit, tool, invocation.ID)
		if err != nil {
			h.t.Fatal(err)
		}
		switch body["status"] {
		case "Completed":
			return body, nil
		case "Error":
			return body, fmt.Errorf("%v: %v", body["error_category"], body["result"])
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatal("the invocation never settled")
	return nil, nil
}

// category is the error category a failed invocation reported.
func category(body map[string]any) string { return fmt.Sprint(body["error_category"]) }

func objects(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal([]byte(body["result"].(string)), &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func llmSettings() map[string]any {
	return map[string]any{
		"api_base":     "https://elitea.example/llm/v1",
		"api_key":      "bearer-for-this-invocation",
		"organization": "7",
	}
}

// -- the five deferred tools ---------------------------------------------------

func TestADeclaredButNeverRoutedToolIsRefusedAsNotFound(t *testing.T) {
	// The legacy descriptor advertised twenty-nine tools on the `inventory`
	// family; its router carried twenty-four of them. The other five have
	// handler bodies in the plugin and were never reachable, so nothing has
	// ever run them — including no test.
	//
	// resource_not_found, not invalid_input: the caller asked for something the
	// descriptor advertises, and there is nothing behind it. invalid_input
	// would tell them to fix their request, and no request makes it run.
	deferred := []string{
		"query_graph", "get_type_stats", "link_toolkits_to_tools",
		"connect_orphan_nodes", "validate_relationships",
	}
	for _, tool := range deferred {
		t.Run(tool, func(t *testing.T) {
			h := newHarness(t, map[string]run.Tool{tool: answer(map[string]any{"success": true, "result": "ran"})})
			body, err := h.invoke("inventory", "inventory", tool, map[string]any{})
			if err == nil {
				t.Fatal("a tool that has never run on this platform completed")
			}
			if got := category(body); got != spi.CategoryResourceNotFound {
				t.Fatalf("category %q, want %q (%v)", got, spi.CategoryResourceNotFound, err)
			}
			if !strings.Contains(err.Error(), "never routed") && !strings.Contains(err.Error(), "never routed it") {
				t.Fatalf("the refusal does not say why: %v", err)
			}
		})
	}
}

func TestEveryDeferredToolIsStillAdvertisedAndAdmitted(t *testing.T) {
	// Refusing them by REMOVING them from the table would be a different
	// claim: "no such tool". What is true is that the tool is declared and
	// unimplemented, and a caller reading the descriptor deserves that answer
	// rather than one that contradicts the document they read.
	family, err := inventory.Toolkits.Resolve("inventory")
	if err != nil {
		t.Fatal(err)
	}
	admitted := map[string]bool{}
	for _, tool := range family.Tools {
		admitted[tool] = true
	}
	for tool := range run.DeferredTools["inventory"] {
		if !admitted[tool] {
			t.Fatalf("%s is refused as deferred but is no longer admitted, so the refusal a caller sees is 'unknown tool'", tool)
		}
	}
}

func TestAToolWhoseLegacyBodyIsAStubIsRefusedRatherThanReportingSuccess(t *testing.T) {
	// delta_update's legacy handler was routed and reachable, and its body is
	//
	//     # TODO: Implement delta update using EliteAClient
	//     return f"Delta update from toolkit {id} - Not yet implemented.", []
	//
	// which reached the caller as status Completed. This port refuses empty
	// successes everywhere else — the unavailable runner, a host wired with no
	// socket, an ingestion that produced no graph — and an agent reading
	// "Completed" cannot tell that its graph was not updated.
	h := newHarness(t, map[string]run.Tool{
		"delta_update": answer(map[string]any{"success": true, "result": "Not yet implemented."}),
	})
	body, err := h.invoke("inventory", "inventory", "delta_update", map[string]any{})
	if err == nil {
		t.Fatal("delta_update reported success; its legacy body is a TODO")
	}
	if got := category(body); got != spi.CategoryResourceNotFound {
		t.Fatalf("category %q, want %q", got, spi.CategoryResourceNotFound)
	}
	if !strings.Contains(err.Error(), "never updated a graph") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
}

func TestQueryGraphIsRefusedOnInventoryAndServedOnInventorySearch(t *testing.T) {
	// It is declared on BOTH families and was routed on only one. Serving it
	// on `inventory` would be a new tool, not a port.
	h := newHarness(t, map[string]run.Tool{"query_graph": answer(map[string]any{"success": true, "result": "rows"})})

	if _, err := h.invoke("inventory", "inventory", "query_graph", map[string]any{}); err == nil {
		t.Fatal("query_graph ran on the inventory family, which never routed it")
	}

	body, err := h.invoke("inventory_search", "inventory_search", "query_graph", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got := objects(t, body)[0]["data"]; got != "rows" {
		t.Fatalf("data %q", got)
	}
}

func TestTheSidecarIsNeverAskedForATooltHostRefuses(t *testing.T) {
	// The engine table is DERIVED from the admission table minus the deferred
	// set, so a sidecar is never wired for a tool this host will refuse — and,
	// more importantly, a tool added to the descriptor is wired without a
	// second list to remember to edit.
	served := map[string]bool{}
	for _, tool := range run.EngineTools() {
		served[tool] = true
	}
	for tool := range run.DeferredTools["inventory"] {
		if tool == "query_graph" {
			continue // served on the search family
		}
		if served[tool] {
			t.Fatalf("%s is wired to the sidecar and refused by the runner", tool)
		}
	}
	for _, want := range []string{"run_ingestion", "get_entity_neighbors", "smart_normalize_types", "investigate"} {
		if !served[want] {
			t.Fatalf("%s is advertised and not wired to the sidecar", want)
		}
	}
}

// -- the source check ----------------------------------------------------------

func TestAnIngestCallWithNoExpandedSourceIsRefused(t *testing.T) {
	// The legacy handler took a bare `toolkit_id` and resolved it ITSELF, with
	// an admin platform token, against an id the caller supplied that nothing
	// checked the caller could see. Under ADR-0022 decision 6 the expansion is
	// the facade's, so an unexpanded reference is refused rather than resolved.
	for _, tool := range []string{"run_ingestion"} {
		t.Run(tool, func(t *testing.T) {
			h := newHarness(t, map[string]run.Tool{tool: answer(map[string]any{"success": true, "result": "ingested"})})
			body, err := h.invoke("inventory", "inventory", tool, map[string]any{
				"parameters": map[string]any{"toolkit_id": float64(42)},
			})
			if err == nil {
				t.Fatal("an ingestion ran with no source to read from")
			}
			if got := category(body); got != spi.CategoryInvalidInput {
				t.Fatalf("category %q, want %q (%v)", got, spi.CategoryInvalidInput, err)
			}
			if !strings.Contains(err.Error(), "the facade does") {
				t.Fatalf("the refusal does not say who expands: %v", err)
			}
		})
	}
}

func TestABareToolkitIdIsRefusedRatherThanResolved(t *testing.T) {
	h := newHarness(t, map[string]run.Tool{"run_ingestion": answer(map[string]any{"success": true})})
	_, err := h.invoke("inventory", "inventory", "run_ingestion", map[string]any{
		"parameters": map[string]any{"source": float64(42)},
	})
	if err == nil || !strings.Contains(err.Error(), "not resolved here") {
		t.Fatalf("a bare id was accepted as a source: %v", err)
	}
}

func TestAReadOnlyToolNeedsNoSource(t *testing.T) {
	// Only the tools that READ a source need one. Requiring it everywhere
	// would make every query tool unusable without an ingestion configuration.
	h := newHarness(t, map[string]run.Tool{"get_stats": answer(map[string]any{"success": true, "result": "42 entities"})})
	if _, err := h.invoke("inventory", "inventory", "get_stats", map[string]any{}); err != nil {
		t.Fatal(err)
	}
}

func TestAnExpandedSourceReachesTheEngineUnchanged(t *testing.T) {
	source := map[string]any{
		"toolkit_id": float64(42), "type": "github", "name": "elitea-platform",
		"settings": map[string]any{"repository": "EliteaAI/elitea-platform"},
	}
	h := newHarness(t, map[string]run.Tool{"run_ingestion": answer(map[string]any{"success": true, "result": "done"})})
	if _, err := h.invoke("inventory", "inventory", "run_ingestion", map[string]any{
		"parameters": map[string]any{"source": source},
	}); err != nil {
		t.Fatal(err)
	}
	params := h.lastArgs["params"].(map[string]any)
	if fmt.Sprint(params["source"]) != fmt.Sprint(source) {
		t.Fatalf("source reached the engine as %v", params["source"])
	}
}

// -- the parameter merge and the identity ---------------------------------------

func TestTheMergeIsTheLegacyOne(t *testing.T) {
	// `if key not in params or value`: a tool argument absent from the
	// configuration always lands, and one that is present only overrides when
	// the tool's value is TRUTHY — so an explicit full_rebuild=false does not
	// override a configured true. Preserved because a caller may depend on
	// either half.
	h := newHarness(t, map[string]run.Tool{"get_stats": answer(map[string]any{"success": true, "result": "ok"})})
	if _, err := h.invoke("inventory", "inventory", "get_stats", map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{
			"bucket": "graphs", "full_rebuild": true,
		}},
		"parameters": map[string]any{"full_rebuild": false, "output_format": "json"},
	}); err != nil {
		t.Fatal(err)
	}
	params := h.lastArgs["params"].(map[string]any)
	if params["full_rebuild"] != true {
		t.Fatalf("an explicit false overrode a configured true: %v", params["full_rebuild"])
	}
	if params["output_format"] != "json" {
		t.Fatalf("a tool argument absent from the configuration did not land: %v", params["output_format"])
	}
}

func TestTheGraphsAddressIsCarriedToTheEngine(t *testing.T) {
	h := newHarness(t, map[string]run.Tool{"get_stats": answer(map[string]any{"success": true, "result": "ok"})})
	if _, err := h.invoke("inventory", "inventory", "get_stats", map[string]any{
		"configuration": map[string]any{"project_id": float64(7), "application_id": float64(42), "parameters": map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}
	if h.lastArgs["project_id"] != float64(7) || h.lastArgs["application_id"] != float64(42) {
		t.Fatalf("the engine was not told which graph: %v", h.lastArgs)
	}
	if h.lastArgs["family"] != "inventory" {
		t.Fatalf("family %v", h.lastArgs["family"])
	}
}

func TestTheSearchFamilyAddressesTheReferencedToolkitsGraph(t *testing.T) {
	// An inventory_search toolkit REFERENCES another toolkit's graph: the
	// application id is the referenced toolkit's, not the search toolkit's,
	// and the legacy handler read the project id off the request ROOT. Reading
	// only `configuration` here would point every search at its own empty
	// graph and answer "no graph configured" for a graph that exists.
	h := newHarness(t, map[string]run.Tool{"search_knowledge_graph": answer(map[string]any{"success": true, "result": "hits"})})
	if _, err := h.invoke("inventory_search", "inventory_search", "search_knowledge_graph", map[string]any{
		"project_id": float64(7),
		"configuration": map[string]any{"application_id": float64(99), "parameters": map[string]any{
			"inventory_toolkit": map[string]any{"id": float64(42), "name": "codebase"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if h.lastArgs["project_id"] != float64(7) {
		t.Fatalf("project %v: the search family carries it on the request root", h.lastArgs["project_id"])
	}
	if h.lastArgs["application_id"] != float64(42) {
		t.Fatalf("toolkit %v: the search family points at the REFERENCED toolkit, not its own", h.lastArgs["application_id"])
	}
}

// -- composition and upload ------------------------------------------------------

func TestTheGraphAndItsCompanionsReachTheConfiguredBucket(t *testing.T) {
	h := newHarness(t, map[string]run.Tool{"run_ingestion": answer(map[string]any{
		"success": true,
		"result":  "# Ingestion Complete",
		"artifacts": []any{
			map[string]any{"name": "graph.json", "type": "application/json", "data": `{"nodes":[]}`},
			map[string]any{"name": ".ingestion-checkpoint-repo.json", "type": "application/json", "data": "{}"},
			map[string]any{"name": "sources_status.json", "type": "application/json", "data": "{}"},
		},
	})})
	body, err := h.invoke("inventory", "inventory", "run_ingestion", map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{
			"bucket": "code-graphs", "llm_settings": llmSettings(),
			"source": map[string]any{"type": "github", "settings": map[string]any{}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.uploads.uploads) != 3 {
		t.Fatalf("uploaded %d objects, want 3: %+v", len(h.uploads.uploads), h.uploads.uploads)
	}
	for _, upload := range h.uploads.uploads {
		if upload.Bucket != "code-graphs" {
			t.Fatalf("%s went to %q, not the toolkit's configured bucket", upload.Name, upload.Bucket)
		}
	}
	list := objects(t, body)
	if list[0]["object_type"] != "message" || list[0]["data"] != "# Ingestion Complete" {
		t.Fatalf("the first object is not the tool's message: %v", list[0])
	}
	if list[1]["object_type"] != "knowledge_graph" || list[1]["result_extension"] != "json" {
		t.Fatalf("the graph is not composed as one: %v", list[1])
	}
}

func TestAFailedUploadIsReportedInBandAndTheInvocationStillCompletes(t *testing.T) {
	// The objects are returned inline either way, so the caller loses nothing
	// it had. What it must not get is a success that claims the bucket holds
	// them: the NEXT query downloads the graph from that bucket, and would
	// find the old one, or none.
	h := newHarness(t, map[string]run.Tool{"run_ingestion": answer(map[string]any{
		"success":   true,
		"result":    "done",
		"artifacts": []any{map[string]any{"name": "graph.json", "type": "application/json", "data": "{}"}},
	})})
	h.uploads.fail["graph.json"] = errors.New("Not authorized to upload artifact (HTTP 403)")

	body, err := h.invoke("inventory", "inventory", "run_ingestion", map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{
			"llm_settings": llmSettings(),
			"source":       map[string]any{"type": "github", "settings": map[string]any{}},
		}},
	})
	if err != nil {
		t.Fatal("a failed upload failed the whole invocation, discarding good artifacts")
	}
	if body["status"] != "Completed" {
		t.Fatalf("status %v", body["status"])
	}
	list := objects(t, body)
	last := fmt.Sprint(list[len(list)-1]["data"])
	if !strings.Contains(last, "⚠️") || !strings.Contains(last, "the next query will not see them") {
		t.Fatalf("the failure is not reported to the caller: %q", last)
	}
	if !strings.Contains(last, "HTTP 403") {
		t.Fatalf("the platform's own reason is not carried: %q", last)
	}
}

func TestWithNoTransportNothingIsUploadedAndTheResultStillCarriesTheObjects(t *testing.T) {
	// A direct SPI call carries no llm_settings. That is a legitimate shape —
	// the host's own tests make them — and it must not fail an invocation.
	h := newHarness(t, map[string]run.Tool{"run_ingestion": answer(map[string]any{
		"success":   true,
		"result":    "done",
		"artifacts": []any{map[string]any{"name": "graph.json", "type": "application/json", "data": "{}"}},
	})})
	body, err := h.invoke("inventory", "inventory", "run_ingestion", map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{
			"source": map[string]any{"type": "github", "settings": map[string]any{}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.uploads.uploads) != 0 {
		t.Fatal("something was uploaded with no transport configured")
	}
	if len(objects(t, body)) != 2 {
		t.Fatalf("the objects were dropped: %v", objects(t, body))
	}
}

func TestANamelessArtifactIsReportedRatherThanSilentlyDropped(t *testing.T) {
	// The key IS the name, so a nameless artifact cannot be uploaded. Skipping
	// it quietly is how a graph goes missing with the invocation still
	// reporting success.
	h := newHarness(t, map[string]run.Tool{"run_ingestion": answer(map[string]any{
		"success":   true,
		"result":    "done",
		"artifacts": []any{map[string]any{"type": "application/json", "data": "{}"}},
	})})
	body, err := h.invoke("inventory", "inventory", "run_ingestion", map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{
			"llm_settings": llmSettings(),
			"source":       map[string]any{"type": "github", "settings": map[string]any{}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	list := objects(t, body)
	if !strings.Contains(fmt.Sprint(list[len(list)-1]["data"]), "no name") {
		t.Fatalf("a nameless artifact vanished: %v", list)
	}
	if len(h.uploads.uploads) != 0 {
		t.Fatal("an artifact with no key was uploaded")
	}
}

func TestAFailedEngineResultBecomesTheErrorItStandsFor(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		result map[string]any
		want   string
	}{
		{"invalid input", map[string]any{"success": false, "error": "source.type 'gitlab' is not ingestible", "error_type": "ValueError"}, spi.CategoryInvalidInput},
		{"missing graph", map[string]any{"success": false, "error": "graph.json is missing", "error_type": "FileNotFoundError"}, spi.CategoryResourceNotFound},
		{"anything else", map[string]any{"success": false, "error": "the extractor gave up"}, spi.CategoryRuntime},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t, map[string]run.Tool{"get_stats": answer(testCase.result)})
			body, err := h.invoke("inventory", "inventory", "get_stats", map[string]any{})
			if err == nil {
				t.Fatal("a failed engine result completed successfully")
			}
			if got := category(body); got != testCase.want {
				t.Fatalf("category %q, want %q (%v)", got, testCase.want, err)
			}
		})
	}
}

func TestTheBucketDefaultsRatherThanFailingAfterTheWorkIsDone(t *testing.T) {
	// `bucket` is a REQUIRED descriptor field, so one always arrives in
	// practice. Failing here would throw away a completed ingestion over a
	// configuration problem that was knowable before it started.
	if run.ResolveBucket(run.Params{}) != run.DefaultBucket {
		t.Fatal("no bucket, no default")
	}
	if run.ResolveBucket(run.Params{"toolkit_configuration_bucket": "kg"}) != "kg" {
		t.Fatal("the UI's prefixed shape is not read")
	}
}

func TestTheArtifactTransportIsDerivedFromTheRequestsLlmSettings(t *testing.T) {
	// The same derivation the engine performs on its download side. Two
	// implementations of one legacy rule, so both are pinned.
	settings := run.ExtractArtifactSettings(llmSettings())
	if settings.BaseURL != "https://elitea.example" {
		t.Fatalf("base %q: the /llm/v1 suffix addresses the gateway, not the artifact routes", settings.BaseURL)
	}
	if settings.ProjectID != "7" {
		t.Fatalf("project %q", settings.ProjectID)
	}
}
