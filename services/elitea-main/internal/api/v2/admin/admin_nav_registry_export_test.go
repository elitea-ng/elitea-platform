package admin

// The web admin navigation gates each item on permission names (ADR-0059
// R0.16, apps/elitea-web/src/pages/admin/adminNavGroups.ts). The names must be
// rows of the permission table, and the Configuration item must name what the
// three Configuration routes admit. This test writes both lists into a
// checked-in JSON beside the navigation, and adminNavGroups.test.ts reads it,
// so a drift fails on both sides: here when the registry, the sections or a
// built-in role's set change without the file, there when an item names a
// string the file lacks.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// adminNavRegistryFile is the JSON, relative to this package.
const adminNavRegistryFile = "../../../../../../apps/elitea-web/src/pages/admin/adminPermissionRegistry.json"

type adminNavRegistry struct {
	GeneratedBy string `json:"generated_by"`
	// Names is every row of the permission table.
	Names []string `json:"names"`
	// ConfigurationRoutePermissions is what the Configuration routes admit:
	// runtime.plugins and every narrow name a section declares.
	ConfigurationRoutePermissions []string `json:"configuration_route_permissions"`
	// DeclaredRoleSets is the set the registry declares for each built-in
	// role that has one, so a navigation test can sign in as a role without
	// copying its set.
	DeclaredRoleSets map[string][]string `json:"declared_role_sets"`
}

func TestAdminNavRegistryFileMatchesTheRegistry(t *testing.T) {
	t.Parallel()
	configuration := configurationRoutePermissions()
	slices.Sort(configuration)
	names := adminaccess.Names()
	slices.Sort(names)
	roleSets := map[string][]string{}
	for _, role := range adminaccess.BuiltInRoles() {
		if role.DeclaresSet() {
			roleSets[role.Name] = role.Set()
		}
	}
	want := adminNavRegistry{
		GeneratedBy:                   "UPDATE_GOLDEN=1 go test ./internal/api/v2/admin/ -run TestAdminNavRegistryFileMatchesTheRegistry",
		Names:                         names,
		ConfigurationRoutePermissions: configuration,
		DeclaredRoleSets:              roleSets,
	}
	encoded, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	encoded = append(encoded, '\n')
	path := filepath.FromSlash(adminNavRegistryFile)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_GOLDEN=1)", path, err)
	}
	if !bytes.Equal(got, encoded) {
		t.Errorf("%s drifted from the permission table or the Configuration sections; "+
			"regenerate with UPDATE_GOLDEN=1 and review the admin navigation's anyPermission lists", path)
	}
}
