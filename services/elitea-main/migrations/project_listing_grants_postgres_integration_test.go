package migrations_test

// The two ADMINISTRATION-mode grants shared/0085 adds, measured against a CLEAN
// database (#313).
//
// # What breaks without them
//
// The admin panel's project member dialog and its project activity drawer read
// two listings for a project the operator is NOT a member of:
//
//	GET /api/v2/admin/users/administration/{projectID}
//	GET /api/v2/admin/roles/administration/{projectID}
//
// Both routes are gated in the ADMINISTRATION mode, because a default-mode gate
// resolves purely from membership of the named project and refuses every
// operator by construction. No migration granted either string in that mode. So
// gating the routes without 0085 answers 403 to every caller, which reads as a
// broken dialog rather than as a missing grant. That is the shape of #354 and
// #359, and it is why the grant and the gate land together.
//
// # Why this file is separate from remaining_permission_grants
//
// That file's adminPanelGrants table carries one claim this surface breaks:
// TestAnAdministrationViewerIsRefusedEveryAdminPanelGate asserts the
// administration viewer resolves NONE of its strings. 0085 gives the viewer
// `configuration.roles.roles.view`, because the legacy matrix does. Folding
// these two rows into that table would force that claim to be weakened for
// every string in it. The split this surface has is real and narrow, so it is
// stated here instead.
//
// # Both directions, per role
//
// Only the pair discriminates. A grant that reached nobody and a grant that
// reached everybody both pass a one-sided check, and a gate that refuses every
// caller reads as a working gate. So every role is measured in both directions
// in ONE resolution: the strings it must resolve, and the strings it must not.

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// projectListingGrants are shared/0085's two strings, with the role names
// testdata/postgres/legacy-rbac-matrix.json gives them in the ADMINISTRATION
// mode.
//
// `system` is dropped, as everywhere else in this corpus: 0060 seeds four
// administration-mode roles — super_admin, admin, editor and viewer — and
// `system` is not one of them.
//
// The holders are sorted, because administrationGrantHolders sorts what it
// reads.
var projectListingGrants = []surfaceGrant{
	// The member listing. coreHandler.Users answers with every member's email,
	// name and roles for the named project.
	{"configuration.users.users.view", []string{"admin", "editor", "super_admin"}},
	// The project role listing. The matrix gives this one to the viewer as
	// well, and it is the ONLY place these two listings differ.
	{"configuration.roles.roles.view", []string{"admin", "editor", "super_admin", "viewer"}},
}

// theOneStringTheViewerDoesNotGet names the asymmetry so a reader does not have
// to derive it from the table, and so a change to the table cannot quietly
// remove the case this file exists to pin.
const theOneStringTheViewerDoesNotGet = "configuration.users.users.view"

/* ── the ledger: what a clean database grants ──────────────────────────── */

// A clean database grants each string to exactly the roles the matrix records.
//
// This reads the grant ROWS, not a status code, so a change to the HTTP surface
// cannot mask a lost grant.
func TestCleanDatabaseGrantsBothProjectListingsInAdministrationMode(t *testing.T) {
	pool := newMigratedPool(t)

	for _, grant := range projectListingGrants {
		holders := administrationGrantHolders(t, pool, grant.permission)
		if !slices.Equal(holders, grant.holders) {
			t.Errorf("administration-mode holders of %s = %v, want %v.\n"+
				"  centralPermissions() has NO super-admin bypass, so with no grant the operator\n"+
				"  is refused too and the admin panel's member dialog cannot load.",
				grant.permission, holders, grant.holders)
		}
	}
}

/* ── the entitled direction ────────────────────────────────────────────── */

// An administration-mode admin resolves BOTH listings.
//
// This is the operator's own path, and the one the member dialog needs. Remove
// shared/0085 and this test fails.
func TestAnAdministrationAdminPassesBothProjectListingGates(t *testing.T) {
	pool := newMigratedPool(t)
	seedAdministrationRole(t, pool, 3131, "admin")

	resolution := resolveAdministrationModeFor(t, pool, "3131")

	for _, grant := range projectListingGrants {
		if !slices.Contains(resolution.Permissions, grant.permission) {
			t.Errorf("an administration admin does not resolve %s on a clean database, so the "+
				"admin panel's project member dialog answers 403 to the operator", grant.permission)
		}
	}
}

/* ── the refused direction, against a REAL role ────────────────────────── */

