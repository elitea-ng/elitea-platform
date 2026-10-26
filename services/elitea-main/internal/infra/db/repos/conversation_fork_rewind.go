package repos

// The fork's rewind, replay and handoff fence (issue 1234). See the file
// comment of conversation_fork.go and conversations/fork_rewind.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

// maxForkGroups bounds the message groups one fork copies.
const maxForkGroups = 1000

// abandonedTurnsRead bounds the groups a rewind's server-built branch
// summary reads: the summary keeps at most 24 entries.
const abandonedTurnsRead = 64

// replayFork finds the fork the request's fork_id made for this caller. It
// answers found when there is one: the same fork (Replayed, with the
// forked_messages map the first answer carried) when it was made of the
// plan's source, fork point and mode, else ErrForkIDReused. The lookup is
// tenant/0155's partial index.
func replayFork(ctx context.Context, q forkQuerier, s string, actorID int64, request conversations.ForkRequest, plan forkPlan) (conversations.ForkResult, bool, error) {
	var id int64
	var forkUUID string
	var markerBytes []byte
	err := q.QueryRow(ctx, fmt.Sprintf(`SELECT c.id, c.uuid::text, c.meta->'%[2]s'
		FROM %[1]s.chat_conversations c
		WHERE c.author_id = $1 AND (c.meta->'%[2]s'->>'fork_id') IS NOT NULL
		  AND (c.meta->'%[2]s'->>'fork_id') = $2
		ORDER BY c.id LIMIT 1`, s, conversations.ForkedFromMetaKey),
		actorID, request.ForkID).Scan(&id, &forkUUID, &markerBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversations.ForkResult{}, false, nil
	}
	if err != nil {
		return conversations.ForkResult{}, false, fmt.Errorf("conversations: fork: read fork_id: %w", err)
	}
	var marker struct {
		ConversationUUID string `json:"conversation_uuid"`
		MessageUUID      string `json:"message_uuid"`
		Mode             string `json:"mode"`
	}
	_ = json.Unmarshal(markerBytes, &marker)
	pointUUID := ""
	if plan.point != nil && request.MessageID != "" {
		pointUUID = plan.point.uuid
	}
	if marker.ConversationUUID != plan.src.uuid || marker.MessageUUID != pointUUID || forkMode(marker.Mode) != forkMode(request.Mode) {
		return conversations.ForkResult{}, true, conversations.ErrForkIDReused
	}
	messageUUIDs, err := replayedMessageUUIDs(ctx, q, s, id, plan.groups)
	if err != nil {
		return conversations.ForkResult{}, true, err
	}
	return conversations.ForkResult{ID: id, UUID: forkUUID, MessageUUIDs: messageUUIDs, Replayed: true}, true, nil
}

// replayedMessageUUIDs rebuilds a fork's source-to-copy group UUID map: a
// copy keeps its source's created_at and author, and the copies were made in
// the source's transcript order, so the n-th source group of one (created_at,
// author) pairs with the n-th copy of it. A group added to either side since
// pairs with nothing and is left out; so is one deleted since.
func replayedMessageUUIDs(ctx context.Context, q forkQuerier, s string, forkID int64, groups []int64) (map[string]string, error) {
	out := map[string]string{}
	if len(groups) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`WITH o AS (
		  SELECT g.uuid, g.created_at, g.author_participant_id AS author,
		         row_number() OVER (PARTITION BY g.created_at, g.author_participant_id ORDER BY src.ord) AS n
		  FROM unnest($2::integer[]) WITH ORDINALITY AS src(id, ord)
		  JOIN %[1]s.chat_message_group g ON g.id = src.id
		), c AS (
		  SELECT g.uuid, g.created_at, g.author_participant_id AS author,
		         row_number() OVER (PARTITION BY g.created_at, g.author_participant_id ORDER BY g.id) AS n
		  FROM %[1]s.chat_message_group g WHERE g.conversation_id = $1
		)
		SELECT o.uuid::text, c.uuid::text
		FROM o JOIN c ON c.created_at = o.created_at AND c.author IS NOT DISTINCT FROM o.author AND c.n = o.n`, s),
		forkID, groups)
	if err != nil {
		return nil, fmt.Errorf("conversations: fork: replay messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return nil, fmt.Errorf("conversations: fork: replay messages: %w", err)
		}
		out[from] = to
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversations: fork: replay messages: %w", err)
	}
	return out, nil
}

