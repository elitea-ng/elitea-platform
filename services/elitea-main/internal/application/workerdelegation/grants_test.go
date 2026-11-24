package workerdelegation

import (
	"context"
	"errors"
	"slices"
	"testing"

	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type grantSourceStub struct {
	byCaller map[int64][]dg.Grant
	calls    int
}

func (s *grantSourceStub) ConsumableFromCallers(_ context.Context, _ int64, callers []int64) ([]dg.Grant, error) {
	s.calls++
	var out []dg.Grant
	for _, caller := range callers {
		out = append(out, s.byCaller[caller]...)
	}
	return out, nil
}

func approved(id, caller, callee int64, set dg.GrantSet) dg.Grant {
	return dg.Grant{ID: id, Edge: dg.Edge{CallerEntityID: caller, CalleeEntityID: callee, Kind: dg.EdgeAgentTool},
		State: dg.StateApproved, GrantSet: set, Digest: set.Digest()}
}

func git() dg.GrantSet {
	return dg.GrantSet{Toolkits: []dg.ToolkitGrant{{Key: "toolkit:7", Tools: dg.Tools{Mode: dg.ToolsAllExcept}}}}
}

// With the switch off (the default) no grant is read or sent: the command is
// unchanged byte for byte, so delegation behaves as before grants.
func TestTheGrantSwitchIsOffByDefault(t *testing.T) {
	t.Parallel()
	source := &grantSourceStub{byCaller: map[int64][]dg.Grant{5: {approved(1, 5, 6, git())}}}
	r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":5}`), nil)
	r.WithGrants(source, false)
	policy, err := r.WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil || policy != nil || source.calls != 0 {
		t.Fatalf("policy = %v err = %v reads = %d; want nil, nil, 0", policy, err, source.calls)
	}
}

// With it on, the closure of the root is sent: the root's edges, then the
// edges below each callee, once per agent, and a policy is built even when the
// section was never saved.
func TestTheClosureOfTheRootIsSent(t *testing.T) {
	t.Parallel()
	source := &grantSourceStub{byCaller: map[int64][]dg.Grant{
		5: {approved(1, 5, 6, git())},
		6: {approved(2, 6, 7, git()), approved(3, 6, 5, git())}, // 6 -> 5 is a cycle back to the root
		9: {approved(4, 9, 10, git())},                          // not reachable from 5
	}}
	r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":5}`), nil)
	r.WithGrants(source, true)
	policy, err := r.WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil || policy == nil {
		t.Fatalf("policy = %v err = %v", policy, err)
	}
	if !policy.GetGrantsEnforced() || policy.GetRootApplicationId() != 5 {
		t.Fatalf("switch/root: %+v", policy)
	}
	var ids []uint64
	for _, grant := range policy.GetGrants() {
		ids = append(ids, grant.GetGrantId())
		if grant.GetDigest() != git().Digest() || grant.GetGrantSet() != string(git().Canonical()) || grant.GetEdgeKind() != "agent_tool" {
			t.Fatalf("grant message: %+v", grant)
		}
	}
	if len(ids) != 3 || ids[0] != 1 || ids[1] != 2 || ids[2] != 3 {
		t.Fatalf("grant ids = %v, want the edges below 5 and 6 (1, 2, 3), none below 9", ids)
	}
}

// A run with no saved root (an ad hoc conversation) leaves the command as it
// was. A root with no grant still carries its id and the switch, so the worker
// names the calling agent on the edge of a refusal (the chat notice's
// `caller_id`, which Review delegation and its deep links need).
func TestNoRootLeavesTheCommandAloneAndARootWithNoGrantStillNamesItself(t *testing.T) {
	t.Parallel()
	source := &grantSourceStub{byCaller: map[int64][]dg.Grant{}}
	adhoc, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"instructions":"x"}`), nil)
	adhoc.WithGrants(source, true)
	if policy, err := adhoc.WorkerDelegationPolicy(context.Background(), agentCommand()); err != nil || policy != nil || source.calls != 0 {
		t.Fatalf("ad hoc: %v %v %d", policy, err, source.calls)
	}
	rooted, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":5}`), nil)
	rooted.WithGrants(source, true)
	policy, err := rooted.WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil || policy == nil || policy.GetRootApplicationId() != 5 || !policy.GetGrantsEnforced() || len(policy.GetGrants()) != 0 {
		t.Fatalf("a root with no grants must still name itself: %v %v", policy, err)
	}
}

