package repos

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SteeringRepository is the durable steering queue (shared/0178). The routes
// run in the project's transaction, because a steering message reads the
// conversation (tenant schema) and the execution (elitea_runtime) together.
type SteeringRepository struct {
	projects projectStore
	shared   sqlExecutor
}

func NewSteeringRepository(pool *pgxpool.Pool) (*SteeringRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newSteeringRepository(projects, pgxExecutor{queryer: pool})
}

func newSteeringRepository(projects projectStore, shared sqlExecutor) (*SteeringRepository, error) {
	if projects == nil || shared == nil {
		return nil, errors.New("steering project database is required")
	}
	return &SteeringRepository{projects: projects, shared: shared}, nil
}

var _ agentexecutionapp.SteeringStore = (*SteeringRepository)(nil)

var steeringWriteTx = pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}

// maxSteeringEventBytes keeps an event inside the replay row bound (64 KiB).
// JSON escaping can grow a 16 KiB text past it; such an event carries no
// content, and the client reads the list.
const maxSteeringEventBytes = 60 << 10

// steeringTargetSQL resolves the answer to its current execution, under a
// share lock of the job row: a settlement or a claim of the same execution
// waits, so "is running" stays true until this transaction commits. The
// caller must be a user participant of the conversation. `agent_type` names a
// pipeline turn; it is NULL for an ad hoc turn or an agent of another project.
const steeringTargetSQL = `
SELECT job.execution_id,
       job.generation,
       (job.desired_state = 'RUNNING'
        AND job.state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING')
        AND response.is_streaming) AS running,
       COALESCE(target_version.agent_type, '') AS agent_type
FROM chat_message_group AS response
JOIN chat_conversations AS conversation
  ON conversation.id = response.conversation_id
JOIN chat_message_group AS question
  ON question.id = response.reply_to_id
 AND question.conversation_id = conversation.id
JOIN elitea_runtime.agent_execution_jobs AS binding
  ON binding.client_message_id = response.uuid::text
 AND binding.execution_id = response.task_id
 AND binding.client_execution_generation = response.meta ->> 'execution_generation'
JOIN elitea_runtime.execution_jobs AS job
  ON job.execution_id = binding.execution_id
 AND job.generation = binding.generation
 AND job.capability_id = binding.capability_id
LEFT JOIN chat_participant_mapping AS target_mapping
  ON target_mapping.conversation_id = conversation.id
 AND target_mapping.participant_id = question.sent_to_id
LEFT JOIN application_versions AS target_version
  ON target_mapping.entity_settings ->> 'version_id' ~ '^[1-9][0-9]{0,8}$'
 AND target_version.id = (target_mapping.entity_settings ->> 'version_id')::integer
WHERE response.uuid = $1::uuid
  AND job.tenant_id = $2::bigint::text
  AND job.resource_project_id = $2::bigint
  AND job.projection_project_id = $2::bigint
  AND job.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1')
  AND EXISTS (
      SELECT 1
      FROM chat_participant_mapping AS caller_mapping
      JOIN chat_participants AS caller
        ON caller.id = caller_mapping.participant_id
       AND caller.entity_name = 'user'
       AND caller.entity_meta ->> 'id' = $3::bigint::text
      WHERE caller_mapping.conversation_id = conversation.id
  )
FOR SHARE OF job`

// steeringColumns is the projection every read returns. A pending or claimed
// row of an execution that no longer runs reads as turn_ended; a claimed row
// of a running one reads as pending (the client cannot see the claim).
const steeringColumns = `
       message.steering_id::text,
       message.response_message_id::text,
       message.author_user_id,
       message.content,
       CASE
           WHEN message.state IN ('pending', 'claimed')
            AND NOT (job.desired_state = 'RUNNING'
                     AND job.state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING'))
           THEN 'turn_ended'
           WHEN message.state = 'claimed' THEN 'pending'
           ELSE message.state
       END,
       message.revision,
       message.queue_order,
       message.created_at,
       message.updated_at,
       message.injected_at,
       COALESCE(message.message_id::text, '')`

