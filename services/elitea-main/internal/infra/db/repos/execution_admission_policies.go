package repos

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AdmissionCapCommand is the operator command that brings the persisted
// admission cap to a new value in one step (#966). It ships in the elitea-main
// image beside elitea-migrate, reads DATABASE_URL, and is named in every
// mismatch error so the operator who meets one is told the fix.
const AdmissionCapCommand = "elitea-admission-ctl set -max <cap>"

// admissionPolicyMismatchError is the one fail-closed message every admission
// path returns when this replica's ELITEA_RUNTIME_MAX_OUTSTANDING differs from
// the cap persisted for the capability. persisted < 1 means the path could not
// read it (the agent start path learns of the mismatch from its trigger).
//
// The admission fails closed by design: two replicas must not admit under two
// different ceilings. What the message adds is the two-sided fix — every
// replica on one value, and the persisted row on the same value — so a cap
// change is not the silent two-step trap the 1k-flow load test hit.
func admissionPolicyMismatchError(capabilityID string, configured, persisted int64) error {
	persistedText := "unknown"
	if persisted > 0 {
		persistedText = fmt.Sprintf("%d", persisted)
	}
	return fmt.Errorf(
		"%w: capability %q configured=%d persisted=%s; set ELITEA_RUNTIME_MAX_OUTSTANDING to the same value on every elitea-main replica, then run `%s` once with that value (deploy/runtime/README.md, \"Changing the admission cap\")",
		ErrAdmissionPolicyMismatch, capabilityID, configured, persistedText, AdmissionCapCommand,
	)
}

// AdmissionPolicy is one persisted per-capability admission cap.
type AdmissionPolicy struct {
	CapabilityID   string
	MaxOutstanding int64
}

// AdmissionPolicyQueryer is what the admission-policy helpers need: a pgx pool
// or transaction.
type AdmissionPolicyQueryer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ListAdmissionPolicies returns every persisted admission cap, ordered by
// capability. A capability with no row has never admitted: its first start
// persists the starting replica's configured cap.
func ListAdmissionPolicies(ctx context.Context, q AdmissionPolicyQueryer) ([]AdmissionPolicy, error) {
	rows, err := q.Query(ctx, `
SELECT capability_id, max_outstanding
FROM elitea_runtime.execution_admission_policies
ORDER BY capability_id`)
	if err != nil {
		return nil, fmt.Errorf("list execution admission policies: %w", err)
	}
	defer rows.Close()
	var policies []AdmissionPolicy
	for rows.Next() {
		var policy AdmissionPolicy
		if err := rows.Scan(&policy.CapabilityID, &policy.MaxOutstanding); err != nil {
			return nil, fmt.Errorf("scan execution admission policy: %w", err)
		}
		policies = append(policies, policy)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list execution admission policies: %w", err)
	}
	return policies, nil
}

// SetAdmissionPolicyMax persists maxOutstanding as the admission cap of
// capabilityID, or of every persisted capability when capabilityID is empty,
// in one statement. A named capability with no row gets one, so the cap is
// right before its first start. It returns the number of rows written.
//
// The statement takes the same row locks an admission takes, so it waits for
// in-flight admissions and the next one reads the new value. Replicas still
// configured with the old value fail closed from that moment until they run
// the new one, which is why the procedure changes the replicas first.
func SetAdmissionPolicyMax(ctx context.Context, q AdmissionPolicyQueryer, capabilityID string, maxOutstanding int64) (int64, error) {
	if maxOutstanding < 1 || maxOutstanding > MaxSupportedOutstandingJobs {
		return 0, fmt.Errorf("admission cap %d is outside 1..%d", maxOutstanding, MaxSupportedOutstandingJobs)
	}
	if capabilityID == "" {
		tag, err := q.Exec(ctx, `
UPDATE elitea_runtime.execution_admission_policies
SET max_outstanding = $1`, maxOutstanding)
		if err != nil {
			return 0, fmt.Errorf("set execution admission caps: %w", err)
		}
		return tag.RowsAffected(), nil
	}
	tag, err := q.Exec(ctx, `
INSERT INTO elitea_runtime.execution_admission_policies (capability_id, max_outstanding)
VALUES ($1, $2)
ON CONFLICT (capability_id) DO UPDATE SET max_outstanding = EXCLUDED.max_outstanding`,
		capabilityID, maxOutstanding)
	if err != nil {
		return 0, fmt.Errorf("set execution admission cap for %q: %w", capabilityID, err)
	}
	return tag.RowsAffected(), nil
}
