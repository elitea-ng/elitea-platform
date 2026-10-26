package repos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
)

// Wake admission (fanout-interrupt-decisions v1.1 §8.1). A QUEUED
// execution_interrupt_wakes row is the durable intent; the admitter
// (agentexecution.FanoutWakeAdmitter) claims it here and admits the job
// through the ordinary agent admission, whose transaction moves the row to
// ADMITTED together with the job (admitCurrentFanoutWake).

type claimedWake struct {
	responseID       string
	decisionRevision int64
	projectID        int64
	parkedExecution  string
	parkedGeneration int64
	attempts         int
}

// ClaimFanoutWakes claims at most limit QUEUED wakes, oldest first. A row
// another admitter holds is skipped (FOR UPDATE SKIP LOCKED), and a claimed
// row is skipped by everyone for lease on the database clock (its attempt
// counter and updated_at move in the claiming statement), so concurrent
// sweepers do not both work one row. A row whose attempts exceed maxAttempts
// ends FAILED (admission_exhausted). For the rest, the parked execution's
// actor is rechecked: still a member with models.chat.messages.create and
// still the conversation's or question's author and a participant. A refused
// actor ends the row FAILED (actor_not_allowed) and creates nothing.
func (r *ExecutionInterruptRepository) ClaimFanoutWakes(ctx context.Context, limit int, lease time.Duration, maxAttempts int) ([]agentexecutionapp.FanoutWakeTarget, error) {
	if r == nil || r.shared == nil || r.projects == nil || limit < 1 || limit > agentexecutionapp.FanoutWakeSweepBatch ||
		lease <= 0 || maxAttempts < 1 || maxAttempts > 1000 {
		return nil, errors.New("fanout wake claim is invalid")
	}
	var claimed []claimedWake
	err := r.shared.WithinTx(ctx, interruptWriteTx, func(tx sqlExecutor) error {
		claimed = claimed[:0]
		rows, err := tx.Query(ctx, `
WITH candidates AS (
  SELECT root_response_id, decision_revision
  FROM elitea_runtime.execution_interrupt_wakes
  WHERE state = 'QUEUED'
    AND (admission_attempts = 0 OR updated_at <= clock_timestamp() - make_interval(secs => $2::double precision))
  ORDER BY created_at, root_response_id, decision_revision
  LIMIT $1
  FOR UPDATE SKIP LOCKED
)
UPDATE elitea_runtime.execution_interrupt_wakes wake
SET admission_attempts = LEAST(wake.admission_attempts + 1, 1000), updated_at = clock_timestamp()
FROM candidates
WHERE wake.root_response_id = candidates.root_response_id AND wake.decision_revision = candidates.decision_revision
RETURNING wake.root_response_id::text, wake.decision_revision, wake.project_id, wake.parked_execution_id,
          wake.parked_generation, wake.admission_attempts`, limit, lease.Seconds())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var wake claimedWake
			if err := rows.Scan(&wake.responseID, &wake.decisionRevision, &wake.projectID, &wake.parkedExecution,
				&wake.parkedGeneration, &wake.attempts); err != nil {
				return err
			}
			claimed = append(claimed, wake)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("claim fanout wakes: %w", err)
	}
	targets := make([]agentexecutionapp.FanoutWakeTarget, 0, len(claimed))
	for _, wake := range claimed {
		turn := agentexecutionapp.CurrentFanoutWakeTurn{
			ProjectID: wake.projectID, ResponseMessageID: wake.responseID, DecisionRevision: wake.decisionRevision,
			ParkedExecutionID: wake.parkedExecution, ParkedGeneration: wake.parkedGeneration,
		}
		refused := ""
		var target agentexecutionapp.FanoutWakeTarget
		if wake.attempts > maxAttempts {
			refused = agentexecutionapp.FanoutWakeFailureExhausted
		} else if target, refused, err = r.loadFanoutWakeTarget(ctx, wake); err != nil {
			return nil, err
		}
		if refused != "" {
			if err := r.FailFanoutWake(ctx, turn, refused); err != nil {
				return nil, err
			}
			// Fixed code only: never the response, decision or actor.
			slog.Info("fanout wake ended without a continuation", "failure_code", refused)
			continue
		}
		targets = append(targets, target)
	}
	return targets, nil
}

// loadFanoutWakeTarget reads the parked execution's job, binding, exact
// request entry and response in the wake's project. It returns a failure
// code instead of a target when the wake can never run.
func (r *ExecutionInterruptRepository) loadFanoutWakeTarget(ctx context.Context, wake claimedWake) (agentexecutionapp.FanoutWakeTarget, string, error) {
	var target agentexecutionapp.FanoutWakeTarget
	refused := ""
	err := r.projects.WithinProjectTx(ctx, wake.projectID, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly}, func(tx sqlExecutor) error {
		target, refused = agentexecutionapp.FanoutWakeTarget{}, ""
		var triggerOrigin string
		var resourceProject, projectionProject int64
		err := tx.QueryRow(ctx, `
SELECT job.tenant_id, job.resource_project_id, job.projection_project_id, job.actor_id, job.capability_id,
       job.trigger_origin, binding.client_stream_id, binding.client_execution_generation,
       COALESCE(response.meta ->> 'thread_id', ''), entry.content_bytes, entry.content_digest
FROM elitea_runtime.execution_jobs job
JOIN elitea_runtime.agent_execution_jobs binding
  ON binding.execution_id = job.execution_id AND binding.generation = job.generation
 AND binding.input_bundle_id = job.input_bundle_id
JOIN elitea_runtime.input_bundle_entries entry
  ON entry.input_bundle_id = job.input_bundle_id AND entry.entry_id = binding.request_entry_id
 AND entry.semantic_role = 'agent.execution_request'
JOIN chat_message_group response ON response.uuid::text = binding.client_message_id
JOIN chat_conversations conversation
  ON conversation.id = response.conversation_id AND conversation.uuid::text = binding.client_stream_id
WHERE job.execution_id = $1 AND job.generation = $2
  AND job.resource_project_id = $3 AND job.projection_project_id = $3
  AND binding.client_message_id = $4`,
			wake.parkedExecution, wake.parkedGeneration, wake.projectID, wake.responseID).Scan(
			&target.TenantID, &resourceProject, &projectionProject, &target.ActorID, &target.CapabilityID,
			&triggerOrigin, &target.Turn.ConversationUUID, &target.Turn.ExecutionGeneration,
			&target.Turn.ThreadID, &target.FrozenInput, &target.FrozenInputDigest)
		if errors.Is(err, pgx.ErrNoRows) {
			refused = agentexecutionapp.FanoutWakeFailureResponseNotParked
			return nil
		}
		if err != nil {
			return err
		}
		actorID, parseErr := strconv.ParseInt(target.ActorID, 10, 64)
		if parseErr != nil || actorID <= 0 || actorID > math.MaxInt32 ||
			(target.CapabilityID != executiondomain.AgentApplicationCapability && target.CapabilityID != executiondomain.AgentAdhocCapability) {
			refused = agentexecutionapp.FanoutWakeFailureAdmission
			return nil
		}
		target.ResourceProjectID = strconv.FormatInt(resourceProject, 10)
		target.ProjectionProjectID = strconv.FormatInt(projectionProject, 10)
		target.TriggerOrigin = executiondomain.TriggerOrigin(triggerOrigin)
		target.Turn.ProjectID, target.Turn.ActorUserID = wake.projectID, actorID
		target.Turn.ResponseMessageID, target.Turn.DecisionRevision = wake.responseID, wake.decisionRevision
		target.Turn.ParkedExecutionID, target.Turn.ParkedGeneration = wake.parkedExecution, wake.parkedGeneration
		if fanoutWakeActorRefused(ctx, tx, r.permissions, target.Turn, &err) {
			refused = agentexecutionapp.FanoutWakeFailureActorNotAllowed
			return nil
		}
		return err
	})
	if err != nil {
		return agentexecutionapp.FanoutWakeTarget{}, "", fmt.Errorf("load fanout wake target: %w", err)
	}
	return target, refused, nil
}

