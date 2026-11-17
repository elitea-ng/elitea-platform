package repos

// The delegation grant store (tenant 0252, ADR-0044 amendment 1) against a
// REAL migrated database. It SKIPS without ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"

	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

type holdsAccess struct{ toolkits, credentials map[string]bool }

func (a holdsAccess) HoldsToolkit(key string) bool          { return a.toolkits[key] }
func (a holdsAccess) HoldsCredential(reference string) bool { return a.credentials[reference] }

func newDelegationGrantStore(t *testing.T, pool *pgxpool.Pool) *DelegationGrants {
	t.Helper()
	appender, err := securityaudit.NewPostgresAppender("elitea-main-test")
	if err != nil {
		t.Fatal(err)
	}
	// The change request of a governed grant is the story's S2b concern
	// (delegation_governed_postgres_integration_test.go); here a recorder that
	// records nothing lets these tests exercise the grant rules alone.
	store, err := NewDelegationGrants(pool, appender, WithDelegationChanges(noChangeRecorder{}))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type noChangeRecorder struct{}

func (noChangeRecorder) WithdrawDelegationChangeTx(context.Context, pgx.Tx, int64, int64, int64) error {
	return nil
}

func (noChangeRecorder) RecordDelegationChangeTx(context.Context, pgx.Tx, agentlifecycle.NewDelegationChange) (agentlifecycle.ChangeRequest, error) {
	return agentlifecycle.ChangeRequest{}, nil
}

func grantAudit(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT event->'data'->>'action' FROM elitea_runtime.security_audit_outbox
WHERE event->'data'->>'action' LIKE 'agent.delegation.%' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
	}
	return actions
}

func gitSet() dg.GrantSet {
	return dg.GrantSet{
		Toolkits: []dg.ToolkitGrant{
			{Key: "toolkit:11", Credential: "ref-git", Tools: dg.Tools{Mode: dg.ToolsOnly, Names: []string{"read", "write"}}},
			{Key: "toolkit:12", Tools: dg.Tools{Mode: dg.ToolsAllExcept}},
		},
		DataClasses: []string{"internal"},
		EgressHosts: []string{"git.example.com"},
	}
}

func grantUser(v int64) *int64 { return &v }

