package workerdelegation

import (
	"context"
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
