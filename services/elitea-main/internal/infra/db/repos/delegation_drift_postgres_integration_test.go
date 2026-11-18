package repos

// Drift of a delegation grant's callee, re-approval and the Marketplace hook
// (ADR-0044 amendment 1, A1.2 rule 3; delegation epic story S4) against a REAL
// migrated database. It SKIPS without ELITEA_TEST_DATABASE_URL. Fixture:
// project 1, callee agent 31 (version 41, toolkit 51 selecting list_products),
// caller agents 32 and 33.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationdrift"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

var holdsFixtureToolkit = holdsAccess{toolkits: map[string]bool{"toolkit:51": true}}

func setCalleeTools(t *testing.T, pool *pgxpool.Pool, tools ...string) {
	t.Helper()
	list := `[`
	for i, tool := range tools {
		if i > 0 {
			list += `,`
		}
		list += `"` + tool + `"`
	}
	list += `]`
	if _, err := pool.Exec(t.Context(), `
UPDATE p_1.elitea_tools SET settings = jsonb_set(settings, '{selected_tools}', $1::jsonb) WHERE id = 51`, list); err != nil {
		t.Fatal(err)
	}
}

// attachFixtureCallee writes the application toolkit row that attaches agent 31
// (version 41) to a version of agent 32, as the attach path does.
func attachFixtureCallee(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO p_1.application_versions (id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at)
VALUES (42, 32, 'base', 'draft', 12, '80000000-0000-4000-8000-000000000042', '{}'::jsonb, '', '[]'::json, '', 'agent',
    '{}'::jsonb, '{}'::jsonb, now() AT TIME ZONE 'UTC');
INSERT INTO p_1.elitea_tools (id, type, name, description, settings, author_id, meta, created_at)
VALUES (52, 'application', 'parent', 'the parent agent', '{"application_id":"31","application_version_id":"41"}'::jsonb, 12,
    '{}'::jsonb, now() AT TIME ZONE 'UTC');
INSERT INTO p_1.entity_tool_mapping (tool_id, entity_id, entity_version_id, entity_type)
VALUES (52, 32, 42, 'agent')`); err != nil {
		t.Fatal(err)
	}
}

func driftNotifications(t *testing.T, pool *pgxpool.Pool, eventType string) map[int64]int {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT user_id FROM centry.notifications WHERE event_type = $1`, eventType)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var user int64
		if err := rows.Scan(&user); err != nil {
			t.Fatal(err)
		}
		out[user]++
	}
	return out
}

func newDriftDetector(t *testing.T, pool *pgxpool.Pool, store *DelegationGrants) *delegationdrift.Detector {
	t.Helper()
	callees, err := NewDelegationCallees(pool, noClasses{})
	if err != nil {
		t.Fatal(err)
	}
	detector, err := delegationdrift.New(store, callees)
	if err != nil {
		t.Fatal(err)
	}
	return detector
}

func approvedFixtureGrant(t *testing.T, pool *pgxpool.Pool, store *DelegationGrants) dg.Grant {
	t.Helper()
	edge := dg.Edge{CallerEntityID: 32, CalleeEntityID: 31, Kind: dg.EdgeAgentTool}
	grant, err := store.ProposeGrant(t.Context(), 1, edge, 41, calleeSet(t, pool), dg.OriginAttach, false, 7)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := store.Approve(t.Context(), 1, grant.ID, 8, nil, holdsFixtureToolkit)
	if err != nil {
		t.Fatal(err)
	}
	return approved
}

