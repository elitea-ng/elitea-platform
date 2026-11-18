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

// GuardrailNoticeVerdict is the guardrail verdicts of a run that a chat notice
// reports, aggregated per (stage, entry, detector, action, outcome): the person
// who sent the message must be told when the gateway blocked the request,
// masked part of it, warned about it, or could not run a guardrail whose fail
// mode is closed. Like GuardrailVerdict it holds no content.
type GuardrailNoticeVerdict struct {
	// OccurredAt is the first verdict of the group.
	OccurredAt time.Time
	Stage      string
	Row        string
	Detector   string
	Action     string
	Outcome    string
	// Labels are the distinct labels of the group, sorted.
	Labels []string
	// Requests is the number of verdict rows (LLM requests) in the group.
	Requests int
}

// GuardrailNoticeOutcomes are the verdict outcomes a chat notice reports. A
// pass, a flag and a failure under fail mode open say nothing to the person:
// a flag is an audit record by definition, and an open failure changed
// nothing.
var GuardrailNoticeOutcomes = []string{"blocked", "masked", "warned", "error_closed"}

// GuardrailNoticeVerdicts reads the notice verdicts of a run for ONE user: the
// run is read by execution id (the run id) and the caller's own user id, so a
// member of the project reads the notices of their own runs only. The groups
// are built in SQL, so a long run with many requests can never push a blocking
// outcome out of the limit: the number of rows is bounded by the number of
// guardrail entries, not by the number of requests. Oldest group first; verdicts
// written in one statement share occurred_at, so the first row id breaks the tie
// in write order.
func (s *RunSteps) GuardrailNoticeVerdicts(ctx context.Context, projectID, userID int64, runID string, limit int) ([]GuardrailNoticeVerdict, error) {
	rows, err := s.pool.Query(ctx, `
SELECT stage, row_name, detector, action, outcome,
       min(occurred_at) AS first_at,
       count(DISTINCT id) AS requests,
       COALESCE(array_agg(DISTINCT label ORDER BY label) FILTER (WHERE label IS NOT NULL), '{}') AS labels
  FROM gateway.guardrail_verdicts v
  LEFT JOIN LATERAL unnest(v.labels) AS label ON true
 WHERE execution_id = $1 AND project_id = $2 AND user_id = $3 AND outcome = ANY($4)
 GROUP BY stage, row_name, detector, action, outcome
 ORDER BY first_at, min(id)
 LIMIT $5`, runID, projectID, userID, GuardrailNoticeOutcomes, limit)
	if err != nil {
		return nil, fmt.Errorf("run steps: read guardrail notices: %w", err)
	}
	verdicts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (GuardrailNoticeVerdict, error) {
		var v GuardrailNoticeVerdict
		err := row.Scan(&v.Stage, &v.Row, &v.Detector, &v.Action, &v.Outcome, &v.OccurredAt, &v.Requests, &v.Labels)
		return v, err
	})
	if err != nil {
		return nil, fmt.Errorf("run steps: read guardrail notices: %w", err)
	}
	return verdicts, nil
}