// A saved section keeps its values and gains the grants.
func TestASavedSectionKeepsItsValuesAndGainsTheGrants(t *testing.T) {
	t.Parallel()
	source := &grantSourceStub{byCaller: map[int64][]dg.Grant{5: {approved(1, 5, 6, git())}}}
	r, _ := resolver(t, savedDefaults(), agentInput(t, `{"id":5}`), nil)
	r.WithGrants(source, true)
	policy, err := r.WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil || policy == nil || policy.GetMaxDepth() != 4 || len(policy.GetGrants()) != 1 {
		t.Fatalf("policy = %v err = %v", policy, err)
	}
}

// FU-359: both a pipeline root and an ordinary agent root whose grants are all
// still pending (0 consumable grants) still carry root_application_id and
// grants_enforced, with default unsaved settings or saved settings + principal
// bound, so a refusal notice names caller_id for Review delegation.
func TestPendingOnlyGrantsPopulateRootApplicationIDForPipelineAndOrdinaryRoots(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		application string
		wantRootID  uint64
		settings    platformconfig.DelegationSettings
		withBound   bool
	}{
		{
			name:        "pipeline root with default settings",
			application: `{"id":174,"version_id":1,"version_details":{"agent_type":"pipeline","tools":[]}}`,
			wantRootID:  174,
			settings:    platformconfig.DefaultDelegationSettings(),
		},
		{
			name:        "pipeline root with saved settings and principal bound",
			application: `{"id":174,"version_id":1,"version_details":{"agent_type":"pipeline","tools":[]}}`,
			wantRootID:  174,
			settings:    boundedSettings(),
			withBound:   true,
		},
		{
			name:        "ordinary root with default settings",
			application: `{"id":175,"version_id":2,"version_details":{"agent_type":"agent","tools":[]}}`,
			wantRootID:  175,
			settings:    platformconfig.DefaultDelegationSettings(),
		},
		{
			name:        "ordinary root with saved settings and principal bound",
			application: `{"id":175,"version_id":2,"version_details":{"agent_type":"agent","tools":[]}}`,
			wantRootID:  175,
			settings:    boundedSettings(),
			withBound:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := &grantSourceStub{byCaller: map[int64][]dg.Grant{}}
			r, _ := resolver(t, tc.settings, agentInput(t, tc.application), nil)
			if tc.withBound {
				r.WithPrincipalBound(&principalStub{
					principal: Principal{UserID: 7},
					reachable: []int64{100},
				})
			}
			r.WithGrants(source, true)
			policy, err := r.WorkerDelegationPolicy(context.Background(), agentCommand())
			if err != nil || policy == nil {
				t.Fatalf("policy = %v err = %v", policy, err)
			}
			if !policy.GetGrantsEnforced() || policy.GetRootApplicationId() != tc.wantRootID || len(policy.GetGrants()) != 0 {
				t.Fatalf("got enforced=%v root=%d grants=%d; want true, %d, 0",
					policy.GetGrantsEnforced(), policy.GetRootApplicationId(), len(policy.GetGrants()), tc.wantRootID)
			}
			if source.calls != 1 {
				t.Fatalf("grant reads = %d, want 1", source.calls)
			}
			if tc.withBound && (policy.GetPrincipalBound() == nil || !policy.GetPrincipalBound().GetBounded()) {
				t.Fatalf("principal bound lost: %+v", policy.GetPrincipalBound())
			}
		})
	}
}

