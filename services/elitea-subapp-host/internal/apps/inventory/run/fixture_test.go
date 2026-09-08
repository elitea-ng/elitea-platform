package run_test

// The Inventory fixture runner.
//
// WHAT THESE TESTS ARE FOR. A fixture is only worth having if a browser
// journey can predict what it will say — so the assertions here are the ones a
// journey makes: the object keys an ingestion lands, the entity a search finds,
// the refusal a missing id gets, and the two output formats a read answers in.
// A test that only checked "success is true" would pass against a runner that
// answered a constant for every tool, which is exactly the runner nobody can
// write a journey against.
//
// Everything runs through the REAL runner and the REAL invocation manager (the
// harness in run_test.go), so composition, the source check and the upload are
// production code here as well.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// fixtureHarness is the shared harness driving the fixture table rather than
// hand-written tools. Step is zero: pacing is a deployment setting and a test
// that waited for it would only be slower.
func fixtureHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, run.FixtureTools(0))
}

// expandedSource is the body shape the facade produces for an ingest call: the
// source arrives EXPANDED, which is the only shape CheckSource admits.
func expandedSource(extra map[string]any) map[string]any {
	source := map[string]any{
		"id": 12, "type": "github", "repository": "elitea/platform", "active_branch": "main",
	}
	params := map[string]any{"source": source, "llm_settings": llmSettings(), "bucket": "graphs"}
	for key, value := range extra {
		params[key] = value
	}
	return map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{"project_id": 7, "application_id": 3}},
		"parameters":    params,
	}
}

// read builds a body for a read tool: no source, merged parameters only.
func read(params map[string]any) map[string]any {
	return map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{"project_id": 7, "application_id": 3}},
		"parameters":    params,
	}
}

// message is the first composed object — the tool's own text — which is where
// every read tool's answer lands.
func message(t *testing.T, body map[string]any) string {
	t.Helper()
	list := objects(t, body)
	if len(list) == 0 {
		t.Fatal("the composed result carried no objects at all")
	}
	if list[0]["object_type"] != "message" {
		t.Fatalf("the first object is %v, not the tool's message", list[0]["object_type"])
	}
	text, _ := list[0]["data"].(string)
	return text
}

// document parses a read's answer under output_format=json.
func document(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(message(t, body)), &decoded); err != nil {
		t.Fatalf("output_format=json did not answer a JSON document: %v\n%s", err, message(t, body))
	}
	return decoded
}

// -- the table ----------------------------------------------------------------

// The fixture serves exactly what the engine serves. A tool the engine answers
// and the fixture does not would make a journey pass on one stack and fail on
// the other; the reverse would let a journey depend on a tool no deployment
// has.
func TestTheFixtureTableIsTheEngineTable(t *testing.T) {
	engine := run.EngineTools()
	fixture := run.FixtureTools(0)
	if len(engine) == 0 {
		t.Fatal("the engine table is empty — this test would pass on nothing")
	}
	sort.Strings(engine)
	for _, name := range engine {
		if _, served := fixture[name]; !served {
			t.Errorf("the engine serves %s and the fixture does not", name)
		}
	}
	for name := range fixture {
		if !contains(engine, name) {
			t.Errorf("the fixture serves %s and the engine does not", name)
		}
	}
}

// Every admitted tool answers. The failure this catches is a table entry that
// was added to the admission list and never given a handler: the host would
// admit the call and the fixture would produce a body a journey cannot read.
func TestEveryFixtureToolAnswersSomething(t *testing.T) {
	for _, family := range inventory.Toolkits.Families {
		for _, tool := range family.Tools {
			if _, deferred := run.DeferredTools[family.Name][tool]; deferred {
				continue
			}
			h := fixtureHarness(t)
			request := read(map[string]any{
				"entity_id": "code:place-order", "preset": "code",
				"query": "checkout", "question": "what places an order?",
				"llm_settings": llmSettings(),
			})
			if tool == "run_ingestion" {
				request = expandedSource(nil)
			}
			body, err := h.invoke(family.Name, family.Name, tool, request)
			if err != nil {
				t.Errorf("%s/%s: %v", family.Name, tool, err)
				continue
			}
			if strings.TrimSpace(message(t, body)) == "" {
				t.Errorf("%s/%s answered an empty message", family.Name, tool)
			}
		}
	}
}

