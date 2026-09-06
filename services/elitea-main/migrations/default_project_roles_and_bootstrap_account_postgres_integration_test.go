package migrations_test

// shared/0111, measured against a CLEAN database — the only arrangement that
// can fail (F4).
//
// Both defects live in rows that internal/infra/db/migrations/001_initial.sql
// hand-writes, and that file runs only when centry.project is absent
// (migrate.Bootstrap). Every dev machine, every dump-loaded database and every
// long-lived deployment already has whatever it has; only a fresh install is
// built from the seed. So a migration that repaired nothing would look correct
// everywhere except on the installs it is for.
//
// newMigratedPool applies exactly that arrangement: the bootstrap schema, then
// the whole shared corpus.

import (
	"context"
	"slices"
	"testing"
	"time"
)

const defaultProjectRolesDeadline = 30 * time.Second

// The four roles projectprovisioning's `project_permissions` step writes for
// every project it creates: the central default-mode role NAMES plus `system`.
// 001_initial.sql seeds admin, editor and viewer as the default-mode roles.
var wantSharedProjectRoles = []string{"admin", "editor", "system", "viewer"}

// A clean database gives the shared project the same roles as every other
// project.
//
// Without them the member write cannot resolve a single role name for project
// 1 (eliteacore/users_write.go resolveProjectRoleIDs reads
// auth_core__project_role for the named project), so the shared/AI project can
// have no members and appears in nobody's switcher.
func TestCleanDatabaseGivesTheSharedProjectItsRoles(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), defaultProjectRolesDeadline)
	defer cancel()

	rows, err := pool.Query(ctx,
		`SELECT name FROM public.auth_core__project_role WHERE project_id = 1 ORDER BY name`)
	if err != nil {
		t.Fatalf("read the shared project's roles: %v", err)
	}
	defer rows.Close()

	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan a role row: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the role rows: %v", err)
	}
	if !slices.Equal(names, wantSharedProjectRoles) {
		t.Fatalf("auth_core__project_role for project 1 = %v, want %v", names, wantSharedProjectRoles)
	}
}

// The roles come WITHOUT per-project permission rows.
//
// legacyrbac.projectPermissions() falls back to the central default-mode grants
// by role NAME, and that fallback is suppressed for any project that carries
// per-project rows. Seeding permissions alongside the roles would therefore cut
// the shared project off from every grant migration in this corpus — the exact
// trap shared/0090 exists to undo for the projects that already have them.
func TestTheSharedProjectRolesCarryNoPerProjectPermissions(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), defaultProjectRolesDeadline)
	defer cancel()

	var overrides int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM public.auth_core__project_role_permission WHERE project_id = 1`,
	).Scan(&overrides); err != nil {
		t.Fatalf("count the per-project permission rows: %v", err)
	}
	if overrides != 0 {
		t.Fatalf("project 1 carries %d per-project permission rows; the central "+
			"default-mode fallback is now suppressed for the shared project", overrides)
	}
}

// The bootstrap account keeps its row and loses its implicit global admin.
//
// It cannot sign in — it has no auth_core__user_provider link, and ADR-0017
// removed the bypass that used it — but the OIDC path adopts an EXISTING
// account by e-mail, so the administration role was a standing escalation for
// anyone who could obtain an identity at that address.
func TestTheBootstrapAccountKeepsItsRowAndLosesItsCentralRoles(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), defaultProjectRolesDeadline)
	defer cancel()

	// The row survives. Integration fixtures own projects and social profiles
	// through it, and the foreign keys cascade.
	var email string
	if err := pool.QueryRow(ctx,
		`SELECT email FROM public.auth_core__user WHERE id = 1`).Scan(&email); err != nil {
		t.Fatalf("the bootstrap account is gone, and its owned rows cascade with it: %v", err)
	}
	if email != "dev@elitea.ai" {
		t.Fatalf("user 1 e-mail = %q, want dev@elitea.ai; the fixture this test "+
			"measures is not the one 001_initial.sql seeds", email)
	}

	rows, err := pool.Query(ctx, `
SELECT role.mode || '|' || role.name
FROM public.auth_core__user_role AS assignment
JOIN public.auth_core__role AS role ON role.id = assignment.role_id
WHERE assignment.user_id = 1
ORDER BY 1`)
	if err != nil {
		t.Fatalf("read the bootstrap account's central roles: %v", err)
	}
	defer rows.Close()

	held := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan a role assignment: %v", err)
		}
		held = append(held, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the role assignments: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("the bootstrap account still holds %v. It has no identity-provider "+
			"link, so nobody signs in as it deliberately — but the OIDC path adopts "+
			"an existing account by e-mail, and whoever obtains dev@elitea.ai "+
			"inherits these on first login", held)
	}
}

// The fence, in the direction that matters. An operator who ADOPTED the
// bootstrap account — renamed it, linked an identity to it, or signed in as it
// — keeps everything, because a migration must not revoke the roles of an
// account somebody is actually using.
//
// Without this case the revoke above could be an unconditional DELETE and read
// as correct.
func TestAnAdoptedBootstrapAccountKeepsItsCentralRoles(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), defaultProjectRolesDeadline)
	defer cancel()

	// Re-create the pre-0111 state and adopt the account: a provider link is
	// the strongest of the three signals, because it means an identity
	// provider has asserted this identity.
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT 1, role.id
FROM public.auth_core__role AS role
WHERE role.mode = 'administration' AND role.name = 'admin'
ON CONFLICT (user_id, role_id) DO NOTHING`); err != nil {
		t.Fatalf("restore the pre-0111 grant: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_provider (user_id, provider_ref)
VALUES (1, 'oidc:adopted-by-an-operator')
ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("link an identity provider to the bootstrap account: %v", err)
	}

	// 0111 is checksum-recorded, so it does not run twice. Re-running its
	// revoke statement is what this test needs, and it is copied from the file
	// rather than paraphrased: a fence that drifts from the migration proves
	// nothing about the migration.
	if _, err := pool.Exec(ctx, `
DELETE FROM public.auth_core__user_role AS assignment
WHERE assignment.user_id = 1
  AND EXISTS (
      SELECT 1
      FROM public.auth_core__user AS account
      WHERE account.id = 1
        AND account.email = 'dev@elitea.ai'
        AND account.last_login IS NULL
  )
  AND NOT EXISTS (
      SELECT 1
      FROM public.auth_core__user_provider AS link
      WHERE link.user_id = 1
  )`); err != nil {
		t.Fatalf("re-run the revoke: %v", err)
	}

	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM public.auth_core__user_role WHERE user_id = 1`,
	).Scan(&remaining); err != nil {
		t.Fatalf("count the surviving assignments: %v", err)
	}
	if remaining == 0 {
		t.Fatal("the revoke stripped an ADOPTED bootstrap account: the fence on " +
			"auth_core__user_provider does not hold, so a deployment that made this " +
			"account its administrator loses that administrator on upgrade")
	}
}
