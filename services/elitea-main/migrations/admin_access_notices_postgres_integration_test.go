package migrations_test

// Shared 0277 (issue 2519, ADR-0059 R2.5a): elitea_auth.admin_access_notices,
// the outbox of the access notices. It applies on the bootstrap schema with
// the corpus and on a database without auth_core; each time twice, with no
// change the second time. Its constraints refuse a row the sender could not
// deliver.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const adminAccessNoticesMigration = "shared/0277_admin_access_notices.sql"

func TestAdminAccessNoticesAppliesTwiceWithNoChange(t *testing.T) {
	for name, build := range map[string]func(*testing.T) *pgxpool.Pool{
		"bootstrap and corpus": newMigratedPool,
		"no auth_core": func(t *testing.T) *pgxpool.Pool {
			pool := newEmptyDatabase(t)
			execMigrationFile(t, pool, adminElevationsMigration)
			execMigrationFile(t, pool, adminAccessNoticesMigration)
			return pool
		},
	} {
		t.Run(name, func(t *testing.T) {
			pool := build(t)
			before := adminAccessNoticesShape(t, pool)
			for _, want := range []string{"created_at timestamp with time zone NO", "sent_at timestamp with time zone YES",
				"admin_access_notices_due_idx", "admin_access_notices_dedupe_idx"} {
				if !strings.Contains(before, want) {
					t.Fatalf("0277 shape lacks %q:\n%s", want, before)
				}
			}
			var rowID string
			queryRow(t, pool, `
INSERT INTO elitea_auth.admin_access_notices
    (kind, recipient_user_id, recipient_email, detail, dedupe_key, created_at, next_attempt_at)
VALUES ('inherit_token', 7, 'owner@example.com', '{}', 'inherit_token:7', now(), now())
RETURNING id::text`, &rowID)

			execMigrationFile(t, pool, adminAccessNoticesMigration)

			if after := adminAccessNoticesShape(t, pool); after != before {
				t.Fatalf("re-applying 0277 changed the table:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			var rows int
			queryRow(t, pool, `SELECT count(*) FROM elitea_auth.admin_access_notices`, &rows)
			if rows != 1 {
				t.Fatalf("re-applying 0277 left %d rows, want 1", rows)
			}
			for refusal, statement := range map[string]string{
				"unknown kind": `INSERT INTO elitea_auth.admin_access_notices (kind, recipient_user_id, recipient_email, created_at, next_attempt_at)
VALUES ('other', 7, 'a@example.com', now(), now())`,
				"elevation kind without an elevation": `INSERT INTO elitea_auth.admin_access_notices (kind, recipient_user_id, recipient_email, created_at, next_attempt_at)
VALUES ('activation', 7, 'a@example.com', now(), now())`,
				"no address": `INSERT INTO elitea_auth.admin_access_notices (kind, recipient_user_id, recipient_email, created_at, next_attempt_at)
VALUES ('inherit_token', 7, ' ', now(), now())`,
				"unknown channel": `INSERT INTO elitea_auth.admin_access_notices (kind, channel, recipient_user_id, recipient_email, created_at, next_attempt_at)
VALUES ('inherit_token', 'sms', 7, 'a@example.com', now(), now())`,
				"duplicate dedupe key": `INSERT INTO elitea_auth.admin_access_notices (kind, recipient_user_id, recipient_email, dedupe_key, created_at, next_attempt_at)
VALUES ('inherit_token', 7, 'a@example.com', 'inherit_token:7', now(), now())`,
			} {
				if _, err := pool.Exec(context.Background(), statement); err == nil {
					t.Errorf("%s: the row was accepted", refusal)
				}
			}
		})
	}
}

// adminAccessNoticesShape is the table's columns, constraints and indexes,
// one per line.
func adminAccessNoticesShape(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT 'column ' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '')
FROM information_schema.columns
WHERE table_schema = 'elitea_auth' AND table_name = 'admin_access_notices'
UNION ALL
SELECT 'constraint ' || conname || ' ' || pg_get_constraintdef(oid)
FROM pg_constraint WHERE conrelid = 'elitea_auth.admin_access_notices'::regclass
UNION ALL
SELECT 'index ' || indexname || ' ' || indexdef
FROM pg_indexes WHERE schemaname = 'elitea_auth' AND tablename = 'admin_access_notices'
ORDER BY 1`)
	if err != nil {
		t.Fatalf("read the shape: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}
