package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// Participant entity names the turn resolver matches on, as literals
// (agent_chat.sql ResolveCurrentApplicationTurn).
const (
	evalApplicationEntityName = "application"
	evalUserEntityName        = "user"
	maxEvalConversationName   = 120
)

// EvalConversationSource marks the conversations a runtime_plane case creates
// (`chat_conversations.source`), beside `mcp` and `pipeline_trigger`. They stay
// in the chat list so a case's transcript can be opened from the scorecard.
const EvalConversationSource = "evaluation"

// EvalCaseTurn is what one read of a case's response message sees.
type EvalCaseTurn struct {
	Settled bool
	// Failed is the projected runtime failure, with its safe message.
	Failed  bool
	Failure string
	// Paused is a human-approval, authorization or output-limit pause.
	Paused string
	// Text is the assistant's answer.
	Text string
}

// EvalRuntimeStore is the chat-side database access of an evaluation case that
// runs as an agent run (the runtimeplane.Store).
type EvalRuntimeStore struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewEvalRuntimeStore builds the store; a nil pool is refused.
func NewEvalRuntimeStore(pool *pgxpool.Pool, logger *slog.Logger) (*EvalRuntimeStore, error) {
	if pool == nil {
		return nil, errors.New("runtimeplane: a database pool is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EvalRuntimeStore{pool: pool, logger: logger}, nil
}

func evalSchemaOf(projectID int64) (string, error) {
	return tenantschema.Quote(strconv.FormatInt(projectID, 10))
}

// PrepareConversation creates the five rows a turn needs, in one transaction
// (as the MCP bridge does): the conversation, the author's participant and its
// mapping, and the agent's participant and its mapping, whose
// `entity_settings.version_id` pins the version the run scores.
func (s *EvalRuntimeStore) PrepareConversation(
	ctx context.Context, projectID, actorUserID, applicationID, versionID int64, name string,
) (string, int64, error) {
	schema, err := evalSchemaOf(projectID)
	if err != nil {
		return "", 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if runes := []rune(name); len(runes) > maxEvalConversationName {
		name = string(runes[:maxEvalConversationName])
	}
	meta, err := json.Marshal(map[string]any{"conversation_type": EvalConversationSource})
	if err != nil {
		return "", 0, err
	}
	var conversationID int64
	var conversationUUID string
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_conversations (uuid, name, is_private, author_id, meta, source, created_at)
VALUES (gen_random_uuid(), $1, TRUE, $2, $3::jsonb, $4, now() AT TIME ZONE 'UTC')
RETURNING id, uuid::text`, schema), name, actorUserID, string(meta), EvalConversationSource,
	).Scan(&conversationID, &conversationUUID); err != nil {
		return "", 0, fmt.Errorf("create conversation: %w", err)
	}

	authorMeta, _ := json.Marshal(map[string]any{"id": actorUserID})
	authorID, err := evalFindOrCreateParticipant(ctx, tx, schema, evalUserEntityName, authorMeta)
	if err != nil {
		return "", 0, err
	}
	if err := evalMapParticipant(ctx, tx, schema, conversationID, authorID, []byte(`{}`)); err != nil {
		return "", 0, err
	}
	agentMeta, _ := json.Marshal(map[string]any{"id": applicationID, "project_id": projectID})
	agentID, err := evalFindOrCreateParticipant(ctx, tx, schema, evalApplicationEntityName, agentMeta)
	if err != nil {
		return "", 0, err
	}
	// The version is pinned on the MAPPING, which belongs to this conversation;
	// the participant is shared across conversations.
	settings, _ := json.Marshal(map[string]any{"version_id": versionID})
	if err := evalMapParticipant(ctx, tx, schema, conversationID, agentID, settings); err != nil {
		return "", 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", 0, fmt.Errorf("commit: %w", err)
	}
	return conversationUUID, agentID, nil
}

func evalFindOrCreateParticipant(ctx context.Context, tx pgx.Tx, schema, entityName string, entityMeta []byte) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.chat_participants
WHERE entity_name = $1::text
  AND entity_meta ->> 'id' IS NOT DISTINCT FROM $2::jsonb ->> 'id'
  AND entity_meta ->> 'project_id' IS NOT DISTINCT FROM $2::jsonb ->> 'project_id'
ORDER BY id
LIMIT 1`, schema), entityName, entityMeta).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("look up %s participant: %w", entityName, err)
	}
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), $1, $2::jsonb, '{}'::json)
RETURNING id`, schema), entityName, entityMeta).Scan(&id); err != nil {
		return 0, fmt.Errorf("create %s participant: %w", entityName, err)
	}
	return id, nil
}

func evalMapParticipant(ctx context.Context, tx pgx.Tx, schema string, conversationID, participantID int64, settings []byte) error {
	// ON CONFLICT names the columns, not the constraint: its generated name
	// differs between a ledgered tenant and the bootstrap schema.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at)
VALUES ($1, $2, $3::jsonb, now() AT TIME ZONE 'UTC')
ON CONFLICT (participant_id, conversation_id) DO NOTHING`, schema),
		conversationID, participantID, settings); err != nil {
		return fmt.Errorf("map participant: %w", err)
	}
	return nil
}

