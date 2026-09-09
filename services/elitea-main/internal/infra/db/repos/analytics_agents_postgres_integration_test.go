package repos

// GetAgentAnalytics against the real ledgered schema.
//
// It runs on the migrated template (shared history through 0101, tenant p_1),
// so the columns migrations 0100 and 0101 add are exercised as DDL rather than
// as strings in a Go file. It SKIPS without ELITEA_TEST_DATABASE_URL, and a
// skipped Go test prints the same `ok` as a passing one — so each case below
// asserts something a skip could not have produced.

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/analytics"
)

const agentAnalyticsProject int64 = 1

func agentAnalyticsWindow() analytics.QueryParams {
	now := time.Now().UTC()
	return analytics.QueryParams{
		ProjectID: "1",
		From:      now.Add(-24 * time.Hour),
		To:        now.Add(24 * time.Hour),
		Period:    "custom",
	}
}

// seedPylonApplicationsTable creates the shape of the PYLON-OWNED applications
// table.
//
// The ledgered corpus does not create it — no migration in this service does,
// deliberately — so a corpus-only database has the chat projection and no agent
// NAMES. The read is built for exactly that split, and
// TestGetAgentAnalytics_SurvivesAnAbsentApplicationsTable covers the other side
// of it.
func seedPylonApplicationsTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
CREATE TABLE IF NOT EXISTS p_1.applications (
    id       integer PRIMARY KEY,
    name     varchar NOT NULL,
    owner_id integer NOT NULL
)`)
	require.NoError(t, err, "seed the pylon-owned applications table")
}

// seedAgentExecution writes the whole chain one attributable request needs: an
// input bundle, an agent execution_jobs row, the chat projection that names the
// agent, and the log rows themselves.
func seedAgentExecution(t *testing.T, pool *pgxpool.Pool, executionID string, applicationID int, agentName string, requests, errors int) {
	t.Helper()
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ($1, 'admission:'||$1, $2, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')
ON CONFLICT DO NOTHING`, "bundle-"+executionID, agentAnalyticsProject)
	require.NoError(t, err, "seed input bundle")

	_, err = pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state
) VALUES (
    $1, 1, 'cmd-'||$1, '1', $2, $2, 'actor-1', 'actor-1',
    'agent.execute.application.v1', 'v1', $3,
    decode(repeat('61', 32), 'hex'), 'scope-'||$1, 'key-'||$1, 'SUCCEEDED', 'RUNNING'
)`, executionID, agentAnalyticsProject, "bundle-"+executionID)
	require.NoError(t, err, "seed execution job")

	seedPylonApplicationsTable(t, pool)
	_, err = pool.Exec(ctx, `
INSERT INTO p_1.applications (id, name, owner_id)
VALUES ($1, $2, 1)
ON CONFLICT (id) DO NOTHING`, applicationID, agentName)
	require.NoError(t, err, "seed application")

	_, err = pool.Exec(ctx, `
INSERT INTO p_1.chat_conversations (id, uuid, name, author_id)
VALUES ($1, gen_random_uuid(), 'conversation', 1)
ON CONFLICT (id) DO NOTHING`, applicationID)
	require.NoError(t, err, "seed conversation")

	_, err = pool.Exec(ctx, `
INSERT INTO p_1.chat_participants (id, uuid, entity_name, entity_meta)
VALUES ($1, gen_random_uuid(), 'application',
        jsonb_build_object('id', $2::text, 'project_id', '1'))
ON CONFLICT (id) DO NOTHING`, applicationID, strconv.Itoa(applicationID))
	require.NoError(t, err, "seed participant")

	_, err = pool.Exec(ctx, `
INSERT INTO p_1.chat_message_group
    (uuid, author_participant_id, conversation_id, task_id)
VALUES (gen_random_uuid(), $1, $2, $3)`, applicationID, applicationID, executionID)
	require.NoError(t, err, "seed message group")

	for i := 0; i < requests; i++ {
		status := 200
		if i < errors {
			status = 500
		}
		_, err = pool.Exec(ctx, `
INSERT INTO gateway.llm_request_logs
    (occurred_at, project_id, user_id, route, method, status, duration_ms,
     provider, model, prompt_tokens, completion_tokens, execution_id)
VALUES (now(), $1, 7, '/llm/v1/chat/completions', 'POST', $2, 100,
        'openai', 'gpt-4o', 10, 20, $3)`, agentAnalyticsProject, status, executionID)
		require.NoError(t, err, "seed request log row")
	}
}

func seedUnattributedRequests(t *testing.T, pool *pgxpool.Pool, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		_, err := pool.Exec(context.Background(), `
INSERT INTO gateway.llm_request_logs
    (occurred_at, project_id, user_id, route, method, status, duration_ms,
     provider, model, prompt_tokens, completion_tokens)
