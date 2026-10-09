package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
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

// TestPostgresCurrentAgentCancelReplayAcceptsConversationAuthor pins the 1.3
// idempotency promise for the second principal cancelChatExecution admits:
// the conversation author stopping a turn another member asked. A stop of a
// turn with no output deletes the question and the empty answer, so the retry
// cannot find the target again and must be recognised as a replay from the
// job/binding alone. Before the fix the replay matched only the job's actor
// (the asker), and the author's retry answered 409.
func TestPostgresCurrentAgentCancelReplayAcceptsConversationAuthor(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	const (
		conversationUUID = "10000000-0000-4000-8000-000000000031"
		questionID       = "20000000-0000-4000-8000-000000000171"
		questionItem     = "40000000-0000-4000-8000-000000000171"
		responseID       = "30000000-0000-4000-8000-000000000171"
		executionID      = "execution-stop-by-conversation-author"
		authorID         = 99 // owns the conversation, did not ask
		askerID          = 11 // asked the question; the job's actor
		strangerID       = 12
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
		mustCurrentPGUUID(t, conversationUUID),
		questionID,
		questionItem,
		responseID,
		"stopped by the conversation author",
		executionID,
	)
	insertPostgresCurrentAgentCancelBinding(
		t, tx, responseMessageID, questionID, executionID,
		"agent.execute.application.v1", "RUNNING", "RUNNING",
	)
	// Admission pins client_stream_id to the conversation uuid; the shared
	// fixture helper writes a placeholder, so put the real value back.
	if _, err := tx.Exec(t.Context(), `
UPDATE elitea_runtime.agent_execution_jobs
SET client_stream_id = $2
WHERE execution_id = $1`, executionID, conversationUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_conversations SET author_id = $1 WHERE uuid = $2::uuid`,
		authorID, conversationUUID); err != nil {
		t.Fatal(err)
	}
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

	first, err := cancel(authorID)
	if err != nil || first.Replay || !first.Deleted {
		t.Fatalf("author's first stop: outcome=%+v err=%v, want a fresh stop that deletes the empty pair", first, err)
	}
	second, err := cancel(authorID)
	if err != nil || !second.Replay {
		t.Fatalf("author's retry: outcome=%+v err=%v, want a replay", second, err)
	}
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_runtime.execution_jobs
SET state = 'CANCELLED', settled_at = clock_timestamp()
WHERE execution_id = $1 AND generation = 1`, executionID); err != nil {
		t.Fatal(err)
	}
	settled, err := cancel(authorID)
	if err != nil || !settled.Replay {
		t.Fatalf("author's stop after settlement: outcome=%+v err=%v, want a replay", settled, err)
	}
	if asker, err := cancel(askerID); err != nil || !asker.Replay {
		t.Fatalf("asker's stop: outcome=%+v err=%v, want a replay", asker, err)
	}
	if _, err := cancel(strangerID); !errors.Is(err, agentexecutionapp.ErrCurrentAgentCancelNotAllowed) {
		t.Fatalf("a stranger's stop: err=%v, want ErrCurrentAgentCancelNotAllowed", err)
	}
}

