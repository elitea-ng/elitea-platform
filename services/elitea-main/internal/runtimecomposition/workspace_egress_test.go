package runtimecomposition

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib/runtoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egresstoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
)

type egressTestTurns struct{}

func (egressTestTurns) ResolveCloudTurn(context.Context, workspaceruntime.TurnClaim) (workspaceruntime.Turn, bool, error) {
	return workspaceruntime.Turn{ThreadUUID: "0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60", Epoch: 1}, true, nil
}

func (egressTestTurns) HandedOff(context.Context, workspaceruntime.TurnClaim) (bool, error) {
	return true, nil
}

type egressTestPolicies struct{}

func (egressTestPolicies) EffectiveCloudPolicy(context.Context, int64, string) (workspaceruntime.Policy, error) {
	return workspaceruntime.Policy{Enabled: true, Network: true, NetworkAllowlist: []string{"pypi.org"},
		MaxSegmentBytes: 1, MaxFileBytes: 1, MaxFiles: 1, MaxUnpackedBytes: 1, TurnWallSeconds: 600}, nil
}

// The egress seam reaches the resolver the runtime context uses: a unit test
// of the resolver alone cannot see this wiring
// (unit-suite-blind-to-composition-root).
func TestComposedWorkspaceContextMintsTheEgressToken(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	minter, err := egresstoken.New(priv)
	if err != nil {
		t.Fatal(err)
	}
	dependencies := Dependencies{Workspace: WorkspaceDependencies{
		Turns: egressTestTurns{}, Policies: egressTestPolicies{},
		Egress: workspaceruntime.Egress{ProxyURL: "http://elitea-egress-proxy:3128", Minter: minter},
	}}
	resolver := newWorkspaceContextResolver(dependencies)
	if resolver == nil {
		t.Fatal("the resolver is composed")
	}
	field, err := resolver.ResolveWorkspaceContext(context.Background(), workspaceruntime.TurnClaim{ExecutionID: "exec-1", Generation: 1, ProjectID: 9})
	if err != nil {
		t.Fatal(err)
	}
	if field.Egress.Mode != workspaceruntime.EgressModeProxy || field.Egress.ProxyURL != "http://elitea-egress-proxy:3128" {
		t.Fatalf("egress: %v", field.Egress)
	}
	keys, _ := runtoken.NewKeySet(pub)
	verified, err := keys.Verify(field.Egress.Token, time.Now())
	if err != nil || verified.Claims.ProjectID != 9 || verified.Claims.ExecutionID != "exec-1" {
		t.Fatalf("token: %v %+v", err, verified)
	}

	// A malformed proxy URL fails the start, not the first allowlist turn,
	// even without the executor seam (the egress checks on its own).
	bad := WorkspaceDependencies{Egress: dependencies.Workspace.Egress}
	if err := bad.validate(); err != nil {
		t.Fatalf("a good proxy URL alone composes: %v", err)
	}
	bad.Egress.ProxyURL = "https://proxy:3128"
	if err := bad.validate(); err == nil {
		t.Fatal("a bad proxy URL must refuse the composition")
	}
}
