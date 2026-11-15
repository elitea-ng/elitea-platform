package marketplace

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SnapshotRetentionDays is how long a withdrawn snapshot that no project holds
// is kept before the purge deletes it (ADR-0051 decision 2, the retention of
// ADR-0042).
const SnapshotRetentionDays = 30

// RetentionPass is one run of the retention routine: it deletes the withdrawn
// snapshots no project holds, and the listings left empty. It answers how many
// snapshots it deleted.
func RetentionPass(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) (int64, error) {
	removed, err := PurgeUnheld(ctx, pool, SnapshotRetentionDays)
	if err != nil {
		return removed, err
	}
	if removed > 0 {
		logger.Info("marketplace retention: snapshots purged", "removed", removed)
	}
	return removed, nil
}