// refuseForkWhileHandedOff answers ErrThreadExecutorIsCloud for a Local
// work source whose handoff row is not LOCAL, read under the handoff's
// shared thread lock (workspaceThreadState). A database without the
// handoff table (a fixture of the tenant chain only) has no handoffs.
func refuseForkWhileHandedOff(ctx context.Context, tx pgx.Tx, projectID string, src forkSource) error {
	if src.source != conversations.LocalWorkSource {
		return nil
	}
	project, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil {
		return fmt.Errorf("conversations: fork: project id: %w", err)
	}
	var present bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('elitea_runtime.workspace_handoffs') IS NOT NULL`).Scan(&present); err != nil {
		return fmt.Errorf("conversations: fork: probe handoffs: %w", err)
	}
	if !present {
		return nil
	}
	state, err := workspaceThreadState(ctx, tx, project, src.uuid)
	if err != nil {
		return err
	}
	if state != workspacestore.StateLocal {
		return conversations.ErrThreadExecutorIsCloud
	}
	return nil
}

// forkMarkerExtras is what the request adds to the marker: the mode, the
// fork_id and a rewind's branch summary (the request's, or one built from
// the abandoned turns).
func forkMarkerExtras(ctx context.Context, tx pgx.Tx, s string, tables map[string][]string, request conversations.ForkRequest, plan forkPlan) (map[string]any, error) {
	mode := forkMode(request.Mode)
	extras := map[string]any{"mode": mode}
	if request.ForkID != "" {
		extras["fork_id"] = request.ForkID
	}
	if mode != conversations.ForkModeRewind {
		return extras, nil
	}
	summary := request.BranchSummary
	if summary == nil {
		turns, err := abandonedTurns(ctx, tx, s, tables, plan)
		if err != nil {
			return nil, err
		}
		summary = conversations.BuildBranchSummary(turns)
	}
	extras["branch_summary"] = summary
	return extras, nil
}

// forkMode is a mode as stored: absent (a fork made before rewinds) is a fork.
func forkMode(mode string) string {
	if mode == "" {
		return conversations.ForkModeFork
	}
	return mode
}

// abandonedTurns reads the groups of the source the fork does not copy,
// oldest first, with their text (a swarm child is its parent's, and left
// out).
func abandonedTurns(ctx context.Context, tx pgx.Tx, s string, tables map[string][]string, plan forkPlan) ([]conversations.AbandonedTurn, error) {
	text := `''`
	join := ""
	if tables["chat_messages_text"] != nil {
		text = `COALESCE(string_agg(t.content, ' ' ORDER BY i.order_index, i.id), '')`
		join = fmt.Sprintf(`LEFT JOIN %[1]s.chat_message_items i ON i.message_group_id = g.id
			LEFT JOIN %[1]s.chat_messages_text t ON t.id = i.id`, s)
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT g.reply_to_id IS NULL, %[2]s
		FROM %[1]s.chat_message_group g %[3]s
		WHERE g.conversation_id = $1 AND NOT (g.id = ANY($2::integer[])) AND NOT (g.meta ? 'parent_message_id')
		GROUP BY g.id ORDER BY g.created_at, g.id LIMIT %[4]d`, s, text, join, abandonedTurnsRead),
		plan.src.id, plan.groups)
	if err != nil {
		return nil, fmt.Errorf("conversations: fork: read abandoned turns: %w", err)
	}
	defer rows.Close()
	var turns []conversations.AbandonedTurn
	for rows.Next() {
		var turn conversations.AbandonedTurn
		if err := rows.Scan(&turn.Question, &turn.Text); err != nil {
			return nil, fmt.Errorf("conversations: fork: scan abandoned turns: %w", err)
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}