// fanoutWakeActorRefused rechecks the parked actor on tx: project membership
// with the decision permission, then response ownership (the Decide rule).
// It reports a refusal; any other failure is stored in *failure.
func fanoutWakeActorRefused(ctx context.Context, tx sqlExecutor, permissions interruptPermissionCheck, turn agentexecutionapp.CurrentFanoutWakeTurn, failure *error) bool {
	if err := permissions(ctx, tx, turn.ProjectID, turn.ActorUserID, domain.PermissionDecide); err != nil {
		if errors.Is(err, domain.ErrNotAllowed) {
			return true
		}
		*failure = err
		return false
	}
	selector := domain.Selector{ProjectID: turn.ProjectID, ActorUserID: turn.ActorUserID, ResponseMessageID: turn.ResponseMessageID}
	if _, err := ownedResponse(ctx, tx, ownedResponseSQL, selector); err != nil {
		if errors.Is(err, domain.ErrNotAllowed) {
			return true
		}
		*failure = err
		return false
	}
	*failure = nil
	return false
}

// FailFanoutWake ends a QUEUED wake with a typed reason. The cards stay
// DECIDED and visible; nothing is admitted for this wake.
func (r *ExecutionInterruptRepository) FailFanoutWake(ctx context.Context, turn agentexecutionapp.CurrentFanoutWakeTurn, failureCode string) error {
	if r == nil || r.shared == nil || !domain.ValidResponseMessageID(turn.ResponseMessageID) || turn.DecisionRevision < 1 {
		return errors.New("fanout wake failure is invalid")
	}
	if _, err := r.shared.Exec(ctx, `
UPDATE elitea_runtime.execution_interrupt_wakes
SET state = 'FAILED', failure_code = $3, updated_at = clock_timestamp()
WHERE root_response_id = $1::uuid AND decision_revision = $2 AND state = 'QUEUED'`,
		turn.ResponseMessageID, turn.DecisionRevision, failureCode); err != nil {
		return fmt.Errorf("fail fanout wake: %w", err)
	}
	return nil
}

