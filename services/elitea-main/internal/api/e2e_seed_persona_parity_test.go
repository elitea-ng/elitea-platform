package api

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// The end-to-end seed and the built-in administration roles must agree
// (ADR-0059 R3.13a).
//
// The seed creates one persona per built-in role with a declared set, each
// holding exactly that role, so that a persona journey tests what a
// deployment's role holds. Two ways the seed could quietly stop doing that:
//
//   - a persona row names another role, or a declared role has no persona,
//     and the journeys for that role sign in as someone else;
//   - a seed grant block adds a name to a built-in role beyond its declared
//     set, and every persona journey for it passes on a grant no deployment
//     has. That is what the `admin` block did before this change: it carried
//     `admin.auth.users.super_admin` and `configuration.secrets.secret.list`,
//     which now live on the custom role `e2e_admin_extras`.

// seedPersonaRow matches one row of the seed's `e2e_admin_personas` VALUES.
var seedPersonaRow = regexp.MustCompile(`^\s*\('([a-z0-9-]+@autotest\.local)',\s*'[^']+',\s*'([a-z_]+)'\)[,;]?\s*$`)

// seedRoleGrantTarget matches the WHERE line that names the role a VALUES
// grant block writes to.
var seedRoleGrantTarget = regexp.MustCompile(`^WHERE r\.name = '([a-z0-9_]+)' AND r\.mode = 'administration'$`)

func readE2ESeed(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(e2eSeedScript))
	if err != nil {
		t.Fatalf("read %s: %v", e2eSeedScript, err)
	}
	return strings.Split(string(raw), "\n")
}

// seededAdminPersonas returns role → email from the persona VALUES list.
func seededAdminPersonas(t *testing.T, lines []string) map[string][]string {
	t.Helper()
	const marker = "INSERT INTO e2e_admin_personas (email, name, role) VALUES"
	start := slices.IndexFunc(lines, func(line string) bool { return strings.TrimSpace(line) == marker })
	if start < 0 {
		t.Fatalf("%q is not in %s; the persona block changed shape", marker, e2eSeedScript)
	}
	personas := map[string][]string{}
	for _, line := range lines[start+1:] {
		match := seedPersonaRow.FindStringSubmatch(line)
		if match == nil {
			break
		}
		personas[match[2]] = append(personas[match[2]], match[1])
		if strings.HasSuffix(strings.TrimSpace(line), ";") {
			break
		}
	}
	return personas
}

// administrationGrantBlocks returns role → names of every
// `CROSS JOIN (VALUES … ) AS p(permission) WHERE r.name = '<role>' AND
// r.mode = 'administration'` block.
func administrationGrantBlocks(lines []string) map[string][]string {
	blocks := map[string][]string{}
	var current []string
	inBlock := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "CROSS JOIN (VALUES":
			inBlock, current = true, nil
		case inBlock && strings.HasPrefix(trimmed, ") AS p(permission)"):
			inBlock = false
			if i+1 < len(lines) {
				if match := seedRoleGrantTarget.FindStringSubmatch(strings.TrimSpace(lines[i+1])); match != nil {
					blocks[match[1]] = append(blocks[match[1]], current...)
				}
			}
		case inBlock:
			if match := seedGrantValue.FindStringSubmatch(line); match != nil {
				current = append(current, match[1])
			}
		}
	}
	return blocks
}

func TestTheEndToEndSeedHasOnePersonaPerDeclaredBuiltInRole(t *testing.T) {
	t.Parallel()

	personas := seededAdminPersonas(t, readE2ESeed(t))
	declared := 0
	for _, role := range adminaccess.BuiltInRoles() {
		if !role.DeclaresSet() {
			if len(personas[role.Name]) > 0 {
				t.Errorf("the seed has a persona for %q, which declares no set", role.Name)
			}
			continue
		}
		declared++
		if got := personas[role.Name]; len(got) != 1 {
			t.Errorf("the seed has %d personas for built-in role %q (%v); want exactly one", len(got), role.Name, got)
		}
	}
	if len(personas) != declared {
		t.Errorf("the seed has personas for %d roles, want %d (one per declared built-in role): %v",
			len(personas), declared, personas)
	}
}

func TestTheEndToEndSeedWidensNoBuiltInRoleBeyondItsDeclaredSet(t *testing.T) {
	t.Parallel()

	blocks := administrationGrantBlocks(readE2ESeed(t))
	// Premise guard: the `admin` block is the long one; a parser that finds
	// nothing must fail rather than pass.
	if len(blocks[adminaccess.RoleAdmin]) < 20 {
		t.Fatalf("parsed %d names for the `admin` grant block of %s; the seed changed shape",
			len(blocks[adminaccess.RoleAdmin]), e2eSeedScript)
	}
	if len(blocks["e2e_admin_extras"]) != 2 {
		t.Errorf("the e2e_admin_extras block holds %v; want its two names", blocks["e2e_admin_extras"])
	}
	for name, granted := range blocks {
		role, ok := adminaccess.LookupBuiltInRole(name)
		if !ok || !role.DeclaresSet() {
			continue
		}
		set := role.Set()
		for _, permission := range granted {
			if !slices.Contains(set, permission) {
				t.Errorf("the seed grants %q to built-in role %q, outside its declared set. The %s persona "+
					"would then hold a name no deployment's %s holds; put it on a custom role such as "+
					"e2e_admin_extras instead.", permission, name, name, name)
			}
		}
	}
}

// The control: the parser reads a widened block as widened.
func TestTheSeedGrantBlockParserSeesAWidenedBuiltInRole(t *testing.T) {
	t.Parallel()

	lines := []string{
		"CROSS JOIN (VALUES",
		"    ('admin.auth.users'),",
		"    ('admin.auth.users.super_admin')",
		") AS p(permission)",
		"WHERE r.name = 'admin' AND r.mode = 'administration'",
	}
	got := administrationGrantBlocks(lines)[adminaccess.RoleAdmin]
	if !slices.Contains(got, "admin.auth.users.super_admin") {
		t.Fatalf("parsed %v; the parser misses names of an administration grant block", got)
	}
	admin, _ := adminaccess.LookupBuiltInRole(adminaccess.RoleAdmin)
	if slices.Contains(admin.Set(), "admin.auth.users.super_admin") {
		t.Fatal("the control name is in admin's declared set; pick another one")
	}
}
