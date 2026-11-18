package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The human score store (tenant/0244) and the run's limit marker, on the run
// repository because they read and write the same tenant tables.

const humanScoreColumns = `
	id::text,
	run_id::text,
	dataset_case_id::text,
	dimension_id::text,
	native_score,
	normalized_score,
	COALESCE(note, ''),
	reviewer_id,
	created_at`

func scanHumanScore(row pgx.Row) (evaluation.HumanScore, error) {
	var score evaluation.HumanScore
	var createdAt time.Time
	err := row.Scan(&score.ID, &score.RunID, &score.DatasetCaseID, &score.DimensionID,
		&score.NativeScore, &score.NormalizedScore, &score.Note, &score.ReviewerID, &createdAt)
	if err != nil {
		return evaluation.HumanScore{}, err
	}
	score.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	return score, nil
}

// ListHumanScores answers every score of the run, oldest first, so the history
// of a re-scored case reads in order and the last row is the one in force.
func (r *EvalRunsRepo) ListHumanScores(ctx context.Context, projectID, runID string) ([]evaluation.HumanScore, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return nil, err
	}
	id, err := parseEvalID(runID, "run")
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
		SELECT `+humanScoreColumns+`
		FROM %s.eval_human_scores
		WHERE run_id = $1
		ORDER BY dataset_case_id, dimension_id, id`, schema), id)
	if err != nil {
		return nil, fmt.Errorf("eval human scores: list: %w", err)
	}
	defer rows.Close()
	scores := []evaluation.HumanScore{}
	for rows.Next() {
		score, scanErr := scanHumanScore(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("eval human scores: scan: %w", scanErr)
		}
		scores = append(scores, score)
	}
	return scores, rows.Err()
}

// RecordHumanScore appends the score, stores it on the result and recomputes
// the headline, in one transaction.
//
// The result row is locked first (FOR UPDATE), so two reviewers scoring the
// same cell are serialised and the newest row is the one stored on the result.
// Only a result that is `pending_human`, or `ok` and already human-scored, can
// be scored: a result the AI judge scored is not overwritten by a person.
func (r *EvalRunsRepo) RecordHumanScore(
	ctx context.Context,
	projectID string,
	in evaluation.HumanScoreWrite,
) (evaluation.HumanScoreRecorded, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return evaluation.HumanScoreRecorded{}, err
	}
	runID, err := parseEvalID(in.RunID, "run")
	if err != nil {
		return evaluation.HumanScoreRecorded{}, err
	}
	caseID, err := parseEvalID(in.DatasetCaseID, "case")
	if err != nil {
		return evaluation.HumanScoreRecorded{}, err
	}
	dimensionID, err := parseEvalID(in.DimensionID, "dimension")
	if err != nil {
		return evaluation.HumanScoreRecorded{}, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return evaluation.HumanScoreRecorded{}, fmt.Errorf("eval human scores: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	err = tx.QueryRow(ctx, fmt.Sprintf(`
		SELECT status FROM %s.eval_results
		WHERE run_id = $1 AND dataset_case_id = $2 AND dimension_id = $3
		FOR UPDATE`, schema), runID, caseID, dimensionID).Scan(&status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evaluation.HumanScoreRecorded{}, apierr.Conflict(
				"that case has no result for this dimension yet: the run has not answered it")
		}
		return evaluation.HumanScoreRecorded{}, fmt.Errorf("eval human scores: lock result: %w", err)
	}
	if status != evaluation.ResultStatusPendingHuman && status != evaluation.ResultStatusOK {
		return evaluation.HumanScoreRecorded{}, apierr.Conflict("that result is " + status + " and cannot be scored by a human")
	}
	if status == evaluation.ResultStatusOK {
		var humanScored bool
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
			SELECT EXISTS (SELECT 1 FROM %s.eval_human_scores
			               WHERE run_id = $1 AND dataset_case_id = $2 AND dimension_id = $3)`, schema),
			runID, caseID, dimensionID).Scan(&humanScored); err != nil {
			return evaluation.HumanScoreRecorded{}, fmt.Errorf("eval human scores: probe: %w", err)
		}
		if !humanScored {
			return evaluation.HumanScoreRecorded{}, apierr.Conflict("that result was scored by the judge, not by a human")
		}
	}

	score, err := scanHumanScore(tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s.eval_human_scores (
			run_id, dataset_case_id, dimension_id, native_score, normalized_score, note, reviewer_id
		) VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7::int)
		RETURNING `+humanScoreColumns, schema),
		runID, caseID, dimensionID, in.NativeScore, in.NormalizedScore, in.Note, in.ReviewerID))
	if err != nil {
		return evaluation.HumanScoreRecorded{}, wrapEvalWrite("human score insert", err)
	}

	verdict, err := json.Marshal(map[string]any{"score": in.NativeScore, "reason": in.Note, "scored_by": "human"})
	if err != nil {
		return evaluation.HumanScoreRecorded{}, fmt.Errorf("eval human scores: verdict: %w", err)
	}
	result, err := scanResult(tx.QueryRow(ctx, fmt.Sprintf(`
		UPDATE %s.eval_results
		SET status = 'ok', native_score = $4, normalized_score = $5, target_met = $6,
		    verdict = $7::jsonb, created_at = now()
		WHERE run_id = $1 AND dataset_case_id = $2 AND dimension_id = $3
		RETURNING `+resultColumns, schema),
		runID, caseID, dimensionID, in.NativeScore, in.NormalizedScore, in.TargetMet, string(verdict)))
	if err != nil {
		return evaluation.HumanScoreRecorded{}, wrapEvalWrite("human score result", err)
	}

	// The run row is locked BEFORE the headline is computed, the way FinishRun
	// locks it: a score that lands while the run finishes is then either seen
	// by FinishRun's recompute or applied to the finished run here, and the
	// stored headline cannot be left stale.
	var runStatus string
	if err := tx.QueryRow(ctx, fmt.Sprintf(
		`SELECT status FROM %s.eval_runs WHERE id = $1 FOR UPDATE`, schema), runID).Scan(&runStatus); err != nil {
		return evaluation.HumanScoreRecorded{}, fmt.Errorf("eval human scores: lock run: %w", err)
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(`
		SELECT normalized_score FROM %s.eval_results
		WHERE run_id = $1 AND status = 'ok' AND normalized_score IS NOT NULL`, schema), runID)
	if err != nil {
		return evaluation.HumanScoreRecorded{}, fmt.Errorf("eval human scores: headline read: %w", err)
	}
	scores := []float64{}
	for rows.Next() {
		var value float64
		if scanErr := rows.Scan(&value); scanErr != nil {
			rows.Close()
			return evaluation.HumanScoreRecorded{}, fmt.Errorf("eval human scores: headline scan: %w", scanErr)
		}
		scores = append(scores, value)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return evaluation.HumanScoreRecorded{}, fmt.Errorf("eval human scores: headline read: %w", err)
	}
	headline := evaluation.HeadlineFromScores(scores)

	// A run that is still executing computes its headline when it finishes,
	// from the same results, so only a run that has already stopped is
	// updated here.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
		UPDATE %s.eval_runs SET headline_score = $2
		WHERE id = $1 AND status IN ('finished', 'errored', 'cancelled')`, schema), runID, headline); err != nil {
		return evaluation.HumanScoreRecorded{}, fmt.Errorf("eval human scores: headline write: %w", err)
	}
	var current *float64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT headline_score FROM %s.eval_runs WHERE id = $1`, schema), runID).
		Scan(&current); err != nil {
		return evaluation.HumanScoreRecorded{}, fmt.Errorf("eval human scores: headline reread: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return evaluation.HumanScoreRecorded{}, fmt.Errorf("eval human scores: commit: %w", err)
	}
	return evaluation.HumanScoreRecorded{Score: score, Result: result, HeadlineScore: current}, nil
}

// MarkLimitReached records the run limit a run reached (`tokens` or `cost`).
func (r *EvalRunsRepo) MarkLimitReached(ctx context.Context, projectID, runID, limit string) error {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	id, err := parseEvalID(runID, "run")
	if err != nil {
		return err
	}
	if _, err := r.pool.Exec(ctx, fmt.Sprintf(
		`UPDATE %s.eval_runs SET limit_reached = $2 WHERE id = $1`, schema), id, limit); err != nil {
		return fmt.Errorf("eval runs: mark limit: %w", err)
	}
	return nil
}

// RecordCaseRun stores the runtime execution a case ran as
// (tenant/0246 eval_case_runs), so the gateway calls signed with that
// execution id are attributed to the run and the case. It is idempotent on
// (run, execution): the orchestrator may report the same admission twice.
func (r *EvalRunsRepo) RecordCaseRun(ctx context.Context, projectID, runID, caseID, executionID, conversationUUID string) error {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	run, err := parseEvalID(runID, "run")
	if err != nil {
		return err
	}
	caseNumber, err := parseEvalID(caseID, "case")
	if err != nil {
		return err
	}
	if _, err := r.pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.eval_case_runs (run_id, dataset_case_id, execution_id, conversation_uuid)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid)
		ON CONFLICT (run_id, execution_id) DO NOTHING`, schema),
		run, caseNumber, executionID, conversationUUID); err != nil {
		return fmt.Errorf("eval runs: record case run: %w", err)
	}
	return nil
}
