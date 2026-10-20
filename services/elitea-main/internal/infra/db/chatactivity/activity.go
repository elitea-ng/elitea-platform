// Package chatactivity reads the activity status of conversation list rows
// (client contract 1.12): whether a turn runs, whether the newest message
// waits for the user, when the conversation last moved, and whether the
// reader has an answer they did not open yet.
//
// Every read is set-based: one statement for a page of conversation ids,
// never one per row. The callers (the conversation list and its delta, the
// fork answer, the folder sidebar) scan their rows first and then annotate
// them with Load, as they annotate the archiver's name.
package chatactivity

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The values of `activity.state`. The wire field is a string, not an
// OpenAPI enum: a client treats an unknown value as StateIdle.
const (
	// StateRunning: the newest message group streams (a turn is in flight).
	StateRunning = "running"
	// StateNeedsYou: the newest message group paused for the user (a tool
	// approval, a HITL answer, an authorization, a static pipeline input, a
	// recovery decision, or "continue" after the output limit).
	StateNeedsYou = "needs_you"
	// StateIdle: anything else.
	StateIdle = "idle"
)

// PausedPredicate is the SQL test, for the message group aliased alias, that
// the group paused for the user. It is the editor Test run's PAUSED rule
// (repos/editor_test_runs.go), and the same text as the partial index
// chat_message_group_paused_idx (tenant/0153), so the planner can use the
// index. alias is a trusted identifier, never caller text.
func PausedPredicate(alias string) string {
	return fmt.Sprintf(`(NOT %[1]s.is_streaming AND (%[1]s.meta ? 'hitl_interrupt' OR %[1]s.meta ? 'hitl_interrupts'
		OR %[1]s.meta ? 'authorization_requests' OR %[1]s.meta->'output_limit_reached' = 'true'::jsonb
		OR %[1]s.meta ? 'pipeline_static_v1' OR %[1]s.meta ? 'node_recovery_required_v1'))`, alias)
}

// Activity is one conversation's status for one reader.
type Activity struct {
	State string
	// Since is when the conversation entered State: the newest group's last
	// write for running and needs_you, LastActivityAt for idle.
	Since time.Time
	// LastActivityAt is the newest write of any message group (a message, a
	// turn start or a turn transition), or the conversation's creation when
	// it has no message.
	LastActivityAt time.Time
	// Unread: an answer (a message group no user participant wrote, that
	// does not stream) changed after the reader's read marker.
	Unread bool
}

// FormatTime writes a naive UTC wall clock (the chat tables' `timestamp`
// columns) as RFC 3339 in UTC.
func FormatTime(at time.Time) string {
	return time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), at.Second(), at.Nanosecond(), time.UTC).
		Format(time.RFC3339Nano)
}

// Fields are the list row's wire fields: `activity`, `last_activity_at` and
// `unread`.
func (a Activity) Fields() map[string]any {
	state := a.State
	if state == "" {
		state = StateIdle
	}
	return map[string]any{
		"activity":         map[string]any{"state": state, "since": FormatTime(a.Since)},
		"last_activity_at": FormatTime(a.LastActivityAt),
		"unread":           a.Unread,
	}
}

// Annotate adds Fields to row.
func (a Activity) Annotate(row map[string]any) {
	for key, value := range a.Fields() {
		row[key] = value
	}
}

// Querier is the read surface Load needs: a pool, a connection or a
// transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Load reads the activity of the conversations ids in the tenant schema s (a
// quoted identifier) for reader. A reader <= 0 has no read marker: every
// row's Unread is false. An id that does not exist is absent from the map.
//
// It reads each conversation's newest message group through
// chat_message_group_conversation_newest_idx and the reader's marker
// through the primary key of chat_conversation_reads (tenant/0153). A
// reader without a marker has read everything before the tenant's
// chat_conversation_read_baseline().
func Load(ctx context.Context, q Querier, s string, reader int64, ids []int64) (map[int64]Activity, error) {
	out := make(map[int64]Activity, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`
		SELECT c.id,
		       COALESCE(latest.is_streaming, false),
		       COALESCE(latest.paused, false),
		       latest.at,
		       COALESCE(agg.last_at, c.created_at),
		       COALESCE(agg.answer_at > COALESCE(r.read_at, %[1]s.chat_conversation_read_baseline()), false)
		         AND $2::bigint > 0
		FROM %[1]s.chat_conversations c
		LEFT JOIN LATERAL (
		    SELECT g.is_streaming, %[2]s AS paused, COALESCE(g.updated_at, g.created_at) AS at
		    FROM %[1]s.chat_message_group g
		    WHERE g.conversation_id = c.id
		    ORDER BY g.created_at DESC, g.id DESC
		    LIMIT 1) latest ON TRUE
		LEFT JOIN LATERAL (
		    SELECT max(COALESCE(g.updated_at, g.created_at)) AS last_at,
		           max(COALESCE(g.updated_at, g.created_at))
		             FILTER (WHERE NOT g.is_streaming AND p.entity_name IS DISTINCT FROM 'user') AS answer_at
		    FROM %[1]s.chat_message_group g
		    LEFT JOIN %[1]s.chat_participants p ON p.id = g.author_participant_id
		    WHERE g.conversation_id = c.id) agg ON TRUE
		LEFT JOIN %[1]s.chat_conversation_reads r ON r.conversation_id = c.id AND r.user_id = $2::bigint
		WHERE c.id = ANY($1::bigint[])`, s, PausedPredicate("g")), ids, reader)
	if err != nil {
		return nil, fmt.Errorf("chat activity: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var streaming, paused, unread bool
		var latestAt *time.Time
		var lastAt time.Time
		if err := rows.Scan(&id, &streaming, &paused, &latestAt, &lastAt, &unread); err != nil {
			return nil, fmt.Errorf("chat activity: scan: %w", err)
		}
		activity := Activity{State: StateIdle, Since: lastAt, LastActivityAt: lastAt, Unread: unread}
		switch {
		case streaming && latestAt != nil:
			activity.State, activity.Since = StateRunning, *latestAt
		case paused && latestAt != nil:
			activity.State, activity.Since = StateNeedsYou, *latestAt
		}
		out[id] = activity
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat activity: %w", err)
	}
	return out, nil
}

// Paused answers which of ids have a newest message group that paused for
// the user (StateNeedsYou). It reads only the paused groups, through the
// partial index chat_message_group_paused_idx, so it is cheap for every
// conversation of a project: the folder sidebar counts "needs you" per group
// over rows it does not page in.
func Paused(ctx context.Context, q Querier, s string, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`
		SELECT DISTINCT g.conversation_id
		FROM %[1]s.chat_message_group g
		WHERE g.conversation_id = ANY($1::bigint[]) AND %[2]s
		  AND NOT EXISTS (
		      SELECT 1 FROM %[1]s.chat_message_group newer
		      WHERE newer.conversation_id = g.conversation_id
		        AND (newer.created_at, newer.id) > (g.created_at, g.id))`, s, PausedPredicate("g")), ids)
	if err != nil {
		return nil, fmt.Errorf("chat activity: paused: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("chat activity: paused: scan: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat activity: paused: %w", err)
	}
	return out, nil
}
