package repos

// Real-PostgreSQL evidence on Main's production registration path for a Code
// node inside a saved pipeline (W3-2b).
//
// Boundary crossed: the production CodeIntentRepository configuration (as in
// code_registration_real_path_postgres_integration_test.go: the shared store,
// checkRecoveryPermission over seeded RBAC, the captured root source and
// CodeDefinitionSourceAdapter), the real captures written by Main's own
// producer (CaptureFrozenSavedChildVersion, the method the nested version
// route calls when the Worker resolves a saved version under its claim), and
// the real visit and intent statements. Only the Worker side is played by the
// test: it sends the visit and intent requests the Worker sends for a Code
// node of a saved pipeline. Requires ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	httpapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/httpaction"
	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	scope "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	savedChildApplication = 23
	savedChildVersion     = 29
	otherChildApplication = 31
	otherChildVersion     = 37
	// An application the root selects but Main never captured.
	uncapturedApplication = 41
	uncapturedVersion     = 43
	// The root pipeline: Agent nodes run saved pipelines at the root
	// (run_saved and run_again, the same saved pipeline twice), as a Map
	// worker (map_saved) and as Parallel branches (branch_saved, branch_other).
	savedChildRootInstructions = "entry_point: run_saved\nstate:\n  entity: str\n  out: str\n  rows: list\n  mapped: list\n  joined: list\nnodes:\n" +
		"  - id: run_saved\n    type: agent\n    tool: saved_etl\n    transition: run_again\n" +
		"  - id: run_again\n    type: agent\n    tool: saved_etl\n    transition: each\n" +
		"  - id: each\n    type: map\n    worker: map_saved\n    source: rows\n    item: entity\n    index: item_index\n    outputs: [out]\n    destination: mapped\n    max_items: 8\n    max_concurrency: 4\n    reduction: ordered_collection\n    transition: gather\n" +
		"  - id: map_saved\n    type: agent\n    tool: saved_etl\n" +
		"  - id: gather\n    type: parallel\n    branches: [{id: left, node: branch_saved}, {id: right, node: branch_other}]\n    max_concurrency: 2\n    wait: all\n    output: [joined]\n    transition: run_uncaptured\n" +
		"  - id: branch_saved\n    type: agent\n    tool: saved_etl\n" +
		"  - id: branch_other\n    type: agent\n    tool: other_etl\n" +
		"  - id: run_uncaptured\n    type: agent\n    tool: uncaptured_etl\n    transition: END\n"
	// The saved pipeline (23, 29): validate (Code) → route_quality (Router)
	// → normalize (Code), the ETL silver_partition shape.
	savedChildInstructions = "entry_point: validate\nstate:\n  entity: str\n  out: str\nnodes:\n" +
		"  - id: validate\n    type: code\n    language: python\n    code: '1'\n    input: [entity]\n    output: [out]\n    transition: route_quality\n" +
		"  - id: route_quality\n    type: router\n    condition: out\n    routes: [normalize]\n    default_output: END\n" +
		"  - id: normalize\n    type: code\n    language: python\n    code: '2'\n    input: [out]\n    output: [out]\n    transition: END\n"
	// The other saved pipeline (31, 37): one Code node only it declares.
	otherChildInstructions = "entry_point: shape\nstate:\n  entity: str\n  out: str\nnodes:\n" +
		"  - id: shape\n    type: code\n    language: python\n    code: '3'\n    input: [entity]\n    output: [out]\n    transition: END\n"
	savedChildCodeCfg = `{"id":"validate","type":"code","language":"python","code":{"type":"fixed","value":"1"},"input":["entity"],"output":["out"],"structured_output":false,"debug":false,"transition":"route_quality"}`
	otherChildCodeCfg = `{"id":"shape","type":"code","language":"python","code":{"type":"fixed","value":"3"},"input":["entity"],"output":["out"],"structured_output":false,"debug":false,"transition":"END"}`
)

// savedChildRootTools is the root version's tool list: each Agent node's
// alias selects a saved pipeline application at its version.
func savedChildRootTools() []map[string]any {
	tool := func(alias string, application, version uint64) map[string]any {
		return map[string]any{"type": "application", "toolkit_name": alias, "settings": map[string]any{"application_id": application, "application_version_id": version}}
	}
	return []map[string]any{
		tool("saved_etl", savedChildApplication, savedChildVersion),
		tool("other_etl", otherChildApplication, otherChildVersion),
		tool("uncaptured_etl", uncapturedApplication, uncapturedVersion),
	}
}

