package repos

// The egress proxy's per-turn audit summaries, shared by every Main replica
// (elitea_runtime.egress_run_summaries, shared/0169). It is the Store of
// application/egressaudit: see that package's doc for what a row is and when a
// turn is due.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/egressaudit"
)

// EgressRunSummaries is the Postgres application/egressaudit.Store.
type EgressRunSummaries struct{ pool *pgxpool.Pool }

var _ app.Store = (*EgressRunSummaries)(nil)

// NewEgressRunSummaries returns a store over pool, or nil when pool is nil.
func NewEgressRunSummaries(pool *pgxpool.Pool) *EgressRunSummaries {
	if pool == nil {
		return nil
	}
	return &EgressRunSummaries{pool: pool}
}

// Upsert replaces one replica's snapshot of one run. A row already claimed
// for an audit event is re-opened as a new turn (a resumed turn reuses the
// execution id); a non-final report never overwrites a final one of the same
// open turn.
func (s *EgressRunSummaries) Upsert(ctx context.Context, report app.Report, now time.Time) error {
	reached, err := json.Marshal(nonNil(report.HostsReached))
	if err != nil {
		return fmt.Errorf("encode the hosts reached: %w", err)
	}
	refused, err := json.Marshal(nonNil(report.HostsRefused))
	if err != nil {
		return fmt.Errorf("encode the hosts refused: %w", err)
	}
	var expires *time.Time
	if !report.TokenExpiresAt.IsZero() {
		expires = &report.TokenExpiresAt
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO elitea_runtime.egress_run_summaries AS t (
    execution_id, replica, project_id, final, token_expires_at,
    connections, bytes_up, bytes_down, other_hosts,
    hosts_reached, hosts_refused, first_reported_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12, $12)
ON CONFLICT (execution_id, replica) DO UPDATE SET
    project_id        = EXCLUDED.project_id,
    final             = EXCLUDED.final,
    token_expires_at  = EXCLUDED.token_expires_at,
    connections       = EXCLUDED.connections,
    bytes_up          = EXCLUDED.bytes_up,
    bytes_down        = EXCLUDED.bytes_down,
    other_hosts       = EXCLUDED.other_hosts,
    hosts_reached     = EXCLUDED.hosts_reached,
    hosts_refused     = EXCLUDED.hosts_refused,
    first_reported_at = CASE WHEN t.audited_at IS NULL
                             THEN t.first_reported_at ELSE EXCLUDED.first_reported_at END,
    updated_at        = EXCLUDED.updated_at,
    audited_at        = NULL
WHERE t.audited_at IS NOT NULL OR EXCLUDED.final OR NOT t.final`,
		report.ExecutionID, report.ReplicaID, report.ProjectID, report.Final || report.ReplicaShutdown, expires,
		report.Connections, report.BytesUp, report.BytesDown, report.OtherHosts,
		reached, refused, now); err != nil {
		return fmt.Errorf("store the egress run summary: %w", err)
	}
	return nil
}

// ClaimDue claims the due turns with one UPDATE ... RETURNING. Two Main
// replicas sweeping at once: the second UPDATE waits on the first's row locks,
// re-reads each row after the commit, finds audited_at set and skips it, so
// each row reaches exactly one caller.
func (s *EgressRunSummaries) ClaimDue(ctx context.Context, now time.Time, settle, maxHold time.Duration) ([]app.Row, error) {
	rows, err := s.pool.Query(ctx, `
WITH due AS (
    SELECT execution_id
    FROM elitea_runtime.egress_run_summaries
    WHERE audited_at IS NULL
    GROUP BY execution_id
    HAVING (bool_and(final) AND max(updated_at) <= $1::timestamptz - make_interval(secs => $2::float8))
        OR max(token_expires_at) <= $1::timestamptz - make_interval(secs => $3::float8)
)
UPDATE elitea_runtime.egress_run_summaries AS s
SET audited_at = $1
FROM due
WHERE s.execution_id = due.execution_id
  AND s.audited_at IS NULL
RETURNING s.execution_id, s.replica, s.project_id, s.final, s.token_expires_at,
          s.connections, s.bytes_up, s.bytes_down, s.other_hosts,
          s.hosts_reached, s.hosts_refused, s.first_reported_at, s.updated_at`,
		now, settle.Seconds(), maxHold.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim the due egress turns: %w", err)
	}
	defer rows.Close()
	var claimed []app.Row
	for rows.Next() {
		var (
			row             app.Row
			expires         *time.Time
			reached, refuse []byte
		)
		if err := rows.Scan(&row.ExecutionID, &row.ReplicaID, &row.ProjectID, &row.Final, &expires,
			&row.Connections, &row.BytesUp, &row.BytesDown, &row.OtherHosts,
			&reached, &refuse, &row.FirstReport, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("read a claimed egress summary: %w", err)
		}
		if expires != nil {
			row.TokenExpiresAt = *expires
		}
		if err := json.Unmarshal(reached, &row.HostsReached); err != nil {
			return nil, fmt.Errorf("decode the hosts reached: %w", err)
		}
		if err := json.Unmarshal(refuse, &row.HostsRefused); err != nil {
			return nil, fmt.Errorf("decode the hosts refused: %w", err)
		}
		claimed = append(claimed, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim the due egress turns: %w", err)
	}
	return claimed, nil
}

// DeleteOlderThan deletes the rows last updated before cutoff.
func (s *EgressRunSummaries) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM elitea_runtime.egress_run_summaries WHERE updated_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete the expired egress summaries: %w", err)
	}
	return tag.RowsAffected(), nil
}

func nonNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}
