package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/chatauthority"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// The conversation archive (client contract 1.10). The archive is the
// project's, as the conversation pin is: one row per archived conversation
// in the tenant table chat_conversation_archives (tenant/0151), naming the
// member who archived it. Its foreign key cascades, so a deleted
// conversation takes its archive row with it.
var _ conversations.ConversationArchiver = (*ConversationsRepo)(nil)

// conversationArchiveTimeout bounds one archive or unarchive transaction.
const conversationArchiveTimeout = 5 * time.Second

// ArchiveConversation archives the conversation in its project.
//
// The visibility check, the row lock and the write are one statement, under
// the chat authority's DETAIL rule (the rule of getConversation and of the
// conversation pin): a conversation the caller cannot open answers 404, and
// nothing is written. A repeated archive, by anyone, keeps the FIRST
// archive (its archived_by and archived_at) and stamps nothing.
//
// A NEW archive stamps the conversation's sync_at in the same transaction
// (stampConversationPin), so the `changes_since` delta re-delivers the row to
// every member who can see it, as a pin does.
//
// FOR NO KEY UPDATE OF c, as pinConversation: Delete takes FOR UPDATE, so a
// concurrent delete either came first (the row is gone: 404) or waits for
// this commit and then cascades the new archive row away.
func (r *ConversationsRepo) ArchiveConversation(ctx context.Context, projectID, conversationID string) error {
	return r.writeConversationArchive(ctx, projectID, conversationID, true, `
	 written AS (
	  INSERT INTO %[1]s.chat_conversation_archives (conversation_id, archived_by, archived_at)
	  SELECT id, $%[2]d::bigint, (now() AT TIME ZONE 'UTC') FROM visible
	  ON CONFLICT (conversation_id) DO NOTHING
	  RETURNING 1)`)
}

// UnarchiveConversation removes the project's archive of the conversation,
// whichever member archived it, as unpinEntity removes a pin. Access and
// idempotency are ArchiveConversation's; only a removal stamps.
func (r *ConversationsRepo) UnarchiveConversation(ctx context.Context, projectID, conversationID string) error {
	return r.writeConversationArchive(ctx, projectID, conversationID, false, `
	 written AS (
	  DELETE FROM %[1]s.chat_conversation_archives a
	  USING visible
	  WHERE a.conversation_id = visible.id
	  RETURNING 1)`)
}

// writeConversationArchive runs one archive write. write is the `written`
// CTE: %[1]s is the tenant schema and, when bindsActor is set, $%[2]d the
// caller's user id; it reads the `visible` CTE, which holds the
// conversation's id when the caller may see it.
func (r *ConversationsRepo) writeConversationArchive(ctx context.Context, projectID, conversationID string, bindsActor bool, write string) error {
	if r.pool == nil {
		return fmt.Errorf("conversation archive storage is unavailable")
	}
	s, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	identity, ok := idPredicate(conversationID)
	if !ok {
		return apierr.NotFound("chat resource not found")
	}
	ctx, cancel := context.WithTimeout(ctx, conversationArchiveTimeout)
	defer cancel()
	access, err := chatauthority.Load(ctx, r.pool, projectID)
	if err != nil {
		return err
	}
	// $1 is the conversation id (idPredicate), then the authority's actor.
	visible, args := access.Predicate(s, "c", 2, chatauthority.Detail)
	actorParam := 2 + len(args)
	written := fmt.Sprintf(write, s, actorParam)
	query := fmt.Sprintf(`WITH visible AS (
	  SELECT c.id FROM %s.chat_conversations c WHERE %s AND %s FOR NO KEY UPDATE OF c
	 ),`, s, identity, visible) + written + `
	 SELECT (SELECT id FROM visible), EXISTS (SELECT 1 FROM written)`
	params := append([]any{conversationID}, args...)
	if bindsActor {
		params = append(params, access.ActorID)
	}

	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var id *int32
		var changed bool
		if err := tx.QueryRow(ctx, query, params...).Scan(&id, &changed); err != nil {
			return fmt.Errorf("conversations: archive: %w", err)
		}
		if id == nil {
			return apierr.NotFound("chat resource not found")
		}
		if !changed {
			return nil
		}
		return stampConversationPin(ctx, tx, s, *id)
	})
}
