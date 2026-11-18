package repos

// The migration of existing compositions to delegation grants and the sync that
// keeps new ones covered (ADR-0044 amendment 1, A1.3; delegation epic 2156 story
// S6) against a REAL migrated database. It SKIPS without
// ELITEA_TEST_DATABASE_URL.
//
// Fixture (project 1): callee agent 31 (version 41) holds toolkit 51. Agent 32
// holds nothing and attaches 31 (its version 42): the callee EXCEEDS the caller.
// Agent 33 holds toolkit 51 as well and attaches 31: CONTAINED, no grant.
// Pipeline 34 attaches 31 as tool `spec` and runs it from an Agent node of its
// YAML; pipeline 35 attaches 31 and runs it from no node.

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

const (
	edgePipelineYAML = "entry_point: run_spec\nnodes:\n  - id: run_spec\n    type: agent\n    tool: spec\n    transition: END\n"
	// A pipeline whose Agent node runs a tool that is not the attached one.
	edgeIdlePipelineYAML = "entry_point: step\nnodes:\n  - id: step\n    type: llm\n    transition: END\n"
)

// edgeCaller writes a caller application with one version that attaches agent 31
// (version 41) as a tool named alias.
func edgeCaller(t *testing.T, pool *pgxpool.Pool, id, versionID, toolID int64, agentType, instructions, alias string) {
	t.Helper()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO p_1.applications (id, name, owner_id) VALUES ($1::bigint, 'caller ' || $1::text, 1) ON CONFLICT (id) DO NOTHING`, []any{id}},
		{`INSERT INTO p_1.application_versions (id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at)
VALUES ($2, $1, 'base', 'draft', 12, gen_random_uuid(), '{}'::jsonb, $3, '[]'::json, '', $4,
    '{}'::jsonb, '{}'::jsonb, now() AT TIME ZONE 'UTC')`, []any{id, versionID, instructions, agentType}},
		{`INSERT INTO p_1.elitea_tools (id, type, name, description, settings, author_id, meta, created_at)
VALUES ($1, 'application', $2, 'attached agent', '{"application_id":"31","application_version_id":"41"}'::jsonb, 12,
    '{}'::jsonb, now() AT TIME ZONE 'UTC')`, []any{toolID, alias}},
		{`INSERT INTO p_1.entity_tool_mapping (tool_id, entity_id, entity_version_id, entity_type) VALUES ($1, $2, $3, 'agent')`, []any{toolID, id, versionID}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func seedEdgeFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	// 32 holds nothing and attaches the callee (written by the drift fixture).
	attachFixtureCallee(t, pool)
	// 33 holds the callee's toolkit too: its set contains the callee's.
	edgeCaller(t, pool, 33, 43, 53, "openai", "", "specialist")
	if _, err := pool.Exec(t.Context(), `INSERT INTO p_1.entity_tool_mapping (tool_id, entity_id, entity_version_id, entity_type) VALUES (51, 33, 43, 'agent')`); err != nil {
		t.Fatal(err)
	}
	edgeCaller(t, pool, 34, 44, 54, "pipeline", edgePipelineYAML, "spec")
	edgeCaller(t, pool, 35, 45, 55, "pipeline", edgeIdlePipelineYAML, "spec")
}

func edgeRows(t *testing.T, store *DelegationGrants) map[[2]int64]dg.Grant {
	t.Helper()
	out := map[[2]int64]dg.Grant{}
	for _, caller := range []int64{32, 33, 34, 35, 36} {
		grants, err := store.ListForCaller(t.Context(), 1, caller, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, grant := range grants {
			out[[2]int64{grant.Edge.CallerEntityID, grant.Edge.CalleeEntityID}] = grant
		}
	}
	return out
}

func countNotices(t *testing.T, pool *pgxpool.Pool) map[int64]int {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT (meta->>'caller_application_id')::bigint FROM centry.notifications WHERE event_type = $1`, dg.NotifyLegacyPending)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var caller int64
		if err := rows.Scan(&caller); err != nil {
			t.Fatal(err)
		}
		out[caller]++
	}
	return out
}

func approvedCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM p_1.delegation_grants WHERE state IN ('approved', 'stale', 'superseded') OR approved_by IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDelegationMigrationWritesPendingLegacyGrantsForExceedingEdges(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newGovernedDelegationStores(t, pool, false, holdsFixtureToolkit)
	ctx := t.Context()
	seedEdgeFixture(t, pool)
	edges, err := NewDelegationEdges(store)
	if err != nil {
		t.Fatal(err)
	}

	report, err := edges.MigrateAll(ctx)
	if err != nil || report.Migrated != 1 || report.Edges != 4 || report.Created != 3 || report.Failed != 0 {
		t.Fatalf("first pass: %+v %v", report, err)
	}
	grants := edgeRows(t, store)
	// 32 and 34 and 35 exceed (32 and 35 hold nothing; 34 holds nothing); 33 holds
	// the callee's toolkit, so it is contained and gets no grant.
	if len(grants) != 3 {
		t.Fatalf("grants = %+v, want one per exceeding edge", grants)
	}
	if _, has := grants[[2]int64{33, 31}]; has {
		t.Fatal("a contained edge needs no grant")
	}
	want := map[int64]dg.EdgeKind{32: dg.EdgeAgentTool, 34: dg.EdgePipelineNode, 35: dg.EdgePipelineChild}
	for caller, kind := range want {
		grant, ok := grants[[2]int64{caller, 31}]
		if !ok {
			t.Fatalf("caller %d has no grant: %+v", caller, grants)
		}
		if grant.State != dg.StatePending || !grant.Legacy || grant.Origin != dg.OriginMigration || grant.Edge.Kind != kind ||
			grant.ApprovedBy != nil || grant.ProposedBy != nil || grant.Governed || grant.CalleeVersionID != 41 {
			t.Fatalf("caller %d: %+v, want a pending legacy %s grant of the migration", caller, grant, kind)
		}
		if len(grant.RequestedSet.Items()) == 0 {
			t.Fatalf("caller %d: the grant lists nothing", caller)
		}
	}
	if n := approvedCount(t, pool); n != 0 {
		t.Fatalf("the migration approved %d grants", n)
	}
	// One notification per calling agent that has grants, to its editors (the
	// author of its version).
	if got := countNotices(t, pool); len(got) != 3 || got[32] != 1 || got[34] != 1 || got[35] != 1 {
		t.Fatalf("notifications = %v, want one for each of 32, 34 and 35", got)
	}
	if audit := grantAudit(t, pool); len(audit) != 3 {
		t.Fatalf("audit = %v, want one proposal per grant", audit)
	}

	// A restart, and the admin's scan, create nothing and tell nobody again.
	report, err = edges.MigrateAll(ctx)
	if err != nil || report.Done != 1 || report.Created != 0 || report.Migrated != 0 {
		t.Fatalf("second pass: %+v %v", report, err)
	}
	report, err = edges.ScanAll(ctx)
	if err != nil || report.Created != 0 || report.Notified != 0 {
		t.Fatalf("scan: %+v %v", report, err)
	}
	if got := edgeRows(t, store); len(got) != 3 {
		t.Fatalf("grants after the reruns = %d", len(got))
	}
	if got := countNotices(t, pool); len(got) != 3 || got[32] != 1 {
		t.Fatalf("notifications after the reruns = %v", got)
	}
	if n := approvedCount(t, pool); n != 0 {
		t.Fatalf("a rerun approved %d grants", n)
	}
}

