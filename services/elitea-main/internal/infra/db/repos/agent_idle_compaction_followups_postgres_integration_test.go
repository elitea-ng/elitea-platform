package repos

// Idle compaction follow-ups against a real PostgreSQL (issue 1776): the idle
// run binds its source's agent identity (ADR-0039), an agent turn that names
// no participant cancels the agent's QUEUED check, and the request-log
// analytics tag an idle run's calls. Requires ELITEA_TEST_DATABASE_URL; every
// test skips without it.

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/analytics"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

const (
	idleBindingExternal = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1a71"
	idleBindingID       = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1a72"
)

// governIdleHarness switches the harness's admissions to the governed
// profile and seeds an active agent identity binding of version 9 of agent 7
// in project 1 (the version settledAnswer's frozen request runs).
func governIdleHarness(t *testing.T, h *idleHarness) {
	t.Helper()
	ctx := t.Context()
	store, err := NewCredentialGovernanceStore(h.pool)
	require.NoError(t, err)
	revision := putIntegrationRevision(t, ctx, store, "it-idle")
	policy := testAgentDispatchPolicy()
	policy.MaxOutstanding = 256
	policy.CredentialProfile = profile.GovernedAdmission(revision)
	jobs, err := NewAgentExecutionJobsRepository(h.pool, policy, 1)
	require.NoError(t, err)
	factory, err := agentexecutionapp.NewInputBundleFactory(agentexecutionapp.InputProfile{
		Classification: "tenant-confidential", RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, randomExecutionID)
	require.NoError(t, err)
	admissions, err := agentexecutionapp.NewAdmissionService(jobs, factory, nil, randomExecutionID)
	require.NoError(t, err)
	h.admitter, err = agentexecutionapp.NewIdleCompactionAdmitter(h.idle, admissions, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	h.exec(`
INSERT INTO elitea_identity.external_agent_identities
    (id, tenant_project_id, application_id, mode, provider, provider_tenant, kind, external_id, state, created_by, last_transition_by)
VALUES ($1::uuid, 1, 7, 'dedicated_agent', 'fake', 't1', 'fake.agent', 'obj-idle', 'active', '100', '200')`, idleBindingExternal)
	h.exec(`
INSERT INTO elitea_identity.agent_identity_bindings
    (id, tenant_project_id, application_id, application_version_id, definition_digest, mode,
     external_agent_identity_id, policy_revision_id, proposed_by, approved_by, last_transition_by, state)
VALUES ($1::uuid, 1, 7, 9, $2, 'dedicated_agent', $3::uuid, $4, '100', '200', '200', 'active')`,
		idleBindingID, bytes.Repeat([]byte{9}, 32), idleBindingExternal, revision.ID)
}

func (h *idleHarness) idleExecution(source string) string {
	h.t.Helper()
	var idle string
	require.NoError(h.t, h.pool.QueryRow(h.t.Context(), `SELECT idle_execution_id FROM elitea_runtime.agent_idle_compactions
WHERE idempotency_key = $1`, agentexecutionapp.IdleCompactionIdempotencyKey(source)).Scan(&idle))
	return idle
}

// Item 1: the idle run's security context names the source's version and
// the agent identity binding of that version, as the source's would.
func TestIdleCompactionRunBindsTheSourcesAgentIdentity(t *testing.T) {
	h := newIdleHarness(t)
	governIdleHarness(t, h)
	source := h.settledAnswer(h.newResponse(), h.agent)
	require.True(t, h.arm(source))
	h.makeDue(source)
	created, err := h.admitter.Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, created, "state %q", h.checkState(source))
	idle := h.idleExecution(source)

	var stored []byte
	var reference securitycontext.Reference
	var digest []byte
	require.NoError(t, h.pool.QueryRow(t.Context(), `
SELECT content_id, schema_revision, digest, context_bytes FROM elitea_runtime.execution_security_contexts
WHERE execution_id = $1`, idle).Scan(&reference.ContentID, &reference.SchemaRevision, &digest, &stored))
	copy(reference.Digest[:], digest)
	decoded, err := securitycontext.Verify(stored, reference)
	require.NoError(t, err)
	require.Equal(t, idleBindingID, decoded.GetAgentBindingId(), "the idle run binds the source version's agent identity")
	require.EqualValues(t, 7, decoded.GetApplicationId())
	require.EqualValues(t, 9, decoded.GetApplicationVersionId())
}

// Item 1, a release run: the idle run carries the source's release, so it
// records the same release and binds the binding of the release's version.
func TestIdleCompactionRunKeepsTheSourcesRelease(t *testing.T) {
	h := newIdleHarness(t)
	source := h.settledAnswer(h.newResponse(), h.agent)
	var releaseID int64
	require.NoError(t, h.pool.QueryRow(t.Context(), `
INSERT INTO elitea_lifecycle.application_releases
    (tenant_project_id, application_id, application_version_id, name, definition_digest, definition)
VALUES (1, 7, 9, 'v1', $1, '{}'::jsonb) RETURNING id`, bytes.Repeat([]byte{5}, 32)).Scan(&releaseID))
	h.exec(`UPDATE elitea_runtime.agent_execution_jobs SET release_id = $2 WHERE execution_id = $1`, source, releaseID)
	require.True(t, h.arm(source))
	h.makeDue(source)
	created, err := h.admitter.Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, created, "state %q", h.checkState(source))
	var recorded *int64
	require.NoError(t, h.pool.QueryRow(t.Context(), `SELECT release_id FROM elitea_runtime.agent_execution_jobs WHERE execution_id = $1`,
		h.idleExecution(source)).Scan(&recorded))
	require.NotNil(t, recorded)
	require.Equal(t, releaseID, *recorded)
}

