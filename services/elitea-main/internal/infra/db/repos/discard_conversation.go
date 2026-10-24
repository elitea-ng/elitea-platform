package repos

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TxBeginner is the slice of a pgx pool [DiscardUnadmittedConversation] needs.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// DiscardUnadmittedConversation removes a conversation that was created for a
// turn the runtime then refused to admit (a desktop child turn, an MCP agent
// call, a pipeline trigger run). It reports whether a row was removed.
//
// It is guarded on the source and on the conversation holding no message
// group, so it can never remove a person's chat or a turn's transcript.
//
// One transaction: the conversation row is locked under both guards, its
// participant mappings go first, then the row itself. Until tenant 0157
// chat_participant_mapping's key to chat_conversations did not cascade, and a
// bare DELETE of the conversation failed with 23503 and left it behind; the
// explicit delete stays so the sync journal is the one
// ConversationsRepo.Delete writes.
// The other tables that name a conversation (selected, archive, reads,
// folders) cascade.
//
// schema must already be a quoted tenant schema identifier.
func DiscardUnadmittedConversation(
	ctx context.Context, db TxBeginner, schema, conversationUUID, source string,
) (bool, error) {
	if db == nil || conversationUUID == "" {
		return false, nil
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Sync cascade (tenant 0144), as ConversationsRepo.Delete runs: the
	// conversation's own tombstone means "drop every message", so the sync
	// triggers skip the parent bumps and the access_orphaned marker the
	// mapping deletes would otherwise journal. The conversation tombstone and
	// the per-user access_participant rows are still written: a run
	// conversation is listed in the ordinary chat list (only a local child is
	// hidden), so a client may have synced it, and those rows are who a
	// private conversation's deletion is shown to.
	if _, err := tx.Exec(ctx, `SELECT set_config('elitea.sync_cascade', 'conversation', true)`); err != nil {
		return false, fmt.Errorf("sync cascade: %w", err)
	}
	var conversationID int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.chat_conversations
 WHERE uuid = $1::uuid AND source = $2
   FOR UPDATE`, schema), conversationUUID, source).Scan(&conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock: %w", err)
	}
	// Checked in a statement of its own, after the lock: a group insert holds
	// a key share on the row, so by now every one is committed (and visible to
	// this new snapshot) or waits for this transaction.
	var holdsTurn bool
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT EXISTS (SELECT 1 FROM %s.chat_message_group WHERE conversation_id = $1)`, schema),
		conversationID).Scan(&holdsTurn); err != nil {
		return false, fmt.Errorf("check groups: %w", err)
	}
	if holdsTurn {
		return false, nil
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.chat_participant_mapping WHERE conversation_id = $1`, schema),
		conversationID); err != nil {
		return false, fmt.Errorf("delete participant mapping: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.chat_conversations WHERE id = $1`, schema),
		conversationID); err != nil {
		return false, fmt.Errorf("delete conversation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}
