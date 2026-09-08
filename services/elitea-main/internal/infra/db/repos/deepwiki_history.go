package repos

// The DeepWiki wiki chat's transcript, in the ordinary tenant chat tables.
//
// It satisfies `internal/domain/wikichat.Store`, whose types live in their own
// package because this one is imported by the facade's dependencies — see that
// package's doc comment for the cycle it avoids.
//
// NO MIGRATION. migrations/tenant/0123 already declares every column this
// file writes: `chat_conversations.source`, `chat_participants.entity_name`,
// `chat_message_group.task_id`. A wiki chat is an ordinary conversation with
// two distinguishing marks — `source = 'deepwiki'` and a `toolkit`
// participant — and both are values, not schema.
//
// # WHY THE STATEMENTS INTERPOLATE THE SCHEMA
//
// `p_%d` is built from an int64 that the facade route already validated as a
// project id (providerhost/facade.ValidProjectID) and that `tenantschema`
// quotes as an identifier here again. No caller-supplied text reaches these
// statements; a value that is not a project id yields a schema that cannot
// exist, so the statement fails closed rather than reading another tenant.
//
// # WHY EVERY PREDICATE CARRIES author_id
//
// The conversation key comes from the browser. It is opaque and unguessable,
// but "unguessable" is not an authorisation rule — so every statement that
// resolves one also requires the row to belong to the caller. The worst a
// caller can do with somebody else's key is fail to match it and open a
// conversation of their own.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/wikichat"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// DeepWikiSource is the `chat_conversations.source` a wiki chat is filed
// under, and DeepWikiParticipantEntity the `entity_name` its toolkit takes
// part under. Both are what the drawer's listing filters by
// (`?source=deepwiki&entity_name=toolkit`), so they are constants rather than
// literals in five statements.
const (
	DeepWikiSource            = wikichat.Source
	DeepWikiParticipantEntity = wikichat.ParticipantEntity
)

// deepWikiConversationName bounds the name derived from the first question.
// `chat_conversations.name` is an unbounded varchar, but the chat list renders
// it in one line and the support assistant already refuses more than this.
const deepWikiConversationName = 255

// DeepWikiHistoryRepo writes wiki chat transcripts.
type DeepWikiHistoryRepo struct {
	pool *pgxpool.Pool
}

// NewDeepWikiHistoryRepo builds the repository, refusing one with no pool
// rather than returning a value whose every method answers an error.
func NewDeepWikiHistoryRepo(pool *pgxpool.Pool) (*DeepWikiHistoryRepo, error) {
	if pool == nil {
		return nil, errors.New("deepwiki history: a database pool is required")
	}
	return &DeepWikiHistoryRepo{pool: pool}, nil
}

// RecordQuestion opens or resumes the caller's wiki conversation and appends
// the question, in ONE transaction.
//
// The whole turn is one transaction because a conversation with no question,
// or a question whose participants were never mapped, is a row the drawer
// renders as an empty chat it cannot explain. Either the turn is there or the
// conversation was never opened.
func (r *DeepWikiHistoryRepo) RecordQuestion(ctx context.Context, question wikichat.Question) error {
	schema, err := tenantschema.QuoteInt(question.ProjectID)
	if err != nil {
		return fmt.Errorf("deepwiki history: project %d: %w", question.ProjectID, err)
	}
	if question.ChatKey == "" || question.InvocationID == "" || question.UserID <= 0 {
		return errors.New("deepwiki history: a conversation key, an invocation and a caller are required")
	}

	transaction, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("deepwiki history: begin: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// The lock serialises the whole turn against a second one for the SAME
	// conversation. Two questions asked at once would otherwise both miss the
	// conversation lookup and open two conversations under one key.
	if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`,
		deepWikiLockClass, lockKeyFor(question.ChatKey)); err != nil {
		return fmt.Errorf("deepwiki history: lock conversation: %w", err)
	}

	conversationID, err := r.resolveConversation(ctx, transaction, schema, question)
	if err != nil {
		return err
	}
	userParticipant, err := r.participant(ctx, transaction, schema, "user",
		map[string]any{"id": question.UserID},
		map[string]any{})
	if err != nil {
		return err
	}
	toolkitParticipant, err := r.participant(ctx, transaction, schema, DeepWikiParticipantEntity,
		map[string]any{"id": question.ToolkitID, "project_id": question.ProjectID},
		map[string]any{"name": question.ToolkitName})
	if err != nil {
		return err
	}
	for _, participantID := range []int64{userParticipant, toolkitParticipant} {
		if _, err := transaction.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participant_mapping (conversation_id, participant_id, entity_settings)
VALUES ($1, $2, '{}'::jsonb)
ON CONFLICT (participant_id, conversation_id) DO NOTHING`, schema),
			conversationID, participantID); err != nil {
			return fmt.Errorf("deepwiki history: map participant: %w", err)
		}
	}

	// task_id is the invocation. It is what the answer's tee looks the turn
	// up by, and it is why a repeated poll cannot write a second answer.
	meta, err := json.Marshal(map[string]any{
		"source":     DeepWikiSource,
		"capability": question.Capability,
		"toolkit_id": question.ToolkitID,
	})
	if err != nil {
		return fmt.Errorf("deepwiki history: encode question meta: %w", err)
	}
	var groupID int64
	if err := transaction.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_message_group
    (uuid, author_participant_id, conversation_id, sent_to_id, meta, is_streaming, created_at, task_id)
