package repos

// The agent workforce (shared/0236, ADR-0050 decisions 5g-5j, #1616)
// against a REAL migrated database: one case per ladder stage, the HR-agent
// guards, the appeal by a different manager and the reinstatement. It SKIPS
// without ELITEA_TEST_DATABASE_URL, and a skip prints the same `ok`, so each
// case asserts values a skip could not have produced.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	wfProject  int64 = 1
	wfOwner    int64 = 4
	wfManager  int64 = 7
	wfManager2 int64 = 8
)

func workforceStore(t *testing.T, pool *pgxpool.Pool, now *time.Time) *AgentWorkforce {
	t.Helper()
	appender, err := securityaudit.NewPostgresAppender("elitea-main/integration-test")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewAgentWorkforce(pool, appender)
	if err != nil {
		t.Fatal(err)
	}
	return store.WithClock(func() time.Time { return *now })
}

func wfExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

// seedWorkforceAgent writes an agent with a published and a draft version,
// owned by person 4, at the given tier; meta sets the versions' meta.
func seedWorkforceAgent(t *testing.T, pool *pgxpool.Pool, id int64, tier, meta string) (published, draft int64) {
	t.Helper()
	published, draft = id*10+1, id*10+2
	wfExec(t, pool, `INSERT INTO centry.project (id) VALUES (1) ON CONFLICT DO NOTHING`)
	wfExec(t, pool, `INSERT INTO p_1.applications (id, name, owner_id) VALUES ($1, $2, 4) ON CONFLICT (id) DO NOTHING`,
		id, fmt.Sprintf("Agent %d", id))
	wfExec(t, pool, `
INSERT INTO p_1.application_versions (id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at)
VALUES ($1, $3, 'v1', 'published', 4, gen_random_uuid(), '{}'::jsonb, 'x', '[]'::json, '', 'agent', $4::jsonb, '{}'::jsonb, now() AT TIME ZONE 'UTC'),
       ($2, $3, 'base', 'draft', 4, gen_random_uuid(), '{}'::jsonb, 'x', '[]'::json, '', 'agent', $4::jsonb, '{}'::jsonb, now() AT TIME ZONE 'UTC')
ON CONFLICT (id) DO NOTHING`, published, draft, id, meta)
	wfExec(t, pool, `
INSERT INTO elitea_lifecycle.agent_registry (tenant_project_id, application_id, business_owner_id, technical_owner_id, risk_tier)
VALUES (1, $1, 4, 4, $2)
ON CONFLICT (tenant_project_id, application_id) DO UPDATE SET risk_tier = EXCLUDED.risk_tier`, id, tier)
	return published, draft
}

func managerActor(id int64) WorkforceActor { return WorkforceActor{UserID: id, Manager: true} }

func propose(t *testing.T, store *AgentWorkforce, app int64, stage agentworkforce.Stage, narrowing agentworkforce.Narrowing) WorkforceProposal {
	t.Helper()
	proposal, err := store.ProposeByPerson(context.Background(), NewWorkforceProposal{
		ProjectID: wfProject, ApplicationID: app, Actor: managerActor(wfManager),
		Input: agentworkforce.ProposalInput{Stage: stage, Rationale: "The scorecard is poor.", Narrowing: narrowing},
	})
	if err != nil {
		t.Fatalf("propose %s: %v", stage, err)
	}
	return proposal
}

func approve(t *testing.T, store *AgentWorkforce, proposal WorkforceProposal, confirmNested bool) WorkforceProposal {
	t.Helper()
	decided, err := store.Approve(context.Background(), Decision{
		ProjectID: wfProject, ProposalID: proposal.ID, Actor: managerActor(wfManager), ConfirmNested: confirmNested,
	})
	if err != nil {
		t.Fatalf("approve %s: %v", proposal.Stage, err)
	}
	if decided.Status != agentworkforce.StatusApproved {
		t.Fatalf("status = %q", decided.Status)
	}
	return decided
}

