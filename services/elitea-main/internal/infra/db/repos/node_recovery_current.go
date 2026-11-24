package repos

import (
	"context"
	"errors"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
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
WHERE execution_id=$1 AND generation=$2 ORDER BY created_at DESC,journal_revision DESC,graph_thread ASC,activation_id DESC LIMIT 1 FOR UPDATE`, executionID, int64(generation)).Scan(&current.activation, &current.revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return currentNodeRecoveryVisit{}, storage.ErrContentRejected
	}
	return current, err
}

// Caller must hold the exact job lock. Selects and locks the latest visit on the
// target activation's own graph_thread (FU-071) across all statuses so sibling
// fan-out threads do not block each other while historical visits on the same
// thread still cannot supersede a newer visit on that thread.
func lockLatestNodeRecoveryVisitForActivation(ctx context.Context, tx sqlExecutor, executionID string, generation uint64, activationID string, expectedRevision uint64) (currentNodeRecoveryVisit, error) {
	var current currentNodeRecoveryVisit
	err := tx.QueryRow(ctx, `SELECT latest.activation_id,latest.journal_revision
FROM elitea_runtime.node_recovery_visits target
JOIN LATERAL (
  SELECT activation_id,journal_revision
  FROM elitea_runtime.node_recovery_visits
  WHERE execution_id=target.execution_id AND generation=target.generation AND graph_thread=target.graph_thread
  ORDER BY created_at DESC,journal_revision DESC,activation_id DESC LIMIT 1 FOR UPDATE
) latest ON TRUE
WHERE target.execution_id=$1 AND target.generation=$2 AND target.activation_id=$3 AND target.journal_revision=$4`, executionID, int64(generation), activationID, int64(expectedRevision)).Scan(&current.activation, &current.revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return currentNodeRecoveryVisit{}, storage.ErrContentRejected
	}
	return current, err
}

// Caller must hold the exact job lock. Selects and locks the latest visit on the
// graph_thread of the recovery action whose owner proof names (contentID, version),
// across all statuses on that thread so a newer STOPPED/CANCELLED visit on the same
// thread still blocks reading an older result while sibling threads do not collide.
func lockLatestNodeRecoveryVisitForResult(ctx context.Context, tx sqlExecutor, executionID string, generation uint64, contentID, version string) (currentNodeRecoveryVisit, error) {
	var current currentNodeRecoveryVisit
	err := tx.QueryRow(ctx, `SELECT latest.activation_id,latest.journal_revision
FROM elitea_runtime.node_recovery_visits target
JOIN elitea_runtime.node_recovery_control_outbox action
 ON action.execution_id=target.execution_id AND action.generation=target.generation
 AND action.activation_id=target.activation_id AND action.expected_revision=target.journal_revision
JOIN LATERAL (
  SELECT activation_id,journal_revision
  FROM elitea_runtime.node_recovery_visits
  WHERE execution_id=target.execution_id AND generation=target.generation AND graph_thread=target.graph_thread
  ORDER BY created_at DESC,journal_revision DESC,activation_id DESC LIMIT 1 FOR UPDATE
) latest ON TRUE
WHERE target.execution_id=$1 AND target.generation=$2
 AND ((action.action_json->'owner_proof'->'result_ref'->>'content_id'=$3 AND action.action_json->'owner_proof'->'result_ref'->>'immutable_version'=$4)
  OR (action.action_json->'owner_proof'->'owner_receipt_ref'->>'content_id'=$3 AND action.action_json->'owner_proof'->'owner_receipt_ref'->>'immutable_version'=$4))
ORDER BY target.created_at DESC,target.journal_revision DESC,target.graph_thread ASC,target.activation_id DESC LIMIT 1`, executionID, int64(generation), contentID, version).Scan(&current.activation, &current.revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return currentNodeRecoveryVisit{}, storage.ErrContentRejected
	}
	return current, err
}
