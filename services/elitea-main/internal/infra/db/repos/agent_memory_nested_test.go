package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

type stubNestedBatchSource struct {
	projectID  int64
	nodes      map[[2]int64]nestedMemoryVersionNode
	retired    map[int64]bool
	hidden     map[int64]bool
	batchCalls int
	err        error
}

func (s *stubNestedBatchSource) deps() nestedMemoryTraversalDeps {
	return nestedMemoryTraversalDeps{
		readBatch: func(_ context.Context, projectID int64, pairs [][2]int64) (map[[2]int64]nestedMemoryVersionNode, error) {
			s.batchCalls++
			if s.err != nil {
				return nil, s.err
			}
			if projectID != s.projectID {
				return nil, nil
			}
			out := make(map[[2]int64]nestedMemoryVersionNode, len(pairs))
			for _, p := range pairs {
				if node, ok := s.nodes[p]; ok {
					out[p] = node
				}
			}
			return out, nil
		},
		retiredIDs: func(_ context.Context, _ int64, appIDs []int64) (map[int64]bool, error) {
			out := map[int64]bool{}
			for _, id := range appIDs {
				if s.retired[id] {
					out[id] = true
				}
			}
			return out, nil
		},
		hiddenIDs: func(_ context.Context, _, _ int64, appIDs []int64) (map[int64]bool, error) {
			out := map[int64]bool{}
			for _, id := range appIDs {
				if s.hidden[id] {
					out[id] = true
				}
			}
			return out, nil
		},
	}
}