// Negative cases: switch off (even with saved settings), non-positive root id,
// or invalid project id must not stamp root_application_id or grants_enforced.
func TestGrantsEnforcementNegativeCasesDoNotPopulateRootApplicationID(t *testing.T) {
	t.Parallel()
	t.Run("switch off with saved settings", func(t *testing.T) {
		t.Parallel()
		source := &grantSourceStub{byCaller: map[int64][]dg.Grant{}}
		r, _ := resolver(t, savedDefaults(), agentInput(t, `{"id":174,"version_details":{"agent_type":"pipeline"}}`), nil)
		r.WithGrants(source, false)
		policy, err := r.WorkerDelegationPolicy(context.Background(), agentCommand())
		if err != nil || policy == nil {
			t.Fatalf("policy = %v err = %v", policy, err)
		}
		if policy.GetGrantsEnforced() || policy.GetRootApplicationId() != 0 || source.calls != 0 {
			t.Fatalf("switch off must leave root/enforced unset: enforced=%v root=%d reads=%d",
				policy.GetGrantsEnforced(), policy.GetRootApplicationId(), source.calls)
		}
	})

	for _, raw := range []string{`{"id":0}`, `{"id":-5}`, `{"version_details":{"agent_type":"pipeline"}}`} {
		t.Run("invalid root "+raw, func(t *testing.T) {
			t.Parallel()
			source := &grantSourceStub{byCaller: map[int64][]dg.Grant{}}
			r, _ := resolver(t, savedDefaults(), agentInput(t, raw), nil)
			r.WithGrants(source, true)
			policy, err := r.WorkerDelegationPolicy(context.Background(), agentCommand())
			if err != nil || policy == nil {
				t.Fatalf("policy = %v err = %v", policy, err)
			}
			if policy.GetGrantsEnforced() || policy.GetRootApplicationId() != 0 || source.calls != 0 {
				t.Fatalf("invalid root must not stamp grants: enforced=%v root=%d reads=%d",
					policy.GetGrantsEnforced(), policy.GetRootApplicationId(), source.calls)
			}
		})
	}

	t.Run("invalid project id", func(t *testing.T) {
		t.Parallel()
		source := &grantSourceStub{byCaller: map[int64][]dg.Grant{}}
		r, _ := resolver(t, savedDefaults(), agentInput(t, `{"id":174}`), nil)
		r.WithGrants(source, true)
		cmd := agentCommand()
		cmd.ResourceProjectId = "not-a-number"
		policy, err := r.WorkerDelegationPolicy(context.Background(), cmd)
		if err != nil || policy == nil {
			t.Fatalf("policy = %v err = %v", policy, err)
		}
		if policy.GetGrantsEnforced() || policy.GetRootApplicationId() != 0 || source.calls != 0 {
			t.Fatalf("invalid project must not stamp grants: enforced=%v root=%d reads=%d",
				policy.GetGrantsEnforced(), policy.GetRootApplicationId(), source.calls)
		}
	})
}

type graphGrantSourceStub struct {
	grantSourceStub
	edgesByProject map[int64]map[int64][]int64
	edgeCalls      int
	edgeErr        error
}

func (s *graphGrantSourceStub) ReachableApplicationCallees(_ context.Context, projectID int64, callers []int64) (map[int64][]int64, error) {
	s.edgeCalls++
	if s.edgeErr != nil {
		return nil, s.edgeErr
	}
	byCaller := s.edgesByProject[projectID]
	out := make(map[int64][]int64, len(callers))
	for _, caller := range callers {
		if callees := byCaller[caller]; len(callees) > 0 {
			out[caller] = slices.Clone(callees)
		}
	}
	return out, nil
}