func refusalCode(err error) string {
	var refusal *agentworkforce.Refusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

func TestWorkforceWatchStageAndRejection(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	const app int64 = 991701
	seedWorkforceAgent(t, pool, app, "medium", `{}`)

	// A rejection keeps the agent and frees the slot.
	first := propose(t, store, app, agentworkforce.StageWatch, agentworkforce.Narrowing{})
	if _, err := store.ProposeByPerson(ctx, NewWorkforceProposal{ProjectID: wfProject, ApplicationID: app, Actor: managerActor(wfManager),
		Input: agentworkforce.ProposalInput{Stage: agentworkforce.StageRetire, Rationale: "again"}}); refusalCode(err) != agentworkforce.OpenProposalCode {
		t.Fatalf("second open proposal: %v", err)
	}
	if _, err := store.Reject(ctx, Decision{ProjectID: wfProject, ProposalID: first.ID, Actor: WorkforceActor{UserID: 99}}); refusalCode(err) != agentworkforce.ForbiddenCode {
		t.Fatalf("a stranger decided: %v", err)
	}
	rejected, err := store.Reject(ctx, Decision{ProjectID: wfProject, ProposalID: first.ID, Actor: managerActor(wfManager)})
	if err != nil || rejected.Status != agentworkforce.StatusRejected {
		t.Fatalf("reject: %v %q", err, rejected.Status)
	}

	watch := approve(t, store, propose(t, store, app, agentworkforce.StageWatch, agentworkforce.Narrowing{}), false)
	agent, err := store.Agent(ctx, wfProject, app)
	if err != nil || !agent.UnderReview || agent.UnderReviewSince == nil || agent.State != agentworkforce.StateActive {
		t.Fatalf("watch: %+v %v", agent, err)
	}
	if watch.Applied["under_review"] != true {
		t.Fatalf("applied = %v", watch.Applied)
	}
	var actions []string
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(array_agg(event->'data'->>'action' ORDER BY seq), '{}') FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 AND event->'data'->>'resource_id' = $2`,
		string(securityaudit.TypeAgentWorkforceChanged), strconv.FormatInt(watch.ID, 10)).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(actions, []string{"agent.workforce.proposed", "agent.workforce.approved"}) {
		t.Fatalf("audit actions = %v", actions)
	}
}

func TestWorkforceDemoteNarrowsTheDraftAndDropsTheLead(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	const app int64 = 991702
	published, draft := seedWorkforceAgent(t, pool, app, "low",
		`{"capabilities":{"can_propose_agents":true,"can_review_agents":true},"memory":{"write":"auto"},"proactivity":{"mode":"heartbeat","interval":"30m"}}`)
	wfExec(t, pool, `INSERT INTO p_1.agent_heartbeat_settings (application_id, enabled, owner_user_id) VALUES ($1, true, 4)`, app)
	wfExec(t, pool, `INSERT INTO p_1.elitea_tools (id, type, name, description, settings, author_id, meta, created_at)
VALUES (9917021, 'github', 'gh', '', '{}'::jsonb, 4, '{}'::jsonb, now() AT TIME ZONE 'UTC'), (9917022, 'jira', 'jira', '', '{}'::jsonb, 4, '{}'::jsonb, now() AT TIME ZONE 'UTC')
ON CONFLICT (id) DO NOTHING`)
	wfExec(t, pool, `INSERT INTO p_1.entity_tool_mapping (tool_id, entity_id, entity_version_id, entity_type, selected_tools)
VALUES (9917021, $1, $2, 'agent', '[]'::jsonb), (9917022, $1, $2, 'agent', '[]'::jsonb)`, app, draft)
	conversation, _ := seedRoster(t, pool, app, true)

	decided := approve(t, store, propose(t, store, app, agentworkforce.StageDemote, agentworkforce.Narrowing{
		RemoveToolkitIDs: []int64{9917021}, MemoryReview: true, RemoveCapabilities: []string{"can_review_agents"}, RemoveLead: true, ProactivityOff: true,
	}), false)
	if decided.Kind != "demote" || decided.Applied["draft_version_id"] != float64(draft) {
		t.Fatalf("applied = %v", decided.Applied)
	}
	var meta string
	if err := pool.QueryRow(ctx, `SELECT meta::text FROM p_1.application_versions WHERE id = $1`, draft).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(meta, `"mode": "off"`) || !strings.Contains(meta, `"interval": "30m"`) || decided.Applied["heartbeat_stopped"] != true {
		t.Fatalf("the heartbeat was not narrowed: %s applied=%v", meta, decided.Applied)
	}
	var heartbeatEnabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM p_1.agent_heartbeat_settings WHERE application_id = $1`, app).Scan(&heartbeatEnabled); err != nil || heartbeatEnabled {
		t.Fatalf("the heartbeat row still runs: %v %v", heartbeatEnabled, err)
	}
	if want := `"write": "review"`; !strings.Contains(meta, want) || strings.Contains(meta, "can_review_agents") || !strings.Contains(meta, "can_propose_agents") {
		t.Fatalf("draft meta = %s", meta)
	}
	var publishedMeta string
	_ = pool.QueryRow(ctx, `SELECT meta::text FROM p_1.application_versions WHERE id = $1`, published).Scan(&publishedMeta)
	if !strings.Contains(publishedMeta, "can_review_agents") {
		t.Fatalf("the published release changed: %s", publishedMeta)
	}
	var tools int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM p_1.entity_tool_mapping WHERE entity_version_id = $1`, draft).Scan(&tools)
	if tools != 1 {
		t.Fatalf("toolkits on the draft = %d, want 1", tools)
	}
	var floor *string
	_ = pool.QueryRow(ctx, `SELECT meta -> 'floor' ->> 'policy' FROM p_1.chat_conversations WHERE id = $1`, conversation).Scan(&floor)
	if floor != nil {
		t.Fatalf("the lead remained: %v", *floor)
	}
}

// seedRoster puts the agent in a conversation (as lead when lead) and gives
// it an active schedule. It answers the conversation and participant ids.
func seedRoster(t *testing.T, pool *pgxpool.Pool, app int64, lead bool) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	var conversation, participant int64
	wfExec(t, pool, `SELECT setval(pg_get_serial_sequence('p_1.chat_conversations', 'id'), GREATEST(1000, (SELECT max(id) FROM p_1.chat_conversations)));
SELECT setval(pg_get_serial_sequence('p_1.chat_participants', 'id'), GREATEST(1000, (SELECT max(id) FROM p_1.chat_participants)))`)
	if err := pool.QueryRow(ctx, `INSERT INTO p_1.chat_conversations (name, author_id, created_at) VALUES ('team', 4, now() AT TIME ZONE 'UTC') RETURNING id`).Scan(&conversation); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta) VALUES (gen_random_uuid(), 'application', jsonb_build_object('id', $1::bigint, 'project_id', 1))
RETURNING id`, app).Scan(&participant); err != nil {
		t.Fatal(err)
	}
	wfExec(t, pool, `INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at) VALUES ($1, $2, '{"version_id": 1}'::jsonb, now() AT TIME ZONE 'UTC')`,
		conversation, participant)
	if lead {
		wfExec(t, pool, `UPDATE p_1.chat_conversations SET meta = jsonb_build_object('floor', jsonb_build_object('policy', 'lead', 'lead_participant_id', $2::bigint)) WHERE id = $1`,
			conversation, participant)
	}
	return conversation, participant
}