func TestResolveReachableNestedVersionEnforcesCompositionDepthProjectAndCycles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := &stubNestedBatchSource{
		projectID: 7,
		nodes: map[[2]int64]nestedMemoryVersionNode{
			{20, 200}: {
				ApplicationID: 20, VersionID: 200, AgentType: "agent",
				Meta:  json.RawMessage(`{"memory":{"enabled":true,"write":"auto_attended"}}`),
				Tools: json.RawMessage(`[{"type":"application","settings":{"application_id":30,"application_version_id":300}}]`),
			},
			{30, 300}: {
				ApplicationID: 30, VersionID: 300, AgentType: "agent",
				Meta:  json.RawMessage(`{"memory":{"enabled":true,"write":"off"}}`),
				Tools: json.RawMessage(`[{"type":"application","settings":{"application_id":20,"application_version_id":200}}]`),
			},
			{40, 400}: {
				ApplicationID: 40, VersionID: 400, AgentType: "pipeline",
				Meta:  json.RawMessage(`{"memory":{"enabled":true,"write":"auto"}}`),
				Tools: json.RawMessage(`[{"type":"application","settings":{"application_id":50,"application_version_id":500}}]`),
			},
			{50, 500}: {
				ApplicationID: 50, VersionID: 500, AgentType: "agent",
				Meta: json.RawMessage(`{"memory":{"enabled":true,"write":"auto"}}`),
			},
			{99, 999}: {
				ApplicationID: 99, VersionID: 999, AgentType: "agent",
				Meta: json.RawMessage(`{"memory":{"enabled":true,"write":"auto"}}`),
			},
		},
	}
	rootInput := &runtimev1.AgentExecutionInputV1{
		Application: []byte(`{"id":10,"version_id":100,"version_details":{"tools":[
			{"type":"application","settings":{"application_id":20,"application_version_id":200}},
			{"type":"application","settings":{"application_id":40,"application_version_id":400}},
			{"type":"application","settings":{"project_id":8,"application_id":99,"application_version_id":999}},
			{"type":"application","settings":{"project_id":"7","application_id":99,"application_version_id":999}}
		]}}`),
	}

	// Depth 1 direct child is reachable, uses 1 batched DB read, and returns [root, child].
	source.batchCalls = 0
	meta, chain, err := resolveReachableNestedVersion(ctx, source.deps(), 7, 42, 10, rootInput, storage.AgentMemoryTarget{ApplicationID: 20, VersionID: 200}, 4)
	if err != nil {
		t.Fatalf("depth 1 reachable: %v", err)
	}
	if source.batchCalls != 1 {
		t.Fatalf("FU353-06 batch calls = %d, want 1", source.batchCalls)
	}
	if !slices.Equal(chain, []int64{10, 20}) {
		t.Fatalf("depth 1 chain = %v, want [10 20]", chain)
	}
	settings, err := metaMemorySettings(meta)
	if err != nil || !settings.Enabled || settings.Write != agentmemory.WriteAutoAttended {
		t.Fatalf("depth 1 settings = %+v, %v", settings, err)
	}

	// Depth 2 grandchild through pipeline intermediate hop: allowed as target (agent_type=="agent"),
	// returns full ancestor chain [10, 40, 50] for risk-tier capping (FU353-02), and uses 2 batched reads (FU353-06).
	source.batchCalls = 0
	meta, chain, err = resolveReachableNestedVersion(ctx, source.deps(), 7, 42, 10, rootInput, storage.AgentMemoryTarget{ApplicationID: 50, VersionID: 500}, 2)
	if err != nil {
		t.Fatalf("depth 2 via pipeline reachable: %v", err)
	}
	if source.batchCalls != 2 {
		t.Fatalf("FU353-06 depth-2 batch calls = %d, want 2", source.batchCalls)
	}
	if !slices.Equal(chain, []int64{10, 40, 50}) {
		t.Fatalf("FU353-02 full ancestor chain = %v, want [10 40 50]", chain)
	}
	if settings, err = metaMemorySettings(meta); err != nil || settings.Write != agentmemory.WriteAuto {
		t.Fatalf("depth 2 settings = %+v, %v", settings, err)
	}

	// FU353-03: resolving a pipeline as the memory target fails closed (must be agent_type == "agent").
	if _, _, err := resolveReachableNestedVersion(ctx, source.deps(), 7, 42, 10, rootInput, storage.AgentMemoryTarget{ApplicationID: 40, VersionID: 400}, 4); !errors.Is(err, storage.ErrContentUnauthorized) {
		t.Fatalf("FU353-03 pipeline target: want ErrContentUnauthorized, got %v", err)
	}

	// Depth 2 past maxDepth=1 fails closed.
	if _, _, err := resolveReachableNestedVersion(ctx, source.deps(), 7, 42, 10, rootInput, storage.AgentMemoryTarget{ApplicationID: 30, VersionID: 300}, 1); !errors.Is(err, storage.ErrContentUnauthorized) {
		t.Fatalf("depth 2 past maxDepth=1: want ErrContentUnauthorized, got %v", err)
	}

	// Unreachable same-project target and foreign/string project_id references fail closed.
	for _, target := range []storage.AgentMemoryTarget{
		{ApplicationID: 99, VersionID: 999},
		{ApplicationID: 20, VersionID: 201},
		{ApplicationID: 77, VersionID: 777},
	} {
		if _, _, err := resolveReachableNestedVersion(ctx, source.deps(), 7, 42, 10, rootInput, target, 4); !errors.Is(err, storage.ErrContentUnauthorized) {
			t.Fatalf("target %+v: want ErrContentUnauthorized, got %v", target, err)
		}
	}

	// Ad-hoc root with instructions-only Application JSON reads from input.Tools.
	adhocInput := &runtimev1.AgentExecutionInputV1{
		Application: []byte(`{"instructions":"be concise"}`),
		Tools:       []byte(`[{"type":"application","project_id":7,"settings":{"application_id":20,"application_version_id":200}}]`),
	}
	if _, chain, err := resolveReachableNestedVersion(ctx, source.deps(), 7, 42, 0, adhocInput, storage.AgentMemoryTarget{ApplicationID: 20, VersionID: 200}, 4); err != nil || !slices.Equal(chain, []int64{20}) {
		t.Fatalf("adhoc child reachable = %v, %v", chain, err)
	}

	// FU353-06: visiting more than maxNestedAgentMemoryVersions (64) fails closed.
	var manyTools []map[string]any
	manyNodes := make(map[[2]int64]nestedMemoryVersionNode, maxNestedAgentMemoryVersions+2)
	for i := int64(1); i <= maxNestedAgentMemoryVersions+1; i++ {
		manyTools = append(manyTools, map[string]any{
			"type":     "application",
			"settings": map[string]any{"application_id": i, "application_version_id": i},
		})
		manyNodes[[2]int64{i, i}] = nestedMemoryVersionNode{ApplicationID: i, VersionID: i, AgentType: "agent"}
	}
	rawMany, _ := json.Marshal(manyTools)
	overLimitInput := &runtimev1.AgentExecutionInputV1{
		Application: []byte(fmt.Sprintf(`{"id":1000,"version_id":1000,"version_details":{"tools":%s}}`, rawMany)),
	}
	overSource := &stubNestedBatchSource{projectID: 7, nodes: manyNodes}
	if _, _, err := resolveReachableNestedVersion(ctx, overSource.deps(), 7, 42, 1000, overLimitInput, storage.AgentMemoryTarget{
		ApplicationID: maxNestedAgentMemoryVersions + 1, VersionID: maxNestedAgentMemoryVersions + 1,
	}, 4); !errors.Is(err, storage.ErrContentUnauthorized) {
		t.Fatalf("limit+1 target: want ErrContentUnauthorized, got %v", err)
	}
}

