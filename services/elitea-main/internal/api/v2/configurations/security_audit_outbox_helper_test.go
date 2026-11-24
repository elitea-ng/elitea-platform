package configurations_test

import (
	"context"
	"io/fs"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// applySecurityAuditOutbox applies the security audit outbox the global
// provider routes append their admin.access.changed events to (ADR-0059
// R1.5). A global provider write fails closed without it.
func applySecurityAuditOutbox(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const file = "shared/0183_security_audit_outbox.sql"
	body, err := fs.ReadFile(platformmigrations.Files, file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply %s: %v", file, err)
	}
}