func collectGrantIDs(t *testing.T, r *Resolver) []uint64 {
	t.Helper()
	policy, err := r.WorkerDelegationPolicy(context.Background(), agentCommand())
	if err != nil || policy == nil {
		t.Fatalf("WorkerDelegationPolicy: policy=%v err=%v", policy, err)
	}
	ids := make([]uint64, 0, len(policy.GetGrants()))
	for _, g := range policy.GetGrants() {
		ids = append(ids, g.GetGrantId())
	}
	return ids
}

// 3-hop closure traversal must not stop when hop 1 (root -> intermediate) or
// hop 2 (intermediate -> sub-intermediate) carries no approved grant of its own.
func TestClosureGrantsWalksThreeHopGraphWithoutHop1Grant(t *testing.T) {
	t.Parallel()

	t.Run("3-hop graph via CompositionEdgeSource auto-wired through WithGrants", func(t *testing.T) {
		t.Parallel()
		// 10 -> 20 (no grant), 20 -> 30 (no grant), 30 -> 40 (approved grant 300),
		// plus unreachable 90 -> 91 (approved grant 900).
		source := &graphGrantSourceStub{
			grantSourceStub: grantSourceStub{
				byCaller: map[int64][]dg.Grant{
					30: {approved(300, 30, 40, git())},
					90: {approved(900, 90, 91, git())},
				},
			},
			edgesByProject: map[int64]map[int64][]int64{
				17: {
					10: {20},
					20: {30},
					30: {40},
				},
			},
		}
		r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":10}`), nil)
		r.WithGrants(source, true)
		ids := collectGrantIDs(t, r)
		if !slices.Equal(ids, []uint64{300}) {
			t.Fatalf("3-hop grant ids = %v, want [300]", ids)
		}
	})

	t.Run("hop 2 and hop 3 both hold approved grants while hop 1 has none", func(t *testing.T) {
		t.Parallel()
		source := &graphGrantSourceStub{
			grantSourceStub: grantSourceStub{
				byCaller: map[int64][]dg.Grant{
					20: {approved(200, 20, 30, git())},
					30: {approved(300, 30, 40, git())},
				},
			},
			edgesByProject: map[int64]map[int64][]int64{
				17: {
					10: {20},
					20: {30},
					30: {40},
				},
			},
		}
		r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":10}`), nil)
		r.WithGrants(source, true)
		ids := collectGrantIDs(t, r)
		if !slices.Equal(ids, []uint64{200, 300}) {
			t.Fatalf("hop2+hop3 grant ids = %v, want [200 300]", ids)
		}
	})

	t.Run("frozen input tools seed hop 1 without DB edge row", func(t *testing.T) {
		t.Parallel()
		source := &graphGrantSourceStub{
			grantSourceStub: grantSourceStub{
				byCaller: map[int64][]dg.Grant{
					30: {approved(301, 30, 40, git())},
				},
			},
			edgesByProject: map[int64]map[int64][]int64{
				17: {
					20: {30},
				},
			},
		}
		input := agentInput(t, `{"id":10,"version_details":{"tools":[{"type":"application","project_id":17,"settings":{"project_id":17,"application_id":20}}]}}`)
		r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), input, nil)
		r.WithGrants(source, true)
		ids := collectGrantIDs(t, r)
		if !slices.Equal(ids, []uint64{301}) {
			t.Fatalf("frozen-input seeded 3-hop grant ids = %v, want [301]", ids)
		}
	})
}

