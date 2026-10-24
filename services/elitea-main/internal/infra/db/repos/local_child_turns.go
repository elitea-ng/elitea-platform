package repos

// Cloud child turns of a desktop local turn (issue 1239): the hidden child
// conversation a nested agent runs in, built the way the MCP `tools/call`
// bridge and the pipeline triggers build theirs (a conversation, the caller's
// user participant, the agent participant pinned to the version on the
// MAPPING), so the ordinary start use case admits the turn and every reader
// sees an ordinary cloud turn.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
)

// childSourceLiteral is localturn.ChildConversationSource as an SQL literal.
// The child lookups name it inline, not as a parameter, so a cached generic
// plan still matches the partial indexes of tenant 0156 (WHERE source =
// 'local_child'), which a parameter cannot prove.
const childSourceLiteral = "'" + localturn.ChildConversationSource + "'"

const (
	// maxChildConversationName bounds the child conversation's title.
	maxChildConversationName = 120
	// maxChildToolCalls bounds the tool-call summary of one read.
	maxChildToolCalls = 32
)

// LocalChildTurnsRepo implements localturn.ChildTurnStore.
type LocalChildTurnsRepo struct {
	pool *pgxpool.Pool
}

var _ localturn.ChildTurnStore = (*LocalChildTurnsRepo)(nil)

func NewLocalChildTurnsRepo(pool *pgxpool.Pool) *LocalChildTurnsRepo {
	return &LocalChildTurnsRepo{pool: pool}
}