VALUES (gen_random_uuid(), $1, $2, $3, $4::jsonb, FALSE, clock_timestamp(), $5)
RETURNING id`, schema),
		userParticipant, conversationID, toolkitParticipant, string(meta), question.InvocationID,
	).Scan(&groupID); err != nil {
		return fmt.Errorf("deepwiki history: insert question group: %w", err)
	}
	if err := insertTextItem(ctx, transaction, schema, groupID, question.Question); err != nil {
		return err
	}

	// The conversation's own clock, so the drawer's "most recent first"
	// listing follows the conversation rather than the row's creation.
	if _, err := transaction.Exec(ctx, fmt.Sprintf(
		`UPDATE %s.chat_conversations SET updated_at = clock_timestamp() WHERE id = $1`, schema),
		conversationID); err != nil {
		return fmt.Errorf("deepwiki history: touch conversation: %w", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("deepwiki history: commit question: %w", err)
	}
	return nil
}

// RecordAnswer appends the provider's answer to the turn one invocation
// opened, and reports whether this call wrote it.
//
// IDEMPOTENT UNDER REPEATED AND CONCURRENT POLLS, which is not optional: the
// browser polls on an interval and nothing stops it reaching the terminal
// payload more than once — a slow render, a second tab, a refocus. The
// question group's `task_id` is the key, the reply is the fact, and the
// advisory lock is what makes "look, then insert" atomic. A plain
// `WHERE NOT EXISTS` would let two simultaneous polls both find nothing.
func (r *DeepWikiHistoryRepo) RecordAnswer(ctx context.Context, answer wikichat.Answer) (bool, error) {
	schema, err := tenantschema.QuoteInt(answer.ProjectID)
	if err != nil {
		return false, fmt.Errorf("deepwiki history: project %d: %w", answer.ProjectID, err)
	}
	if answer.InvocationID == "" || answer.UserID <= 0 {
		return false, errors.New("deepwiki history: an invocation and a caller are required")
	}

	transaction, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("deepwiki history: begin: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`,
		deepWikiLockClass, lockKeyFor(answer.InvocationID)); err != nil {
		return false, fmt.Errorf("deepwiki history: lock invocation: %w", err)
	}

	// The question this invocation opened, in the caller's OWN wiki
	// conversation. A caller who names an invocation belonging to somebody
	// else's conversation matches nothing.
	var questionGroup, conversationID, toolkitParticipant int64
	err = transaction.QueryRow(ctx, fmt.Sprintf(`
SELECT mg.id, mg.conversation_id, mg.sent_to_id
FROM %s.chat_message_group mg
JOIN %s.chat_conversations c ON c.id = mg.conversation_id
WHERE mg.task_id = $1
  AND mg.sent_to_id IS NOT NULL
  AND c.source = $2
  AND c.author_id = $3
ORDER BY mg.id
LIMIT 1`, schema, schema), answer.InvocationID, DeepWikiSource, answer.UserID).
		Scan(&questionGroup, &conversationID, &toolkitParticipant)
	if errors.Is(err, pgx.ErrNoRows) {
		// No question to reply to. That is the ordinary state of a poll on an
		// invocation this platform never recorded — an older conversation, or
		// a caller that sent no conversation key — and not a failure.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("deepwiki history: resolve question: %w", err)
	}

	var existing int64
	err = transaction.QueryRow(ctx, fmt.Sprintf(
		`SELECT id FROM %s.chat_message_group WHERE reply_to_id = $1 LIMIT 1`, schema),
		questionGroup).Scan(&existing)
	if err == nil {
		return false, nil // already written by an earlier poll
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("deepwiki history: read existing answer: %w", err)
	}

	meta, err := json.Marshal(map[string]any{
		"source":   DeepWikiSource,
		"is_error": answer.IsError,
	})
	if err != nil {
		return false, fmt.Errorf("deepwiki history: encode answer meta: %w", err)
	}
	var groupID int64
	if err := transaction.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_message_group
    (uuid, author_participant_id, conversation_id, reply_to_id, meta, is_streaming, created_at, task_id)
