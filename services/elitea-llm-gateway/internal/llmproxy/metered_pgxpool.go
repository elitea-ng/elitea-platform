package llmproxy

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolMeteredLedger reads the usage ledger for the metered settle's duplicate
// flag (metered.go). The lookup is on the ledger's primary key.
type PoolMeteredLedger struct {
	Pool *pgxpool.Pool
}

var _ MeteredLedger = (*PoolMeteredLedger)(nil)

// MeteredEventBilled reports whether gateway.llm_usage_events holds eventID.
func (l *PoolMeteredLedger) MeteredEventBilled(ctx context.Context, eventID string) (bool, error) {
	var exists bool
	err := l.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM gateway.llm_usage_events WHERE event_id = $1::uuid)`, eventID).Scan(&exists)
	return exists, err
}