func TestClosureGrantsHandlesCyclesWithoutLoopingOrDuplicates(t *testing.T) {
	t.Parallel()
	// Cycle: 1 -> 2 (no grant), 2 -> 2 (self-loop), 2 -> 3 (no grant), 3 -> 1 (grant 31), 3 -> 2 (grant 32).
	source := &graphGrantSourceStub{
		grantSourceStub: grantSourceStub{
			byCaller: map[int64][]dg.Grant{
				3: {approved(31, 3, 1, git()), approved(32, 3, 2, git())},
			},
		},
		edgesByProject: map[int64]map[int64][]int64{
			17: {
				1: {2},
				2: {2, 3},
				3: {1, 2},
			},
		},
	}
	r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":1}`), nil)
	r.WithGrants(source, true)
	ids := collectGrantIDs(t, r)
	if !slices.Equal(ids, []uint64{31, 32}) {
		t.Fatalf("cycle grant ids = %v, want [31 32]", ids)
	}
	if source.calls > 4 {
		t.Fatalf("cycle traversal did not terminate promptly: grant calls = %d", source.calls)
	}
}

func TestClosureGrantsEnforcesDepthAndNodeBounds(t *testing.T) {
	t.Parallel()

	t.Run("depth bound includes hop MaxGrantClosureDepth and excludes hop MaxGrantClosureDepth+1", func(t *testing.T) {
		t.Parallel()
		edges := map[int64][]int64{}
		// Chain 1 -> 2 -> ... -> 18 (17 hops).
		for node := int64(1); node <= int64(MaxGrantClosureDepth+2); node++ {
			edges[node] = []int64{node + 1}
		}
		// Grant at depth 15 (16th hop: 16 -> 17) must be included;
		// grant at depth 16 (17th hop: 17 -> 18) must be excluded.
		lastAllowedCaller := int64(MaxGrantClosureDepth)
		overDepthCaller := int64(MaxGrantClosureDepth + 1)
		source := &graphGrantSourceStub{
			grantSourceStub: grantSourceStub{
				byCaller: map[int64][]dg.Grant{
					lastAllowedCaller: {approved(1600, lastAllowedCaller, lastAllowedCaller+1, git())},
					overDepthCaller:   {approved(1700, overDepthCaller, overDepthCaller+1, git())},
				},
			},
			edgesByProject: map[int64]map[int64][]int64{17: edges},
		}
		r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":1}`), nil)
		r.WithGrants(source, true)
		ids := collectGrantIDs(t, r)
		if !slices.Equal(ids, []uint64{1600}) {
			t.Fatalf("depth bound grant ids = %v, want [1600]", ids)
		}
	})

	t.Run("node bound caps visited applications at MaxGrantClosureNodes", func(t *testing.T) {
		t.Parallel()
		// Root 1 attaches 2..(MaxGrantClosureNodes + 5).
		// Nodes 2..MaxGrantClosureNodes fit in the 256-node budget (root + 255 callees).
		// Node MaxGrantClosureNodes+1 exceeds the node budget and its outgoing grant is not loaded.
		callees := make([]int64, 0, MaxGrantClosureNodes+4)
		for id := int64(2); id <= int64(MaxGrantClosureNodes+5); id++ {
			callees = append(callees, id)
		}
		insideNode := int64(MaxGrantClosureNodes)
		outsideNode := int64(MaxGrantClosureNodes + 1)
		source := &graphGrantSourceStub{
			grantSourceStub: grantSourceStub{
				byCaller: map[int64][]dg.Grant{
					insideNode:  {approved(7001, insideNode, 9001, git())},
					outsideNode: {approved(7002, outsideNode, 9002, git())},
				},
			},
			edgesByProject: map[int64]map[int64][]int64{
				17: {1: callees},
			},
		}
		r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":1}`), nil)
		r.WithGrants(source, true)
		ids := collectGrantIDs(t, r)
		if !slices.Equal(ids, []uint64{7001}) {
			t.Fatalf("node bound grant ids = %v, want [7001]", ids)
		}
	})
}

