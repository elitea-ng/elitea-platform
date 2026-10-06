package repos

import (
	"context"
	"errors"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestPostgresCurrentAgentCancelAllowsPausedRootSettledExecution(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	for _, test := range []struct {
		name         string
		responseID   string
		questionID   string
		questionItem string
		executionID  string
		state        string
		desiredState string
		metaSQL      string
		metaArg      string
	}{
		{
			name:         "paused hitl root",
			responseID:   "30000000-0000-4000-8000-000000000141",
			questionID:   "20000000-0000-4000-8000-000000000141",
			questionItem: "40000000-0000-4000-8000-000000000141",
			executionID:  "execution-stop-paused-hitl",
			state:        "SUCCEEDED",
			desiredState: "RUNNING",
			metaSQL: `
UPDATE chat_message_group
SET is_streaming = FALSE,
    meta = meta || jsonb_build_object(
        'thread_id', 'thread-stop-paused-hitl',
        'hitl_interrupt', ($2::jsonb -> 0),
        'hitl_interrupts', $2::jsonb
    )
WHERE uuid = $1`,
			metaArg: `[{"interrupt_id":"interrupt-stop-hitl-1","available_actions":["approve","reject"]}]`,
		},
		{
			name:         "paused authorization root",
			responseID:   "30000000-0000-4000-8000-000000000142",
			questionID:   "20000000-0000-4000-8000-000000000142",
			questionItem: "40000000-0000-4000-8000-000000000142",
			executionID:  "execution-stop-paused-auth",
			state:        "SUCCEEDED",
			desiredState: "RUNNING",
			metaSQL: `
UPDATE chat_message_group
SET is_streaming = FALSE,
    meta = meta || jsonb_build_object(
        'thread_id', 'thread-stop-paused-auth',
        'authorization_requests', $2::jsonb
    )
WHERE uuid = $1`,
			metaArg: `[{"tool_run_id":"tool-run-sharepoint-1","toolkit_name":"SharePoint"}]`,
		},
		{
			name:         "failed execution with abandoned stream",
			responseID:   "30000000-0000-4000-8000-000000000143",
			questionID:   "20000000-0000-4000-8000-000000000143",
			questionItem: "40000000-0000-4000-8000-000000000143",
			executionID:  "execution-stop-failed-stream",
			state:        "FAILED",
			desiredState: "RUNNING",
			metaSQL: `
UPDATE chat_message_group
SET meta = meta || jsonb_build_object('is_error', true, 'error', $2::text)
WHERE uuid = $1`,
			metaArg: `dependency unavailable`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
				t.Fatal(err)
			}
			queries := sqlcgen.New(tx)
			conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
			responseMessageID := insertPostgresCurrentApplicationTurn(
				t,
				queries,
				conversationID,
				test.questionID,
				test.questionItem,
				test.responseID,
				"stop the paused execution",
				test.executionID,
			)
			insertPostgresCurrentAgentCancelBinding(
				t,
				tx,
				responseMessageID,
				test.questionID,
				test.executionID,
				"agent.execute.application.v1",
				test.state,
				test.desiredState,
			)
			if _, err := tx.Exec(t.Context(), test.metaSQL, responseMessageID, test.metaArg); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}

			repository, err := NewCurrentAgentCancelRepository(pool)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := repository.CancelCurrentAgent(
				t.Context(),
				agentexecutionapp.CurrentAgentCancelRequest{
					ProjectID:         1,
					ActorUserID:       11,
					ResponseMessageID: uuid.UUID(responseMessageID.Bytes).String(),
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.Deleted || outcome.Salvaged || outcome.Replay {
				t.Fatalf("outcome=%+v", outcome)
			}

			var desiredState string
			if err := pool.QueryRow(t.Context(), `
SELECT desired_state
FROM elitea_runtime.execution_jobs
WHERE execution_id = $1
  AND generation = 1`, test.executionID).Scan(&desiredState); err != nil {
				t.Fatal(err)
			}
			if desiredState != "CANCELLED" {
				t.Fatalf("desired_state=%q", desiredState)
			}

			var remainingResponses int
			if err := pool.QueryRow(t.Context(), `
SELECT count(*)
FROM p_1.chat_message_group
WHERE uuid = $1`, responseMessageID).Scan(&remainingResponses); err != nil {
				t.Fatal(err)
			}
			if remainingResponses != 0 {
				t.Fatalf("remaining response rows=%d", remainingResponses)
			}
		})
	}
}

