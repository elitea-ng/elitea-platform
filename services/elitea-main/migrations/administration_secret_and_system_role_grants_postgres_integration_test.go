package migrations_test

// Two grant gaps that only a CLEAN database shows, measured against the corpus
// a real deployment applies.
//
// # shared/0087 — the global secret vault answered 403 to everybody
//
// internal/api/v2/secrets/handler.go serves six routes in `administration`
// mode through `adminGate`, which is RequireCentralPermissions(resolver,
// "administration", permission). Nothing granted those strings in that mode:
// 0075 wrote the secrets permissions under `role.mode = 'default'`, and
// `configuration.secrets.secret.view` was granted in no mode at all. The global
// vault is merged into every project's `{{secret.<name>}}` resolution, so a
// platform-wide credential could not be managed by any principal, a
// super_admin included.
//
// # shared/0088 — the scheduled-execution identity resolved nothing
//
// projectprovisioning assigns the per-project system user exactly one project
// role, `system`, and writes no per-project permission row. legacyrbac resolves
// such a role through the CENTRAL default-mode role of the same name, and no
// migration ever created one. So the PAT a scheduled index run mints resolved
// the empty set and every worker callback answered 403 — while the same run
// started by a human succeeded, which makes the failure read as a scheduler
// bug.
//
// # Why these assertions are on the grant rows and the resolver
//
// Not on a status code. A route can move, and the two suites that would
// otherwise cover this are both blind here: apps/elitea-web/scripts/
// e2e-stack.sh seeds the administration secrets permissions BY HAND (its own
// header says the list deliberately includes "permissions no migration
// grants"), and it never creates a `system` project role at all.

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// administrationSecretGrants are shared/0087's four strings with the role names
// testdata/postgres/legacy-rbac-matrix.json gives them in the ADMINISTRATION
// mode.
//
// Only these four are reachable through a gate in that mode. `.list`, `.hide`
// and `.unsecret` gate no administration route, so 0087 does not grant them and
// this table does not expect them.
//
// `system` is dropped, as everywhere else in this corpus: 0060 seeds four
// administration-mode roles — super_admin, admin, editor and viewer — and
// `system` is not one of them.
var administrationSecretGrants = []surfaceGrant{
	// The listing and the single-value read. The matrix withholds both from
	// the administration-mode editor, which is the legacy runtime's write-without-read
	// asymmetry, not a transcription slip.
	{"configuration.secrets.secret.view", []string{"admin", "super_admin"}},
	{"configuration.secrets.secret.create", []string{"admin", "editor", "super_admin"}},
	{"configuration.secrets.secret.edit", []string{"admin", "editor", "super_admin"}},
	{"configuration.secrets.secret.delete", []string{"admin", "editor", "super_admin"}},
}

func TestCleanDatabaseGrantsTheAdministrationSecretPermissions(t *testing.T) {
	pool := newMigratedPool(t)

	for _, grant := range administrationSecretGrants {
		holders := administrationGrantHolders(t, pool, grant.permission)
		want := slices.Clone(grant.holders)
		slices.Sort(want)
		if !slices.Equal(holders, want) {
			t.Errorf("administration-mode holders of %s = %v, want %v.\n"+
				"  Without the grant the global secret vault answers 403 to every principal,\n"+
				"  a super_admin included, and no platform-wide credential can be managed.",
				grant.permission, holders, want)
		}
	}

	// The other direction. The administration-mode viewer holds none of them,
	// and the editor holds no READ. A grant that reached everybody would pass a
	// positive-only check.
	for _, grant := range administrationSecretGrants {
		holders := administrationGrantHolders(t, pool, grant.permission)
		if slices.Contains(holders, "viewer") {
			t.Errorf("the administration-mode viewer holds %s; the matrix gives it nothing here",
				grant.permission)
		}
	}
	if holders := administrationGrantHolders(t, pool, "configuration.secrets.secret.view"); slices.Contains(holders, "editor") {
		t.Errorf("the administration-mode editor holds the global-vault READ: %v", holders)
	}
}

