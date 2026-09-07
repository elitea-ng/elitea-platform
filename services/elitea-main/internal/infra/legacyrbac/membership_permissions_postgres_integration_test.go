package legacyrbac_test

// ResolveMembershipPermissions, against a real PostgreSQL (#830).
//
// The method answers "what does this caller hold ANYWHERE" — the union over
// every project they are a member of. It exists for the routes whose answer is
// already scoped to those memberships, the project list first among them: that
// route resolved its gate against the public project in its path, so an account
// enrolled only in its own personal project was refused its own projects.
//
// Each case below pins one rule the single-project ResolvePermissions already
// obeys, because the union must not lose any of them.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

// The reported shape: the caller holds the permission in the one project they
// belong to, and the public project is not it.
func TestMembershipPermissionsSeeAProjectTheCallerIsNotAskedAbout(t *testing.T) {
	pool := newGrantPool(t)
	seedMembershipShape(t, pool)
	resolver := legacyrbac.NewPostgresResolver(pool)
	member := auth.User{ID: "9", UserID: "9"}

	// The single-project answer for the PUBLIC project is empty, which is what
	// the project-list gate used to ask for and refuse on.
	public, err := resolver.ResolvePermissions(
		context.Background(), member, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(public.Permissions) != 0 {
		t.Fatalf("public-project permissions = %v, want empty — the premise of this test is that "+
			"the caller is not a member of project 1", public.Permissions)
	}

	resolution, err := resolver.ResolveMembershipPermissions(
		context.Background(), member, auth.PermissionModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.UserID != 9 {
		t.Fatalf("resolved user = %d, want 9", resolution.UserID)
	}
	if len(resolution.Permissions) != 1 || resolution.Permissions[0] != "projects.projects.project.view" {
		t.Fatalf("membership permissions = %v, want [projects.projects.project.view]", resolution.Permissions)
	}
}

// A user with no membership is not refused — it is an EMPTY answer, which is
// what turns into 403 at the gate rather than a 500.
func TestMembershipPermissionsAreEmptyForANonMember(t *testing.T) {
	pool := newGrantPool(t)
	seedMembershipShape(t, pool)
	resolver := legacyrbac.NewPostgresResolver(pool)

	resolution, err := resolver.ResolveMembershipPermissions(
		context.Background(), auth.User{ID: "8", UserID: "8"}, auth.PermissionModeDefault)
	if err != nil {
		t.Fatalf("non-member resolution = %v, want an empty answer and no error", err)
	}
	if len(resolution.Permissions) != 0 {
		t.Fatalf("non-member permissions = %v, want empty", resolution.Permissions)
	}
}

// The union covers every membership, and a suspended project contributes
// nothing — the rule requireActiveProject enforces on the single-project path.
func TestMembershipPermissionsUnionSkipsSuspendedProjects(t *testing.T) {
	pool := newGrantPool(t)
	seedMembershipShape(t, pool)
	ctx := context.Background()
	// Project 4 carries its own per-project grant, so its member reads that
	// grant and not the central fallback.
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
VALUES (4, 14, 'models.something.else')`); err != nil {
		t.Fatal(err)
	}
	resolver := legacyrbac.NewPostgresResolver(pool)
	member := auth.User{ID: "7", UserID: "7"}

	resolution, err := resolver.ResolveMembershipPermissions(ctx, member, auth.PermissionModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Permissions) != 2 ||
		resolution.Permissions[0] != "models.something.else" ||
		resolution.Permissions[1] != "projects.projects.project.view" {
		t.Fatalf("union = %v, want the per-project grant of project 4 and the central fallback of "+
			"project 3 — the fallback is suppressed PER PROJECT, not globally", resolution.Permissions)
	}

	if _, err := pool.Exec(ctx, `UPDATE centry.project SET suspended = true WHERE id = 4`); err != nil {
		t.Fatal(err)
	}
	afterSuspension, err := resolver.ResolveMembershipPermissions(ctx, member, auth.PermissionModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterSuspension.Permissions) != 1 ||
		afterSuspension.Permissions[0] != "projects.projects.project.view" {
		t.Fatalf("union after suspension = %v, want the suspended project to contribute nothing",
			afterSuspension.Permissions)
	}
}

// A suspended USER is refused outright, exactly as ResolvePermissions refuses
// one. The membership union must not become a way around that check.
func TestMembershipPermissionsRefuseASuspendedUser(t *testing.T) {
	pool := newGrantPool(t)
	seedMembershipShape(t, pool)
	if _, err := pool.Exec(context.Background(),
		`UPDATE public.auth_core__user SET suspended = true WHERE id = 9`); err != nil {
		t.Fatal(err)
	}
	resolver := legacyrbac.NewPostgresResolver(pool)

	_, err := resolver.ResolveMembershipPermissions(
		context.Background(), auth.User{ID: "9", UserID: "9"}, auth.PermissionModeDefault)
	if err == nil {
		t.Fatal("suspended user resolved, want a refusal")
	}
}

// The CENTRAL modes never consult a project, so the membership resolver must
// answer them exactly as ResolvePermissions does.
func TestMembershipPermissionsAnswerCentralModesUnchanged(t *testing.T) {
	pool := newGrantPool(t)
	seedMembershipShape(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__role (id, name, mode) VALUES (4, 'admin', 'administration');
INSERT INTO public.auth_core__role_permission (role_id, permission) VALUES (4, 'projects.projects.project.create');
INSERT INTO public.auth_core__user_role (user_id, role_id) VALUES (8, 4)`); err != nil {
		t.Fatal(err)
	}
	resolver := legacyrbac.NewPostgresResolver(pool)
	administrator := auth.User{ID: "8", UserID: "8"}

	central, err := resolver.ResolvePermissions(
		ctx, administrator, auth.PermissionModeAdministration, "")
	if err != nil {
		t.Fatal(err)
	}
	membership, err := resolver.ResolveMembershipPermissions(
		ctx, administrator, auth.PermissionModeAdministration)
	if err != nil {
		t.Fatal(err)
	}
	if len(membership.Permissions) != 1 || membership.Permissions[0] != central.Permissions[0] {
		t.Fatalf("central-mode membership answer = %v, want the same as ResolvePermissions %v",
			membership.Permissions, central.Permissions)
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

// seedMembershipShape is a Go-provisioned install after
// 0081_project_permissions.sql: default-mode roles carrying the central grant,
// no per-project grant rows, a public project the fresh accounts do not belong
// to, and their own projects beside it.
func seedMembershipShape(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success) VALUES
			(1, 'promptlib_public', 5, '{}', true),
			(3, 'team_alpha', 7, '{}', true),
			(4, 'team_beta', 7, '{}', true),
			(5, 'project_user_9', 9, '{}', true)`,
		`INSERT INTO public.auth_core__user (id, email, name) VALUES
			(7, 'two@example.com', 'Two'),
			(8, 'nobody@example.com', 'Nobody'),
			(9, 'alice@example.com', 'Alice')`,
		`INSERT INTO public.auth_core__role (id, name, mode) VALUES
			(1, 'admin', 'default'), (2, 'editor', 'default'), (3, 'viewer', 'default')`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT id, 'projects.projects.project.view' FROM public.auth_core__role WHERE mode = 'default'`,
		`INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES
			(13, 5, 'admin'),
			(14, 4, 'editor'),
			(15, 3, 'editor')`,
		`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES
			(5, 9, 13),
			(4, 7, 14),
			(3, 7, 15)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}
