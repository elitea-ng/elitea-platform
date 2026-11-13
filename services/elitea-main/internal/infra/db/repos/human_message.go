package repos

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
)

// HumanMessageRepo stores a person's chat message that starts no turn: a
// people-only message (application/agentexecution/people_message.go).
//
// It writes what a turn's admission writes for the question and nothing
// else: the sender's message group in the conversation, its text item and
// its attachment items, in one transaction under the conversation's row
// lock. There is no response group, no execution, no outbox row and no
// floor exchange, so nothing streams and nothing answers. The group is
// addressed to no agent (`sent_to_id` NULL, `addressed_to` empty); the
// people it tags are in its `meta`, as for a turn's question.
type HumanMessageRepo struct {
	pool *pgxpool.Pool
}

// NewHumanMessageRepo builds the repository over the admission pool.
func NewHumanMessageRepo(pool *pgxpool.Pool) *HumanMessageRepo {
	return &HumanMessageRepo{pool: pool}
}

// PostHumanMessage implements agentexecutionapp.HumanMessageStore.
func (repo *HumanMessageRepo) PostHumanMessage(ctx context.Context, message agentexecutionapp.HumanMessage) (bool, error) {
	if repo == nil || repo.pool == nil {
		return false, errors.New("human messages: no database")
	}
	conversationUUID, err := currentPGUUID(message.ConversationUUID)
	if err != nil {
		return false, executionapp.ErrInvalidAdmission
	}
	questionID, err := currentPGUUID(message.QuestionID)
	if err != nil {
		return false, executionapp.ErrInvalidAdmission
	}
	itemID, err := currentPGUUID(message.QuestionItemID)
	if err != nil {
		return false, executionapp.ErrInvalidAdmission
	}
	tx, err := repo.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return false, fmt.Errorf("begin human message: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err := tenant.BindProject(ctx, tx, tenant.Project{ID: message.ProjectID}); err != nil {
		return false, fmt.Errorf("bind human message project: %w", err)
	}
	queries := sqlcgen.New(tx)
	conversationID, err := queries.LockCurrentAgentConversation(ctx, conversationUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, agentexecutionapp.ErrUnsupportedCurrentAgentStart
	}
	if err != nil {
		return false, fmt.Errorf("lock human message conversation: %w", err)
	}
	// The sender, as a participant of this conversation (the same join a
	// turn's question uses): a person who is not in it cannot post to it.
	var authorID int32
	err = tx.QueryRow(ctx, `
SELECT participant.id
FROM chat_participant_mapping AS mapping
JOIN chat_participants AS participant ON participant.id = mapping.participant_id
WHERE mapping.conversation_id = $1
  AND participant.entity_name = 'user'
  AND (participant.entity_meta ->> 'id')::bigint = $2::bigint
LIMIT 1`, conversationID, message.AuthorUserID).Scan(&authorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, agentexecutionapp.ErrUnsupportedCurrentAgentStart
	}
	if err != nil {
		return false, fmt.Errorf("resolve human message author: %w", err)
	}
	// The message id is the idempotency key and the group's uuid, which is unique
	// across conversations: read it WITHOUT the conversation filter, so the same
	// id used elsewhere is a conflict here and not a unique violation (a 500) at
	// the insert. Only a replay of this very post by its sender stores nothing;
	// an id that started a turn (a reply group, an addressee) or belongs to
	// another sender or conversation is a conflict, never "posted, no turn".
	var existingAuthor, existingConversation int32
	var startedTurn bool
	err = tx.QueryRow(ctx, `
SELECT g.author_participant_id, g.conversation_id,
       g.sent_to_id IS NOT NULL OR EXISTS (SELECT 1 FROM chat_message_group AS reply WHERE reply.reply_to_id = g.id)
FROM chat_message_group AS g
WHERE g.uuid = $1::uuid`, questionID).Scan(&existingAuthor, &existingConversation, &startedTurn)
	switch {
	case err == nil:
		if existingAuthor != authorID || existingConversation != conversationID || startedTurn {
			return false, executionapp.ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit human message replay: %w", err)
		}
		committed = true
		return false, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("read human message replay: %w", err)
	}
	var groupID int32
	if err := tx.QueryRow(ctx, `
INSERT INTO chat_message_group (uuid, author_participant_id, conversation_id, meta, is_streaming, created_at,
                                addressed_to, visibility, cause)
VALUES ($1::uuid, $2, $3, $4::jsonb, FALSE, clock_timestamp() AT TIME ZONE 'UTC',
        '{}'::integer[], 'all', 'human_message')
RETURNING id`, questionID, authorID, conversationID, string(message.Meta)).Scan(&groupID); err != nil {
		return false, fmt.Errorf("insert human message: %w", err)
	}
	var textItemID int32
	if err := tx.QueryRow(ctx, `
INSERT INTO chat_message_items (uuid, item_type, order_index, meta, message_group_id, created_at)
VALUES ($1::uuid, 'text_message', 0, '{}'::jsonb, $2, now() AT TIME ZONE 'UTC')
RETURNING id`, itemID, groupID).Scan(&textItemID); err != nil {
		return false, fmt.Errorf("insert human message item: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO chat_messages_text (id, content) VALUES ($1, $2)`,
		textItemID, message.UserInput); err != nil {
		return false, fmt.Errorf("insert human message text: %w", err)
	}
	if err := insertCurrentTurnAttachments(ctx, queries, &groupID, message.Attachments); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit human message: %w", err)
	}
	committed = true
	return true, nil
}

var _ agentexecutionapp.HumanMessageStore = (*HumanMessageRepo)(nil)