// The project-system identity resolves the callback surface a scheduled run
// makes, in the project it was provisioned for.
//
// The shape below is projectprovisioning's exactly: one auth_core__project_role
// per central default-mode role name plus `system`, the system user assigned
// the `system` role, and NO auth_core__project_role_permission row.
func TestTheProjectSystemIdentityResolvesTheWorkerCallbackPermissions(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := testContext()
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name)
VALUES (8880, 'system_user_1@centry.user', 'System')`); err != nil {
		t.Fatalf("seed the system user: %v", err)
	}
	// createProjectPermissions' own statement, so this test measures what
	// provisioning writes and not a paraphrase of it.
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role (project_id, name)
SELECT $1, role_name
FROM (
    SELECT name AS role_name FROM public.auth_core__role WHERE mode = 'default'
    UNION
    SELECT $2::text
) AS project_roles
ON CONFLICT (project_id, name) DO NOTHING`, 1, "system"); err != nil {
		t.Fatalf("create the project roles: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT 1, 8880, id
FROM public.auth_core__project_role
WHERE project_id = 1 AND name = 'system'`); err != nil {
		t.Fatalf("assign the system project role: %v", err)
	}

	resolution := resolveDefaultModeFor(t, pool, "8880", "1")
	for _, permission := range []string{
		"configuration.artifacts.artifacts.view",
		"configuration.artifacts.artifacts.create",
		"configuration.artifacts.artifacts.edit",
		"configuration.artifacts.artifacts.delete",
		"configuration.secrets.secret.unsecret",
		"configurations.configurations.list",
		"models.applications.applications.list",
		"models.applications.application.details",
		"models.applications.versions.get",
		"models.applications.version.details",
	} {
		if !slices.Contains(resolution.Permissions, permission) {
			t.Errorf("the project-system identity does not resolve %s, so the worker callback "+
				"it gates answers 403 on every scheduled run: %v", permission, resolution.Permissions)
		}
	}

	// The other direction. It is a machine identity, not an operator: it holds
	// the callback surface and nothing else.
	for _, permission := range []string{
		"configuration.secrets.secret.list",
		"configuration.users.users.create",
		"models.applications.application.delete",
	} {
		if slices.Contains(resolution.Permissions, permission) {
			t.Errorf("the project-system identity resolves %s, which no worker callback needs",
				permission)
		}
	}
}

// A human who holds only the machine role gets a SUBSET of the editor, so the
// admin console's "Apply to Projects" sync cannot widen anybody through it.
//
// This is the check shared/0088's header claims and 0083's role-count test
// defers to. If a later file gives `system` a permission the editor does not
// have, that claim stops being true and this test says so.
func TestTheMachineRoleIsASubsetOfTheEditor(t *testing.T) {
	pool := newMigratedPool(t)

	machine := defaultModeGrantsOf(t, pool, machineModeRole)
	editor := defaultModeGrantsOf(t, pool, "editor")
	if len(machine) == 0 {
		t.Fatalf("the machine role holds nothing, so this test discriminates nothing")
	}
	for _, permission := range machine {
		if !slices.Contains(editor, permission) {
			t.Errorf("the machine role holds %s and the default-mode editor does not. "+
				"An operator who assigns a human the `system` project role then widens them.",
				permission)
		}
	}
}

// defaultModeGrantsOf reads every default-mode grant of one role name.
func defaultModeGrantsOf(t *testing.T, pool *pgxpool.Pool, roleName string) []string {
	t.Helper()
	ctx, cancel := testContext()
	defer cancel()

	rows, err := pool.Query(ctx, `
SELECT DISTINCT grant_row.permission
FROM public.auth_core__role_permission AS grant_row
JOIN public.auth_core__role AS role ON role.id = grant_row.role_id
WHERE role.mode = 'default' AND role.name = $1
ORDER BY grant_row.permission`, roleName)
	if err != nil {
		t.Fatalf("read the default-mode grants of %q: %v", roleName, err)
	}
	defer rows.Close()

	permissions := []string{}
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			t.Fatalf("scan a grant row: %v", err)
		}
		permissions = append(permissions, permission)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the grant rows: %v", err)
	}
	return permissions
}
