package requestlog

// The guardrail verdict sink (ADR-0043 decision 1, shared migration 0214).
//
// A guardrail verdict is a step record of the run (ADR-0042) when the request
// carries an execution id. The gateway does not write elitea_runtime.run_steps:
// that table is a hash chain elitea-main owns, and the gateway has no internal
// channel to elitea-main. So the gateway writes its verdicts to its own table,
// gateway.guardrail_verdicts, keyed by execution_id, and elitea-main joins the
// table into the run's step evidence when it reads a run (runevidence). A
// request without an execution id still gets its verdict rows, with a NULL
// execution_id, next to its request log row.
//
// The verdicts ride the request log's batch: no second queue and no second
// writer. They are written after the request log rows, and their failure does
// not fail the request log batch. They follow the request log's retention
// window (the `gateway_request_log` retention class).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const verdictInsertColumns = `(occurred_at, project_id, user_id, execution_id, route, stage, row_name,
	detector, detector_type, action, outcome, fail_mode, labels, findings, max_score, latency_ms, runs)`

const verdictColumnsPerRow = 17

// maxVerdictsPerBatch bounds one INSERT: 17 columns × 3000 rows stays far
// below the 65535 bind parameters Postgres accepts.
const maxVerdictsPerBatch = 3000

// isUndefinedTable reports Postgres 42P01, the code a missing table raises.
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// writeVerdicts inserts the verdicts of the batch. A database without the
// table (shared 0214 not applied yet) latches the skip for
// legacyColumnsRetry, and the verdicts of that window are dropped: the request
// log rows are already written and must not be lost with them.
func (s *Store) writeVerdicts(ctx context.Context, records []Record) error {
	if s.latched(&s.verdictsUntil) {
		return nil
	}
	var (
		placeholders []string
		args         []any
	)
	flush := func() error {
		if len(placeholders) == 0 {
			return nil
		}
		statement := fmt.Sprintf(`INSERT INTO gateway.guardrail_verdicts %s VALUES %s`,
			verdictInsertColumns, strings.Join(placeholders, ","))
		_, err := s.db.Exec(ctx, statement, args...)
		placeholders, args = placeholders[:0], args[:0]
		if err != nil {
			if isUndefinedTable(err) {
				s.verdictsUntil.Store(s.now().Add(legacyColumnsRetry).UnixNano())
				return nil
			}
			return fmt.Errorf("write guardrail verdicts: %w", err)
		}
		return nil
	}
	for _, record := range records {
		for _, v := range record.Guardrails {
			base := len(placeholders) * verdictColumnsPerRow
			slots := make([]string, verdictColumnsPerRow)
			for column := range slots {
				slots[column] = "$" + strconv.Itoa(base+column+1)
			}
			placeholders = append(placeholders, "("+strings.Join(slots, ",")+")")
			labels := v.Labels
			if labels == nil {
				labels = []string{}
			}
			args = append(args,
				record.OccurredAt,
				nullableID(record.ProjectID),
				nullableID(record.UserID),
				nullableExecutionID(record.ExecutionID),
				truncate(record.Route, 128),
				truncate(v.Stage, 16),
				truncate(v.Row, 255),
				truncate(v.Detector, 64),
				truncate(v.DetectorType, 16),
				truncate(v.Action, 16),
				truncate(v.Outcome, 16),
				truncate(v.FailMode, 8),
				labels,
				v.Findings,
				v.MaxScore,
				v.LatencyMS,
				v.Runs,
			)
			if len(placeholders) >= maxVerdictsPerBatch {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	return flush()
}

// pruneVerdicts deletes verdicts older than the request log window, bounded
// like Prune.
func (s *Store) pruneVerdicts(ctx context.Context, olderThan time.Time) (int64, error) {
	if s.latched(&s.verdictsUntil) {
		return 0, nil
	}
	tag, err := s.db.Exec(ctx, `
		DELETE FROM gateway.guardrail_verdicts
		 WHERE id IN (
		       SELECT id FROM gateway.guardrail_verdicts
		        WHERE occurred_at < $1
		        LIMIT 10000)`, olderThan)
	if err != nil {
		if isUndefinedTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("prune guardrail verdicts: %w", err)
	}
	return tag.RowsAffected(), nil
}
