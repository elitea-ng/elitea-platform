package repos

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/chatactivity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/chatauthority"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// The per-user read marker (client contract 1.12, markConversationRead), in
// the tenant table chat_conversation_reads (tenant/0153). Its foreign key
// cascades, so a deleted conversation takes its markers with it.
var _ conversations.ConversationReadMarker = (*ConversationsRepo)(nil)

// conversationReadTimeout bounds one read-marker transaction.
const conversationReadTimeout = 5 * time.Second

// MarkConversationRead moves the caller's read marker of the conversation.
//
// The visibility check is the chat authority's DETAIL rule (the rule of
// getConversation): a conversation the caller cannot open answers 404 and
// nothing is written. Only read access is needed, because the marker is the
// caller's own state.
//
// messageID (a numeric message group id or its UUID) reads up to that
// message: the marker moves to the message group's last write. Empty reads
// up to now. A message that is not in the conversation answers 404. The
// marker never moves back: an older message after a newer one keeps the
// newer marker, so a repeated or late request changes nothing.
//
// The marker does not stamp the conversation's sync_at: it is not a change
// for the other members.
func (r *ConversationsRepo) MarkConversationRead(ctx context.Context, projectID, conversationID, messageID string) (conversations.ReadState, error) {
	if r.pool == nil {
		return conversations.ReadState{}, fmt.Errorf("conversation read storage is unavailable")
	}
	s, err := tenantSchema(projectID)
	if err != nil {
		return conversations.ReadState{}, err
	}
	identity, ok := idPredicate(conversationID)
	if !ok {
		return conversations.ReadState{}, apierr.NotFound("chat resource not found")
	}
	var messagePredicate string
	switch {
	case messageID == "":
	case isNumericRowID(messageID):
		messagePredicate = "g.id = $2::bigint"
	case validUUIDText(messageID):
		messagePredicate = "g.uuid = $2::uuid"
	default:
		return conversations.ReadState{}, apierr.NotFound("message not found")
	}
	ctx, cancel := context.WithTimeout(ctx, conversationReadTimeout)
	defer cancel()
	access, err := chatauthority.Load(ctx, r.pool, projectID)
	if err != nil {
		return conversations.ReadState{}, err
	}
	visible, args := access.Predicate(s, "c", 2, chatauthority.Detail)

	var state conversations.ReadState
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// FOR KEY SHARE: a concurrent delete waits, then cascades the marker.
		var id int64
		var uuid string
		err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT c.id, COALESCE(c.uuid::text, '') FROM %s.chat_conversations c
			WHERE %s AND %s FOR KEY SHARE OF c`, s, identity, visible),
			append([]any{conversationID}, args...)...).Scan(&id, &uuid)
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.NotFound("chat resource not found")
		}
		if err != nil {
			return fmt.Errorf("conversations: read marker: conversation: %w", err)
		}

		var messageGroupID *int64
		var readAt time.Time
		if messagePredicate == "" {
			err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT clock_timestamp() AT TIME ZONE 'UTC',
				(SELECT g.id FROM %s.chat_message_group g WHERE g.conversation_id = $1
				  ORDER BY g.created_at DESC, g.id DESC LIMIT 1)`, s), id).Scan(&readAt, &messageGroupID)
		} else {
			var groupID int64
			err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT g.id, COALESCE(g.updated_at, g.created_at)
				FROM %s.chat_message_group g WHERE g.conversation_id = $1 AND %s`, s, messagePredicate),
				id, messageID).Scan(&groupID, &readAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return apierr.NotFound("message not found")
			}
			messageGroupID = &groupID
		}
		if err != nil {
			return fmt.Errorf("conversations: read marker: point: %w", err)
		}

		var lastRead *int64
		if err := tx.QueryRow(ctx, fmt.Sprintf(`INSERT INTO %[1]s.chat_conversation_reads AS reads
				(conversation_id, user_id, read_at, last_read_message_id)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (conversation_id, user_id) DO UPDATE SET
				read_at = GREATEST(reads.read_at, EXCLUDED.read_at),
				last_read_message_id = CASE WHEN EXCLUDED.read_at >= reads.read_at
					THEN COALESCE(EXCLUDED.last_read_message_id, reads.last_read_message_id)
					ELSE reads.last_read_message_id END
			RETURNING read_at, last_read_message_id`, s),
			id, access.ActorID, readAt, messageGroupID).Scan(&readAt, &lastRead); err != nil {
			return fmt.Errorf("conversations: read marker: write: %w", err)
		}

		activity, err := chatactivity.Load(ctx, tx, s, access.ActorID, []int64{id})
		if err != nil {
			return err
		}
		state = conversations.ReadState{
			ConversationID:    id,
			ConversationUUID:  uuid,
			ReaderID:          access.ActorID,
			ReadAt:            chatactivity.FormatTime(readAt),
			LastReadMessageID: lastRead,
			Unread:            activity[id].Unread,
		}
		return nil
	})
	if err != nil {
		return conversations.ReadState{}, err
	}
	return state, nil
}

// validUUIDText reports whether text parses as a UUID, so a non-UUID message
// id answers 404 instead of a cast error.
func validUUIDText(text string) bool {
	_, err := uuid.Parse(text)
	return err == nil
}
