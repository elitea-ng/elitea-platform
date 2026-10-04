package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/changesync"
)

// messageSelectSQL is the projection of one transcript row, shared by the
// legacy page (ListMessages) and the delta (ListMessageChanges) so the two
// cannot drift: a delta row is byte-identical to the row the legacy list
// serves for the same group. withSync appends mg.sync_at, which only the
// delta reads (it is the cursor position, never a wire field).
func messageSelectSQL(s string, withSync bool) string {
	sync := ""
	if withSync {
		sync = ", mg.sync_at"
	}
	return fmt.Sprintf(`
		SELECT mg.id, mg.conversation_id, COALESCE(mg.uuid::text, ''),
			p.entity_name, mg.meta, mg.created_at, mg.updated_at,
			mg.author_participant_id, mg.sent_to_id, mg.reply_to_id,
			COALESCE((
				SELECT string_agg(mt.content,
                    CASE WHEN COALESCE(mg.meta ->> 'output_limit_sequence', '') ~ '^[1-9][0-9]*$'
                        THEN '' ELSE E'\n' END
                    ORDER BY mi.order_index, mi.id)
				FROM %s.chat_message_items mi
				JOIN %s.chat_messages_text mt ON mt.id = mi.id
				WHERE mi.message_group_id = mg.id AND mi.item_type = 'text_message'
			), ''),
			mg.is_streaming, mg.task_id%s
		FROM %s.chat_message_group mg
		JOIN %s.chat_participants p ON p.id = mg.author_participant_id`, s, s, sync, s, s)
}

// scanMessageRows reads rows produced by messageSelectSQL into wire messages,
// with their attachment and canvas items, and (withSync) the stream position
// of each row. It closes rows.
func (r *ConversationsRepo) scanMessageRows(ctx context.Context, s string, rows pgx.Rows, withSync bool) ([]conversations.Message, []changesync.Position, error) {
	defer rows.Close()

	items := []conversations.Message{}
	positions := []changesync.Position{}
	// Index-aligned with `items`: the numeric group id each row was built
	// from, which the attachment projection below keys on. Kept beside the
	// slice rather than re-parsed out of `Message.ID` (a string on the wire)
	// so the join reads the id the database returned, not a round trip
	// through its decimal spelling.
	groupIDs := []int{}
	for rows.Next() {
		var m conversations.Message
		var meta []byte
		var entityName string
		var groupID int
		var isStreaming bool
		var taskID *string
		// A scan failure used to `continue`, so an unreadable row silently
		// dropped a message out of the transcript.
		// `updated_at` is nullable, so it is scanned into the pointer the wire
		// field is: a group that has never been rewritten states no update
		// time rather than claiming one.
		var syncAt time.Time
		dest := []any{&groupID, &m.ConversationID, &m.UUID, &entityName, &meta, &m.CreatedAt,
			&m.UpdatedAt, &m.AuthorParticipantID, &m.SentToID, &m.ReplyToID, &m.Content, &isStreaming, &taskID}
		if withSync {
			dest = append(dest, &syncAt)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, fmt.Errorf("conversations: scan message: %w", err)
		}
		m.ID = strconv.Itoa(groupID)
		if isStreaming {
			m.IsStreaming = true
			if taskID != nil && *taskID != "" {
				m.TaskID = taskID
			}
		}
		if meta != nil {
			_ = json.Unmarshal(meta, &m.Metadata) // best-effort: DB column is trusted JSON
		}
		// Map entity_name to role
		if entityName == "user" {
			m.Role = "user"
		} else {
			m.Role = "assistant"
		}
		m.ContentType = "text"
		items = append(items, m)
		groupIDs = append(groupIDs, groupID)
		positions = append(positions, changesync.Position{At: syncAt, ID: int64(groupID)})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("conversations: list messages: %w", err)
	}

	// The files each question was sent with (#606 read path, part 2).
	//
	// This projection is what the CHAT PAGE reads: useChatPageData.ts hands
	// these rows to ChatBox as `message_groups`, and `UserMessage`'s
	// `findAttachmentItems` filters them for `attachment_message` items. Until
	// this join existed the route answered every row with no items at all, so a
	// reloaded conversation showed the question and silently dropped the file
	// that rode it — while the details route, reading the SAME rows through
	// ListMessageGroups, returned it. Two projections of one transcript
	// disagreeing is the defect; the second one is now this.
	//
	// TEXT ITEMS ARE DELIBERATELY NOT INCLUDED. This route already collapses
	// each group's text into `content` (the string_agg above), which every
	// client reads as the message body; re-emitting the same text as items
	// would give two sources for one sentence and let them drift. Attachments
	// and CANVASES have no such representation here — they exist in this
	// response only as items — so they are what this carries.
	//
	// The canvas half is the same gap as the attachment half, one route later
	// (issue 853). `content` above aggregates `text_message` items ALONE, and
	// this projection carried attachments alone, so a canvas carved out of an
	// answer was invisible to the chat page in both halves of this response at
	// once: its text was not in `content` and its item was not in
	// `message_items`. The details route (ListMessageGroups) served it and this
	// one did not — two projections of one transcript disagreeing, which is the
	// defect the attachment note above already names. A frontend opener alone
	// could not have closed it: there was nothing in this payload to open.
	if len(groupIDs) > 0 {
		byGroup, err := r.attachmentItemsByGroup(ctx, s, groupIDs)
		if err != nil {
			return nil, nil, err
		}
		canvasByGroup, err := r.canvasItemsByGroup(ctx, s, groupIDs)
		if err != nil {
			return nil, nil, err
		}
		for i := range items {
			// One list per group, in the order the items are STORED. Each
			// projection is ordered on its own, so concatenating them would
			// put every attachment before every canvas whatever the message
			// actually looks like; `order_index` is what says where the canvas
			// sits, and a client rendering the items in the order given must
			// be given the right one.
			merged := append(append([]map[string]any{}, byGroup[groupIDs[i]]...), canvasByGroup[groupIDs[i]]...)
			if len(merged) == 0 {
				continue
			}
			sort.SliceStable(merged, func(a, b int) bool {
				left, leftOK := merged[a]["order_index"].(int)
				right, rightOK := merged[b]["order_index"].(int)
				if !leftOK || !rightOK {
					return false
				}
				return left < right
			})
			items[i].MessageItems = merged
		}
	}
	return items, positions, nil
}