VALUES (now(), $1, 7, '/llm/v1/chat/completions', 'POST', 200, 50,
        'openai', 'gpt-4o', 1, 1)`, agentAnalyticsProject)
		require.NoError(t, err)
	}
}

// TestGetAgentAnalytics_AnEmptyWindowIsUnavailableRatherThanZero is the
// no-backfill contract.
//
// A window with traffic but no execution ids is a PRE-MIGRATION window, or a
// deployment whose runtime is not tagging its calls. Neither is "no agent ran",
// and answering with an empty list beside a live llm_calls figure would render
// "0 agent runs" for a month in which agents ran constantly.
//
// This is the case a backfill would have destroyed: had migration 0100 written
// a value into the pre-existing rows, this window would report agents that
// nothing measured.
func TestGetAgentAnalytics_AnEmptyWindowIsUnavailableRatherThanZero(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)

	seedUnattributedRequests(t, pool, 3)

	breakdown, err := repo.GetAgentAnalytics(context.Background(), agentAnalyticsWindow())
	require.NoError(t, err)

	require.False(t, breakdown.Available,
		"a window with no execution ids must report the dimension as unavailable, not as zero agents")
	require.Nil(t, breakdown.Agents,
		"the breakdown must be ABSENT rather than an empty list when it is unavailable")
	require.EqualValues(t, 0, breakdown.AttributedCalls)
	require.EqualValues(t, 3, breakdown.UnattributedCalls,
		"the window's real traffic must still be reported; the absence is of the agent dimension, not of the requests")
}

// TestGetAgentAnalytics_ResolvesTheAgentThroughExecutionJobs is the read-time
// resolution: execution_id -> execution_jobs (the guard) -> the chat projection
// -> the agent's name.
func TestGetAgentAnalytics_ResolvesTheAgentThroughExecutionJobs(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)

	seedAgentExecution(t, pool, "exec-alpha", 4001, "Research Agent", 4, 1)
	seedAgentExecution(t, pool, "exec-beta", 4002, "Support Agent", 2, 0)
	seedUnattributedRequests(t, pool, 5)

	breakdown, err := repo.GetAgentAnalytics(context.Background(), agentAnalyticsWindow())
	require.NoError(t, err)

	require.True(t, breakdown.Available)
	require.EqualValues(t, 6, breakdown.AttributedCalls)
	require.EqualValues(t, 5, breakdown.UnattributedCalls,
		"the per-agent rows are NOT a partition of the project's traffic, and both halves must be published")
	require.False(t, breakdown.Truncated)

	require.Len(t, breakdown.Agents, 2)

	// Busiest first.
	first := breakdown.Agents[0]
	require.Equal(t, "4001", first.ApplicationID)
	require.Equal(t, "Research Agent", first.Name,
		"the display name comes from the tenant applications table; an empty name means the join did not run")
	require.EqualValues(t, 4, first.RunCount)
	require.EqualValues(t, 4*30, first.TotalTokens)
	require.InDelta(t, 100.0, first.AvgDuration, 0.01)
	require.InDelta(t, 25.0, first.ErrorRate, 0.01, "1 failure in 4 requests is 25%")

	second := breakdown.Agents[1]
	require.Equal(t, "4002", second.ApplicationID)
	require.Equal(t, "Support Agent", second.Name)
	require.EqualValues(t, 2, second.RunCount)
	require.InDelta(t, 0.0, second.ErrorRate, 0.01)
}

// TestGetAgentAnalytics_PricesEachAgentsCallsAtTheCatalogueRate is issue #875's
// acceptance case: cost attributable to an agent.
//
// The price is applied at the ROW's own rate before the per-agent sum — the
// same order estimate.go's by_model and by_user reads already use — so an
// agent whose calls addressed several models would still get the right total.
// Here one model is enough to pin the arithmetic itself: 4 calls of 10 prompt
// + 20 completion tokens at $5 / $10 per 1,000,000 tokens is
// 4*(10*5 + 20*10)/1e6 = $0.001 exactly.
func TestGetAgentAnalytics_PricesEachAgentsCallsAtTheCatalogueRate(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)

	seedAgentExecution(t, pool, "exec-alpha", 4001, "Research Agent", 4, 0)
	_, err := pool.Exec(context.Background(), `
