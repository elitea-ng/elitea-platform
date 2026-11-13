package repos

// The view each agent answer was given (ADR-0050 decision 9, issue 1669):
// the admission writes it with the answer, in the same transaction, and the
// "What the agent saw" inspector reads it back
// (internal/api/v2/contextviews). tenant/0167 declares the table; tenant/0169
// stores the entries as a frame chain (context_view_frames.go, issue 1893).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// recordContextView writes the response's view, replacing an earlier run's
// (a regeneration). The transaction's search_path is the tenant schema
// (tenant.BindProject). A nil record writes nothing.
//
// The entries go into a new frame: a delta against the reader's newest frame
// in the conversation (the view of its last recorded answer) other than the
// one this answer points to now, or a keyframe
// when the reader has none, the chain has reached
// contextViewKeyframeInterval, or the delta is not smaller than the list.
// Frames are immutable, so a regeneration in the middle of a chain writes a
// new frame and repoints its answer; the later frames built on the old one
// keep it. Writes in one conversation are serialised so a frame that becomes
// unreferenced can be deleted without racing a writer that builds on it.
func recordContextView(ctx context.Context, tx pgx.Tx, responseMessageID string, view *agentexecutionapp.ContextViewRecord) error {
	if view == nil {
		return nil
	}
	entries := view.Entries
	if entries == nil {
		entries = []agentexecutionapp.ContextViewEntry{}
	}
	full, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("encode context view entries: %w", err)
	}

	var conversationID *int64
	err = tx.QueryRow(ctx, `SELECT conversation_id FROM chat_message_group WHERE uuid = $1::uuid`,
		responseMessageID).Scan(&conversationID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("record context view: %w", err)
	}
	if conversationID == nil {
		// No answer row (or one outside any conversation): the upsert
		// reports it as the table's key would, with the full list.
		return upsertContextView(ctx, tx, responseMessageID, view, full, nil)
	}

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('chat_message_context_views:' || current_schema() || ':' || $1::bigint::text, 0))`,
		*conversationID); err != nil {
		return fmt.Errorf("lock context view chain: %w", err)
	}

	base, err := contextViewBase(ctx, tx, *conversationID, view.ReaderParticipantID, responseMessageID)
	if err != nil {
		return err
	}
	body, baseID, depth := full, (*int64)(nil), 0
	if base != nil && base.depth+1 < contextViewKeyframeInterval {
		baseEntries, err := readContextViewFrame(ctx, tx, "", base.id)
		if err != nil {
			return fmt.Errorf("record context view: %w", err)
		}
		delta, err := json.Marshal(encodeContextViewDelta(baseEntries, entries))
		if err != nil {
			return fmt.Errorf("encode context view delta: %w", err)
		}
		if len(delta) < len(full) {
			body, baseID, depth = delta, &base.id, base.depth+1
		}
	}
	var frameID int64
	if err := tx.QueryRow(ctx, `
INSERT INTO chat_message_context_view_frames
    (conversation_id, reader_participant_id, base_frame_id, depth, body, entry_count)
VALUES ($1, $2, $3, $4, $5::jsonb, $6)
RETURNING id`,
		*conversationID, view.ReaderParticipantID, baseID, depth, body, len(entries),
	).Scan(&frameID); err != nil {
		return fmt.Errorf("record context view frame: %w", err)
	}

	// The frame a regeneration replaces, collected once the answer points
	// at the new one.
	var previousFrame *int64
	err = tx.QueryRow(ctx, `SELECT frame_id FROM chat_message_context_views WHERE message_group_uuid = $1::uuid FOR UPDATE`,
		responseMessageID).Scan(&previousFrame)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("record context view: %w", err)
	}
	if err := upsertContextView(ctx, tx, responseMessageID, view, []byte(`[]`), &frameID); err != nil {
		return err
	}
	if previousFrame != nil {
		return collectContextViewFrames(ctx, tx, *previousFrame)
	}
	return nil
}

type contextViewBaseFrame struct {
	id    int64
	depth int
}

// contextViewBase is the frame a new view of reader in the conversation is
// encoded against: the reader's newest frame there, the frame the answer
// points to now excluded (a regeneration's replaced frame is then
// unreferenced unless a later frame built on it, and is collected). A reader
// with no frame
// yet whose views were written before tenant/0169 has its newest such row
// (by answer time, this answer's own excluded) moved into a keyframe, so the
// pre-upgrade row is the base and reads exactly as before. Nil means a
// keyframe.
func contextViewBase(ctx context.Context, tx pgx.Tx, conversationID, readerParticipantID int64, responseMessageID string) (*contextViewBaseFrame, error) {
	var base contextViewBaseFrame
	err := tx.QueryRow(ctx, `
