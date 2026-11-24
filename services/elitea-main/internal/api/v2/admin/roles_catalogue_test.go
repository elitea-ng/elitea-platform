package admin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// preRegistryDeclared is the hand-kept declaration declaredPermissions()
// returned before the permission registry (ADR-0059 R0.1a): the five governed
// credential permissions and the `required_permission` of every Configuration
// section. It is a golden list on purpose; do not derive it from code.
var preRegistryDeclared = []string{
	"configuration.branding",
	"configuration.governance",
	"configuration.microsoft_apps",
	"configuration.native_clients",
	"configuration.service_descriptors",
	"entitlements.export",
	"models.credentials.governed.approve",
	"models.credentials.governed.manage",
	"models.credentials.governed.probe",
	"models.credentials.governed.reveal",
	"models.credentials.governed.use",
	"runtime.plugins",
}

// TestRolesCatalogueIsUnchangedForTheExistingNames is the golden test of the
// catalogue `GET /admin/permissions/{scope}/{mode}` lists. catalogueFrom is
// the whole composition of that list from the names the database holds a
// grant for, so it is tested without a database.
//
// On the database the shared migrations seed, the catalogue the registry
// builds is the catalogue the hand-kept list built: same names, same order.
// Every name the old list declared is still declared, so a database where no
// role holds one keeps the row.
func TestRolesCatalogueIsUnchangedForTheExistingNames(t *testing.T) {
	t.Parallel()
	seeded := seededGrants(t)
	if len(seeded) == 0 {
		t.Fatal("found no grant in migrations/shared; the INSERT shape changed")
	}

	before := mergePermissionCatalogue(withoutRetired(seeded), preRegistryDeclared)
	after := catalogueFrom(seeded)
	if !slices.Equal(before, after) {
		t.Errorf("the catalogue on a seeded database changed.\n  added:   %v\n  removed: %v",
			difference(after, before), difference(before, after))
	}

	clean := catalogueFrom(nil)
	if missing := difference(preRegistryDeclared, clean); len(missing) != 0 {
		t.Errorf("a name the hand-kept list declared is no longer declared: %v", missing)
	}
	// The registry adds only administration-mode names that are already
	// granted on a seeded database, so the clean catalogue grows by registry
	// names alone.
	for _, name := range difference(clean, preRegistryDeclared) {
		if _, ok := adminaccess.Lookup(name); !ok {
			t.Errorf("the clean catalogue gained %q, which is not a registry name", name)
		}
	}
	// A seeded_only name is offered only where a role already holds it.
	for _, row := range adminaccess.Registry().Rows() {
		if row.Grantable == adminaccess.GrantableSeededOnly && slices.Contains(clean, row.Name) {
			t.Errorf("the clean catalogue declares the seeded_only %q", row.Name)
		}
	}
}

// TestEveryConfigSectionGateIsARegistryName pins that a section's
// `required_permission` is a row of the permission table, so the catalogue,
// which reads the table, can always grant it.
func TestEveryConfigSectionGateIsARegistryName(t *testing.T) {
	t.Parallel()
	for _, section := range configSections() {
		permission, ok := section["required_permission"].(string)
		if !ok || permission == "" {
			continue
		}
		if _, registered := adminaccess.Lookup(permission); !registered {
			t.Errorf("section %v gates on %q, which internal/adminaccess does not register",
				section["id"], permission)
		}
	}
}

func withoutRetired(names []string) []string {
	kept := make([]string, 0, len(names))
	for _, name := range names {
		if _, retired := retiredPermissions[name]; !retired {
			kept = append(kept, name)
		}
	}
	return kept
}

func difference(a, b []string) []string {
	var out []string
	for _, name := range a {
		if !slices.Contains(b, name) {
			out = append(out, name)
		}
	}
	return out
}

var seededPermissionLiteral = regexp.MustCompile(`'([a-z0-9_]+(?:\.[a-z0-9_]+)+)'`)

// seededGrants is every permission string a shared migration writes into a
// role grant table, central or per project: the source of the catalogue's
// granted half on a clean, migrated database.
func seededGrants(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "..", "migrations", "shared")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations/shared: %v", err)
	}
	seen := map[string]bool{}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(dir, entry.Name())) //nolint:gosec // fixed corpus path
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		var kept []string
		for _, line := range strings.Split(string(source), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				kept = append(kept, line)
			}
		}
		text := strings.Join(kept, "\n")
		for _, marker := range []string{
			"INSERT INTO public.auth_core__role_permission",
			"INSERT INTO public.auth_core__project_role_permission",
		} {
			for _, block := range strings.Split(text, marker)[1:] {
				if end := strings.Index(block, ";"); end >= 0 {
					block = block[:end]
				}
				for _, match := range seededPermissionLiteral.FindAllStringSubmatch(block, -1) {
					name := match[1]
					if strings.HasPrefix(name, "public.") || strings.Contains(name, "auth_core__") || seen[name] {
						continue
					}
					seen[name] = true
					names = append(names, name)
				}
			}
		}
	}
	return names
}

const goldenCatalogueDeprecated = "testdata/roles_catalogue_deprecated.golden"

// TestRolesCatalogueMarksExactlyTheLegacyNamesDeprecated is the golden test of
// the `catalogue` the matrix read serves beside its rows (ADR-0059 R0.14a):
// on a seeded database, and on one where no role holds a legacy name, the
// entries marked `deprecated: true` are exactly runtime.plugins and
// configuration.governance, and every row has its entry. Regenerate with
// UPDATE_GOLDEN=1 only for a change that means to deprecate another name.
func TestRolesCatalogueMarksExactlyTheLegacyNamesDeprecated(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for _, database := range []struct {
		name    string
		granted []string
	}{{"seeded", seededGrants(t)}, {"clean", nil}} {
		matrix := permissionMatrix{catalogue: catalogueFrom(database.granted)}
		encoded, err := json.Marshal(matrix.response())
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Rows      []map[string]any `json:"rows"`
			Catalogue []struct {
				Name       string `json:"name"`
				Deprecated *bool  `json:"deprecated"`
			} `json:"catalogue"`
		}
		if err := json.Unmarshal(encoded, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Catalogue) != len(body.Rows) {
			t.Fatalf("%s: %d catalogue entries for %d rows", database.name, len(body.Catalogue), len(body.Rows))
		}
		fmt.Fprintf(&b, "# %s database\n", database.name)
		for i, entry := range body.Catalogue {
			if entry.Name != body.Rows[i]["name"] || entry.Deprecated == nil {
				t.Fatalf("%s: catalogue entry %d = %+v, want the name of row %v and a deprecated flag",
					database.name, i, entry, body.Rows[i]["name"])
			}
			if *entry.Deprecated {
				fmt.Fprintf(&b, "%s deprecated\n", entry.Name)
			}
		}
	}
	got := b.String()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenCatalogueDeprecated, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenCatalogueDeprecated)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_GOLDEN=1)", goldenCatalogueDeprecated, err)
	}
	if got != string(want) {
		t.Errorf("the deprecated names of the catalogue changed:\n got:\n%s\nwant:\n%s", got, want)
	}
}
