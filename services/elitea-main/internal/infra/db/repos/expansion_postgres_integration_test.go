package repos

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCurrentExpansionAdaptersPostgresParity crosses the SQLC finder and scope
// adapters against a real PostgreSQL 16-18 database. It is an opt-in service
// integration test, not route, transport, or system E2E evidence.
func TestCurrentExpansionAdaptersPostgresParity(t *testing.T) {
	pool := newPostgresIntegrationPool(t)
	prepareCurrentExpansionPostgres(t, pool)

	finder, err := NewCurrentConfigurationsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewCurrentExpansionScopeRepository(pool, 41)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	publicProjectID, err := scope.PublicProjectID(ctx)
	if err != nil || publicProjectID != 41 {
		t.Fatalf("injected public project=%d err=%v", publicProjectID, err)
	}

	team, found, err := finder.FindByEliteaTitle(ctx, 7, "github_exact", false)
	if err != nil || !found || team.ProjectID != 7 || team.Type != "github" ||
		team.Data["large"] != json.Number("9007199254740993") {
		t.Fatalf("team configuration=%#v found=%t err=%v", team, found, err)
	}
	for _, nonExact := range []string{"github", "GitHub_exact", "github_exact "} {
		configuration, found, err := finder.FindByEliteaTitle(ctx, 7, nonExact, false)
		if err != nil || found || configuration.ProjectID != 0 {
			t.Fatalf("non-exact title %q returned configuration=%#v found=%t err=%v", nonExact, configuration, found, err)
		}
	}

	shared, found, err := finder.FindByEliteaTitle(ctx, 41, "public_shared", true)
	if err != nil || !found || shared.ProjectID != 41 || shared.Type != "openapi" {
		t.Fatalf("public shared configuration=%#v found=%t err=%v", shared, found, err)
	}
	privatePublic, found, err := finder.FindByEliteaTitle(ctx, 41, "public_private", true)
	if err != nil || found || privatePublic.ProjectID != 0 {
		t.Fatalf("non-shared public row escaped filter: configuration=%#v found=%t err=%v", privatePublic, found, err)
	}

	assertCurrentPersonalProject(t, ctx, scope, 100, 73)
	assertCurrentPersonalProject(t, ctx, scope, 103, 41)
	// Current auth_get_user does not filter suspended users; preserve that
	// behavior for the canonical system-user fallback.
	assertCurrentPersonalProject(t, ctx, scope, 104, 73)
	for _, userID := range []int32{101, 102, 105} {
		projectID, err := scope.PersonalProjectID(ctx, userID)
		if projectID != 0 || !errors.Is(err, ErrCurrentPersonalProjectNotFound) {
			t.Fatalf("user %d personal project=%d err=%v", userID, projectID, err)
		}
	}
}

func assertCurrentPersonalProject(
	t *testing.T,
	ctx context.Context,
	scope *CurrentExpansionScopeRepository,
	userID int32,
	want int32,
) {
	t.Helper()
	projectID, err := scope.PersonalProjectID(ctx, userID)
	if err != nil || projectID != want {
		t.Fatalf("user %d personal project=%d want=%d err=%v", userID, projectID, want, err)
	}
}

func prepareCurrentExpansionPostgres(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id INTEGER PRIMARY KEY,
    name VARCHAR(256) NOT NULL,
    owner_id INTEGER NOT NULL,
    secrets_json JSON,
    plugins TEXT[],
    keycloak_groups JSON NOT NULL,
    create_success BOOLEAN NOT NULL,
    suspended BOOLEAN NOT NULL DEFAULT false
);
CREATE TABLE public.auth_core__user (
    id INTEGER PRIMARY KEY,
    email TEXT,
    name TEXT,
    last_login TIMESTAMP,
    suspended BOOLEAN NOT NULL DEFAULT false
);
CREATE UNIQUE INDEX ix_auth_core__user_email
    ON public.auth_core__user (email);
CREATE TABLE public.auth_core__project_user_role (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL,
    user_id INTEGER,
    role_id INTEGER
);

INSERT INTO centry.project (
    id, name, owner_id, secrets_json, plugins, keycloak_groups,
    create_success, suspended
) VALUES
    (7, 'team-seven', 100, '{}', ARRAY['configuration'], '{}', true, false),
    (41, 'configured-public', 1, '{}', ARRAY['configuration'], '{}', true, false),
    (73, 'project_user_100', 100, '{}', ARRAY['configuration'], '{}', true, false),
    (74, 'project_user_101', 101, '{}', ARRAY['configuration'], '{}', true, false);

INSERT INTO public.auth_core__user (id, email, name, suspended) VALUES
    (100, 'member@example.invalid', 'member', false),
    (101, 'system_user_7@centry.user', ':system:project:7:', false),
    (102, 'ordinary@example.invalid', 'ordinary', false),
	(103, 'system_user_41@centry.user', ':system:project:41:', false),
	(104, 'system_user_73@centry.user', ':system:project:73:', true),
    (105, 'system_user_7@centryXuser', ':system:project:malformed:', false);

INSERT INTO public.auth_core__project_user_role (id, project_id, user_id, role_id) VALUES
    (1, 73, 100, 10),
	(2, 41, 103, 10),
	(3, 73, 104, 10);

CREATE SCHEMA p_7;
CREATE TABLE p_7.configuration (
    id SERIAL PRIMARY KEY,
    uuid UUID NOT NULL UNIQUE,
    project_id INTEGER NOT NULL,
    label VARCHAR,
    elitea_title VARCHAR NOT NULL UNIQUE,
    type VARCHAR NOT NULL,
    section VARCHAR NOT NULL,
    data JSONB NOT NULL,
    meta JSONB NOT NULL,
    shared BOOLEAN NOT NULL,
    status_ok BOOLEAN NOT NULL,
    status_logs TEXT,
    source VARCHAR NOT NULL,
    author_id INTEGER,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP
);
INSERT INTO p_7.configuration (
    uuid, project_id, elitea_title, type, section, data, meta, shared,
    status_ok, source
, created_at) VALUES (
    '00000000-0000-0000-0000-000000000701', 7, 'github_exact',
    'github', 'credentials', '{"large":9007199254740993}'::jsonb,
    '{}'::jsonb, false, true, 'test'
, now() AT TIME ZONE 'UTC');

CREATE SCHEMA p_41;
CREATE TABLE p_41.configuration (LIKE p_7.configuration INCLUDING ALL);
INSERT INTO p_41.configuration (
    uuid, project_id, elitea_title, type, section, data, meta, shared,
    status_ok, source
, created_at) VALUES
    ('00000000-0000-0000-0000-000000004101', 41, 'public_shared',
     'openapi', 'credentials', '{"spec":"https://api.invalid"}'::jsonb,
     '{}'::jsonb, true, true, 'test', now() AT TIME ZONE 'UTC'),
    ('00000000-0000-0000-0000-000000004102', 41, 'public_private',
     'company_custom', 'credentials', '{}'::jsonb, '{}'::jsonb,
     false, true, 'test', now() AT TIME ZONE 'UTC');
`); err != nil {
		t.Fatalf("prepare current expansion PostgreSQL baseline: %v", err)
	}
}