// FU353-02: retired or actor-hidden intermediate hops cannot be traversed to
// reach a grandchild target.
func TestResolveReachableNestedVersionBlocksRetiredAndHiddenIntermediateHops(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nodes := map[[2]int64]nestedMemoryVersionNode{
		{20, 200}: {
			ApplicationID: 20, VersionID: 200, AgentType: "agent",
			Tools: json.RawMessage(`[{"type":"application","settings":{"application_id":30,"application_version_id":300}}]`),
		},
		{30, 300}: {
			ApplicationID: 30, VersionID: 300, AgentType: "agent",
			Meta: json.RawMessage(`{"memory":{"enabled":true,"write":"auto"}}`),
		},
	}
	rootInput := &runtimev1.AgentExecutionInputV1{
		Application: []byte(`{"id":10,"version_id":100,"version_details":{"tools":[{"type":"application","settings":{"application_id":20,"application_version_id":200}}]}}`),
	}
	target := storage.AgentMemoryTarget{ApplicationID: 30, VersionID: 300}

	// Retired intermediate hop blocks target without even reading hop 20's children.
	retiredSrc := &stubNestedBatchSource{projectID: 7, nodes: nodes, retired: map[int64]bool{20: true}}
	if _, _, err := resolveReachableNestedVersion(ctx, retiredSrc.deps(), 7, 42, 10, rootInput, target, 4); !errors.Is(err, storage.ErrContentUnauthorized) {
		t.Fatalf("retired intermediate hop: want ErrContentUnauthorized, got %v", err)
	}
	if retiredSrc.batchCalls != 0 {
		t.Fatalf("retired intermediate hop must not query version details, batchCalls=%d", retiredSrc.batchCalls)
	}

	// Hidden intermediate hop blocks target without reading hop 20's children.
	hiddenSrc := &stubNestedBatchSource{projectID: 7, nodes: nodes, hidden: map[int64]bool{20: true}}
	if _, _, err := resolveReachableNestedVersion(ctx, hiddenSrc.deps(), 7, 42, 10, rootInput, target, 4); !errors.Is(err, storage.ErrContentUnauthorized) {
		t.Fatalf("hidden intermediate hop: want ErrContentUnauthorized, got %v", err)
	}
	if hiddenSrc.batchCalls != 0 {
		t.Fatalf("hidden intermediate hop must not query version details, batchCalls=%d", hiddenSrc.batchCalls)
	}
}