// -- ingestion ----------------------------------------------------------------

// The three object keys a journey waits for, in the bucket the toolkit
// configured, with the source label the REQUEST named. The label is what makes
// this an assertion about the expansion rather than about a constant: a facade
// that forwarded an unexpanded id would produce a different one.
func TestAnIngestionLandsTheGraphAndItsCompanionsUnderTheRequestsSource(t *testing.T) {
	h := fixtureHarness(t)
	body, err := h.invoke("inventory", "inventory", "run_ingestion", expandedSource(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"graph.json":                           "knowledge_graph",
		"sources_status.json":                  "sources_status",
		".ingestion-checkpoint-github:12.json": "ingestion_checkpoint",
	}
	got := map[string]string{}
	for _, uploaded := range h.uploads.uploads {
		if uploaded.Bucket != "graphs" {
			t.Errorf("%s landed in %q, not the configured bucket", uploaded.Name, uploaded.Bucket)
		}
		got[uploaded.Name] = ""
	}
	for name := range want {
		if _, landed := got[name]; !landed {
			t.Errorf("%s never reached the bucket; uploaded %v", name, got)
		}
	}
	// The object_type each artifact composed as: the classification a graph
	// browser keys off, and the reason ComposeResultObjects has a switch.
	for _, object := range objects(t, body) {
		name, _ := object["name"].(string)
		if expected, tracked := want[name]; tracked && object["object_type"] != expected {
			t.Errorf("%s composed as %v, want %s", name, object["object_type"], expected)
		}
	}
	if !strings.Contains(message(t, body), "github:12") {
		t.Errorf("the result text does not name the ingested source: %q", message(t, body))
	}
}

// The graph that lands is a graph: a journey reads it back out of the bucket
// and renders it, so an empty or unparseable document would be a screen with
// nothing on it and a green test.
func TestTheLandedGraphIsAReadableDocument(t *testing.T) {
	h := fixtureHarness(t)
	if _, err := h.invoke("inventory", "inventory", "run_ingestion", expandedSource(nil)); err != nil {
		t.Fatal(err)
	}
	var graph map[string]any
	for _, uploaded := range h.uploads.uploads {
		if uploaded.Name != "graph.json" {
			continue
		}
		if err := json.Unmarshal([]byte(uploaded.Data), &graph); err != nil {
			t.Fatalf("graph.json is not JSON: %v", err)
		}
	}
	nodes, _ := graph["nodes"].([]any)
	edges, _ := graph["edges"].([]any)
	if len(nodes) != len(run.FixtureEntities) || len(edges) != len(run.FixtureRelations) {
		t.Fatalf("graph.json holds %d nodes and %d edges, want %d and %d",
			len(nodes), len(edges), len(run.FixtureEntities), len(run.FixtureRelations))
	}
	metadata, _ := graph["_metadata"].(map[string]any)
	if metadata["ingested_source"] != "github:12" {
		t.Errorf("the graph does not record the source it was built from: %v", metadata)
	}
}

// A source with no id still produces a usable label rather than an empty key.
// An object key that ends in `-.json` is one no bucket listing can attribute.
func TestASourceWithNoIdStillProducesANamedCheckpoint(t *testing.T) {
	cases := []struct {
		name   string
		source map[string]any
		want   string
	}{
		{"type only", map[string]any{"type": "github"}, "github"},
		{"id only", map[string]any{"id": 9}, "9"},
		{"neither", map[string]any{"repository": "x/y"}, "fixture-source"},
		{"toolkit_id", map[string]any{"toolkit_id": 4}, "4"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := run.SourceLabelFor(run.Params{"source": testCase.source}); got != testCase.want {
				t.Fatalf("label %q, want %q", got, testCase.want)
			}
		})
	}
}