// A rejected legacy grant stays rejected: the scan does not propose it again.
func TestDelegationMigrationDoesNotReproposeARejectedLegacyEdge(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newGovernedDelegationStores(t, pool, false, holdsFixtureToolkit)
	ctx := t.Context()
	seedEdgeFixture(t, pool)
	edges, err := NewDelegationEdges(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := edges.MigrateAll(ctx); err != nil {
		t.Fatal(err)
	}
	grant := edgeRows(t, store)[[2]int64{32, 31}]
	if _, err := store.Revoke(ctx, 1, grant.ID, 9); err != nil {
		t.Fatal(err)
	}
	report, err := edges.ScanAll(ctx)
	if err != nil || report.Created != 0 {
		t.Fatalf("scan: %+v %v", report, err)
	}
	if got := edgeRows(t, store)[[2]int64{32, 31}]; got.State != dg.StateRevoked {
		t.Fatalf("the rejected edge came back: %+v", got)
	}
}

// An edge whose attach grant was revoked while the attachment stayed (a path
// the relation route does not see) is picked up by the scan as legacy.
func TestDelegationScanFindsAnEdgeWithNoActiveGrant(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newGovernedDelegationStores(t, pool, false, holdsFixtureToolkit)
	ctx := t.Context()
	seedEdgeFixture(t, pool)
	edges, err := NewDelegationEdges(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := edges.MigrateAll(ctx); err != nil {
		t.Fatal(err)
	}
	// 36 is a pipeline created AFTER the upgrade: its YAML defines the edge.
	edgeCaller(t, pool, 36, 46, 56, "pipeline", edgePipelineYAML, "spec")
	result, err := edges.Run(ctx, 1, DelegationEdgeOptions{Origin: dg.OriginAttach, Actor: 9})
	if err != nil || result.Created != 1 || result.Contained != 1 {
		t.Fatalf("sync: %+v %v", result, err)
	}
	grant := edgeRows(t, store)[[2]int64{36, 31}]
	if grant.State != dg.StatePending || grant.Legacy || grant.Origin != dg.OriginAttach || grant.Edge.Kind != dg.EdgePipelineNode ||
		grant.ProposedBy == nil || *grant.ProposedBy != 9 {
		t.Fatalf("a YAML edge created after the upgrade: %+v", grant)
	}
	// The sync is idempotent too.
	if again, err := edges.Run(ctx, 1, DelegationEdgeOptions{Origin: dg.OriginAttach, Actor: 9}); err != nil || again.Created != 0 {
		t.Fatalf("sync again: %+v %v", again, err)
	}
	// And a sync names its proposer.
	if _, err := edges.Run(ctx, 1, DelegationEdgeOptions{Origin: dg.OriginAttach}); err == nil {
		t.Fatal("a sync with no proposer was accepted")
	}
	if n := approvedCount(t, pool); n != 0 {
		t.Fatalf("the sync approved %d grants", n)
	}
}

// Under the governed profile the migration's grants are governed, name a
// proposer (the last author of the caller) and carry a change request: nothing
// is approved, and the maker is never the checker.
func TestDelegationMigrationRespectsTheGovernedProfile(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	lifecycle, store := newGovernedDelegationStores(t, pool, true, holdsFixtureToolkit)
	ctx := t.Context()
	seedEdgeFixture(t, pool)
	edges, err := NewDelegationEdges(store)
	if err != nil {
		t.Fatal(err)
	}
	report, err := edges.MigrateAll(ctx)
	if err != nil || report.Created != 3 || report.Failed != 0 {
		t.Fatalf("governed pass: %+v %v", report, err)
	}
	for caller, grant := range map[int64]dg.Grant{
		32: edgeRows(t, store)[[2]int64{32, 31}], 34: edgeRows(t, store)[[2]int64{34, 31}], 35: edgeRows(t, store)[[2]int64{35, 31}],
	} {
		if !grant.Governed || grant.State != dg.StatePending || !grant.Legacy || grant.ProposedBy == nil || *grant.ProposedBy != 12 || grant.ApprovedBy != nil {
			t.Fatalf("caller %d: %+v, want a pending governed legacy grant proposed by 12", caller, grant)
		}
		change, found, err := lifecycle.DelegationChangeForGrant(ctx, 1, grant.ID)
		if err != nil || !found || !change.State.Open() {
			t.Fatalf("caller %d: change request %+v found=%t err=%v", caller, change, found, err)
		}
	}
	// The maker (12) cannot approve it; the table says so too.
	grant := edgeRows(t, store)[[2]int64{32, 31}]
	if _, err := store.Approve(ctx, 1, grant.ID, 12, nil, holdsFixtureToolkit); err == nil {
		t.Fatal("the proposer approved a governed grant")
	}
	if n := approvedCount(t, pool); n != 0 {
		t.Fatalf("the governed migration approved %d grants", n)
	}
}

func TestDelegationReviewQueueCountsPendingAndStalePerProject(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newGovernedDelegationStores(t, pool, false, holdsFixtureToolkit)
	ctx := t.Context()
	seedEdgeFixture(t, pool)
	edges, err := NewDelegationEdges(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := edges.MigrateAll(ctx); err != nil {
		t.Fatal(err)
	}
	queue, err := store.ReviewQueue(ctx)
	if err != nil || len(queue) != 1 {
		t.Fatalf("queue = %+v %v", queue, err)
	}
	project := queue[0]
	if project.ProjectID != 1 || project.Pending != 3 || project.Legacy != 3 || project.Stale != 0 || len(project.Callers) != 3 {
		t.Fatalf("project 1: %+v", project)
	}
	// Approving one clears it from the queue.
	grant := edgeRows(t, store)[[2]int64{32, 31}]
	if _, err := store.Approve(ctx, 1, grant.ID, 8, nil, holdsFixtureToolkit); err != nil {
		t.Fatal(err)
	}
	queue, err = store.ReviewQueue(ctx)
	if err != nil || len(queue) != 1 || queue[0].Pending != 2 || len(queue[0].Callers) != 2 {
		t.Fatalf("after one approval: %+v %v", queue, err)
	}
}
