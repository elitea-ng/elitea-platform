package repos

import (
	"encoding/json"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	httpapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/httpaction"
	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	scope "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"google.golang.org/protobuf/proto"
)

// fanoutInstructions holds a Map with a Code worker, a Parallel with a Code
// branch, and a root Code node no fan-out owns.
const fanoutInstructions = "entry_point: each\nstate:\n  items: list\n  mapped: list\n  out: str\n  topic: str\n  joined: list\nnodes:\n" +
	"  - id: each\n    type: map\n    worker: shout\n    source: items\n    item: entity\n    index: item_index\n    outputs: [out]\n    destination: mapped\n    max_items: 8\n    max_concurrency: 2\n    reduction: ordered_collection\n    transition: gather\n" +
	"  - id: shout\n    type: code\n    language: python\n    code: '1'\n    input: [entity, item_index]\n    output: [out]\n" +
	"  - id: gather\n    type: parallel\n    branches: [{id: left, node: left_code}, {id: right, node: right_code}]\n    max_concurrency: 2\n    wait: all\n    output: [joined]\n    transition: lone\n" +
	"  - id: left_code\n    type: code\n    language: python\n    code: '2'\n    input: [topic]\n    output: [out]\n" +
	"  - id: right_code\n    type: code\n    language: javascript\n    code: '3'\n    input: [topic]\n    output: [out]\n" +
	"  - id: lone\n    type: code\n    language: python\n    code: '4'\n    input: [topic]\n    output: [out]\n    transition: END\n"

type fanoutSourceFixture struct {
	source scope.SourceDefinition
	access codeAccess
	root   string
}

func newFanoutSourceFixture(t *testing.T) fanoutSourceFixture {
	t.Helper()
	base := newOriginalCodeFixture(t)
	version, _ := json.Marshal(map[string]any{"instructions": fanoutInstructions, "agent_type": "pipeline"})
	wire, err := scope.NewSourceWire(7, 11, 17, 19, version)
	if err != nil {
		t.Fatal(err)
	}
	source, err := scope.DecodeSourceWire(wire.CanonicalBytes(), version, wire.Reference(), 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	application, _ := json.Marshal(map[string]any{"id": 17, "version_id": 19, "source_definition": source.Reference})
	input, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{ThreadId: proto.String("original-public-thread"), Application: application})
	if err != nil {
		t.Fatal(err)
	}
	access := base.access
	access.input = input
	return fanoutSourceFixture{source: source, access: access, root: httpapp.RootGraphThread(access.tenant, access.project, access.projection, "original-public-thread")}
}

