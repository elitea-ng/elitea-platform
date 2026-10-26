package repos

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedSteeringTurn writes one running chat turn of user 11 in project 1 and
// returns its response message id.
func seedSteeringTurn(t *testing.T, pool *pgxpool.Pool, suffix, executionID, state string) string {
	t.Helper()
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	questionID := "20000000-0000-4000-8000-0000000002" + suffix
	responseID := insertPostgresCurrentApplicationTurn(
		t,
		sqlcgen.New(tx),
		mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"),
		questionID,
		"40000000-0000-4000-8000-0000000002"+suffix,
		"30000000-0000-4000-8000-0000000002"+suffix,
		"build the release",
		executionID,
	)
	insertPostgresCurrentAgentCancelBinding(t, tx, responseID, questionID, executionID,
		"agent.execute.application.v1", state, "RUNNING")
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return uuid.UUID(responseID.Bytes).String()
}

func newSteeringIntegrationRepository(t *testing.T) (*SteeringRepository, *pgxpool.Pool) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	repository, err := NewSteeringRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return repository, pool
}

func steeringTarget(response string) agentexecutionapp.SteeringTarget {
	return agentexecutionapp.SteeringTarget{ProjectID: 1, ActorUserID: 11, ResponseMessageID: response}
}

func TestPostgresSteeringQueueEditRemoveAndReplay(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	response := seedSteeringTurn(t, pool, "01", "execution-steer-queue", "RUNNING")
	target := steeringTarget(response)

	queued, effects, err := repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
		SteeringTarget: target, IdempotencyKey: "key-1", Content: "skip the tests",
	})
	if err != nil {
		t.Fatal(err)
	}
	if queued.State != agentexecutionapp.SteeringPending || queued.Revision != 1 || len(effects) != 1 || effects[0].Cursor == 0 {
		t.Fatalf("queued=%+v effects=%+v", queued, effects)
	}

	// A retry with the same key and text answers the same message and
	// appends no second event.
	replay, effects, err := repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
		SteeringTarget: target, IdempotencyKey: "key-1", Content: "skip the tests",
	})
	if err != nil || replay.ID != queued.ID || len(effects) != 0 {
		t.Fatalf("replay=%+v effects=%+v err=%v", replay, effects, err)
	}
	if _, _, err := repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
		SteeringTarget: target, IdempotencyKey: "key-1", Content: "another text",
	}); !errors.Is(err, agentexecutionapp.ErrSteeringIdempotencyConflict) {
		t.Fatalf("conflict err=%v", err)
	}

	edited, _, err := repository.EditSteering(t.Context(), agentexecutionapp.SteeringEditRequest{
		SteeringTarget: target, SteeringID: queued.ID, Content: "fix the build first",
	})
	if err != nil || edited.Revision != 2 || edited.Content != "fix the build first" {
		t.Fatalf("edited=%+v err=%v", edited, err)
	}

	// Another user is not a participant: the queue refuses and the list is empty.
	stranger := target
	stranger.ActorUserID = 12
	if _, _, err := repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
		SteeringTarget: stranger, IdempotencyKey: "key-2", Content: "hello",
	}); !errors.Is(err, agentexecutionapp.ErrSteeringTurnNotRunning) {
		t.Fatalf("stranger err=%v", err)
	}
	if listed, err := repository.ListSteering(t.Context(), stranger); err != nil || len(listed) != 0 {
		t.Fatalf("stranger list=%+v err=%v", listed, err)
	}

	if _, err := repository.RemoveSteering(t.Context(), agentexecutionapp.SteeringRemoveRequest{
		SteeringTarget: target, SteeringID: queued.ID,
	}); err != nil {
		t.Fatal(err)
	}
	// Removal is idempotent.
	if effects, err := repository.RemoveSteering(t.Context(), agentexecutionapp.SteeringRemoveRequest{
		SteeringTarget: target, SteeringID: queued.ID,
	}); err != nil || len(effects) != 0 {
		t.Fatalf("second remove effects=%+v err=%v", effects, err)
	}
	listed, err := repository.ListSteering(t.Context(), target)
	if err != nil || len(listed) != 1 || listed[0].State != agentexecutionapp.SteeringRemoved {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	var events int
	if err := repository.shared.QueryRow(t.Context(), `
SELECT count(*) FROM elitea_runtime.execution_replay_events
WHERE execution_id = 'execution-steer-queue' AND event_type LIKE 'steer.%'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	// queued, edited (queued again), dropped.
	if events != 3 {
		t.Fatalf("steer events=%d", events)
	}
}

func TestPostgresSteeringInjectionWritesTheMessageAfterTheQuestion(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	response := seedSteeringTurn(t, pool, "02", "execution-steer-inject", "RUNNING")
	target := steeringTarget(response)
	for index, text := range []string{"first steer", "second steer"} {
		if _, _, err := repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
			SteeringTarget: target, IdempotencyKey: "inject-" + string(rune('a'+index)), Content: text,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// An old activity time, so the injection's move is visible.
	err := repository.projects.WithinProjectTx(t.Context(), 1, steeringWriteTx, func(tx sqlExecutor) error {
		_, err := tx.Exec(t.Context(), `
UPDATE chat_conversations SET updated_at = now() - interval '1 day'
WHERE id = (SELECT conversation_id FROM chat_message_group WHERE uuid = $1::uuid)`, response)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var claimed []agentexecutionapp.ClaimedSteeringMessage
	err = repository.projects.WithinProjectTx(t.Context(), 1, steeringWriteTx, func(tx sqlExecutor) error {
		var err error
		claimed, err = claimWaitingSteering(t.Context(), tx, "execution-steer-inject", 1, 1)
		if err != nil {
			return err
		}
		_, err = injectAcknowledgedSteering(t.Context(), tx, 1, "execution-steer-inject", 1,
			[]string{claimed[0].ID, claimed[1].ID})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || claimed[0].Content != "first steer" || claimed[1].Content != "second steer" {
		t.Fatalf("claimed=%+v", claimed)
	}
	listed, err := repository.ListSteering(t.Context(), target)
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	for _, message := range listed {
		if message.State != agentexecutionapp.SteeringInjected || message.MessageID == "" || message.InjectedAt == nil {
			t.Fatalf("message=%+v", message)
		}
	}
	// An injected message can no longer be edited or removed.
	if _, _, err := repository.EditSteering(t.Context(), agentexecutionapp.SteeringEditRequest{
		SteeringTarget: target, SteeringID: listed[0].ID, Content: "late",
	}); !errors.Is(err, agentexecutionapp.ErrSteeringNotPending) {
		t.Fatalf("edit err=%v", err)
	}

	// The transcript order: the question, the two steering messages, the answer.
	var order []string
	err = repository.projects.WithinProjectTx(t.Context(), 1, steeringWriteTx, func(tx sqlExecutor) error {
		rows, err := tx.Query(t.Context(), `
SELECT COALESCE(text.content, '<answer>')
FROM chat_message_group AS message_group
LEFT JOIN chat_message_items AS item ON item.message_group_id = message_group.id
LEFT JOIN chat_messages_text AS text ON text.id = item.id
WHERE message_group.conversation_id = (
    SELECT conversation_id FROM chat_message_group WHERE uuid = $1::uuid)
ORDER BY message_group.created_at, message_group.id`, response)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				return err
			}
			order = append(order, text)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build the release", "first steer", "second steer", "<answer>"}
	if len(order) != len(want) {
		t.Fatalf("order=%q", order)
	}
	for index := range want {
		if order[index] != want[index] {
			t.Fatalf("order=%q want=%q", order, want)
		}
	}

	// The injection moved the conversation's activity time.
	var recent bool
	err = repository.projects.WithinProjectTx(t.Context(), 1, steeringWriteTx, func(tx sqlExecutor) error {
		return tx.QueryRow(t.Context(), `
SELECT updated_at > now() - interval '1 minute' FROM chat_conversations
WHERE id = (SELECT conversation_id FROM chat_message_group WHERE uuid = $1::uuid)`, response).Scan(&recent)
	})
	if err != nil || !recent {
		t.Fatalf("activity moved=%v err=%v", recent, err)
	}

	// A stop before any output deletes the turn: its steering goes with it.
	err = repository.projects.WithinProjectTx(t.Context(), 1, steeringWriteTx, func(tx sqlExecutor) error {
		var conversationID int64
		if err := tx.QueryRow(t.Context(), `SELECT conversation_id FROM chat_message_group WHERE uuid = $1::uuid`,
			response).Scan(&conversationID); err != nil {
			return err
		}
		return DiscardTurnSteering(t.Context(), tx, 1, conversationID, response)
	})
	if err != nil {
		t.Fatal(err)
	}
	if listed, err := repository.ListSteering(t.Context(), target); err != nil || len(listed) != 0 {
		t.Fatalf("after discard listed=%+v err=%v", listed, err)
	}
	var steeringGroups int
	err = repository.projects.WithinProjectTx(t.Context(), 1, steeringWriteTx, func(tx sqlExecutor) error {
		return tx.QueryRow(t.Context(), `
SELECT count(*) FROM chat_message_group WHERE meta -> 'steering' ->> 'response_message_id' = $1`, response).Scan(&steeringGroups)
	})
	if err != nil || steeringGroups != 0 {
		t.Fatalf("steering groups=%d err=%v", steeringGroups, err)
	}
}

func TestPostgresSteeringRefusesEndedTurnsAndPipelines(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	response := seedSteeringTurn(t, pool, "03", "execution-steer-ended", "RUNNING")
	target := steeringTarget(response)
	queued, _, err := repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
		SteeringTarget: target, IdempotencyKey: "ended-1", Content: "too late",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.shared.Exec(t.Context(), `
UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED', settled_at = clock_timestamp()
WHERE execution_id = 'execution-steer-ended'`); err != nil {
		t.Fatal(err)
	}
	err = repository.projects.WithinProjectTx(t.Context(), 1, steeringWriteTx, func(tx sqlExecutor) error {
		_, err := tx.Exec(t.Context(), `UPDATE chat_message_group SET is_streaming = FALSE WHERE uuid = $1::uuid`, response)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// The pending message of an ended turn reads turn_ended.
	listed, err := repository.ListSteering(t.Context(), target)
	if err != nil || len(listed) != 1 || listed[0].ID != queued.ID || listed[0].State != agentexecutionapp.SteeringTurnEnded {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	if _, _, err := repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
		SteeringTarget: target, IdempotencyKey: "ended-2", Content: "after the end",
	}); !errors.Is(err, agentexecutionapp.ErrSteeringTurnNotRunning) {
		t.Fatalf("ended err=%v", err)
	}
	// The client removes a turn_ended message once it sent it as a new turn.
	if _, err := repository.RemoveSteering(t.Context(), agentexecutionapp.SteeringRemoveRequest{
		SteeringTarget: target, SteeringID: queued.ID,
	}); err != nil {
		t.Fatal(err)
	}

	pipeline := seedSteeringTurn(t, pool, "04", "execution-steer-pipeline", "RUNNING")
	err = repository.projects.WithinProjectTx(t.Context(), 1, steeringWriteTx, func(tx sqlExecutor) error {
		_, err := tx.Exec(t.Context(), `UPDATE application_versions SET agent_type = 'pipeline' WHERE id = 41`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
		SteeringTarget: steeringTarget(pipeline), IdempotencyKey: "pipeline-1", Content: "steer a pipeline",
	}); !errors.Is(err, agentexecutionapp.ErrSteeringNotSupported) {
		t.Fatalf("pipeline err=%v", err)
	}
}

func TestPostgresSteeringClaimNeedsTheLiveClaimFence(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	const executionID = "execution-steer-claim"
	response := seedSteeringTurn(t, pool, "05", executionID, "RUNNING")
	if _, _, err := repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
		SteeringTarget: steeringTarget(response), IdempotencyKey: "claim-1", Content: "use the staging database",
	}); err != nil {
		t.Fatal(err)
	}
	fence := runtimedomain.Fence{
		CommandID: "command-" + executionID, ExecutionID: executionID, Generation: 1,
		WorkloadIdentity: "spiffe://elitea.test/runtime/rust-worker", WorkloadSessionID: "session-" + executionID,
		ProducerID: "rust-worker-1", ClaimAttempt: 1, LeaseEpoch: 1,
	}
	copy(fence.Token[:], strings.Repeat("f", 32))
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.workload_sessions (workload_session_id, workload_identity, producer_id, issued_at, expires_at)
VALUES ($1, $2, 'rust-worker-1', clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour')`,
		fence.WorkloadSessionID, fence.WorkloadIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.execution_claims (
    claim_id, execution_id, generation, workload_session_id, workload_identity, producer_id,
    claim_attempt, lease_epoch, fence_token, claimed_at, lease_expires_at
) VALUES ('claim-' || $3, $3, 1, $1, $2, 'rust-worker-1', 1, 1, $4, clock_timestamp(), clock_timestamp() + interval '10 minutes')`,
		fence.WorkloadSessionID, fence.WorkloadIdentity, executionID, fence.Token[:]); err != nil {
		t.Fatal(err)
	}

	stale := fence
	copy(stale.Token[:], strings.Repeat("e", 32))
	claim := func(f runtimedomain.Fence, acknowledged []string, only bool) ([]agentexecutionapp.ClaimedSteeringMessage, []agentexecutionapp.SteeringEffect, error) {
		return repository.ClaimSteering(t.Context(), agentexecutionapp.SteeringClaimRequest{
			Fence: f, Acknowledged: acknowledged, AcknowledgeOnly: only,
		})
	}
	if _, _, err := claim(stale, nil, false); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("stale err=%v", err)
	}
	claimed, effects, err := claim(fence, nil, false)
	if err != nil || len(claimed) != 1 || claimed[0].Content != "use the staging database" || len(effects) != 0 {
		t.Fatalf("claimed=%+v effects=%+v err=%v", claimed, effects, err)
	}
	// Claimed, not injected: the client still reads pending, but cannot edit.
	listed, err := repository.ListSteering(t.Context(), steeringTarget(response))
	if err != nil || len(listed) != 1 || listed[0].State != agentexecutionapp.SteeringPending {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	if _, _, err := repository.EditSteering(t.Context(), agentexecutionapp.SteeringEditRequest{
		SteeringTarget: steeringTarget(response), SteeringID: claimed[0].ID, Content: "late",
	}); !errors.Is(err, agentexecutionapp.ErrSteeringNotPending) {
		t.Fatalf("edit claimed err=%v", err)
	}
	// A lost claim answer loses nothing: the same epoch gets its
	// unacknowledged message again (the worker ignores ids it holds).
	if again, _, err := claim(fence, nil, false); err != nil || len(again) != 1 || again[0].ID != claimed[0].ID {
		t.Fatalf("second claim=%+v err=%v", again, err)
	}

	// The worker stops before its checkpoint is persisted. The replacement
	// claim (lease epoch 2) gets the message again.
	replacement := fence
	replacement.ClaimAttempt, replacement.LeaseEpoch = 2, 2
	copy(replacement.Token[:], strings.Repeat("g", 32))
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_runtime.execution_claims SET released_at = clock_timestamp() WHERE execution_id = $1`, executionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.execution_claims (
    claim_id, execution_id, generation, workload_session_id, workload_identity, producer_id,
    claim_attempt, lease_epoch, fence_token, claimed_at, lease_expires_at
) VALUES ('claim-2-' || $3, $3, 1, $1, $2, 'rust-worker-1', 2, 2, $4, clock_timestamp(), clock_timestamp() + interval '10 minutes')`,
		replacement.WorkloadSessionID, replacement.WorkloadIdentity, executionID, replacement.Token[:]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := claim(fence, nil, false); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("released claim err=%v", err)
	}
	reclaimed, _, err := claim(replacement, nil, false)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].ID != claimed[0].ID {
		t.Fatalf("reclaimed=%+v err=%v", reclaimed, err)
	}

	// Its checkpoint is persisted: the acknowledgement injects it once.
	_, effects, err = claim(replacement, []string{reclaimed[0].ID}, true)
	if err != nil || len(effects) != 1 || effects[0].Cursor == 0 {
		t.Fatalf("ack effects=%+v err=%v", effects, err)
	}
	if _, effects, err = claim(replacement, []string{reclaimed[0].ID}, true); err != nil || len(effects) != 0 {
		t.Fatalf("repeated ack effects=%+v err=%v", effects, err)
	}
	listed, err = repository.ListSteering(t.Context(), steeringTarget(response))
	if err != nil || len(listed) != 1 || listed[0].State != agentexecutionapp.SteeringInjected {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
}

// A replacement claim whose restored checkpoint holds the message
// acknowledges it in the same call: it is injected, never returned again.
func TestPostgresSteeringReplacementClaimAcknowledgesItsCheckpoint(t *testing.T) {
	repository, pool := newSteeringIntegrationRepository(t)
	const executionID = "execution-steer-recover"
	response := seedSteeringTurn(t, pool, "06", executionID, "RUNNING")
	queued, _, err := repository.QueueSteering(t.Context(), agentexecutionapp.SteeringQueueRequest{
		SteeringTarget: steeringTarget(response), IdempotencyKey: "recover-1", Content: "keep the old API",
	})
	if err != nil {
		t.Fatal(err)
	}
	// An earlier epoch claimed it, then stopped after its checkpoint.
	err = repository.projects.WithinProjectTx(t.Context(), 1, steeringWriteTx, func(tx sqlExecutor) error {
		_, err := claimWaitingSteering(t.Context(), tx, executionID, 1, 1)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fence := runtimedomain.Fence{
		CommandID: "command-" + executionID, ExecutionID: executionID, Generation: 1,
		WorkloadIdentity: "spiffe://elitea.test/runtime/rust-worker", WorkloadSessionID: "session-" + executionID,
		ProducerID: "rust-worker-1", ClaimAttempt: 2, LeaseEpoch: 2,
	}
	copy(fence.Token[:], strings.Repeat("h", 32))
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.workload_sessions (workload_session_id, workload_identity, producer_id, issued_at, expires_at)
VALUES ($1, $2, 'rust-worker-1', clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour')`,
		fence.WorkloadSessionID, fence.WorkloadIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.execution_claims (
    claim_id, execution_id, generation, workload_session_id, workload_identity, producer_id,
    claim_attempt, lease_epoch, fence_token, claimed_at, lease_expires_at
) VALUES ('claim-' || $3, $3, 1, $1, $2, 'rust-worker-1', 2, 2, $4, clock_timestamp(), clock_timestamp() + interval '10 minutes')`,
		fence.WorkloadSessionID, fence.WorkloadIdentity, executionID, fence.Token[:]); err != nil {
		t.Fatal(err)
	}
	claimed, effects, err := repository.ClaimSteering(t.Context(), agentexecutionapp.SteeringClaimRequest{
		Fence: fence, Acknowledged: []string{queued.ID},
	})
	if err != nil || len(claimed) != 0 || len(effects) != 1 {
		t.Fatalf("claimed=%+v effects=%+v err=%v", claimed, effects, err)
	}
}
