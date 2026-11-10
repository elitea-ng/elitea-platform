package repos

// The "What the agent saw" read (ADR-0050 decision 9, issue 1669) over
// tenant/0166's chat_message_context_views: the view one agent answer was
// given, its entries labelled with their authors, and the answer's own
// context measurement. agent_context_view.go writes the row.
//
// What it never returns: a message body (the client shows the text of the
// messages it already holds), a tool call, a tool result or a thinking step
// (none is in a view: agent_chat.sql projects visible message items only),
// and any view but the answer's own reader's. There is no parameter that
// names a reader: the row is keyed by the answer.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// ContextViews reads the recorded views.
type ContextViews struct {
	pool *pgxpool.Pool
}

// NewContextViews builds the store. A nil pool answers nil.
func NewContextViews(pool *pgxpool.Pool) *ContextViews {
	if pool == nil {
		return nil
	}
	return &ContextViews{pool: pool}
}

// ContextViewEntry is one projected message of a view.
type ContextViewEntry struct {
	MessageID string `json:"message_id"`
	// Reason is why the message is not in the view; nil when it is.
	Reason *string `json:"reason"`
	// AuthorName and AuthorKind are the message's author; nil when the
	// message was deleted after the turn.
	AuthorName *string    `json:"author_name"`
	AuthorKind *string    `json:"author_kind"`
	CreatedAt  *time.Time `json:"created_at"`
}

// ContextViewCompaction is the answer's last context measurement
// (meta.runtime_context): whether the reader's session was compacted.
type ContextViewCompaction struct {
	Phase                   string `json:"phase"`
	EstimatedInputTokens    int64  `json:"estimated_input_tokens"`
	UsableInputTokens       int64  `json:"usable_input_tokens"`
	CompactionTriggerTokens int64  `json:"compaction_trigger_tokens"`
}

// ContextView is the recorded view of one answer.
type ContextView struct {
	MessageID           string                 `json:"message_id"`
	ReaderParticipantID int64                  `json:"reader_participant_id"`
	ReaderName          *string                `json:"reader_name"`
	History             string                 `json:"history"`
	IncludeSideTalk     string                 `json:"include_side_talk"`
	ParticipantHistory  *string                `json:"participant_history"`
	WindowTokens        int64                  `json:"window_tokens"`
	Digest              string                 `json:"digest"`
	RecordedAt          time.Time              `json:"recorded_at"`
	Entries             []ContextViewEntry     `json:"entries"`
	Compaction          *ContextViewCompaction `json:"compaction"`
}

// MessageContextView answers the view recorded for the answer messageUUID.
// The caller has already authorised the message (AuthorizeChatResource);
// an answer without a record, or written by another participant than the
// record's reader, is a 404.
func (s *ContextViews) MessageContextView(ctx context.Context, projectID, messageUUID string) (ContextView, error) {
	if s == nil || s.pool == nil {
		return ContextView{}, fmt.Errorf("context view storage is unavailable")
	}
	tenant, err := tenantSchema(projectID)
	if err != nil {
		return ContextView{}, apierr.NotFound("context view not found")
	}
	if _, parseErr := uuid.Parse(messageUUID); parseErr != nil {
		return ContextView{}, apierr.NotFound("context view not found")
	}
	var view ContextView
	var conversationID int64
	var compaction []byte
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT response.uuid::text, view.reader_participant_id,
       NULLIF(COALESCE(reader.meta->>'name', reader.meta->>'user_name', ''), ''),
       view.history, view.include_side_talk, view.participant_history,
       view.window_tokens, view.digest, view.created_at, response.conversation_id,
       CASE WHEN jsonb_typeof(response.meta->'runtime_context'->'measurement') = 'object'
            THEN response.meta->'runtime_context'->'measurement' END
FROM %[1]s.chat_message_context_views view
JOIN %[1]s.chat_message_group response ON response.uuid = view.message_group_uuid
LEFT JOIN %[1]s.chat_participants reader ON reader.id = view.reader_participant_id
WHERE view.message_group_uuid = $1::uuid
  AND response.author_participant_id = view.reader_participant_id`, tenant),
		messageUUID,
	).Scan(&view.MessageID, &view.ReaderParticipantID, &view.ReaderName, &view.History,
		&view.IncludeSideTalk, &view.ParticipantHistory, &view.WindowTokens, &view.Digest,
		&view.RecordedAt, &conversationID, &compaction)
	if errors.Is(err, pgx.ErrNoRows) {
		return ContextView{}, apierr.NotFound("context view not found")
	}
	if err != nil {
		return ContextView{}, fmt.Errorf("read context view: %w", err)
	}
	view.Compaction = decodeContextViewCompaction(compaction)
	// The entries in recorded order, each joined to its message in the same
	// conversation only: an id that names a group elsewhere labels nothing.
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT entry.value->>'message_id', NULLIF(entry.value->>'reason', ''),
       NULLIF(COALESCE(author.meta->>'name', author.meta->>'user_name', ''), ''),
       COALESCE(message.author_kind, author.kind), message.created_at
FROM %[1]s.chat_message_context_views view
CROSS JOIN LATERAL jsonb_array_elements(view.entries) WITH ORDINALITY AS entry(value, position)
LEFT JOIN %[1]s.chat_message_group message
       ON message.uuid::text = entry.value->>'message_id' AND message.conversation_id = $2
LEFT JOIN %[1]s.chat_participants author ON author.id = message.author_participant_id
WHERE view.message_group_uuid = $1::uuid
ORDER BY entry.position`, tenant), messageUUID, conversationID)
	if err != nil {
		return ContextView{}, fmt.Errorf("read context view entries: %w", err)
	}
	defer rows.Close()
	view.Entries = []ContextViewEntry{}
	for rows.Next() {
		var entry ContextViewEntry
		var createdAt *time.Time
		if err := rows.Scan(&entry.MessageID, &entry.Reason, &entry.AuthorName, &entry.AuthorKind, &createdAt); err != nil {
			return ContextView{}, fmt.Errorf("scan context view entry: %w", err)
		}
		if createdAt != nil {
			utc := createdAt.UTC()
			entry.CreatedAt = &utc
		}
		view.Entries = append(view.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return ContextView{}, fmt.Errorf("read context view entries: %w", err)
	}
	return view, nil
}

// decodeContextViewCompaction reads the four numbers the inspector shows from
// a stored measurement; anything else in it stays out of the response.
func decodeContextViewCompaction(raw []byte) *ContextViewCompaction {
	if len(raw) == 0 {
		return nil
	}
	var measurement struct {
		Phase                   string `json:"phase"`
		EstimatedInputTokens    int64  `json:"estimated_input_tokens"`
		UsableInputTokens       int64  `json:"usable_input_tokens"`
		CompactionTriggerTokens int64  `json:"compaction_trigger_tokens"`
	}
	if err := json.Unmarshal(raw, &measurement); err != nil || measurement.Phase == "" {
		return nil
	}
	compaction := ContextViewCompaction(measurement)
	return &compaction
}
