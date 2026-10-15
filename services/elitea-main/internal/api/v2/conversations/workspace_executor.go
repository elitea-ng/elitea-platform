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
