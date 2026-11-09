package repos

// The workforce policy (shared/0250, ADR-0050 decisions 5c and 5k, #1617)
// against a REAL migrated database: off by default, auto_hire's invariants
// (low tier, read-only, unpublished, no tool bound, probation, the 14-day
// undo, max_auto_hired, min successful runs), auto_dismiss's (never demote
// or delete, never past retire, the grace period and the objection, the
// exemptions) and the orphan flag. It SKIPS without ELITEA_TEST_DATABASE_URL,
// and a skip prints the same `ok`, so each case asserts values a skip could
// not have produced.

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const policyProposer int64 = 991750

func onPolicy(t *testing.T, store *AgentWorkforce, mutate func(*agentworkforce.Policy)) WorkforcePolicy {
	t.Helper()
	policy := agentworkforce.DefaultPolicy()
	mutate(&policy)
	saved, err := store.SetPolicy(context.Background(), wfProject, managerActor(wfManager), policy)
	if err != nil {
		t.Fatalf("set the policy: %v", err)
	}
	return saved
}

// seedHireProposal writes an open agent proposal by policyProposer with the
// given tool keys, and `completed` + `failed` spawns of its spec.
func seedHireProposal(t *testing.T, pool *pgxpool.Pool, n int, tools []string, completed, failed int) int64 {
	t.Helper()
	digest := fmt.Sprintf("%064x", 0x1617000+n)
	toolJSON := "["
	for i, tool := range tools {
		if i > 0 {
			toolJSON += ","
		}
		toolJSON += fmt.Sprintf(`{"key":%q,"toolkit_type":"","category":"read"}`, tool)
	}
	toolJSON += "]"
	draft := fmt.Sprintf(`{"name":"Hired %d","description":"Reads code.","instructions":"Read and summarise.","risk_tier":"low",
"soul":{"schema":"elitea.soul/v1","display_name":"Ada","body":"Calm."},"tools":%s}`, n, toolJSON)
	var id int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO elitea_lifecycle.agent_proposals
    (project_id, proposer_application_id, proposed_for_user_id, execution_id, ephemeral_agent_id, spec_digest, rationale, draft)
VALUES (1, $1, $2, $3, $4, $5, 'It works.', $6::jsonb) RETURNING id`,
		policyProposer, wfOwner, fmt.Sprintf("exec-hire-%d", n), fmt.Sprintf("a_%08x", n), digest, draft).Scan(&id); err != nil {
		t.Fatalf("seed the proposal: %v", err)
	}
	for i := range completed + failed {
		status := "completed"
		if i >= completed {
			status = "failed"
		}
		wfExec(t, pool, `
INSERT INTO elitea_runtime.ephemeral_agents
    (execution_id, agent_id, command_id, generation, project_id, parent_application_id, parent_call_id, ordinal,
     profile_digest, status, started_at)
VALUES ($1, $2, 'cmd', 1, 1, $3, 'call', 1, $4, $5, now())`,
			fmt.Sprintf("exec-spawn-%d-%d", n, i), fmt.Sprintf("a_%08x", i), policyProposer, digest, status)
	}
	return id
}

func actionsOf(t *testing.T, store *AgentWorkforce, filter ActionFilter) []AutomaticAction {
	t.Helper()
	rows, _, err := store.ListAutomaticActions(context.Background(), wfProject, filter)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func seedPolicyBase(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedCurrentAgentContinuationSchema(t, pool)
	wfExec(t, pool, `ALTER TABLE p_1.applications ADD COLUMN IF NOT EXISTS icon varchar,
    ADD COLUMN IF NOT EXISTS meta jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS created_at timestamp NOT NULL DEFAULT (now() AT TIME ZONE 'UTC'),
    ADD COLUMN IF NOT EXISTS updated_at timestamp`)
	wfExec(t, pool, `ALTER TABLE p_1.application_versions ADD COLUMN IF NOT EXISTS soul jsonb`)
	wfExec(t, pool, `ALTER TABLE p_1.application_versions ALTER COLUMN uuid SET DEFAULT gen_random_uuid()`)
}

func TestWorkforcePolicyIsOffByDefaultAndOnlyAManagerSetsIt(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedPolicyBase(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()

	policy, err := store.Policy(ctx, wfProject)
	if err != nil || policy.Saved || policy.AutoHireEnabled || policy.AutoDismissEnabled || len(policy.DefaultRules) != 4 {
		t.Fatalf("default: %+v %v", policy, err)
	}
	// With no policy row, an eligible proposal stays a proposal and a bad
	// scorecard changes nothing.
	seedWorkforceAgent(t, pool, policyProposer, "medium", `{}`)
	seedHireProposal(t, pool, 1, []string{"github/read_file"}, 5, 0)
	const app int64 = 991751
	seedWorkforceAgent(t, pool, app, "low", `{}`)
	wfExec(t, pool, `INSERT INTO elitea_lifecycle.agent_scorecards (project_id, application_id, day, breaker_trips_7d) VALUES (1, $1, current_date, 9)`, app)
	summary, err := store.WorkforcePolicyPass(ctx)
	if err != nil || summary.Projects != 0 || summary.Hired != 0 || summary.Applied != 0 {
		t.Fatalf("pass with no policy: %+v %v", summary, err)
	}
	var proposals, actions int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_lifecycle.agent_workforce_proposals`).Scan(&proposals)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_lifecycle.agent_automatic_actions`).Scan(&actions)
	if proposals != 0 || actions != 0 {
		t.Fatalf("something automatic happened: proposals=%d actions=%d", proposals, actions)
	}

	if _, err := store.SetPolicy(ctx, wfProject, WorkforceActor{UserID: wfOwner}, agentworkforce.DefaultPolicy()); refusalCode(err) != agentworkforce.ForbiddenCode {
		t.Fatalf("a non-manager set the policy: %v", err)
	}
	bad := agentworkforce.DefaultPolicy()
	bad.Rules = []agentworkforce.Rule{{ID: "x", Conditions: []agentworkforce.Condition{{Metric: "runs_30d", Op: ">=", Value: 1}}, Then: agentworkforce.StageDelete}}
	if _, err := store.SetPolicy(ctx, wfProject, managerActor(wfManager), bad); refusalCode(err) != agentworkforce.PolicyInvalidCode {
		t.Fatalf("a delete rule: %v", err)
	}
	// The table itself refuses a policy proposal of demote or delete.
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_lifecycle.agent_workforce_proposals (project_id, application_id, kind, stage, source, policy_rule, rationale, evidence_digest)
VALUES (1, $1, 'demote', 'demote', 'policy', 'x', 'r', repeat('a', 64))`, app); err == nil {
		t.Fatal("the database stored a policy demotion")
	}
	saved := onPolicy(t, store, func(p *agentworkforce.Policy) { p.AutoHireEnabled = true })
	if !saved.Saved || !saved.AutoHireEnabled || saved.AutoDismissEnabled {
		t.Fatalf("saved: %+v", saved)
	}
}