func TestWorkforceSuspendWritesADismissalSuspensionAndTheAppealResumesIt(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	const app int64 = 991703
	seedWorkforceAgent(t, pool, app, "medium", `{}`)

	decided := approve(t, store, propose(t, store, app, agentworkforce.StageSuspend, agentworkforce.Narrowing{}), false)
	agent, err := store.Agent(ctx, wfProject, app)
	if err != nil || agent.SuspensionID == 0 || agent.SuspensionSource != "dismissal" {
		t.Fatalf("suspend: %+v %v", agent, err)
	}

	// Only the owner appeals; the manager who decided cannot review it.
	if _, err := store.Appeal(ctx, wfProject, decided.ID, wfManager2, "unfair"); refusalCode(err) != agentworkforce.ForbiddenCode {
		t.Fatalf("a non-owner appealed: %v", err)
	}
	appealed, err := store.Appeal(ctx, wfProject, decided.ID, wfOwner, "The failures were an upstream outage.")
	if err != nil || appealed.AppealState == nil || *appealed.AppealState != agentworkforce.AppealOpen {
		t.Fatalf("appeal: %v %+v", err, appealed.AppealState)
	}
	if _, err := store.DecideAppeal(ctx, AppealDecision{ProjectID: wfProject, ProposalID: decided.ID, Actor: managerActor(wfManager), Grant: true}); refusalCode(err) != agentworkforce.ForbiddenCode {
		t.Fatalf("the deciding manager reviewed the appeal: %v", err)
	}
	granted, err := store.DecideAppeal(ctx, AppealDecision{ProjectID: wfProject, ProposalID: decided.ID, Actor: managerActor(wfManager2), Grant: true, Note: "Outage confirmed."})
	if err != nil || *granted.AppealState != agentworkforce.AppealGranted {
		t.Fatalf("grant: %v", err)
	}
	agent, _ = store.Agent(ctx, wfProject, app)
	if agent.SuspensionID != 0 {
		t.Fatalf("the dismissal suspension is still active: %+v", agent)
	}
	if _, err := store.Appeal(ctx, wfProject, decided.ID, wfOwner, "again"); refusalCode(err) != agentworkforce.AppealClosedCode {
		t.Fatalf("a second appeal: %v", err)
	}
}

