package workspaceruntime

import (
	"context"
	"testing"
)

type pullRequestTurns struct{ turn Turn }

func (t pullRequestTurns) ResolveCloudTurn(context.Context, TurnClaim) (Turn, bool, error) {
	return t.turn, true, nil
}
func (pullRequestTurns) HandedOff(context.Context, TurnClaim) (bool, error) { return true, nil }

type pullRequestPolicy Policy

func (p pullRequestPolicy) EffectiveCloudPolicy(context.Context, int64, string) (Policy, error) {
	return Policy(p), nil
}

// ADR-0034 decision 10, amended 2026-10-26: the field says whether
// open_pull_request exists and asks, and offers the thread's credential
// only while pull requests are on.
func TestRuntimeContextServesThePullRequestPolicy(t *testing.T) {
	claim := TurnClaim{ExecutionID: "exec-1", Generation: 1, ProjectID: 42}
	turn := Turn{ThreadUUID: "6f1c2d3e-4a5b-4c6d-8e7f-901234567890", Epoch: 2, ForgeCredential: true}
	for name, tc := range map[string]struct {
		policy         Policy
		wantMode       string
		wantCredential bool
	}{
		"off":   {Policy{Enabled: true}, PullRequestsOff, false},
		"ask":   {Policy{Enabled: true, PullRequests: true, PullRequestApproval: true}, PullRequestsAsk, true},
		"allow": {Policy{Enabled: true, PullRequests: true}, PullRequestsAllow, true},
	} {
		t.Run(name, func(t *testing.T) {
			resolver := NewContextResolver(pullRequestTurns{turn: turn}, pullRequestPolicy(tc.policy))
			field, err := resolver.ResolveWorkspaceContext(context.Background(), claim)
			if err != nil || field == nil || field.PullRequests != tc.wantMode || field.PullRequestCredential != tc.wantCredential {
				t.Fatalf("field = %+v %v", field, err)
			}
		})
	}
	// No credential chosen: none offered, whatever the policy.
	resolver := NewContextResolver(pullRequestTurns{turn: Turn{ThreadUUID: turn.ThreadUUID, Epoch: 2}},
		pullRequestPolicy(Policy{Enabled: true, PullRequests: true}))
	if field, err := resolver.ResolveWorkspaceContext(context.Background(), claim); err != nil || field.PullRequestCredential {
		t.Fatalf("no credential = %+v %v", field, err)
	}
}
