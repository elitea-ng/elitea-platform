package migrations_test

// The 41 permissions that #386 grants, measured against a CLEAN database.
//
// # Why this file exists, and why no other test could catch this
//
// Each permission below gates a mounted production route. No migration granted
// any of them. So on a clean database each route answered 403 to EVERY caller,
// the operator included. Whole surfaces were dead: secrets, artifacts,
// notifications, social, projects and the admin panel.
//
// This is the fourth instance of one class. #354 was one permission. The audit
// it forced (#359) found six more, closed by migration 0072. The gate added by
// #372, internal/api/router_permission_grant_gate_test.go, then found 41.
//
// # Why every earlier suite stayed green
//
// apps/elitea-web/scripts/e2e-stack.sh grants permissions as PER-PROJECT
// auth_core__project_role_permission rows. Those rows supply the permissions
// directly, and they suppress legacyrbac's central default-mode fallback
// completely. A pylon-backed database and a legacy dump hold the same shape. So
// only a clean Go bootstrap breaks, and no seeded suite could see it.
//
// So this file asserts against the corpus a real deployment applies, and against
// nothing else. newMigratedPool CREATEs an empty database, applies
// 001_initial.sql, then applies every shared and tenant migration through the
// real runner. No seed script runs here. Remove shared/0074 to shared/0082 and
// this file fails; no other test does.
//
// # Both directions, per surface
//
// Only the pair of directions discriminates. A gate that refuses EVERY caller
// passes a positive-only suite, and so does a gate that admits every caller. So
// every surface is measured twice:
//
//	entitled caller passes    → TestAnAdminPassesEveryDefaultModeGate
//	                          → TestAnAdministrationAdminPassesEveryAdminPanelGate
//	unentitled caller refused → TestAViewerIsRefusedTheRestrictedGates
//	                            (a REAL role that the matrix withholds from)
//	                          → TestAnAdministrationViewerIsRefusedEveryAdminPanelGate
//	                          → TestANonMemberIsRefusedEveryDefaultModeGate
//	                          → TestAProjectWithItsOwnGrantsIsRefusedEveryGate
//
// Every assertion reads the grant ROWS or the resolver, never a status code, so
// a change to the HTTP surface cannot mask a lost grant.
//
// # Why the permission strings are literals here
//
// Several of these gates name their permission inline in router.go rather than
// through an exported constant, so this table cannot read them all from
// production. The coupling that keeps the literals honest is
// router_permission_grant_gate_test.go: it parses every gate call site and every
// grant in migrations/shared. Rename a permission at its route and that gate
// fails, because the new string has no grant. So a rename cannot leave this
// table silently measuring a dead string.

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

// surfaceGrant is one permission, and the role names the legacy matrix gives it.
//
// `holders` is sorted, because defaultModeGrantHolders sorts what it reads.
type surfaceGrant struct {
	permission string
	holders    []string
}

