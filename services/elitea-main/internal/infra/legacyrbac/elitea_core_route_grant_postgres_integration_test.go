package legacyrbac_test

// The blast radius of the default-mode grants added by
// migrations/shared/0068_elitea_core_route_permissions.sql (#302, #313), pinned
// the way 0062's, 0063's and 0066's are.
//
// The defect this migration closes is the one that stopped #302 half way: the
// /elitea_core routes' legacy permission names were recoverable, but on a
// Go-bootstrapped database nothing granted them, so gating the routes would
// have answered 403 to every caller including the operator. 0063's header
// states it — "gating a route on a permission nothing grants is
// 403-for-everyone, which reads as a broken page rather than as a missing
// grant".
//
// The cases below assert against the migration FILE rather than a copy of its
// SQL, so an edit that changed which roles it grants cannot pass here, and they
// discriminate the per-role split the legacy matrix gives: an editor gets the
// writes, a viewer gets the reads and NOT the writes. A file that granted
// everything to everybody would pass a test that only checked the editor.

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const eliteaCoreRouteGrantMigration = "shared/0068_elitea_core_route_permissions.sql"

// A sample of the strings the router now gates on, one per shape the migration
// distinguishes. Reads first, then writes; the viewer case below turns on
// exactly that boundary.
var (
	eliteaCoreMemberReads = []string{
		"models.applications.applications.list",
		"models.applications.skills.list",
		"models.applications.tools.list",
		"models.chat.conversations.list",
		"models.chat.folders.get",
		"models.promptlib_shared.tags.list",
		"configuration.users.users.view",
	}
	eliteaCoreEditorWrites = []string{
		"models.applications.applications.create",
		"models.applications.publish.post",
		"models.applications.unpublish.post",
		"models.applications.upload_icon.post",
		"models.applications.export_import.import",
		"models.project_context.edit",
	}
)

// An editor member gains both halves. The `before` half is the test's premise:
// it fails against the pre-0068 database, where every one of these routes would
// be 403 for every caller.
func TestEliteaCoreRouteGrantsResolveForAnEditorMember(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	resolver := legacyrbac.NewPostgresResolver(pool)
	member := auth.User{ID: "1", UserID: "1"}

	before, err := resolver.ResolvePermissions(context.Background(), member, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, permission := range append(slices.Clone(eliteaCoreMemberReads), eliteaCoreEditorWrites...) {
		if slices.Contains(before.Permissions, permission) {
			t.Fatalf("%s resolves before the migration; this test's premise is that a fresh "+
				"Go database grants none of the /elitea_core route permissions", permission)
		}
	}

	applyEliteaCoreRouteGrant(t, pool)

	after, err := resolver.ResolvePermissions(context.Background(), member, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, permission := range append(slices.Clone(eliteaCoreMemberReads), eliteaCoreEditorWrites...) {
		if !slices.Contains(after.Permissions, permission) {
			t.Fatalf("%s does not resolve after the migration: the route it gates would be 403 for "+
				"every member, which the browser renders as a broken page rather than a refusal",
				permission)
		}
	}
}

// A viewer member gains the reads and NOT the writes. This is the half that
// makes the per-role split load-bearing rather than decorative: granting the
// families uniformly would let a viewer publish a project's agent into the
// public catalogue.
func TestEliteaCoreRouteGrantsGiveAViewerTheReadsOnly(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES (12, 1, 'viewer');
INSERT INTO public.auth_core__user (id, email, name) VALUES (5, 'view313@example.com', 'View');
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES (1, 5, 12)`); err != nil {
		t.Fatal(err)
	}
	applyEliteaCoreRouteGrant(t, pool)

	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(
		context.Background(), auth.User{ID: "5", UserID: "5"}, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, permission := range eliteaCoreMemberReads {
		if !slices.Contains(resolution.Permissions, permission) {
			t.Fatalf("a viewer cannot resolve %s; the legacy matrix grants a default-mode viewer "+
				"every one of these reads: %v", permission, resolution.Permissions)
		}
	}
	for _, permission := range eliteaCoreEditorWrites {
		if slices.Contains(resolution.Permissions, permission) {
			t.Fatalf("a viewer resolves %s; the legacy matrix withholds every one of these writes "+
				"from a default-mode viewer", permission)
		}
	}
}

// A user with NO role in the project gains nothing, which is what makes the
// per-route permission gate a strict strengthening of the membership check it
// replaces rather than a swap: the central fallback is joined THROUGH the
// caller's assigned project roles, so a non-member has no row to fall back
// from and resolves the empty set before any permission is compared.
func TestEliteaCoreRouteGrantsGiveANonMemberNothing(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	applyEliteaCoreRouteGrant(t, pool)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO public.auth_core__user (id, email, name) VALUES (6, 'out313@example.com', 'Out')`); err != nil {
		t.Fatal(err)
	}

	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(
		context.Background(), auth.User{ID: "6", UserID: "6"}, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Permissions) != 0 {
		t.Fatalf("non-member permissions = %v, want empty", resolution.Permissions)
	}
}

// A project that carries its OWN per-project grants — the shape every
// legacy-backed database, every legacy dump and the E2E stack all have —
// suppresses the central fallback entirely, so this migration cannot change
// what an existing deployment's members can do. It is also why a seeded stack
// has to list these strings itself.
func TestEliteaCoreRouteGrantsAreInertForAProjectWithItsOwnGrants(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	applyEliteaCoreRouteGrant(t, pool)

	if _, err := pool.Exec(context.Background(), `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
VALUES (1, 10, 'models.applications.applications.list')`); err != nil {
		t.Fatal(err)
	}

	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(
		context.Background(), auth.User{ID: "1", UserID: "1"}, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Permissions) != 1 ||
		resolution.Permissions[0] != "models.applications.applications.list" {
		t.Fatalf("permissions = %v, want only the project's own grant — a project with per-project "+
			"rows must not pick up the central fallback, which is what bounds this migration's "+
			"blast radius to fresh Go databases", resolution.Permissions)
	}
}

// Applying it twice changes nothing. Migrations are checksum-immutable and run
// once, but elitea-migrate also runs against databases that already carry a
// legacy dump's rows, and ON CONFLICT DO NOTHING is what keeps that from
// erroring on the unique (role_id, permission) key.
func TestEliteaCoreRouteGrantIsIdempotent(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	applyEliteaCoreRouteGrant(t, pool)

	var first int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM public.auth_core__role_permission`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	applyEliteaCoreRouteGrant(t, pool)

	var second int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM public.auth_core__role_permission`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("row count moved from %d to %d on a second apply", first, second)
	}
}

func applyEliteaCoreRouteGrant(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	sql, err := migrations.Files.ReadFile(eliteaCoreRouteGrantMigration)
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if _, err := pool.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("apply 0068: %v", err)
	}
}
