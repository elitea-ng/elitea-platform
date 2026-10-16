package legacyrbac

import (
	"context"
	"errors"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type deniedRow struct{}

func (deniedRow) Scan(...any) error { return pgx.ErrNoRows }

type countingPostgresStore struct {
	calls int
}

func (s *countingPostgresStore) Query(context.Context, string, ...any) (pgx.Rows, error) {
	s.calls++
	return nil, errors.New("unexpected query")
}

func (s *countingPostgresStore) QueryRow(context.Context, string, ...any) pgx.Row {
	s.calls++
	return deniedRow{}
}

func TestNormalizeModeKeepsLegacyPromptLibAlias(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "administration", want: auth.PermissionModeAdministration, ok: true},
		{input: "developer", want: auth.PermissionModeDeveloper, ok: true},
		{input: "default", want: auth.PermissionModeDefault, ok: true},
		{input: "prompt_lib", want: auth.PermissionModeDefault, ok: true},
		{input: "unknown", ok: false},
	} {
		got, err := normalizeMode(test.input)
		if (err == nil) != test.ok || got != test.want {
			t.Fatalf("normalizeMode(%q) = (%q, %v), want (%q, ok=%v)", test.input, got, err, test.want, test.ok)
		}
	}
}

func TestNewPostgresResolverWithNilPoolFailsClosed(t *testing.T) {
	resolver := NewPostgresResolver(nil)
	if _, err := resolver.ResolvePermissions(
		context.Background(),
		auth.User{ID: "1", UserID: "1", AuthType: "token"},
		auth.PermissionModeDefault,
		"7",
	); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("error = %v, want permission denied", err)
	}
}

func TestPostgresResolverRejectsIncompleteCachedTokenBeforeLookup(t *testing.T) {
	// A compatibility ID of 42 is deliberately ambiguous: both a user row and
	// a token row may have that number. Old or partially upgraded cache values
	// must be denied rather than selecting either row type.
	for _, test := range []struct {
		name      string
		principal auth.User
	}{
		{
			name:      "ID-only stale cache with numeric collision",
			principal: auth.User{ID: "42", AuthType: "token"},
		},
		{
			name:      "owner ID without token ID",
			principal: auth.User{ID: "42", UserID: "42", AuthType: "token"},
		},
		{
			name:      "token ID without owner ID",
			principal: auth.User{ID: "42", TokenID: "42", AuthType: "token"},
		},
		{
			name:      "typed token ID without auth type or owner ID",
			principal: auth.User{ID: "42", TokenID: "42"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &countingPostgresStore{}
			resolver := &PostgresResolver{store: store}

			_, err := resolver.resolveUserID(context.Background(), test.principal)
			if !errors.Is(err, ErrPermissionDenied) {
				t.Fatalf("error = %v, want ErrPermissionDenied", err)
			}
			if store.calls != 0 {
				t.Fatalf("database calls = %d, want 0 for incomplete token principal", store.calls)
			}
		})
	}
}