func TestClosureGrantsEnforcesForeignProjectBoundaries(t *testing.T) {
	t.Parallel()

	// Root in project 17 references:
	// - foreign project 99 at top level -> ignored
	// - foreign project 99 in settings -> ignored
	// - string "17", float 17.0, zero 0, negative -17 -> ignored
	// - canonical project 17 -> app 20 -> followed
	// Plus grant source returns one foreign-project grant (project 99, 20 -> 88)
	// and one same-project grant (project 17, 20 -> 30).
	foreignGrant := approved(888, 20, 88, git())
	foreignGrant.ProjectID = 99
	sameProjectGrant := approved(333, 20, 30, git())
	sameProjectGrant.ProjectID = 17
	deeperFromForeign := approved(889, 88, 89, git())
	deeperFromForeign.ProjectID = 17

	source := &graphGrantSourceStub{
		grantSourceStub: grantSourceStub{
			byCaller: map[int64][]dg.Grant{
				20: {foreignGrant, sameProjectGrant},
				50: {approved(500, 50, 51, git())},
				52: {approved(502, 52, 53, git())},
				54: {approved(504, 54, 55, git())},
				56: {approved(506, 56, 57, git())},
				58: {approved(508, 58, 59, git())},
				88: {deeperFromForeign},
			},
		},
		edgesByProject: map[int64]map[int64][]int64{
			// Foreign project edges must never be queried for project 17.
			99: {10: {50}},
		},
	}
	input := agentInput(t, `{"id":10,"version_details":{"tools":[
		{"type":"application","project_id":99,"settings":{"application_id":50}},
		{"type":"application","project_id":17,"settings":{"project_id":99,"application_id":52}},
		{"type":"application","project_id":"17","settings":{"application_id":54}},
		{"type":"application","project_id":17.0,"settings":{"application_id":56}},
		{"type":"application","project_id":0,"settings":{"application_id":58}},
		{"type":"application","project_id":17,"settings":{"project_id":17,"application_id":20}}
	]}}`)
	r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), input, nil)
	r.WithGrants(source, true)
	ids := collectGrantIDs(t, r)
	if !slices.Equal(ids, []uint64{333}) {
		t.Fatalf("foreign-project boundary grant ids = %v, want [333]", ids)
	}
}

