package api_test

// The REVERSE of router_permission_grant_gate_test.go.
//
// That file proves that every gated permission has a grant. This one proves
// that a retired permission (#6874) has no gate, no grant and no web check. A
// retired string is one no code checks, so a row that carries it grants
// nothing and only clutters the admin Roles matrix. Shared migration 0136
// deletes the rows, and the admin catalogue hides the names.
//
// Three places must agree, and each one can drift alone:
//
//   - the dead_permission array in migrations/shared/0136;
//   - admin.RetiredPermissions(), which hides the names in the Roles matrix;
//   - the code: no route gate, no later grant and no web permission constant
//     may name a retired string.
//
// If a retired name gets a real check again, this test fails. The fix is to
// remove the name from BOTH lists, in a new migration if rows must come back.

import (
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	v2admin "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
)

const retiredPermissionsMigration = "migrations/shared/0136_project_settings_permission_and_dead_permissions.sql"

var (
	deadPermissionArray = regexp.MustCompile(`(?s)dead_permission text\[\] := ARRAY\[(.*?)\];`)
	quotedLiteral       = regexp.MustCompile(`'([^']+)'`)
)

// migrationRetiredPermissions reads the dead_permission array from 0136.
func migrationRetiredPermissions(t *testing.T, root string) []string {
	t.Helper()
	source := stripSQLComments(readFile(t, filepath.Join(root, retiredPermissionsMigration)))
	match := deadPermissionArray.FindStringSubmatch(source)
	if match == nil {
		t.Fatalf("%s carries no `dead_permission text[] := ARRAY[...]`; this test's premise is gone",
			retiredPermissionsMigration)
	}
	var names []string
	for _, literal := range quotedLiteral.FindAllStringSubmatch(match[1], -1) {
		names = append(names, literal[1])
	}
	sort.Strings(names)
	return names
}

func TestRetiredPermissionsAgreeBetweenMigrationAndCatalogue(t *testing.T) {
	t.Parallel()

	fromMigration := migrationRetiredPermissions(t, repoRootFrom(t))
	fromCatalogue := v2admin.RetiredPermissions()
	if len(fromMigration) == 0 {
		t.Fatal("0136 retires no permission; the array was emptied or its shape changed")
	}
	if !slices.Equal(fromMigration, fromCatalogue) {
		t.Fatalf("retired lists disagree.\n  0136 deletes: %v\n  catalogue hides: %v\n"+
			"  A name only 0136 knows comes back on the Roles matrix when pylon re-seeds it.\n"+
			"  A name only the catalogue knows keeps its rows forever.",
			fromMigration, fromCatalogue)
	}
}

func TestRetiredPermissionsAreNeitherGatedNorGranted(t *testing.T) {
	t.Parallel()

	root := repoRootFrom(t)
	retired := map[string]bool{}
	for _, name := range migrationRetiredPermissions(t, root) {
		retired[name] = true
	}

	gates := collectGates(t, root)
	if len(gates) == 0 {
		t.Fatal("found no permission gate under internal/; this test would pass for the wrong reason")
	}
	for _, g := range gates {
		for _, permission := range g.permissions {
			if retired[permission] {
				t.Errorf("%s:%d gates a route on %q, which shared/0136 retires as checked by no code.\n"+
					"  Remove the name from the retired lists, or gate the route on a live permission.",
					relativeTo(root, g.position), g.position.Line, permission)
			}
		}
	}

	for mode, permissions := range grantsByMode(t, root) {
		for permission := range permissions {
			if retired[permission] {
				t.Errorf("a shared migration grants the retired permission %q in %q mode", permission, mode)
			}
		}
	}

	// The web app's permission constants are its only gate source.
	web := readFile(t, filepath.Join(root, "../../apps/elitea-web/src/shared/lib/permissions.ts"))
	for name := range retired {
		if strings.Contains(web, "'"+name+"'") {
			t.Errorf("apps/elitea-web permissions.ts names the retired permission %q", name)
		}
	}
}

// The new project settings permission is the opposite case: it is gated, and
// only the default-mode admin gets it. A grant to `editor` would undo #6789.
func TestProjectSettingsPermissionIsGrantedToAdminOnly(t *testing.T) {
	t.Parallel()

	root := repoRootFrom(t)
	const permission = "models.project_settings.edit"

	gated := false
	for _, g := range collectGates(t, root) {
		if slices.Contains(g.permissions, permission) {
			gated = true
		}
	}
	if !gated {
		t.Fatalf("no route gates on %q; the grant in 0136 would widen nothing and buy nothing", permission)
	}

	source := stripSQLComments(readFile(t, filepath.Join(root, retiredPermissionsMigration)))
	for _, block := range insertBlocks(source) {
		if !slices.Contains(permissionLiterals(block), permission) {
			continue
		}
		if !strings.Contains(block, "role.name IN ('admin')") {
			t.Fatalf("0136's central grant of %q is not admin-only:\n%s", permission, block)
		}
	}
	if !strings.Contains(source, "('models.project_settings.edit', ARRAY['admin'])") {
		t.Fatalf("0136's per-project override delivery of %q is not admin-only", permission)
	}
}
