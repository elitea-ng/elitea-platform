package migrations_test

// Shared 0269 (issue 2515, ADR-0059 R2.1a): elitea_auth.admin_elevations, the
// elevation ledger. It applies on the bootstrap schema with the corpus, on
// the legacy RBAC matrix fixture, and on a database without auth_core; each
// time twice, with no change the second time.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const adminElevationsMigration = "shared/0269_admin_elevations.sql"

func TestAdminElevationsAppliesTwiceWithNoChange(t *testing.T) {
	for name, build := range map[string]func(*testing.T) *pgxpool.Pool{
		"bootstrap and corpus": newMigratedPool,
		"legacy matrix": func(t *testing.T) *pgxpool.Pool {
			pool := newEmptyDatabase(t)
			applyBootstrapSchema(t, pool)
			loadLegacyRBACMatrix(t, pool)
			applySharedCorpus(t, pool)
			return pool
		},
		"no auth_core": func(t *testing.T) *pgxpool.Pool {
			pool := newEmptyDatabase(t)
			execMigrationFile(t, pool, adminElevationsMigration)
			return pool
		},
	} {
		t.Run(name, func(t *testing.T) {
			pool := build(t)
			before := adminElevationsShape(t, pool)
			if !strings.Contains(before, "admin_elevations_append_only") {
				t.Fatalf("0269 created no append-only trigger:\n%s", before)
			}
			var rowID string
			queryRow(t, pool, `
INSERT INTO elitea_auth.admin_elevations
    (user_id, browser_session_ref, level, path, reason_code, reason, requester_user_id,
     approver_user_id, requested_at, decision, decided_at, started_at, expires_at)
VALUES (7, 41, 'admin', 'approved', 'incident', 'r', 7, 8, now(), 'approved', now(), now(), now() + interval '1 hour')
RETURNING id::text`, &rowID)

			execMigrationFile(t, pool, adminElevationsMigration)

			if after := adminElevationsShape(t, pool); after != before {
				t.Fatalf("re-applying 0269 changed the table:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			var rows int
			queryRow(t, pool, `SELECT count(*) FROM elitea_auth.admin_elevations`, &rows)
			if rows != 1 {
				t.Fatalf("re-applying 0269 left %d rows, want 1", rows)
			}
			if _, err := pool.Exec(context.Background(),
				`UPDATE elitea_auth.admin_elevations SET reason = 'other' WHERE id = $1::uuid`, rowID); err == nil ||
				!strings.Contains(err.Error(), "append-only") {
				t.Fatalf("after a re-apply the trigger does not refuse an UPDATE of the reason: %v", err)
			}
		})
	}
}

// adminElevationsShape is the table's columns, constraints, indexes, triggers
// and trigger function, one per line.
func adminElevationsShape(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT 'column ' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '')
FROM information_schema.columns
WHERE table_schema = 'elitea_auth' AND table_name = 'admin_elevations'
UNION ALL
SELECT 'constraint ' || conname || ' ' || pg_get_constraintdef(oid)
FROM pg_constraint WHERE conrelid = 'elitea_auth.admin_elevations'::regclass
UNION ALL
SELECT 'index ' || indexname || ' ' || indexdef
FROM pg_indexes WHERE schemaname = 'elitea_auth' AND tablename = 'admin_elevations'
UNION ALL
SELECT 'trigger ' || tgname || ' ' || pg_get_triggerdef(oid)
FROM pg_trigger WHERE tgrelid = 'elitea_auth.admin_elevations'::regclass AND NOT tgisinternal
UNION ALL
SELECT 'function ' || md5(prosrc)
FROM pg_proc WHERE oid = 'elitea_auth.admin_elevations_append_only'::regproc
ORDER BY 1`)
	if err != nil {
		t.Fatalf("read the table's shape: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan the table's shape: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the table's shape: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("0269 created no elitea_auth.admin_elevations")
	}
	return strings.Join(lines, "\n")
}
