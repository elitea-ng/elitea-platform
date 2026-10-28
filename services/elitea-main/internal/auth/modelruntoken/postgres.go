package modelruntoken

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Querier is the one PostgreSQL call the lease reader makes (*pgxpool.Pool).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PostgresLeases reads claim liveness from elitea_runtime.execution_claims.
type PostgresLeases struct {
	store Querier
}

// NewPostgresLeases returns the reader over store.
func NewPostgresLeases(store Querier) (*PostgresLeases, error) {
	if store == nil {
		return nil, errors.New("modelruntoken: the lease store is required")
	}
	return &PostgresLeases{store: store}, nil
}

// ClaimLive is true while the bound claim is unreleased and its lease has
// not expired, with the execution's project and actor the token names. It
// uses the same lease predicates every claim-bound content read uses; a
// renewal moves lease_expires_at and keeps claim id and fence, so it keeps
// the answer true.
func (l *PostgresLeases) ClaimLive(ctx context.Context, binding Binding) (bool, error) {
	var live bool
	err := l.store.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1
  FROM elitea_runtime.execution_claims AS c
  JOIN elitea_runtime.execution_jobs AS j
    ON j.execution_id = c.execution_id AND j.generation = c.generation
  WHERE c.claim_id = $1
    AND c.execution_id = $2
    AND c.generation = $3
    AND c.fence_token = $4
    AND c.released_at IS NULL
    AND c.lease_expires_at > clock_timestamp()
    AND j.resource_project_id = $5
    AND j.actor_id = $6
)`,
		binding.ClaimID, binding.ExecutionID, int64(binding.Generation), binding.FenceToken,
		binding.ProjectID, strconv.FormatInt(binding.UserID, 10),
	).Scan(&live)
	if err != nil {
		return false, err
	}
	return live, nil
}