// TestPostgresResolverLegacyCompatibility crosses the real PostgreSQL protocol
// in an isolated opt-in database. It rolls all legacy-schema fixtures back.
func TestPostgresResolverLegacyCompatibility(t *testing.T) {
	const requireEnvironment = "ELITEA_REQUIRE_LEGACY_RBAC_POSTGRES_TEST"
	required := os.Getenv(requireEnvironment) == "true"
	databaseURL := testpg.DSN(os.Getenv("ELITEA_LEGACY_RBAC_TEST_DATABASE_URL"))
	if databaseURL == "" {
		if required {
			t.Fatal("ELITEA_REQUIRE_LEGACY_RBAC_POSTGRES_TEST requires ELITEA_LEGACY_RBAC_TEST_DATABASE_URL")
		}
		t.Skip("set ELITEA_LEGACY_RBAC_TEST_DATABASE_URL to an isolated PostgreSQL database")
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

	var existing *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.auth_core__user')::text`).Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if existing != nil {
		if required {
			t.Fatal("required RBAC integration database already contains legacy auth_core tables")
		}
		t.Skip("RBAC integration test requires a database without legacy auth_core tables")
	}
	var centrySchema *string
	if err := tx.QueryRow(ctx, `SELECT to_regnamespace('centry')::text`).Scan(&centrySchema); err != nil {
		t.Fatal(err)
	}
	if centrySchema != nil {
		if required {
			t.Fatal("required RBAC integration database already contains the centry schema")
		}
		t.Skip("RBAC integration test requires a database without the centry schema")
	}

	if _, err := tx.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE public.auth_core__user (id bigint PRIMARY KEY, suspended boolean NOT NULL, last_login timestamp without time zone);
CREATE TABLE public.auth_core__token (id bigint PRIMARY KEY, user_id bigint NOT NULL, expires timestamp without time zone);
CREATE TABLE public.auth_core__role (id bigint PRIMARY KEY, name text NOT NULL, mode text NOT NULL);
CREATE TABLE public.auth_core__role_permission (id bigint PRIMARY KEY, role_id bigint NOT NULL, permission text NOT NULL);
CREATE TABLE public.auth_core__user_role (id bigint PRIMARY KEY, user_id bigint NOT NULL, role_id bigint NOT NULL);
CREATE TABLE public.auth_core__project_role (id bigint PRIMARY KEY, project_id bigint NOT NULL, name text NOT NULL);
CREATE TABLE public.auth_core__project_role_permission (id bigint PRIMARY KEY, project_id bigint NOT NULL, role_id bigint NOT NULL, permission text NOT NULL);
CREATE TABLE public.auth_core__project_user_role (id bigint PRIMARY KEY, project_id bigint NOT NULL, user_id bigint NOT NULL, role_id bigint NOT NULL);
CREATE TABLE centry.project (id bigint PRIMARY KEY, suspended boolean NOT NULL);

INSERT INTO public.auth_core__user (id, suspended) VALUES (1, false), (2, true);
INSERT INTO public.auth_core__token (id, user_id, expires) VALUES
    (100, 1, (clock_timestamp() AT TIME ZONE 'UTC') + interval '1 hour'),
    (101, 1, (clock_timestamp() AT TIME ZONE 'UTC') - interval '1 hour'),
    (102, 2, (clock_timestamp() AT TIME ZONE 'UTC') + interval '1 hour');
INSERT INTO public.auth_core__role (id, name, mode) VALUES
    (10, 'editor', 'default'),
    (11, 'admin', 'administration'),
    (12, 'editor', 'developer');
INSERT INTO public.auth_core__role_permission (id, role_id, permission) VALUES
    (20, 10, 'fallback.details'),
    (21, 10, 'fallback.list'),
    (22, 11, 'central.administration'),
    (23, 12, 'central.developer');
INSERT INTO public.auth_core__user_role (id, user_id, role_id) VALUES
    (30, 1, 11),
    (31, 1, 12);
INSERT INTO centry.project (id, suspended) VALUES (7, false), (8, true);
INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES
    (40, 7, 'editor'),
    (41, 8, 'editor');
INSERT INTO public.auth_core__project_user_role (id, project_id, user_id, role_id) VALUES
    (50, 7, 1, 40),
    (51, 8, 1, 41);
INSERT INTO public.auth_core__project_role_permission (id, project_id, role_id, permission) VALUES
    (60, 7, 40, 'project.override');`); err != nil {
		t.Fatal(err)
	}

	resolver := &PostgresResolver{store: tx}
	assertResolution(t, resolver, auth.User{ID: "1", AuthType: "user"}, auth.PermissionModeDefault, "7", 1, []string{"project.override"})

	if _, err := tx.Exec(ctx, `DELETE FROM public.auth_core__project_role_permission WHERE project_id = 7`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL TIME ZONE 'Pacific/Honolulu'`); err != nil {
		t.Fatal(err)
	}
	assertResolution(t, resolver, auth.User{ID: "1", AuthType: "user"}, "prompt_lib", "7", 1, []string{"fallback.details", "fallback.list"})
	assertResolution(t, resolver, auth.User{ID: "1", UserID: "1", TokenID: "100", AuthType: "token"}, auth.PermissionModeDefault, "7", 1, []string{"fallback.details", "fallback.list"})
	var lastLogin *time.Time
	if err := tx.QueryRow(ctx, `SELECT last_login FROM public.auth_core__user WHERE id = 1`).Scan(&lastLogin); err != nil {
		t.Fatal(err)
	}
	if lastLogin == nil || lastLogin.UTC().Format("2006-01-02") != time.Now().UTC().Format("2006-01-02") {
		t.Fatalf("token permission lookup last_login = %v, want current UTC day", lastLogin)
	}
	assertResolution(t, resolver, auth.User{ID: "1", AuthType: "user"}, auth.PermissionModeAdministration, "", 1, []string{"central.administration"})
	assertResolution(t, resolver, auth.User{ID: "1", AuthType: "user"}, auth.PermissionModeDeveloper, "", 1, []string{"central.developer"})

	for _, test := range []struct {
		principal auth.User
		projectID string
	}{
		{principal: auth.User{ID: "100", AuthType: "token"}, projectID: "7"},
		{principal: auth.User{ID: "1", UserID: "1", AuthType: "token"}, projectID: "7"},
		{principal: auth.User{ID: "1", UserID: "1", TokenID: "101", AuthType: "token"}, projectID: "7"},
		{principal: auth.User{ID: "2", UserID: "2", TokenID: "102", AuthType: "token"}, projectID: "7"},
		{principal: auth.User{ID: "2", AuthType: "user"}, projectID: "7"},
		{principal: auth.User{ID: "1", UserID: "999", TokenID: "100", AuthType: "token"}, projectID: "7"},
		{principal: auth.User{ID: "999", AuthType: "user"}, projectID: "7"},
		{principal: auth.User{ID: "1", AuthType: "user"}, projectID: "8"},
		{principal: auth.User{ID: "1", AuthType: "user"}, projectID: "999"},
	} {
		if _, err := resolver.ResolvePermissions(ctx, test.principal, auth.PermissionModeDefault, test.projectID); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("principal=%+v project=%q error=%v, want permission denied", test.principal, test.projectID, err)
		}
	}
}

func assertResolution(
	t *testing.T,
	resolver *PostgresResolver,
	principal auth.User,
	mode string,
	projectID string,
	wantUserID int64,
	wantPermissions []string,
) {
	t.Helper()
	resolution, err := resolver.ResolvePermissions(context.Background(), principal, mode, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.UserID != wantUserID || !reflect.DeepEqual(resolution.Permissions, wantPermissions) {
		t.Fatalf("resolution = %+v, want user=%d permissions=%v", resolution, wantUserID, wantPermissions)
	}
}
