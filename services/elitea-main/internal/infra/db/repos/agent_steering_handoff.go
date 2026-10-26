package repos

import (
	"context"
	"errors"
	"fmt"
	"time"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The hand-off of turn_ended steering messages (shared/0178 HAND-OFF). A
// client and the server routine take rows with the same guarded update: only
// rows still `pending` or `claimed` of an execution that no longer runs move
// to `handed_off`. The first transaction that updates a row wins; the other
// one finds no row and starts nothing.

var _ agentexecutionapp.SteeringHandOffStore = (*SteeringRepository)(nil)

// steeringEndedSQL is "the row's execution no longer runs" (see steeringColumns).
const steeringEndedSQL = `NOT (job.desired_state = 'RUNNING'
     AND job.state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING'))`

// handOffRows reads the rows of one hand-off in queue order.
func handOffRows(ctx context.Context, tx sqlExecutor, projectID int64, responseMessageID string, authorUserID int64, key string) ([]agentexecutionapp.SteeringMessage, error) {
	rows, err := tx.Query(ctx, `SELECT`+steeringColumns+steeringFrom+`
WHERE message.project_id = $1 AND message.response_message_id = $2::uuid
  AND message.author_user_id = $3 AND message.handoff_key = $4
  AND message.state = 'handed_off'
ORDER BY message.queue_order`, projectID, responseMessageID, authorUserID, key)
	if err != nil {
		return nil, fmt.Errorf("read steering hand-off: %w", err)
	}
	defer rows.Close()
	var messages []agentexecutionapp.SteeringMessage
	for rows.Next() {
		message, scanErr := scanSteering(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan steering hand-off: %w", scanErr)
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

// takeTurnEnded moves the author's waiting rows of an ended answer to
// handed_off and answers how many it took.
func takeTurnEnded(ctx context.Context, tx sqlExecutor, projectID int64, responseMessageID string, authorUserID int64, key, by string, questionID *string) (int64, error) {
	// The rows are locked in queue order first, so two concurrent takes wait
	// on the same first row and the second one finds every row taken: a
	// group is never split between a client and the server.
	tag, err := tx.Exec(ctx, `
WITH waiting AS (
    SELECT message.steering_id
    FROM elitea_runtime.execution_steering_messages AS message
    JOIN elitea_runtime.execution_jobs AS job
      ON job.execution_id = message.execution_id
     AND job.generation = message.generation
    WHERE message.project_id = $1
      AND message.response_message_id = $2::uuid
      AND message.author_user_id = $3
      AND message.state IN ('pending', 'claimed')
      AND `+steeringEndedSQL+`
    ORDER BY message.queue_order
    FOR UPDATE OF message
)
UPDATE elitea_runtime.execution_steering_messages AS message
SET state = 'handed_off',
    handoff_key = $4,
    handoff_by = $5,
    handoff_question_id = $6::uuid,
    handed_off_at = clock_timestamp(),
    updated_at = clock_timestamp(),
    claim_lease_epoch = NULL
FROM waiting
WHERE message.steering_id = waiting.steering_id
  AND message.state IN ('pending', 'claimed')`, projectID, responseMessageID, authorUserID, key, by, questionID)
	if err != nil {
		return 0, fmt.Errorf("hand off steering: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (r *SteeringRepository) HandOffSteering(
	ctx context.Context,
	request agentexecutionapp.SteeringHandOffRequest,
) ([]agentexecutionapp.SteeringMessage, error) {
	var messages []agentexecutionapp.SteeringMessage
	err := r.projects.WithinProjectTx(ctx, request.ProjectID, steeringWriteTx, func(tx sqlExecutor) error {
		replay, err := handOffRows(ctx, tx, request.ProjectID, request.ResponseMessageID, request.ActorUserID, request.IdempotencyKey)
		if err != nil {
			return err
		}
		if len(replay) > 0 {
			messages = replay
			return nil
		}
		// The caller must be a participant; the share lock orders this take
		// after a settlement of the same execution.
		target, err := r.lockTarget(ctx, tx, request.SteeringTarget)
		if errors.Is(err, agentexecutionapp.ErrSteeringTurnNotRunning) {
			// No answer of a conversation the caller is in: nothing to take.
			return nil
		}
		if err != nil {
			return err
		}
		if target.running {
			return nil
		}
		taken, err := takeTurnEnded(ctx, tx, request.ProjectID, request.ResponseMessageID, request.ActorUserID,
			request.IdempotencyKey, "client", nil)
		if err != nil || taken == 0 {
			return err
		}
		messages, err = handOffRows(ctx, tx, request.ProjectID, request.ResponseMessageID, request.ActorUserID, request.IdempotencyKey)
		return err
	})
	if err != nil {
		return nil, err
	}
	return messages, nil
}

type handOffCandidate struct {
	projectID         int64
	responseMessageID string
	authorUserID      int64
	// key is set for a server hand-off whose start was not confirmed.
	key string
}

func (r *SteeringRepository) TakeServerHandOffs(ctx context.Context, readyBefore, retryBefore time.Duration, limit int) ([]agentexecutionapp.ServerHandOff, []agentexecutionapp.SteeringEffect, error) {
	if limit <= 0 {
		return nil, nil, nil
	}
	// A server hand-off that did not start within the give-up window (the
	// start keeps failing) is removed: the author sees it in the list as gone.
	if _, err := r.shared.Exec(ctx, `
UPDATE elitea_runtime.execution_steering_messages
SET state = 'removed', handoff_key = NULL, handoff_by = NULL, handoff_question_id = NULL,
    handed_off_at = NULL, updated_at = clock_timestamp()
WHERE state = 'handed_off' AND handoff_by = 'server' AND handoff_started_at IS NULL
  AND handed_off_at < clock_timestamp() - make_interval(secs => $1)`,
		agentexecutionapp.ServerHandOffGiveUpAfter.Seconds()); err != nil {
		return nil, nil, fmt.Errorf("expire steering hand-offs: %w", err)
	}
	rows, err := r.shared.Query(ctx, `
(SELECT DISTINCT message.project_id, message.response_message_id::text, message.author_user_id, ''
 FROM elitea_runtime.execution_steering_messages AS message
 JOIN elitea_runtime.execution_jobs AS job
   ON job.execution_id = message.execution_id
  AND job.generation = message.generation
 WHERE message.state IN ('pending', 'claimed')
   AND `+steeringEndedSQL+`
   AND job.settled_at IS NOT NULL
   AND job.settled_at < clock_timestamp() - make_interval(secs => $1)
 LIMIT $3)
UNION ALL
(SELECT DISTINCT message.project_id, message.response_message_id::text, message.author_user_id, message.handoff_key
 FROM elitea_runtime.execution_steering_messages AS message
 WHERE message.state = 'handed_off'
   AND message.handoff_by = 'server'
   AND message.handoff_started_at IS NULL
   AND message.handed_off_at < clock_timestamp() - make_interval(secs => $2)
 LIMIT $3)`, readyBefore.Seconds(), retryBefore.Seconds(), limit)
	if err != nil {
		return nil, nil, fmt.Errorf("find steering hand-offs: %w", err)
	}
	var candidates []handOffCandidate
	for rows.Next() {
		var candidate handOffCandidate
		if err := rows.Scan(&candidate.projectID, &candidate.responseMessageID, &candidate.authorUserID, &candidate.key); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan steering hand-off: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("find steering hand-offs: %w", err)
	}
	var handOffs []agentexecutionapp.ServerHandOff
	var effects []agentexecutionapp.SteeringEffect
	for _, candidate := range candidates {
		if len(handOffs) >= limit {
			break
		}
		handOff, ok, carried, err := r.takeServerHandOff(ctx, candidate)
		effects = append(effects, carried...)
		if err != nil {
			return handOffs, effects, err
		}
		if ok {
			handOffs = append(handOffs, handOff)
		}
	}
	return handOffs, effects, nil
}

// takeServerHandOff takes one group (or reads a group it took before) with the
// context of the next turn: the answer's conversation and participant.
func (r *SteeringRepository) takeServerHandOff(ctx context.Context, candidate handOffCandidate) (agentexecutionapp.ServerHandOff, bool, []agentexecutionapp.SteeringEffect, error) {
	handOff := agentexecutionapp.ServerHandOff{
		ProjectID: candidate.projectID, ResponseMessageID: candidate.responseMessageID, AuthorUserID: candidate.authorUserID,
	}
	ok := false
	var effects []agentexecutionapp.SteeringEffect
	err := r.projects.WithinProjectTx(ctx, candidate.projectID, steeringWriteTx, func(tx sqlExecutor) error {
		key := candidate.key
		if key == "" {
			// The author sent a newer message: the messages go to that
			// turn, and this answer starts nothing.
			carried, handled, err := carryTurnEnded(ctx, tx, candidate)
			if err != nil || handled {
				effects = carried
				return err
			}
			questionID := uuid.NewString()
			key = agentexecutionapp.ServerHandOffKey + "_" + questionID
			taken, err := takeTurnEnded(ctx, tx, candidate.projectID, candidate.responseMessageID, candidate.authorUserID,
				key, "server", &questionID)
			if err != nil || taken == 0 {
				// A client took the rows first.
				return err
			}
		}
		var capability string
		err := tx.QueryRow(ctx, `
SELECT conversation.uuid::text, question.sent_to_id, job.capability_id,
       message.handoff_question_id::text
FROM elitea_runtime.execution_steering_messages AS message
JOIN elitea_runtime.execution_jobs AS job
  ON job.execution_id = message.execution_id
 AND job.generation = message.generation
JOIN chat_message_group AS response
  ON response.uuid = message.response_message_id
JOIN chat_conversations AS conversation
  ON conversation.id = response.conversation_id
JOIN chat_message_group AS question
  ON question.id = response.reply_to_id
WHERE message.project_id = $1 AND message.response_message_id = $2::uuid
  AND message.author_user_id = $3 AND message.handoff_key = $4
  AND message.state = 'handed_off' AND question.sent_to_id IS NOT NULL
ORDER BY message.queue_order
LIMIT 1`, candidate.projectID, candidate.responseMessageID, candidate.authorUserID, key).
			Scan(&handOff.ConversationUUID, &handOff.ParticipantID, &capability, &handOff.QuestionID)
		if errors.Is(err, pgx.ErrNoRows) {
			// The conversation or the answer is gone: nothing can start.
			_, dropErr := tx.Exec(ctx, dropServerHandOffSQL, candidate.projectID, candidate.responseMessageID, candidate.authorUserID, key)
			return dropErr
		}
		if err != nil {
			return fmt.Errorf("read steering hand-off target: %w", err)
		}
		handOff.Adhoc = capability == "agent.execute.adhoc.v1"
		messages, err := handOffRows(ctx, tx, candidate.projectID, candidate.responseMessageID, candidate.authorUserID, key)
		if err != nil {
			return err
		}
		for _, message := range messages {
			handOff.SteeringIDs = append(handOff.SteeringIDs, message.ID)
			handOff.Texts = append(handOff.Texts, message.Content)
		}
		ok = len(messages) > 0
		return nil
	})
	if err != nil {
		return agentexecutionapp.ServerHandOff{}, false, nil, err
	}
	return handOff, ok, effects, nil
}

// newerAuthorTurnSQL finds the author's newest message in the answer's
// conversation that is newer than the answer, its age, and the newest answer
// to it with that answer's execution, when it has one. A steering message
// (meta.steering) is not a newer message: it is part of a turn.
const newerAuthorTurnSQL = `
SELECT EXTRACT(EPOCH FROM clock_timestamp() - newer.created_at)::float8,
       COALESCE(binding.response_uuid, ''),
       COALESCE(binding.execution_id, ''),
       COALESCE(binding.generation, 0)
FROM chat_message_group AS response
JOIN chat_participant_mapping AS author_mapping
  ON author_mapping.conversation_id = response.conversation_id
JOIN chat_participants AS author
  ON author.id = author_mapping.participant_id
 AND author.entity_name = 'user'
 AND author.entity_meta ->> 'id' = $2::bigint::text
JOIN chat_message_group AS newer
  ON newer.conversation_id = response.conversation_id
 AND newer.author_participant_id = author.id
 AND newer.id > response.id
 AND newer.reply_to_id IS NULL
 AND NOT (COALESCE(newer.meta, '{}'::jsonb) ? 'steering')
LEFT JOIN LATERAL (
    SELECT newer_response.uuid::text AS response_uuid,
           job_binding.execution_id,
           job_binding.generation
    FROM chat_message_group AS newer_response
    JOIN elitea_runtime.agent_execution_jobs AS job_binding
      ON job_binding.client_message_id = newer_response.uuid::text
     AND job_binding.execution_id = newer_response.task_id
     AND job_binding.client_execution_generation = newer_response.meta ->> 'execution_generation'
    WHERE newer_response.reply_to_id = newer.id
      AND newer_response.conversation_id = newer.conversation_id
    ORDER BY newer_response.id DESC
    LIMIT 1
) AS binding ON TRUE
WHERE response.uuid = $1::uuid
ORDER BY newer.id DESC
LIMIT 1`

// carryWaitForAnswer is how long a newer message may have no answer yet (its
// start is being admitted) before the hand-off starts its own turn.
const carryWaitForAnswer = time.Minute

// carryTurnEnded carries the author's turn_ended messages of the answer over
// to the author's newer turn, when the author already sent a newer message.
// The rows move to the newer answer's execution as `pending`, with their
// revision advanced, and each gets steer.queued on the newer turn's stream:
//   - The newer turn runs: its worker injects them at its next safe point.
//     They keep their queue order, which is lower than that of every
//     steering message of the newer turn (they were queued before it
//     started), so they come first.
//   - The newer turn ended (or is a pipeline turn, which has no safe point):
//     they read `turn_ended` on the newer answer, and the newer answer's
//     hand-off (a client's, or this routine's) starts the next turn with
//     them, after the newer turn.
//
// handled is false when there is no newer message, or when its answer has
// been unknown for longer than carryWaitForAnswer (the caller then starts a
// turn). It is true without a move while a young newer message waits for its
// answer: the next pass tries again.
func carryTurnEnded(ctx context.Context, tx sqlExecutor, candidate handOffCandidate) ([]agentexecutionapp.SteeringEffect, bool, error) {
	var ageSeconds float64
	var newerResponse, newerExecution string
	var newerGeneration int64
	err := tx.QueryRow(ctx, newerAuthorTurnSQL, candidate.responseMessageID, candidate.authorUserID).
		Scan(&ageSeconds, &newerResponse, &newerExecution, &newerGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("find a newer message of the steering author: %w", err)
	}
	if newerExecution == "" {
		return nil, ageSeconds < carryWaitForAnswer.Seconds(), nil
	}
	// The same ordered row lock as the client's hand-off: the first of the
	// two wins, the other finds no waiting row.
	rows, err := tx.Query(ctx, `
WITH waiting AS (
    SELECT message.steering_id
    FROM elitea_runtime.execution_steering_messages AS message
    JOIN elitea_runtime.execution_jobs AS job
      ON job.execution_id = message.execution_id
     AND job.generation = message.generation
    WHERE message.project_id = $1
      AND message.response_message_id = $2::uuid
      AND message.author_user_id = $3
      AND message.state IN ('pending', 'claimed')
      AND `+steeringEndedSQL+`
    ORDER BY message.queue_order
    FOR UPDATE OF message
), message AS (
    UPDATE elitea_runtime.execution_steering_messages AS message
    SET execution_id = $4,
        generation = $5,
        response_message_id = $6::uuid,
        state = 'pending',
        claim_lease_epoch = NULL,
        -- The client's key belonged to the old answer; a fresh one cannot
        -- collide with a key of the newer turn (UNIQUE per execution).
        idempotency_key = 'carried_' || replace(message.steering_id::text, '-', ''),
        revision = message.revision + 1,
        updated_at = clock_timestamp()
    FROM waiting
    WHERE message.steering_id = waiting.steering_id
      AND message.state IN ('pending', 'claimed')
    RETURNING message.*
)
SELECT`+steeringColumns+`
FROM message
JOIN elitea_runtime.execution_jobs AS job
  ON job.execution_id = message.execution_id
 AND job.generation = message.generation
ORDER BY message.queue_order`, candidate.projectID, candidate.responseMessageID, candidate.authorUserID,
		newerExecution, newerGeneration, newerResponse)
	if err != nil {
		return nil, true, fmt.Errorf("carry steering over: %w", err)
	}
	var carried []agentexecutionapp.SteeringMessage
	for rows.Next() {
		message, scanErr := scanSteering(rows)
		if scanErr != nil {
			rows.Close()
			return nil, true, fmt.Errorf("scan carried steering: %w", scanErr)
		}
		carried = append(carried, message)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, true, fmt.Errorf("carry steering over: %w", err)
	}
	var effects []agentexecutionapp.SteeringEffect
	for _, message := range carried {
		effect, err := appendSteeringEvent(ctx, tx, candidate.projectID, newerExecution, newerGeneration,
			agentexecutionapp.SteeringEventQueued, message, "")
		if err != nil {
			return nil, true, err
		}
		effects = append(effects, effect)
	}
	return effects, true, nil
}

func (r *SteeringRepository) MarkServerHandOffStarted(ctx context.Context, handOff agentexecutionapp.ServerHandOff) error {
	if _, err := r.shared.Exec(ctx, `
UPDATE elitea_runtime.execution_steering_messages
SET handoff_started_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE project_id = $1 AND response_message_id = $2::uuid AND author_user_id = $3
  AND state = 'handed_off' AND handoff_by = 'server'
  AND handoff_question_id = $4::uuid AND handoff_started_at IS NULL`,
		handOff.ProjectID, handOff.ResponseMessageID, handOff.AuthorUserID, handOff.QuestionID); err != nil {
		return fmt.Errorf("mark steering hand-off started: %w", err)
	}
	return nil
}

// dropServerHandOffSQL removes the rows of a server hand-off that can never
// start. A removed row keeps no hand-off columns (0178 _handoff).
const dropServerHandOffSQL = `
UPDATE elitea_runtime.execution_steering_messages
SET state = 'removed', handoff_key = NULL, handoff_by = NULL, handoff_question_id = NULL,
    handed_off_at = NULL, updated_at = clock_timestamp()
WHERE project_id = $1 AND response_message_id = $2::uuid AND author_user_id = $3
  AND handoff_key = $4 AND state = 'handed_off' AND handoff_by = 'server'
  AND handoff_started_at IS NULL`

func (r *SteeringRepository) DropServerHandOff(ctx context.Context, handOff agentexecutionapp.ServerHandOff) error {
	key := agentexecutionapp.ServerHandOffKey + "_" + handOff.QuestionID
	if _, err := r.shared.Exec(ctx, dropServerHandOffSQL,
		handOff.ProjectID, handOff.ResponseMessageID, handOff.AuthorUserID, key); err != nil {
		return fmt.Errorf("drop steering hand-off: %w", err)
	}
	return nil
}