// DiscardConversation removes a conversation whose turn was never admitted,
// guarded on its source and on holding no message group.
func (s *EvalRuntimeStore) DiscardConversation(ctx context.Context, projectID int64, conversationUUID string) {
	schema, err := evalSchemaOf(projectID)
	if err != nil || conversationUUID == "" {
		return
	}
	if _, err := DiscardUnadmittedConversation(context.WithoutCancel(ctx), s.pool, schema,
		conversationUUID, EvalConversationSource); err != nil {
		s.logger.Warn("evaluation: discard case conversation failed", "conversation_uuid", conversationUUID, "error", err)
	}
}

// ReadTurn reads the response message group the way the chat transcript and
// the MCP bridge do: the discriminators are written in the same transaction as
// the terminal state, so a row read after is_streaming clears is final.
func (s *EvalRuntimeStore) ReadTurn(ctx context.Context, projectID int64, responseMessageID string) (EvalCaseTurn, error) {
	schema, err := evalSchemaOf(projectID)
	if err != nil {
		return EvalCaseTurn{}, err
	}
	var (
		turn                      EvalCaseTurn
		streaming, hitl, auth, ol bool
	)
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT response.is_streaming,
       COALESCE(response.meta ->> 'is_error', 'false') = 'true',
       COALESCE(response.meta ->> 'error', ''),
       response.meta ? 'hitl_interrupt',
       response.meta ? 'authorization_requests',
       COALESCE(response.meta -> 'output_limit_reached' = 'true'::jsonb, FALSE),
       COALESCE((
           SELECT string_agg(item_text.content, '' ORDER BY item.order_index, item.id)
           FROM %[1]s.chat_message_items AS item
           JOIN %[1]s.chat_messages_text AS item_text ON item_text.id = item.id
           WHERE item.message_group_id = response.id
             AND item.item_type = 'text_message'
       ), '')
FROM %[1]s.chat_message_group AS response
WHERE response.uuid = $1::uuid`, schema), responseMessageID).Scan(
		&streaming, &turn.Failed, &turn.Failure, &hitl, &auth, &ol, &turn.Text)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The admission committed the row, so its absence means somebody
			// removed the turn (a cancel deletes an empty response group).
			return EvalCaseTurn{Settled: true, Failed: true, Failure: "the run was cancelled before it answered"}, nil
		}
		return EvalCaseTurn{}, fmt.Errorf("read agent turn: %w", err)
	}
	turn.Settled = !streaming
	switch {
	case hitl && auth:
		turn.Paused = "human approval and tool authorization"
	case hitl:
		turn.Paused = "human approval"
	case auth:
		turn.Paused = "tool authorization"
	case ol:
		turn.Paused = "its output limit"
	}
	return turn, nil
}

// StepCount counts the run's recorded steps that the step limit counts.
func (s *EvalRuntimeStore) StepCount(ctx context.Context, projectID int64, executionID string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.run_steps
WHERE run_id = $1 AND project_id = $2
  AND kind IN ('model_call', 'tool_call', 'retrieval', 'delegation')`, executionID, projectID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count run steps: %w", err)
	}
	return count, nil
}

// StepsSettled reports whether the run's step record is final: its settlement
// was prepared (anchor_requested_at, shared 0205), so no step is still to
// arrive.
func (s *EvalRuntimeStore) StepsSettled(ctx context.Context, projectID int64, executionID string) (bool, error) {
	var settled bool
	err := s.pool.QueryRow(ctx, `
SELECT COALESCE(bool_or(anchor_requested_at IS NOT NULL OR anchored_seq > 0), false)
FROM elitea_runtime.run_step_heads WHERE run_id = $1 AND project_id = $2`, executionID, projectID).Scan(&settled)
	if err != nil {
		return false, fmt.Errorf("read run step settlement: %w", err)
	}
	return settled, nil
}
