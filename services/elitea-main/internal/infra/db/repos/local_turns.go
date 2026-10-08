package repos

// Desktop local turns (ADR-0029 decision 5c): the execution row in
// elitea_runtime.local_turn_executions (shared/0155), and the commit that
// writes the finished turn into the EXISTING tenant chat and trace projection
// tables — the same rows a cloud turn's admission and finalization write
// (agent_chat.sql InsertCurrentApplicationTurn / FinalizeCurrentAgentFullMessage,
// agent_trace.go's trace rows), so every reader, the changes_since delta
// included (tenant/0144 triggers), sees a local turn like any other.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/localturn"
)

// LocalTurnsRepo implements localturn.Store.
type LocalTurnsRepo struct {
	pool *pgxpool.Pool
}

var _ localturn.Store = (*LocalTurnsRepo)(nil)

func NewLocalTurnsRepo(pool *pgxpool.Pool) *LocalTurnsRepo {
	return &LocalTurnsRepo{pool: pool}
}

type localTurnTarget struct {
	conversationID    int64
	userParticipantID int64
	targetID          int64
}

// resolveLocalTurnTarget applies the admission's own rule: the caller must
// be a mapped `user` participant of the conversation, and the answering
// participant must be a mapped `application` (participantID > 0) or the
// conversation's `dummy` model participant (participantID 0).
func resolveLocalTurnTarget(
	ctx context.Context, tx pgx.Tx, schema, conversationUUID string, actorUserID, participantID int64,
) (localTurnTarget, error) {
	var target localTurnTarget
	var targetID *int64
	err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT conversation.id, author.id, answering.id
FROM %[1]s.chat_conversations AS conversation
JOIN %[1]s.chat_participant_mapping AS author_mapping
  ON author_mapping.conversation_id = conversation.id
JOIN %[1]s.chat_participants AS author
  ON author.id = author_mapping.participant_id
 AND author.entity_name = 'user'
 AND author.entity_meta ->> 'id' = ($2::bigint)::text
LEFT JOIN LATERAL (
    SELECT participant.id
    FROM %[1]s.chat_participant_mapping AS mapping
    JOIN %[1]s.chat_participants AS participant ON participant.id = mapping.participant_id
    WHERE mapping.conversation_id = conversation.id
      AND (
          ($3::bigint > 0 AND participant.id = $3::bigint AND participant.entity_name IN ('application', 'dummy'))
          OR ($3::bigint = 0 AND participant.entity_name = 'dummy')
      )
    ORDER BY participant.id
    LIMIT 1
) AS answering ON TRUE
WHERE conversation.uuid = $1::uuid
ORDER BY author.id
LIMIT 1`, schema), conversationUUID, actorUserID, participantID).
		Scan(&target.conversationID, &target.userParticipantID, &targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return localTurnTarget{}, localturn.ErrNotFound
	}
	if err != nil {
		return localTurnTarget{}, fmt.Errorf("local turn: resolve conversation: %w", err)
	}
	if targetID == nil {
		return localTurnTarget{}, localturn.ErrParticipant
	}
	target.targetID = *targetID
	return target, nil
}

// StartLocalTurn opens the execution, or replays the one this caller already
// opened for the same question.
func (r *LocalTurnsRepo) StartLocalTurn(ctx context.Context, record localturn.StartRecord) (localturn.StartedTurn, error) {
	schema, err := tenantSchema(strconv.FormatInt(record.ProjectID, 10))
	if err != nil {
		return localturn.StartedTurn{}, localturn.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return localturn.StartedTurn{}, fmt.Errorf("local turn: begin start: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	target, err := resolveLocalTurnTarget(ctx, tx, schema, record.ConversationUUID, record.ActorUserID, record.ParticipantID)
	if err != nil {
		return localturn.StartedTurn{}, err
	}
	actor := strconv.FormatInt(record.ActorUserID, 10)
	turn := localturn.StartedTurn{Created: true}
	err = tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.local_turn_executions (
    execution_id, project_id, actor_id, token_id, native_client_id, conversation_uuid,
    question_id, response_message_id, target_participant_id, memories_used, expires_at
) VALUES ($1, $2, $3, $4, $5, $6::uuid, $7::uuid, $8::uuid, $9, $10,
          clock_timestamp() + make_interval(secs => $11))
ON CONFLICT (project_id, actor_id, question_id) DO NOTHING
RETURNING execution_id, response_message_id::text, target_participant_id, expires_at`,
		record.ExecutionID, record.ProjectID, actor, record.TokenID, record.NativeClientID,
		record.ConversationUUID, record.QuestionID, record.ResponseMessageID, target.targetID,
		record.MemoriesUsed, record.TTL.Seconds(),
	).Scan(&turn.ExecutionID, &turn.ResponseMessageID, &turn.ParticipantID, &turn.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		turn, err = replayLocalTurnStart(ctx, tx, record, actor, target.targetID)
	}
	if err != nil {
		return localturn.StartedTurn{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return localturn.StartedTurn{}, fmt.Errorf("local turn: commit start: %w", err)
	}
	return turn, nil
}

