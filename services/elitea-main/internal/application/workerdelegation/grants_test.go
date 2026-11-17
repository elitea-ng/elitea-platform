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

// A run with no saved root (an ad hoc conversation) and a root with no grant
// leave the command as it was.
func TestNoRootOrNoGrantLeavesTheCommandAlone(t *testing.T) {
	t.Parallel()
	source := &grantSourceStub{byCaller: map[int64][]dg.Grant{}}
	adhoc, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"instructions":"x"}`), nil)
	adhoc.WithGrants(source, true)
	if policy, err := adhoc.WorkerDelegationPolicy(context.Background(), agentCommand()); err != nil || policy != nil || source.calls != 0 {
		t.Fatalf("ad hoc: %v %v %d", policy, err, source.calls)
	}
	rooted, _ := resolver(t, platformconfig.DefaultDelegationSettings(), agentInput(t, `{"id":5}`), nil)
	rooted.WithGrants(source, true)
	if policy, err := rooted.WorkerDelegationPolicy(context.Background(), agentCommand()); err != nil || policy != nil {
		t.Fatalf("no grants: %v %v", policy, err)
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