VALUES (gen_random_uuid(), $1, $2, $3, $4::jsonb, FALSE, clock_timestamp(), $5)
RETURNING id`, schema),
		toolkitParticipant, conversationID, questionGroup, string(meta), answer.InvocationID,
	).Scan(&groupID); err != nil {
		return false, fmt.Errorf("deepwiki history: insert answer group: %w", err)
	}
	if err := insertTextItem(ctx, transaction, schema, groupID, answer.Content); err != nil {
		return false, err
	}
	if _, err := transaction.Exec(ctx, fmt.Sprintf(
		`UPDATE %s.chat_conversations SET updated_at = clock_timestamp() WHERE id = $1`, schema),
		conversationID); err != nil {
		return false, fmt.Errorf("deepwiki history: touch conversation: %w", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, fmt.Errorf("deepwiki history: commit answer: %w", err)
	}
	return true, nil
}

// resolveConversation finds the caller's conversation for this key, or opens
// one.
//
// The meta document is written HERE and never accepted from a client, for the
// reason the support assistant gives: a client that could choose
// `is_hidden: false` could publish its own wiki transcript into the project's
// ordinary chat list. `single_participant` is the shape
// `conversations.Handler.List` filters on — it is what makes
// `?entity_name=toolkit&entity_meta_id={id}` select this toolkit's chats.
func (r *DeepWikiHistoryRepo) resolveConversation(
	ctx context.Context, transaction pgx.Tx, schema string, question wikichat.Question,
) (int64, error) {
	var conversationID int64
	err := transaction.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.chat_conversations
WHERE source = $1 AND author_id = $2 AND meta->>'wiki_chat_key' = $3
ORDER BY id
LIMIT 1`, schema), DeepWikiSource, question.UserID, question.ChatKey).Scan(&conversationID)
	if err == nil {
		return conversationID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("deepwiki history: resolve conversation: %w", err)
	}

	meta, err := json.Marshal(map[string]any{
		"is_hidden":         true,
		"conversation_type": DeepWikiSource,
		"wiki_chat_key":     question.ChatKey,
		"single_participant": map[string]any{
			"entity_name": DeepWikiParticipantEntity,
			"entity_meta": map[string]any{
				"id":         question.ToolkitID,
				"project_id": question.ProjectID,
				"name":       question.ToolkitName,
			},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("deepwiki history: encode conversation meta: %w", err)
	}
	if err := transaction.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_conversations (uuid, name, is_private, author_id, meta, source)
VALUES (gen_random_uuid(), $1, TRUE, $2, $3::jsonb, $4)
RETURNING id`, schema),
		conversationTitle(question.Question), question.UserID, string(meta), DeepWikiSource,
	).Scan(&conversationID); err != nil {
		return 0, fmt.Errorf("deepwiki history: create conversation: %w", err)
	}
	return conversationID, nil
}

// participant finds or creates one chat_participants row.
//
// Find THEN create, which is the order ConversationsRepo.AddParticipant had to
// be corrected to: the table has no unique key over (entity_name,
// entity_meta), so an insert-first approach cannot fail and the lookup after
// it is unreachable — which is how one agent came to have two participant
// rows in one conversation. Here the enclosing advisory lock covers the race
// the mapping's ON CONFLICT cannot.
func (r *DeepWikiHistoryRepo) participant(
	ctx context.Context, transaction pgx.Tx, schema, entityName string,
	entityMeta, displayMeta map[string]any,
) (int64, error) {
	encodedEntity, err := json.Marshal(entityMeta)
	if err != nil {
		return 0, fmt.Errorf("deepwiki history: encode participant identity: %w", err)
	}
	var participantID int64
	err = transaction.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.chat_participants
WHERE entity_name = $1
  AND entity_meta->>'id' IS NOT DISTINCT FROM $2::jsonb->>'id'
  AND entity_meta->>'project_id' IS NOT DISTINCT FROM $2::jsonb->>'project_id'
ORDER BY id
LIMIT 1`, schema), entityName, string(encodedEntity)).Scan(&participantID)
	if err == nil {
		return participantID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("deepwiki history: resolve participant: %w", err)
	}

	encodedDisplay, err := json.Marshal(displayMeta)
	if err != nil {
		return 0, fmt.Errorf("deepwiki history: encode participant meta: %w", err)
	}
	if err := transaction.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), $1, $2::jsonb, $3::json)
