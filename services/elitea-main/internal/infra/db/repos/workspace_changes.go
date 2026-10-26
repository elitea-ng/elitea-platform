package repos

// The files a cloud turn changed (shared/0177): workspace_turn_changes holds
// one row per published cloud commit of a handoff, workspace_change_diffs the
// kept diffs. The erasure of a cloud copy and a new handoff of the thread
// delete both (workspace_handoffs.go, deleteWorkspaceChanges).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacechanges"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
)

// WorkspaceChangesRepository implements workspacechanges.Repository.
type WorkspaceChangesRepository struct {
	pool *pgxpool.Pool
}

var _ workspacechanges.Repository = (*WorkspaceChangesRepository)(nil)

func NewWorkspaceChangesRepository(pool *pgxpool.Pool) (*WorkspaceChangesRepository, error) {
	if pool == nil {
		return nil, errors.New("workspace changes database is required")
	}
	return &WorkspaceChangesRepository{pool: pool}, nil
}

// deleteWorkspaceChanges removes a thread's records and diffs, inside the
// caller's transaction (erasure, a new handoff).
func deleteWorkspaceChanges(ctx context.Context, tx pgx.Tx, thread workspacehandoff.Thread) error {
	if _, err := tx.Exec(ctx, `
DELETE FROM elitea_runtime.workspace_change_diffs WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
		thread.ProjectID, thread.ConversationUUID); err != nil {
		return fmt.Errorf("workspace changes: delete diffs: %w", err)
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM elitea_runtime.workspace_turn_changes WHERE project_id = $1 AND conversation_uuid = $2::uuid`,
		thread.ProjectID, thread.ConversationUUID); err != nil {
		return fmt.Errorf("workspace changes: delete records: %w", err)
	}
	return nil
}

// Save replaces the commit's record under a share lock of the handoff row, so
// an erasure or a new handoff (which update that row) never interleaves: a
// record for a handoff that is no longer the thread's is ErrNotFound.
func (r *WorkspaceChangesRepository) Save(ctx context.Context, record workspacechanges.Record) error {
	turn, err := json.Marshal(record.Turn)
	if err != nil {
		return fmt.Errorf("workspace changes: encode: %w", err)
	}
	cumulative, err := json.Marshal(record.Cumulative)
	if err != nil {
		return fmt.Errorf("workspace changes: encode: %w", err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("workspace changes: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var live bool
	err = tx.QueryRow(ctx, `
SELECT TRUE FROM elitea_runtime.workspace_handoffs
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND handoff_id = $3::uuid AND deleted_at IS NULL
FOR SHARE`, record.Thread.ProjectID, record.Thread.ConversationUUID, record.HandoffID).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspacechanges.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("workspace changes: lock handoff: %w", err)
	}
	thread := []any{record.Thread.ProjectID, record.Thread.ConversationUUID}
	// The commit's diffs are replaced; other commits keep only their turn
	// diffs (the cumulative ones of the tip are the only ones read).
	if _, err := tx.Exec(ctx, `
DELETE FROM elitea_runtime.workspace_change_diffs
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND (commit_sha = $3 OR scope = 'cumulative')`,
		append(thread, record.Commit)...); err != nil {
		return fmt.Errorf("workspace changes: replace diffs: %w", err)
	}
	var response *string
	if record.ResponseMessageID != "" {
		response = &record.ResponseMessageID
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.workspace_turn_changes (
    project_id, conversation_uuid, commit_sha, handoff_id, parent_sha, base_sha, seq, execution_id,
    response_message_id, turn_files, cumulative_files, depth)
VALUES ($1, $2::uuid, $3, $4::uuid, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12)
ON CONFLICT (project_id, conversation_uuid, commit_sha) DO UPDATE SET
    handoff_id = EXCLUDED.handoff_id, parent_sha = EXCLUDED.parent_sha, base_sha = EXCLUDED.base_sha,
    seq = EXCLUDED.seq, depth = EXCLUDED.depth, execution_id = EXCLUDED.execution_id, response_message_id = EXCLUDED.response_message_id,
    turn_files = EXCLUDED.turn_files, cumulative_files = EXCLUDED.cumulative_files, created_at = clock_timestamp()`,
		record.Thread.ProjectID, record.Thread.ConversationUUID, record.Commit, record.HandoffID, record.Parent,
		record.Base, record.Seq, record.ExecutionID, response, string(turn), string(cumulative), record.Depth); err != nil {
		return fmt.Errorf("workspace changes: save record: %w", err)
	}
	if len(record.Diffs) > 0 {
		batch := &pgx.Batch{}
		for _, diff := range record.Diffs {
			batch.Queue(`
INSERT INTO elitea_runtime.workspace_change_diffs (project_id, conversation_uuid, commit_sha, scope, path, diff)
VALUES ($1, $2::uuid, $3, $4, $5, $6)`, record.Thread.ProjectID, record.Thread.ConversationUUID, record.Commit,
				diff.Scope, diff.Path, diff.Text)
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("workspace changes: save diffs: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("workspace changes: commit: %w", err)
	}
	return nil
}

// Turns reads the handoff's records, newest first, at most limit.
func (r *WorkspaceChangesRepository) Turns(ctx context.Context, thread workspacehandoff.Thread, handoffID string, limit int) ([]workspacechanges.Stored, error) {
	rows, err := r.pool.Query(ctx, `
SELECT commit_sha, parent_sha, seq, depth, COALESCE(response_message_id, ''), created_at, turn_files, cumulative_files
FROM elitea_runtime.workspace_turn_changes
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND handoff_id = $3::uuid
ORDER BY seq DESC, created_at DESC
LIMIT $4`, thread.ProjectID, thread.ConversationUUID, handoffID, limit)
	if err != nil {
		return nil, fmt.Errorf("workspace changes: read: %w", err)
	}
	defer rows.Close()
	out := []workspacechanges.Stored{}
	for rows.Next() {
		var (
			stored           workspacechanges.Stored
			turn, cumulative []byte
		)
		if err := rows.Scan(&stored.Commit, &stored.Parent, &stored.Seq, &stored.Depth, &stored.ResponseMessageID, &stored.CreatedAt,
			&turn, &cumulative); err != nil {
			return nil, fmt.Errorf("workspace changes: scan: %w", err)
		}
		if err := json.Unmarshal(turn, &stored.Turn); err != nil {
			return nil, fmt.Errorf("workspace changes: decode: %w", err)
		}
		if err := json.Unmarshal(cumulative, &stored.Cumulative); err != nil {
			return nil, fmt.Errorf("workspace changes: decode: %w", err)
		}
		out = append(out, stored)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace changes: read: %w", err)
	}
	return out, nil
}

// Diff reads one kept diff.
func (r *WorkspaceChangesRepository) Diff(ctx context.Context, thread workspacehandoff.Thread, commit, scope, path string) (string, bool, error) {
	var text string
	err := r.pool.QueryRow(ctx, `
SELECT diff FROM elitea_runtime.workspace_change_diffs
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND commit_sha = $3 AND scope = $4 AND path = $5`,
		thread.ProjectID, thread.ConversationUUID, commit, scope, path).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("workspace changes: read diff: %w", err)
	}
	return text, true, nil
}