const steeringFrom = `
FROM elitea_runtime.execution_steering_messages AS message
JOIN elitea_runtime.execution_jobs AS job
  ON job.execution_id = message.execution_id
 AND job.generation = message.generation`

type steeringRunningTarget struct {
	executionID string
	generation  int64
	running     bool
	agentType   string
}

func (r *SteeringRepository) lockTarget(ctx context.Context, tx sqlExecutor, target agentexecutionapp.SteeringTarget) (steeringRunningTarget, error) {
	var result steeringRunningTarget
	err := tx.QueryRow(ctx, steeringTargetSQL, target.ResponseMessageID, target.ProjectID, target.ActorUserID).
		Scan(&result.executionID, &result.generation, &result.running, &result.agentType)
	if errors.Is(err, pgx.ErrNoRows) {
		return steeringRunningTarget{}, agentexecutionapp.ErrSteeringTurnNotRunning
	}
	if err != nil {
		return steeringRunningTarget{}, fmt.Errorf("resolve steering target: %w", err)
	}
	return result, nil
}

func scanSteering(row sqlRow) (agentexecutionapp.SteeringMessage, error) {
	var message agentexecutionapp.SteeringMessage
	var state string
	var injectedAt *time.Time
	err := row.Scan(
		&message.ID, &message.ResponseMessageID, &message.AuthorUserID, &message.Content,
		&state, &message.Revision, &message.QueueOrder, &message.CreatedAt, &message.UpdatedAt, &injectedAt, &message.MessageID,
	)
	if err != nil {
		return agentexecutionapp.SteeringMessage{}, err
	}
	message.State = agentexecutionapp.SteeringState(state)
	message.CreatedAt = message.CreatedAt.UTC()
	message.UpdatedAt = message.UpdatedAt.UTC()
	if injectedAt != nil {
		utc := injectedAt.UTC()
		message.InjectedAt = &utc
	}
	return message, nil
}

func (r *SteeringRepository) QueueSteering(
	ctx context.Context,
	request agentexecutionapp.SteeringQueueRequest,
) (agentexecutionapp.SteeringMessage, []agentexecutionapp.SteeringEffect, error) {
	var message agentexecutionapp.SteeringMessage
	var effects []agentexecutionapp.SteeringEffect
	err := r.projects.WithinProjectTx(ctx, request.ProjectID, steeringWriteTx, func(tx sqlExecutor) error {
		// A retry with the same key answers the first message, in any state
		// and also after the turn ended.
		existing, err := scanSteering(tx.QueryRow(ctx, `SELECT`+steeringColumns+steeringFrom+`
WHERE message.project_id = $1 AND message.response_message_id = $2::uuid
  AND message.author_user_id = $3 AND message.idempotency_key = $4`,
			request.ProjectID, request.ResponseMessageID, request.ActorUserID, request.IdempotencyKey))
		if err == nil {
			if existing.Content != request.Content {
				return agentexecutionapp.ErrSteeringIdempotencyConflict
			}
			message = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read steering replay: %w", err)
		}
		target, err := r.lockTarget(ctx, tx, request.SteeringTarget)
		if err != nil {
			return err
		}
		if target.agentType == "pipeline" {
			return agentexecutionapp.ErrSteeringNotSupported
		}
		if !target.running {
			return agentexecutionapp.ErrSteeringTurnNotRunning
		}
		// Two queues of one execution count one after the other: the share
		// lock of the job row does not order them, this lock does.
		if err := lockSteeringQueue(ctx, tx, target.executionID, target.generation); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.execution_steering_messages
WHERE execution_id = $1 AND generation = $2 AND state IN ('pending', 'claimed')`,
			target.executionID, target.generation).Scan(&pending); err != nil {
			return fmt.Errorf("count pending steering: %w", err)
		}
		if pending >= agentexecutionapp.MaxPendingSteeringMessages {
			return agentexecutionapp.ErrSteeringQueueFull
		}
		message, err = scanSteering(tx.QueryRow(ctx, `
WITH message AS (
    INSERT INTO elitea_runtime.execution_steering_messages (
        steering_id, execution_id, generation, project_id, response_message_id,
        author_user_id, idempotency_key, content
    ) VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6, $7, $8)
    RETURNING *
)
SELECT`+steeringColumns+`
FROM message
JOIN elitea_runtime.execution_jobs AS job
  ON job.execution_id = message.execution_id
 AND job.generation = message.generation`,
			uuid.NewString(), target.executionID, target.generation, request.ProjectID,
			request.ResponseMessageID, request.ActorUserID, request.IdempotencyKey, request.Content))
		if err != nil {
			return fmt.Errorf("queue steering message: %w", err)
		}
		effect, err := appendSteeringEvent(ctx, tx, request.ProjectID, target.executionID, target.generation,
			agentexecutionapp.SteeringEventQueued, message, "")
		if err != nil {
			return err
		}
		effects = append(effects, effect)
		return nil
	})
	if err != nil {
		return agentexecutionapp.SteeringMessage{}, nil, err
	}
	return message, effects, nil
}

// lockSteeringQueue serializes the writers that count an execution's waiting
// messages (a queue, a carry-over), until the transaction ends.
func lockSteeringQueue(ctx context.Context, tx sqlExecutor, executionID string, generation int64) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('steering:' || $1::text || ':' || $2::bigint::text, 0))`,
		executionID, generation); err != nil {
		return fmt.Errorf("lock the steering queue: %w", err)
	}
	return nil
}

