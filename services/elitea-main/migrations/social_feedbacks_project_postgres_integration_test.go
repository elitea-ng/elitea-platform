package migrations_test

// shared/0158 gives centry.social_feedbacks a project_id. The table exists on
// databases carried over from legacy (with rows) and on no fresh install, so
// both arrivals are exercised: the real runner on an empty database, and the
// same file over a legacy-shaped table that already holds rows.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	migrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const socialFeedbacksColumns = `
SELECT column_name || ':' || data_type || ':' || is_nullable
FROM information_schema.columns
WHERE table_schema = 'centry' AND table_name = 'social_feedbacks'
ORDER BY ordinal_position`

func TestSharedMigration0158CreatesTheFeedbackTableOnAFreshInstall(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	rows, err := pool.Query(ctx, socialFeedbacksColumns)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		got = append(got, column)
	}
	want := []string{
		"id:integer:NO", "user_id:integer:NO", "referrer:character varying:YES",
		"description:text:NO", "rating:integer:NO", "user_agent:character varying:YES",
		"created_at:timestamp without time zone:NO", "project_id:integer:YES",
	}
	if len(got) != len(want) {
		t.Fatalf("columns = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("columns = %v, want %v", got, want)
		}
	}
	assertFeedbackIndexes(t, pool)
}

func TestSharedMigration0158KeepsLegacyRowsAndIsIdempotent(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A database carried over from legacy: the table without project_id, with rows.
	if _, err := pool.Exec(ctx, `
DROP TABLE centry.social_feedbacks;
CREATE TABLE centry.social_feedbacks (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL,
    referrer VARCHAR,
    description TEXT NOT NULL,
    rating INTEGER NOT NULL,
    user_agent VARCHAR,
    created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT now()
);
INSERT INTO centry.social_feedbacks (user_id, description, rating) VALUES (41, 'legacy one', 5), (42, 'legacy two', 1);`); err != nil {
		t.Fatal(err)
	}

	sql, err := migrations.Files.ReadFile("shared/0158_social_feedbacks_project.sql")
	if err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply run %d: %v", run, err)
		}
	}

	var rows, nullProjects int
	var projectColumn string
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int, count(*) FILTER (WHERE project_id IS NULL)::int FROM centry.social_feedbacks`,
	).Scan(&rows, &nullProjects); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || nullProjects != 2 {
		t.Fatalf("legacy rows = %d with %d NULL project_id, want 2 and 2", rows, nullProjects)
	}
	if err := pool.QueryRow(ctx, `
SELECT data_type || ':' || is_nullable FROM information_schema.columns
WHERE table_schema = 'centry' AND table_name = 'social_feedbacks' AND column_name = 'project_id'`,
	).Scan(&projectColumn); err != nil || projectColumn != "integer:YES" {
		t.Fatalf("project_id = %q err=%v", projectColumn, err)
	}
	assertFeedbackIndexes(t, pool)
}

func assertFeedbackIndexes(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*)::int FROM pg_indexes
WHERE schemaname = 'centry' AND tablename = 'social_feedbacks'
  AND indexname IN ('social_feedbacks_project_id_idx', 'social_feedbacks_legacy_author_idx')`,
	).Scan(&count); err != nil || count != 2 {
		t.Fatalf("feedback list indexes = %d err=%v, want 2", count, err)
	}
}
