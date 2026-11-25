package migrations_test

// Shared 0271 (issue 2520, ADR-0059 R2.6a): elitea_auth.sealed_credentials.
// It applies on the bootstrap schema with the corpus, on the legacy RBAC
// matrix fixture, and on a database without auth_core; each time twice, with
// no change the second time and the rows kept.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const sealedCredentialsMigration = "shared/0271_sealed_credentials.sql"

func TestSealedCredentialsAppliesTwiceWithNoChange(t *testing.T) {
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
			execMigrationFile(t, pool, sealedCredentialsMigration)
			return pool
		},
	} {
		t.Run(name, func(t *testing.T) {
			pool := build(t)
			before := tableShape(t, pool, "sealed_credentials", "elitea_auth.sealed_credentials_append_only")
			if !strings.Contains(before, "sealed_credentials_append_only") {
				t.Fatalf("0271 created no trigger:\n%s", before)
			}
			var rowID string
			queryRow(t, pool, `
INSERT INTO elitea_auth.sealed_credentials (selector, verifier_digest, level, user_id, issued_at, expires_at)
VALUES ('0123456789abcdef01234567', '$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA', 'admin', 7, now(), now() + interval '30 days')
RETURNING id::text`, &rowID)

			// The 365-day ceiling is 365 x 24 hours whatever the session's
			// time zone: a lifetime crossing a daylight-saving change in a
			// zone that has one is admitted, an hour more is not.
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(context.Background(), `SET LOCAL TimeZone = 'America/New_York'`); err != nil {
					return err
				}
				_, err := tx.Exec(context.Background(), `
INSERT INTO elitea_auth.sealed_credentials (selector, verifier_digest, level, user_id, issued_at, expires_at)
VALUES ('0123456789abcdef01234568', '$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA', 'admin', 7,
        '2027-03-13T17:00:00Z', '2027-03-13T17:00:00Z'::timestamptz + interval '8760 hours')`)
				return err
			}); err != nil {
				t.Fatalf("a 365 x 24h lifetime across a DST change is refused: %v", err)
			}
			if _, err := pool.Exec(context.Background(), `
INSERT INTO elitea_auth.sealed_credentials (selector, verifier_digest, level, user_id, issued_at, expires_at)
VALUES ('0123456789abcdef01234569', '$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA', 'admin', 7,
        now(), now() + interval '8761 hours')`); err == nil {
				t.Fatal("a lifetime above 365 x 24h is admitted")
			}

			execMigrationFile(t, pool, sealedCredentialsMigration)

			if after := tableShape(t, pool, "sealed_credentials", "elitea_auth.sealed_credentials_append_only"); after != before {
				t.Fatalf("re-applying 0271 changed the table:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			var rows int
			queryRow(t, pool, `SELECT count(*) FROM elitea_auth.sealed_credentials`, &rows)
			if rows != 2 {
				t.Fatalf("re-applying 0271 left %d rows, want 2", rows)
			}
			if _, err := pool.Exec(context.Background(),
				`DELETE FROM elitea_auth.sealed_credentials WHERE id = $1::uuid`, rowID); err == nil ||
				!strings.Contains(err.Error(), "keeps its history") {
				t.Fatalf("after a re-apply the trigger does not refuse a DELETE: %v", err)
			}
		})
	}
}

// tableShape is an elitea_auth table's columns, constraints, indexes,
// triggers and the source digest of its trigger function, one per line.
func tableShape(t *testing.T, pool *pgxpool.Pool, table, function string) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT 'column ' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '')
FROM information_schema.columns
WHERE table_schema = 'elitea_auth' AND table_name = $1
UNION ALL
SELECT 'constraint ' || conname || ' ' || pg_get_constraintdef(oid)
FROM pg_constraint WHERE conrelid = ('elitea_auth.' || $1)::regclass
UNION ALL
SELECT 'index ' || indexname || ' ' || indexdef
FROM pg_indexes WHERE schemaname = 'elitea_auth' AND tablename = $1
UNION ALL
SELECT 'trigger ' || tgname || ' ' || pg_get_triggerdef(oid)
FROM pg_trigger WHERE tgrelid = ('elitea_auth.' || $1)::regclass AND NOT tgisinternal
UNION ALL
SELECT 'function ' || md5(prosrc)
FROM pg_proc WHERE $2 <> '' AND oid = to_regproc($2)
ORDER BY 1`, table, function)
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
		t.Fatalf("no elitea_auth.%s", table)
	}
	return strings.Join(lines, "\n")
}