// The role split for every string below is transcribed from
// testdata/postgres/legacy-rbac-matrix.json, with THREE stated exceptions.
// `system` and `super_admin` are dropped from the DEFAULT mode, as everywhere
// else in this corpus: Go seeds neither role there. `system` is dropped from the
// ADMINISTRATION mode too, for the reason 0060 and 0061 give: it is not in the
// Go product's role vocabulary.
//
// THE THREE EXCEPTIONS are `configuration.secrets.secret.list`,
// `models.social.avatar.get` and `models.social.avatar.update`. #402 decided
// each one against the matrix, on a product reason, and
// shared/0083_viewer_secret_list_and_own_avatar.sql adds the holders. Read that
// file's header before you change any holder set here. Every other row still
// transcribes the matrix, and that stays the rule.
var (
	// shared/0074_artifact_permissions.sql, and shared/0088 for the machine
	// role.
	//
	// All four are on the per-project system identity's callback surface: the
	// SDK's artifact toolkit lists the bucket and reads every object with the
	// project-system PAT, and writes with it too. 0088 seeds the central
	// default-mode `system` role those grants hang on, so `system` is an
	// expected holder here. withMachineRole names it at each such row, so a
	// grant that reaches `system` anywhere else still fails.
	artifactGrants = []surfaceGrant{
		{"configuration.artifacts.artifacts.view", withMachineRole("admin", "editor", "viewer")},
		{"configuration.artifacts.artifacts.create", withMachineRole("admin", "editor")},
		{"configuration.artifacts.artifacts.edit", withMachineRole("admin", "editor")},
		{"configuration.artifacts.artifacts.delete", withMachineRole("admin", "editor")},
	}

	// shared/0075_secret_permissions.sql, and shared/0083 for the LIST.
	//
	// The matrix withholds every string here from the viewer, the list
	// included. #402 decided that the list is wrong for the product and 0083
	// adds the viewer to it. The list discloses secret NAMES only; the VALUE
	// is `unsecret`, on a different route, and it stays with admin and editor.
	// 0083's header carries the full reason.
	secretGrants = []surfaceGrant{
		{"configuration.secrets.secret.list", []string{"admin", "editor", "viewer"}},
		{"configuration.secrets.secret.create", []string{"admin", "editor"}},
		{"configuration.secrets.secret.edit", []string{"admin", "editor"}},
		{"configuration.secrets.secret.delete", []string{"admin", "editor"}},
		{"configuration.secrets.secret.hide", []string{"admin", "editor"}},
		// The value read is on the machine role's callback surface too
		// (shared/0088): a worker resolves `{{secret.<name>}}` with the
		// project-system PAT.
		{"configuration.secrets.secret.unsecret", withMachineRole("admin", "editor")},
	}

	// shared/0076_project_member_permissions.sql. 0068 already grants the
	// matching `…users.view`, which is why the members page LOADS and every
	// button on it answered 403.
	memberGrants = []surfaceGrant{
		{"configuration.users.users.create", []string{"admin"}},
		{"configuration.users.users.edit", []string{"admin"}},
		{"configuration.users.users.delete", []string{"admin"}},
	}

	// shared/0077_moderation_permissions.sql. A viewer may ASK for a moderation
	// decision. Only the administration mode MAKES one; see adminPanelGrants.
	moderationGrants = []surfaceGrant{
		{"admin.moderation.view", []string{"admin", "editor", "viewer"}},
		{"admin.moderation.create", []string{"admin", "editor", "viewer"}},
	}

	// shared/0078_scheduling_permissions.sql
	schedulingGrants = []surfaceGrant{
		{"configuration.scheduling.schedules.view", []string{"admin"}},
		{"configuration.scheduling.schedules.edit", []string{"admin"}},
	}

	// shared/0079_notification_permissions.sql. The update and the delete
	// differ, and that is the matrix: a viewer may delete a notification and may
	// not update one. This file transcribes the matrix rather than correct it.
	notificationGrants = []surfaceGrant{
		{"models.notifications.notifications.list", []string{"admin", "editor", "viewer"}},
		{"models.notifications.notification.details", []string{"admin", "editor", "viewer"}},
		{"models.notifications.notification.delete", []string{"admin", "editor", "viewer"}},
		{"models.notifications.notification.update", []string{"admin", "editor"}},
	}

	// shared/0080_social_permissions.sql, and shared/0083 for the two avatar
	// strings.
	//
	// The matrix gives `models.social.avatar.get` to admin alone, and has no
	// entry for `models.social.avatar.update`; 0080 mapped the second onto
	// `models.social.avatar.post`, the legacy name for the same operation, so
	// it took the same holders. #402 decided that both are wrong for the
	// product, because the two routes are PER-USER: avatar.go reads the user id
	// from the principal, so the permission decides only whether the caller may
	// act on the caller. 0083 gives both to every default-mode role Go seeds.
	// 0083's header records why the gate stays in place.
	socialGrants = []surfaceGrant{
		{"models.social.authors.get", []string{"admin", "editor", "viewer"}},
		{"models.social.feedbacks.create", []string{"admin", "editor", "viewer"}},
		{"models.social.avatar.get", []string{"admin", "editor", "viewer"}},
		{"models.social.avatar.update", []string{"admin", "editor", "viewer"}},
	}

	// shared/0081_project_permissions.sql. `projects.projects.project.view`
	// gates the project LIST, which every session calls first.
	projectGrants = []surfaceGrant{
		{"projects.projects.project.view", []string{"admin", "editor", "viewer"}},
		{"projects.projects.groups.edit", []string{"admin", "editor"}},
		{"projects.projects.group.create", []string{"admin", "editor"}},
		{"projects.projects.group.delete", []string{"admin", "editor"}},
		{"projects.projects.project.edit", []string{"admin"}},
	}

	// shared/0082_admin_panel_permissions.sql, in the ADMINISTRATION mode.
	//
	// `configuration.governance` has no matrix entry. It takes the holders every
	// other central administration grant takes, for the reasons 0082 records.
	// Its gate also needed a code change: router.go used `RequirePermissions`,
	// which reads the never-populated `auth.User.Permissions`.
	adminPanelGrants = []surfaceGrant{
		{"configuration.users.users.create", []string{"admin", "super_admin"}},
		{"configuration.users.users.edit", []string{"admin", "super_admin"}},
		{"configuration.roles.permissions.edit", []string{"admin", "super_admin"}},
		{"projects.projects.projects.edit", []string{"admin", "super_admin"}},
		{"configuration.scheduling.schedules.view", []string{"admin", "super_admin"}},
		{"configuration.scheduling.schedules.edit", []string{"admin", "super_admin"}},
		{"models.admin.audit_trail.view", []string{"admin", "super_admin"}},
		{"runtime.airun.serviceproviders", []string{"admin", "super_admin"}},
		{"provider_hub.descriptor.register", []string{"admin", "super_admin"}},
		// shared/0109_provider_policy_overlay.sql. A CHOSEN string with no
		// pylon original — pylon had no activation because it had no admission
		// plane — kept separate from `.register` so that recording a descriptor
		// and putting it in force are separately grantable. Same two holders,
		// and no editor: activation is a fact about the deployment.
		{"provider_hub.descriptor.activate", []string{"admin", "super_admin"}},
		{"configuration.governance", []string{"admin", "super_admin"}},
		// shared/0114_toolkit_type_policy.sql. A CHOSEN string with no pylon
		// original — the legacy platform has no add-or-remove-a-type surface,
		// only the guardrails deny-list — kept separate from `runtime.plugins`
		// so that editing a plugin's configuration values and deciding which
		// clients may build a toolkit type are separately grantable. 0114 also
		// names `system`, which a clean database seeds no administration role
		// for, so the holder set here is the same two.
		{"toolkit_catalogue.type.manage", []string{"admin", "super_admin"}},
		{"admin.moderation.edit", []string{"admin", "editor", "super_admin"}},
	}
)