func TestDriftWiderCalleeMarksStaleAndNotifiesOnceAndReapprovalRestores(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newGovernedDelegationStores(t, pool, false, holdsFixtureToolkit)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	attachFixtureCallee(t, pool)
	detector := newDriftDetector(t, pool, store)
	grant := approvedFixtureGrant(t, pool, store)

	// Nothing changed: the first check only records the closure baseline.
	result, err := detector.CheckProject(ctx, 1)
	if err != nil || result.Stale != 0 || result.Notified != 0 {
		t.Fatalf("no drift: %+v %v", result, err)
	}
	if got, _ := store.Get(ctx, 1, grant.ID); got.State != dg.StateApproved || got.Drift != nil {
		t.Fatalf("an unchanged callee: %+v", got)
	}

	// The callee gains a tool: the grant goes stale, records what changed, and
	// the caller's editors (the proposer, the approver and the author of the
	// caller's version) are told once.
	setCalleeTools(t, pool, "list_products", "list_releases")
	result, err = detector.CheckProject(ctx, 1)
	if err != nil || result.Stale != 1 || result.Notified != 3 {
		t.Fatalf("wider callee: %+v %v", result, err)
	}
	stale, _ := store.Get(ctx, 1, grant.ID)
	if stale.State != dg.StateStale || stale.Drift == nil || stale.Drift.CalleeVersionID != 41 || len(stale.Drift.Added) != 1 ||
		stale.Drift.Added[0] != (dg.Item{Kind: dg.ItemTool, Name: "list_releases", Toolkit: "toolkit:51"}) {
		t.Fatalf("stale grant: %+v drift %+v", stale, stale.Drift)
	}
	if !stale.State.Consumable() || stale.Digest != grant.Digest {
		t.Fatalf("a stale grant still covers what it recorded: %+v", stale)
	}
	if got := driftNotifications(t, pool, dg.NotifyStale); got[7] != 1 || got[8] != 1 || got[12] != 1 || len(got) != 3 {
		t.Fatalf("one notification per person: %+v", got)
	}

	// A second check, and a check after the bell was cleared, tell nobody again:
	// the notice row is the dedup record for (edge, digest).
	if result, err = detector.CheckProject(ctx, 1); err != nil || result.Notified != 0 {
		t.Fatalf("second check: %+v %v", result, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM centry.notifications WHERE event_type = $1`, dg.NotifyStale); err != nil {
		t.Fatal(err)
	}
	if _, err = detector.CheckProject(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := driftNotifications(t, pool, dg.NotifyStale); len(got) != 0 {
		t.Fatalf("a deleted notification must not come back: %+v", got)
	}

	// The same or a smaller set needs nothing and brings the grant back.
	setCalleeTools(t, pool, "list_products")
	if result, err = detector.CheckProject(ctx, 1); err != nil || result.Restored != 1 {
		t.Fatalf("restored: %+v %v", result, err)
	}
	if got, _ := store.Get(ctx, 1, grant.ID); got.State != dg.StateApproved || got.Drift != nil {
		t.Fatalf("back within the grant: %+v", got)
	}

	// A drift that comes back after a restore is a new event and is told again.
	setCalleeTools(t, pool, "list_products", "list_releases")
	if result, err = detector.CheckProject(ctx, 1); err != nil || result.Stale != 1 || result.Notified != 3 {
		t.Fatalf("the drift again after a restore: %+v %v", result, err)
	}
	// A different drift is a new digest and is told.
	setCalleeTools(t, pool, "list_products", "list_releases", "delete_product")
	if result, err = detector.CheckProject(ctx, 1); err != nil || result.Notified != 3 {
		t.Fatalf("a different drift: %+v %v", result, err)
	}

	// Re-approval replaces the grant, keeps the old one as superseded, and the
	// new grant lists what the callee holds now.
	reapproved, err := store.ReapproveStale(ctx, 1, grant.ID, 8, nil, holdsFixtureToolkit)
	if err != nil {
		t.Fatal(err)
	}
	if reapproved.State != dg.StateApproved || reapproved.ID == grant.ID || reapproved.Drift != nil ||
		len(reapproved.GrantSet.Toolkits) != 1 || len(reapproved.GrantSet.Toolkits[0].Tools.Names) != 3 {
		t.Fatalf("re-approved: %+v", reapproved)
	}
	if old, _ := store.Get(ctx, 1, grant.ID); old.State != dg.StateSuperseded || old.SupersededBy == nil || *old.SupersededBy != reapproved.ID {
		t.Fatalf("the old grant is kept as superseded: %+v", old)
	}
	if result, err = detector.CheckProject(ctx, 1); err != nil || result.Stale != 0 {
		t.Fatalf("after re-approval the callee is within its grant: %+v %v", result, err)
	}
	// An approved grant cannot be re-approved through the stale route.
	if _, err := store.ReapproveStale(ctx, 1, reapproved.ID, 8, nil, holdsFixtureToolkit); !errors.Is(err, dg.ErrWrongState) {
		t.Fatalf("re-approving an approved grant: %v", err)
	}
}

func TestDriftOfAPartialApprovalIsMeasuredAgainstWhatTheCalleeHeldNotWhatWasApproved(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newGovernedDelegationStores(t, pool, false, holdsFixtureToolkit)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	edge := dg.Edge{CallerEntityID: 32, CalleeEntityID: 31, Kind: dg.EdgeAgentTool}
	setCalleeTools(t, pool, "list_products", "list_features")
	grant, err := store.ProposeGrant(ctx, 1, edge, 41, calleeSet(t, pool), dg.OriginAttach, false, 7)
	if err != nil {
		t.Fatal(err)
	}
	partial, err := store.Approve(ctx, 1, grant.ID, 8, []dg.Item{
		{Kind: dg.ItemToolkit, Name: "toolkit:51"}, {Kind: dg.ItemTool, Name: "list_products", Toolkit: "toolkit:51"},
	}, holdsFixtureToolkit)
	if err != nil || len(partial.Omitted) != 1 {
		t.Fatalf("partial approval: %+v %v", partial, err)
	}
	// The callee still holds what the approver left out: that is not drift.
	if result, err := newDriftDetector(t, pool, store).CheckProject(ctx, 1); err != nil || result.Stale != 0 {
		t.Fatalf("an omitted item is not drift: %+v %v", result, err)
	}
}

func TestClosureGrowthNotifiesWithoutChangingState(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newGovernedDelegationStores(t, pool, false, holdsFixtureToolkit)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	detector := newDriftDetector(t, pool, store)
	grant := approvedFixtureGrant(t, pool, store)
	if _, err := detector.CheckProject(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := driftNotifications(t, pool, dg.NotifyClosureGrew); len(got) != 0 {
		t.Fatalf("the first reading is a baseline: %+v", got)
	}

	// The callee (31) gains an approved edge of its own: the closure of the
	// approved edge 32 -> 31 grew.
	below, err := store.ProposeGrant(ctx, 1, dg.Edge{CallerEntityID: 31, CalleeEntityID: 33, Kind: dg.EdgeAgentTool}, 41,
		dg.GrantSet{EgressHosts: []string{"db.example.com"}}, dg.OriginAttach, false, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(ctx, 1, below.ID, 8, nil, holdsFixtureToolkit); err != nil {
		t.Fatal(err)
	}
	result, err := detector.CheckProject(ctx, 1)
	if err != nil || result.Stale != 0 || result.Notified != 2 {
		t.Fatalf("closure growth: %+v %v", result, err)
	}
	if got, _ := store.Get(ctx, 1, grant.ID); got.State != dg.StateApproved {
		t.Fatalf("closure growth changes no state: %+v", got)
	}
	if got := driftNotifications(t, pool, dg.NotifyClosureGrew); got[7] != 1 || got[8] != 1 {
		t.Fatalf("closure notifications: %+v", got)
	}
	// Once per closure: a second check says nothing.
	if result, err = detector.CheckProject(ctx, 1); err != nil || result.Notified != 0 {
		t.Fatalf("second check: %+v %v", result, err)
	}
}

func TestGovernedStaleGrantIsReapprovedThroughAChangeRequest(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	all := holdsFixtureToolkit
	lifecycle, store := newGovernedDelegationStores(t, pool, true, all)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	edge := dg.Edge{CallerEntityID: 32, CalleeEntityID: 31, Kind: dg.EdgeAgentTool}
	grant, err := store.ProposeGrant(ctx, 1, edge, 41, calleeSet(t, pool), dg.OriginAttach, true, 7)
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := lifecycle.DelegationChangeForGrant(ctx, 1, grant.ID)
	if _, err := lifecycle.ApproveChangeRequest(ctx, Approval{ProjectID: 1, ApplicationID: 32, ChangeID: first.ID, ApproverID: 8}); err != nil {
		t.Fatal(err)
	}
	setCalleeTools(t, pool, "list_products", "list_releases")
	if result, err := newDriftDetector(t, pool, store).CheckProject(ctx, 1); err != nil || result.Stale != 1 {
		t.Fatalf("stale: %+v %v", result, err)
	}

	created, err := store.RequestReapproval(ctx, 1, grant.ID, 9)
	if err != nil || !created {
		t.Fatalf("request re-approval: %v %v", created, err)
	}
	if again, err := store.RequestReapproval(ctx, 1, grant.ID, 9); err != nil || again {
		t.Fatalf("an open request is not recorded twice: %v %v", again, err)
	}
	change, found, err := lifecycle.DelegationChangeForGrant(ctx, 1, grant.ID)
	if err != nil || !found || change.ID == first.ID || change.State != agentlifecycle.ChangeSubmitted || change.ProposedBy != 9 {
		t.Fatalf("re-approval change request: %+v found=%v err=%v", change, found, err)
	}
	// Maker-checker: the proposer cannot approve it.
	if _, err := lifecycle.ApproveChangeRequest(ctx, Approval{ProjectID: 1, ApplicationID: 32, ChangeID: change.ID, ApproverID: 9}); err == nil {
		t.Fatal("the proposer approved a governed re-approval")
	}
	if _, err := lifecycle.ApproveChangeRequest(ctx, Approval{ProjectID: 1, ApplicationID: 32, ChangeID: change.ID, ApproverID: 8}); err != nil {
		t.Fatal(err)
	}
	active, found, err := store.ActiveForEdge(ctx, 1, edge)
	if err != nil || !found || active.ID == grant.ID || active.State != dg.StateApproved || !active.Governed ||
		len(active.GrantSet.Toolkits[0].Tools.Names) != 2 {
		t.Fatalf("re-approved through the change request: %+v found=%v err=%v", active, found, err)
	}
	if old, _ := store.Get(ctx, 1, grant.ID); old.State != dg.StateSuperseded {
		t.Fatalf("the old grant is superseded: %+v", old)
	}
}

func TestMarketplaceChangeApprovesAndReapprovesTheInternalEdges(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newGovernedDelegationStores(t, pool, false, holdsFixtureToolkit)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	attachFixtureCallee(t, pool)
	callees, _ := NewDelegationCallees(pool, noClasses{})
	if versions, err := callees.AttachedVersions(ctx, 1, 32, 31); err != nil || len(versions) != 1 || versions[0] != 41 {
		t.Fatalf("attached versions: %v %v", versions, err)
	}
	reconcile := func(approvers ...int64) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.ReconcileCompositionTx(ctx, tx, CompositionApproval{
			ProjectID: 1, RootApplicationID: 32, Origin: dg.OriginMarketplaceUpgrade, ProposerID: 7, ApproverIDs: approvers,
		}); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	edge := dg.Edge{CallerEntityID: 32, CalleeEntityID: 31, Kind: dg.EdgeAgentTool}

	// A grant for an edge whose agent an upgrade removed is revoked.
	orphan, err := store.ProposeGrant(ctx, 1, dg.Edge{CallerEntityID: 32, CalleeEntityID: 9999, Kind: dg.EdgeAgentTool}, 41,
		dg.GrantSet{EgressHosts: []string{"gone.example.com"}}, dg.OriginAttach, false, 7)
	if err != nil {
		t.Fatal(err)
	}

	// The change creates and approves the edge's grant.
	reconcile(8)
	grant, found, err := store.ActiveForEdge(ctx, 1, edge)
	if err != nil || !found || grant.State != dg.StateApproved || grant.Origin != dg.OriginMarketplaceUpgrade ||
		grant.ApprovedBy == nil || *grant.ApprovedBy != 8 {
		t.Fatalf("the change approves its internal edge: %+v found=%v err=%v", grant, found, err)
	}
	if got, _ := store.Get(ctx, 1, orphan.ID); got.State != dg.StateRevoked {
		t.Fatalf("an edge whose agent is gone is revoked: %+v", got)
	}

	// Applying it again changes nothing: the callee is within its grant.
	reconcile(8)
	if again, _, _ := store.ActiveForEdge(ctx, 1, edge); again.ID != grant.ID {
		t.Fatalf("an unchanged edge keeps its grant: %+v", again)
	}

	// The callee widens; the next change re-approves the edge.
	setCalleeTools(t, pool, "list_products", "list_releases")
	reconcile(8)
	next, _, _ := store.ActiveForEdge(ctx, 1, edge)
	if next.ID == grant.ID || next.State != dg.StateApproved || len(next.GrantSet.Toolkits[0].Tools.Names) != 2 {
		t.Fatalf("a widened edge is re-approved: %+v", next)
	}
	if old, _ := store.Get(ctx, 1, grant.ID); old.State != dg.StateSuperseded {
		t.Fatalf("the old edge grant is superseded: %+v", old)
	}

	// Approvers who hold access to nothing the callee lists approve nothing: the
	// grant stays pending, and the change itself goes on.
	setCalleeTools(t, pool, "list_products", "list_releases", "delete_product")
	if _, err := pool.Exec(ctx, `UPDATE p_1.delegation_grants SET state = 'revoked', revoked_at = now() WHERE id = $1`, next.ID); err != nil {
		t.Fatal(err)
	}
	appender, err := securityaudit.NewPostgresAppender("elitea-main-test")
	if err != nil {
		t.Fatal(err)
	}
	store2, err := NewDelegationGrants(pool, appender, WithDelegationAccess(fixedAccessResolver{holdsAccess{}}), WithDelegationCallees(callees))
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := pool.Begin(ctx)
	if err := store2.ReconcileCompositionTx(ctx, tx, CompositionApproval{
		ProjectID: 1, RootApplicationID: 32, Origin: dg.OriginMarketplaceHire, ProposerID: 7, ApproverIDs: []int64{8},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if pending, found, _ := store2.ActiveForEdge(ctx, 1, edge); !found || pending.State != dg.StatePending {
		t.Fatalf("approvers without access leave the edge pending: %+v found=%v", pending, found)
	}
}
