package repos

import (
	"context"
	"errors"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
)

type currentNodeRecoveryVisit struct {
	activation string
	revision   uint64
}

// Caller must hold the exact job lock. All statuses participate in current visit
// selection; historical eligible actions may not supersede a newer stopped visit.
func lockLatestNodeRecoveryVisit(ctx context.Context, tx sqlExecutor, executionID string, generation uint64) (currentNodeRecoveryVisit, error) {
	var current currentNodeRecoveryVisit
	err := tx.QueryRow(ctx, `SELECT activation_id,journal_revision FROM elitea_runtime.node_recovery_visits
WHERE execution_id=$1 AND generation=$2 ORDER BY created_at DESC,journal_revision DESC LIMIT 1 FOR UPDATE`, executionID, int64(generation)).Scan(&current.activation, &current.revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return currentNodeRecoveryVisit{}, storage.ErrContentRejected
	}
	return current, err
}
