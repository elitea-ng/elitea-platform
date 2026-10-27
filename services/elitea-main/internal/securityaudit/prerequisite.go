package securityaudit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

// OutboxMigrationVersion is the shared migration that creates the outbox.
const OutboxMigrationVersion = 183

// Querier is the read slice of *pgxpool.Pool.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrOutboxMissing refuses a governed boot without the outbox schema.
var ErrOutboxMissing = errors.New("security audit outbox is not installed")

// OutboxPrerequisite refuses governed boot (and revision publishing) when the
// 0183 migration is not applied or the durable store lost its append-only
// trigger. Build it only with the governance flag on.
type OutboxPrerequisite struct {
	db Querier
}

var _ profile.Prerequisite = OutboxPrerequisite{}

// NewOutboxPrerequisite builds the prerequisite.
func NewOutboxPrerequisite(db Querier) (OutboxPrerequisite, error) {
	if db == nil {
		return OutboxPrerequisite{}, errors.New("security audit prerequisite needs a database")
	}
	return OutboxPrerequisite{db: db}, nil
}

// Name names the prerequisite in a boot refusal.
func (OutboxPrerequisite) Name() string { return "security-audit-outbox" }

const outboxInstalledSQL = `
SELECT
    EXISTS (
        SELECT 1 FROM elitea_runtime.schema_migrations
         WHERE target_kind = 'shared' AND target_id = 'platform' AND version = $1
    ),
    to_regclass('elitea_runtime.security_audit_heads') IS NOT NULL
        AND to_regclass('elitea_runtime.security_audit_outbox') IS NOT NULL
        AND to_regclass('elitea_identity.security_audit_events') IS NOT NULL,
    EXISTS (
        SELECT 1 FROM pg_trigger
         WHERE tgname = 'security_audit_events_append_only'
           AND tgrelid = to_regclass('elitea_identity.security_audit_events')
           AND tgenabled <> 'D'
    )`

// Check reads the ledger and the catalogue. revision must carry a backlog
// alert threshold; the relay refuses to start without one.
func (p OutboxPrerequisite) Check(ctx context.Context, revision profile.PolicyRevision) error {
	if p.db == nil {
		return errors.New("security audit prerequisite is not configured")
	}
	if revision.Document.AuditBacklogAlertThreshold <= 0 {
		return fmt.Errorf("%w: revision %q has no audit backlog alert threshold", ErrOutboxMissing, revision.ID)
	}
	var applied, tables, trigger bool
	if err := p.db.QueryRow(ctx, outboxInstalledSQL, OutboxMigrationVersion).Scan(&applied, &tables, &trigger); err != nil {
		return fmt.Errorf("%w: read the migration ledger: %w", ErrOutboxMissing, err)
	}
	switch {
	case !applied:
		return fmt.Errorf("%w: shared migration %04d is not applied", ErrOutboxMissing, OutboxMigrationVersion)
	case !tables:
		return fmt.Errorf("%w: an outbox table is missing", ErrOutboxMissing)
	case !trigger:
		return fmt.Errorf("%w: the durable store's append-only trigger is missing or disabled", ErrOutboxMissing)
	}
	return nil
}