// defaultModeSurfaces are the eight surfaces 0074 to 0081 grant, by surface
// name, so a failure names the screen that breaks.
var defaultModeSurfaces = map[string][]surfaceGrant{
	"artifacts":     artifactGrants,
	"secrets":       secretGrants,
	"members":       memberGrants,
	"moderation":    moderationGrants,
	"scheduling":    schedulingGrants,
	"notifications": notificationGrants,
	"social":        socialGrants,
	"projects":      projectGrants,
}

// The count #386 fixes, plus what has been added to this ledger since. It is
// asserted rather than trusted, so a table edited down to fewer strings cannot
// pass quietly.
//
// 42 → 43: `toolkit_catalogue.type.manage`, granted by
// shared/0114_toolkit_type_policy.sql for the five routes of the new
// `Admin › Toolkits` surface.
//
// 41 → 42: `provider_hub.descriptor.activate`, granted by
// shared/0109_provider_policy_overlay.sql. This ledger is the holder test for
// every string it lists, so a new central grant belongs in it or the grant is
// unmeasured on a clean database — which is the whole failure class #386 exists
// for. The number moves with the table on purpose: a count derived with len()
// would agree with any table, including one somebody deleted rows from.
const remainingPermissionCount = 43

/* ── the ledger: what a clean database grants ──────────────────────────── */

// A clean database grants each of the 41 to exactly the roles the legacy matrix
// records. This is the assertion #386 exists for.
func TestCleanDatabaseGrantsEveryRemainingPermission(t *testing.T) {
	pool := newMigratedPool(t)

	measured := 0
	for surface, grants := range defaultModeSurfaces {
		for _, grant := range grants {
			measured++
			holders := defaultModeGrantHolders(t, pool, grant.permission)
			if !slices.Equal(holders, grant.holders) {
				t.Errorf("the %s surface: default-mode holders of %s = %v, want %v.\n"+
					"  With no grant the route answers 403 to every caller and the screen reads as broken.",
					surface, grant.permission, holders, grant.holders)
			}
		}
	}
	for _, grant := range adminPanelGrants {
		measured++
		holders := administrationGrantHolders(t, pool, grant.permission)
		if !slices.Equal(holders, grant.holders) {
			t.Errorf("the admin panel: administration-mode holders of %s = %v, want %v.\n"+
				"  centralPermissions() has NO super-admin bypass, so with no grant the operator is refused too.",
				grant.permission, holders, grant.holders)
		}
	}

	if measured != remainingPermissionCount {
		t.Fatalf("measured %d permissions, want %d: the table no longer covers what #386 fixes",
			measured, remainingPermissionCount)
	}
}