var _ agentexecutionapp.FanoutWakeStore = (*ExecutionInterruptRepository)(nil)

// admitCurrentFanoutWake runs inside the agent admission transaction, after
// the conversation lock, and before the job's command outbox row. Lock order
// is the ledger's: the chat_message_group response row, then the
// execution_interrupt_responses row, then the wake row. It rechecks that the
// wake is still QUEUED for the parked execution, that the actor may still
// continue the response, and that the response is still parked; then it
// resumes the response for the new execution and moves the wake to ADMITTED
// with the job's id, so the job and ADMITTED commit together or not at all.
// The returned notice is the agent_fanout_wake_admitted project-plane event
// for the conversation's readers, owed after commit.
func admitCurrentFanoutWake(ctx context.Context, tx sqlExecutor, executionID string, generation uint64, turn agentexecutionapp.CurrentFanoutWakeTurn) (resolvedNotice, error) {
	notice, err := admitCurrentFanoutWakeRows(ctx, tx, executionID, turn)
	if err != nil {
		return resolvedNotice{}, err
	}
	notice.live = []liveEvent{{eventType: LiveFanoutWakeAdmittedEventType, data: map[string]any{
		"conversation_id": turn.ConversationUUID, "root_response_id": turn.ResponseMessageID,
		"execution_id": executionID, "generation": generation,
	}}}
	return notice, nil
}

