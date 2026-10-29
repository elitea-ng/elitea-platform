// Package retentionpolicy reads the administrator's retention policy
// (elitea_runtime.retention_policy, elitea-main shared 0195, ADR-0042
// decision 4) for the audit sweepers of this daemon.
//
// Only a platform row an administrator has SAVED counts (updated_by NOT
// NULL). The seeded rows restate the deployment defaults, and following them
// would override an operator who tuned AUDIT_RETENTION_DAYS or
// SECURITY_AUDIT_OUTBOX_RETENTION_DAYS before the policy existed. Until the
// row is saved, the deployment configuration applies, exactly as before.
//
// The table and column names are a DATABASE contract with elitea-main, which
// this module cannot import (internal/ of another module), the same reason
// internal/maintenance restates the maintenance switch's coordinates.
package retentionpolicy

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The data classes this daemon enforces.
const (
	ClassAdminAudit    = "admin_audit"
	ClassSecurityAudit = "security_audit"
)

// Querier is the slice of *pgxpool.Pool the reader uses.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Decision is what the policy says for one pass.
type Decision struct {
	// Set is true when an administrator saved the platform row; false means
	// the deployment configuration applies.
	Set bool
	// MaxAgeDays is the saved maximum; nil keeps forever. Meaningful only when
	// Set.
	MaxAgeDays *int
}

// Reader returns the policy of one data class, read once per pass.
type Reader func(ctx context.Context) (Decision, error)

const readSQL = `
	SELECT max_age_days
	  FROM elitea_runtime.retention_policy
	 WHERE scope = 'platform' AND data_class = $1 AND updated_by IS NOT NULL`

// NewReader reads class from q. A database without the table (elitea-main
// below shared 0195) reads as "not set".
func NewReader(q Querier, class string) Reader {
	return func(ctx context.Context) (Decision, error) {
		var present bool
		if err := q.QueryRow(ctx, `SELECT to_regclass('elitea_runtime.retention_policy') IS NOT NULL`).Scan(&present); err != nil {
			return Decision{}, fmt.Errorf("retention policy: %w", err)
		}
		if !present {
			return Decision{}, nil
		}
		var maxDays *int
		err := q.QueryRow(ctx, readSQL, class).Scan(&maxDays)
		if errors.Is(err, pgx.ErrNoRows) {
			return Decision{}, nil
		}
		if err != nil {
			return Decision{}, fmt.Errorf("retention policy: %w", err)
		}
		return Decision{Set: true, MaxAgeDays: maxDays}, nil
	}
}