func (f fanoutSourceFixture) scopeJSON(t *testing.T, patch func(map[string]any)) json.RawMessage {
	t.Helper()
	value := map[string]any{"schema": storage.FanoutCodeChildScopeSchema, "kind": "map_item", "node_id": "each", "owned_node_id": "shout", "ordinal": 2, "item_count": 3, "parent_thread_id": f.root, "child_thread_id": "m1:item-two"}
	if patch != nil {
		patch(value)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// C1: a fan-out Code child's owner scope is resolved against the run's own
// frozen root source inside the caller's transaction, and fails closed.
func TestFanoutCodeChildScopeResolvesItsOwnerAndFailsClosed(t *testing.T) {
	f := newFanoutSourceFixture(t)
	type visit struct {
		access codeAccess
		scope  json.RawMessage
		thread string
		node   string
	}
	admitted := map[string]visit{
		"map item": {f.access, f.scopeJSON(t, nil), "m1:item-two", "shout"},
		"parallel branch": {f.access, f.scopeJSON(t, func(v map[string]any) {
			v["kind"], v["node_id"], v["owned_node_id"], v["ordinal"], v["item_count"], v["child_thread_id"] = "parallel_branch", "gather", "right_code", 1, 2, "p1:branch-right"
		}), "p1:branch-right", "right_code"},
		"last map index": {f.access, f.scopeJSON(t, func(v map[string]any) { v["ordinal"], v["item_count"] = 7, 8 }), "m1:item-two", "shout"},
	}
	foreign := f.access
	foreign.project, foreign.projection = 8, 8
	otherRoot := httpapp.RootGraphThread(f.access.tenant, f.access.project, f.access.projection, "another-conversation")
	refused := map[string]visit{
		// A scope minted for another execution names that run's root thread.
		"scope from another execution":       {f.access, f.scopeJSON(t, func(v map[string]any) { v["parent_thread_id"] = otherRoot }), "m1:item-two", "shout"},
		"map index outside the frozen count": {f.access, f.scopeJSON(t, func(v map[string]any) { v["ordinal"] = 3 }), "m1:item-two", "shout"},
		"frozen count above max_items":       {f.access, f.scopeJSON(t, func(v map[string]any) { v["ordinal"], v["item_count"] = 8, 9 }), "m1:item-two", "shout"},
		"empty frozen family":                {f.access, f.scopeJSON(t, func(v map[string]any) { v["ordinal"], v["item_count"] = 0, 0 }), "m1:item-two", "shout"},
		"parallel ordinal outside branches": {f.access, f.scopeJSON(t, func(v map[string]any) {
			v["kind"], v["node_id"], v["owned_node_id"], v["ordinal"], v["item_count"], v["child_thread_id"] = "parallel_branch", "gather", "right_code", 2, 3, "p1:branch-right"
		}), "p1:branch-right", "right_code"},
		"parallel branch at the wrong ordinal": {f.access, f.scopeJSON(t, func(v map[string]any) {
			v["kind"], v["node_id"], v["owned_node_id"], v["ordinal"], v["item_count"], v["child_thread_id"] = "parallel_branch", "gather", "right_code", 0, 2, "p1:branch-right"
		}), "p1:branch-right", "right_code"},
		"foreign project": {foreign, f.scopeJSON(t, func(v map[string]any) {
			v["parent_thread_id"] = httpapp.RootGraphThread(foreign.tenant, foreign.project, foreign.projection, "original-public-thread")
		}), "m1:item-two", "shout"},
		"node id not owned by the scope":    {f.access, f.scopeJSON(t, func(v map[string]any) { v["owned_node_id"] = "lone" }), "m1:item-two", "lone"},
		"visit node differs from the scope": {f.access, f.scopeJSON(t, nil), "m1:item-two", "lone"},
		"owned node is not Code":            {f.access, f.scopeJSON(t, func(v map[string]any) { v["node_id"], v["owned_node_id"] = "gather", "each" }), "m1:item-two", "each"},
		"forged family for a Map node":      {f.access, f.scopeJSON(t, func(v map[string]any) { v["kind"], v["child_thread_id"] = "parallel_branch", "p1:item-two" }), "p1:item-two", "shout"},
		"forged family kind":                {f.access, f.scopeJSON(t, func(v map[string]any) { v["kind"] = "agent_graph" }), "m1:item-two", "shout"},
		"root thread as the child thread":   {f.access, f.scopeJSON(t, func(v map[string]any) { v["child_thread_id"] = f.root }), f.root, "shout"},
		"child thread of the other family":  {f.access, f.scopeJSON(t, func(v map[string]any) { v["child_thread_id"] = "p1:item-two" }), "p1:item-two", "shout"},
		"visit on another thread":           {f.access, f.scopeJSON(t, nil), "m1:item-three", "shout"},
		"unknown scope field":               {f.access, json.RawMessage(strings.Replace(string(f.scopeJSON(t, nil)), `"kind"`, `"extra":1,"kind"`, 1)), "m1:item-two", "shout"},
	}
	run := func(name string, v visit) (scope.SourceDefinition, error) {
		tx := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{f.source.CanonicalWire, []byte(f.source.PreRedemptionVersion)}}}}
		adapter, err := NewCodeDefinitionSourceAdapter(&ExecutionChildScopesRepository{})
		if err != nil {
			t.Fatal(name, err)
		}
		return adapter.readOriginalCodeSource(t.Context(), tx, recoveryClaim().ExecutionID, 1, v.access, v.scope, "code_recovery", v.thread, v.node)
	}
	for name, v := range admitted {
		source, err := run(name, v)
		if err != nil || source.Reference != f.source.Reference || source.Instructions != fanoutInstructions {
			t.Errorf("%s: the owner scope was not resolved: %v", name, err)
		}
	}
	for name, v := range refused {
		if source, err := run(name, v); err == nil || source.Reference != (scope.SourceReference{}) {
			t.Errorf("%s: a forged or foreign scope was admitted", name)
		}
	}
}

// A Map Code worker reads its item (any JSON) and its index; nothing else
// outside its declared inputs, and a Parallel branch's policy is unchanged.
func TestAMapItemCodeWorkerInputPolicyAdmitsOnlyItsItemAndIndex(t *testing.T) {
	yamlSHA := code.Digest([]byte(fanoutInstructions))
	policy, err := storage.OriginalMapItemCodeInputPolicy(fanoutInstructions, yamlSHA, "shout", "each")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{`{"entity":{"k":[1,2]},"item_index":2}`, `{"entity":"a","item_index":0}`, `{"entity":null}`} {
		if err := policy.ValidateInput(json.RawMessage(input)); err != nil {
			t.Errorf("%s: %v", input, err)
		}
	}
	for _, input := range []string{`{"entity":"a","item_index":"two"}`, `{"topic":"not selected"}`, `{"other":1}`} {
		if policy.ValidateInput(json.RawMessage(input)) == nil {
			t.Errorf("%s: admitted", input)
		}
	}
	// The plain policy (root or Parallel) never admits the Map's channels.
	if _, err := storage.OriginalSavedCodeInputPolicy(fanoutInstructions, yamlSHA, "shout"); err == nil {
		t.Error("a Map worker's item and index were admitted without the Map scope")
	}
	if _, err := storage.OriginalMapItemCodeInputPolicy(fanoutInstructions, yamlSHA, "shout", "gather"); err == nil {
		t.Error("another node's channels were admitted as the Map's")
	}
	colliding := strings.Replace(fanoutInstructions, "item: entity", "item: topic", 1)
	if _, err := storage.OriginalMapItemCodeInputPolicy(colliding, code.Digest([]byte(colliding)), "shout", "each"); err == nil {
		t.Error("an item channel shadowing a business root was admitted")
	}
}