INSERT INTO gateway.gateway_models (provider, model_name, input_cost_per_1m_tokens, output_cost_per_1m_tokens)
VALUES ('openai', 'gpt-4o', 5, 10)`)
	require.NoError(t, err, "seed the price catalogue")

	breakdown, err := repo.GetAgentAnalytics(context.Background(), agentAnalyticsWindow())
	require.NoError(t, err)
	require.Len(t, breakdown.Agents, 1)

	agent := breakdown.Agents[0]
	require.True(t, agent.Priced, "the catalogue prices this agent's only model")
	require.NotNil(t, agent.TotalCost, "priced must publish money, not nil")
	require.Equal(t, "0.000200000000", agent.InputCost.String())
	require.Equal(t, "0.000800000000", agent.OutputCost.String())
	require.Equal(t, "0.001000000000", agent.TotalCost.String())
}

// TestGetAgentAnalytics_AnUnpricedModelKeepsTokensAndOmitsMoney: the catalogue
// carries no rate for the model this agent addressed, so the row keeps its
// token counts (already asserted by
// TestGetAgentAnalytics_ResolvesTheAgentThroughExecutionJobs) and Priced stays
// false with the money fields nil — never a fabricated zero, for the reason
// estimate.go's own by_model rows refuse one.
func TestGetAgentAnalytics_AnUnpricedModelKeepsTokensAndOmitsMoney(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)

	seedAgentExecution(t, pool, "exec-alpha", 4001, "Research Agent", 4, 0)
	// Deliberately no gateway.gateway_models row for openai/gpt-4o.

	breakdown, err := repo.GetAgentAnalytics(context.Background(), agentAnalyticsWindow())
	require.NoError(t, err)
	require.Len(t, breakdown.Agents, 1)

	agent := breakdown.Agents[0]
	require.False(t, agent.Priced)
	require.Nil(t, agent.InputCost)
	require.Nil(t, agent.OutputCost)
	require.Nil(t, agent.TotalCost)
	require.EqualValues(t, 4*30, agent.TotalTokens, "the tokens must survive the absence of a price")
}

// TestGetAgentAnalytics_ARetriedExecutionIsNotCountedTwice.
//
// elitea_runtime.execution_jobs is keyed (execution_id, generation), so a JOIN
// on the id alone multiplies every request by the number of retries the turn
// had. The read uses EXISTS for exactly this reason, and a second generation is
// the only thing that can tell the two shapes apart.
func TestGetAgentAnalytics_ARetriedExecutionIsNotCountedTwice(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)

	seedAgentExecution(t, pool, "exec-retry", 4003, "Retried Agent", 3, 0)

	_, err := pool.Exec(context.Background(), `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state
) VALUES (
    'exec-retry', 2, 'cmd-exec-retry-2', '1', $1, $1, 'actor-1', 'actor-1',
    'agent.execute.application.v1', 'v1', 'bundle-exec-retry',
    decode(repeat('61', 32), 'hex'), 'scope-exec-retry-2', 'key-exec-retry-2',
    'SUCCEEDED', 'RUNNING'
)`, agentAnalyticsProject)
	require.NoError(t, err, "seed a second generation")

	breakdown, err := repo.GetAgentAnalytics(context.Background(), agentAnalyticsWindow())
	require.NoError(t, err)

	require.EqualValues(t, 3, breakdown.AttributedCalls,
		"a second execution generation must not double the request count")
	require.Len(t, breakdown.Agents, 1)
	require.EqualValues(t, 3, breakdown.Agents[0].RunCount)
}

// TestGetAgentAnalytics_ANonAgentExecutionIsNotAnAgentRun.
//
// execution_jobs also holds configuration validations and index ingests —
// executions the platform made for itself. Folding those into an agent
// breakdown would report runs no agent performed, so the capability list is a
// fixed pair rather than a prefix match.
func TestGetAgentAnalytics_ANonAgentExecutionIsNotAnAgentRun(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ('bundle-validate', 'admission:bundle-validate', $1,
        'application/x-protobuf', decode(repeat('61', 32), 'hex'), 1,
        decode('00', 'hex'), 'actor-1')`,
		agentAnalyticsProject)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, configuration_revision_id, configuration_type,
    catalog_revision, catalog_digest, schema_id, schema_revision, schema_digest,
    settings_entry_id, state, desired_state
) VALUES (
    'exec-validate', 1, 'cmd-exec-validate', '1', $1, $1, 'actor-1', 'actor-1',
    'configuration.validate.v1', 'v1', 'bundle-validate',
    decode(repeat('61', 32), 'hex'), 'scope-validate', 'key-validate', 'rev-validate',
    'application', 'catalog-1', decode(repeat('61', 32), 'hex'), 'schema-1', 'rev-1',
    decode(repeat('61', 32), 'hex'), 'agent-request', 'SUCCEEDED', 'RUNNING'
)`, agentAnalyticsProject)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
INSERT INTO gateway.llm_request_logs
    (occurred_at, project_id, user_id, route, method, status, duration_ms,
     provider, model, prompt_tokens, completion_tokens, execution_id)
VALUES (now(), $1, 7, '/llm/v1/chat/completions', 'POST', 200, 10,
        'openai', 'gpt-4o', 1, 1, 'exec-validate')`, agentAnalyticsProject)
	require.NoError(t, err)

	breakdown, err := repo.GetAgentAnalytics(ctx, agentAnalyticsWindow())
	require.NoError(t, err)

	require.False(t, breakdown.Available,
		"a configuration validation is not an agent run")
	require.EqualValues(t, 0, breakdown.AttributedCalls)
	require.EqualValues(t, 1, breakdown.UnattributedCalls)
}