func (r *SteeringRepository) ListSteering(
	ctx context.Context,
	target agentexecutionapp.SteeringTarget,
) ([]agentexecutionapp.SteeringMessage, error) {
	var messages []agentexecutionapp.SteeringMessage
	readTx := pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly}
	err := r.projects.WithinProjectTx(ctx, target.ProjectID, readTx, func(tx sqlExecutor) error {
		// Only a participant of the answer's conversation reads its queue.
		// The cap keeps the waiting messages (pending, claimed) first and
		// then the newest final ones, so a long history never hides a
		// message that still waits; the answer is in queue order.
		rows, err := tx.Query(ctx, `SELECT`+steeringColumns+steeringFrom+`
JOIN chat_message_group AS response
  ON response.uuid = message.response_message_id
WHERE message.project_id = $1 AND message.response_message_id = $2::uuid
  AND EXISTS (
      SELECT 1
      FROM chat_participant_mapping AS caller_mapping
      JOIN chat_participants AS caller
        ON caller.id = caller_mapping.participant_id
       AND caller.entity_name = 'user'
       AND caller.entity_meta ->> 'id' = $3::bigint::text
      WHERE caller_mapping.conversation_id = response.conversation_id
  )
ORDER BY (message.state IN ('pending', 'claimed')) DESC, message.queue_order DESC
LIMIT 64`, target.ProjectID, target.ResponseMessageID, target.ActorUserID)
		if err != nil {
			return fmt.Errorf("list steering messages: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			message, scanErr := scanSteering(rows)
			if scanErr != nil {
				return fmt.Errorf("scan steering message: %w", scanErr)
			}
			messages = append(messages, message)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(messages, func(a, b agentexecutionapp.SteeringMessage) int { return cmp.Compare(a.QueueOrder, b.QueueOrder) })
	if messages == nil {
		messages = []agentexecutionapp.SteeringMessage{}
	}
	return messages, nil
}

// lockOwnSteering locks one message of its author, with its execution's state.
// A claimed message is in the worker's hands: it reads as pending but cannot
// be edited, and it can be removed only after its turn ended.
func lockOwnSteering(ctx context.Context, tx sqlExecutor, target agentexecutionapp.SteeringTarget, steeringID string) (agentexecutionapp.SteeringMessage, string, int64, error) {
	var executionID string
	var generation int64
	var stored string
	row := tx.QueryRow(ctx, `
SELECT message.execution_id, message.generation, message.state,`+steeringColumns+steeringFrom+`
WHERE message.steering_id = $1::uuid AND message.project_id = $2
  AND message.response_message_id = $3::uuid AND message.author_user_id = $4
FOR UPDATE OF message`, steeringID, target.ProjectID, target.ResponseMessageID, target.ActorUserID)
	var message agentexecutionapp.SteeringMessage
	var state string
	var injectedAt *time.Time
	err := row.Scan(&executionID, &generation, &stored,
		&message.ID, &message.ResponseMessageID, &message.AuthorUserID, &message.Content,
		&state, &message.Revision, &message.QueueOrder, &message.CreatedAt, &message.UpdatedAt, &injectedAt, &message.MessageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentexecutionapp.SteeringMessage{}, "", 0, agentexecutionapp.ErrSteeringNotFound
	}
	if err != nil {
		return agentexecutionapp.SteeringMessage{}, "", 0, fmt.Errorf("lock steering message: %w", err)
	}
	message.State = agentexecutionapp.SteeringState(state)
	if stored == "claimed" && message.State == agentexecutionapp.SteeringPending {
		message.State = steeringClaimed
	}
	return message, executionID, generation, nil
}

// steeringClaimed is the internal state of a claimed message of a running
// turn. It never leaves this file.
const steeringClaimed agentexecutionapp.SteeringState = "claimed"

func (r *SteeringRepository) EditSteering(
	ctx context.Context,
	request agentexecutionapp.SteeringEditRequest,
) (agentexecutionapp.SteeringMessage, []agentexecutionapp.SteeringEffect, error) {
	var message agentexecutionapp.SteeringMessage
	var effects []agentexecutionapp.SteeringEffect
	err := r.projects.WithinProjectTx(ctx, request.ProjectID, steeringWriteTx, func(tx sqlExecutor) error {
		current, executionID, generation, err := lockOwnSteering(ctx, tx, request.SteeringTarget, request.SteeringID)
		if err != nil {
			return err
		}
		if current.State != agentexecutionapp.SteeringPending {
			return agentexecutionapp.ErrSteeringNotPending
		}
		if current.Content == request.Content {
			message = current
			return nil
		}
		message, err = scanSteering(tx.QueryRow(ctx, `
WITH message AS (
    UPDATE elitea_runtime.execution_steering_messages
    SET content = $2, revision = revision + 1, updated_at = clock_timestamp()
    WHERE steering_id = $1::uuid AND state = 'pending'
    RETURNING *
)
SELECT`+steeringColumns+`
FROM message
JOIN elitea_runtime.execution_jobs AS job
  ON job.execution_id = message.execution_id
 AND job.generation = message.generation`, request.SteeringID, request.Content))
		if err != nil {
			return fmt.Errorf("edit steering message: %w", err)
		}
		effect, err := appendSteeringEvent(ctx, tx, request.ProjectID, executionID, generation,
			agentexecutionapp.SteeringEventQueued, message, "")
		if err != nil {
			return err
		}
		effects = append(effects, effect)
		return nil
	})
	if err != nil {
		return agentexecutionapp.SteeringMessage{}, nil, err
	}
	return message, effects, nil
}

func (r *SteeringRepository) RemoveSteering(
	ctx context.Context,
	request agentexecutionapp.SteeringRemoveRequest,
) ([]agentexecutionapp.SteeringEffect, error) {
	var effects []agentexecutionapp.SteeringEffect
	err := r.projects.WithinProjectTx(ctx, request.ProjectID, steeringWriteTx, func(tx sqlExecutor) error {
		current, executionID, generation, err := lockOwnSteering(ctx, tx, request.SteeringTarget, request.SteeringID)
		if err != nil {
			return err
		}
		switch current.State {
		case agentexecutionapp.SteeringRemoved:
			return nil
		case agentexecutionapp.SteeringPending, agentexecutionapp.SteeringTurnEnded:
		default:
			// Injected, claimed by a running turn, handed off: final or in
			// the worker's hands. No change, no event.
			return agentexecutionapp.ErrSteeringNotPending
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_steering_messages
SET state = 'removed', updated_at = clock_timestamp()
WHERE steering_id = $1::uuid AND state IN ('pending', 'claimed')`, request.SteeringID); err != nil {
			return fmt.Errorf("remove steering message: %w", err)
		}
		if current.State == agentexecutionapp.SteeringTurnEnded {
			// The turn's stream is over; nobody waits for the event.
			return nil
		}
		current.State = agentexecutionapp.SteeringRemoved
		current.UpdatedAt = time.Now().UTC()
		effect, err := appendSteeringEvent(ctx, tx, request.ProjectID, executionID, generation,
			agentexecutionapp.SteeringEventDropped, current, "removed")
		if err != nil {
			return err
		}
		effects = append(effects, effect)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return effects, nil
}

// steeringClaimAuthoritySQL locks the live claim of the execution: an
// ordinary claim (never node recovery) inside its lease, of an agent job that
// runs and has no terminal output. The control server has verified the same
// fence already; this lock keeps it true until the claim commits.
const steeringClaimAuthoritySQL = `
SELECT j.resource_project_id
FROM elitea_runtime.execution_jobs j
JOIN elitea_runtime.execution_claims c USING (execution_id, generation)
WHERE j.execution_id = $1 AND j.generation = $2
 AND c.workload_identity = $3 AND c.fence_token = $4
 AND c.released_at IS NULL AND c.lease_expires_at > clock_timestamp() AND c.recovery_mode = 'NONE'
 AND j.state = 'RUNNING' AND j.desired_state = 'RUNNING'
 AND j.tenant_id = j.resource_project_id::text AND j.projection_project_id = j.resource_project_id
 AND j.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1')
 AND NOT EXISTS (SELECT 1 FROM elitea_runtime.output_inbox terminal
                 WHERE terminal.execution_id = j.execution_id AND terminal.generation = j.generation)
FOR UPDATE OF j, c`

// ClaimSteering serves one worker call of a verified claim fence: first the
// acknowledged messages become conversation messages, then the messages that
// wait are claimed for this lease epoch.
func (r *SteeringRepository) ClaimSteering(
	ctx context.Context,
	request agentexecutionapp.SteeringClaimRequest,
) ([]agentexecutionapp.ClaimedSteeringMessage, []agentexecutionapp.SteeringEffect, error) {
	fence := request.Fence
	var projectID int64
	// The project selects the tenant schema; it is the job's, never the caller's.
	err := r.shared.QueryRow(ctx, `
SELECT resource_project_id FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = $2`, fence.ExecutionID, int64(fence.Generation)).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, runtimedomain.ErrStaleFence
	}
	if err != nil {
		return nil, nil, fmt.Errorf("resolve steering claim project: %w", err)
	}
	var claimed []agentexecutionapp.ClaimedSteeringMessage
	var effects []agentexecutionapp.SteeringEffect
	err = r.projects.WithinProjectTx(ctx, projectID, steeringWriteTx, func(tx sqlExecutor) error {
		var lockedProject int64
		// The first read takes the locks; the second re-checks the lease and
		// the terminal output with the locks held (a READ COMMITTED statement
		// that waited does not re-read its joined rows).
		for range 2 {
			err := tx.QueryRow(ctx, steeringClaimAuthoritySQL, fence.ExecutionID, int64(fence.Generation),
				fence.WorkloadIdentity, fence.Token[:]).Scan(&lockedProject)
			if errors.Is(err, pgx.ErrNoRows) {
				return runtimedomain.ErrStaleFence
			}
			if err != nil {
				return fmt.Errorf("lock steering claim: %w", err)
			}
		}
		if lockedProject != projectID {
			return runtimedomain.ErrStaleFence
		}
		var err error
		effects, err = injectAcknowledgedSteering(ctx, tx, projectID, fence.ExecutionID, int64(fence.Generation), request.Acknowledged)
		if err != nil || request.AcknowledgeOnly {
			return err
		}
		claimed, err = claimWaitingSteering(ctx, tx, fence.ExecutionID, int64(fence.Generation), int64(fence.LeaseEpoch))
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return claimed, effects, nil
}

type pendingSteering struct {
	id                string
	responseMessageID string
	authorUserID      int64
	content           string
	revision          int32
	queueOrder        int64
	createdAt         time.Time
}

// claimWaitingSteering claims, in queue order and inside the byte bound, the
// pending messages and the claimed messages that are not acknowledged yet,
// of this lease epoch or an earlier one. The re-claim is idempotent: when a
// claim's answer was lost, the next claim of the same epoch gets the same
// messages again, and the worker ignores the ids it already holds. A message
// of an earlier epoch (its worker stopped before the checkpoint was
// persisted) moves to this epoch.
func claimWaitingSteering(ctx context.Context, tx sqlExecutor, executionID string, generation, leaseEpoch int64) ([]agentexecutionapp.ClaimedSteeringMessage, error) {
	rows, err := tx.Query(ctx, `
SELECT steering_id::text, content, created_at
FROM elitea_runtime.execution_steering_messages
WHERE execution_id = $1 AND generation = $2
  AND (state = 'pending' OR (state = 'claimed' AND claim_lease_epoch <= $3))
ORDER BY queue_order
LIMIT $4
FOR UPDATE`, executionID, generation, leaseEpoch, agentexecutionapp.MaxClaimedSteeringMessages)
	if err != nil {
		return nil, fmt.Errorf("read waiting steering: %w", err)
	}
	var claimed []agentexecutionapp.ClaimedSteeringMessage
	claimedBytes := 0
	for rows.Next() {
		var message agentexecutionapp.ClaimedSteeringMessage
		if err := rows.Scan(&message.ID, &message.Content, &message.QueuedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan waiting steering: %w", err)
		}
		// One answer stays inside the worker's control response bound: the
		// rest wait for the next safe point, in order.
		if len(claimed) > 0 && claimedBytes+len(message.Content) > agentexecutionapp.MaxClaimedSteeringBytes {
			break
		}
		message.QueuedAt = message.QueuedAt.UTC()
		claimed = append(claimed, message)
		claimedBytes += len(message.Content)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read waiting steering: %w", err)
	}
	if len(claimed) == 0 {
		return claimed, nil
	}
	ids := make([]string, 0, len(claimed))
	for _, message := range claimed {
		ids = append(ids, message.ID)
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_steering_messages
SET state = 'claimed', claim_lease_epoch = $2, updated_at = clock_timestamp()
WHERE steering_id = ANY($1::uuid[])`, ids, leaseEpoch); err != nil {
		return nil, fmt.Errorf("claim steering messages: %w", err)
	}
	return claimed, nil
}

// injectAcknowledgedSteering writes each acknowledged claimed message of the
// execution as a user message of the conversation and marks it injected. Any
// lease epoch's claim counts: a replacement claim acknowledges what its
// restored checkpoint holds. An id that is not claimed is ignored.
//
// The message is placed directly after the turn's question (the same
// created_at, a larger id), so the transcript and the next turn's history
// read the question, the steering messages, then the answer that took them
// into account. The insert moves the conversation's activity time (tenant
// trigger chat_conversation_activity, 0145).
func injectAcknowledgedSteering(ctx context.Context, tx sqlExecutor, projectID int64, executionID string, generation int64, acknowledged []string) ([]agentexecutionapp.SteeringEffect, error) {
	if len(acknowledged) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
SELECT steering_id::text, response_message_id::text, author_user_id, content, revision, queue_order, created_at
FROM elitea_runtime.execution_steering_messages
WHERE execution_id = $1 AND generation = $2 AND state = 'claimed'
  AND steering_id = ANY($3::uuid[])
ORDER BY queue_order
FOR UPDATE`, executionID, generation, acknowledged)
	if err != nil {
		return nil, fmt.Errorf("read acknowledged steering: %w", err)
	}
	var pending []pendingSteering
	for rows.Next() {
		var item pendingSteering
		if err := rows.Scan(&item.id, &item.responseMessageID, &item.authorUserID, &item.content, &item.revision, &item.queueOrder, &item.createdAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan acknowledged steering: %w", err)
		}
		pending = append(pending, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read acknowledged steering: %w", err)
	}
	var effects []agentexecutionapp.SteeringEffect
	for _, item := range pending {
		effect, err := injectSteeringMessage(ctx, tx, projectID, executionID, generation, item)
		if err != nil {
			return nil, err
		}
		effects = append(effects, effect)
	}
	return effects, nil
}

func injectSteeringMessage(ctx context.Context, tx sqlExecutor, projectID int64, executionID string, generation int64, item pendingSteering) (agentexecutionapp.SteeringEffect, error) {
	messageID := uuid.NewString()
	var injectedAt time.Time
	err := tx.QueryRow(ctx, `
WITH turn AS MATERIALIZED (
    SELECT question.conversation_id, question.sent_to_id, question.created_at,
           (SELECT author.id
            FROM chat_participants AS author
            JOIN chat_participant_mapping AS author_mapping
              ON author_mapping.participant_id = author.id
             AND author_mapping.conversation_id = question.conversation_id
            WHERE author.entity_name = 'user'
              AND author.entity_meta ->> 'id' = $3::bigint::text
            ORDER BY author.id
            LIMIT 1) AS author_participant_id
    FROM chat_message_group AS response
    JOIN chat_message_group AS question ON question.id = response.reply_to_id
    WHERE response.uuid = $2::uuid AND response.task_id = $6
), steering_group AS (
    INSERT INTO chat_message_group (
        uuid, author_participant_id, conversation_id, sent_to_id, meta,
        is_streaming, created_at
    )
    SELECT $1::uuid, turn.author_participant_id, turn.conversation_id, turn.sent_to_id,
           jsonb_build_object('steering', jsonb_build_object(
               'steering_id', $4::text, 'response_message_id', $2::text)),
           FALSE, turn.created_at
    FROM turn
    WHERE turn.author_participant_id IS NOT NULL
    RETURNING id
), steering_item AS (
    INSERT INTO chat_message_items (uuid, item_type, order_index, meta, message_group_id, created_at)
    SELECT gen_random_uuid(), 'text_message', 0, '{}'::jsonb, steering_group.id, now() AT TIME ZONE 'UTC'
    FROM steering_group
    RETURNING id
), steering_text AS (
    INSERT INTO chat_messages_text (id, content)
    SELECT steering_item.id, $5::text FROM steering_item
    RETURNING id
)
UPDATE elitea_runtime.execution_steering_messages AS message
SET state = 'injected', message_id = $1::uuid, injected_at = clock_timestamp(),
    updated_at = clock_timestamp()
FROM steering_text
WHERE message.steering_id = $4::uuid AND message.state = 'claimed'
RETURNING message.injected_at`,
		messageID, item.responseMessageID, item.authorUserID, item.id, item.content, executionID).Scan(&injectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// The author is no longer in the conversation: the message cannot
		// become theirs, so it is removed and the stream says so.
		if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_steering_messages
SET state = 'removed', updated_at = clock_timestamp()
WHERE steering_id = $1::uuid AND state = 'claimed'`, item.id); err != nil {
			return agentexecutionapp.SteeringEffect{}, fmt.Errorf("drop steering message: %w", err)
		}
		return appendSteeringEvent(ctx, tx, projectID, executionID, generation,
			agentexecutionapp.SteeringEventDropped, agentexecutionapp.SteeringMessage{
				ID: item.id, ResponseMessageID: item.responseMessageID, AuthorUserID: item.authorUserID,
				Content: item.content, State: agentexecutionapp.SteeringRemoved, Revision: item.revision, QueueOrder: item.queueOrder,
				CreatedAt: item.createdAt.UTC(), UpdatedAt: time.Now().UTC(),
			}, "removed")
	}
	if err != nil {
		return agentexecutionapp.SteeringEffect{}, fmt.Errorf("inject steering message: %w", err)
	}
	injectedAt = injectedAt.UTC()
	return appendSteeringEvent(ctx, tx, projectID, executionID, generation,
		agentexecutionapp.SteeringEventInjected, agentexecutionapp.SteeringMessage{
			ID: item.id, ResponseMessageID: item.responseMessageID, AuthorUserID: item.authorUserID,
			Content: item.content, State: agentexecutionapp.SteeringInjected, Revision: item.revision, QueueOrder: item.queueOrder,
			CreatedAt: item.createdAt.UTC(), UpdatedAt: injectedAt, InjectedAt: &injectedAt, MessageID: messageID,
		}, "")
}

// DiscardTurnSteering deletes the steering of a turn that a stop deleted
// before any output (agent_cancel.go): the queue rows and the conversation
// messages of the injected ones, in the transaction that deletes the
// question and the answer. The turn never happened, so nothing of it stays.
//
// conversationID scopes the search to the turn's conversation (its index),
// so the steering meta is read only on that conversation's rows.
func DiscardTurnSteering(ctx context.Context, tx sqlExecutor, projectID int64, conversationID int64, responseMessageID string) error {
	const steeringGroups = `
    SELECT steering_group.id
    FROM chat_message_group AS steering_group
    WHERE steering_group.conversation_id = $1
      AND steering_group.reply_to_id IS NULL
      AND steering_group.meta -> 'steering' ->> 'response_message_id' = $2::text`
	if _, err := tx.Exec(ctx, `
DELETE FROM chat_message_items
WHERE message_group_id IN (`+steeringGroups+`)`, conversationID, responseMessageID); err != nil {
		return fmt.Errorf("discard injected steering items: %w", err)
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM chat_message_group
WHERE id IN (`+steeringGroups+`)`, conversationID, responseMessageID); err != nil {
		return fmt.Errorf("discard injected steering: %w", err)
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM elitea_runtime.execution_steering_messages
WHERE project_id = $1 AND response_message_id = $2::uuid`, projectID, responseMessageID); err != nil {
		return fmt.Errorf("discard steering queue: %w", err)
	}
	return nil
}

// SteeringEventData is the `data` of a steering event: the API's
// SteeringMessage, plus `reason` on steer.dropped.
func SteeringEventData(message agentexecutionapp.SteeringMessage, reason string) ([]byte, error) {
	view := map[string]any{
		"id":                  message.ID,
		"response_message_id": message.ResponseMessageID,
		"author_id":           message.AuthorUserID,
		"content":             message.Content,
		"state":               string(message.State),
		"revision":            message.Revision,
		"queue_order":         message.QueueOrder,
		"created_at":          message.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":          message.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"injected_at":         nil,
		"message_id":          nil,
	}
	if message.InjectedAt != nil {
		view["injected_at"] = message.InjectedAt.UTC().Format(time.RFC3339Nano)
	}
	if message.MessageID != "" {
		view["message_id"] = message.MessageID
	}
	if reason != "" {
		view["reason"] = reason
	}
	data, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	if len(data) > maxSteeringEventBytes {
		delete(view, "content")
		return json.Marshal(view)
	}
	return data, nil
}

func appendSteeringEvent(
	ctx context.Context,
	tx sqlExecutor,
	projectID int64,
	executionID string,
	generation int64,
	eventType string,
	message agentexecutionapp.SteeringMessage,
	reason string,
) (agentexecutionapp.SteeringEffect, error) {
	data, err := SteeringEventData(message, reason)
	if err != nil {
		return agentexecutionapp.SteeringEffect{}, fmt.Errorf("encode steering event: %w", err)
	}
	digest := runtimedomain.SHA256(data)
	eventID := fmt.Sprintf("steer:%s:%s:%d", message.ID, eventType, message.Revision)
	var cursor int64
	err = tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.execution_replay_events (
    event_id, execution_id, generation, projection_project_id,
    event_type, event_bytes, event_digest
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (event_id) DO NOTHING
RETURNING cursor`,
		eventID, executionID, generation, projectID, eventType, data, digest[:]).Scan(&cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentexecutionapp.SteeringEffect{}, nil
	}
	if err != nil {
		return agentexecutionapp.SteeringEffect{}, fmt.Errorf("append steering event: %w", err)
	}
	return agentexecutionapp.SteeringEffect{ProjectID: projectID, ExecutionID: executionID, Cursor: uint64(cursor)}, nil
}
