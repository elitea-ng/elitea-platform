package repos

import "context"

// The caller already owns the response authorization and cancellation transaction.
func cancelNodeRecovery(ctx context.Context, tx sqlExecutor, executionID string, generation uint64) error {
	_, err := tx.Exec(ctx, `INSERT INTO elitea_runtime.node_recovery_audit
(execution_id,generation,request_id,transition,actor_id,activation_id,journal_revision)
SELECT execution_id,generation,request_id,'CANCELLED',actor_id,activation_id,expected_revision
FROM elitea_runtime.node_recovery_control_outbox WHERE execution_id=$1 AND generation=$2 AND consumed_at IS NULL
ON CONFLICT DO NOTHING`, executionID, int64(generation))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE elitea_runtime.node_recovery_visits SET status='CANCELLED',updated_at=clock_timestamp()
WHERE execution_id=$1 AND generation=$2 AND status IN ('SUSPENDED','AUTHORIZED')`, executionID, int64(generation))
	return err
}
