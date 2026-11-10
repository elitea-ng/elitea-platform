package repos

// The view each agent answer was given (ADR-0050 decision 9, issue 1669):
// the admission writes it with the answer, in the same transaction, and the
// "What the agent saw" inspector reads it back
// (internal/api/v2/contextviews). tenant/0166 declares the table.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// recordContextView writes the response's view, replacing an earlier run's
// (a regeneration). The transaction's search_path is the tenant schema
// (tenant.BindProject). A nil record writes nothing.
func recordContextView(ctx context.Context, tx pgx.Tx, responseMessageID string, view *agentexecutionapp.ContextViewRecord) error {
	if view == nil {
		return nil
	}
	entries := view.Entries
	if entries == nil {
		entries = []agentexecutionapp.ContextViewEntry{}
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("encode context view entries: %w", err)
	}
	var participantHistory *string
	if view.ParticipantHistory != "" {
		participantHistory = &view.ParticipantHistory
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO chat_message_context_views
    (message_group_uuid, reader_participant_id, history, include_side_talk,
     participant_history, window_tokens, digest, entries)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8::jsonb)
ON CONFLICT (message_group_uuid) DO UPDATE SET
    reader_participant_id = EXCLUDED.reader_participant_id,
    history = EXCLUDED.history,
    include_side_talk = EXCLUDED.include_side_talk,
    participant_history = EXCLUDED.participant_history,
    window_tokens = EXCLUDED.window_tokens,
    digest = EXCLUDED.digest,
    entries = EXCLUDED.entries,
    created_at = clock_timestamp()`,
		responseMessageID, view.ReaderParticipantID, string(view.Settings.History),
		string(view.Settings.SideTalk), participantHistory, int64(view.WindowTokens),
		view.Digest, encoded,
	); err != nil {
		return fmt.Errorf("record context view: %w", err)
	}
	return nil
}