func TestWorkforceRetireRefusesRunsAndReinstatementRestoresTheAgent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	const app, parent int64 = 991704, 991705
	published, _ := seedWorkforceAgent(t, pool, app, "medium", `{}`)
	_, parentDraft := seedWorkforceAgent(t, pool, parent, "medium", `{}`)
	wfExec(t, pool, `INSERT INTO elitea_lifecycle.application_releases
    (tenant_project_id, application_id, application_version_id, name, definition_digest, definition, toolkit_pins, state)
VALUES (1, $1, $2, 'v1', decode(repeat('ab', 32), 'hex'), '{}'::jsonb, '[]'::jsonb, 'published')`, app, published)
	wfExec(t, pool, `INSERT INTO elitea_lifecycle.application_release_heads (tenant_project_id, application_id, release_id, cause)
SELECT 1, $1, id, 'publish' FROM elitea_lifecycle.application_releases WHERE tenant_project_id = 1 AND application_id = $1`, app)
	conversation, participant := seedRoster(t, pool, app, true)
	wfExec(t, pool, `INSERT INTO p_1.pipeline_schedules (application_id, version_id, cron, active, author_id) VALUES ($1, $2, '0 * * * *', true, 4)`, app, published)
	// A proactive agent (#1618): retirement stops its heartbeat.
	wfExec(t, pool, `INSERT INTO p_1.agent_heartbeat_settings (application_id, enabled, owner_user_id, next_wake_at, paused_until)
VALUES ($1, true, 4, now() - interval '40 days', now() - interval '39 days')`, app)
	heartbeatOn := func() (enabled, fresh bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT enabled, next_wake_at > now() - interval '1 minute' AND paused_until IS NULL
FROM p_1.agent_heartbeat_settings WHERE application_id = $1`, app).Scan(&enabled, &fresh); err != nil {
			t.Fatal(err)
		}
		return enabled, fresh
	}
	// The parent nests the agent.
	wfExec(t, pool, `INSERT INTO p_1.elitea_tools (id, type, name, description, settings, author_id, meta, created_at)
VALUES (9917051, 'application', 'child', '', jsonb_build_object('application_id', $1::bigint::text, 'application_version_id', $2::bigint::text), 4, '{}'::jsonb, now() AT TIME ZONE 'UTC')
ON CONFLICT (id) DO NOTHING`, app, published)
	wfExec(t, pool, `INSERT INTO p_1.entity_tool_mapping (tool_id, entity_id, entity_version_id, entity_type, selected_tools)
VALUES (9917051, $1, $2, 'agent', '[]'::jsonb)`, parent, parentDraft)

	impact, err := store.Impact(ctx, wfProject, app)
	if err != nil || impact.Conversations != 1 || impact.ConversationsLed != 1 || impact.Schedules != 1 ||
		len(impact.NestedBy) != 1 || impact.NestedBy[0].ApplicationID != parent || impact.PublishedVersions != 1 {
		t.Fatalf("impact = %+v %v", impact, err)
	}

	retire := propose(t, store, app, agentworkforce.StageRetire, agentworkforce.Narrowing{})
	if _, err := store.Approve(ctx, Decision{ProjectID: wfProject, ProposalID: retire.ID, Actor: managerActor(wfManager)}); refusalCode(err) != agentworkforce.ConfirmNestedCode {
		t.Fatalf("retire without confirming the nesting: %v", err)
	}
	approve(t, store, retire, true)

	agent, _ := store.Agent(ctx, wfProject, app)
	if agent.State != agentworkforce.StateRetired || agent.ReinstateUntil == nil || agent.PurgeAfter == nil {
		t.Fatalf("retired = %+v", agent)
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM p_1.application_versions WHERE id = $1`, published).Scan(&status)
	var heads, rosters, active int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_lifecycle.application_release_heads WHERE tenant_project_id = 1 AND application_id = $1`, app).Scan(&heads)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_participant_mapping WHERE participant_id = $1`, participant).Scan(&rosters)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM p_1.pipeline_schedules WHERE application_id = $1 AND active`, app).Scan(&active)
	if status != "draft" || heads != 0 || rosters != 0 || active != 0 {
		t.Fatalf("after retire: status=%s heads=%d rosters=%d schedules=%d", status, heads, rosters, active)
	}
	if enabled, _ := heartbeatOn(); enabled {
		t.Fatal("the heartbeat still runs after retirement")
	}

	// Admission's read: the agent and its parent are refused.
	retired, err := store.RetiredAgents(ctx, wfProject, []int64{app, parent})
	if err != nil || !retired[app] || retired[parent] {
		t.Fatalf("retired agents = %v %v", retired, err)
	}

	// Delete is refused inside the reinstate window.
	del := propose(t, store, app, agentworkforce.StageDelete, agentworkforce.Narrowing{})
	if _, err := store.Approve(ctx, Decision{ProjectID: wfProject, ProposalID: del.ID, Actor: managerActor(wfManager)}); refusalCode(err) != agentworkforce.RetentionCode {
		t.Fatalf("delete inside the window: %v", err)
	}
	if _, err := store.Reject(ctx, Decision{ProjectID: wfProject, ProposalID: del.ID, Actor: managerActor(wfManager)}); err != nil {
		t.Fatal(err)
	}

	// Reinstate restores what retirement changed.
	if _, err := store.Reinstate(ctx, wfProject, app, WorkforceActor{UserID: wfOwner}, ""); refusalCode(err) != agentworkforce.ForbiddenCode {
		t.Fatalf("the owner reinstated: %v", err)
	}
	reinstated, err := store.Reinstate(ctx, wfProject, app, managerActor(wfManager2), "Mistake.")
	if err != nil || reinstated.State != agentworkforce.StateActive {
		t.Fatalf("reinstate: %+v %v", reinstated, err)
	}
	_ = pool.QueryRow(ctx, `SELECT status FROM p_1.application_versions WHERE id = $1`, published).Scan(&status)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_lifecycle.application_release_heads WHERE tenant_project_id = 1 AND application_id = $1`, app).Scan(&heads)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_participant_mapping WHERE participant_id = $1`, participant).Scan(&rosters)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM p_1.pipeline_schedules WHERE application_id = $1 AND active`, app).Scan(&active)
	var lead *string
	_ = pool.QueryRow(ctx, `SELECT meta -> 'floor' ->> 'lead_participant_id' FROM p_1.chat_conversations WHERE id = $1`, conversation).Scan(&lead)
	if status != "published" || heads != 1 || rosters != 1 || active != 1 || lead == nil || *lead != strconv.FormatInt(participant, 10) {
		t.Fatalf("after reinstate: status=%s heads=%d rosters=%d schedules=%d lead=%v", status, heads, rosters, active, lead)
	}
	if enabled, fresh := heartbeatOn(); !enabled || !fresh {
		t.Fatalf("after reinstate: heartbeat enabled=%v rescheduled=%v", enabled, fresh)
	}
	retired, _ = store.RetiredAgents(ctx, wfProject, []int64{app})
	if retired[app] {
		t.Fatal("still retired")
	}
}