// The source check is the runner's, not the fixture's — but a fixture that
// answered anyway would hide it, so this asserts the pair together.
func TestTheFixtureDoesNotIngestWithoutAnExpandedSource(t *testing.T) {
	h := fixtureHarness(t)
	body, err := h.invoke("inventory", "inventory", "run_ingestion",
		map[string]any{"parameters": map[string]any{"toolkit_id": 12}})
	if err == nil {
		t.Fatal("an unexpanded ingest call was served")
	}
	if category(body) != "invalid_input" {
		t.Fatalf("category %q, want invalid_input", category(body))
	}
	if len(h.uploads.uploads) != 0 {
		t.Fatalf("a refused ingestion still uploaded %d objects", len(h.uploads.uploads))
	}
}

// -- reads --------------------------------------------------------------------

// The search finds the entity a journey types, and finds nothing for a word
// the graph does not hold. Both halves matter: a matcher that returned
// everything would satisfy the first assertion alone.
func TestSearchDiscriminates(t *testing.T) {
	h := fixtureHarness(t)
	body, err := h.invoke("inventory", "inventory", "search_graph",
		read(map[string]any{"query": "payment", "output_format": "json"}))
	if err != nil {
		t.Fatal(err)
	}
	decoded := document(t, body)
	results, _ := decoded["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("searching for \"payment\" found %d entities, want the 2 that mention it: %v",
			len(results), decoded)
	}

	empty := fixtureHarness(t)
	body, err = empty.invoke("inventory", "inventory", "search_graph",
		read(map[string]any{"query": "no-such-word", "output_format": "json"}))
	if err != nil {
		t.Fatal(err)
	}
	if total, _ := document(t, body)["total"].(float64); total != 0 {
		t.Fatalf("a word the graph does not hold matched %v entities", total)
	}
}

