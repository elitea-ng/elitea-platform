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

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
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

// seedWorkforceReleases gives the test tenant what the release recorder
// reads (the applications columns the definition projection selects).
func seedWorkforceReleases(t *testing.T, pool *pgxpool.Pool) *AgentReleases {
	t.Helper()
	wfExec(t, pool, `ALTER TABLE p_1.applications
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMP,
    ADD COLUMN IF NOT EXISTS meta JSONB NOT NULL DEFAULT '{}'::jsonb`)
	releases, err := NewAgentReleases(pool)
	if err != nil {
		t.Fatal(err)
	}
	return releases
}

// wfRecordRelease records the release of a published version and points
// the head at it, as a publish does.
func wfRecordRelease(t *testing.T, releases *AgentReleases, app, version int64) int64 {
	t.Helper()
	release, err := releases.RecordRelease(context.Background(), RecordReleaseRequest{
		ProjectID: wfProject, ApplicationID: app, VersionID: version, SourceVersionID: version, Name: "v" + strconv.FormatInt(version, 10), ActorID: wfOwner,
	})
	if err != nil {
		t.Fatalf("record the release of version %d: %v", version, err)
	}
	return release.ID
}

func wfToolkits(t *testing.T, pool *pgxpool.Pool, version int64) []int64 {
	t.Helper()
	var ids []int64
	if err := pool.QueryRow(context.Background(), `
SELECT COALESCE(array_agg(tool_id ORDER BY tool_id), '{}') FROM p_1.entity_tool_mapping WHERE entity_version_id = $1`, version).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

func wfHead(t *testing.T, pool *pgxpool.Pool, app int64) (release int64, cause, reason string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
SELECT release_id, cause, reason FROM elitea_lifecycle.application_release_heads WHERE tenant_project_id = 1 AND application_id = $1`,
		app).Scan(&release, &cause, &reason); err != nil {
		t.Fatal(err)
	}
	return release, cause, reason
}

// Issue 1873 (ADR-0050 decision 5b as amended): an approved demotion
// publishes the narrowing at once, cut from the published version, and
// rebases the same narrowing onto the draft without touching the draft's
// other edits. No change request is opened.
func TestWorkforceDemotePublishesTheNarrowingAndRebasesItOntoTheDraft(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	releases := seedWorkforceReleases(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	const app int64 = 991702
	published, draft := seedWorkforceAgent(t, pool, app, "low",
		`{"capabilities":{"can_propose_agents":true,"can_review_agents":true},"memory":{"enabled":true,"write":"auto"},"proactivity":{"mode":"heartbeat","interval":"30m"}}`)
	// The owner's unpublished draft edits: other instructions, another meta
	// key, and a toolkit only the draft has.
	wfExec(t, pool, `UPDATE p_1.application_versions SET instructions = 'unpublished draft edit', meta = meta || '{"draft_note":"wip"}'::jsonb WHERE id = $1`, draft)
	wfExec(t, pool, `INSERT INTO p_1.agent_heartbeat_settings (application_id, enabled, owner_user_id) VALUES ($1, true, 4)`, app)
	wfExec(t, pool, `INSERT INTO p_1.elitea_tools (id, type, name, description, settings, author_id, meta, created_at)
VALUES (9917021, 'github', 'gh', '', '{}'::jsonb, 4, '{}'::jsonb, now() AT TIME ZONE 'UTC'),
       (9917022, 'jira', 'jira', '', '{}'::jsonb, 4, '{}'::jsonb, now() AT TIME ZONE 'UTC'),
       (9917023, 'slack', 'slack', '', '{}'::jsonb, 4, '{}'::jsonb, now() AT TIME ZONE 'UTC')
ON CONFLICT (id) DO NOTHING`)
	wfExec(t, pool, `INSERT INTO p_1.entity_tool_mapping (tool_id, entity_id, entity_version_id, entity_type, selected_tools)
VALUES (9917021, $1, $2, 'agent', '[]'::jsonb), (9917022, $1, $2, 'agent', '[]'::jsonb),
       (9917021, $1, $3, 'application', '[]'::jsonb), (9917022, $1, $3, 'agent', '[]'::jsonb), (9917023, $1, $3, 'agent', '[]'::jsonb)`,
		app, published, draft)
	// An older published version, published before the head's: a run
	// pinned to it is narrowed too, and resolves its own new release.
	const older int64 = app * 10
	wfExec(t, pool, `
INSERT INTO p_1.application_versions (id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at)
SELECT $1, application_id, 'v0', status, author_id, gen_random_uuid(), llm_settings, 'older', conversation_starters,
       welcome_message, agent_type, meta, pipeline_settings, created_at
FROM p_1.application_versions WHERE id = $2`, older, published)
	wfExec(t, pool, `INSERT INTO p_1.entity_tool_mapping (tool_id, entity_id, entity_version_id, entity_type, selected_tools)
VALUES (9917021, $1, $2, 'agent', '[]'::jsonb)`, app, older)
	olderWide := wfRecordRelease(t, releases, app, older)
	wide := wfRecordRelease(t, releases, app, published)
	conversation, _ := seedRoster(t, pool, app, true)

	decided := approve(t, store, propose(t, store, app, agentworkforce.StageDemote, agentworkforce.Narrowing{
		RemoveToolkitIDs: []int64{9917021}, MemoryReview: true, RemoveCapabilities: []string{"can_review_agents"}, RemoveLead: true, ProactivityOff: true,
	}), false)
	narrowed, ok := appliedInt(decided.Applied, "release_id")
	if decided.Kind != "demote" || !ok || narrowed == wide || decided.Applied["published"] != true ||
		decided.Applied["publish_needed"] != false || decided.Applied["published_version_id"] != float64(published) ||
		decided.Applied["draft_version_id"] != float64(draft) {
		t.Fatalf("applied = %v", decided.Applied)
	}

	// The published version is narrowed, and its new release is live.
	var publishedMeta string
	_ = pool.QueryRow(ctx, `SELECT meta::text FROM p_1.application_versions WHERE id = $1`, published).Scan(&publishedMeta)
	if !strings.Contains(publishedMeta, `"write": "review"`) || strings.Contains(publishedMeta, "can_review_agents") ||
		!strings.Contains(publishedMeta, "can_propose_agents") || !strings.Contains(publishedMeta, `"mode": "off"`) {
		t.Fatalf("published meta = %s", publishedMeta)
	}
	if got := wfToolkits(t, pool, published); !slices.Equal(got, []int64{9917022}) {
		t.Fatalf("toolkits on the published version = %v", got)
	}
	head, cause, reason := wfHead(t, pool, app)
	if head != narrowed || cause != "publish" || !strings.Contains(reason, strconv.FormatInt(decided.ID, 10)) {
		t.Fatalf("head = %d %s %q, want %d", head, cause, reason, narrowed)
	}
	var (
		wideState, narrowedState, definition, pins string
		releaseVersion, releaseSource, publishedBy int64
	)
	_ = pool.QueryRow(ctx, `SELECT state FROM elitea_lifecycle.application_releases WHERE id = $1`, wide).Scan(&wideState)
	if err := pool.QueryRow(ctx, `
SELECT state, application_version_id, source_version_id, published_by, definition::text, toolkit_pins::text
FROM elitea_lifecycle.application_releases WHERE id = $1`, narrowed).Scan(
		&narrowedState, &releaseVersion, &releaseSource, &publishedBy, &definition, &pins); err != nil {
		t.Fatal(err)
	}
	if wideState != "withdrawn" || narrowedState != "published" || releaseVersion != published || releaseSource != published || publishedBy != wfManager {
		t.Fatalf("releases: wide %s, narrowed %s version=%d source=%d by=%d", wideState, narrowedState, releaseVersion, releaseSource, publishedBy)
	}
	if strings.Contains(definition, "can_review_agents") || !strings.Contains(definition, `"review"`) ||
		strings.Contains(pins, "9917021") || !strings.Contains(pins, "9917022") {
		t.Fatalf("the narrowed release: definition=%s pins=%s", definition, pins)
	}
	// The older version: its wide release withdrawn, a new one below the
	// head's, and a run of it resolves that new release.
	var olderWideState string
	var olderNarrowed int64
	_ = pool.QueryRow(ctx, `SELECT state FROM elitea_lifecycle.application_releases WHERE id = $1`, olderWide).Scan(&olderWideState)
	_ = pool.QueryRow(ctx, `
SELECT id FROM elitea_lifecycle.application_releases WHERE tenant_project_id = 1 AND application_version_id = $1 AND state = 'published'`,
		older).Scan(&olderNarrowed)
	if olderWideState != "withdrawn" || olderNarrowed == 0 || olderNarrowed >= narrowed || !slices.Equal(wfToolkits(t, pool, older), []int64{}) {
		t.Fatalf("older version: wide %s, narrowed %d (head %d), toolkits %v", olderWideState, olderNarrowed, narrowed, wfToolkits(t, pool, older))
	}
	if run, _, err := wfResolveRelease(t, pool, app, older); err != nil || run == nil || run.ID != olderNarrowed {
		t.Fatalf("a run of the older version resolves %+v %v, want %d", run, err, olderNarrowed)
	}
	// A run of the published version resolves the narrowed release.
	if run, _, err := wfResolveRelease(t, pool, app, published); err != nil || run == nil || run.ID != narrowed {
		t.Fatalf("the run resolves %+v %v, want release %d", run, err, narrowed)
	}

	// The draft gets the same narrowing and keeps its own edits.
	var draftMeta, draftInstructions string
	_ = pool.QueryRow(ctx, `SELECT meta::text, instructions FROM p_1.application_versions WHERE id = $1`, draft).Scan(&draftMeta, &draftInstructions)
	if draftInstructions != "unpublished draft edit" || !strings.Contains(draftMeta, `"draft_note": "wip"`) ||
		!strings.Contains(draftMeta, `"write": "review"`) || strings.Contains(draftMeta, "can_review_agents") ||
		!strings.Contains(draftMeta, `"mode": "off"`) || !strings.Contains(draftMeta, `"interval": "30m"`) {
		t.Fatalf("draft: instructions=%q meta=%s", draftInstructions, draftMeta)
	}
	if got := wfToolkits(t, pool, draft); !slices.Equal(got, []int64{9917022, 9917023}) {
		t.Fatalf("toolkits on the draft = %v", got)
	}

	// No change request; the publish is in the audit trail.
	var changes int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_lifecycle.change_requests WHERE tenant_project_id = 1 AND application_id = $1`, app).Scan(&changes)
	if changes != 0 {
		t.Fatalf("change requests = %d, want none", changes)
	}
	var published1873 int
	_ = pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 AND event->'data'->>'action' = 'agent.workforce.demotion_published' AND event->'data'->>'resource_id' = $2`,
		string(securityaudit.TypeAgentWorkforceChanged), strconv.FormatInt(decided.ID, 10)).Scan(&published1873)
	if published1873 != 1 {
		t.Fatalf("demotion_published audit events = %d", published1873)
	}

	var heartbeatEnabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM p_1.agent_heartbeat_settings WHERE application_id = $1`, app).Scan(&heartbeatEnabled); err != nil || heartbeatEnabled {
		t.Fatalf("the heartbeat row still runs: %v %v", heartbeatEnabled, err)
	}
	var floor *string
	_ = pool.QueryRow(ctx, `SELECT meta -> 'floor' ->> 'policy' FROM p_1.chat_conversations WHERE id = $1`, conversation).Scan(&floor)
	if floor != nil {
		t.Fatalf("the lead remained: %v", *floor)
	}
}

