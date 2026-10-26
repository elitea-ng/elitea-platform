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

func (r *SteeringRepository) TakeServerHandOffs(ctx context.Context, readyBefore, retryBefore time.Duration, limit int) ([]agentexecutionapp.ServerHandOff, error) {
	if limit <= 0 {
		return nil, nil
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
		return nil, fmt.Errorf("expire steering hand-offs: %w", err)
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
		return nil, fmt.Errorf("find steering hand-offs: %w", err)
	}
	var candidates []handOffCandidate
	for rows.Next() {
		var candidate handOffCandidate
		if err := rows.Scan(&candidate.projectID, &candidate.responseMessageID, &candidate.authorUserID, &candidate.key); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan steering hand-off: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find steering hand-offs: %w", err)
	}
	var handOffs []agentexecutionapp.ServerHandOff
	for _, candidate := range candidates {
		if len(handOffs) >= limit {
			break
		}
		handOff, ok, err := r.takeServerHandOff(ctx, candidate)
		if err != nil {
			return handOffs, err
		}
		if ok {
			handOffs = append(handOffs, handOff)
		}
	}
	return handOffs, nil
}

// takeServerHandOff takes one group (or reads a group it took before) with the
// context of the next turn: the answer's conversation and participant.
func (r *SteeringRepository) takeServerHandOff(ctx context.Context, candidate handOffCandidate) (agentexecutionapp.ServerHandOff, bool, error) {
	handOff := agentexecutionapp.ServerHandOff{
		ProjectID: candidate.projectID, ResponseMessageID: candidate.responseMessageID, AuthorUserID: candidate.authorUserID,
	}
	ok := false
	err := r.projects.WithinProjectTx(ctx, candidate.projectID, steeringWriteTx, func(tx sqlExecutor) error {
		key := candidate.key
		if key == "" {
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
		return agentexecutionapp.ServerHandOff{}, false, err
	}
	return handOff, ok, nil
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