func TestWorkforceAutoHireInvariantsProbationUndoAndMax(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedPolicyBase(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	seedWorkforceAgent(t, pool, policyProposer, "medium", `{}`)
	onPolicy(t, store, func(p *agentworkforce.Policy) { p.AutoHireEnabled = true; p.MaxAutoHired = 2 })

	eligible := seedHireProposal(t, pool, 1, []string{"github/read_file"}, 3, 0)
	writer := seedHireProposal(t, pool, 2, []string{"github/read_file", "github/create_pull_request"}, 5, 0)
	wildcard := seedHireProposal(t, pool, 3, []string{"github/*"}, 5, 0)
	few := seedHireProposal(t, pool, 4, []string{"github/read_file"}, 2, 0)
	capped := seedHireProposal(t, pool, 5, []string{"github/read_file"}, 4, 1)
	second := seedHireProposal(t, pool, 6, []string{"github/read_file"}, 3, 0)
	third := seedHireProposal(t, pool, 7, []string{}, 3, 0)

	summary, err := store.WorkforcePolicyPass(ctx)
	if err != nil || summary.Hired != 2 {
		t.Fatalf("pass: %+v %v", summary, err)
	}
	statuses := map[int64]string{}
	created := map[int64]int64{}
	rows, _ := pool.Query(ctx, `SELECT id, status, COALESCE(created_application_id, 0) FROM elitea_lifecycle.agent_proposals`)
	for rows.Next() {
		var id, app int64
		var status string
		_ = rows.Scan(&id, &status, &app)
		statuses[id], created[id] = status, app
	}
	rows.Close()
	for _, id := range []int64{writer, wildcard, few, capped} {
		if statuses[id] != "proposed" {
			t.Errorf("proposal %d (not eligible) became %s", id, statuses[id])
		}
	}
	if statuses[eligible] != "created" || statuses[second] != "created" || statuses[third] != "proposed" {
		t.Fatalf("max_auto_hired 2: %v", statuses)
	}

	hired := created[eligible]
	agent, err := store.Agent(ctx, wfProject, hired)
	if err != nil || agent.Tier != "low" || agent.AutoHiredAt == nil || agent.ProbationUntil == nil || !agent.OnProbation() ||
		agent.BusinessOwnerID != wfOwner || agent.State != agentworkforce.StateActive {
		t.Fatalf("the hire: %+v %v", agent, err)
	}
	if got := agent.ProbationUntil.Sub(*agent.AutoHiredAt); got != agentworkforce.ProbationWindow {
		t.Fatalf("probation = %v", got)
	}
	var published, versions, tools int
	var memoryWrite, soulName string
	_ = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status <> 'draft'), count(*), max(meta->'memory'->>'write'), max(soul->>'display_name')
FROM p_1.application_versions WHERE application_id = $1`, hired).Scan(&published, &versions, &memoryWrite, &soulName)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM p_1.entity_tool_mapping m JOIN p_1.application_versions v ON v.id = m.entity_version_id WHERE v.application_id = $1`, hired).Scan(&tools)
	if published != 0 || versions != 1 || tools != 0 || memoryWrite != "review" || soulName != "Ada" {
		t.Fatalf("unpublished, no tools, memory review, soul: published=%d versions=%d tools=%d memory=%q soul=%q",
			published, versions, tools, memoryWrite, soulName)
	}

	// Audited as policy:auto_hire.
	var principals []string
	_ = pool.QueryRow(ctx, `SELECT COALESCE(array_agg(DISTINCT event->'data'->>'principal_ref'), '{}') FROM elitea_runtime.security_audit_outbox
WHERE event->'data'->>'action' IN ('agent.workforce.auto_hired', 'agent.proposal.auto_hired')`).Scan(&principals)
	if !slices.Equal(principals, []string{"policy:auto_hire"}) {
		t.Fatalf("audit principals = %v", principals)
	}

	hires := actionsOf(t, store, ActionFilter{})
	if len(hires) != 2 || !hires[0].CanUndo || hires[0].Kind != agentworkforce.ActionAutoHire {
		t.Fatalf("actions: %+v", hires)
	}
	var hireAction, secondAction AutomaticAction
	for _, a := range hires {
		if a.ApplicationID == hired {
			hireAction = a
		} else {
			secondAction = a
		}
	}

	// Undo: only a manager; the agent is retired by the ladder (still
	// reinstatable), the slot frees and the third proposal is hired next.
	if _, err := store.UndoAutoHire(ctx, wfProject, hireAction.ID, WorkforceActor{UserID: wfOwner}, "", false); refusalCode(err) != agentworkforce.ForbiddenCode {
		t.Fatalf("the owner undid: %v", err)
	}
	undone, err := store.UndoAutoHire(ctx, wfProject, hireAction.ID, managerActor(wfManager), "Not needed.", false)
	if err != nil || undone.Status != agentworkforce.ActionUndone || undone.CanUndo {
		t.Fatalf("undo: %+v %v", undone, err)
	}
	agent, _ = store.Agent(ctx, wfProject, hired)
	if agent.State != agentworkforce.StateRetired || agent.ReinstateUntil == nil || agent.OnProbation() {
		t.Fatalf("undone agent: %+v", agent)
	}
	if _, err := store.UndoAutoHire(ctx, wfProject, hireAction.ID, managerActor(wfManager), "", false); refusalCode(err) != agentworkforce.ActionClosedCode {
		t.Fatalf("undo twice: %v", err)
	}

	// Keep ends the second hire's probation; then the undo window is moot.
	kept, err := store.KeepAutoHire(ctx, wfProject, secondAction.ID, managerActor(wfManager), "")
	if err != nil || kept.Status != agentworkforce.ActionKept {
		t.Fatalf("keep: %+v %v", kept, err)
	}
	if summary, err := store.WorkforcePolicyPass(ctx); err != nil || summary.Hired != 1 {
		t.Fatalf("the freed slots: %+v %v", summary, err)
	}

	// The undo window closes after 14 days.
	if _, err := pool.Exec(ctx, `UPDATE elitea_lifecycle.agent_proposals SET status = 'proposed', decided_at = NULL, created_application_id = NULL WHERE id = $1`, few); err != nil {
		t.Fatal(err)
	}
	open := actionsOf(t, store, ActionFilter{Status: agentworkforce.ActionApplied})
	if len(open) != 1 {
		t.Fatalf("one open hire: %+v", open)
	}
	now = now.Add(agentworkforce.ProbationWindow + time.Hour)
	if _, err := store.UndoAutoHire(ctx, wfProject, open[0].ID, managerActor(wfManager), "", false); refusalCode(err) != agentworkforce.ActionClosedCode {
		t.Fatalf("undo after 14 days: %v", err)
	}
}