// savedChildWorld is a running root whose Agent nodes run the saved
// pipelines (23, 29) and (31, 37), each captured by Main at freeze.
type savedChildWorld struct {
	*realPathWorld
	project     int64
	digest      string
	otherDigest string
}

func newSavedChildWorld(t *testing.T) *savedChildWorld {
	t.Helper()
	r := newRealPathWorldWithVersion(t, effectsParent, effectsClaim2, 3, savedChildRootInstructions, savedChildRootTools())
	var project int64
	if err := r.w.pool.QueryRow(t.Context(), `SELECT resource_project_id FROM elitea_runtime.execution_jobs WHERE execution_id=$1`, effectsParent).Scan(&project); err != nil {
		t.Fatal(err)
	}
	world := &savedChildWorld{realPathWorld: r, project: project}
	world.digest = world.capture(t, world.claim, savedChildApplication, savedChildVersion, savedChildInstructions)
	world.otherDigest = world.capture(t, world.claim, otherChildApplication, otherChildVersion, otherChildInstructions)
	return world
}

// capture runs Main's own freeze producer for one saved version, as the
// nested version route does when the Worker resolves it under claim.
func (s *savedChildWorld) capture(t *testing.T, claim storage.ContentClaim, application, version uint64, instructions string) string {
	t.Helper()
	captures, err := NewExecutionChildScopesRepository(s.w.pool, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := json.Marshal(map[string]any{"instructions": instructions, "agent_type": "pipeline"})
	frozen, _ := json.Marshal(map[string]any{"instructions": instructions, "agent_type": "pipeline", "frozen": true})
	digest := scope.FrozenDefinitionDigest(s.project, application, version, frozen)
	if err := captures.CaptureFrozenSavedChildVersion(t.Context(), claim, application, version, digest, frozen, source); err != nil {
		t.Fatalf("capture: %v", err)
	}
	return digest
}

// rootScope is the owner scope the Worker names for the saved pipeline its
// root Agent node agent runs.
func (s *savedChildWorld) rootScope(agent string) map[string]any {
	return map[string]any{"schema": storage.SavedCodeChildScopeSchema, "application_id": savedChildApplication, "version_id": savedChildVersion, "definition_sha256": s.digest,
		"agent_node_id": agent, "fanout": nil, "parent_thread_id": s.root, "child_thread_id": s.root + "/" + agent, "code_node_id": "validate"}
}

// mapScope is the scope of Map item ordinal of count, whose worker
// map_saved runs the saved pipeline.
func (s *savedChildWorld) mapScope(ordinal, count int) map[string]any {
	parent := fmt.Sprintf("m1:item-%d", ordinal)
	return map[string]any{"schema": storage.SavedCodeChildScopeSchema, "application_id": savedChildApplication, "version_id": savedChildVersion, "definition_sha256": s.digest,
		"agent_node_id": "map_saved", "fanout": map[string]any{"kind": "map_item", "node_id": "each", "ordinal": ordinal, "item_count": count},
		"parent_thread_id": parent, "child_thread_id": parent + "/map_saved", "code_node_id": "validate"}
}

// branchScope is the scope of Parallel branch "left", whose branch_saved
// runs the saved pipeline.
func (s *savedChildWorld) branchScope() map[string]any {
	return map[string]any{"schema": storage.SavedCodeChildScopeSchema, "application_id": savedChildApplication, "version_id": savedChildVersion, "definition_sha256": s.digest,
		"agent_node_id": "branch_saved", "fanout": map[string]any{"kind": "parallel_branch", "node_id": "gather", "ordinal": 0, "item_count": 2},
		"parent_thread_id": "p1:left", "child_thread_id": "p1:left/branch_saved", "code_node_id": "validate"}
}

func marshalScope(t *testing.T, value map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// savedCodeRequest is one Code node of a saved pipeline: its node id, its
// pipeline's instructions and its configuration.
type savedCodeRequest struct{ node, instructions, configuration string }

var (
	validateCode = savedCodeRequest{"validate", savedChildInstructions, savedChildCodeCfg}
	shapeCode    = savedCodeRequest{"shape", otherChildInstructions, otherChildCodeCfg}
)

// codeVisit is the visit request for Code node c on thread, carrying
// childScope, under activation.
func (s *savedChildWorld) codeVisit(t *testing.T, activation int, thread string, c savedCodeRequest, childScope json.RawMessage) (code.VisitRequest, []byte) {
	t.Helper()
	yamlSHA := code.Digest([]byte(c.instructions))
	policy, err := storage.OriginalSavedCodePolicy(c.instructions, yamlSHA, c.node)
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := storage.MatchOriginalSavedCodeConfiguration(policy, c.configuration)
	if err != nil {
		t.Fatal(err)
	}
	prepared := []byte(fmt.Sprintf(`{"revision":1,"language":"python","source":"%s","input":{"entity":"row-%d"},"image_digest":"sha256:%s","policy_revision":"code-v1","timeout_seconds":10}`, map[string]string{"validate": "1", "shape": "3"}[c.node], activation, strings.Repeat("9", 64)))
	return code.VisitRequest{Schema: "elitea.sandbox.original-code-visit-request.v1", ActivationID: fmt.Sprintf("%064x", 0xe000+activation), NodeID: c.node, GraphThread: thread, Step: 1, Attempt: 1,
		NodeDigest: hex.EncodeToString(declaration.ConfigurationDigest[:]), OwningYAMLSHA256: yamlSHA,
		ConfigurationBase64URL: base64.RawURLEncoding.EncodeToString([]byte(c.configuration)), PreWorkspacePreparedBase64URL: base64.RawURLEncoding.EncodeToString(prepared), SavedChildScope: childScope}, prepared
}

// registerValidate registers the visit and then the intent of "validate"
// under scope, as the Worker does before submission; it returns the visit.
func (s *savedChildWorld) registerValidate(ctx context.Context, t *testing.T, activation int, value map[string]any) (code.VisitResponse, error) {
	request, prepared := s.codeVisit(t, activation, value["child_thread_id"].(string), validateCode, marshalScope(t, value))
	visit, err := s.repo.RegisterOriginalCodeVisit(ctx, s.claim, request)
	if err != nil {
		return code.VisitResponse{}, fmt.Errorf("visit: %w", err)
	}
	policy, _ := storage.OriginalSavedCodePolicy(savedChildInstructions, code.Digest([]byte(savedChildInstructions)), "validate")
	declaration, _ := storage.MatchOriginalSavedCodeConfiguration(policy, savedChildCodeCfg)
	metadata, err := storage.MatchCodePrepared(declaration, []byte(savedChildCodeCfg), prepared)
	if err != nil {
		return code.VisitResponse{}, err
	}
	intent := code.IntentRequest{Schema: "elitea.sandbox.original-code-intent-request.v1", OriginalVisit: visit.OriginalVisit, DispatchActivation: fmt.Sprintf("%064x", 0xf000+activation),
		RequestDigest: metadata.Fingerprint, SupervisorAudience: realPathAudience, PreparedBase64URL: base64.RawURLEncoding.EncodeToString(prepared)}
	if _, err := s.repo.RegisterOriginalCodeIntent(ctx, s.claim, intent); err != nil {
		return code.VisitResponse{}, fmt.Errorf("intent: %w", err)
	}
	return visit, nil
}

func refusedRegistration(err error) bool {
	return errors.Is(err, code.ErrRejected) || errors.Is(err, storage.ErrContentUnauthorized)
}

// registrationStage is the step Main's fault line names for err.
func registrationStage(err error) string {
	var labelled *codeRegistrationStage
	if errors.As(err, &labelled) {
		return labelled.stage
	}
	return ""
}

// Q-b1, then D-b1: a Code node inside a saved pipeline that the root's Agent
// node runs (outside any fan-out) registers its visit and intent on Main's
// production path once the visit names its saved-child owner scope. The
// shape the Worker sent before W3-2b (no scope, on the saved pipeline's own
// subgraph thread) stays refused: on 36788446f this test sent only that
// shape and failed with "source: original Code authority denied".
func TestSavedChildCodeAtTheRootRegistersOnTheProductionPathPostgres(t *testing.T) {
	s := newSavedChildWorld(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	unscoped, _ := s.codeVisit(t, 1, s.root+"/run_saved", validateCode, json.RawMessage(`null`))
	if _, err := s.repo.RegisterOriginalCodeVisit(ctx, s.claim, unscoped); !refusedRegistration(err) {
		t.Fatalf("an unscoped saved child's Code visit was %v, want a refusal", err)
	}
	first, err := s.registerValidate(ctx, t, 1, s.rootScope("run_saved"))
	if err != nil {
		t.Fatalf("the saved child's Code was refused: %v", err)
	}
	// B3: the same saved pipeline used twice in one run; each use's thread
	// is bound to its own visit.
	second, err := s.registerValidate(ctx, t, 2, s.rootScope("run_again"))
	if err != nil {
		t.Fatalf("the second use of the saved pipeline was refused: %v", err)
	}
	if first.OriginalVisit.VisitID == second.OriginalVisit.VisitID {
		t.Fatal("two uses of one saved pipeline share a visit")
	}
	crossed := s.rootScope("run_again")
	request, _ := s.codeVisit(t, 3, s.root+"/run_saved", validateCode, marshalScope(t, crossed))
	if _, err := s.repo.RegisterOriginalCodeVisit(ctx, s.claim, request); !refusedRegistration(err) || registrationStage(err) != "saved_child_scope" {
		t.Fatalf("a visit naming the other use's thread was %v, want a saved_child_scope refusal", err)
	}
	// A replay is idempotent: the same visit, no second record.
	again, err := s.registerValidate(ctx, t, 1, s.rootScope("run_saved"))
	if err != nil || again.OriginalVisit != first.OriginalVisit {
		t.Fatalf("the replay did not re-attach to its visit: %v", err)
	}
	if visits, intents := s.count(t, "original_code_visits"), s.count(t, "original_code_intents"); visits != 2 || intents != 2 {
		t.Fatalf("visits=%d intents=%d, want 2 each", visits, intents)
	}
	waitNoAdvisoryLocks(t, s.w)
}

// Depth 2: the saved pipeline as a Map worker and as a Parallel branch of a
// root fan-out node, Code inside it.
func TestSavedChildCodeInAFanoutMemberRegistersOnTheProductionPathPostgres(t *testing.T) {
	s := newSavedChildWorld(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for item := range 4 {
		if _, err := s.registerValidate(ctx, t, 10+item, s.mapScope(item, 4)); err != nil {
			t.Fatalf("Map item %d: %v", item, err)
		}
	}
	if _, err := s.registerValidate(ctx, t, 20, s.branchScope()); err != nil {
		t.Fatalf("Parallel branch: %v", err)
	}
	// The other branch runs the other captured saved pipeline.
	other := s.branchScope()
	other["application_id"], other["version_id"], other["definition_sha256"] = otherChildApplication, otherChildVersion, s.otherDigest
	other["agent_node_id"], other["child_thread_id"], other["parent_thread_id"], other["code_node_id"] = "branch_other", "p1:right/branch_other", "p1:right", "shape"
	other["fanout"] = map[string]any{"kind": "parallel_branch", "node_id": "gather", "ordinal": 1, "item_count": 2}
	request, _ := s.codeVisit(t, 21, "p1:right/branch_other", shapeCode, marshalScope(t, other))
	if _, err := s.repo.RegisterOriginalCodeVisit(ctx, s.claim, request); err != nil {
		t.Fatalf("the other branch: %v", err)
	}
	if visits := s.count(t, "original_code_visits"); visits != 6 {
		t.Fatalf("visits=%d, want 6", visits)
	}
	waitNoAdvisoryLocks(t, s.w)
}

// B2, B3, B4: every field of the scope is bound to Main's own state; anything
// else is refused fail closed, at its named stage, before any visit row.
func TestSavedChildCodeScopeFailsClosedOnTheProductionPathPostgres(t *testing.T) {
	s := newSavedChildWorld(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	with := func(base map[string]any, patch map[string]any) map[string]any {
		out := maps.Clone(base)
		maps.Copy(out, patch)
		return out
	}
	root := s.rootScope("run_saved")
	item := s.mapScope(1, 4)
	// Another run's root thread: the same project, another conversation.
	otherRoot := httpapp.RootGraphThread(fmt.Sprint(s.project), s.project, s.project, "other-thread")
	fanout := func(patch map[string]any) map[string]any {
		out := map[string]any{"kind": "map_item", "node_id": "each", "ordinal": 1, "item_count": 4}
		maps.Copy(out, patch)
		return out
	}
	cases := []struct {
		name   string
		scope  map[string]any
		thread string // the visit's thread; the scope's child thread when empty
		code   savedCodeRequest
		stage  string
	}{
		{"a version other than the selected and captured one", with(root, map[string]any{"version_id": savedChildVersion + 1}), "", validateCode, "saved_child_edge"},
		{"a digest other than the captured one", with(root, map[string]any{"definition_sha256": strings.Repeat("a", 64)}), "", validateCode, "saved_child_capture"},
		{"an application the root selects but Main never captured for this execution", with(root, map[string]any{"application_id": uncapturedApplication, "version_id": uncapturedVersion, "agent_node_id": "run_uncaptured", "child_thread_id": s.root + "/run_uncaptured"}), "", validateCode, "saved_child_capture"},
		{"an application the Agent node does not select", with(root, map[string]any{"application_id": otherChildApplication, "version_id": otherChildVersion, "definition_sha256": s.otherDigest}), "", validateCode, "saved_child_edge"},
		{"a Code node declared in a different captured application than the scope names", with(root, map[string]any{"code_node_id": "shape"}), "", shapeCode, "saved_child_code"},
		{"a node of the saved pipeline that is not Code", with(root, map[string]any{"code_node_id": "route_quality"}), "", validateCode, "saved_child_code"},
		{"a visit node differing from the scope's Code node", with(root, map[string]any{"code_node_id": "normalize"}), "", validateCode, "saved_child_scope"},
		{"a visit thread differing from the scope's child thread", root, s.root + "/run_again", validateCode, "saved_child_scope"},
		{"a parent thread that is not this run's root", with(root, map[string]any{"parent_thread_id": "another-root", "child_thread_id": "another-root/run_saved"}), "", validateCode, "saved_child_parent"},
		{"a scope from another execution", with(root, map[string]any{"parent_thread_id": otherRoot, "child_thread_id": otherRoot + "/run_saved"}), "", validateCode, "saved_child_parent"},
		{"a child thread that is not the Agent node's subgraph thread", with(root, map[string]any{"child_thread_id": s.root + "/run_saved/deeper"}), "", validateCode, "saved_child_thread"},
		{"a root-thread child", with(root, map[string]any{"child_thread_id": s.root}), "", validateCode, "saved_child_thread"},
		{"an Agent node a Map owns, named at the root", with(root, map[string]any{"agent_node_id": "map_saved", "child_thread_id": s.root + "/map_saved"}), "", validateCode, "saved_child_edge"},
		{"a nested saved child (an Agent node of the saved pipeline, not the root)", with(root, map[string]any{"agent_node_id": "route_quality", "child_thread_id": s.root + "/route_quality"}), "", validateCode, "saved_child_edge"},
		{"a Map index outside the frozen count", with(item, map[string]any{"fanout": fanout(map[string]any{"ordinal": 4})}), "", validateCode, "saved_child_edge"},
		{"a frozen count above max_items", with(item, map[string]any{"fanout": fanout(map[string]any{"item_count": 9})}), "", validateCode, "saved_child_edge"},
		{"a forged family for a Map node", with(item, map[string]any{"fanout": fanout(map[string]any{"kind": "parallel_branch"}), "parent_thread_id": "p1:item-1", "child_thread_id": "p1:item-1/map_saved"}), "", validateCode, "saved_child_edge"},
		{"a forged family kind", with(item, map[string]any{"fanout": fanout(map[string]any{"kind": "agent_graph"})}), "", validateCode, "saved_child_parent"},
		{"a fan-out member on the root thread", with(item, map[string]any{"parent_thread_id": s.root, "child_thread_id": s.root + "/map_saved"}), "", validateCode, "saved_child_parent"},
		{"a fan-out member of the other family", with(item, map[string]any{"parent_thread_id": "p1:item-1", "child_thread_id": "p1:item-1/map_saved"}), "", validateCode, "saved_child_parent"},
		{"a Parallel branch at the wrong ordinal", with(s.branchScope(), map[string]any{"fanout": map[string]any{"kind": "parallel_branch", "node_id": "gather", "ordinal": 1, "item_count": 2}}), "", validateCode, "saved_child_edge"},
		{"a Parallel branch count that is not its declared count", with(s.branchScope(), map[string]any{"fanout": map[string]any{"kind": "parallel_branch", "node_id": "gather", "ordinal": 0, "item_count": 3}}), "", validateCode, "saved_child_edge"},
		{"a root Agent node named as a fan-out member", with(item, map[string]any{"agent_node_id": "run_saved", "child_thread_id": "m1:item-1/run_saved"}), "", validateCode, "saved_child_edge"},
		{"an unknown scope field", with(root, map[string]any{"project_id": 1}), "", validateCode, "source"},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			thread := tc.thread
			if thread == "" {
				thread, _ = tc.scope["child_thread_id"].(string)
			}
			request, _ := s.codeVisit(t, 100+index, thread, tc.code, marshalScope(t, tc.scope))
			if node := tc.scope["code_node_id"]; node == "route_quality" {
				// The Router's id on a Code visit: the declaration lookup refuses it.
				request.NodeID = "route_quality"
			}
			_, err := s.repo.RegisterOriginalCodeVisit(ctx, s.claim, request)
			if !refusedRegistration(err) {
				t.Fatalf("got %v, want a refusal", err)
			}
			if stage := registrationStage(err); stage != tc.stage {
				t.Fatalf("refused at stage %q (%v), want %q", stage, err, tc.stage)
			}
		})
	}
	if visits := s.count(t, "original_code_visits"); visits != 0 {
		t.Fatalf("a refused scope wrote %d visits", visits)
	}
	waitNoAdvisoryLocks(t, s.w)
}

// B3: a capture from another generation of this execution, and one of
// another execution in the same project, prove nothing for this claim; a
// captured definition whose bytes no longer hash to its digest is refused.
func TestSavedChildCodeNeedsThisClaimsOwnCapturePostgres(t *testing.T) {
	t.Run("another generation", func(t *testing.T) {
		s := newSavedChildWorld(t)
		s.w.exec(`INSERT INTO elitea_runtime.execution_jobs (execution_id, generation, command_id, tenant_id, resource_project_id, projection_project_id, actor_id, principal_ref, capability_id, capability_version, input_bundle_id, request_digest, idempotency_scope, idempotency_key, state, desired_state, invocation_state, admitted_at)
SELECT execution_id, 2, command_id||'-2', tenant_id, resource_project_id, projection_project_id, actor_id, principal_ref, capability_id, capability_version, input_bundle_id, request_digest, idempotency_scope, idempotency_key||'-2', 'FAILED', 'RUNNING', invocation_state, admitted_at FROM elitea_runtime.execution_jobs WHERE execution_id=$1 AND generation=1`, effectsParent)
		s.w.exec(`UPDATE elitea_runtime.execution_saved_child_captures SET generation=2 WHERE execution_id=$1`, effectsParent)
		request, _ := s.codeVisit(t, 1, s.root+"/run_saved", validateCode, marshalScope(t, s.rootScope("run_saved")))
		if _, err := s.repo.RegisterOriginalCodeVisit(t.Context(), s.claim, request); !refusedRegistration(err) || registrationStage(err) != "saved_child_capture" {
			t.Fatalf("got %v, want a saved_child_capture refusal", err)
		}
	})
	t.Run("another execution in the same project", func(t *testing.T) {
		s := newSavedChildWorld(t)
		s.w.exec(`DELETE FROM elitea_runtime.execution_saved_child_captures WHERE execution_id=$1`, effectsParent)
		// The sibling run of the same project, actor and input captured it.
		s.w.exec(`INSERT INTO elitea_runtime.input_bundles (input_bundle_id, immutable_version, media_type, resource_project_id, manifest_digest, manifest_size, manifest_bytes, created_by)
SELECT 'bundle-'||$2, immutable_version, media_type, resource_project_id, manifest_digest, manifest_size, manifest_bytes, created_by FROM elitea_runtime.input_bundles WHERE input_bundle_id='bundle-'||$1`, effectsParent, effectsForeign)
		s.w.exec(`INSERT INTO elitea_runtime.input_bundle_entries (input_bundle_id, entry_id, entry_version, semantic_role, media_type, content_digest, content_size, content_reference, classification, required_grant_audience, content_bytes)
SELECT 'bundle-'||$2, entry_id, entry_version, semantic_role, media_type, content_digest, content_size, content_reference, classification, required_grant_audience, content_bytes FROM elitea_runtime.input_bundle_entries WHERE input_bundle_id='bundle-'||$1`, effectsParent, effectsForeign)
		s.w.exec(`INSERT INTO elitea_runtime.execution_jobs (execution_id, generation, command_id, tenant_id, resource_project_id, projection_project_id, actor_id, principal_ref, capability_id, capability_version, input_bundle_id, request_digest, idempotency_scope, idempotency_key, state, desired_state, invocation_state, admitted_at)
SELECT $2, 1, command_id||'-sibling', tenant_id, resource_project_id, projection_project_id, actor_id, principal_ref, capability_id, capability_version, 'bundle-'||$2, sha256(convert_to($2::text,'UTF8')), idempotency_scope, idempotency_key||'-sibling', state, desired_state, invocation_state, admitted_at FROM elitea_runtime.execution_jobs WHERE execution_id=$1 AND generation=1`, effectsParent, effectsForeign)
		s.w.admit(effectsForeign)
		sibling := s.w.access(effectsForeign, s.project, effectsClaim4, 1, 5)
		if digest := s.capture(t, sibling, savedChildApplication, savedChildVersion, savedChildInstructions); digest != s.digest {
			t.Fatal("the sibling captured another definition")
		}
		request, _ := s.codeVisit(t, 1, s.root+"/run_saved", validateCode, marshalScope(t, s.rootScope("run_saved")))
		if _, err := s.repo.RegisterOriginalCodeVisit(t.Context(), s.claim, request); !refusedRegistration(err) || registrationStage(err) != "saved_child_capture" {
			t.Fatalf("got %v, want a saved_child_capture refusal", err)
		}
	})
	t.Run("captured bytes that no longer hash to the digest", func(t *testing.T) {
		s := newSavedChildWorld(t)
		s.w.exec(`UPDATE elitea_runtime.execution_captured_definitions SET definition_bytes=convert_to('{"instructions":"x"}','UTF8'), byte_length=octet_length(convert_to('{"instructions":"x"}','UTF8')) WHERE resource_project_id=$1 AND application_id=$2 AND definition_sha256=$3`, s.project, savedChildApplication, s.digest)
		request, _ := s.codeVisit(t, 1, s.root+"/run_saved", validateCode, marshalScope(t, s.rootScope("run_saved")))
		if _, err := s.repo.RegisterOriginalCodeVisit(t.Context(), s.claim, request); !refusedRegistration(err) || registrationStage(err) != "saved_child_capture" {
			t.Fatalf("got %v, want a saved_child_capture refusal", err)
		}
	})
}

// B5, Main's half: a Worker registers the saved child's Code of 4 of 8 Map
// items, then is replaced (its claim released, a new claim of the next
// attempt owns the run) and the replacement registers all 8 at once: the 4
// re-attach to the same visits, one visit and one intent per item, nothing
// is refused and nothing stays locked.
func TestATakeoverReRegistersSavedChildCodeOnTheProductionPathPostgres(t *testing.T) {
	const items = 8
	s := newSavedChildWorld(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	first := map[int]string{}
	for item := range 4 {
		visit, err := s.registerValidate(ctx, t, 30+item, s.mapScope(item, items))
		if err != nil {
			t.Fatalf("first Worker, item %d: %v", item, err)
		}
		first[item] = visit.OriginalVisit.VisitID
	}
	s.w.exec(`UPDATE elitea_runtime.execution_claims SET released_at=clock_timestamp() WHERE claim_id=$1`, effectsClaim2)
	s.claim = s.w.access(effectsParent, s.project, effectsClaim3, 2, 4)
	errs := make([]error, items)
	visits := make([]string, items)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for item := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			visit, err := s.registerValidate(ctx, t, 30+item, s.mapScope(item, items))
			errs[item], visits[item] = err, visit.OriginalVisit.VisitID
		}()
	}
	close(start)
	wg.Wait()
	for item, err := range errs {
		if err != nil {
			t.Errorf("replacement Worker, item %d: %v", item, err)
		}
	}
	for item, visit := range first {
		if visits[item] != visit {
			t.Errorf("item %d did not re-attach to its visit", item)
		}
	}
	if visits, intents := s.count(t, "original_code_visits"), s.count(t, "original_code_intents"); visits != items || intents != items {
		t.Fatalf("visits=%d intents=%d, want %d each (one record per item)", visits, intents, items)
	}
	waitNoAdvisoryLocks(t, s.w)
}