func TestDelegationGrantLifecycleAndAudit(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store := newDelegationGrantStore(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	edge := dg.Edge{CallerEntityID: 2, CalleeEntityID: 1, Kind: dg.EdgeSubAgent}
	all := holdsAccess{
		toolkits:    map[string]bool{"toolkit:11": true, "toolkit:12": true},
		credentials: map[string]bool{"ref-git": true},
	}

	proposed, err := store.Propose(ctx, ProposeInput{
		ProjectID: 1, Edge: edge, CalleeVersionID: 5, Requested: gitSet(), Origin: dg.OriginAttach, ProposedBy: grantUser(7),
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposed.State != dg.StatePending || proposed.RequestedDigest != gitSet().Digest() || proposed.ApprovedBy != nil || proposed.State.Consumable() {
		t.Fatalf("proposed: %+v", proposed)
	}
	// The attach path retries: the same pending grant comes back, with no second row or event.
	again, err := store.Propose(ctx, ProposeInput{
		ProjectID: 1, Edge: edge, CalleeVersionID: 5, Requested: gitSet(), Origin: dg.OriginAttach, ProposedBy: grantUser(7),
	})
	if err != nil || again.ID != proposed.ID {
		t.Fatalf("retry: %+v %v", again, err)
	}
	// A different set for the same edge cannot take the slot.
	if _, err := store.Propose(ctx, ProposeInput{
		ProjectID: 1, Edge: edge, CalleeVersionID: 6, Requested: dg.GrantSet{EgressHosts: []string{"x.test"}}, Origin: dg.OriginAttach, ProposedBy: grantUser(7),
	}); !errors.Is(err, dg.ErrActiveEdgeExists) {
		t.Fatalf("a second active grant for the edge: %v", err)
	}

	// Outside the governed profile the proposer may approve their own grant.
	approved, err := store.Approve(ctx, 1, proposed.ID, 7, nil, all)
	if err != nil {
		t.Fatal(err)
	}
	if approved.State != dg.StateApproved || approved.ApprovedBy == nil || *approved.ApprovedBy != 7 || approved.ApprovedAt == nil ||
		approved.Digest != gitSet().Digest() || len(approved.Omitted) != 0 || !approved.State.Consumable() {
		t.Fatalf("approved: %+v", approved)
	}
	if _, err := store.Approve(ctx, 1, proposed.ID, 7, nil, all); !errors.Is(err, dg.ErrWrongState) {
		t.Fatalf("approving twice: %v", err)
	}

	// The stored digest column is the digest of the stored set.
	var stored string
	if err := pool.QueryRow(ctx, `SELECT digest FROM p_1.delegation_grants WHERE id = $1`, approved.ID).Scan(&stored); err != nil || stored != approved.GrantSet.Digest() {
		t.Fatalf("stored digest %q vs set %q: %v", stored, approved.GrantSet.Digest(), err)
	}

	revoked, err := store.Revoke(ctx, 1, approved.ID, 8)
	if err != nil || revoked.State != dg.StateRevoked || revoked.RevokedBy == nil || *revoked.RevokedBy != 8 || revoked.RevokedAt == nil {
		t.Fatalf("revoked: %+v %v", revoked, err)
	}
	if _, err := store.Revoke(ctx, 1, approved.ID, 8); !errors.Is(err, dg.ErrWrongState) {
		t.Fatalf("revoking twice: %v", err)
	}
	// The edge is free again: a detach then an attach is a new grant, the old row stays as history.
	next, err := store.Propose(ctx, ProposeInput{
		ProjectID: 1, Edge: edge, CalleeVersionID: 5, Requested: gitSet(), Origin: dg.OriginAttach, ProposedBy: grantUser(7),
	})
	if err != nil || next.ID == proposed.ID {
		t.Fatalf("re-attach: %+v %v", next, err)
	}
	history, err := store.ListForCaller(ctx, 1, 2, true)
	if err != nil || len(history) != 2 {
		t.Fatalf("history: %+v %v", history, err)
	}
	live, err := store.ListForCaller(ctx, 1, 2, false)
	if err != nil || len(live) != 1 || live[0].ID != next.ID {
		t.Fatalf("live: %+v %v", live, err)
	}
	if _, err := store.Get(ctx, 1, next.ID+1000); !errors.Is(err, dg.ErrNotFound) {
		t.Fatalf("a missing grant: %v", err)
	}

	// The RevokeEdge path is the detach.
	if ok, err := store.RevokeEdge(ctx, 1, edge, 7); err != nil || !ok {
		t.Fatalf("detach: %v %v", ok, err)
	}
	if ok, err := store.RevokeEdge(ctx, 1, edge, 7); err != nil || ok {
		t.Fatalf("detach with no grant: %v %v", ok, err)
	}

	want := []string{dg.ActionProposed, dg.ActionApproved, dg.ActionRevoked, dg.ActionProposed, dg.ActionRevoked}
	if got := grantAudit(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("audit = %v, want %v", got, want)
	}
}

func TestDelegationGrantMakerCheckerInCodeAndInTheTable(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store := newDelegationGrantStore(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	all := holdsAccess{
		toolkits:    map[string]bool{"toolkit:11": true, "toolkit:12": true},
		credentials: map[string]bool{"ref-git": true},
	}
	grant, err := store.Propose(ctx, ProposeInput{
		ProjectID: 1, Edge: dg.Edge{CallerEntityID: 2, CalleeEntityID: 1, Kind: dg.EdgeAgentTool}, CalleeVersionID: 5,
		Requested: gitSet(), Origin: dg.OriginAttach, Governed: true, ProposedBy: grantUser(7),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Code: the proposer cannot approve a governed grant.
	if _, err := store.Approve(ctx, 1, grant.ID, 7, nil, all); !errors.Is(err, dg.ErrSelfApproval) {
		t.Fatalf("same-person approval: %v", err)
	}
	if got, _ := store.Get(ctx, 1, grant.ID); got.State != dg.StatePending {
		t.Fatalf("a refused approval must not approve: %+v", got)
	}
	// CHECK: the same write, past the code, is refused by the table.
	_, err = pool.Exec(ctx, `UPDATE p_1.delegation_grants SET state = 'approved', approved_by = 7, approved_at = now() WHERE id = $1`, grant.ID)
	if err == nil {
		t.Fatal("the table must refuse a governed grant approved by its proposer")
	}
	if mapped := mapDelegationGrantError(err); !errors.Is(mapped, dg.ErrSelfApproval) {
		t.Fatalf("the CHECK maps to %v (raw %v)", mapped, err)
	}
	// A second person approves.
	approved, err := store.Approve(ctx, 1, grant.ID, 8, nil, all)
	if err != nil || approved.State != dg.StateApproved || !approved.Governed {
		t.Fatalf("second approver: %+v %v", approved, err)
	}
	// A governed grant needs a proposer, in code and in the table.
	if _, err := store.Propose(ctx, ProposeInput{
		ProjectID: 1, Edge: dg.Edge{CallerEntityID: 3, CalleeEntityID: 1, Kind: dg.EdgeSubAgent}, CalleeVersionID: 5,
		Requested: gitSet(), Origin: dg.OriginMigration, Governed: true,
	}); !errors.Is(err, dg.ErrInvalid) {
		t.Fatalf("governed with no proposer: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.delegation_grants (project_id, caller_entity_id, callee_entity_id, edge_kind, callee_version_id,
  requested_set, requested_digest, grant_set, digest, governed, origin)
VALUES (1, 3, 1, 'sub_agent', 5, '{}', repeat('a', 64), '{}', repeat('a', 64), true, 'migration')`); err == nil {
		t.Fatal("the table must refuse a governed grant with no proposer")
	}
}

func TestDelegationGrantPartialApprovalByAnApproverWithoutAccess(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store := newDelegationGrantStore(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	grant, err := store.Propose(ctx, ProposeInput{
		ProjectID: 1, Edge: dg.Edge{CallerEntityID: 2, CalleeEntityID: 1, Kind: dg.EdgePipelineChild}, CalleeVersionID: 5,
		Requested: gitSet(), Origin: dg.OriginAttach, ProposedBy: grantUser(7),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The approver holds toolkit 12 only, not the git toolkit or its credential.
	approved, err := store.Approve(ctx, 1, grant.ID, 9, nil, holdsAccess{toolkits: map[string]bool{"toolkit:12": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(approved.GrantSet.Toolkits) != 1 || approved.GrantSet.Toolkits[0].Key != "toolkit:12" {
		t.Fatalf("approved set: %+v", approved.GrantSet)
	}
	if approved.Digest == approved.RequestedDigest || approved.RequestedDigest != gitSet().Digest() {
		t.Fatalf("the grant digest is the approved part's, the requested digest stays the callee's: %+v", approved)
	}
	omitted := map[dg.Item]bool{}
	for _, item := range approved.Omitted {
		omitted[item] = true
	}
	for _, want := range []dg.Item{
		{Kind: dg.ItemToolkit, Name: "toolkit:11"},
		{Kind: dg.ItemCredential, Name: "ref-git", Toolkit: "toolkit:11"},
	} {
		if !omitted[want] {
			t.Fatalf("omitted lacks %+v: %+v", want, approved.Omitted)
		}
	}
	// What the worker narrows: the callee's current (full) set, less the approved part.
	if missing := approved.GrantSet.Missing(gitSet()); len(missing) == 0 {
		t.Fatal("the callee's full set must narrow against a partial grant")
	}
	// An approver who holds nothing approves nothing; the grant stays pending.
	second, err := store.Propose(ctx, ProposeInput{
		ProjectID: 1, Edge: dg.Edge{CallerEntityID: 4, CalleeEntityID: 1, Kind: dg.EdgeSubAgent}, CalleeVersionID: 5,
		Requested: dg.GrantSet{Toolkits: gitSet().Toolkits[:1]}, Origin: dg.OriginAttach, ProposedBy: grantUser(7),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(ctx, 1, second.ID, 9, nil, holdsAccess{}); !errors.Is(err, dg.ErrNothingToApprove) {
		t.Fatalf("holding nothing: %v", err)
	}
	if got, _ := store.Get(ctx, 1, second.ID); got.State != dg.StatePending {
		t.Fatalf("still pending: %+v", got)
	}
}

func TestDelegationGrantDriftReapprovalAndConsumption(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store := newDelegationGrantStore(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	edge := dg.Edge{CallerEntityID: 2, CalleeEntityID: 1, Kind: dg.EdgeSubAgent}
	all := holdsAccess{
		toolkits:    map[string]bool{"toolkit:11": true, "toolkit:12": true, "toolkit:13": true},
		credentials: map[string]bool{"ref-git": true},
	}
	pending, err := store.Propose(ctx, ProposeInput{ProjectID: 1, Edge: edge, CalleeVersionID: 5, Requested: gitSet(), Origin: dg.OriginAttach, ProposedBy: grantUser(7)})
	if err != nil {
		t.Fatal(err)
	}
	// A pending grant is not consumed.
	if got, err := store.ConsumableForCallee(ctx, 1, []int64{1}); err != nil || len(got) != 0 {
		t.Fatalf("pending is not consumable: %+v %v", got, err)
	}
	approved, err := store.Approve(ctx, 1, pending.ID, 7, nil, all)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.ConsumableForCallee(ctx, 1, []int64{1, 99}); err != nil || len(got) != 1 || got[0].ID != approved.ID {
		t.Fatalf("approved is consumable: %+v %v", got, err)
	}

	// The callee's current digest still matches: nothing changes.
	if _, changed, err := store.MarkStale(ctx, 1, approved.ID, gitSet().Digest()); err != nil || changed {
		t.Fatalf("no drift: %v %v", changed, err)
	}
	// The callee gains a toolkit: stale, still consumed (the drift is narrowed at run time).
	grown := gitSet()
	grown.Toolkits = append(grown.Toolkits, dg.ToolkitGrant{Key: "toolkit:13", Tools: dg.Tools{Mode: dg.ToolsAllExcept}})
	stale, changed, err := store.MarkStale(ctx, 1, approved.ID, grown.Digest())
	if err != nil || !changed || stale.State != dg.StateStale {
		t.Fatalf("drift: %+v %v %v", stale, changed, err)
	}
	if _, changed, _ := store.MarkStale(ctx, 1, approved.ID, grown.Digest()); changed {
		t.Fatal("a stale grant is marked once")
	}
	if got, _ := store.ConsumableForCallee(ctx, 1, []int64{1}); len(got) != 1 || got[0].State != dg.StateStale {
		t.Fatalf("a stale grant is consumed: %+v", got)
	}
	if missing := stale.GrantSet.Missing(grown); len(missing) != 1 || missing[0].Name != "toolkit:13" {
		t.Fatalf("narrowed out: %+v", missing)
	}

	// Re-approval keeps the old row as superseded and the edge has one active grant.
	fresh, err := store.Reapprove(ctx, ReapproveInput{
		ProjectID: 1, GrantID: stale.ID, CalleeVersionID: 6, Requested: grown, Access: all, Origin: dg.OriginAttach,
		ProposedBy: grantUser(7), ApprovedBy: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State != dg.StateApproved || fresh.ID == stale.ID || fresh.Digest != grown.Digest() || fresh.CalleeVersionID != 6 {
		t.Fatalf("re-approved: %+v", fresh)
	}
	old, _ := store.Get(ctx, 1, stale.ID)
	if old.State != dg.StateSuperseded || old.SupersededBy == nil || *old.SupersededBy != fresh.ID {
		t.Fatalf("old: %+v", old)
	}
	if active, found, err := store.ActiveForEdge(ctx, 1, edge); err != nil || !found || active.ID != fresh.ID {
		t.Fatalf("active: %+v %v %v", active, found, err)
	}
	if _, err := store.Reapprove(ctx, ReapproveInput{ProjectID: 1, GrantID: stale.ID, CalleeVersionID: 6, Requested: grown, Access: all, Origin: dg.OriginAttach, ApprovedBy: 8}); !errors.Is(err, dg.ErrWrongState) {
		t.Fatalf("re-approving a superseded grant: %v", err)
	}

	want := []string{dg.ActionProposed, dg.ActionApproved, dg.ActionStale, dg.ActionSuperseded, dg.ActionApproved}
	if got := grantAudit(t, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("audit = %v, want %v", got, want)
	}
}

func TestDelegationGrantTableRefusesBadRows(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const insert = `
INSERT INTO p_1.delegation_grants (project_id, caller_entity_id, callee_entity_id, edge_kind, callee_version_id,
  requested_set, requested_digest, grant_set, digest, state, origin)
VALUES (1, $1, $2, $3, 5, '{}', repeat('a', 64), '{}', repeat('a', 64), $4, 'attach')`
	for name, args := range map[string][]any{
		"an agent delegating to itself":      {1, 1, "sub_agent", "pending"},
		"an unknown edge kind":               {2, 1, "skill", "pending"},
		"an unknown state":                   {2, 1, "sub_agent", "ok"},
		"an approved grant with no approver": {2, 1, "sub_agent", "approved"},
	} {
		if _, err := pool.Exec(ctx, insert, args...); err == nil {
			t.Errorf("%s: the table accepted it", name)
		}
	}
}

func TestDelegationGrantReapprovalKeepsMakerCheckerAndStaleCanRecover(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store := newDelegationGrantStore(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	edge := dg.Edge{CallerEntityID: 2, CalleeEntityID: 1, Kind: dg.EdgeSubAgent}
	all := holdsAccess{
		toolkits:    map[string]bool{"toolkit:11": true, "toolkit:12": true},
		credentials: map[string]bool{"ref-git": true},
	}
	pending, err := store.Propose(ctx, ProposeInput{ProjectID: 1, Edge: edge, CalleeVersionID: 5, Requested: gitSet(), Origin: dg.OriginAttach, Governed: true, ProposedBy: grantUser(7)})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := store.Approve(ctx, 1, pending.ID, 8, nil, all)
	if err != nil {
		t.Fatal(err)
	}
	// A caller that drops the governed flag still meets maker-checker: the edge stays governed.
	if _, err := store.Reapprove(ctx, ReapproveInput{ProjectID: 1, GrantID: approved.ID, CalleeVersionID: 6, Requested: gitSet(), Access: all, Origin: dg.OriginAttach, Governed: false, ProposedBy: grantUser(7), ApprovedBy: 7}); !errors.Is(err, dg.ErrSelfApproval) {
		t.Fatalf("self re-approval of a governed edge: %v", err)
	}
	if _, err := store.Reapprove(ctx, ReapproveInput{ProjectID: 1, GrantID: approved.ID, CalleeVersionID: 6, Requested: gitSet(), Access: all, Origin: dg.OriginAttach, ApprovedBy: 9}); !errors.Is(err, dg.ErrInvalid) {
		t.Fatalf("a governed re-approval with no proposer: %v", err)
	}
	if _, err := store.Reapprove(ctx, ReapproveInput{ProjectID: 1, GrantID: approved.ID, CalleeVersionID: 0, Requested: gitSet(), Access: all, Origin: dg.OriginAttach, ApprovedBy: 9}); !errors.Is(err, dg.ErrInvalid) {
		t.Fatalf("no callee version: %v", err)
	}
	if _, err := store.Reapprove(ctx, ReapproveInput{ProjectID: 1, GrantID: approved.ID, CalleeVersionID: 6, Requested: gitSet(), Origin: dg.OriginAttach, ApprovedBy: 9, ProposedBy: grantUser(7)}); !errors.Is(err, dg.ErrInvalid) {
		t.Fatalf("no access: %v", err)
	}
	fresh, err := store.Reapprove(ctx, ReapproveInput{ProjectID: 1, GrantID: approved.ID, CalleeVersionID: 6, Requested: gitSet(), Access: all, Origin: dg.OriginAttach, ApprovedBy: 9, ProposedBy: grantUser(7)})
	if err != nil || !fresh.Governed {
		t.Fatalf("a second person re-approves: %+v %v", fresh, err)
	}

	// Drift and recovery: stale, then the callee returns to the recorded set.
	grown := gitSet()
	grown.EgressHosts = append(grown.EgressHosts, "new.example.com")
	if _, changed, err := store.MarkStale(ctx, 1, fresh.ID, grown.Digest()); err != nil || !changed {
		t.Fatalf("drift: %v %v", changed, err)
	}
	if _, changed, err := store.MarkCurrent(ctx, 1, fresh.ID, grown.Digest()); err != nil || changed {
		t.Fatalf("still drifted: %v %v", changed, err)
	}
	back, changed, err := store.MarkCurrent(ctx, 1, fresh.ID, gitSet().Digest())
	if err != nil || !changed || back.State != dg.StateApproved {
		t.Fatalf("recovered: %+v %v %v", back, changed, err)
	}
	// A detach revokes the live grant.
	if ok, err := store.RevokeEdge(ctx, 1, edge, 9); err != nil || !ok {
		t.Fatalf("detach: %v %v", ok, err)
	}
	if _, found, _ := store.ActiveForEdge(ctx, 1, edge); found {
		t.Fatal("no active grant after a detach")
	}
}

func TestDelegationGrantConcurrentProposalsReturnOneGrant(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store := newDelegationGrantStore(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	edge := dg.Edge{CallerEntityID: 2, CalleeEntityID: 1, Kind: dg.EdgeAgentTool}
	const workers = 6
	type result struct {
		id  int64
		err error
	}
	results := make(chan result, workers)
	for range workers {
		go func() {
			grant, err := store.Propose(ctx, ProposeInput{ProjectID: 1, Edge: edge, CalleeVersionID: 5, Requested: gitSet(), Origin: dg.OriginAttach, ProposedBy: grantUser(7)})
			results <- result{grant.ID, err}
		}()
	}
	first := int64(0)
	for range workers {
		r := <-results
		if r.err != nil {
			t.Fatalf("a concurrent retry: %v", r.err)
		}
		if first == 0 {
			first = r.id
		} else if r.id != first {
			t.Fatalf("two grants for one edge: %d and %d", first, r.id)
		}
	}
}

func TestDelegationGrantPartialApprovalKeepsDataClasses(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store := newDelegationGrantStore(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	grant, err := store.Propose(ctx, ProposeInput{ProjectID: 1, Edge: dg.Edge{CallerEntityID: 2, CalleeEntityID: 1, Kind: dg.EdgeSubAgent}, CalleeVersionID: 5, Requested: gitSet(), Origin: dg.OriginAttach, ProposedBy: grantUser(7)})
	if err != nil {
		t.Fatal(err)
	}
	// The approver keeps only toolkit 12: the data class bound stays, whatever they uncheck.
	approved, err := store.Approve(ctx, 1, grant.ID, 7, []dg.Item{{Kind: dg.ItemToolkit, Name: "toolkit:12"}},
		holdsAccess{toolkits: map[string]bool{"toolkit:11": true, "toolkit:12": true}, credentials: map[string]bool{"ref-git": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(approved.GrantSet.DataClasses) != 1 || approved.GrantSet.DataClasses[0] != "internal" {
		t.Fatalf("the data class bound must survive a partial approval: %+v", approved.GrantSet)
	}
}