// Item 1: a source whose frozen request names another agent's version is
// skipped, never admitted under a version it did not run.
func TestIdleCompactionSkipsASourceOfAnotherVersion(t *testing.T) {
	h := newIdleHarness(t)
	other := h.settledAnswer(h.newResponse(), h.agent)
	h.exec(`UPDATE elitea_runtime.agent_execution_jobs SET application_id = 8 WHERE execution_id = $1`, other)
	require.True(t, h.arm(other))
	h.makeDue(other)
	created, err := h.admitter.Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, 0, created)
	require.Equal(t, "SKIPPED:"+agentexecutionapp.IdleCompactionSkipSourceInvalid, h.checkState(other))
}

// Item 2: a turn of the agent that names no participant cancels the agent's
// QUEUED check in the conversation; another agent's turn, a turn in another
// conversation and an ADMITTED row are left alone.
func TestIdleCompactionCancelledByAnUntargetedTurnOfTheAgent(t *testing.T) {
	h := newIdleHarness(t)
	source := h.settledAnswer(h.newResponse(), h.agent) // application 7
	require.True(t, h.arm(source))
	cancel := func(conversation string, application int64) {
		t.Helper()
		require.NoError(t, pgx.BeginFunc(t.Context(), h.pool, func(tx pgx.Tx) error {
			return cancelApplicationIdleCompactions(t.Context(), pgxExecutor{queryer: tx}, 1, conversation, application)
		}))
	}
	cancel(h.conversation, 2)
	require.Equal(t, "QUEUED", h.checkState(source), "another agent's turn")
	cancel("00000000-0000-4000-8000-000000000001", 7)
	require.Equal(t, "QUEUED", h.checkState(source), "a turn in another conversation")
	cancel(h.conversation, 7)
	require.Equal(t, "CANCELLED:superseded_by_agent_turn", h.checkState(source))

	// An ADMITTED idle run is left to the session lease.
	second := h.settledAnswer(h.newResponse(), h.agent)
	require.True(t, h.arm(second))
	h.makeDue(second)
	created, err := h.admitter.Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, created)
	cancel(h.conversation, 7)
	require.Equal(t, "ADMITTED", h.checkState(second))
}

// Item 3: an idle run's calls are tagged idle_compaction. They stay in the
// call and token totals, leave the active-user figures, and are their own
// automated bucket; the execution read names the origin.
func TestIdleCompactionCallsAreTaggedInAnalytics(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	// User 7 chats (the source turn); user 8's only call is an idle run's.
	seedOriginExecution(t, pool, 1, "exec-source", "manual", now.Add(-time.Minute))
	seedOriginExecution(t, pool, 1, "exec-idle", "manual", now.Add(-time.Minute))
	_, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.agent_idle_compactions
    (project_id, conversation_uuid, participant_id, source_execution_id, source_generation, request_started_at,
     cache_ttl_seconds, due_at, idempotency_key, state, idle_execution_id)
VALUES (1, gen_random_uuid(), 1, $1, 1, $3, 300, $3, 'idle-compaction/' || $1, 'DONE', $2)`,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", now.Add(-time.Minute))
	require.Error(t, err, "the row's ids are checked execution ids")
	seedOriginExecutionAs(t, pool, 1, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "manual", "7", now.Add(-time.Minute), nil)
	seedOriginExecutionAs(t, pool, 1, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "manual", "8", now.Add(-time.Minute), nil)
	_, err = pool.Exec(ctx, `
INSERT INTO elitea_runtime.agent_idle_compactions
    (project_id, conversation_uuid, participant_id, source_execution_id, source_generation, request_started_at,
     cache_ttl_seconds, due_at, idempotency_key, state, idle_execution_id)
VALUES (1, gen_random_uuid(), 1, $1, 1, $3, 300, $3, 'idle-compaction/' || $1, 'DONE', $2)`,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", now.Add(-time.Minute))
	require.NoError(t, err)
	plantRunCall(t, pool, 1, 7, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", now, "gpt-4o", 200, 10, 10)
	plantRunCall(t, pool, 1, 8, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", now, "gpt-4o", 200, 100, 100)

	params := analytics.QueryParams{ProjectID: "1", From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	summary, err := repo.GetUsageSummary(ctx, params)
	require.NoError(t, err)
	require.EqualValues(t, 2, summary.TotalRuns, "the call total counts the idle run's call")
	require.EqualValues(t, 220, summary.TotalTokens)
	require.EqualValues(t, 1, summary.ActiveUsers, "an idle run is no person's activity")
	byOrigin := map[string]analytics.AutomatedActivity{}
	for _, row := range summary.Automated {
		byOrigin[row.TriggerOrigin] = row
	}
	require.Len(t, byOrigin, 1)
	require.EqualValues(t, 1, byOrigin["idle_compaction"].LLMCalls)
	require.EqualValues(t, 1, byOrigin["idle_compaction"].Executions)
	require.EqualValues(t, 200, byOrigin["idle_compaction"].TotalTokens)

	users, _, err := repo.GetUserActivity(ctx, params)
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.Equal(t, "7", users[0].UserID)

	execution, err := repo.GetExecutionAnalytics(ctx, "1", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	require.Equal(t, "idle_compaction", execution.TriggerOrigin)
	source, err := repo.GetExecutionAnalytics(ctx, "1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	require.Equal(t, "manual", source.TriggerOrigin)
}