func TestClosureGrantsEnforcesMaxGrantEdgesAndPropagatesEdgeErrors(t *testing.T) {
	t.Parallel()

	t.Run("truncates deterministically at MaxGrantEdges across multi-hop graph", func(t *testing.T) {
		t.Parallel()
		hop2Grants := make([]dg.Grant, 0, MaxGrantEdges+10)
		for i := int64(1); i <= int64(MaxGrantEdges+10); i++ {
			hop2Grants = append(hop2Grants, approved(1000+i, 20, 2000+i, git()))
		}
		source := &graphGrantSourceStub{
			grantSourceStub: grantSourceStub{
				byCaller: map[int64][]dg.Grant{
					20: hop2Grants,
				},
			},
			edgesByProject: map[int64]map[int64][]int64{
				17: {10: {20}},
			},
		}
		r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":10}`), nil)
		r.WithGrants(source, true)
		ids := collectGrantIDs(t, r)
		if len(ids) != MaxGrantEdges {
			t.Fatalf("len(grants) = %d, want %d", len(ids), MaxGrantEdges)
		}
		if ids[0] != 1001 || ids[MaxGrantEdges-1] != uint64(1000+MaxGrantEdges) {
			t.Fatalf("unexpected truncation boundaries: first=%d last=%d", ids[0], ids[MaxGrantEdges-1])
		}
	})

	t.Run("composition edge read error fails closed", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("edge store down")
		source := &graphGrantSourceStub{edgeErr: boom}
		r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":10}`), nil)
		r.WithGrants(source, true)
		if _, err := r.WorkerDelegationPolicy(context.Background(), agentCommand()); !errors.Is(err, boom) {
			t.Fatalf("edge error = %v, want %v", err, boom)
		}
	})

	t.Run("mixed edge kinds across 3-hop chain and non-consumable states filtered", func(t *testing.T) {
		t.Parallel()
		// 10 -> 20 (ungranted agent_tool), 20 -> 30 (approved pipeline_child),
		// 30 -> 40 (stale pipeline_node), plus non-consumable states (pending,
		// revoked, superseded, empty), non-positive ID, and unsolicited caller
		// 999 -> 1000 which must all be ignored.
		hop2 := dg.Grant{
			ID: 210, ProjectID: 17, State: dg.StateApproved,
			Edge:     dg.Edge{CallerEntityID: 20, CalleeEntityID: 30, Kind: dg.EdgePipelineChild},
			GrantSet: git(), Digest: git().Digest(),
		}
		hop3 := dg.Grant{
			ID: 310, ProjectID: 17, State: dg.StateStale,
			Edge:     dg.Edge{CallerEntityID: 30, CalleeEntityID: 40, Kind: dg.EdgePipelineNode},
			GrantSet: git(), Digest: git().Digest(),
		}
		pending := dg.Grant{
			ID: 211, ProjectID: 17, State: dg.StatePending,
			Edge:     dg.Edge{CallerEntityID: 20, CalleeEntityID: 31, Kind: dg.EdgeAgentTool},
			GrantSet: git(), Digest: git().Digest(),
		}
		revoked := dg.Grant{
			ID: 212, ProjectID: 17, State: dg.StateRevoked,
			Edge:     dg.Edge{CallerEntityID: 20, CalleeEntityID: 32, Kind: dg.EdgeAgentTool},
			GrantSet: git(), Digest: git().Digest(),
		}
		superseded := dg.Grant{
			ID: 213, ProjectID: 17, State: dg.StateSuperseded,
			Edge:     dg.Edge{CallerEntityID: 20, CalleeEntityID: 33, Kind: dg.EdgeAgentTool},
			GrantSet: git(), Digest: git().Digest(),
		}
		zeroID := dg.Grant{
			ID: 0, ProjectID: 17, State: dg.StateApproved,
			Edge:     dg.Edge{CallerEntityID: 20, CalleeEntityID: 34, Kind: dg.EdgeAgentTool},
			GrantSet: git(), Digest: git().Digest(),
		}
		unsolicitedCaller := dg.Grant{
			ID: 999, ProjectID: 17, State: dg.StateApproved,
			Edge:     dg.Edge{CallerEntityID: 999, CalleeEntityID: 1000, Kind: dg.EdgeAgentTool},
			GrantSet: git(), Digest: git().Digest(),
		}
		source := &graphGrantSourceStub{
			grantSourceStub: grantSourceStub{
				byCaller: map[int64][]dg.Grant{
					20: {hop2, pending, revoked, superseded, zeroID, unsolicitedCaller},
					30: {hop3},
					31: {approved(311, 31, 41, git())}, // unreachable because pending 20->31 filtered
					32: {approved(312, 32, 42, git())}, // unreachable because revoked 20->32 filtered
				},
			},
			edgesByProject: map[int64]map[int64][]int64{
				17: {10: {20}},
			},
		}
		r, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":10}`), nil)
		r.WithGrants(source, true)
		policy, err := r.WorkerDelegationPolicy(context.Background(), agentCommand())
		if err != nil || policy == nil {
			t.Fatalf("policy = %v err = %v", policy, err)
		}
		if len(policy.GetGrants()) != 2 {
			t.Fatalf("got %d grants (%+v), want 2", len(policy.GetGrants()), policy.GetGrants())
		}
		if policy.GetGrants()[0].GetGrantId() != 210 || policy.GetGrants()[0].GetEdgeKind() != string(dg.EdgePipelineChild) {
			t.Fatalf("hop2 grant = %+v", policy.GetGrants()[0])
		}
		if policy.GetGrants()[1].GetGrantId() != 310 || policy.GetGrants()[1].GetEdgeKind() != string(dg.EdgePipelineNode) {
			t.Fatalf("hop3 grant = %+v", policy.GetGrants()[1])
		}
	})
}
