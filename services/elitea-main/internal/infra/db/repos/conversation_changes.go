package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/changesync"
)

var _ conversations.MessageChangesLister = (*ConversationsRepo)(nil)

// ListMessageChanges is the transcript delta (`changes_since`, ADR-0025 WP6):
// the message groups of one conversation that changed after the cursor, in
// the legacy item shape, and tombstones for the groups deleted since. The
// stamps and tombstones are written by tenant migration 0144's triggers, so
// every writer of the chat tables is covered. The caller has already been
// authorised on the conversation (Handler.authorizeConversation).
func (r *ConversationsRepo) ListMessageChanges(ctx context.Context, projectID, conversationID, changesSince string, limit int) (conversations.MessageChanges, error) {
	s := schema(projectID)
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return conversations.MessageChanges{}, err
	}
	cursor, err := changesync.Decode(changesSince, changesync.StreamMessages, conversations.MessageScope(projectID, id))
	if err != nil {
		return conversations.MessageChanges{}, err
	}
	if limit < 1 || limit > changesync.MaxLimit {
		limit = changesync.DefaultLimit
	}

	var dbNow time.Time
	if err := r.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		return conversations.MessageChanges{}, fmt.Errorf("conversations: sync clock: %w", err)
	}
	if err := cursor.CheckExpiry(dbNow); err != nil {
		return conversations.MessageChanges{}, err
	}
	bound := changesync.SettleBound(dbNow)

	page := conversations.MessageChanges{Items: []conversations.Message{}, Tombstones: []changesync.Tombstone{}}
	if err := r.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %s.chat_message_group mg WHERE mg.conversation_id = $1`, s), id,
	).Scan(&page.Total); err != nil {
		return conversations.MessageChanges{}, fmt.Errorf("conversations: count messages: %w", err)
	}

	rowsStart := cursor.RowsStart()
	rows, err := r.pool.Query(ctx, messageSelectSQL(s, true)+`
		WHERE mg.conversation_id = $1 AND (mg.sync_at, mg.id) > ($2::timestamptz, $3::bigint)
		ORDER BY mg.sync_at, mg.id
		LIMIT $4`, id, rowsStart.At, rowsStart.ID, limit+1)
	if err != nil {
		return conversations.MessageChanges{}, fmt.Errorf("conversations: list message changes: %w", err)
	}
	items, positions, err := r.scanMessageRows(ctx, s, rows, true)
	if err != nil {
		return conversations.MessageChanges{}, err
	}
	rowsTruncated := len(items) > limit
	if rowsTruncated {
		items, positions = items[:limit], positions[:limit]
	}
	page.Items = items

	tombsStart := cursor.TombstonesStart(bound)
	tombRows, err := r.pool.Query(ctx, fmt.Sprintf(`
		SELECT id, entity_id, entity_uuid::text, deleted_at
		FROM %s.chat_sync_tombstones
		WHERE kind = 'message_group' AND conversation_id = $1
		  AND (deleted_at, id) > ($2::timestamptz, $3::bigint)
		ORDER BY deleted_at, id
		LIMIT $4`, s), id, tombsStart.At, tombsStart.ID, limit+1)
	if err != nil {
		return conversations.MessageChanges{}, fmt.Errorf("conversations: list message tombstones: %w", err)
	}
	defer tombRows.Close()
	tombPositions := []changesync.Position{}
	for tombRows.Next() {
		var tombID, groupID int64
		var uuid *string
		var deletedAt time.Time
		if err := tombRows.Scan(&tombID, &groupID, &uuid, &deletedAt); err != nil {
			return conversations.MessageChanges{}, fmt.Errorf("conversations: scan message tombstone: %w", err)
		}
		page.Tombstones = append(page.Tombstones, changesync.Tombstone{
			ID: groupID, UUID: uuid, Reason: changesync.ReasonDeleted, DeletedAt: changesync.FormatTime(deletedAt),
		})
		tombPositions = append(tombPositions, changesync.Position{At: deletedAt, ID: tombID})
	}
	if err := tombRows.Err(); err != nil {
		return conversations.MessageChanges{}, fmt.Errorf("conversations: list message tombstones: %w", err)
	}
	tombsTruncated := len(page.Tombstones) > limit
	if tombsTruncated {
		page.Tombstones, tombPositions = page.Tombstones[:limit], tombPositions[:limit]
	}

	nextRows, moreRows := changesync.Advance(rowsStart, lastSyncPosition(positions), rowsTruncated, bound)
	nextTombs, moreTombs := changesync.Advance(tombsStart, lastSyncPosition(tombPositions), tombsTruncated, bound)
	page.NextCursor = changesync.Encode(changesync.Cursor{
		Stream: cursor.Stream, Scope: cursor.Scope, Rows: nextRows, Tombs: nextTombs,
	})
	page.HasMore = moreRows || moreTombs
	return page, nil
}

func lastSyncPosition(positions []changesync.Position) *changesync.Position {
	if len(positions) == 0 {
		return nil
	}
	last := positions[len(positions)-1]
	return &last
}
