package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	httpapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
)

const savedChildHTTPYAML = `state: {report: dict}
entry_point: send
nodes:
  - id: send
    type: http
    revision: 1
    request:
      method: POST
      url: https://api.example.test/items
      body: {kind: json, value: {n: 184467440737095516170000000000000001}}
      response: {mode: json}
    output: [report]
    transition: END
`

type savedChildScopeCaptureFixture struct {
	definitions map[string]json.RawMessage
	reads       int
}

func (fixture *savedChildScopeCaptureFixture) ReadFrozenSavedChildVersion(_ context.Context, _ ContentClaim, app, version uint64, full string) (json.RawMessage, error) {
	fixture.reads++
	raw, ok := fixture.definitions[full]
	if !ok || scope.FrozenDefinitionDigest(7, app, version, raw) != full {
		return nil, scope.ErrDenied
	}
	return bytes.Clone(raw), nil
}
func childVersionFixture(t *testing.T, app, version uint64, instructions string, tools []any) ([]byte, string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"agent_type": "pipeline", "instructions": instructions, "tools": tools})
	if err != nil {
		t.Fatal(err)
	}
	raw, err = FreezeHTTPChildVersion(int64(app), int64(version), raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw, scope.FrozenDefinitionDigest(7, app, version, raw)
}
func childToolFixture(alias string, app, version uint64) any {
	return map[string]any{"type": "application", "toolkit_name": alias, "settings": map[string]any{"application_id": app, "application_version_id": version}}
}
func childParentFixture(t *testing.T, instructions string, tools []any) scope.OwningDefinition {
	t.Helper()
	raw, full := childVersionFixture(t, 11, 12, instructions, tools)
	return scope.OwningDefinition{ThreadID: "root", ResourceProjectID: 7, ActorID: 9, ApplicationID: 11, VersionID: 12, FrozenDefinitionSHA256: full, Instructions: instructions, PreRedemptionVersion: raw}
}
func childRegistrationFixture(parent scope.OwningDefinition, app, version uint64, full string) scope.Registration {
	return scope.Registration{SchemaVersion: scope.Schema, Family: scope.OriginalFamily{Kind: "agent_native", ParentThreadID: parent.ThreadID, ChildThreadID: "actual/child", NodeID: SavedApplicationCallName(app, version), OriginalInvocationID: "original-turn", OriginalCallID: "call-a", OriginalBatchSHA256: scope.Digest([]byte("batch-a")), OriginalLineageWireB64: "YQ==", Ordinal: 1, InputSHA256: scope.Digest([]byte("arguments")), CatalogSHA256: scope.Digest([]byte("catalog")), AdmittedThreads: []string{"actual/child"}}, Selected: scope.SavedSelector{ApplicationID: app, VersionID: version, FrozenDefinitionSHA256: full}, Purposes: []scope.Purpose{scope.NestedHTTP}, Members: []scope.RequestedMember{{ThreadID: "actual/child", FrozenDefinitionSHA256: full}}}
}

func TestCapturedChildUsesMainFrozenWireAndExactGeneratedSelectedIDs(t *testing.T) {
	child, full := childVersionFixture(t, 31, 41, savedChildHTTPYAML, nil)
	parent := childParentFixture(t, "nodes: []", []any{childToolFixture("editable display name", 31, 41), childToolFixture("same display name", 32, 42)})
	fixture := &savedChildScopeCaptureFixture{definitions: map[string]json.RawMessage{full: child}}
	source, err := NewRuntimeSavedChildCatalogSource(fixture)
	if err != nil {
		t.Fatal(err)
	}
	request := childRegistrationFixture(parent, 31, 41, full)
	captured, err := source.CaptureSavedChildCatalog(context.Background(), ContentClaim{}, HTTPActionInput{ProjectID: 7, ActorID: 9}, parent, request)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.reads != 1 || len(captured.Members) != 1 || len(captured.Members[0].AllowedNodeIDs) != 1 || captured.Members[0].AllowedNodeIDs[0] != "send" || !bytes.Equal(captured.Definitions[full], child) {
		t.Fatal("actual immutable member not captured")
	}
	var version struct {
		Snapshot httpapp.FrozenSnapshot `json:"http_action_snapshot"`
	}
	if json.Unmarshal(child, &version) != nil || len(version.Snapshot.Nodes) != 1 {
		t.Fatal("Main snapshot missing")
	}
	wire, err := base64.StdEncoding.Strict().DecodeString(version.Snapshot.Nodes[0].RequestWireB64)
	if err != nil || !bytes.Contains(wire, []byte("184467440737095516170000000000000001")) {
		t.Fatal("large numeric token lost before child materialization")
	}
	request.Family.NodeID = "editable display name"
	if _, err = source.CaptureSavedChildCatalog(context.Background(), ContentClaim{}, HTTPActionInput{ProjectID: 7, ActorID: 9}, parent, request); err == nil {
		t.Fatal("display alias selected native child")
	}
}