func TestWorkforceAutoDismissStagesGraceObjectionAndExemptions(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedPolicyBase(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	onPolicy(t, store, func(p *agentworkforce.Policy) { p.AutoDismissEnabled = true; p.GraceDays = 7 })

	const (
		tripping int64 = 991760 // breaker_trips -> suspend at once
		failing  int64 = 991761 // failure_rate -> watch at once
		unused   int64 = 991762 // unused -> retire after the grace period
		objected int64 = 991763 // unused, the owner objects
		critical int64 = 991764 // critical tier: exempt
		held     int64 = 991765 // legal hold: exempt
	)
	for _, app := range []int64{tripping, failing, unused, objected, held} {
		seedWorkforceAgent(t, pool, app, "medium", `{}`)
	}
	seedWorkforceAgent(t, pool, critical, "critical", `{}`)
	wfExec(t, pool, `INSERT INTO elitea_runtime.legal_holds (scope, project_id, agent_id, reason, created_by) VALUES ('agent', 1, $1, 'litigation', 1)`, held)
	card := func(app int64, set string) {
		wfExec(t, pool, `INSERT INTO elitea_lifecycle.agent_scorecards (project_id, application_id, day, last_used_at) VALUES (1, $1, current_date, now())`, app)
		wfExec(t, pool, `UPDATE elitea_lifecycle.agent_scorecards SET `+set+` WHERE application_id = $1`, app)
	}
	card(tripping, "breaker_trips_7d = 3")
	card(failing, "runs_30d = 25, failed_30d = 20")
	card(unused, "last_used_at = now() - interval '120 days'")
	card(objected, "last_used_at = now() - interval '120 days'")
	card(critical, "breaker_trips_7d = 9, last_used_at = now() - interval '120 days'")
	card(held, "breaker_trips_7d = 9")

	summary, err := store.WorkforcePolicyPass(ctx)
	if err != nil || summary.Applied != 2 || summary.Noticed != 2 {
		t.Fatalf("first pass: %+v %v", summary, err)
	}
	a, _ := store.Agent(ctx, wfProject, tripping)
	if a.SuspensionID == 0 || a.SuspensionSource != "dismissal" {
		t.Fatalf("suspended: %+v", a)
	}
	a, _ = store.Agent(ctx, wfProject, failing)
	if !a.UnderReview || a.SuspensionID != 0 {
		t.Fatalf("watched: %+v", a)
	}
	for _, app := range []int64{unused, objected} {
		a, _ = store.Agent(ctx, wfProject, app)
		if a.State != agentworkforce.StateActive {
			t.Fatalf("retired before the grace period: %+v", a)
		}
	}
	for _, app := range []int64{critical, held} {
		a, _ = store.Agent(ctx, wfProject, app)
		if a.SuspensionID != 0 || a.State != agentworkforce.StateActive {
			t.Fatalf("an exempt agent was touched: %+v", a)
		}
		if rows := actionsOf(t, store, ActionFilter{ApplicationID: app}); len(rows) != 0 {
			t.Fatalf("an exempt agent has actions: %+v", rows)
		}
	}
	// Every policy proposal is watch, suspend or retire, sourced policy, and
	// the applied ones were decided by nobody.
	var bad int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_lifecycle.agent_workforce_proposals
WHERE source <> 'policy' OR stage NOT IN ('watch', 'suspend', 'retire') OR decided_by_user_id IS NOT NULL OR policy_rule IS NULL`).Scan(&bad)
	if bad != 0 {
		t.Fatalf("%d proposals break the policy invariants", bad)
	}

	// The owner objects to one notice; a stranger cannot.
	var notice AutomaticAction
	for _, row := range actionsOf(t, store, ActionFilter{ApplicationID: objected}) {
		notice = row
	}
	if !notice.CanObject || notice.DueAt == nil || notice.DueAt.Sub(now).Round(time.Hour) != 7*24*time.Hour {
		t.Fatalf("notice: %+v", notice)
	}
	if _, err := store.ObjectToAction(ctx, wfProject, notice.ID, WorkforceActor{UserID: 99}, ""); refusalCode(err) != agentworkforce.ForbiddenCode {
		t.Fatalf("a stranger objected: %v", err)
	}
	if obj, err := store.ObjectToAction(ctx, wfProject, notice.ID, WorkforceActor{UserID: wfOwner}, "Seasonal agent."); err != nil || obj.Status != agentworkforce.ActionObjected {
		t.Fatalf("object: %+v %v", obj, err)
	}

	// Inside the grace period nothing retires; after it, the unobjected one
	// does and the objected one stays (the rule is quiet for 30 days).
	now = now.Add(3 * 24 * time.Hour)
	if summary, err := store.WorkforcePolicyPass(ctx); err != nil || summary.Retired != 0 {
		t.Fatalf("inside the grace period: %+v %v", summary, err)
	}
	now = now.Add(5 * 24 * time.Hour)
	wfExec(t, pool, `UPDATE elitea_lifecycle.agent_scorecards SET day = $1::date`, now)
	summary, err = store.WorkforcePolicyPass(ctx)
	if err != nil || summary.Retired != 1 || summary.Noticed != 0 {
		t.Fatalf("after the grace period: %+v %v", summary, err)
	}
	a, _ = store.Agent(ctx, wfProject, unused)
	if a.State != agentworkforce.StateRetired || a.ReinstateUntil == nil {
		t.Fatalf("retired after notice: %+v", a)
	}
	a, _ = store.Agent(ctx, wfProject, objected)
	if a.State != agentworkforce.StateActive {
		t.Fatalf("the objected agent was retired: %+v", a)
	}

	// Never past retire, never delete: passes long after retention leave a
	// retired agent retired, with its definition.
	now = now.Add(agentworkforce.ReinstateWindow + 24*time.Hour)
	wfExec(t, pool, `UPDATE elitea_lifecycle.agent_scorecards SET day = $1::date`, now)
	if _, err := store.WorkforcePolicyPass(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ = store.Agent(ctx, wfProject, unused)
	var versions int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM p_1.application_versions WHERE application_id = $1`, unused).Scan(&versions)
	if a.State != agentworkforce.StateRetired || versions == 0 {
		t.Fatalf("past retire: %+v versions=%d", a, versions)
	}
	var stages []string
	_ = pool.QueryRow(ctx, `SELECT COALESCE(array_agg(DISTINCT stage ORDER BY stage), '{}') FROM elitea_lifecycle.agent_workforce_proposals`).Scan(&stages)
	for _, stage := range stages {
		if stage == "demote" || stage == "delete" {
			t.Fatalf("the policy took %s", stage)
		}
	}
	var policyAudit int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 AND event->'data'->>'action' = 'agent.workforce.policy_applied' AND event->'data'->>'principal_ref' LIKE 'policy:%'`,
		string(securityaudit.TypeAgentWorkforceChanged)).Scan(&policyAudit)
	if policyAudit != 3 {
		t.Fatalf("policy_applied audit events = %d", policyAudit)
	}

	// Turning the policy off cancels the open notices.
	const late int64 = 991766
	seedWorkforceAgent(t, pool, late, "medium", `{}`)
	wfExec(t, pool, `INSERT INTO elitea_lifecycle.agent_scorecards (project_id, application_id, day, last_used_at) VALUES (1, $1, $2::date, now() - interval '200 days')`, late, now)
	if summary, err := store.WorkforcePolicyPass(ctx); err != nil || summary.Noticed != 1 {
		t.Fatalf("late notice: %+v %v", summary, err)
	}
	onPolicy(t, store, func(p *agentworkforce.Policy) { p.AutoDismissEnabled = false })
	if open := actionsOf(t, store, ActionFilter{Status: "open"}); len(open) != 0 {
		t.Fatalf("open after turning off: %+v", open)
	}
}

func TestWorkforceAutoHiredProbationFailureRetiresAndReinstateRestartsProbation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedPolicyBase(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	seedWorkforceAgent(t, pool, policyProposer, "medium", `{}`)
	onPolicy(t, store, func(p *agentworkforce.Policy) { p.AutoHireEnabled = true; p.AutoDismissEnabled = true; p.GraceDays = 1 })
	proposal := seedHireProposal(t, pool, 1, []string{"github/read_file"}, 3, 0)
	if summary, err := store.WorkforcePolicyPass(ctx); err != nil || summary.Hired != 1 {
		t.Fatalf("hire: %+v %v", summary, err)
	}
	var hired int64
	_ = pool.QueryRow(ctx, `SELECT created_application_id FROM elitea_lifecycle.agent_proposals WHERE id = $1`, proposal).Scan(&hired)
	wfExec(t, pool, `INSERT INTO elitea_lifecycle.agent_scorecards (project_id, application_id, day, guardrail_hits_30d, last_used_at) VALUES (1, $1, current_date, 3, now())`, hired)
	if summary, err := store.WorkforcePolicyPass(ctx); err != nil || summary.Noticed != 1 {
		t.Fatalf("probation failed notice: %+v %v", summary, err)
	}
	now = now.Add(25 * time.Hour)
	wfExec(t, pool, `UPDATE elitea_lifecycle.agent_scorecards SET day = $1::date`, now)
	if summary, err := store.WorkforcePolicyPass(ctx); err != nil || summary.Retired != 1 {
		t.Fatalf("retire after grace: %+v %v", summary, err)
	}
	if _, err := store.Reinstate(ctx, wfProject, hired, managerActor(wfManager), ""); err != nil {
		t.Fatal(err)
	}
	agent, _ := store.Agent(ctx, wfProject, hired)
	if agent.State != agentworkforce.StateActive || !agent.OnProbation() || agent.ProbationUntil == nil ||
		agent.ProbationUntil.Before(now.Add(agentworkforce.ProbationWindow-time.Minute)) {
		t.Fatalf("reinstated without a fresh probation: %+v", agent)
	}
}

func TestWorkforceOrphanIsFlaggedThenWatchedNeverRetired(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedPolicyBase(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	onPolicy(t, store, func(p *agentworkforce.Policy) { p.AutoDismissEnabled = true; p.Rules = []agentworkforce.Rule{} })
	const (
		orphan int64 = 991770
		owned  int64 = 991771
	)
	seedWorkforceAgent(t, pool, orphan, "medium", `{}`)
	seedWorkforceAgent(t, pool, owned, "medium", `{}`)
	// Person 4 owns both; only 991771's owner is a member: give it owner 5.
	wfExec(t, pool, `
CREATE TABLE IF NOT EXISTS public.auth_core__project_role (id SERIAL PRIMARY KEY, project_id INTEGER NOT NULL, name TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS public.auth_core__project_user_role (project_id INTEGER NOT NULL, user_id INTEGER NOT NULL, role_id INTEGER NOT NULL)`)
	wfExec(t, pool, `INSERT INTO public.auth_core__project_role (project_id, name) VALUES (1, 'viewer')`)
	wfExec(t, pool, `INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT 1, 5, id FROM public.auth_core__project_role WHERE project_id = 1 AND name = 'viewer'`)
	wfExec(t, pool, `UPDATE elitea_lifecycle.agent_registry SET business_owner_id = 5, technical_owner_id = 5 WHERE application_id = $1`, owned)

	summary, err := store.WorkforcePolicyPass(ctx)
	if err != nil || summary.Orphans != 1 {
		t.Fatalf("flag: %+v %v", summary, err)
	}
	a, _ := store.Agent(ctx, wfProject, orphan)
	if a.OrphanedSince == nil || a.UnderReview {
		t.Fatalf("flagged only: %+v", a)
	}
	if summary, _ := store.WorkforcePolicyPass(ctx); summary.Orphans != 0 {
		t.Fatalf("flagged twice: %+v", summary)
	}
	now = now.Add(agentworkforce.OrphanWatchAfter + time.Hour)
	if summary, err := store.WorkforcePolicyPass(ctx); err != nil || summary.Applied != 1 {
		t.Fatalf("watch after 30 days: %+v %v", summary, err)
	}
	a, _ = store.Agent(ctx, wfProject, orphan)
	if !a.UnderReview || a.State != agentworkforce.StateActive || a.SuspensionID != 0 {
		t.Fatalf("watched, not retired: %+v", a)
	}
	now = now.Add(365 * 24 * time.Hour)
	if _, err := store.WorkforcePolicyPass(ctx); err != nil {
		t.Fatal(err)
	}
	if a, _ = store.Agent(ctx, wfProject, orphan); a.State != agentworkforce.StateActive {
		t.Fatalf("an orphan was retired: %+v", a)
	}
}