func replayLocalTurnStart(
	ctx context.Context, tx pgx.Tx, record localturn.StartRecord, actor string, targetID int64,
) (localturn.StartedTurn, error) {
	var (
		turn         localturn.StartedTurn
		conversation string
		committed    bool
		expired      bool
	)
	err := tx.QueryRow(ctx, `
SELECT execution_id, response_message_id::text, target_participant_id, expires_at,
       conversation_uuid::text, committed_at IS NOT NULL, expires_at <= clock_timestamp()
FROM elitea_runtime.local_turn_executions
WHERE project_id = $1 AND actor_id = $2 AND question_id = $3::uuid
FOR UPDATE`, record.ProjectID, actor, record.QuestionID).
		Scan(&turn.ExecutionID, &turn.ResponseMessageID, &turn.ParticipantID, &turn.ExpiresAt,
			&conversation, &committed, &expired)
	if err != nil {
		return localturn.StartedTurn{}, fmt.Errorf("local turn: read replayed start: %w", err)
	}
	switch {
	case conversation != record.ConversationUUID || turn.ParticipantID != targetID:
		return localturn.StartedTurn{}, localturn.ErrConflict
	case committed:
		return localturn.StartedTurn{}, localturn.ErrAlreadyCommitted
	case expired:
		return localturn.StartedTurn{}, localturn.ErrExpired
	}
	// The recall is recomputed on every start; keep the count of the one the
	// caller was last given, which is the one its turn uses.
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.local_turn_executions SET memories_used = $2 WHERE execution_id = $1`,
		turn.ExecutionID, record.MemoriesUsed); err != nil {
		return localturn.StartedTurn{}, fmt.Errorf("local turn: refresh replayed start: %w", err)
	}
	return turn, nil
}

// CommitLocalTurn writes the finished turn, once per execution.
func (r *LocalTurnsRepo) CommitLocalTurn(ctx context.Context, record localturn.CommitRecord) (localturn.CommittedTurn, error) {
	schema, err := tenantSchema(strconv.FormatInt(record.ProjectID, 10))
	if err != nil {
		return localturn.CommittedTurn{}, localturn.ErrInvalid
	}
	toolCalls, err := decodeOrderedCurrentAgentToolCalls(record.ToolCalls)
	if err != nil || len(toolCalls) > localturn.MaxToolCalls {
		return localturn.CommittedTurn{}, localturn.ErrInvalid
	}
	thinkingSteps := make([]map[string]any, 0, len(record.ThinkingSteps))
	for _, raw := range record.ThinkingSteps {
		step, err := decodeCurrentAgentJSONObject(raw)
		if err != nil {
			return localturn.CommittedTurn{}, localturn.ErrInvalid
		}
		thinkingSteps = append(thinkingSteps, step)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return localturn.CommittedTurn{}, fmt.Errorf("local turn: begin commit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		turn         = localturn.CommittedTurn{ExecutionID: record.ExecutionID}
		questionID   string
		participant  int64
		committedAt  *time.Time
		storedDigest []byte
		expired      bool
		startedAt    time.Time
	)
	err = tx.QueryRow(ctx, `
SELECT conversation_uuid::text, question_id::text, response_message_id::text,
       target_participant_id, memories_used, committed_at, commit_digest,
       expires_at <= clock_timestamp(), started_at
FROM elitea_runtime.local_turn_executions
WHERE execution_id = $1 AND project_id = $2 AND actor_id = $3
FOR UPDATE`, record.ExecutionID, record.ProjectID, strconv.FormatInt(record.ActorUserID, 10)).
		Scan(&turn.ConversationUUID, &questionID, &turn.ResponseMessageID, &participant,
			&turn.MemoriesUsed, &committedAt, &storedDigest, &expired, &startedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return localturn.CommittedTurn{}, localturn.ErrNotFound
	}
	if err != nil {
		return localturn.CommittedTurn{}, fmt.Errorf("local turn: lock execution: %w", err)
	}
	turn.QuestionMessageID = questionID
	if committedAt != nil {
		if !bytes.Equal(storedDigest, record.Digest[:]) {
			return localturn.CommittedTurn{}, localturn.ErrAlreadyCommitted
		}
		turn.CommittedAt = *committedAt
		return turn, nil // a retried commit: the same body, already written
	}
	if expired {
		return localturn.CommittedTurn{}, localturn.ErrExpired
	}
	target, err := resolveLocalTurnTarget(ctx, tx, schema, turn.ConversationUUID, record.ActorUserID, participant)
	if err != nil {
		return localturn.CommittedTurn{}, err
	}

	// The question is dated at the turn's START, the moment a cloud turn's
	// admission writes its question group, not at the commit. The memory
	// next-turn guarantee reads "the user's previous turn" as the newest
	// message the user authored, so a memory saved while this turn ran
	// (after started_at) must stay newer than it, and the next turn reserves it.
	var questionGroup int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_message_group
    (uuid, author_participant_id, conversation_id, sent_to_id, meta, is_streaming, created_at)
VALUES ($1::uuid, $2, $3, $4, $5::jsonb, FALSE, $6::timestamptz)
RETURNING id`, schema),
		questionID, target.userParticipantID, target.conversationID, target.targetID, string(record.QuestionMeta),
		startedAt,
	).Scan(&questionGroup)
	if err != nil {
		return localturn.CommittedTurn{}, localTurnWriteError("insert question", err)
	}
	if err := insertLocalTurnText(ctx, tx, schema, questionGroup, record.UserMessage); err != nil {
		return localturn.CommittedTurn{}, err
	}

	var responseGroup int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_message_group
    (uuid, author_participant_id, conversation_id, reply_to_id, meta, is_streaming, created_at, task_id)