func TestCapturedChildRejectsChangedRevisionForeignProjectAndSiblingClosure(t *testing.T) {
	child, full := childVersionFixture(t, 31, 41, savedChildHTTPYAML, nil)
	parent := childParentFixture(t, "nodes: []", []any{childToolFixture("alias", 31, 41)})
	source, _ := NewRuntimeSavedChildCatalogSource(&savedChildScopeCaptureFixture{definitions: map[string]json.RawMessage{full: child}})
	for _, mutate := range []func(*scope.Registration){
		func(r *scope.Registration) { r.Selected.VersionID++ },
		func(r *scope.Registration) {
			r.Members[0].FrozenDefinitionSHA256 = scope.Digest([]byte("edited saved version"))
		},
		func(r *scope.Registration) {
			r.Members[0].ThreadID += "/sibling"
			r.Family.AdmittedThreads = []string{r.Members[0].ThreadID}
		},
		func(r *scope.Registration) {
			r.Members = append(r.Members, scope.RequestedMember{MemberPath: "sibling", ThreadID: "actual/child/sibling", FrozenDefinitionSHA256: full})
			r.Family.AdmittedThreads = append(r.Family.AdmittedThreads, "actual/child/sibling")
		},
	} {
		request := childRegistrationFixture(parent, 31, 41, full)
		mutate(&request)
		if _, err := source.CaptureSavedChildCatalog(context.Background(), ContentClaim{}, HTTPActionInput{ProjectID: 7, ActorID: 9}, parent, request); err == nil {
			t.Fatal("changed frozen child accepted")
		}
	}
	foreign := childParentFixture(t, "nodes: []", []any{map[string]any{"type": "application", "toolkit_name": "alias", "settings": map[string]any{"application_id": 31, "application_version_id": 41, "application_project_id": 8}}})
	if _, err := source.CaptureSavedChildCatalog(context.Background(), ContentClaim{}, HTTPActionInput{ProjectID: 7, ActorID: 9}, foreign, childRegistrationFixture(foreign, 31, 41, full)); err == nil {
		t.Fatal("caller selected foreign resource project")
	}
}

func TestDefinitionClosureDoesNotGrantOwnedMapWorkerBeforeRebasedFamily(t *testing.T) {
	worker, workerFull := childVersionFixture(t, 31, 41, savedChildHTTPYAML, nil)
	mapYAML := `entry_point: each
nodes:
  - {id: each, type: map, worker: worker}
  - {id: worker, type: agent, tool: selected}
`
	selected, selectedFull := childVersionFixture(t, 21, 22, mapYAML, []any{childToolFixture("selected", 31, 41)})
	parent := childParentFixture(t, "nodes: []", []any{childToolFixture("outer", 21, 22)})
	source, _ := NewRuntimeSavedChildCatalogSource(&savedChildScopeCaptureFixture{definitions: map[string]json.RawMessage{selectedFull: selected, workerFull: worker}})
	request := childRegistrationFixture(parent, 21, 22, selectedFull)
	request.Members = append(request.Members, scope.RequestedMember{MemberPath: "worker", ThreadID: "actual/child/worker", FrozenDefinitionSHA256: workerFull})
	request.Family.AdmittedThreads = append(request.Family.AdmittedThreads, "actual/child/worker")
	captured, err := source.CaptureSavedChildCatalog(context.Background(), ContentClaim{}, HTTPActionInput{ProjectID: 7, ActorID: 9}, parent, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range captured.Members {
		if member.MemberPath == "worker" && len(member.AllowedNodeIDs) != 0 {
			t.Fatal("static map worker acquired effect authority")
		}
	}
	owner := scope.OwningDefinition{ThreadID: "actual/child", ResourceProjectID: 7, ActorID: 9, ApplicationID: 21, VersionID: 22, FrozenDefinitionSHA256: selectedFull, PreRedemptionVersion: selected}
	request = scope.Registration{SchemaVersion: scope.Schema, Family: scope.OriginalFamily{Kind: "map_item", ParentThreadID: owner.ThreadID, ChildThreadID: "actual-map-item-ordinal-2", NodeID: "each", OwnedNodeID: "worker", GraphStep: 4, Ordinal: 2, ConfigSHA256: scope.Digest([]byte("map config")), InputSHA256: scope.Digest([]byte("item2")), WorkerSHA256: scope.Digest([]byte("worker")), SourceSHA256: scope.Digest([]byte("source")), CatalogSHA256: scope.Digest([]byte("catalog")), AdmittedThreads: []string{"actual-map-item-ordinal-2", "actual-map-item-ordinal-2/worker"}}, Selected: scope.SavedSelector{ApplicationID: 21, VersionID: 22, FrozenDefinitionSHA256: selectedFull}, Purposes: []scope.Purpose{scope.NestedHTTP}, Members: []scope.RequestedMember{{ThreadID: "actual-map-item-ordinal-2", FrozenDefinitionSHA256: selectedFull}, {MemberPath: "worker", ThreadID: "actual-map-item-ordinal-2/worker", FrozenDefinitionSHA256: workerFull}}}
	captured, err = source.CaptureSavedChildCatalog(context.Background(), ContentClaim{}, HTTPActionInput{ProjectID: 7, ActorID: 9}, owner, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range captured.Members {
		if member.MemberPath == "" && (len(member.AllowedNodeIDs) != 1 || member.AllowedNodeIDs[0] != "worker") {
			t.Fatal("map family admitted sibling root node")
		}
		if member.MemberPath == "worker" && (len(member.AllowedNodeIDs) != 1 || member.AllowedNodeIDs[0] != "send") {
			t.Fatal("actual rebased saved worker HTTP not admitted")
		}
	}
	request.Family.OwnedNodeID = "each"
	if _, err = source.CaptureSavedChildCatalog(context.Background(), ContentClaim{}, HTTPActionInput{ProjectID: 7, ActorID: 9}, owner, request); err == nil {
		t.Fatal("changed owned worker accepted")
	}
}

func TestFreezeChildDiscardsEditableReceiptAndNeverCallsCredentialOwner(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"agent_type": "pipeline", "instructions": savedChildHTTPYAML, "http_action_snapshot": map[string]any{"forged": "caller"}})
	frozen, err := FreezeHTTPChildVersion(31, 41, raw)
	if err != nil || bytes.Contains(frozen, []byte("forged")) {
		t.Fatal("editable receipt survived Main freeze")
	}
	if _, err = FreezeHTTPChildVersion(31, 42, frozen); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(frozen, raw) {
		t.Fatal("HTTP child lacked exact Main binding")
	}
}