func TestPostgresCurrentAgentCancelRejectsSettledExecutionWithoutPauseProjection(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	const (
		questionID   = "20000000-0000-4000-8000-000000000151"
		questionItem = "40000000-0000-4000-8000-000000000151"
		responseID   = "30000000-0000-4000-8000-000000000151"
		executionID  = "execution-stop-terminal-complete"
	)
	responseMessageID := insertPostgresCurrentApplicationTurn(
		t,
		queries,
		conversationID,
		questionID,
		questionItem,
		responseID,
		"do not stop a completed execution",
		executionID,
	)
	insertPostgresCurrentAgentCancelBinding(
		t,
		tx,
		responseMessageID,
		questionID,
		executionID,
		"agent.execute.application.v1",
		"SUCCEEDED",
		"RUNNING",
	)
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group
SET is_streaming = FALSE
WHERE uuid = $1`, responseMessageID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	repository, err := NewCurrentAgentCancelRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.CancelCurrentAgent(
		t.Context(),
		agentexecutionapp.CurrentAgentCancelRequest{
			ProjectID:         1,
			ActorUserID:       11,
			ResponseMessageID: uuid.UUID(responseMessageID.Bytes).String(),
		},
	)
	if !errors.Is(err, agentexecutionapp.ErrCurrentAgentCancelNotAllowed) {
		t.Fatalf("error=%v", err)
	}

	var desiredState string
	if err := pool.QueryRow(t.Context(), `
SELECT desired_state
FROM elitea_runtime.execution_jobs
WHERE execution_id = $1
  AND generation = 1`, executionID).Scan(&desiredState); err != nil {
		t.Fatal(err)
	}
	if desiredState != "RUNNING" {
		t.Fatalf("desired_state=%q", desiredState)
	}
}

func insertPostgresCurrentAgentCancelBinding(
	t *testing.T,
	tx pgx.Tx,
	responseMessageID pgtype.UUID,
	clientExecutionGeneration,
	executionID,
	capabilityID,
	state,
	desiredState string,
) {
	t.Helper()
	const (
		inputBundleID  = "input-bundle-current-agent-cancel"
		requestEntryID = "request"
	)
	if _, err := tx.Exec(t.Context(), `
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by, created_at
) VALUES (
	$1, '1', 'application/x-protobuf', 1,
    decode(repeat('ab', 32), 'hex'), 2, '{}'::bytea, 'tests', clock_timestamp()
)
	ON CONFLICT (input_bundle_id) DO NOTHING`, inputBundleID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
INSERT INTO elitea_runtime.input_bundle_entries (
    input_bundle_id, entry_id, entry_version, semantic_role, media_type,
    content_digest, content_size, content_reference, classification,
    required_grant_audience, content_bytes
) VALUES (
    $1, $2, '1', 'request', 'application/json',
    decode(repeat('cd', 32), 'hex'), 2, 'inline://request', 'internal',
    'worker', '{}'::bytea
)
	ON CONFLICT (input_bundle_id, entry_id) DO NOTHING`, inputBundleID, requestEntryID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state, admitted_at, settled_at
) VALUES (
	$1, 1, $2, '1', 1,
	1, '11', 'user:11', $3,
	'1', $4, decode(repeat('ef', 32), 'hex'), 'current-agent-stop',
	$5, $6, $7, clock_timestamp(),
	CASE WHEN $6 IN ('SUCCEEDED', 'FAILED', 'CANCELLED') THEN clock_timestamp() ELSE NULL END
)`,
		executionID,
		"command-"+executionID,
		capabilityID,
		inputBundleID,
		"idempotency-"+executionID,
		state,
		desiredState,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
INSERT INTO elitea_runtime.agent_execution_jobs (
    execution_id, generation, capability_id, input_bundle_id, request_entry_id,
    client_stream_id, client_message_id, client_execution_generation, sio_event
) VALUES (
	$1, 1, $2, $3, $4,
	$5, $6, $7, 'chat_predict'
)`,
		executionID,
		capabilityID,
		inputBundleID,
		requestEntryID,
		"stream-"+executionID,
		uuid.UUID(responseMessageID.Bytes).String(),
		clientExecutionGeneration,
	); err != nil {
		t.Fatal(err)
	}
}

// TestPostgresCurrentAgentCancelIsIdempotentAndSurvivesSettlement pins the
// semantics client contract 1.3 publishes for cancelChatExecution: a stop
// repeated by the same caller is a replay (the route's 204), also after the
// worker settled the run as cancelled; another user's stop is refused.
func TestPostgresCurrentAgentCancelIsIdempotentAndSurvivesSettlement(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	const (
		questionID   = "20000000-0000-4000-8000-000000000161"
		questionItem = "40000000-0000-4000-8000-000000000161"
		responseID   = "30000000-0000-4000-8000-000000000161"
		executionID  = "execution-stop-running-twice"
	)
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	responseMessageID := insertPostgresCurrentApplicationTurn(
		t,
		sqlcgen.New(tx),
		mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"),
		questionID,
		questionItem,
		responseID,
		"stop me twice",
		executionID,
	)
	insertPostgresCurrentAgentCancelBinding(
		t, tx, responseMessageID, questionID, executionID,
		"agent.execute.application.v1", "RUNNING", "RUNNING",
	)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	repository, err := NewCurrentAgentCancelRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	cancel := func(actor int64) (agentexecutionapp.CurrentAgentCancelOutcome, error) {
		return repository.CancelCurrentAgent(t.Context(), agentexecutionapp.CurrentAgentCancelRequest{
			ProjectID:         1,
			ActorUserID:       actor,
			ResponseMessageID: uuid.UUID(responseMessageID.Bytes).String(),
		})
	}

	first, err := cancel(11)
	if err != nil || first.Replay {
		t.Fatalf("first stop: outcome=%+v err=%v, want a fresh stop", first, err)
	}
	second, err := cancel(11)
	if err != nil || !second.Replay {
		t.Fatalf("second stop: outcome=%+v err=%v, want a replay", second, err)
	}

	// The worker observes CANCELLED and settles the run.
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_runtime.execution_jobs
SET state = 'CANCELLED', settled_at = clock_timestamp()
WHERE execution_id = $1 AND generation = 1`, executionID); err != nil {
		t.Fatal(err)
	}
	settled, err := cancel(11)
	if err != nil || !settled.Replay {
		t.Fatalf("stop after settlement: outcome=%+v err=%v, want a replay", settled, err)
	}

	if _, err := cancel(12); !errors.Is(err, agentexecutionapp.ErrCurrentAgentCancelNotAllowed) {
		t.Fatalf("another user's stop: err=%v, want ErrCurrentAgentCancelNotAllowed", err)
	}
}
