package conversations

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The executor of a Local work thread (ADR-0034 decision 14,
// `workspace_executor`): who owns the thread now.
const (
	WorkspaceExecutorLocal     = "local"
	WorkspaceExecutorCloud     = "cloud"
	WorkspaceExecutorReturning = "returning"
)

// WorkspaceExecutorOf maps a handoff state to the wire value. A thread with
// no handoff row, or one still being sent (HANDING_OFF), is the desktop's.
func WorkspaceExecutorOf(state string) string {
	switch state {
	case "CLOUD_IDLE", "CLOUD_RUNNING":
		return WorkspaceExecutorCloud
	case "RECLAIMING":
		return WorkspaceExecutorReturning
	}
	return WorkspaceExecutorLocal
}

// workspaceExecutor reads one conversation's executor. A database without the
// handoff table (a partial schema) or a failed read answers `local`: the
// conversation read must not fail on the banner's field.
func (h *Handler) workspaceExecutor(ctx context.Context, projectID, conversationUUID string) string {
	pool, _ := h.pool.(*pgxpool.Pool)
	project, err := strconv.ParseInt(projectID, 10, 32)
	if pool == nil || err != nil || conversationUUID == "" {
		return WorkspaceExecutorLocal
	}
	var present bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('elitea_runtime.workspace_handoffs') IS NOT NULL`).Scan(&present); err != nil || !present {
		return WorkspaceExecutorLocal
	}
	var state string
	err = pool.QueryRow(ctx, `
SELECT state FROM elitea_runtime.workspace_handoffs WHERE project_id = $1 AND conversation_uuid::text = $2`,
		project, conversationUUID).Scan(&state)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.WarnContext(ctx, "conversation: workspace executor unreadable", "project_id", projectID, "err", err)
		return WorkspaceExecutorLocal
	}
	return WorkspaceExecutorOf(state)
}

// annotateWorkspaceExecutors sets `workspace_executor` on conversation-list
// rows (each row's "id" is the numeric conversation id). A database without
// the handoff table, or a failed read, leaves every row `local`: the listing
// must not fail on the badge.
func annotateWorkspaceExecutors(ctx context.Context, pool *pgxpool.Pool, schema, projectID string, rows []map[string]any) {
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		row["workspace_executor"] = WorkspaceExecutorLocal
		if id, ok := row["id"].(int); ok {
			ids = append(ids, int64(id))
		}
	}
	project, err := strconv.ParseInt(projectID, 10, 32)
	if pool == nil || err != nil || len(ids) == 0 {
		return
	}
	var present bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('elitea_runtime.workspace_handoffs') IS NOT NULL`).Scan(&present); err != nil || !present {
		return
	}
	result, err := pool.Query(ctx, `
SELECT c.id, h.state FROM `+schema+`.chat_conversations AS c
JOIN elitea_runtime.workspace_handoffs AS h ON h.project_id = $1 AND h.conversation_uuid = c.uuid
WHERE c.id = ANY($2::bigint[])`, project, ids)
	if err != nil {
		slog.WarnContext(ctx, "conversations: workspace executors unreadable", "project_id", projectID, "err", err)
		return
	}
	defer result.Close()
	states := map[int64]string{}
	for result.Next() {
		var id int64
		var state string
		if err := result.Scan(&id, &state); err != nil {
			return
		}
		states[id] = state
	}
	for _, row := range rows {
		if id, ok := row["id"].(int); ok {
			row["workspace_executor"] = WorkspaceExecutorOf(states[int64(id)])
		}
	}
}