// The administration viewer is the discriminating role, and it is measured in
// BOTH directions in one resolution.
//
// The matrix gives that role the ROLE listing and withholds the MEMBER listing.
// A migration that granted both would pass an entitled-only check and would
// widen what a viewer may read; a migration that granted neither would pass a
// refusal-only check and would leave the role listing dead.
//
// The asymmetry costs no caller a screen, which is why 0085 transcribes it
// rather than correcting it the way #402 corrected two other splits: the dialog
// that reads these listings sits behind the admin Projects page, and the matrix
// withholds `projects.projects.projects.view` from the administration viewer AND
// from the administration editor. Neither role can reach the page at all. If
// that page's gate ever changes, this comment is wrong and the split has to be
// read again.
func TestAnAdministrationViewerGetsTheRoleListingAndNotTheMemberListing(t *testing.T) {
	pool := newMigratedPool(t)
	seedAdministrationRole(t, pool, 3132, "viewer")

	resolution := resolveAdministrationModeFor(t, pool, "3132")

	for _, grant := range projectListingGrants {
		entitled := slices.Contains(grant.holders, "viewer")
		resolved := slices.Contains(resolution.Permissions, grant.permission)
		switch {
		case entitled && !resolved:
			t.Errorf("an administration viewer does not resolve %s, and the ledger above gives "+
				"it to that role", grant.permission)
		case !entitled && resolved:
			t.Errorf("an administration viewer resolves %s, and the ledger above withholds it. "+
				"This grant widens what a viewer may read: the member listing carries every "+
				"member's email address.", grant.permission)
		}
	}

	// Stated separately from the loop, because the loop passes unchanged if the
	// table is edited to give the viewer both strings. This case is the reason
	// the file exists.
	if slices.Contains(resolution.Permissions, theOneStringTheViewerDoesNotGet) {
		t.Errorf("an administration viewer resolves %s. The two listings are NOT the same grant: "+
			"the role listing names roles, the member listing names people.",
			theOneStringTheViewerDoesNotGet)
	}
}

// A caller who holds a DEFAULT-mode project role and no administration role
// resolves neither string.
//
// centralPermissions() reads the grants by role NAME from auth_core__user_role.
// A caller with no row there resolves nothing, which is what keeps these two
// central grants from reaching an ordinary project member. Without this case a
// central grant that leaked into the project tier would still pass the two
// tests above.
//
// The caller is created HERE rather than reusing user 1. 001_initial.sql seeds
// user 1 with a central administration role, so user 1 resolves both strings
// legitimately and could not show this. The user and role ids are this file's
// own, so they cannot collide with the ids the other migration tests seed.
func TestAProjectRoleAloneResolvesNeitherProjectListing(t *testing.T) {
	pool := newMigratedPool(t)
	seedProjectOnlyCaller(t, pool, 3133, 3133)

	resolution := resolveAdministrationModeFor(t, pool, "3133")

	for _, grant := range projectListingGrants {
		if slices.Contains(resolution.Permissions, grant.permission) {
			t.Errorf("a caller holding only a DEFAULT-mode project role resolves %s in the "+
				"administration mode. A project role must not reach the admin panel.",
				grant.permission)
		}
	}
}

// seedProjectOnlyCaller creates a caller who holds a DEFAULT-mode project role
// in project 1 and NO central role of any kind.
//
// It does not reuse seedRoleMembership, which assumes the user row already
// exists, nor user 1, which 001_initial.sql seeds WITH a central administration
// role. Either shortcut resolves both strings for a legitimate reason and would
// make the caller in this file's last test indistinguishable from an operator.
func seedProjectOnlyCaller(t *testing.T, pool *pgxpool.Pool, userID, roleID int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The email is built in Go. Writing `$1 || '@example.com'` makes PostgreSQL
	// deduce both integer and text for the same parameter, and it refuses with
	// SQLSTATE 42P08.
	email := fmt.Sprintf("project-only-%d@example.com", userID)
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name)
VALUES ($1, $2, 'Project-only caller')
ON CONFLICT (id) DO NOTHING`, userID, email); err != nil {
		t.Fatalf("seed the project-only caller: %v", err)
	}
	// The conflict target is (project_id, name), not (id): shared/0111 gives
	// project 1 the four roles every provisioned project has, so `admin` is
	// already there under an id this fixture did not choose. The membership is
	// assigned against the row that exists.
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES ($1, 1, 'admin')
ON CONFLICT (project_id, name) DO NOTHING`, roleID); err != nil {
		t.Fatalf("seed the project role: %v", err)
	}
	var adminRoleID int
	if err := pool.QueryRow(ctx,
		`SELECT id FROM public.auth_core__project_role WHERE project_id = 1 AND name = 'admin'`,
	).Scan(&adminRoleID); err != nil {
		t.Fatalf("resolve the project role: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES (1, $1, $2)
ON CONFLICT DO NOTHING`, userID, adminRoleID); err != nil {
		t.Fatalf("assign the project role: %v", err)
	}
}
