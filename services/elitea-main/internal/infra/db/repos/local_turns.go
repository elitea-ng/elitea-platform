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
	applicationID, versionID, err := readLocalTurnAgent(ctx, tx, schema, target.conversationID, target.targetID, record.ProjectID)
	if err != nil {
		return localturn.StartedTurn{}, err
	}
	actor := strconv.FormatInt(record.ActorUserID, 10)
	turn := localturn.StartedTurn{Created: true}
	err = tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.local_turn_executions (
    execution_id, project_id, actor_id, token_id, native_client_id, conversation_uuid,
    question_id, response_message_id, target_participant_id, memories_used, expires_at,
    application_id, version_id
) VALUES ($1, $2, $3, $4, $5, $6::uuid, $7::uuid, $8::uuid, $9, $10,
          clock_timestamp() + make_interval(secs => $11), $12, $13)
ON CONFLICT (project_id, actor_id, question_id) DO NOTHING
RETURNING execution_id, response_message_id::text, target_participant_id, expires_at`,
		record.ExecutionID, record.ProjectID, actor, record.TokenID, record.NativeClientID,
		record.ConversationUUID, record.QuestionID, record.ResponseMessageID, target.targetID,
		record.MemoriesUsed, record.TTL.Seconds(), applicationID, versionID,
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
		turn           localturn.StartedTurn
		conversation   string
		committed      bool
		expired        bool
		tokenID        string
		nativeClientID string
	)
	err := tx.QueryRow(ctx, `
SELECT execution_id, response_message_id::text, target_participant_id, expires_at,
       conversation_uuid::text, committed_at IS NOT NULL, expires_at <= clock_timestamp(),
       token_id, native_client_id
FROM elitea_runtime.local_turn_executions
WHERE project_id = $1 AND actor_id = $2 AND question_id = $3::uuid
FOR UPDATE`, record.ProjectID, actor, record.QuestionID).
		Scan(&turn.ExecutionID, &turn.ResponseMessageID, &turn.ParticipantID, &turn.ExpiresAt,
			&conversation, &committed, &expired, &tokenID, &nativeClientID)
	if err != nil {
		return localturn.StartedTurn{}, fmt.Errorf("local turn: read replayed start: %w", err)
	}
	switch {
	// Another device or token of the same user retrying this question is a
	// conflict, not a replay: the turn stays bound to the family that started it.
	case conversation != record.ConversationUUID || turn.ParticipantID != targetID ||
		tokenID != record.TokenID || nativeClientID != record.NativeClientID:
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
		credential   localturn.Credential
	)
	err = tx.QueryRow(ctx, `
SELECT conversation_uuid::text, question_id::text, response_message_id::text,
       target_participant_id, memories_used, committed_at, commit_digest,
       expires_at <= clock_timestamp(), started_at, token_id, native_client_id
FROM elitea_runtime.local_turn_executions
WHERE execution_id = $1 AND project_id = $2 AND actor_id = $3
FOR UPDATE`, record.ExecutionID, record.ProjectID, strconv.FormatInt(record.ActorUserID, 10)).
		Scan(&turn.ConversationUUID, &questionID, &turn.ResponseMessageID, &participant,
			&turn.MemoriesUsed, &committedAt, &storedDigest, &expired, &startedAt,
			&credential.TokenID, &credential.NativeClientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return localturn.CommittedTurn{}, localturn.ErrNotFound
	}
	if err != nil {
		return localturn.CommittedTurn{}, fmt.Errorf("local turn: lock execution: %w", err)
	}
	// Only the credential family that started the turn commits it, a replay
	// included; another one learns no more than another caller would.
	if credential != record.Credential {
		return localturn.CommittedTurn{}, localturn.ErrNotFound
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

// readLocalTurnAgent answers the agent version the answering participant is
// mapped to (the version a cloud turn of that participant would run:
// ResolveCurrentApplicationTurn reads the same entity_meta.id and mapping
// entity_settings.version_id), or nil/nil for the model (dummy) participant and
// for an agent of another project (a public catalogue agent): the remote
// toolkit call serves only this project's agents. Start pins the answer on the
// execution row, so the turn keeps the version it started with.
func readLocalTurnAgent(
	ctx context.Context, tx pgx.Tx, schema string, conversationID, participantID, projectID int64,
) (*int64, *int64, error) {
	var applicationID, versionID, applicationProject *int64
	err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT CASE WHEN participant.entity_meta ->> 'id' ~ '^[1-9][0-9]{0,9}$'
            THEN (participant.entity_meta ->> 'id')::bigint END,
       CASE WHEN participant.entity_meta ->> 'project_id' ~ '^[1-9][0-9]{0,9}$'
            THEN (participant.entity_meta ->> 'project_id')::bigint END,
       CASE WHEN mapping.entity_settings ->> 'version_id' ~ '^[1-9][0-9]{0,9}$'
            THEN (mapping.entity_settings ->> 'version_id')::bigint END
FROM %[1]s.chat_participant_mapping AS mapping
JOIN %[1]s.chat_participants AS participant
  ON participant.id = mapping.participant_id AND participant.entity_name = 'application'
WHERE mapping.conversation_id = $1 AND mapping.participant_id = $2
LIMIT 1`, schema), conversationID, participantID).Scan(&applicationID, &applicationProject, &versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil // the model participant: no agent version
	}
	if err != nil {
		return nil, nil, fmt.Errorf("local turn: read participant agent: %w", err)
	}
	// The project must be stated and equal, as the cloud admission requires
	// (agent_start.go compares entity_meta.project_id): an id alone could name
	// a catalogue agent that shares its number with one of this project's.
	// Both ids fit the INTEGER columns: the patterns admit at most 10 digits,
	// and an id above MaxInt32 is no agent.
	if applicationID == nil || versionID == nil || applicationProject == nil || *applicationProject != projectID ||
		*applicationID > 2147483647 || *versionID > 2147483647 {
		return nil, nil, nil
	}
	return applicationID, versionID, nil
}

var _ localturn.BindingStore = (*LocalTurnsRepo)(nil)

// ReadLocalTurnBinding answers a started turn's state and the agent version it
// PINNED at start (readLocalTurnAgent). A model turn, or one whose
// participant was an agent of another project, answers 0/0.
func (r *LocalTurnsRepo) ReadLocalTurnBinding(
	ctx context.Context, projectID, actorUserID int64, executionID string,
) (localturn.StoredBinding, error) {
	if _, err := tenantSchema(strconv.FormatInt(projectID, 10)); err != nil {
		return localturn.StoredBinding{}, localturn.ErrInvalid
	}
	var (
		binding                  localturn.StoredBinding
		applicationID, versionID *int64
	)
	err := r.pool.QueryRow(ctx, `
SELECT application_id, version_id, committed_at IS NOT NULL, expires_at <= clock_timestamp(),
       token_id, native_client_id
FROM elitea_runtime.local_turn_executions
WHERE execution_id = $1 AND project_id = $2 AND actor_id = $3`,
		executionID, projectID, strconv.FormatInt(actorUserID, 10)).
		Scan(&applicationID, &versionID, &binding.Committed, &binding.Expired,
			&binding.Credential.TokenID, &binding.Credential.NativeClientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return localturn.StoredBinding{}, localturn.ErrNotFound
	}
	if err != nil {
		return localturn.StoredBinding{}, fmt.Errorf("local turn: read binding: %w", err)
	}
	if applicationID != nil && versionID != nil {
		binding.ApplicationID, binding.VersionID = *applicationID, *versionID
	}
	return binding, nil
}
