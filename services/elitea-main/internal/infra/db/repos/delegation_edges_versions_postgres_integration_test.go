package repos

// Which versions and which attachments make an edge, and who proposes it under
// the governed profile (story S6). Fixture as in
// delegation_edges_postgres_integration_test.go. It SKIPS without
// ELITEA_TEST_DATABASE_URL.

import (
	"testing"

	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

// An attachment that a caller's newer version no longer holds is not an edge: the
// migration reads each caller's newest and published versions only.
func TestDelegationMigrationIgnoresAVersionTheCallerReplaced(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newGovernedDelegationStores(t, pool, false, holdsFixtureToolkit)
	ctx := t.Context()
	attachFixtureCallee(t, pool)
	// A newer draft of agent 32 that attaches nothing replaces version 42.
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.application_versions (id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at)
VALUES (47, 32, 'newer', 'draft', 12, gen_random_uuid(), '{}'::jsonb, '', '[]'::json, '', 'openai',
    '{}'::jsonb, '{}'::jsonb, now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}
	edges, err := NewDelegationEdges(store)
	if err != nil {
		t.Fatal(err)
	}
	report, err := edges.MigrateAll(ctx)
	if err != nil || report.Edges != 0 || report.Created != 0 {
		t.Fatalf("a replaced attachment made an edge: %+v %v", report, err)
	}
}

// A tool row that pins no callee version runs the callee's newest, so the edge is
// still found and granted.
func TestDelegationMigrationFindsAnAttachmentThatPinsNoCalleeVersion(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newGovernedDelegationStores(t, pool, false, holdsFixtureToolkit)
	ctx := t.Context()
	attachFixtureCallee(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE p_1.elitea_tools SET settings = '{"application_id":"31"}'::jsonb WHERE id = 52`); err != nil {
		t.Fatal(err)
	}
	edges, err := NewDelegationEdges(store)
	if err != nil {
		t.Fatal(err)
	}
	report, err := edges.MigrateAll(ctx)
	if err != nil || report.Created != 1 {
		t.Fatalf("an unpinned attachment: %+v %v", report, err)
	}
	if grant := edgeRows(t, store)[[2]int64{32, 31}]; grant.State != dg.StatePending || grant.CalleeVersionID != 41 {
		t.Fatalf("grant = %+v, want a pending grant of the callee's newest version", grant)
	}
}

// Under the governed profile the sync proposes as the maker, the caller's last
// author, not as whoever armed the debounced check: another person's write may
// have made the edge.
func TestDelegationSyncProposesAsTheMakerUnderTheGovernedProfile(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newGovernedDelegationStores(t, pool, true, holdsFixtureToolkit)
	ctx := t.Context()
	attachFixtureCallee(t, pool)
	edges, err := NewDelegationEdges(store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := edges.Run(ctx, 1, DelegationEdgeOptions{Origin: dg.OriginAttach, Actor: 9})
	if err != nil || result.Created != 1 {
		t.Fatalf("sync: %+v %v", result, err)
	}
	grant := edgeRows(t, store)[[2]int64{32, 31}]
	if !grant.Governed || grant.ProposedBy == nil || *grant.ProposedBy != 12 {
		t.Fatalf("grant = %+v, want proposed by the caller's last author (12), not the actor (9)", grant)
	}
}
