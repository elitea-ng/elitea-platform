package repos

import (
	"context"
	"errors"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

// TestCurrentSocialAuthorsPostgresCompatibility crosses the real PostgreSQL
// protocol in an isolated opt-in database. The transaction rolls back every
// current-schema fixture.
func TestCurrentSocialAuthorsPostgresCompatibility(t *testing.T) {
	databaseURL := testpg.DSN(os.Getenv("ELITEA_SOCIAL_AUTHORS_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set ELITEA_SOCIAL_AUTHORS_TEST_DATABASE_URL to an isolated PostgreSQL database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := connection.Close(context.Background()); err != nil {
			t.Errorf("close PostgreSQL connection: %v", err)
		}
	})
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback PostgreSQL transaction: %v", err)
		}
	})

	for _, relation := range []string{
		"public.auth_core__user",
		"public.auth_core__project_user_role",
		"centry.social_users",
	} {
		var existing *string
		if err := tx.QueryRow(ctx, `SELECT to_regclass($1)::text`, relation).Scan(&existing); err != nil {
			t.Fatal(err)
		}
		if existing != nil {
			t.Skip("current social-author integration test requires an empty isolated database")
		}
	}

	if _, err := tx.Exec(ctx, `
CREATE SCHEMA IF NOT EXISTS centry;
CREATE TABLE public.auth_core__user (
    id integer PRIMARY KEY,
    email text,
    name text,
    last_login timestamp without time zone,
    suspended boolean NOT NULL DEFAULT false
);
CREATE TABLE public.auth_core__project_user_role (
    id integer PRIMARY KEY,
    project_id integer NOT NULL,
    user_id integer,
    role_id integer,
    UNIQUE (project_id, user_id, role_id)
);
CREATE INDEX ix_social_authors_assignment_project_id
    ON public.auth_core__project_user_role (project_id);
CREATE TABLE centry.social_users (
    id integer PRIMARY KEY,
    user_id integer NOT NULL UNIQUE,
    avatar varchar
);

INSERT INTO public.auth_core__user (id, email, name, last_login, suspended) VALUES
    (1, 'member@example.test', 'Member', TIMESTAMP '2026-07-27 11:12:13', false),
    (2, NULL, NULL, NULL, true),
    (3, 'system_user_7@centry.user', 'Project system', NULL, false),
    (4, 'other-project@example.test', 'Other project', NULL, false);
INSERT INTO public.auth_core__project_user_role (id, project_id, user_id, role_id) VALUES
    (11, 7, 1, 101),
    (12, 7, 1, 102),
    (13, 7, 2, 103),
    (14, 7, 3, 104),
    (15, 8, 4, 105);
INSERT INTO centry.social_users (id, user_id, avatar) VALUES
    (21, 1, 'avatar-data'),
    (22, 3, 'system-avatar'),
    (23, 99, 'social-only');`); err != nil {
		t.Fatal(err)
	}

	repository, err := newCurrentSocialAuthorsRepository(sqlcgen.New(tx))
	if err != nil {
		t.Fatal(err)
	}
	authors, err := repository.ListCurrentProjectAuthors(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(authors) != 2 || authors[0].ID != 1 || authors[1].ID != 2 {
		t.Fatalf("project authors=%+v", authors)
	}
	if authors[0].Avatar == nil || *authors[0].Avatar != "avatar-data" ||
		authors[0].LastLogin == nil ||
		authors[0].LastLogin.Format("2006-01-02 15:04:05") != "2026-07-27 11:12:13" {
		t.Fatalf("enriched author=%+v", authors[0])
	}
	if authors[1].Email != nil || authors[1].Name != nil ||
		authors[1].LastLogin != nil || authors[1].Avatar != nil || !authors[1].Suspended {
		t.Fatalf("nullable/suspended author=%+v", authors[1])
	}

	empty, err := repository.ListCurrentProjectAuthors(ctx, 99)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty-membership authors=%#v error=%v", empty, err)
	}
	otherProject, err := repository.ListCurrentProjectAuthors(ctx, 8)
	if err != nil || len(otherProject) != 1 || otherProject[0].ID != 4 {
		t.Fatalf("other-project authors=%+v error=%v", otherProject, err)
	}

	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `
EXPLAIN (COSTS OFF)
SELECT DISTINCT
    user_account.id,
    user_account.email,
    user_account.name,
    user_account.last_login,
    user_account.suspended,
    social_user.avatar
FROM public.auth_core__project_user_role AS assignment
JOIN public.auth_core__user AS user_account
    ON user_account.id = assignment.user_id
LEFT JOIN centry.social_users AS social_user
    ON social_user.user_id = user_account.id
WHERE assignment.project_id = $1::integer
  AND user_account.email IS DISTINCT FROM
      ('system_user_' || $1::integer::text || '@centry.user')
ORDER BY user_account.id`, int32(7))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var planLines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		planLines = append(planLines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(planLines, "\n")
	for _, index := range []string{
		"ix_social_authors_assignment_project_id",
		"auth_core__user_pkey",
		"social_users_user_id_key",
	} {
		if !strings.Contains(plan, index) {
			t.Errorf("EXPLAIN plan missing %q:\n%s", index, plan)
		}
	}
}