// TestPostgresCurrentAgentCancelConcurrentStopsOfAnEmptyTurnBothAnswer204:
// a double tap (or a stop plus its timeout retry) on a turn with no output.
// Both stops read the answer before either commits; the first deletes the
// empty pair, and the second, re-checking the job row after the first's
// lock is released, still matches `desired_state = 'CANCELLED'` and so
// skips the replay path, while its projection finds no rows. That second
// stop is the same caller's repeated stop and must be a replay (204), not
// "projection did not settle" (502). A held lock on the job row lines both
// stops up behind it so the interleaving is deterministic.
func TestPostgresCurrentAgentCancelConcurrentStopsOfAnEmptyTurnBothAnswer204(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	const (
		questionID   = "20000000-0000-4000-8000-000000000181"
		questionItem = "40000000-0000-4000-8000-000000000181"
		responseID   = "30000000-0000-4000-8000-000000000181"
		executionID  = "execution-stop-double-tap"
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
		"stop me twice at once",
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

	blocker, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	var blockerPID int32
	if err := blocker.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(t.Context(), `
SELECT 1 FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = 1
FOR UPDATE`, executionID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		outcome agentexecutionapp.CurrentAgentCancelOutcome
		err     error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			outcome, err := repository.CancelCurrentAgent(t.Context(), agentexecutionapp.CurrentAgentCancelRequest{
				ProjectID:         1,
				ActorUserID:       11,
				ResponseMessageID: uuid.UUID(responseMessageID.Bytes).String(),
			})
			results <- result{outcome: outcome, err: err}
		}()
	}
	// Wait until both stops are queued on the job row (the second behind the
	// first's tuple lock): each has already taken the statement snapshot
	// that still sees the answer.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database() AND pid <> $1::int
  AND wait_event_type = 'Lock' AND cardinality(pg_blocking_pids(pid)) > 0`,
			blockerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d stop(s) queued behind the job row lock", waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	var deleted, replayed int
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatalf("a concurrent stop failed: %v (the route answers 502)", got.err)
		}
		switch {
		case got.outcome.Deleted && !got.outcome.Replay:
			deleted++
		case got.outcome.Replay && !got.outcome.Deleted:
			replayed++
		default:
			t.Fatalf("outcome=%+v", got.outcome)
		}
	}
	if deleted != 1 || replayed != 1 {
		t.Fatalf("deleted=%d replayed=%d, want one fresh stop that deletes the empty pair and one replay", deleted, replayed)
	}
}

// TestPostgresCurrentAgentCancelReplayAdmitsTheQuestionAuthorOfARegeneration:
// a shared conversation whose owner regenerated another member's question,
// so the job's actor is the owner while the question's author is the member.
// The member may stop it (CancelCurrentAgentExecution admits the question
// author), and the stop of the empty turn deletes the question and answer.
// The member's retry must replay (204) like any repeated stop by the same
// caller; before the fix the replay admitted the job's actor and the
// conversation author only, and the member's retry answered 409. The other
// direction: a job actor with no standing on the turn is not admitted by the
// replay just because someone else stopped it.
func TestPostgresCurrentAgentCancelReplayAdmitsTheQuestionAuthorOfARegeneration(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	const (
		conversationUUID = "10000000-0000-4000-8000-000000000031"
		ownerID          = 99 // owns the conversation and regenerated the question
		memberID         = 11 // asked the question (its author participant)
		strangerID       = 12
		lapsedActorID    = 55 // a job actor with no standing on the turn
	)
	repository, err := NewCurrentAgentCancelRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	seed := func(suffix, executionID string, jobActor int) pgtype.UUID {
		t.Helper()
		tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
			t.Fatal(err)
		}
		questionID := "20000000-0000-4000-8000-000000000" + suffix
		responseMessageID := insertPostgresCurrentApplicationTurn(
			t,
			sqlcgen.New(tx),
			mustCurrentPGUUID(t, conversationUUID),
			questionID,
			"40000000-0000-4000-8000-000000000"+suffix,
			"30000000-0000-4000-8000-000000000"+suffix,
			"regenerated by the conversation owner",
			executionID,
		)
		insertPostgresCurrentAgentCancelBinding(
			t, tx, responseMessageID, questionID, executionID,
			"agent.execute.application.v1", "RUNNING", "RUNNING",
		)
		if _, err := tx.Exec(t.Context(), `
UPDATE elitea_runtime.agent_execution_jobs
SET client_stream_id = $2
WHERE execution_id = $1`, executionID, conversationUUID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `
UPDATE elitea_runtime.execution_jobs
SET actor_id = $2::bigint::text, principal_ref = 'user:' || $2::bigint::text
WHERE execution_id = $1`, executionID, jobActor); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `
UPDATE chat_conversations SET author_id = $1 WHERE uuid = $2::uuid`,
			ownerID, conversationUUID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		return responseMessageID
	}
	cancel := func(responseMessageID pgtype.UUID, actor int64) (agentexecutionapp.CurrentAgentCancelOutcome, error) {
		return repository.CancelCurrentAgent(t.Context(), agentexecutionapp.CurrentAgentCancelRequest{
			ProjectID:         1,
			ActorUserID:       actor,
			ResponseMessageID: uuid.UUID(responseMessageID.Bytes).String(),
		})
	}

	regenerated := seed("191", "execution-stop-regenerated-by-owner", ownerID)
	first, err := cancel(regenerated, memberID)
	if err != nil || first.Replay || !first.Deleted {
		t.Fatalf("member's first stop: outcome=%+v err=%v, want a fresh stop that deletes the empty pair", first, err)
	}
	if retry, err := cancel(regenerated, memberID); err != nil || !retry.Replay {
		t.Fatalf("member's retry: outcome=%+v err=%v, want a replay", retry, err)
	}
	if owner, err := cancel(regenerated, ownerID); err != nil || !owner.Replay {
		t.Fatalf("owner's stop: outcome=%+v err=%v, want a replay", owner, err)
	}
	if _, err := cancel(regenerated, strangerID); !errors.Is(err, agentexecutionapp.ErrCurrentAgentCancelNotAllowed) {
		t.Fatalf("a stranger's stop: err=%v, want ErrCurrentAgentCancelNotAllowed", err)
	}

	lapsed := seed("192", "execution-stop-lapsed-job-actor", lapsedActorID)
	if first, err := cancel(lapsed, memberID); err != nil || !first.Deleted {
		t.Fatalf("member's stop of the lapsed actor's run: outcome=%+v err=%v", first, err)
	}
	if _, err := cancel(lapsed, lapsedActorID); !errors.Is(err, agentexecutionapp.ErrCurrentAgentCancelNotAllowed) {
		t.Fatalf("a job actor with no standing: err=%v, want ErrCurrentAgentCancelNotAllowed", err)
	}
}

// TestPostgresCurrentAgentStopSettlesAKeptAnswerAsNotAnError pins the client
// contract's settle check on a stopped turn that kept partial output
// (agent-zefir#21): an answer is settled when `is_streaming` is false AND
// `metadata.is_error` is PRESENT; an absent `is_error` reads as "still
// running". Both writers of a stopped answer left it absent: the synchronous
// Stop projection never set it, and the worker's later CANCELLED terminal
// deleted it.
func TestPostgresCurrentAgentStopSettlesAKeptAnswerAsNotAnError(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	const (
		questionID   = "20000000-0000-4000-8000-000000000171"
		questionItem = "40000000-0000-4000-8000-000000000171"
		responseID   = "30000000-0000-4000-8000-000000000171"
		executionID  = "execution-stop-keeps-partial-output"

		currentAgentStopConversationID = "10000000-0000-4000-8000-000000000031"
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
		t, sqlcgen.New(tx),
		mustCurrentPGUUID(t, currentAgentStopConversationID),
		questionID, questionItem, responseID, "tell me a long story", executionID,
	)
	insertPostgresCurrentAgentCancelBinding(
		t, tx, responseMessageID, questionID, executionID,
		"agent.execute.application.v1", "RUNNING", "RUNNING",
	)
	// The partial answer the worker streamed before the stop.
	if _, err := tx.Exec(t.Context(), `
WITH item AS (
    INSERT INTO chat_message_items (uuid, item_type, order_index, meta, message_group_id)
    SELECT gen_random_uuid(), 'text_message', 0, '{}'::jsonb, response.id
    FROM chat_message_group AS response WHERE response.uuid = $1
    RETURNING id
)
INSERT INTO chat_messages_text (id, content) SELECT id, 'Once upon a' FROM item`, responseMessageID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	settled := func(stage string) {
		t.Helper()
		var isStreaming bool
		var isError *string
		if err := pool.QueryRow(t.Context(), `
SELECT is_streaming, meta ->> 'is_error'
FROM p_1.chat_message_group WHERE uuid = $1`, responseMessageID).Scan(&isStreaming, &isError); err != nil {
			t.Fatal(err)
		}
		if isStreaming || isError == nil || *isError != "false" {
			got := "<absent>"
			if isError != nil {
				got = *isError
			}
			t.Fatalf("%s: is_streaming=%t is_error=%s, want a settled answer with is_error false", stage, isStreaming, got)
		}
	}

	repository, err := NewCurrentAgentCancelRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := repository.CancelCurrentAgent(t.Context(), agentexecutionapp.CurrentAgentCancelRequest{
		ProjectID:         1,
		ActorUserID:       11,
		ResponseMessageID: uuid.UUID(responseMessageID.Bytes).String(),
	})
	if err != nil || outcome.Deleted || outcome.Replay {
		t.Fatalf("stop: outcome=%+v err=%v, want the partial answer kept", outcome, err)
	}
	settled("after the Stop projection")

	// The worker's CANCELLED terminal arrives later. Start it from a row as an
	// older stop left it (no is_error), so a terminal that missed the row
	// could not pass by leaving the Stop projection's value in place.
	if _, err := pool.Exec(t.Context(), `
UPDATE p_1.chat_message_group SET meta = meta - 'is_error' WHERE uuid = $1`, responseMessageID); err != nil {
		t.Fatal(err)
	}
	// The terminal matches its response through the binding's client_stream_id,
	// which admission pins to the conversation uuid. The shared cancel fixture
	// leaves a placeholder there, and a CANCELLED terminal that matches no row
	// is (deliberately) a silent no-op, so pin the real value.
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_runtime.agent_execution_jobs SET client_stream_id = $1
WHERE execution_id = $2`, currentAgentStopConversationID, executionID); err != nil {
		t.Fatal(err)
	}
	if err := persistCurrentAgentRuntimeTerminal(
		t.Context(), pgxExecutor{queryer: pool}, 1, executiondomain.AgentApplicationCapability,
		outputRecord{ExecutionID: executionID, Generation: 1}, "CANCELLED", "Execution was cancelled.",
	); err != nil {
		t.Fatal(err)
	}
	settled("after the worker's CANCELLED terminal")
}