/* ── the entitled direction ────────────────────────────────────────────── */

// An admin of a Go-provisioned project resolves every default-mode string, so
// every one of those gates admits them.
func TestAnAdminPassesEveryDefaultModeGate(t *testing.T) {
	pool := newMigratedPool(t)
	seedRoleMembership(t, pool, 3861, "admin", 1)

	resolution := resolveDefaultModeFor(t, pool, "1", "1")

	for surface, grants := range defaultModeSurfaces {
		for _, grant := range grants {
			if !slices.Contains(resolution.Permissions, grant.permission) {
				t.Errorf("the %s surface: an admin does not resolve %s on a clean database, "+
					"so the route refuses them", surface, grant.permission)
			}
		}
	}
}

// An administration-mode admin resolves every admin-panel string.
//
// This is the operator's own path. Before #386 the admin panel refused the
// operator at each of these gates, and the operator could not grant their way
// out: the admin write APIs are themselves gated on strings in this list.
func TestAnAdministrationAdminPassesEveryAdminPanelGate(t *testing.T) {
	pool := newMigratedPool(t)
	seedAdministrationRole(t, pool, 1, "admin")

	resolution := resolveAdministrationModeFor(t, pool, "1")

	for _, grant := range adminPanelGrants {
		if !slices.Contains(resolution.Permissions, grant.permission) {
			t.Errorf("an administration admin does not resolve %s on a clean database, "+
				"so the admin panel refuses the operator", grant.permission)
		}
	}
}

/* ── the refused direction ─────────────────────────────────────────────── */

// The refused direction against a REAL role rather than an absent one.
//
// A default-mode viewer holds the strings the matrix gives them and none of the
// rest, in ONE resolution. A migration that granted a write string too widely
// fails here, and a migration that granted nothing fails the entitled half.
func TestAViewerIsRefusedTheRestrictedGates(t *testing.T) {
	pool := newMigratedPool(t)
	seedRoleMembership(t, pool, 3862, "viewer", 1)

	resolution := resolveDefaultModeFor(t, pool, "1", "1")

	for surface, grants := range defaultModeSurfaces {
		for _, grant := range grants {
			entitled := slices.Contains(grant.holders, "viewer")
			resolved := slices.Contains(resolution.Permissions, grant.permission)
			switch {
			case entitled && !resolved:
				t.Errorf("the %s surface: a viewer does not resolve %s, and the ledger above "+
					"gives it to the default-mode viewer", surface, grant.permission)
			case !entitled && resolved:
				t.Errorf("the %s surface: a viewer resolves %s, and the ledger above withholds "+
					"it. This grant widens what a viewer may do.", surface, grant.permission)
			}
		}
	}
}

// The refused direction for the admin panel, against a REAL role.
//
// 0060 seeds an administration `viewer` role and grants it nothing. 0082 keeps
// it that way. So the operator's panel refuses a viewer at every gate.
func TestAnAdministrationViewerIsRefusedEveryAdminPanelGate(t *testing.T) {
	pool := newMigratedPool(t)
	seedAdministrationRole(t, pool, 2, "viewer")

	resolution := resolveAdministrationModeFor(t, pool, "2")

	for _, grant := range adminPanelGrants {
		if slices.Contains(resolution.Permissions, grant.permission) {
			t.Errorf("an administration viewer resolves %s; 0082 grants it to super_admin and "+
				"admin only, so this grant widens the admin panel", grant.permission)
		}
	}
}

