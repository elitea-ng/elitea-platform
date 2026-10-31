package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GuardrailVerdict is one row of gateway.guardrail_verdicts (migration 0214,
// ADR-0043 decision 1): the result of one guardrail detector on one LLM
// request of a run. The row holds no content: no inspected text and no
// matched text.
type GuardrailVerdict struct {
	OccurredAt   time.Time
	Route        string
	Stage        string
	Row          string
	Detector     string
	DetectorType string
	Action       string
	Outcome      string
	FailMode     string
	Labels       []string
	Findings     int32
	MaxScore     float32
	LatencyMS    int64
	Runs         int32
}

// GuardrailVerdicts reads up to limit of the guardrail verdicts the LLM
// gateway recorded for a run, oldest first. The gateway writes them with its
// request log, keyed by the execution id, which is the run id. They are not
// part of the run's hash chain: the gateway has no channel to append to it.
func (s *RunSteps) GuardrailVerdicts(ctx context.Context, projectID int64, runID string, limit int) ([]GuardrailVerdict, error) {
	rows, err := s.pool.Query(ctx, `
SELECT occurred_at, route, stage, row_name, detector, detector_type, action, outcome,
       fail_mode, labels, findings, max_score, latency_ms, runs
  FROM gateway.guardrail_verdicts
 WHERE execution_id = $1 AND project_id = $2
 ORDER BY occurred_at, id
 LIMIT $3`, runID, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("run steps: read guardrail verdicts: %w", err)
	}
	verdicts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (GuardrailVerdict, error) {
		var v GuardrailVerdict
		err := row.Scan(&v.OccurredAt, &v.Route, &v.Stage, &v.Row, &v.Detector, &v.DetectorType, &v.Action,
			&v.Outcome, &v.FailMode, &v.Labels, &v.Findings, &v.MaxScore, &v.LatencyMS, &v.Runs)
		return v, err
	})
	if err != nil {
		return nil, fmt.Errorf("run steps: read guardrail verdicts: %w", err)
	}
	return verdicts, nil
}