// TestGetAgentAnalytics_AnExecutionFromAnotherProjectDoesNotAttribute.
//
// The execution id reaches the gateway signed, so a caller cannot normally
// attach one at all. This pins the second line of defence: the guard requires
// one of execution_jobs' two project columns to agree with the project the LOG
// row already named, so an id belonging elsewhere resolves to nothing.
//
// The guard is a CHECK and not the project source. llm_request_logs.project_id
// stays the only column that scopes the window; execution_jobs is never asked
// "which project is this", because with resource_project_id AND
// projection_project_id it has no single answer.
func TestGetAgentAnalytics_AnExecutionFromAnotherProjectDoesNotAttribute(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()

	seedAgentExecution(t, pool, "exec-foreign", 4004, "Foreign Agent", 0, 0)

	// Re-point BOTH project columns away from project 1, then log a request
	// under project 1 that claims the id.
	var otherProject int64
	require.NoError(t, pool.QueryRow(ctx, `
INSERT INTO centry.project (id) VALUES (9001) RETURNING id`).Scan(&otherProject))

	_, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
   SET resource_project_id = $1, projection_project_id = $1
 WHERE execution_id = 'exec-foreign'`, otherProject)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
INSERT INTO gateway.llm_request_logs
    (occurred_at, project_id, user_id, route, method, status, duration_ms,
     provider, model, prompt_tokens, completion_tokens, execution_id)
VALUES (now(), $1, 7, '/llm/v1/chat/completions', 'POST', 200, 10,
        'openai', 'gpt-4o', 1, 1, 'exec-foreign')`, agentAnalyticsProject)
	require.NoError(t, err)

	breakdown, err := repo.GetAgentAnalytics(ctx, agentAnalyticsWindow())
	require.NoError(t, err)

	require.False(t, breakdown.Available,
		"an execution belonging to another project must not attribute in this one")
	require.EqualValues(t, 0, breakdown.AttributedCalls)
	require.EqualValues(t, 1, breakdown.UnattributedCalls)
}

// TestGetAgentAnalytics_RefusesWhenTheColumnIsAbsent.
//
// A deployment that has not run migration 0100 gets a NAMED absence, not a 200
// with an empty breakdown. The probe has to happen BEFORE the statement: this
// read runs on a snapshot transaction, and a 42703 caught afterwards would have
// already poisoned it.
func TestGetAgentAnalytics_RefusesWhenTheColumnIsAbsent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)

	_, err := pool.Exec(context.Background(),
		`ALTER TABLE gateway.llm_request_logs DROP COLUMN execution_id`)
	require.NoError(t, err)

	_, err = repo.GetAgentAnalytics(context.Background(), agentAnalyticsWindow())
	require.ErrorIs(t, err, analytics.ErrNoSource)
	require.Contains(t, err.Error(), "0100",
		"the refusal must name the migration that has not run")
}

// TestGetAgentAnalytics_SurvivesAnAbsentApplicationsTable.
//
// p_<id>.applications is pylon-owned and no migration in this service creates
// it, so a Go-bootstrapped database legitimately has none. An absent name table
// must cost the DISPLAY NAME and nothing else: dropping the rows would silently
// shrink the breakdown, and refusing the read because a decoration is missing
// is the "absence reads as failure" defect.
func TestGetAgentAnalytics_SurvivesAnAbsentApplicationsTable(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)

	seedAgentExecution(t, pool, "exec-nameless", 4005, "Nameless Agent", 2, 0)

	_, err := pool.Exec(context.Background(), `DROP TABLE p_1.applications`)
	require.NoError(t, err)

	breakdown, err := repo.GetAgentAnalytics(context.Background(), agentAnalyticsWindow())
	require.NoError(t, err, "an absent pylon table must not fail the read")

	require.True(t, breakdown.Available)
	require.EqualValues(t, 2, breakdown.AttributedCalls)
	require.Len(t, breakdown.Agents, 1, "the row must survive; only its name is unavailable")
	require.Equal(t, "4005", breakdown.Agents[0].ApplicationID)
	require.Equal(t, "", breakdown.Agents[0].Name)
	require.EqualValues(t, 2, breakdown.Agents[0].RunCount)
}