func TestWorkforceDeleteAfterRetentionAndLegalHold(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	const app int64 = 991706
	seedWorkforceAgent(t, pool, app, "medium", `{}`)
	approve(t, store, propose(t, store, app, agentworkforce.StageRetire, agentworkforce.Narrowing{}), false)

	now = now.Add(agentworkforce.ReinstateWindow + time.Hour)
	if _, err := store.Reinstate(ctx, wfProject, app, managerActor(wfManager), ""); refusalCode(err) != agentworkforce.ReinstateClosedCode {
		t.Fatalf("reinstate after the window: %v", err)
	}
	wfExec(t, pool, `INSERT INTO elitea_runtime.legal_holds (scope, project_id, agent_id, reason, created_by) VALUES ('agent', 1, $1, 'litigation', 1)`, app)
	del := propose(t, store, app, agentworkforce.StageDelete, agentworkforce.Narrowing{})
	if _, err := store.Approve(ctx, Decision{ProjectID: wfProject, ProposalID: del.ID, Actor: managerActor(wfManager)}); refusalCode(err) != agentworkforce.LegalHoldCode {
		t.Fatalf("delete under a legal hold: %v", err)
	}
	wfExec(t, pool, `UPDATE elitea_runtime.legal_holds SET released_at = now(), released_by = 1 WHERE agent_id = $1`, app)
	approve(t, store, del, false)
	agent, _ := store.Agent(ctx, wfProject, app)
	var versions int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM p_1.application_versions WHERE application_id = $1`, app).Scan(&versions)
	if agent.State != agentworkforce.StateDeleted || versions != 0 {
		t.Fatalf("deleted: %+v versions=%d", agent, versions)
	}
}

// seedHRRun writes a claimed run of proposer whose frozen version sets
// can_review_agents to hr.
func seedHRRun(t *testing.T, pool *pgxpool.Pool, executionID string, proposer int64, hr bool) {
	t.Helper()
	seedEgressExecution(t, pool, executionID, wfProject, proposer)
	input := agentCapacityInput("gen-" + executionID)
	input.Application = fmt.Appendf(nil, `{"id":%d,"version_details":{"meta":{"capabilities":{"can_review_agents":%t}}}}`, proposer, hr)
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	wfExec(t, pool, `UPDATE elitea_runtime.input_bundle_entries
SET content_bytes = $2, content_digest = $3, content_size = $4, semantic_role = 'agent.execution_request'
WHERE input_bundle_id = $1`, "bundle-"+executionID, encoded, digest[:], len(encoded))
	wfExec(t, pool, `UPDATE elitea_runtime.execution_jobs SET actor_id = '4', principal_ref = '4' WHERE execution_id = $1`, executionID)
}

func TestWorkforceHRAgentGuards(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	const (
		hr, target, otherHR, high, critical, held, hirer, plain int64 = 991710, 991711, 991712, 991713, 991714, 991715, 991716, 991717
	)
	seedWorkforceAgent(t, pool, hr, "medium", `{"capabilities":{"can_review_agents":true}}`)
	seedWorkforceAgent(t, pool, target, "low", `{}`)
	seedWorkforceAgent(t, pool, otherHR, "low", `{"capabilities":{"can_review_agents":true}}`)
	seedWorkforceAgent(t, pool, high, "high", `{}`)
	seedWorkforceAgent(t, pool, critical, "critical", `{}`)
	seedWorkforceAgent(t, pool, held, "low", `{}`)
	seedWorkforceAgent(t, pool, hirer, "low", `{}`)
	seedWorkforceAgent(t, pool, plain, "medium", `{}`)
	wfExec(t, pool, `INSERT INTO elitea_runtime.legal_holds (scope, project_id, agent_id, reason, created_by) VALUES ('agent', 1, $1, 'hold', 1)`, held)
	wfExec(t, pool, `
INSERT INTO elitea_lifecycle.agent_proposals (project_id, proposer_application_id, proposed_for_user_id, execution_id,
    ephemeral_agent_id, spec_digest, rationale, draft, status, decided_by_user_id, decided_at, created_application_id)
VALUES (1, $1, 4, 'exec-hire', 'a_0000abcd', repeat('a', 64), 'hire', '{}'::jsonb, 'created', 4, now(), $2)`, hirer, hr)

	seedHRRun(t, pool, "exec-hr-1616", hr, true)
	seedHRRun(t, pool, "exec-plain-1616", plain, false)
	run, err := store.ResolveProposeDismissalRun(ctx, wfProject, "exec-hr-1616", 1)
	if err != nil || !run.IsHRAgent || run.ApplicationID != hr || run.ActorID != 4 {
		t.Fatalf("run = %+v %v", run, err)
	}
	notHR, err := store.ResolveProposeDismissalRun(ctx, wfProject, "exec-plain-1616", 1)
	if err != nil || notHR.IsHRAgent {
		t.Fatalf("plain run = %+v %v", notHR, err)
	}
	ask := func(r storage.ProposeDismissalRun, app int64, stage string) storage.DismissalOutcome {
		t.Helper()
		outcome, err := store.ProposeDismissal(ctx, r, storage.DismissalRequest{ApplicationID: app, Stage: stage, Rationale: "Fails half its runs."})
		if err != nil {
			t.Fatalf("propose against %d: %v", app, err)
		}
		return outcome
	}
	for name, c := range map[string]struct {
		run   storage.ProposeDismissalRun
		app   int64
		stage string
		want  string
	}{
		"not an HR agent":  {notHR, target, "retire", agentworkforce.OutcomeNotAllowed},
		"self":             {run, hr, "retire", agentworkforce.OutcomeGuardRefused},
		"hirer":            {run, hirer, "retire", agentworkforce.OutcomeGuardRefused},
		"another HR agent": {run, otherHR, "retire", agentworkforce.OutcomeGuardRefused},
		"higher tier":      {run, high, "suspend", agentworkforce.OutcomeGuardRefused},
		"critical":         {run, critical, "watch", agentworkforce.OutcomeGuardRefused},
		"legal hold":       {run, held, "retire", agentworkforce.OutcomeGuardRefused},
		"delete":           {run, target, "delete", agentworkforce.OutcomeGuardRefused},
		"unknown agent":    {run, 99171999, "retire", agentworkforce.OutcomeUnknownAgent},
		"invalid stage":    {run, target, "fire", agentworkforce.OutcomeInvalid},
	} {
		if got := ask(c.run, c.app, c.stage); got.Status != c.want || got.ProposalID != 0 {
			t.Errorf("%s: %+v, want %s", name, got, c.want)
		}
	}
	var stored int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_lifecycle.agent_workforce_proposals WHERE proposer_application_id = $1`, hr).Scan(&stored)
	if stored != 0 {
		t.Fatalf("a refused guard stored %d proposals", stored)
	}

	proposed := ask(run, target, "suspend")
	if proposed.Status != agentworkforce.OutcomeProposed || proposed.ProposalID == 0 {
		t.Fatalf("proposed = %+v", proposed)
	}
	if again := ask(run, target, "retire"); again.Status != agentworkforce.OutcomeAlreadyOpen {
		t.Fatalf("second = %+v", again)
	}
	proposal, err := store.GetProposal(ctx, wfProject, proposed.ProposalID)
	if err != nil || proposal.Source != agentworkforce.SourceHRAgent || proposal.ProposerApplicationID == nil ||
		*proposal.ProposerApplicationID != hr || proposal.Status != agentworkforce.StatusProposed || len(proposal.EvidenceDigest) != 64 {
		t.Fatalf("proposal = %+v %v", proposal, err)
	}
	// Nothing applied until a person approves.
	agent, _ := store.Agent(ctx, wfProject, target)
	if agent.SuspensionID != 0 {
		t.Fatal("an HR agent's proposal suspended the agent")
	}
}

