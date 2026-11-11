package repos

// The workforce policy pass's batched reads (#1876) and the automatic hire
// through the applications create path (#1877), against a REAL migrated
// database. It SKIPS without ELITEA_TEST_DATABASE_URL, and a skip prints the
// same `ok`, so each case asserts values a skip could not have produced.

import (
	"context"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
)

// The hire is written by createApplicationTx, the create route's own path:
// the draft's welcome message and conversation starters reach the version,
// the version stamps the agent's updated_at, and the hire's invariants hold.
func TestWorkforceAutoHireWritesThroughTheApplicationsCreatePath(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedPolicyBase(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	seedWorkforceAgent(t, pool, policyProposer, "medium", `{}`)
	onPolicy(t, store, func(p *agentworkforce.Policy) { p.AutoHireEnabled = true; p.MaxAutoHired = 1 })

	id := seedHireProposal(t, pool, 21, []string{"github/read_file"}, 3, 0)
	wfExec(t, pool, `UPDATE elitea_lifecycle.agent_proposals
SET draft = draft || '{"welcome_message":"Hello.","conversation_starters":["Summarise the repo", "", 7]}'::jsonb WHERE id = $1`, id)

	summary, err := store.WorkforcePolicyPass(ctx)
	if err != nil || summary.Hired != 1 {
		t.Fatalf("pass: %+v %v", summary, err)
	}
	var app int64
	if err := pool.QueryRow(ctx, `SELECT created_application_id FROM elitea_lifecycle.agent_proposals WHERE id = $1`, id).Scan(&app); err != nil {
		t.Fatal(err)
	}
	var (
		name, welcome, status, agentType, memoryWrite, soulName string
		starters                                                []string
		author, owner                                           int64
		stamped                                                 bool
		llm                                                     string
	)
	if err := pool.QueryRow(ctx, `
SELECT a.name, a.owner_id, a.updated_at IS NOT NULL, v.welcome_message, v.status, v.agent_type, v.author_id,
       ARRAY(SELECT jsonb_array_elements_text(v.conversation_starters::jsonb)), v.llm_settings::text,
       v.meta->'memory'->>'write', v.soul->>'display_name'
FROM p_1.applications a JOIN p_1.application_versions v ON v.application_id = a.id WHERE a.id = $1`, app).Scan(
		&name, &owner, &stamped, &welcome, &status, &agentType, &author, &starters, &llm, &memoryWrite, &soulName); err != nil {
		t.Fatal(err)
	}
	if name != "Hired 21" || owner != wfProject || !stamped || welcome != "Hello." || status != "draft" ||
		agentType != defaultAgentType || author != wfOwner || len(starters) != 1 || starters[0] != "Summarise the repo" ||
		llm != "{}" || memoryWrite != "review" || soulName != "Ada" {
		t.Fatalf("the hire: name=%q owner=%d stamped=%v welcome=%q status=%q type=%q author=%d starters=%v llm=%s memory=%q soul=%q",
			name, owner, stamped, welcome, status, agentType, author, starters, llm, memoryWrite, soulName)
	}
	agent, err := store.Agent(ctx, wfProject, app)
	if err != nil || agent.Tier != "low" || !agent.OnProbation() || agent.BusinessOwnerID != wfOwner {
		t.Fatalf("registry: %+v %v", agent, err)
	}
}

// At max_auto_hired the pass opens no transaction: a proposal another
// session holds locked does not block it (#1876).
func TestWorkforceAutoHireAtTheCapTouchesNoProposal(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedPolicyBase(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	seedWorkforceAgent(t, pool, policyProposer, "medium", `{}`)
	onPolicy(t, store, func(p *agentworkforce.Policy) { p.AutoHireEnabled = true; p.MaxAutoHired = 1 })
	seedHireProposal(t, pool, 31, []string{"github/read_file"}, 3, 0)
	if summary, err := store.WorkforcePolicyPass(ctx); err != nil || summary.Hired != 1 {
		t.Fatalf("first pass: %+v %v", summary, err)
	}
	locked := seedHireProposal(t, pool, 32, []string{"github/read_file"}, 3, 0)

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM elitea_lifecycle.agent_proposals WHERE id = $1 FOR UPDATE`, locked); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	summary, err := store.WorkforcePolicyPass(bounded)
	if err != nil || summary.Hired != 0 {
		t.Fatalf("the pass at the cap: %+v %v", summary, err)
	}
	_ = holder.Rollback(ctx)
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM elitea_lifecycle.agent_proposals WHERE id = $1`, locked).Scan(&status)
	if status != "proposed" {
		t.Fatalf("the capped proposal became %q", status)
	}
}

// The batched rule pass reads every agent at once and still skips what the
// per-agent reads skipped: a stale card, an open review, an agent already at
// the stage, a project-wide legal hold.
func TestWorkforceRulePassBatchedReadsKeepTheSkips(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedPolicyBase(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	onPolicy(t, store, func(p *agentworkforce.Policy) { p.AutoDismissEnabled = true })

	const (
		stale     int64 = 991880 // tripping, but its card is four days old
		reviewing int64 = 991881 // tripping, a person's proposal is open
		suspended int64 = 991882 // tripping, already suspended by an admin
		acted     int64 = 991883 // tripping: suspended by the pass
		quiet     int64 = 991884 // a clean card
	)
	for _, app := range []int64{stale, reviewing, suspended, acted, quiet} {
		seedWorkforceAgent(t, pool, app, "medium", `{}`)
	}
	card := func(app int64, day string, trips int) {
		wfExec(t, pool, `INSERT INTO elitea_lifecycle.agent_scorecards (project_id, application_id, day, last_used_at, breaker_trips_7d)
VALUES (1, $1, `+day+`, now(), $2)`, app, trips)
	}
	card(stale, "current_date - 4", 9)
	card(reviewing, "current_date", 9)
	card(suspended, "current_date", 9)
	card(acted, "current_date - 3", 1) // an older card the newest one replaces
	card(acted, "current_date", 9)
	card(quiet, "current_date", 0)
	propose(t, store, reviewing, agentworkforce.StageWatch, agentworkforce.Narrowing{})
	wfExec(t, pool, `INSERT INTO elitea_runtime.execution_suspensions (scope, project_id, scope_key, reason, actor_id, source)
VALUES ('agent', 1, $1, 'x', 7, 'manual')`, "991882")

	summary, err := store.WorkforcePolicyPass(ctx)
	if err != nil || summary.Applied != 1 {
		t.Fatalf("pass: %+v %v", summary, err)
	}
	for app, want := range map[int64]int{stale: 0, reviewing: 0, suspended: 0, acted: 1, quiet: 0} {
		if got := len(actionsOf(t, store, ActionFilter{ApplicationID: app})); got != want {
			t.Errorf("agent %d: %d actions, want %d", app, got, want)
		}
	}

	// A project-wide hold exempts every agent.
	const late int64 = 991885
	seedWorkforceAgent(t, pool, late, "medium", `{}`)
	card(late, "current_date", 9)
	wfExec(t, pool, `INSERT INTO elitea_runtime.legal_holds (scope, project_id, reason, created_by) VALUES ('project', 1, 'audit', 1)`)
	if summary, err := store.WorkforcePolicyPass(ctx); err != nil || summary.Applied != 0 {
		t.Fatalf("under a project hold: %+v %v", summary, err)
	}
}

// A hire whose create path wrote no registry row (a trigger swallows it
// here) is not recorded: no tier or probation would hold it, so the
// proposal stays proposed and no auto_hire action is written.
func TestWorkforceAutoHireWithoutARegistryRowIsNotRecorded(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedPolicyBase(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	seedWorkforceAgent(t, pool, policyProposer, "medium", `{}`)
	onPolicy(t, store, func(p *agentworkforce.Policy) { p.AutoHireEnabled = true; p.MaxAutoHired = 1 })
	id := seedHireProposal(t, pool, 41, []string{"github/read_file"}, 3, 0)
	wfExec(t, pool, `
CREATE FUNCTION wf_test_skip_registry() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$`)
	wfExec(t, pool, `
CREATE TRIGGER skip_registry BEFORE INSERT ON elitea_lifecycle.agent_registry
FOR EACH ROW EXECUTE FUNCTION wf_test_skip_registry()`)

	summary, _ := store.WorkforcePolicyPass(ctx)
	if summary.Hired != 0 {
		t.Fatalf("a hire with no registry row was recorded: %+v", summary)
	}
	var status string
	var actions int
	if err := pool.QueryRow(ctx, `SELECT status FROM elitea_lifecycle.agent_proposals WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_lifecycle.agent_automatic_actions WHERE kind = $1`,
		agentworkforce.ActionAutoHire).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if status != "proposed" || actions != 0 {
		t.Fatalf("status=%q auto_hire actions=%d", status, actions)
	}
}
