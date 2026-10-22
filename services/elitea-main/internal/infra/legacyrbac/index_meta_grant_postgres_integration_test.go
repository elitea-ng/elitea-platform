package legacyrbac_test

// The blast radius of the default-mode grants added by
// migrations/shared/0066_index_meta_permissions.sql (issue 93 Surface A),
// pinned the way 0062's and 0063's were.
//
// The defect this migration closes is that `models.applications.index_meta.details`
// — the LIST permission for the indexes rail — is a different string from
// `.edit`, and nothing on a Go-bootstrapped database granted it. The rail
// therefore 403s for every member while the Indexes tab still renders, which
// reads as a hung fetch rather than as a refusal.
//
// The cases below assert against the migration FILE rather than a copy of its
// SQL, so an edit that changed which roles it grants cannot pass here, and they
// discriminate the per-role split: a viewer gets the read and NOT the writes.

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	indexMetaListPermission   = "models.applications.index_meta.details"
	indexMetaEditPermission   = "models.applications.index_meta.edit"
	indexMetaDeletePermission = "models.applications.index_meta.delete"
)

// An editor member gains all three: the rail's read and both source-only
// writes. The `before` half is the test's premise — it fails against today's
// behaviour, where the list route is 403 for everyone.
func TestIndexMetaGrantsResolveForAnEditorMember(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	resolver := legacyrbac.NewPostgresResolver(pool)
	member := auth.User{ID: "1", UserID: "1"}

	before, err := resolver.ResolvePermissions(context.Background(), member, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(before.Permissions, indexMetaListPermission) {
		t.Fatalf("%s resolves before the migration; this test's premise is that it does not",
			indexMetaListPermission)
	}

	applyIndexMetaGrant(t, pool)

	after, err := resolver.ResolvePermissions(context.Background(), member, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, permission := range []string{indexMetaListPermission, indexMetaEditPermission, indexMetaDeletePermission} {
		if !slices.Contains(after.Permissions, permission) {
			t.Fatalf("%s does not resolve after the migration: the route it gates would be 403 for "+
				"every member, which the rail renders as an indefinite skeleton", permission)
		}
	}
}

// A viewer member gains the read and NOT the writes. This is the half that
// makes the per-role split load-bearing rather than decorative: granting the
// family uniformly would let a viewer rewrite an index schedule.
func TestIndexMetaGrantsGiveAViewerTheReadOnly(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES (11, 1, 'viewer');
INSERT INTO public.auth_core__user (id, email, name) VALUES (4, 'view93@example.com', 'View');
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES (1, 4, 11)`); err != nil {
		t.Fatal(err)
	}
	applyIndexMetaGrant(t, pool)

	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(
		context.Background(), auth.User{ID: "4", UserID: "4"}, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(resolution.Permissions, indexMetaListPermission) {
		t.Fatalf("a viewer cannot read the indexes rail: %v", resolution.Permissions)
	}
	for _, permission := range []string{indexMetaEditPermission, indexMetaDeletePermission} {
		if slices.Contains(resolution.Permissions, permission) {
			t.Fatalf("a viewer resolves %s; the legacy matrix grants a default-mode viewer the "+
				"read alone", permission)
		}
	}
}

// A project that carries its OWN per-project grants — every legacy-backed
// database and every legacy dump — suppresses the central fallback entirely, so
// this migration cannot change what an existing deployment's members can do.
// It is also why the E2E seed's project-1 list has to be fixed separately.
func TestIndexMetaGrantsAreInertForAProjectWithItsOwnGrants(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	applyIndexMetaGrant(t, pool)

	if _, err := pool.Exec(context.Background(), `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
VALUES (1, 10, 'models.applications.index_meta.edit')`); err != nil {
		t.Fatal(err)
	}

	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(
		context.Background(), auth.User{ID: "1", UserID: "1"}, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(resolution.Permissions, indexMetaListPermission) {
		t.Fatalf("permissions = %v: a project with per-project rows must not pick up the central "+
			"fallback — this shape is exactly the reproduced defect", resolution.Permissions)
	}
}

func applyIndexMetaGrant(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	sql, err := migrations.Files.ReadFile("shared/0066_index_meta_permissions.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if _, err := pool.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("apply 0066: %v", err)
	}
}