SELECT frame.id, frame.depth FROM chat_message_context_view_frames frame
WHERE frame.conversation_id = $1 AND frame.reader_participant_id = $2
  AND NOT EXISTS (
      SELECT 1 FROM chat_message_context_views own
      WHERE own.message_group_uuid = $3::uuid AND own.frame_id = frame.id)
ORDER BY frame.id DESC LIMIT 1`, conversationID, readerParticipantID, responseMessageID).Scan(&base.id, &base.depth)
	if err == nil {
		return &base, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("find context view base: %w", err)
	}
	var legacy string
	err = tx.QueryRow(ctx, `
SELECT view.message_group_uuid::text
FROM chat_message_context_views view
JOIN chat_message_group answer ON answer.uuid = view.message_group_uuid
WHERE answer.conversation_id = $1
  AND view.reader_participant_id = $2
  AND view.frame_id IS NULL
  AND view.message_group_uuid <> $3::uuid
ORDER BY answer.created_at DESC, answer.id DESC
LIMIT 1
FOR UPDATE OF view`, conversationID, readerParticipantID, responseMessageID).Scan(&legacy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find context view base: %w", err)
	}
	// The row's own jsonb moves unchanged, so it decodes to what it held.
	if err := tx.QueryRow(ctx, `
WITH keyframe AS (
    INSERT INTO chat_message_context_view_frames
        (conversation_id, reader_participant_id, depth, body, entry_count)
    SELECT $1, $2, 0, view.entries, jsonb_array_length(view.entries)
    FROM chat_message_context_views view
    WHERE view.message_group_uuid = $3::uuid
    RETURNING id
)
UPDATE chat_message_context_views
SET frame_id = (SELECT id FROM keyframe), entries = '[]'::jsonb
WHERE message_group_uuid = $3::uuid
RETURNING frame_id`, conversationID, readerParticipantID, legacy).Scan(&base.id); err != nil {
		return nil, fmt.Errorf("move context view into a keyframe: %w", err)
	}
	return &base, nil
}

// upsertContextView writes the answer's row: the full list with no frame (no
// conversation to chain in), or an empty list and the frame.
func upsertContextView(ctx context.Context, tx pgx.Tx, responseMessageID string, view *agentexecutionapp.ContextViewRecord, entries []byte, frameID *int64) error {
	var participantHistory *string
	if view.ParticipantHistory != "" {
		participantHistory = &view.ParticipantHistory
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO chat_message_context_views
    (message_group_uuid, reader_participant_id, history, include_side_talk,
     participant_history, window_tokens, digest, entries, frame_id)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)
ON CONFLICT (message_group_uuid) DO UPDATE SET
    reader_participant_id = EXCLUDED.reader_participant_id,
    history = EXCLUDED.history,
    include_side_talk = EXCLUDED.include_side_talk,
    participant_history = EXCLUDED.participant_history,
    window_tokens = EXCLUDED.window_tokens,
    digest = EXCLUDED.digest,
    entries = EXCLUDED.entries,
    frame_id = EXCLUDED.frame_id,
    created_at = clock_timestamp()`,
		responseMessageID, view.ReaderParticipantID, string(view.Settings.History),
		string(view.Settings.SideTalk), participantHistory, int64(view.WindowTokens),
		view.Digest, entries, frameID,
	); err != nil {
		return fmt.Errorf("record context view: %w", err)
	}
	return nil
}

// collectContextViewFrames deletes frame id if no answer and no frame names
// it any more, then its base on the same terms, down the chain. A frame that
// is still a base stays: the views built on it decode through it. The caller
// holds the conversation's chain lock.
func collectContextViewFrames(ctx context.Context, tx pgx.Tx, id int64) error {
	for {
		var base *int64
		err := tx.QueryRow(ctx, `
DELETE FROM chat_message_context_view_frames frame
WHERE frame.id = $1
  AND NOT EXISTS (SELECT 1 FROM chat_message_context_views view WHERE view.frame_id = frame.id)
  AND NOT EXISTS (SELECT 1 FROM chat_message_context_view_frames child WHERE child.base_frame_id = frame.id)
RETURNING frame.base_frame_id`, id).Scan(&base)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && base == nil) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("collect context view frames: %w", err)
		}
		id = *base
	}
}