func TestWorkforceScorecardPass(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	const app int64 = 991720
	seedWorkforceAgent(t, pool, app, "medium", `{}`)
	seedEgressExecution(t, pool, "exec-score-1", wfProject, app)
	seedEgressExecution(t, pool, "exec-score-2", wfProject, app)
	wfExec(t, pool, `UPDATE elitea_runtime.execution_jobs SET state = 'FAILED', settled_at = now() WHERE execution_id = 'exec-score-1'`)
	wfExec(t, pool, `UPDATE elitea_runtime.execution_jobs SET state = 'CANCELLED', cancel_reason = 'cap_reached', settled_at = now() WHERE execution_id = 'exec-score-2'`)
	now = time.Now().UTC().Add(time.Minute) // the database clock may run ahead of this one

	summary, err := store.ScorecardPass(ctx)
	if err != nil || summary.Agents == 0 {
		t.Fatalf("pass: %+v %v", summary, err)
	}
	cards, err := store.Scorecards(ctx, wfProject, app, 7)
	if err != nil || len(cards) != 1 {
		t.Fatalf("cards = %+v %v", cards, err)
	}
	card := cards[0]
	if card.Runs30d != 2 || card.Failed30d != 1 || card.CapReached30d != 1 || card.FailureRate30d != 1 || card.LastUsedAt == nil {
		t.Fatalf("card = %+v", card)
	}
	// A rerun the same day replaces the row.
	if _, err := store.ScorecardPass(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := store.Scorecards(ctx, wfProject, app, 7); len(again) != 1 {
		t.Fatalf("rows = %d", len(again))
	}
}