// PrepareChildConversation serialises on (project, parent, key) with a
// transaction-scoped advisory lock, so two attempts of one call never both
// create.
func (r *LocalChildTurnsRepo) PrepareChildConversation(
	ctx context.Context, request localturn.ChildConversationRequest,
) (localturn.ChildConversation, error) {
	if r == nil || r.pool == nil {
		return localturn.ChildConversation{}, localturn.ErrUnavailable
	}
	project := strconv.FormatInt(request.ProjectID, 10)
	schema, err := tenantSchema(project)
	if err != nil {
		return localturn.ChildConversation{}, localturn.ErrInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return localturn.ChildConversation{}, fmt.Errorf("local child turn: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"local_child:"+project+":"+request.ParentExecutionID+":"+request.IdempotencyKey); err != nil {
		return localturn.ChildConversation{}, fmt.Errorf("local child turn: lock: %w", err)
	}

	var existing localturn.ChildConversation
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT conversation.uuid::text,
       COALESCE(conversation.meta ->> 'input_sha256', ''),
       COALESCE(conversation.meta ->> 'child_execution_id', ''),
       COALESCE(conversation.meta ->> 'child_response_message_id', ''),
       COALESCE((
           SELECT participant.id
             FROM %[1]s.chat_participant_mapping AS mapping
             JOIN %[1]s.chat_participants AS participant ON participant.id = mapping.participant_id
            WHERE mapping.conversation_id = conversation.id
              AND participant.entity_name = 'application'
            ORDER BY participant.id
            LIMIT 1
       ), 0)
  FROM %[1]s.chat_conversations AS conversation
 WHERE conversation.source = %[2]s
   AND conversation.author_id = $1
   AND conversation.meta ->> 'parent_execution_id' = $2
   AND conversation.meta ->> 'idempotency_key' = $3
 ORDER BY conversation.id
 LIMIT 1`, schema, childSourceLiteral),
		request.ActorUserID, request.ParentExecutionID, request.IdempotencyKey,
	).Scan(&existing.ConversationUUID, &existing.InputSHA256, &existing.ExecutionID,
		&existing.ResponseMessageID, &existing.ParticipantID)
	switch {
	case err == nil:
		// A conversation whose start was never stamped (the attempt that made
		// it died, or its stamp failed) is answered as it is: the caller
		// starts it again, and the start's question id (derived from the
		// same key) makes that a replay of an admitted turn, never a second.
		if err := tx.Commit(ctx); err != nil {
			return localturn.ChildConversation{}, fmt.Errorf("local child turn: commit: %w", err)
		}
		return existing, nil
	case errors.Is(err, pgx.ErrNoRows):
		return r.createChildConversation(ctx, tx, schema, request)
	default:
		return localturn.ChildConversation{}, fmt.Errorf("local child turn: find: %w", err)
	}
}

func (r *LocalChildTurnsRepo) createChildConversation(
	ctx context.Context, tx pgx.Tx, schema string, request localturn.ChildConversationRequest,
) (localturn.ChildConversation, error) {
	meta, err := json.Marshal(map[string]any{
		"conversation_type": localturn.ChildConversationSource,
		// Hidden: Chats lists it only when asked (`hidden=only`).
		"is_hidden":            true,
		"parent_execution_id":  request.ParentExecutionID,
		"idempotency_key":      request.IdempotencyKey,
		"input_sha256":         request.InputSHA256,
		"child_application_id": request.ApplicationID,
		"child_version_id":     request.VersionID,
	})
	if err != nil {
		return localturn.ChildConversation{}, err
	}
	var conversationID int64
	created := localturn.ChildConversation{Created: true, InputSHA256: request.InputSHA256}
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_conversations (uuid, name, is_private, author_id, meta, source, created_at)
VALUES (gen_random_uuid(), $1, TRUE, $2, $3::jsonb, $4, now() AT TIME ZONE 'UTC')
RETURNING id, uuid::text`, schema),
		childConversationName(request.Name), request.ActorUserID, string(meta), localturn.ChildConversationSource,
	).Scan(&conversationID, &created.ConversationUUID); err != nil {
		return localturn.ChildConversation{}, fmt.Errorf("local child turn: create conversation: %w", err)
	}
	author, err := childParticipant(ctx, tx, schema, "user", map[string]any{"id": request.ActorUserID})
	if err != nil {
		return localturn.ChildConversation{}, err
	}
	if err := mapChildParticipant(ctx, tx, schema, conversationID, author, map[string]any{}); err != nil {
		return localturn.ChildConversation{}, err
	}
	agent, err := childParticipant(ctx, tx, schema, "application",
		map[string]any{"id": request.ApplicationID, "project_id": request.ProjectID})
	if err != nil {
		return localturn.ChildConversation{}, err
	}
	// The version goes on the MAPPING: the participant is shared across
	// conversations, the version is this conversation's.
	if err := mapChildParticipant(ctx, tx, schema, conversationID, agent, map[string]any{"version_id": request.VersionID}); err != nil {
		return localturn.ChildConversation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return localturn.ChildConversation{}, fmt.Errorf("local child turn: commit conversation: %w", err)
	}
	created.ParticipantID = agent
	return created, nil
}

// childParticipant is find-then-create: chat_participants has no unique key
// on (entity_name, entity_meta).
func childParticipant(ctx context.Context, tx pgx.Tx, schema, entityName string, entityMeta map[string]any) (int64, error) {
	encoded, err := json.Marshal(entityMeta)
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.chat_participants
 WHERE entity_name = $1::text
   AND entity_meta ->> 'id' IS NOT DISTINCT FROM $2::jsonb ->> 'id'
   AND entity_meta ->> 'project_id' IS NOT DISTINCT FROM $2::jsonb ->> 'project_id'
 ORDER BY id
 LIMIT 1`, schema), entityName, string(encoded)).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("local child turn: look up %s participant: %w", entityName, err)
	}
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), $1, $2::jsonb, '{}'::json)
RETURNING id`, schema), entityName, string(encoded)).Scan(&id); err != nil {
		return 0, fmt.Errorf("local child turn: create %s participant: %w", entityName, err)
	}
	return id, nil
}

func mapChildParticipant(ctx context.Context, tx pgx.Tx, schema string, conversationID, participantID int64, settings map[string]any) error {
	encoded, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at)