VALUES ($1::uuid, $2, $3, $4, $5::jsonb, FALSE, $7::timestamptz + interval '1 second', $6)
RETURNING id`, schema),
		turn.ResponseMessageID, target.targetID, target.conversationID, questionGroup,
		string(record.ResponseMeta), record.ExecutionID, startedAt,
	).Scan(&responseGroup)
	if err != nil {
		return localturn.CommittedTurn{}, localTurnWriteError("insert answer", err)
	}
	if record.AssistantMessage != "" {
		if err := insertLocalTurnText(ctx, tx, schema, responseGroup, record.AssistantMessage); err != nil {
			return localturn.CommittedTurn{}, err
		}
	}

	// Tool steps and thinking steps go through the SAME merge and row mapping
	// a worker's partial_message frames go through, so a local turn's trace
	// reads back from listMessageTraces exactly like a cloud turn's.
	if len(toolCalls) > 0 || len(thinkingSteps) > 0 {
		desired, err := mergeCurrentAgentTraceRows(responseGroup, nil, currentAgentTraceDelta{
			toolCalls: toolCalls, thinkingSteps: thinkingSteps,
		})
		if err != nil {
			return localturn.CommittedTurn{}, errors.Join(localturn.ErrInvalid, err)
		}
		if err := reconcileCurrentAgentTraceRows(ctx, pgxExecutor{queryer: tx}, schema, responseGroup, nil, desired); err != nil {
			return localturn.CommittedTurn{}, localTurnWriteError("write trace", err)
		}
	}

	if err := tx.QueryRow(ctx, `
UPDATE elitea_runtime.local_turn_executions
SET committed_at = clock_timestamp(), commit_digest = $2
WHERE execution_id = $1
RETURNING committed_at`, record.ExecutionID, record.Digest[:]).Scan(&turn.CommittedAt); err != nil {
		return localturn.CommittedTurn{}, fmt.Errorf("local turn: mark committed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return localturn.CommittedTurn{}, fmt.Errorf("local turn: commit: %w", err)
	}
	turn.Created = true
	return turn, nil
}

func insertLocalTurnText(ctx context.Context, tx pgx.Tx, schema string, groupID int64, content string) error {
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
WITH item AS (
    INSERT INTO %[1]s.chat_message_items (uuid, item_type, order_index, meta, message_group_id)
    VALUES (gen_random_uuid(), 'text_message', 0, '{}'::jsonb, $1)
    RETURNING id
)
INSERT INTO %[1]s.chat_messages_text (id, content)
SELECT item.id, $2 FROM item`, schema), groupID, content); err != nil {
		return fmt.Errorf("local turn: insert text: %w", err)
	}
	return nil
}

// localTurnWriteError maps a unique violation (the question id or the answer
// id already names a message, for example a cloud turn's) to ErrConflict.
func localTurnWriteError(step string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return localturn.ErrConflict
	}
	return fmt.Errorf("local turn: %s: %w", step, err)
}