// FU353-01: ad-hoc and saved nested application project_id validation checks
// BOTH top-level and settings keys with canonical positive integer parsing.
func TestNestedApplicationChildrenCanonicalProjectIDParity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want [][2]int64
	}{
		{
			name: "matching top-level and settings canonical int",
			raw:  `[{"type":"application","project_id":7,"settings":{"project_id":7,"application_id":20,"application_version_id":200}}]`,
			want: [][2]int64{{20, 200}},
		},
		{
			name: "top-level foreign project_id rejected even when settings omits it",
			raw:  `[{"type":"application","project_id":8,"settings":{"application_id":20,"application_version_id":200}}]`,
			want: nil,
		},
		{
			name: "top-level and settings mismatch rejected",
			raw:  `[{"type":"application","project_id":7,"settings":{"project_id":8,"application_id":20,"application_version_id":200}}]`,
			want: nil,
		},
		{
			name: "float project_id rejected",
			raw:  `[{"type":"application","project_id":7.0,"settings":{"application_id":20,"application_version_id":200}}]`,
			want: nil,
		},
		{
			name: "string project_id rejected",
			raw:  `[{"type":"application","project_id":"7","settings":{"application_id":20,"application_version_id":200}}]`,
			want: nil,
		},
		{
			name: "whitespace string settings project_id rejected",
			raw:  `[{"type":"application","settings":{"project_id":" 7 ","application_id":20,"application_version_id":200}}]`,
			want: nil,
		},
		{
			name: "zero or negative project_id rejected",
			raw:  `[{"type":"application","project_id":0,"settings":{"application_id":20,"application_version_id":200}},{"type":"application","project_id":-7,"settings":{"application_id":21,"application_version_id":201}}]`,
			want: nil,
		},
		{
			name: "float application_id rejected",
			raw:  `[{"type":"application","settings":{"application_id":20.0,"application_version_id":200}}]`,
			want: nil,
		},
	}
	for _, tc := range cases {
		got := nestedApplicationChildren(json.RawMessage(tc.raw), 7)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// FU353-07: strict actor_id parsing fails closed on malformed non-numeric ids.
func TestParseStrictAgentMemoryActorID(t *testing.T) {
	t.Parallel()
	if id, ok, err := parseStrictAgentMemoryActorID("42"); err != nil || !ok || id != 42 {
		t.Fatalf("valid actor = (%d, %v, %v)", id, ok, err)
	}
	if id, ok, err := parseStrictAgentMemoryActorID(""); err != nil || ok || id != 0 {
		t.Fatalf("empty actor = (%d, %v, %v)", id, ok, err)
	}
	for _, bad := range []string{"0", "-1", "01", " 42 ", "svc-bot", "42abc", "99999999999999999999"} {
		if _, _, err := parseStrictAgentMemoryActorID(bad); err == nil {
			t.Errorf("malformed actor %q must fail closed", bad)
		}
	}
}

// FU353-04 + FU353-07: explicit self-targeting and missing actor fail closed.
func TestValidateNestedMemoryTargetActorRejectsSelfTargetAndMissingActor(t *testing.T) {
	t.Parallel()
	// Explicit self-targeting (root == target) fails closed.
	if err := validateNestedMemoryTargetActor(10, true, storage.AgentMemoryTarget{ApplicationID: 10, VersionID: 100}); !errors.Is(err, storage.ErrContentUnauthorized) {
		t.Fatalf("self-target: want ErrContentUnauthorized, got %v", err)
	}
	// Missing actor on nested target fails closed.
	if err := validateNestedMemoryTargetActor(10, false, storage.AgentMemoryTarget{ApplicationID: 20, VersionID: 200}); !errors.Is(err, storage.ErrContentUnauthorized) {
		t.Fatalf("missing actor: want ErrContentUnauthorized, got %v", err)
	}
	// Distinct nested target with valid actor passes precondition.
	if err := validateNestedMemoryTargetActor(10, true, storage.AgentMemoryTarget{ApplicationID: 20, VersionID: 200}); err != nil {
		t.Fatalf("valid nested target: got %v", err)
	}
	// Ad-hoc root (rootApplicationID == 0) with valid actor passes precondition.
	if err := validateNestedMemoryTargetActor(0, true, storage.AgentMemoryTarget{ApplicationID: 20, VersionID: 200}); err != nil {
		t.Fatalf("adhoc valid nested target: got %v", err)
	}
}

func TestDelegatedReasonPreservesProvenance(t *testing.T) {
	t.Parallel()
	if got := delegatedReasons("pending", 0); !slices.Equal(got, []string{"pending"}) {
		t.Fatalf("root reasons = %v, want [pending]", got)
	}
	if got := delegatedReasons("pending", 11); !slices.Equal(got, []string{"pending", "delegated_from:11"}) {
		t.Fatalf("nested reasons = %v, want [pending delegated_from:11]", got)
	}
	if got := delegatedReasons("", 11); !slices.Equal(got, []string{"delegated_from:11"}) {
		t.Fatalf("empty base nested reasons = %v, want [delegated_from:11]", got)
	}
	if got := delegatedReasons("", 0); len(got) != 0 {
		t.Fatalf("empty root reasons = %v, want []", got)
	}

	delegated := buildAgentMemoryAuditEvent(1, "agent.memory.remembered", 0, 990189, 42, 990188, "active", "exec-1")
	delegated.ID = "11111111-1111-4111-8111-111111111111"
	delegated.Source = "elitea-main/test"
	if err := delegated.Validate(); err != nil {
		t.Fatalf("delegated audit event invalid: %v", err)
	}
	if !slices.Equal(delegated.Reasons, []string{"active", "delegated_from:990188"}) {
		t.Fatalf("delegated reasons = %v", delegated.Reasons)
	}
	if !slices.Equal(delegated.ActorChain, []string{"agent:990188"}) {
		t.Fatalf("delegated actor chain = %v, want [agent:990188]", delegated.ActorChain)
	}

	rootEvent := buildAgentMemoryAuditEvent(1, "agent.memory.remembered", 0, 990188, 43, 0, "active", "exec-2")
	rootEvent.ID = "22222222-2222-4222-8222-222222222222"
	rootEvent.Source = "elitea-main/test"
	if err := rootEvent.Validate(); err != nil {
		t.Fatalf("root audit event invalid: %v", err)
	}
	if len(rootEvent.ActorChain) != 0 {
		t.Fatalf("root actor chain = %v, want empty", rootEvent.ActorChain)
	}
}

func TestDirectAgentMemoryTargetTypeAndReachabilityOpenAI(t *testing.T) {
	t.Parallel()
	for _, allowed := range []string{"agent", "openai"} {
		if !isDirectAgentMemoryTargetType(allowed) {
			t.Errorf("isDirectAgentMemoryTargetType(%q) = false, want true", allowed)
		}
	}
	for _, refused := range []string{"pipeline", "react", "dial", "", "custom"} {
		if isDirectAgentMemoryTargetType(refused) {
			t.Errorf("isDirectAgentMemoryTargetType(%q) = true, want false", refused)
		}
	}

	ctx := context.Background()
	source := &stubNestedBatchSource{
		projectID: 7,
		nodes: map[[2]int64]nestedMemoryVersionNode{
			{20, 200}: {
				ApplicationID: 20, VersionID: 200, AgentType: "openai",
				Meta: json.RawMessage(`{"memory":{"enabled":true,"write":"auto_attended"}}`),
			},
			{40, 400}: {
				ApplicationID: 40, VersionID: 400, AgentType: "pipeline",
				Meta: json.RawMessage(`{"memory":{"enabled":true,"write":"auto"}}`),
			},
			{41, 401}: {
				ApplicationID: 41, VersionID: 401, AgentType: "react",
				Meta: json.RawMessage(`{"memory":{"enabled":true,"write":"auto"}}`),
			},
		},
	}
	rootInput := &runtimev1.AgentExecutionInputV1{
		Application: []byte(`{"id":10,"version_id":100,"version_details":{"tools":[
			{"type":"application","settings":{"application_id":20,"application_version_id":200}},
			{"type":"application","settings":{"application_id":40,"application_version_id":400}},
			{"type":"application","settings":{"application_id":41,"application_version_id":401}}
		]}}`),
	}
	if _, chain, err := resolveReachableNestedVersion(ctx, source.deps(), 7, 42, 10, rootInput, storage.AgentMemoryTarget{ApplicationID: 20, VersionID: 200}, 4); err != nil || !slices.Equal(chain, []int64{10, 20}) {
		t.Fatalf("stored openai direct agent target = (%v, %v), want [10 20], nil", chain, err)
	}
	for _, nonAgent := range []storage.AgentMemoryTarget{{ApplicationID: 40, VersionID: 400}, {ApplicationID: 41, VersionID: 401}} {
		if _, _, err := resolveReachableNestedVersion(ctx, source.deps(), 7, 42, 10, rootInput, nonAgent, 4); !errors.Is(err, storage.ErrContentUnauthorized) {
			t.Fatalf("non-direct target %+v: want ErrContentUnauthorized, got %v", nonAgent, err)
		}
	}
}