VALUES ($1, $2, $3::jsonb, now() AT TIME ZONE 'UTC')
ON CONFLICT (participant_id, conversation_id) DO NOTHING`, schema),
		conversationID, participantID, string(encoded)); err != nil {
		return fmt.Errorf("local child turn: map participant: %w", err)
	}
	return nil
}

func childConversationName(name string) string {
	if name == "" {
		name = "Nested agent"
	}
	if utf8.RuneCountInString(name) <= maxChildConversationName {
		return name
	}
	return string([]rune(name)[:maxChildConversationName])
}

func (r *LocalChildTurnsRepo) RecordChildStart(ctx context.Context, projectID int64, conversationUUID, executionID, responseMessageID string) error {
	if r == nil || r.pool == nil {
		return localturn.ErrUnavailable
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return localturn.ErrInvalid
	}
	if _, err := r.pool.Exec(ctx, fmt.Sprintf(`
UPDATE %s.chat_conversations
   SET meta = meta || jsonb_build_object('child_execution_id', $2::text, 'child_response_message_id', $3::uuid::text)
 WHERE uuid = $1::uuid AND source = $4`, schema),
		conversationUUID, executionID, responseMessageID, localturn.ChildConversationSource); err != nil {
		return fmt.Errorf("local child turn: record start: %w", err)
	}
	return nil
}

// DiscardChildConversation removes a refused child's conversation through
// [DiscardUnadmittedConversation]: guarded on the source and on the
// conversation holding no message group, so it can never remove a turn's
// transcript, and its participant mappings go first in the same transaction.
func (r *LocalChildTurnsRepo) DiscardChildConversation(ctx context.Context, projectID int64, conversationUUID string) {
	if r == nil || r.pool == nil || conversationUUID == "" {
		return
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return
	}
	if _, err := DiscardUnadmittedConversation(context.WithoutCancel(ctx), r.pool, schema, conversationUUID,
		localturn.ChildConversationSource); err != nil {
		slog.WarnContext(ctx, "local child turn: discard conversation failed", "err", err)
	}
}

// ReadChildTurn reads the response group the child's first execution
// answered in. A resumed turn keeps that group (its continuation runs under a
// new execution id, which `ExecutionID` reports).
func (r *LocalChildTurnsRepo) ReadChildTurn(
	ctx context.Context, projectID, actorUserID int64, parentExecutionID, childExecutionID string,
) (localturn.ChildTurnState, error) {
	if r == nil || r.pool == nil {
		return localturn.ChildTurnState{}, localturn.ErrUnavailable
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return localturn.ChildTurnState{}, localturn.ErrInvalid
	}
	var state localturn.ChildTurnState
	var groupID *int64
	var streaming, isError, authorization, outputLimit *bool
	var responseID, executionID, failure, text *string
	var interrupt []byte
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT conversation.uuid::text,
       conversation.meta ->> 'child_response_message_id',
       response.id,
       response.task_id,
       response.is_streaming,
       COALESCE(response.meta ->> 'is_error', 'false') = 'true',
       COALESCE(response.meta ->> 'error', ''),
       CASE WHEN jsonb_typeof(response.meta -> 'hitl_interrupt') = 'object'
            THEN (response.meta -> 'hitl_interrupt')::text END,
       response.meta ? 'authorization_requests',
       COALESCE(response.meta -> 'output_limit_reached' = 'true'::jsonb, FALSE),
       COALESCE((
           SELECT string_agg(item_text.content, '' ORDER BY item.order_index, item.id)
             FROM %[1]s.chat_message_items AS item
             JOIN %[1]s.chat_messages_text AS item_text ON item_text.id = item.id
            WHERE item.message_group_id = response.id
              AND item.item_type = 'text_message'
       ), '')
  FROM %[1]s.chat_conversations AS conversation
  LEFT JOIN %[1]s.chat_message_group AS response
    ON response.conversation_id = conversation.id
   -- uuid to uuid, so the group's unique uuid index applies (a text
   -- comparison of response.uuid::text scanned the groups). The stamp is
   -- written as a uuid (RecordChildStart); anything else matches nothing.
   AND response.uuid = CASE
           WHEN conversation.meta ->> 'child_response_message_id'
                ~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
           THEN (conversation.meta ->> 'child_response_message_id')::uuid
       END
 WHERE conversation.source = %[2]s
   AND conversation.author_id = $1
   AND conversation.meta ->> 'child_execution_id' = $2
   AND conversation.meta ->> 'parent_execution_id' = $3
 LIMIT 1`, schema, childSourceLiteral),
		actorUserID, childExecutionID, parentExecutionID,
	).Scan(&state.ConversationUUID, &responseID, &groupID, &executionID, &streaming, &isError,
		&failure, &interrupt, &authorization, &outputLimit, &text)
	if errors.Is(err, pgx.ErrNoRows) {
		return localturn.ChildTurnState{}, localturn.ErrChildTurnNotFound
	}
	if err != nil {
		return localturn.ChildTurnState{}, fmt.Errorf("local child turn: read: %w", err)
	}
	state.ResponseMessageID = childText(responseID)
	if groupID == nil {
		state.Removed = true
		state.ExecutionID = childExecutionID
		return state, nil
	}
	state.ExecutionID = childText(executionID)
	if state.ExecutionID == "" {
		state.ExecutionID = childExecutionID
	}
	state.Streaming = childBool(streaming)
	state.IsError = childBool(isError)
	state.Failure = childText(failure)
	if len(interrupt) > 0 {
		state.Interrupt = json.RawMessage(interrupt)
	}
	state.AuthorizationPending = childBool(authorization)
	state.OutputLimit = childBool(outputLimit)
	state.Text = childText(text)

	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
