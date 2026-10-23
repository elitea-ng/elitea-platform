package projects_test

import (
	"context"
	"errors"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

// TestCurrentProjectListPostgresCompatibility crosses the real PostgreSQL
// protocol in an isolated opt-in database. The transaction rolls back every
// current-schema fixture.
func TestCurrentProjectListPostgresCompatibility(t *testing.T) {
	databaseURL := testpg.DSN(os.Getenv("ELITEA_PROJECTS_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set ELITEA_PROJECTS_TEST_DATABASE_URL to an isolated PostgreSQL database")
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

	var publicUserTable, centrySchema *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.auth_core__project_user_role')::text`).Scan(&publicUserTable); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT to_regnamespace('centry')::text`).Scan(&centrySchema); err != nil {
		t.Fatal(err)
	}
	if publicUserTable != nil || centrySchema != nil {
		t.Skip("project-list integration test requires an empty isolated database")
	}

	if _, err := tx.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id integer PRIMARY KEY,
    name varchar(256) NOT NULL,
    owner_id integer NOT NULL,
    plugins text[],
    keycloak_groups json NOT NULL,
    create_success boolean NOT NULL,
    suspended boolean NOT NULL DEFAULT false
);
CREATE TABLE centry.project_group (id integer PRIMARY KEY, name varchar(256) NOT NULL UNIQUE);
CREATE TABLE centry.project_group_association (project_id integer, group_id integer);
CREATE TABLE public.auth_core__project_role (
    id integer PRIMARY KEY,
    project_id integer NOT NULL,
    name text NOT NULL
);
CREATE TABLE public.auth_core__project_user_role (
    id integer PRIMARY KEY,
    project_id integer NOT NULL,
    user_id integer,
    role_id integer
);

INSERT INTO centry.project (id, name, owner_id, plugins, keycloak_groups, create_success, suspended) VALUES
    (1, 'promptlib_public', 1, ARRAY['configuration', 'models'], '{}', true, false),
    (2, 'inaccessible_between_page_rows', 2, NULL, '{}', true, false),
    (3, 'project_user_7', 7, ARRAY['configuration', 'models'], '{}', true, false);
INSERT INTO centry.project_group (id, name) VALUES (8, 'delivery');
INSERT INTO centry.project_group_association (project_id, group_id) VALUES (3, 8), (3, 8);
INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES
    (10, 1, 'admin'),
    (11, 3, 'editor'),
    (12, 1, 'viewer');
INSERT INTO public.auth_core__project_user_role (id, project_id, user_id, role_id) VALUES
    (20, 1, 7, 10),
    (21, 3, 7, 11),
    (22, 1, 8, 12);`); err != nil {
		t.Fatal(err)
	}

	queries := sqlcgen.New(tx)
	router := setupProjectRouter(queries)

	for _, test := range []struct {
		name   string
		userID string
		query  string
		want   string
	}{
		{
			name:   "admin sees public and private with exact DTO",
			userID: "7",
			query:  "?check_public_role=true",
			want:   `[{"id":1,"name":"promptlib_public","owner_id":1,"plugins":["configuration","models"],"keycloak_groups":{},"create_success":true,"suspended":false,"groups":[]},{"id":3,"name":"project_user_7","owner_id":7,"plugins":["configuration","models"],"keycloak_groups":{},"create_success":true,"suspended":false,"groups":[{"id":8,"name":"delivery"}]}]`,
		},
		{
			name:   "non-admin public membership is hidden",
			userID: "8",
			query:  "?check_public_role=true",
			want:   `[]`,
		},
		{
			name:   "pagination precedes membership filtering",
			userID: "7",
			query:  "?limit=2&offset=0",
			want:   `[{"id":1,"name":"promptlib_public","owner_id":1,"plugins":["configuration","models"],"keycloak_groups":{},"create_success":true,"suspended":false,"groups":[]}]`,
		},
		{
			name:   "search uses current case-insensitive wildcard behavior",
			userID: "7",
			query:  "?search=PROJECT_USER",
			want:   `[{"id":3,"name":"project_user_7","owner_id":7,"plugins":["configuration","models"],"keycloak_groups":{},"create_success":true,"suspended":false,"groups":[{"id":8,"name":"delivery"}]}]`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/projects/project/default/1"+test.query, nil)
			request = withUser(request, auth.User{ID: test.userID, UserID: test.userID, AuthType: "user"})
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
			}
			if got := strings.TrimSpace(recorder.Body.String()); got != test.want {
				t.Fatalf("body = %s\nwant = %s", got, test.want)
			}
		})
	}
}
