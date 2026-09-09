package repos

// PipelineRunsRepo backs internal/application/pipelineruns.Tracker over
// public.pipeline_runs (migrations/shared/0124_pipeline_runs.sql). See that
// migration's own header for why this table exists, and
// pipelineruns/settlement_hook.go's package doc for how the three methods
// below are used together.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/pipelineruns"
)

type PipelineRunsRepo struct {
	pool *pgxpool.Pool
}

func NewPipelineRunsRepo(pool *pgxpool.Pool) *PipelineRunsRepo {
	return &PipelineRunsRepo{pool: pool}
}

// RecordRunStart inserts one row. ON CONFLICT DO NOTHING: execution ids are
// minted fresh per admitted run (uuid), so a real duplicate should not
// happen, but a caller that retries an already-recorded admission (a replay
// at a layer ABOVE this one, not this table's own concern) must not fail on
// this write alone — the row it would have written is already there.
func (r *PipelineRunsRepo) RecordRunStart(ctx context.Context, run pipelineruns.Run) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("pipeline_runs: no database configured")
	}
	_, err := r.pool.Exec(ctx, `
INSERT INTO public.pipeline_runs (execution_id, project_id, application_id, version_id, conversation_uuid, origin)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (execution_id) DO NOTHING`,
		run.ExecutionID, run.ProjectID, run.ApplicationID, run.VersionID, run.ConversationUUID, run.Origin,
	)
	if err != nil {
		return fmt.Errorf("pipeline_runs: record start: %w", err)
	}
	return nil
}

// RecordExecutionError sets error_summary. Affecting zero rows (executionID
// names no tracked pipeline run) is NOT an error — see the Tracker interface
// doc comment.
func (r *PipelineRunsRepo) RecordExecutionError(ctx context.Context, executionID, safeMessage string) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("pipeline_runs: no database configured")
	}
	_, err := r.pool.Exec(ctx,
		`UPDATE public.pipeline_runs SET error_summary = $2 WHERE execution_id = $1`,
		executionID, safeMessage,
	)
	if err != nil {
		return fmt.Errorf("pipeline_runs: record execution error: %w", err)
	}
	return nil
}

// ClaimForEvent atomically claims ONE emission — see the Tracker interface
// doc comment for the exactly-once contract this implements.
func (r *PipelineRunsRepo) ClaimForEvent(
	ctx context.Context, executionID string,
) (run pipelineruns.Run, startedAt time.Time, errorSummary string, found bool, err error) {
	if r == nil || r.pool == nil {
		return pipelineruns.Run{}, time.Time{}, "", false, fmt.Errorf("pipeline_runs: no database configured")
	}
	var summary *string
	scanErr := r.pool.QueryRow(ctx, `
UPDATE public.pipeline_runs
SET event_emitted_at = now()
WHERE execution_id = $1 AND event_emitted_at IS NULL
RETURNING project_id, application_id, version_id, conversation_uuid, origin, started_at, error_summary`,
		executionID,
	).Scan(&run.ProjectID, &run.ApplicationID, &run.VersionID, &run.ConversationUUID, &run.Origin, &startedAt, &summary)
	if scanErr != nil {
		if scanErr == pgx.ErrNoRows {
			return pipelineruns.Run{}, time.Time{}, "", false, nil
		}
		return pipelineruns.Run{}, time.Time{}, "", false, fmt.Errorf("pipeline_runs: claim for event: %w", scanErr)
	}
	run.ExecutionID = executionID
	if summary != nil {
		errorSummary = *summary
	}
	return run, startedAt, errorSummary, true, nil
}

var _ pipelineruns.Tracker = (*PipelineRunsRepo)(nil)