// A user who belongs to no project gains nothing at all.
//
// projectPermissions()' central fallback joins THROUGH the caller's assigned
// project roles, so a non-member has no row to fall back from. This is what
// keeps 40 central grants from becoming a leak across projects.
func TestANonMemberIsRefusedEveryDefaultModeGate(t *testing.T) {
	pool := newMigratedPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.auth_core__user (id, email, name) VALUES (386, 'out@example.com', 'Out')`); err != nil {
		t.Fatalf("seed the non-member: %v", err)
	}

	resolution := resolveDefaultModeFor(t, pool, "386", "1")
	if len(resolution.Permissions) != 0 {
		t.Fatalf("a non-member resolves %v, want nothing", resolution.Permissions)
	}
}

// A member of a project that carries its OWN per-project grant rows resolves
// only those rows. The central fallback is suppressed for that project, so none
// of the 40 default-mode grants reaches it.
//
// This pins the blast radius. It is the shape of every pylon-backed database,
// every legacy dump and the end-to-end stack. So these migrations cannot change
// what an existing deployment's members may do.
func TestAProjectWithItsOwnGrantsIsRefusedEveryGate(t *testing.T) {
	pool := newMigratedPool(t)
	roleID := seedRoleMembership(t, pool, 3863, "admin", 1)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
VALUES (1, $1, 'models.something.else')`, roleID); err != nil {
		t.Fatalf("seed the per-project grant: %v", err)
	}

	resolution := resolveDefaultModeFor(t, pool, "1", "1")
	if !slices.Equal(resolution.Permissions, []string{"models.something.else"}) {
		t.Fatalf("permissions = %v, want only the project's own grant; the central fallback must "+
			"not apply to a project that carries per-project rows", resolution.Permissions)
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

// administrationGrantHolders reads the ADMINISTRATION-mode grant rows the corpus
// leaves behind. It is defaultModeGrantHolders for the other mode, and the mode
// is the point: legacyrbac resolves each mode against its own roles, so a
// default-mode grant does NOT satisfy an administration-mode gate.
func administrationGrantHolders(t *testing.T, pool *pgxpool.Pool, permission string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	rows, err := pool.Query(ctx, `
SELECT role.name
FROM public.auth_core__role_permission AS grant_row
JOIN public.auth_core__role AS role ON role.id = grant_row.role_id
WHERE role.mode = 'administration' AND grant_row.permission = $1
ORDER BY role.name`, permission)
	if err != nil {
		t.Fatalf("read the administration-mode grant rows for %s: %v", permission, err)
	}
	defer rows.Close()

	holders := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan a grant row: %v", err)
		}
		holders = append(holders, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the grant rows: %v", err)
	}
	slices.Sort(holders)
	return slices.Compact(holders)
}

// resolveAdministrationModeFor makes the call the admin-panel gates make.
//
// `RequireCentralPermissions` resolves auth.PermissionModeAdministration and
// passes no project: centralPermissions() reads auth_core__user_role,
// auth_core__role filtered on the mode, and auth_core__role_permission. The
// project id is not used.
func resolveAdministrationModeFor(t *testing.T, pool *pgxpool.Pool, userID string) auth.PermissionResolution {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(
		ctx,
		auth.User{ID: userID, UserID: userID},
		auth.PermissionModeAdministration,
		"",
	)
	if err != nil {
		t.Fatalf("resolve the caller's administration-mode permissions: %v", err)
	}
	return resolution
}

// seedAdministrationRole gives a user a named administration role.
//
// The role is looked up by name rather than by id. 001_initial.sql seeds the
// four administration roles, and 0060 seeds the same four on a database that has
// none, so the ids are not stable across bootstrap paths.
//
// The user is created when it does not exist. requireUser() refuses a caller
// with no auth_core__user row, so the resolution would fail for the wrong
// reason.
func seedAdministrationRole(t *testing.T, pool *pgxpool.Pool, userID int, roleName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The email is built in Go. Writing `$1 || '@example.com'` makes PostgreSQL
	// deduce both integer and text for the same parameter, and it refuses with
	// SQLSTATE 42P08.
	email := fmt.Sprintf("administration-%d@example.com", userID)
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name)
VALUES ($1, $2, 'Administration caller')
ON CONFLICT (id) DO NOTHING`, userID, email); err != nil {
		t.Fatalf("seed the administration caller: %v", err)
	}

	var roleID int
	if err := pool.QueryRow(ctx,
		`SELECT id FROM public.auth_core__role WHERE name = $1 AND mode = 'administration'`,
		roleName).Scan(&roleID); err != nil {
		t.Fatalf("find the administration %q role: %v.\n"+
			"  001_initial.sql seeds the four administration roles; without them the admin panel "+
			"has no role to grant to at all.", roleName, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.auth_core__user_role (user_id, role_id) VALUES ($1, $2)
ON CONFLICT (user_id, role_id) DO NOTHING`, userID, roleID); err != nil {
		t.Fatalf("assign the administration %q role: %v", roleName, err)
	}
}