// The search family's alias answers the same graph as the inventory family's
// tool. They are one handler in the legacy plugin, and a fixture that split
// them would let a journey pass on one toolkit and fail on the other.
func TestTheSearchFamilyAliasAnswersTheSameGraph(t *testing.T) {
	one := fixtureHarness(t)
	other := fixtureHarness(t)
	first, err := one.invoke("inventory", "inventory", "search_graph",
		read(map[string]any{"query": "order", "output_format": "json"}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := other.invoke("inventory_search", "inventory_search", "search_knowledge_graph",
		read(map[string]any{"query": "order", "output_format": "json"}))
	if err != nil {
		t.Fatal(err)
	}
	if message(t, first) != message(t, second) {
		t.Fatalf("the alias answered differently:\n%s\n%s", message(t, first), message(t, second))
	}
}

// Both output formats, on one tool. The legacy handlers all carry the switch,
// so a fixture that answered markdown to `output_format=json` would let a
// caller ship a parser against a shape the engine never sends.
func TestAReadAnswersMarkdownByDefaultAndJSONWhenAsked(t *testing.T) {
	text := fixtureHarness(t)
	body, err := text.invoke("inventory", "inventory", "get_stats", read(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(message(t, body), "# ") {
		t.Fatalf("the default answer is not markdown: %q", message(t, body))
	}

	structured := fixtureHarness(t)
	body, err = structured.invoke("inventory", "inventory", "get_stats",
		read(map[string]any{"output_format": "json"}))
	if err != nil {
		t.Fatal(err)
	}
	decoded := document(t, body)
	if count, _ := decoded["node_count"].(float64); int(count) != len(run.FixtureEntities) {
		t.Fatalf("node_count %v, want %d", decoded["node_count"], len(run.FixtureEntities))
	}
	if types, _ := decoded["entities_by_type"].(map[string]any); len(types) < 2 {
		t.Fatalf("the graph reports %d entity types; a fixture with one cannot prove a grouping", len(types))
	}
}

// An id the graph does not hold is a REFUSAL, not a success with an empty
// body. A screen written against the second renders a missing entity as an
// empty one and nobody finds out.
func TestAMissingEntityIsRefusedRatherThanAnsweredEmpty(t *testing.T) {
	for _, tool := range []string{"get_entity", "get_entity_content", "get_related_entities", "impact_analysis"} {
		h := fixtureHarness(t)
		body, err := h.invoke("inventory", "inventory", tool,
			read(map[string]any{"entity_id": "code:does-not-exist"}))
		if err == nil {
			t.Errorf("%s answered for an entity the graph does not hold", tool)
			continue
		}
		if category(body) != "resource_not_found" {
			t.Errorf("%s: category %q, want resource_not_found", tool, category(body))
		}
	}
	h := fixtureHarness(t)
	if _, err := h.invoke("inventory", "inventory", "get_preset_info",
		read(map[string]any{"preset": "no-such-preset"})); err == nil {
		t.Error("get_preset_info answered for a preset that does not exist")
	}
}

// The neighbour read reports both directions, because the graph view's
// "expand connections" draws both and a one-directional answer looks like a
// leaf node.
func TestNeighboursAreReportedInBothDirections(t *testing.T) {
	h := fixtureHarness(t)
	body, err := h.invoke("inventory", "inventory", "get_entity_neighbors",
		read(map[string]any{"entity_id": "code:place-order", "output_format": "json"}))
	if err != nil {
		t.Fatal(err)
	}
	related, _ := document(t, body)["related"].([]any)
	directions := map[string]int{}
	for _, raw := range related {
		row, _ := raw.(map[string]any)
		directions[strings.TrimSpace(strings.ToLower(toString(row["direction"])))]++
	}
	if directions["outgoing"] == 0 || directions["incoming"] == 0 {
		t.Fatalf("neighbours of code:place-order are %v; both directions must appear", directions)
	}
}

// Impact is the transitive closure, not the direct edges. A one-hop answer
// would be indistinguishable from the neighbour read.
func TestImpactIsTransitive(t *testing.T) {
	h := fixtureHarness(t)
	body, err := h.invoke("inventory", "inventory", "impact_analysis",
		read(map[string]any{"entity_id": "code:payment-client", "output_format": "json"}))
	if err != nil {
		t.Fatal(err)
	}
	impacted, _ := document(t, body)["impacted"].([]any)
	var names []string
	for _, raw := range impacted {
		names = append(names, toString(raw))
	}
	// place_order calls it directly; CheckoutService defines place_order; the
	// checkout guide documents CheckoutService. Two of those are more than one
	// hop away.
	for _, want := range []string{"code:place-order", "code:checkout-service", "docs:checkout-guide"} {
		if !contains(names, want) {
			t.Errorf("%s is not reported as impacted by code:payment-client: %v", want, names)
		}
	}
}

// Only the edges that cross a source boundary. An answer holding every edge
// would make the tool a synonym for the graph.
func TestCrossSourceRelationsAreOnlyTheCrossingOnes(t *testing.T) {
	h := fixtureHarness(t)
	body, err := h.invoke("inventory", "inventory", "get_cross_source_relations",
		read(map[string]any{"output_format": "json"}))
	if err != nil {
		t.Fatal(err)
	}
	relations, _ := document(t, body)["relations"].([]any)
	if len(relations) == 0 || len(relations) >= len(run.FixtureRelations) {
		t.Fatalf("%d of %d edges reported as cross-source", len(relations), len(run.FixtureRelations))
	}
	for _, raw := range relations {
		row, _ := raw.(map[string]any)
		if row["from_source"] == row["to_source"] {
			t.Errorf("a same-source edge is reported as crossing: %v", row)
		}
	}
}

// The filters filter. Each one is asserted against a value the graph holds
// more than one of and one it holds none of.
func TestTheGroupingReadsFilter(t *testing.T) {
	cases := []struct{ tool, key, hit string }{
		{"list_entities_by_type", "entity_type", "class"},
		{"list_entities_by_layer", "layer", "documentation"},
		{"list_entities_by_source", "source_toolkit", "docs"},
	}
	for _, testCase := range cases {
		t.Run(testCase.tool, func(t *testing.T) {
			h := fixtureHarness(t)
			body, err := h.invoke("inventory", "inventory", testCase.tool,
				read(map[string]any{testCase.key: testCase.hit, "output_format": "json"}))
			if err != nil {
				t.Fatal(err)
			}
			matched, _ := document(t, body)["total"].(float64)
			if matched == 0 || int(matched) == len(run.FixtureEntities) {
				t.Fatalf("%s=%s matched %v of %d entities — that filters nothing",
					testCase.key, testCase.hit, matched, len(run.FixtureEntities))
			}

			miss := fixtureHarness(t)
			body, err = miss.invoke("inventory", "inventory", testCase.tool,
				read(map[string]any{testCase.key: "no-such-value", "output_format": "json"}))
			if err != nil {
				t.Fatal(err)
			}
			if total, _ := document(t, body)["total"].(float64); total != 0 {
				t.Fatalf("%s=no-such-value matched %v entities", testCase.key, total)
			}
		})
	}
}

// A batch read reports what it could not find, rather than returning a shorter
// list the caller has to diff itself.
func TestABatchReadNamesTheIdsItCouldNotFind(t *testing.T) {
	h := fixtureHarness(t)
	body, err := h.invoke("inventory", "inventory", "get_entities_by_ids",
		read(map[string]any{
			"entity_ids":    []any{"code:order-model", "code:missing"},
			"output_format": "json",
		}))
	if err != nil {
		t.Fatal(err)
	}
	decoded := document(t, body)
	found, _ := decoded["entities"].([]any)
	missing, _ := decoded["missing"].([]any)
	if len(found) != 1 || len(missing) != 1 {
		t.Fatalf("found %d, missing %d: %v", len(found), len(missing), decoded)
	}
}

// -- pacing and stop ----------------------------------------------------------

// The progress steps reach the caller in order, and the stop checkpoint sits
// between them: a cancelled invocation returns instead of finishing and
// uploading. Proved by counting the uploads, not by reading a status.
func TestAStoppedIngestionUploadsNothing(t *testing.T) {
	h := newHarness(t, run.FixtureTools(20*time.Millisecond))
	resolved, err := inventory.Toolkits.Resolve("inventory")
	if err != nil {
		t.Fatal(err)
	}
	manager := spi.NewManager(nil, time.Hour, nil)
	manager.Start(context.Background())
	defer manager.Stop()
	ctx := context.Background()
	invocation, err := manager.Submit(ctx, "inventory", "run_ingestion",
		func(ctx context.Context, tc *spi.Context) (map[string]any, error) {
			return h.runner.Invoke(ctx, spi.Invoke{
				Family:  resolved,
				Toolkit: "inventory",
				Tool:    "run_ingestion",
				Request: expandedSource(nil),
			}, tc)
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Cancel(ctx, "inventory", "run_ingestion", invocation.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, err := manager.Poll(ctx, "inventory", "run_ingestion", invocation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if body["status"] == "Completed" {
			t.Fatal("a stopped ingestion ran to completion")
		}
		if body["status"] == "Error" || body["status"] == "Cancelled" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(h.uploads.uploads) != 0 {
		t.Fatalf("a stopped ingestion uploaded %d objects", len(h.uploads.uploads))
	}
}

// -- helpers ------------------------------------------------------------------

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func toString(value any) string {
	text, _ := value.(string)
	return text
}
