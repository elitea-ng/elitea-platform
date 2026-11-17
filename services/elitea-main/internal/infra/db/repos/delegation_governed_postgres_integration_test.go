package repos

// The governed delegation grant (ADR-0044 amendment 1, story S2b): a grant
// proposed under the governed profile is approved through an ADR-0040 change
// request of kind delegation on the CALLER, with maker-checker at every tier,
// and the attach path proposes and revokes grants in its own transaction.
// REAL database; SKIPS without ELITEA_TEST_DATABASE_URL. Fixture: project 1,
// callee agent 31 (version 41, toolkit 51), caller agents 32 and 33.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

type fixedAccessResolver struct{ access dg.Access }

func (r fixedAccessResolver) AccessFor(context.Context, int64, int64) (dg.Access, error) {
	return r.access, nil
}

type noClasses struct{}

func (noClasses) AgentDataClasses(context.Context, int64, int64) ([]string, error) { return nil, nil }

func newGovernedDelegationStores(t *testing.T, pool *pgxpool.Pool, governed bool, access dg.Access) (*AgentLifecycle, *DelegationGrants) {
	t.Helper()
	seedAgentReleaseFixture(t, pool)
	if _, err := pool.Exec(t.Context(), `
INSERT INTO p_1.applications (id, name, owner_id) VALUES (32, 'Caller A', 1), (33, 'Caller B', 1)`); err != nil {
		t.Fatal(err)
	}
	lifecycle := newLifecycleTestStore(t, pool, agentlifecycle.DefaultPolicy())
	appender, err := securityaudit.NewPostgresAppender("elitea-main-test")
	if err != nil {
		t.Fatal(err)
	}
	callees, err := NewDelegationCallees(pool, noClasses{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewDelegationGrants(pool, appender,
		WithDelegationChanges(lifecycle), WithDelegationAccess(fixedAccessResolver{access}),
		WithDelegationCallees(callees), WithDelegationGoverned(func(context.Context) bool { return governed }))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle.ComposeDelegationApplier(store)
	return lifecycle, store
}

func calleeSet(t *testing.T, pool *pgxpool.Pool) dg.GrantSet {
	t.Helper()
	callees, err := NewDelegationCallees(pool, noClasses{})
	if err != nil {
		t.Fatal(err)
	}
	callee, err := callees.Snapshot(t.Context(), 1, 31, 41)
	if err != nil {
		t.Fatal(err)
	}
	return callee.Set
}

func TestDelegationCalleeSnapshotReadsTheVersionsToolkits(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, _ = newGovernedDelegationStores(t, pool, false, holdsAccess{})
	set := calleeSet(t, pool)
	if len(set.Toolkits) != 1 || set.Toolkits[0].Key != "toolkit:51" || set.Toolkits[0].Tools.Mode != dg.ToolsOnly ||
		len(set.Toolkits[0].Tools.Names) != 2 {
		t.Fatalf("callee set: %+v", set)
	}
	callees, _ := NewDelegationCallees(pool, noClasses{})
	if _, err := callees.Snapshot(t.Context(), 1, 31, 9999); !errors.Is(err, dg.ErrCalleeNotFound) {
		t.Fatalf("a version of nothing: %v", err)
	}
	if name, err := callees.ApplicationName(t.Context(), 1, 32); err != nil || name != "Caller A" {
		t.Fatalf("name %q %v", name, err)
	}
}

func TestGovernedDelegationIsApprovedThroughItsChangeRequest(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	all := holdsAccess{toolkits: map[string]bool{"toolkit:51": true}}
	lifecycle, store := newGovernedDelegationStores(t, pool, true, all)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	edge := dg.Edge{CallerEntityID: 32, CalleeEntityID: 31, Kind: dg.EdgeAgentTool}

	grant, err := store.ProposeGrant(ctx, 1, edge, 41, calleeSet(t, pool), dg.OriginAttach, true, 7)
	if err != nil {
		t.Fatal(err)
	}
	change, found, err := lifecycle.DelegationChangeForGrant(ctx, 1, grant.ID)
	if err != nil || !found || change.Kind != agentlifecycle.KindDelegation || change.State != agentlifecycle.ChangeSubmitted ||
		change.ApplicationID != 32 || change.ProposedBy != 7 || change.RequiredApprovals < 1 {
		t.Fatalf("change request: %+v found=%v err=%v", change, found, err)
	}
	// A delegation request is beside the agent's one open request, not in its way.
	if _, err := lifecycle.CreateChangeRequest(ctx, agentlifecycle.NewChangeRequest{
		ProjectID: 1, ApplicationID: 32, Kind: agentlifecycle.KindRecertification, ProposedBy: 7, Summary: "x"}); err != nil {
		if refusal, ok := agentlifecycle.AsRefusal(err); ok && refusal.Code == agentlifecycle.OpenChangeCode {
			t.Fatalf("an open delegation request blocked the agent's own change request: %v", err)
		}
	}

	// Maker-checker: the proposer cannot approve, in code (the lifecycle refuses)...
	_, err = lifecycle.ApproveChangeRequest(ctx, Approval{ProjectID: 1, ApplicationID: 32, ChangeID: change.ID, ApproverID: 7})
	requireRefusal(t, err, agentlifecycle.SelfApprovalCode)
	if got, _ := store.Get(ctx, 1, grant.ID); got.State != dg.StatePending {
		t.Fatalf("a refused approval must not approve: %+v", got)
	}
	// ... and the direct route is closed for a governed grant (the grant's own CHECK is the second line).
	if _, err := store.Approve(ctx, 1, grant.ID, 7, nil, all); !errors.Is(err, dg.ErrSelfApproval) {
		t.Fatalf("the proposer approving directly: %v", err)
	}

	outcome, err := lifecycle.ApproveChangeRequest(ctx, Approval{ProjectID: 1, ApplicationID: 32, ChangeID: change.ID, ApproverID: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Complete || outcome.Change.State != agentlifecycle.ChangeApproved || outcome.Published != nil {
		t.Fatalf("outcome: %+v", outcome)
	}
	approved, _ := store.Get(ctx, 1, grant.ID)
	if approved.State != dg.StateApproved || approved.ApprovedBy == nil || *approved.ApprovedBy != 8 || !approved.Governed ||
		approved.Digest != calleeSet(t, pool).Digest() {
		t.Fatalf("approved grant: %+v", approved)
	}
}

func TestGovernedDelegationApprovesOnlyWhatTheApproversHold(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	lifecycle, store := newGovernedDelegationStores(t, pool, true, holdsAccess{}) // holds no toolkit
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	grant, err := store.ProposeGrant(ctx, 1, dg.Edge{CallerEntityID: 32, CalleeEntityID: 31, Kind: dg.EdgeAgentTool}, 41, calleeSet(t, pool), dg.OriginAttach, true, 7)
	if err != nil {
		t.Fatal(err)
	}
	change, _, _ := lifecycle.DelegationChangeForGrant(ctx, 1, grant.ID)
	_, err = lifecycle.ApproveChangeRequest(ctx, Approval{ProjectID: 1, ApplicationID: 32, ChangeID: change.ID, ApproverID: 8})
	requireRefusal(t, err, agentlifecycle.InvalidCode)
	if got, _ := store.Get(ctx, 1, grant.ID); got.State != dg.StatePending {
		t.Fatalf("nothing approved: %+v", got)
	}
}

func TestRejectedDelegationRevokesItsGrant(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	lifecycle, store := newGovernedDelegationStores(t, pool, true, holdsAccess{toolkits: map[string]bool{"toolkit:51": true}})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	set := calleeSet(t, pool)
	one, err := store.ProposeGrant(ctx, 1, dg.Edge{CallerEntityID: 32, CalleeEntityID: 31, Kind: dg.EdgeAgentTool}, 41, set, dg.OriginAttach, true, 7)
	if err != nil {
		t.Fatal(err)
	}
	two, err := store.ProposeGrant(ctx, 1, dg.Edge{CallerEntityID: 33, CalleeEntityID: 31, Kind: dg.EdgeAgentTool}, 41, set, dg.OriginAttach, true, 7)
	if err != nil {
		t.Fatal(err) // a second open delegation request on another caller, and on the same callee
	}
	changeOne, _, _ := lifecycle.DelegationChangeForGrant(ctx, 1, one.ID)
	if _, err := lifecycle.RejectChangeRequest(ctx, 1, 32, changeOne.ID, 8, "not needed"); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, 1, one.ID); got.State != dg.StateRevoked {
		t.Fatalf("rejected: %+v", got)
	}
	changeTwo, _, _ := lifecycle.DelegationChangeForGrant(ctx, 1, two.ID)
	if _, err := lifecycle.WithdrawChangeRequest(ctx, 1, 33, changeTwo.ID, 7, false, "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, 1, two.ID); got.State != dg.StateRevoked {
		t.Fatalf("withdrawn: %+v", got)
	}
	// The edge is free again.
	if _, err := store.ProposeGrant(ctx, 1, dg.Edge{CallerEntityID: 32, CalleeEntityID: 31, Kind: dg.EdgeAgentTool}, 41, set, dg.OriginAttach, true, 7); err != nil {
		t.Fatalf("re-propose: %v", err)
	}
}

func TestRevokingAGovernedGrantClosesItsOpenChangeRequest(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	lifecycle, store := newGovernedDelegationStores(t, pool, true, holdsAccess{toolkits: map[string]bool{"toolkit:51": true}})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	grant, err := store.ProposeGrant(ctx, 1, dg.Edge{CallerEntityID: 32, CalleeEntityID: 31, Kind: dg.EdgeAgentTool}, 41, calleeSet(t, pool), dg.OriginAttach, true, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke(ctx, 1, grant.ID, 7); err != nil {
		t.Fatal(err)
	}
	change, found, err := lifecycle.DelegationChangeForGrant(ctx, 1, grant.ID)
	if err != nil || !found || change.State != agentlifecycle.ChangeWithdrawn {
		t.Fatalf("the request must be withdrawn with its grant: %+v %v %v", change, found, err)
	}
	// And nobody can approve it afterwards.
	if _, err := lifecycle.ApproveChangeRequest(ctx, Approval{ProjectID: 1, ApplicationID: 32, ChangeID: change.ID, ApproverID: 8}); err == nil {
		t.Fatal("a withdrawn request was approved")
	}
}

func TestDelegationChangeRequestMigrationKeepsOneOpenRequestPerAgent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, _ = newGovernedDelegationStores(t, pool, true, holdsAccess{})
	var open int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM pg_indexes WHERE schemaname = 'elitea_lifecycle' AND tablename = 'change_requests'
  AND indexname IN ('change_requests_one_open', 'change_requests_one_open_delegation')`).Scan(&open); err != nil || open != 2 {
		t.Fatalf("indexes %d %v", open, err)
	}
	// A delegation request must name its grant, and no other kind may.
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_lifecycle.change_requests (tenant_project_id, application_id, kind, state, risk_tier, summary, proposed_by)
VALUES (1, 32, 'delegation', 'submitted', 'low', 's', 7)`); err == nil {
		t.Fatal("a delegation request with no grant was accepted")
	}
}

func TestConsumableFromCallersListsApprovedAndStaleEdgesOnly(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	holds := holdsAccess{toolkits: map[string]bool{"toolkit:51": true}}
	_, store := newGovernedDelegationStores(t, pool, false, holds)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	set := calleeSet(t, pool)
	propose := func(caller int64) dg.Grant {
		g, err := store.ProposeGrant(ctx, 1, dg.Edge{CallerEntityID: caller, CalleeEntityID: 31, Kind: dg.EdgeAgentTool}, 41, set, dg.OriginAttach, false, 7)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	pending := propose(32)
	ok := propose(33)
	if _, err := store.Approve(ctx, 1, ok.ID, 7, nil, holds); err != nil {
		t.Fatal(err)
	}
	got, err := store.ConsumableFromCallers(ctx, 1, []int64{32, 33, 99})
	if err != nil || len(got) != 1 || got[0].ID != ok.ID {
		t.Fatalf("only the approved edge is consumable (pending %d): %+v %v", pending.ID, got, err)
	}
	if _, _, err := store.MarkStale(ctx, 1, ok.ID, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.ConsumableFromCallers(ctx, 1, []int64{33}); len(got) != 1 || got[0].State != dg.StateStale {
		t.Fatalf("a stale grant is consumed: %+v", got)
	}
	if got, err := NewDelegationGrantReader(pool).ConsumableFromCallers(ctx, 1, []int64{33}); err != nil || len(got) != 1 {
		t.Fatalf("the read-only store: %+v %v", got, err)
	}
}

func TestAttachProposesAndDetachRevokesInTheRelationTransaction(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newGovernedDelegationStores(t, pool, false, holdsAccess{})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	attach := DelegationAttach{ProjectID: 1, CallerID: 32, CalleeID: 31, CalleeVersionID: 41, Actor: 7}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachTx(ctx, tx, attach); err != nil {
		t.Fatal(err)
	}
	// A retry in the same transaction is the same grant.
	if err := store.AttachTx(ctx, tx, attach); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	grant, found, err := store.ActiveForEdge(ctx, 1, dg.Edge{CallerEntityID: 32, CalleeEntityID: 31, Kind: dg.EdgeAgentTool})
	if err != nil || !found || grant.State != dg.StatePending || grant.Origin != dg.OriginAttach || grant.Governed {
		t.Fatalf("attach grant: %+v found=%v err=%v", grant, found, err)
	}
	// A pipeline parent makes a pipeline child edge.
	pipeline := attach
	pipeline.CallerID, pipeline.Pipeline = 33, true
	tx, _ = pool.Begin(ctx)
	if err := store.AttachTx(ctx, tx, pipeline); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	if _, found, _ := store.ActiveForEdge(ctx, 1, dg.Edge{CallerEntityID: 33, CalleeEntityID: 31, Kind: dg.EdgePipelineChild}); !found {
		t.Fatal("a pipeline child edge")
	}

	// An attach that rolls back leaves no grant.
	rolled := attach
	rolled.CallerID = 33
	rolled.Pipeline = false
	tx, _ = pool.Begin(ctx)
	if err := store.AttachTx(ctx, tx, rolled); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	if _, found, _ := store.ActiveForEdge(ctx, 1, dg.Edge{CallerEntityID: 33, CalleeEntityID: 31, Kind: dg.EdgeAgentTool}); found {
		t.Fatal("a rolled-back attach left a grant")
	}

	// Detach: no version of the caller attaches the callee any more.
	tx, _ = pool.Begin(ctx)
	if err := store.DetachTx(ctx, tx, attach); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	if got, _ := store.Get(ctx, 1, grant.ID); got.State != dg.StateRevoked {
		t.Fatalf("detach: %+v", got)
	}

	// No signed-in user, no grant.
	anon := attach
	anon.Actor = 0
	anon.CallerID = 33
	tx, _ = pool.Begin(ctx)
	if err := store.AttachTx(ctx, tx, anon); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	if _, found, _ := store.ActiveForEdge(ctx, 1, dg.Edge{CallerEntityID: 33, CalleeEntityID: 31, Kind: dg.EdgeAgentTool}); found {
		t.Fatal("an attach with no user proposed a grant")
	}
}
