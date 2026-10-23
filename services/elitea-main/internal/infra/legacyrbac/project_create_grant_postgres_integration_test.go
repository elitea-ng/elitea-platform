package legacyrbac_test

// The blast radius of migrations/shared/0069_project_create_permissions.sql
// (issue #333), pinned the way 0062's, 0063's, 0066's and 0068's are.
//
// The defect this migration exists to prevent is the one 0063's header names:
// gating a route on a permission NOTHING grants is 403-for-everyone, which
// reads as a broken feature rather than as a missing grant.
// `projects.projects.project.create` is a real legacy name — it is in
// testdata/legacy/legacy-rbac-static-catalog.json — but no migration in this
// corpus granted it before 0069, so the create route would have refused every
// caller including a super administrator.
//
// The cases below assert against the migration FILE rather than a copy of its
// SQL, so an edit that changed which roles it grants cannot pass here. They also
// pin the two NEGATIVE halves, which are the parts that make the split
// load-bearing rather than decorative:
//
//   - an administration-mode `editor` and `viewer` gain nothing, because the
//     reference declares `{"admin": True, "viewer": False, "editor": False}`;
//   - a DEFAULT-mode project member gains nothing, because a default-mode grant
//     to `admin` would reach every project administrator on every project
//     through legacyrbac's central fallback.

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	projectCreatePermission = "projects.projects.project.create"
	projectDeletePermission = "projects.projects.project.delete"
)

// TestProjectCreateGrantResolvesForAnAdministrationAdmin is the premise-and-
// effect pair. The `before` half fails against a database without 0069, which is
// the state every Go deployment is in today.
func TestProjectCreateGrantResolvesForAnAdministrationAdmin(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	seedAdministrationRoles(t, pool)
	resolver := legacyrbac.NewPostgresResolver(pool)
	administrator := auth.User{ID: "1", UserID: "1"}

	before, err := resolver.ResolvePermissions(
		context.Background(), administrator, auth.PermissionModeAdministration, "")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(before.Permissions, projectCreatePermission) {
		t.Fatalf("%s resolves before the migration; this test's premise is that it does not",
			projectCreatePermission)
	}

	applyProjectCreateGrant(t, pool)

	after, err := resolver.ResolvePermissions(
		context.Background(), administrator, auth.PermissionModeAdministration, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, permission := range []string{projectCreatePermission, projectDeletePermission} {
		if !slices.Contains(after.Permissions, permission) {
			t.Fatalf("%s does not resolve after the migration: the route it gates would answer 403 "+
				"for every caller, including a super administrator", permission)
		}
	}
}

// An administration-mode editor and viewer gain nothing. A viewer that could
// create or delete a tenant would be a widening this migration has no mandate
// to make.
func TestProjectCreateGrantWithholdsFromEditorAndViewer(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	seedAdministrationRoles(t, pool)
	applyProjectCreateGrant(t, pool)
	resolver := legacyrbac.NewPostgresResolver(pool)

	for role, userID := range map[string]string{"editor": "2", "viewer": "3"} {
		t.Run(role, func(t *testing.T) {
			resolution, err := resolver.ResolvePermissions(
				context.Background(), auth.User{ID: userID, UserID: userID},
				auth.PermissionModeAdministration, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, permission := range []string{projectCreatePermission, projectDeletePermission} {
				if slices.Contains(resolution.Permissions, permission) {
					t.Errorf("an administration-mode %s resolves %s", role, permission)
				}
			}
		})
	}
}

// TestProjectCreateGrantDoesNotLeakIntoProjectScope is the containment case.
//
// legacyrbac.projectPermissions() falls back to the central DEFAULT-mode grants
// by role NAME for a project with no per-project rows — the shape of every
// freshly provisioned project. Had 0069 granted these names in the default mode,
// every project admin would silently gain the right to create and delete
// tenants. seedFreshDatabaseShape's user 1 is exactly that member.
func TestProjectCreateGrantDoesNotLeakIntoProjectScope(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	seedAdministrationRoles(t, pool)
	applyProjectCreateGrant(t, pool)
	resolver := legacyrbac.NewPostgresResolver(pool)

	resolution, err := resolver.ResolvePermissions(
		context.Background(), auth.User{ID: "1", UserID: "1"}, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, permission := range []string{projectCreatePermission, projectDeletePermission} {
		if slices.Contains(resolution.Permissions, permission) {
			t.Errorf("%s reached PROJECT scope through the central default-mode fallback; "+
				"every project administrator could create or delete tenants", permission)
		}
	}
}

// seedAdministrationRoles adds the administration-mode roles 001_initial.sql
// seeds, and three holders. seedFreshDatabaseShape covers only the default mode.
func seedAdministrationRoles(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO public.auth_core__user (id, email, name) VALUES
			(2, 'editor@example.com', 'Edith'), (3, 'viewer@example.com', 'Vera')`,
		`INSERT INTO public.auth_core__role (id, name, mode) VALUES
			(4, 'super_admin', 'administration'), (5, 'admin', 'administration'),
			(6, 'editor', 'administration'), (7, 'viewer', 'administration')`,
		`INSERT INTO public.auth_core__user_role (user_id, role_id) VALUES (1, 5), (2, 6), (3, 7)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func applyProjectCreateGrant(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	sql, err := migrations.Files.ReadFile("shared/0069_project_create_permissions.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if _, err := pool.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("apply 0069: %v", err)
	}
}