SELECT step.tool_name, step.is_error
  FROM %s.chat_message_trace_step AS step
 WHERE step.message_group_id = $1
   AND COALESCE(step.tool_name, '') <> ''
 ORDER BY step.id
 LIMIT $2`, schema), *groupID, maxChildToolCalls)
	if err != nil {
		return localturn.ChildTurnState{}, fmt.Errorf("local child turn: read tool calls: %w", err)
	}
	for rows.Next() {
		var call localturn.ChildToolCall
		if err := rows.Scan(&call.Name, &call.IsError); err != nil {
			rows.Close()
			return localturn.ChildTurnState{}, fmt.Errorf("local child turn: read tool call: %w", err)
		}
		state.ToolCalls = append(state.ToolCalls, call)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return localturn.ChildTurnState{}, fmt.Errorf("local child turn: read tool calls: %w", err)
	}
	state.Usage = r.childUsage(ctx, projectID, childExecutionID, state.ExecutionID)
	return state, nil
}

// childUsage sums the gateway's request log over the child's executions (the
// first and the current one). The log lives in another schema that a
// deployment may not have migrated: then the usage is left out, and the read
// still answers (an absent table is not a failed turn).
func (r *LocalChildTurnsRepo) childUsage(ctx context.Context, projectID int64, executionIDs ...string) *localturn.ChildUsage {
	var usage localturn.ChildUsage
	if err := r.pool.QueryRow(ctx, `
SELECT count(*)::bigint, COALESCE(sum(prompt_tokens), 0)::bigint, COALESCE(sum(completion_tokens), 0)::bigint
  FROM gateway.llm_request_logs
 WHERE project_id = $1 AND execution_id = ANY($2::text[])`, projectID, executionIDs,
	).Scan(&usage.LLMCalls, &usage.PromptTokens, &usage.CompletionTokens); err != nil {
		if ctx.Err() == nil {
			slog.DebugContext(ctx, "local child turn: usage not readable", "err", err)
		}
		return nil
	}
	return &usage
}

func childText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func childBool(value *bool) bool { return value != nil && *value }
