package admin_test

import (
	"context"
	"io/fs"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// securityAuditOutboxMigration is the security audit outbox the role write
// paths append their admin.access.changed events to (ADR-0059 R1.5). A role
// write fails closed without it, so every fixture that writes roles applies
// the real migration.
const securityAuditOutboxMigration = "shared/0183_security_audit_outbox.sql"

func applySecurityAuditOutbox(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	body, err := fs.ReadFile(migrations.Files, securityAuditOutboxMigration)
	if err != nil {
		t.Fatalf("read %s: %v", securityAuditOutboxMigration, err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply %s: %v", securityAuditOutboxMigration, err)
	}
}