RETURNING id`, schema), entityName, string(encodedEntity), string(encodedDisplay)).
		Scan(&participantID); err != nil {
		return 0, fmt.Errorf("deepwiki history: create participant: %w", err)
	}
	return participantID, nil
}

// insertTextItem writes one group's single `text_message` item and its
// payload. Two rows in one statement, because chat_messages_text's primary
// key IS the item's id (0123 shares it so the discriminator and the payload
// cannot disagree), so the payload insert must see the id the item insert
// generated.
func insertTextItem(
	ctx context.Context, transaction pgx.Tx, schema string, groupID int64, content string,
) error {
	if _, err := transaction.Exec(ctx, fmt.Sprintf(`
WITH item AS (
    INSERT INTO %s.chat_message_items (uuid, item_type, order_index, meta, message_group_id)
    VALUES (gen_random_uuid(), 'text_message', 0, '{}'::jsonb, $1)
    RETURNING id
)
INSERT INTO %s.chat_messages_text (id, content)
SELECT item.id, $2 FROM item`, schema, schema), groupID, content); err != nil {
		return fmt.Errorf("deepwiki history: insert text item: %w", err)
	}
	return nil
}

// conversationTitle names a new conversation after the question that opened
// it, which is what the ordinary chat list does and what makes a list of wiki
// chats readable at all. A blank question yields the same default the support
// assistant uses.
func conversationTitle(question string) string {
	trimmed := strings.TrimSpace(question)
	if trimmed == "" {
		return "New conversation"
	}
	// Runes, not bytes: cutting a multi-byte character in half writes invalid
	// UTF-8 into a text column.
	runes := []rune(trimmed)
	if len(runes) > deepWikiConversationName {
		return string(runes[:deepWikiConversationName])
	}
	return trimmed
}

// deepWikiLockClass is this feature's half of the two-integer advisory lock
// key space. It is an arbitrary fixed constant; the only requirement is that
// every replica uses the same one and no other feature uses it.
const deepWikiLockClass int32 = 0x0DEE0417

// lockKeyFor hashes an opaque identifier into the second half of the key.
//
// A hash collision costs two unrelated turns a moment of serialisation and
// nothing else — the statements inside the lock are still correct on their
// own predicates, so the lock is a race narrower, not the authorisation.
func lockKeyFor(identifier string) int32 {
	digest := fnv.New32a()
	_, _ = digest.Write([]byte(identifier))
	return int32(digest.Sum32()) //nolint:gosec // a hash bucket, not a number
}