// wfResolveRelease is the start resolver's release read for a run of a
// version (resolveRunRelease), in a project transaction.
func wfResolveRelease(t *testing.T, pool *pgxpool.Pool, app, version int64) (*agentexecutionapp.ReleaseRun, []byte, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tenant.BindProject(ctx, tx, tenant.Project{ID: wfProject}); err != nil {
		t.Fatal(err)
	}
	executor := pgxExecutor{queryer: tx}
	row, err := executor.ResolveCurrentApplicationVersionDetails(ctx, sqlcgen.ResolveCurrentApplicationVersionDetailsParams{
		ApplicationVersionID: int32(version), ApplicationID: int32(app),
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolveRunRelease(ctx, executor, func() int64 { return 99 }, wfProject, app, version, []byte(row.ApplicationVersionDetailsJson), 0)
}

// An agent with no published version: only the draft is narrowed, and no
// release is recorded. An embedded copy (a sub-agent frozen inside another
// agent's release) is not this agent's release and stays as it is. An
// agent with neither a draft nor a published version is refused.
func TestWorkforceDemoteWithoutAPublishedVersionNarrowsTheDraftOnly(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	seedWorkforceReleases(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	const app, empty int64 = 991708, 991709
	meta := `{"capabilities":{"can_propose_agents":true,"can_review_agents":true}}`
	embedded, draft := seedWorkforceAgent(t, pool, app, "low", meta)
	wfExec(t, pool, `UPDATE p_1.application_versions SET status = 'embedded' WHERE id = $1`, embedded)

	decided := approve(t, store, propose(t, store, app, agentworkforce.StageDemote, agentworkforce.Narrowing{
		RemoveCapabilities: []string{"can_review_agents"},
	}), false)
	if decided.Applied["published"] != false || decided.Applied["release_id"] != nil || decided.Applied["draft_version_id"] != float64(draft) ||
		decided.Applied["publish_needed"] != false {
		t.Fatalf("applied = %v", decided.Applied)
	}
	var draftMeta, embeddedMeta string
	var releases int
	_ = pool.QueryRow(ctx, `SELECT meta::text FROM p_1.application_versions WHERE id = $1`, draft).Scan(&draftMeta)
	_ = pool.QueryRow(ctx, `SELECT meta::text FROM p_1.application_versions WHERE id = $1`, embedded).Scan(&embeddedMeta)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_lifecycle.application_releases WHERE tenant_project_id = 1 AND application_id = $1`, app).Scan(&releases)
	if strings.Contains(draftMeta, "can_review_agents") || !strings.Contains(embeddedMeta, "can_review_agents") || releases != 0 {
		t.Fatalf("draft=%s embedded=%s releases=%d", draftMeta, embeddedMeta, releases)
	}

	gonePublished, goneDraft := seedWorkforceAgent(t, pool, empty, "low", meta)
	wfExec(t, pool, `DELETE FROM p_1.application_versions WHERE id IN ($1, $2)`, gonePublished, goneDraft)
	proposal := propose(t, store, empty, agentworkforce.StageDemote, agentworkforce.Narrowing{RemoveCapabilities: []string{"can_review_agents"}})
	if _, err := store.Approve(ctx, Decision{ProjectID: wfProject, ProposalID: proposal.ID, Actor: managerActor(wfManager)}); refusalCode(err) != agentworkforce.StateConflictCode {
		t.Fatalf("a demotion with nothing to narrow: %v", err)
	}
}

// Every published version is narrowed, so a run pinned to an older one is
// narrowed too; the catalogue twin follows. When the head's version had
// nothing to narrow, the head stays exactly where it was.
func TestWorkforceDemoteNarrowsOlderPublishedVersionsAndKeepsAnUnchangedHead(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	releases := seedWorkforceReleases(t, pool)
	now := time.Now().UTC()
	store := workforceStore(t, pool, &now)
	ctx := context.Background()
	const app int64 = 991710
	older, draft := seedWorkforceAgent(t, pool, app, "low", `{}`)
	const newer int64 = 991710*10 + 3
	wfExec(t, pool, `
INSERT INTO p_1.application_versions (id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at)
VALUES ($1, $2, 'v2', 'published', 4, gen_random_uuid(), '{}'::jsonb, 'x', '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb, now() AT TIME ZONE 'UTC')`,
		newer, app)
	wfExec(t, pool, `INSERT INTO p_1.elitea_tools (id, type, name, description, settings, author_id, meta, created_at)
VALUES (9917101, 'github', 'gh', '', '{}'::jsonb, 4, '{}'::jsonb, now() AT TIME ZONE 'UTC') ON CONFLICT (id) DO NOTHING`)
	// Only the older published version (and the draft) carry the toolkit.
	wfExec(t, pool, `INSERT INTO p_1.entity_tool_mapping (tool_id, entity_id, entity_version_id, entity_type, selected_tools)
VALUES (9917101, $1, $2, 'agent', '[]'::jsonb), (9917101, $1, $3, 'agent', '[]'::jsonb)`, app, older, draft)
	olderRelease := wfRecordRelease(t, releases, app, older)
	newerRelease := wfRecordRelease(t, releases, app, newer)
	wfExec(t, pool, `UPDATE elitea_lifecycle.application_release_heads SET cause = 'rollback', reason = 'kept'
WHERE tenant_project_id = 1 AND application_id = $1`, app)

	// The older version has a catalogue twin in public project 2.
	t.Setenv("ELITEA_AI_PROJECT_ID", "2")
	wfExec(t, pool, `INSERT INTO centry.project (id, create_success, suspended) VALUES (2, TRUE, FALSE) ON CONFLICT DO NOTHING`)
	wfExec(t, pool, `CREATE SCHEMA IF NOT EXISTS p_2;
CREATE TABLE IF NOT EXISTS p_2.applications (LIKE p_1.applications INCLUDING ALL);
CREATE TABLE IF NOT EXISTS p_2.application_versions (LIKE p_1.application_versions INCLUDING ALL);
ALTER TABLE p_2.application_versions ADD COLUMN IF NOT EXISTS shared_owner_id integer, ADD COLUMN IF NOT EXISTS shared_id integer`)
	wfExec(t, pool, `INSERT INTO p_2.applications (id, name, owner_id) VALUES (87, 'Agent twin', 4)`)
	wfExec(t, pool, `INSERT INTO p_2.application_versions (id, application_id, name, status, author_id, shared_owner_id, shared_id, instructions, uuid, llm_settings,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at)
VALUES (88, 87, 'v1', 'published', 4, 1, $1, 'twin', gen_random_uuid(), '{}'::jsonb, '[]'::json, '', 'agent',
        '{"capabilities":{"can_review_agents":true},"category":"Other"}'::jsonb, '{}'::jsonb, now() AT TIME ZONE 'UTC')`, older)

	decided := approve(t, store, propose(t, store, app, agentworkforce.StageDemote, agentworkforce.Narrowing{
		RemoveToolkitIDs: []int64{9917101}, RemoveCapabilities: []string{"can_review_agents"},
	}), false)
	if got := wfToolkits(t, pool, older); len(got) != 0 {
		t.Fatalf("toolkits on the older published version = %v", got)
	}
	// The older version's wide release is withdrawn and no release is cut
	// for it: a new one would have an id above the head's, and the run
	// resolver would then read the head as a rollback and run the newer
	// version's definition for it.
	var olderState, newerState string
	var releaseCount int
	_ = pool.QueryRow(ctx, `SELECT state FROM elitea_lifecycle.application_releases WHERE id = $1`, olderRelease).Scan(&olderState)
	_ = pool.QueryRow(ctx, `SELECT state FROM elitea_lifecycle.application_releases WHERE id = $1`, newerRelease).Scan(&newerState)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_lifecycle.application_releases WHERE tenant_project_id = 1 AND application_id = $1`, app).Scan(&releaseCount)
	if olderState != "withdrawn" || newerState != "published" || releaseCount != 2 {
		t.Fatalf("releases: older %s, newer %s, count %d", olderState, newerState, releaseCount)
	}
	if head, cause, reason := wfHead(t, pool, app); head != newerRelease || cause != "rollback" || reason != "kept" {
		t.Fatalf("head = %d %s %q, want %d rollback kept", head, cause, reason, newerRelease)
	}
	versions, _ := decided.Applied["published_versions"].([]any)
	if decided.Applied["published"] != true || decided.Applied["release_id"] != nil || len(versions) != 2 {
		t.Fatalf("applied = %v (the narrowing is live; the head did not move)", decided.Applied)
	}
	// A run of the older version executes its narrowed row (no release);
	// a run of the newer one still resolves its release.
	if run, _, err := wfResolveRelease(t, pool, app, older); err != nil || run != nil {
		t.Fatalf("a run of the older version resolves %+v %v, want the narrowed row", run, err)
	}
	if run, _, err := wfResolveRelease(t, pool, app, newer); err != nil || run == nil || run.ID != newerRelease {
		t.Fatalf("a run of the newer version resolves %+v %v, want %d", run, err, newerRelease)
	}
	var twinMeta string
	_ = pool.QueryRow(ctx, `SELECT meta::text FROM p_2.application_versions WHERE id = 88`).Scan(&twinMeta)
	if strings.Contains(twinMeta, "can_review_agents") || !strings.Contains(twinMeta, `"category": "Other"`) {
		t.Fatalf("catalogue twin meta = %s", twinMeta)
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

	// A stale appeal undoes only its own stage: suspension A is appealed,
	// an administrator resumes it, suspension B follows on a new proposal;
	// granting A's appeal leaves B in force.
	first := approve(t, store, propose(t, store, app, agentworkforce.StageSuspend, agentworkforce.Narrowing{}), false)
	if _, err := store.Appeal(ctx, wfProject, first.ID, wfOwner, "unfair"); err != nil {
		t.Fatal(err)
	}
	wfExec(t, pool, `UPDATE elitea_runtime.execution_suspensions SET resumed_at = clock_timestamp(), resumed_by = 1, resume_reason = 'admin'
WHERE scope = 'agent' AND project_id = 1 AND scope_key = $1 AND resumed_at IS NULL`, strconv.FormatInt(app, 10))
	approve(t, store, propose(t, store, app, agentworkforce.StageSuspend, agentworkforce.Narrowing{}), false)
	second, _ := store.Agent(ctx, wfProject, app)
	if _, err := store.DecideAppeal(ctx, AppealDecision{ProjectID: wfProject, ProposalID: first.ID, Actor: managerActor(wfManager2), Grant: true}); err != nil {
		t.Fatal(err)
	}
	if agent, _ := store.Agent(ctx, wfProject, app); agent.SuspensionID == 0 || agent.SuspensionID != second.SuspensionID {
		t.Fatalf("a stale appeal resumed the later suspension: %+v (was %d)", agent, second.SuspensionID)
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
	// The public project is 2 here, and the published version has a
	// catalogue twin there: retirement removes it, reinstatement restores it.
	t.Setenv("ELITEA_AI_PROJECT_ID", "2")
	wfExec(t, pool, `INSERT INTO centry.project (id, create_success, suspended) VALUES (2, TRUE, FALSE) ON CONFLICT DO NOTHING`)
	wfExec(t, pool, `CREATE SCHEMA IF NOT EXISTS p_2;
CREATE TABLE p_2.applications (LIKE p_1.applications INCLUDING ALL);
CREATE TABLE p_2.application_versions (LIKE p_1.application_versions INCLUDING ALL);
ALTER TABLE p_2.applications ADD COLUMN IF NOT EXISTS shared_owner_id integer, ADD COLUMN IF NOT EXISTS shared_id integer;
ALTER TABLE p_2.application_versions ADD COLUMN IF NOT EXISTS shared_owner_id integer, ADD COLUMN IF NOT EXISTS shared_id integer`)
	wfExec(t, pool, `INSERT INTO p_2.applications (id, name, owner_id, shared_owner_id, shared_id) VALUES (77, 'Agent twin', 4, 1, $1)`, app)
	wfExec(t, pool, `INSERT INTO p_2.application_versions (id, application_id, name, status, author_id, shared_owner_id, shared_id, instructions, uuid, llm_settings,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at)
VALUES (78, 77, 'v1', 'published', 4, 1, $1, 'twin', gen_random_uuid(), '{}'::jsonb, '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb, now() AT TIME ZONE 'UTC')`, published)
	twins := func() (apps, versions int) {
		t.Helper()
		_ = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM p_2.applications WHERE id = 77 AND shared_id = $1),
       (SELECT count(*) FROM p_2.application_versions WHERE id = 78 AND shared_id = $2 AND instructions = 'twin')`, app, published).Scan(&apps, &versions)
		return apps, versions
	}
	wfExec(t, pool, `INSERT INTO elitea_lifecycle.application_releases
    (tenant_project_id, application_id, application_version_id, name, definition_digest, definition, toolkit_pins, state)
VALUES (1, $1, $2, 'v1', decode(repeat('ab', 32), 'hex'), '{}'::jsonb, '[]'::jsonb, 'published')`, app, published)
	wfExec(t, pool, `INSERT INTO elitea_lifecycle.application_release_heads (tenant_project_id, application_id, release_id, cause)
SELECT 1, $1, id, 'publish' FROM elitea_lifecycle.application_releases WHERE tenant_project_id = 1 AND application_id = $1`, app)
	conversation, participant := seedRoster(t, pool, app, true)
	wfExec(t, pool, `INSERT INTO p_1.pipeline_schedules (application_id, version_id, cron, active, author_id) VALUES ($1, $2, '0 * * * *', true, 4)`, app, published)
	// A run of the agent in flight: retirement cancels it.
	seedEgressExecution(t, pool, "exec-retire-inflight", wfProject, app)
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
	if apps, versions := twins(); apps != 0 || versions != 0 {
		t.Fatalf("the catalogue twin survived retirement: %d %d", apps, versions)
	}
	var desired string
	_ = pool.QueryRow(ctx, `SELECT desired_state FROM elitea_runtime.execution_jobs WHERE execution_id = 'exec-retire-inflight'`).Scan(&desired)
	if desired != "CANCELLED" {
		t.Fatalf("the run in flight was not cancelled at retirement: %q", desired)
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
	if apps, versions := twins(); apps != 1 || versions != 1 {
		t.Fatalf("after reinstate: catalogue twin %d %d", apps, versions)
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
	// A draft that claims can_review_agents (the run's frozen meta says so)
	// is not an HR agent until a published version carries it.
	seedHRRun(t, pool, "exec-draft-hr-1616", plain, true)
	draftHR, err := store.ResolveProposeDismissalRun(ctx, wfProject, "exec-draft-hr-1616", 1)
	if err != nil || !draftHR.IsHRAgent {
		t.Fatalf("draft-HR run = %+v %v", draftHR, err)
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
		"HR only in draft": {draftHR, target, "retire", agentworkforce.OutcomeNotAllowed},
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

	// The guards hold at the decision too: a legal hold placed after the
	// HR agent proposed stops the approval; nothing is applied.
	wfExec(t, pool, `INSERT INTO elitea_runtime.legal_holds (scope, project_id, agent_id, reason, created_by) VALUES ('agent', 1, $1, 'later', 1)`, target)
	if _, err := store.Approve(ctx, Decision{ProjectID: wfProject, ProposalID: proposed.ProposalID, Actor: managerActor(wfManager)}); refusalCode(err) != agentworkforce.StateConflictCode {
		t.Fatalf("approving a stale HR proposal under legal hold: %v", err)
	}
	if agent, _ := store.Agent(ctx, wfProject, target); agent.SuspensionID != 0 {
		t.Fatal("the stale HR proposal suspended a held agent")
	}
	if _, err := store.Reject(ctx, Decision{ProjectID: wfProject, ProposalID: proposed.ProposalID, Actor: managerActor(wfManager)}); err != nil {
		t.Fatalf("rejecting it: %v", err)
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