func admitCurrentFanoutWakeRows(ctx context.Context, tx sqlExecutor, executionID string, turn agentexecutionapp.CurrentFanoutWakeTurn) (resolvedNotice, error) {
	var responseGroupID int64
	err := tx.QueryRow(ctx, `
SELECT response.id
FROM chat_message_group response
JOIN chat_conversations conversation ON conversation.id = response.conversation_id
WHERE response.uuid = $1::uuid AND conversation.uuid = $2::uuid
FOR UPDATE OF response`, turn.ResponseMessageID, turn.ConversationUUID).Scan(&responseGroupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return resolvedNotice{}, agentexecutionapp.ErrFanoutWakeTargetGone
	}
	if err != nil {
		return resolvedNotice{}, fmt.Errorf("lock fanout wake response: %w", err)
	}
	var held int
	err = tx.QueryRow(ctx, `SELECT 1 FROM elitea_runtime.execution_interrupt_responses
WHERE root_response_id = $1::uuid AND project_id = $2 FOR UPDATE`, turn.ResponseMessageID, turn.ProjectID).Scan(&held)
	if errors.Is(err, pgx.ErrNoRows) {
		return resolvedNotice{}, agentexecutionapp.ErrFanoutWakeTargetGone
	}
	if err != nil {
		return resolvedNotice{}, fmt.Errorf("lock fanout wake ledger response: %w", err)
	}
	var state, parkedExecution string
	var parkedGeneration int64
	err = tx.QueryRow(ctx, `
SELECT state, parked_execution_id, parked_generation
FROM elitea_runtime.execution_interrupt_wakes
WHERE root_response_id = $1::uuid AND decision_revision = $2 AND project_id = $3
FOR UPDATE`, turn.ResponseMessageID, turn.DecisionRevision, turn.ProjectID).Scan(&state, &parkedExecution, &parkedGeneration)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && state != "QUEUED") {
		return resolvedNotice{}, agentexecutionapp.ErrFanoutWakeNotQueued
	}
	if err != nil {
		return resolvedNotice{}, fmt.Errorf("lock fanout wake: %w", err)
	}
	if parkedExecution != turn.ParkedExecutionID || parkedGeneration != turn.ParkedGeneration {
		return resolvedNotice{}, agentexecutionapp.ErrFanoutWakeTargetGone
	}
	// Authorization inside the effect transaction, after the locks: a
	// revocation that committed while this waited is seen.
	var failure error
	if fanoutWakeActorRefused(ctx, tx, checkInterruptPermission, turn, &failure) {
		return resolvedNotice{}, agentexecutionapp.ErrFanoutWakeActorNotAllowed
	}
	if failure != nil {
		return resolvedNotice{}, fmt.Errorf("recheck fanout wake actor: %w", failure)
	}
	tag, err := tx.Exec(ctx, `
UPDATE chat_message_group
SET meta = meta - 'fanout_parked_v1', is_streaming = TRUE, task_id = $2,
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
WHERE id = $1 AND NOT is_streaming AND task_id = $3
  AND jsonb_typeof(meta -> 'fanout_parked_v1') = 'object'
  AND meta ->> 'execution_generation' = $4 AND meta ->> 'thread_id' = $5`,
		responseGroupID, executionID, turn.ParkedExecutionID, turn.ExecutionGeneration, turn.ThreadID)
	if err != nil {
		return resolvedNotice{}, fmt.Errorf("resume fanout wake response: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return resolvedNotice{}, agentexecutionapp.ErrFanoutWakeTargetGone
	}
	tag, err = tx.Exec(ctx, `
UPDATE elitea_runtime.execution_interrupt_wakes
SET state = 'ADMITTED', wake_execution_id = $3, updated_at = clock_timestamp()
WHERE root_response_id = $1::uuid AND decision_revision = $2 AND state = 'QUEUED'`,
		turn.ResponseMessageID, turn.DecisionRevision, executionID)
	if err != nil {
		return resolvedNotice{}, fmt.Errorf("admit fanout wake: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return resolvedNotice{}, agentexecutionapp.ErrFanoutWakeNotQueued
	}
	notice := resolvedNotice{projectID: turn.ProjectID, conversationID: turn.ConversationUUID}
	if err := tx.QueryRow(ctx, `SELECT `+conversationReadersSQL("$1::text"), turn.ConversationUUID).Scan(&notice.audience); err != nil {
		return resolvedNotice{}, fmt.Errorf("load fanout wake readers: %w", err)
	}
	return notice, nil
}

// retireSettledWakes moves ADMITTED wakes of the response whose execution
// already reached a terminal job state to DONE, so the "one active
// continuation" index only counts continuations that can still run. The
// caller holds the response's execution_interrupt_responses row lock.
func retireSettledWakes(ctx context.Context, tx sqlExecutor, rootResponseID string) error {
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_interrupt_wakes wake
SET state = 'DONE', updated_at = clock_timestamp()
WHERE wake.root_response_id = $1::uuid AND wake.state = 'ADMITTED'
  AND EXISTS (SELECT 1 FROM elitea_runtime.execution_jobs job
              WHERE job.execution_id = wake.wake_execution_id
                AND job.state IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'QUARANTINED'))`, rootResponseID); err != nil {
		return fmt.Errorf("retire settled fanout wakes: %w", err)
	}
	return nil
}

// newTurnSupersedeBatch bounds one supersede statement of a new turn.
const newTurnSupersedeBatch = 16

// supersedeEarlierTurnInterrupts runs in the new turn's admission
// transaction, after the conversation lock: every open card (PENDING or
// DECIDED) of an earlier response of the conversation becomes SUPERSEDED,
// its queued wake FAILED (response_superseded), and agent_hitl_resolved is
// emitted for each (contract §5 "new turn"). Responses are locked in id
// order before their ledger rows (the ledger lock order). Each batch closes
// what it found, so the loop ends after at most ceil(open/16) batches.
func supersedeEarlierTurnInterrupts(ctx context.Context, tx sqlExecutor, projectID int64, conversationUUID, newResponseID string, actorUserID int64) ([]resolvedNotice, error) {
	var conversationID int64
	if err := tx.QueryRow(ctx, `SELECT id FROM chat_conversations WHERE uuid = $1::uuid`, conversationUUID).Scan(&conversationID); err != nil {
		return nil, fmt.Errorf("load new turn conversation: %w", err)
	}
	var notices []resolvedNotice
	for {
		// Driven by the open-card index (project_id, conversation_id), so a
		// long conversation costs only its open cards.
		rows, err := tx.Query(ctx, `
SELECT response.uuid::text
FROM chat_message_group response
WHERE response.conversation_id = $1 AND response.uuid <> $2::uuid
  AND response.uuid IN (
    SELECT card.root_response_id FROM elitea_runtime.execution_interrupts card
    WHERE card.project_id = $3 AND card.conversation_id = $1 AND card.state IN ('PENDING', 'DECIDED'))
ORDER BY response.id
LIMIT $4
FOR UPDATE OF response`, conversationID, newResponseID, projectID, newTurnSupersedeBatch)
		if err != nil {
			return nil, fmt.Errorf("find earlier open interrupt cards: %w", err)
		}
		var responses []string
		for rows.Next() {
			var response string
			if err := rows.Scan(&response); err != nil {
				rows.Close()
				return nil, err
			}
			responses = append(responses, response)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("find earlier open interrupt cards: %w", err)
		}
		for _, response := range responses {
			_, notice, err := closeOpenExecutionInterrupts(ctx, tx, response, domain.StateSuperseded, domain.Closer{ActorUserID: actorUserID})
			if err != nil {
				return nil, fmt.Errorf("supersede earlier interrupt cards: %w", err)
			}
			notices = append(notices, notice)
		}
		if len(responses) < newTurnSupersedeBatch {
			return notices, nil
		}
	}
}
