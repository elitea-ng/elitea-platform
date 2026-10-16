package repos

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cloudExecutorAttestationLock serialises the attestation audit across
// replicas that start at the same time.
const cloudExecutorAttestationLock = "elitea.cloud_executor.storage_encryption_attested"

// CloudExecutorAttestationAction is the audit action of an operator's
// attestation of storage encryption at rest for the named physical buckets.
func CloudExecutorAttestationAction(buckets []string) string {
	return "cloud_executor.storage_encryption_attested, attested by operator " +
		"(ELITEA_CLOUD_EXECUTOR_STORAGE_ENCRYPTION_ATTESTED), buckets " + strings.Join(buckets, ",")
}

// RecordCloudExecutorEncryptionAttestation writes the attestation to the
// audit trail ONCE for a set of buckets: a restart, or a second replica, finds
// the row and writes nothing. It answers whether this call wrote it.
//
// The write is direct, not through the asynchronous audit recorder, because
// the check and the insert must be one locked transaction.
func RecordCloudExecutorEncryptionAttestation(ctx context.Context, pool *pgxpool.Pool, buckets []string) (bool, error) {
	if pool == nil {
		return false, fmt.Errorf("cloud executor attestation: nil pool")
	}
	action := CloudExecutorAttestationAction(buckets)
	if len([]rune(action)) > 512 {
		action = string([]rune(action)[:511]) + "…"
	}
	recorded := false
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, cloudExecutorAttestationLock); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
INSERT INTO centry.audit_events (timestamp, event_type, action, status_code, is_error, entity_type, entity_name)
SELECT clock_timestamp(), 'admin_task', $1::varchar, 200, false, 'cloud_executor', 'storage_encryption'
WHERE NOT EXISTS (
    SELECT 1 FROM centry.audit_events
    WHERE entity_type = 'cloud_executor' AND entity_name = 'storage_encryption' AND action = $1::varchar
)`, action)
		if err != nil {
			return err
		}
		recorded = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("cloud executor attestation: %w", err)
	}
	return recorded, nil
}
