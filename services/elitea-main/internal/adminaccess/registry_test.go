package adminaccess_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

func valid(name string) adminaccess.Permission {
	return adminaccess.Permission{
		Name: name, Area: adminaccess.AreaPlatform, Class: adminaccess.ClassRead,
		Grantable: adminaccess.GrantableAny,
	}
}

// TestRegistryBuilds pins that the table of this service passes its own
// constructor; a bad row would otherwise surface only as an init panic.
func TestRegistryBuilds(t *testing.T) {
	t.Parallel()
	table := adminaccess.Registry()
	if len(table.Rows()) == 0 {
		t.Fatal("the permission table is empty")
	}
	if _, err := adminaccess.NewTable(table.Rows()); err != nil {
		t.Fatalf("the registry does not pass NewTable: %v", err)
	}
	for _, row := range table.Rows() {
		if len(row.Name) > adminaccess.MaxNameLength {
			t.Errorf("%q is longer than %d characters", row.Name, adminaccess.MaxNameLength)
		}
		if row.Area == "" || row.Class == "" {
			t.Errorf("%q has no area or no class", row.Name)
		}
	}
}

func TestNewTableRefusesABadRow(t *testing.T) {
	t.Parallel()
	noArea := valid("a.no_area")
	noArea.Area = ""
	noClass := valid("a.no_class")
	noClass.Class = ""
	unknownArea := valid("a.unknown_area")
	unknownArea.Area = "billing"
	noGrantable := valid("a.no_grantable")
	noGrantable.Grantable = ""
	narrowsMissing := valid("a.narrow")
	narrowsMissing.Narrows = []string{"a.absent"}
	narrowsItself := valid("a.self")
	narrowsItself.Narrows = []string{"a.self"}

	cases := map[string][]adminaccess.Permission{
		"a name longer than 64 characters": {valid("a." + strings.Repeat("x", adminaccess.MaxNameLength-1))},
		"a name that appears twice":        {valid("a.twice"), valid("a.twice")},
		"a row with no area":               {noArea},
		"a row with no class":              {noClass},
		"a row with an unknown area":       {unknownArea},
		"a row with no grantable value":    {noGrantable},
		"a row that narrows a missing row": {narrowsMissing},
		"a row that narrows itself":        {narrowsItself},
		"a row with no name":               {valid("")},
	}
	for name, rows := range cases {
		if _, err := adminaccess.NewTable(rows); err == nil {
			t.Errorf("NewTable accepted %s", name)
		}
	}

	exactly64 := valid("a." + strings.Repeat("x", adminaccess.MaxNameLength-2))
	if _, err := adminaccess.NewTable([]adminaccess.Permission{exactly64}); err != nil {
		t.Errorf("NewTable refused a 64-character name: %v", err)
	}
}

// TestTableIsACopy pins that a caller cannot write the registry through the
// slice Rows returns.
func TestTableIsACopy(t *testing.T) {
	t.Parallel()
	rows := adminaccess.Registry().Rows()
	first := rows[0].Name
	rows[0].Name = "mutated"
	if got := adminaccess.Registry().Rows()[0].Name; got != first {
		t.Fatalf("Rows() exposed the registry: first row is now %q", got)
	}
	for _, row := range rows {
		if len(row.Narrows) == 0 {
			continue
		}
		row.Narrows[0] = "mutated"
		if got, _ := adminaccess.Lookup(row.Name); got.Narrows[0] == "mutated" {
			t.Fatalf("Rows() exposed the Narrows of %q", row.Name)
		}
		break
	}
}

func TestSunsetConstantsAreUnset(t *testing.T) {
	t.Parallel()
	// Story R3.15 sets both in one commit and replaces this test with one
	// for the dates it sets; until then both stay empty, and
	// scripts/ci/check-sunset.mjs (R0.14b) reads them as unset.
	constants := map[string]string{
		"ReplacementShippedOn": adminaccess.ReplacementShippedOn,
		"SunsetAfter":          adminaccess.SunsetAfter,
	}
	for name, value := range constants {
		if value != "" {
			t.Errorf("%s = %q, want empty until R3.15", name, value)
		}
	}
}

// TestAttributesOfTheNamedRows pins the attributes this story sets, so a later
// story changes them on purpose.
func TestAttributesOfTheNamedRows(t *testing.T) {
	t.Parallel()
	var seededOnly, nativeClient, maintenance, token, legacy []string
	for _, row := range adminaccess.Registry().Rows() {
		if row.Grantable == adminaccess.GrantableSeededOnly {
			seededOnly = append(seededOnly, row.Name)
		}
		if row.NativeClient {
			nativeClient = append(nativeClient, row.Name)
		}
		if row.Maintenance {
			maintenance = append(maintenance, row.Name)
		}
		if row.Token {
			token = append(token, row.Name)
		}
		if row.Legacy {
			legacy = append(legacy, row.Name)
		}
	}
	want := func(what string, got []string, names ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(names, ",") {
			t.Errorf("%s = %v, want %v", what, got, names)
		}
	}
	// The global administrator's own permission, identity trust (R0.2,
	// super_admin alone) and the four break-glass names (R0.2).
	want("seeded_only", seededOnly,
		adminaccess.AccessBreakglassApprove, adminaccess.AccessBreakglassManage,
		adminaccess.AccessBreakglassRequest, adminaccess.AccessBreakglassReview,
		adminaccess.AdminAuthUsersSuperAdmin, adminaccess.IdentitySSOTrustManage)
	// The two in-application checks a native session keeps (ADR-0059
	// decision 7): Marketplace moderation and the maintenance bypass, which
	// accepts platform.maintenance.manage beside runtime.plugins (R0.4b).
	want("native_client", nativeClient, adminaccess.PlatformMaintenanceManage,
		adminaccess.RuntimeAdminPublishedAgents, adminaccess.RuntimePlugins)
	want("maintenance", maintenance, adminaccess.PlatformMaintenanceManage, adminaccess.RuntimePlugins)
	// Every row is a token scope (R1.3a) except a CLOSED list: the seeded-only
	// names (identity trust, the global administrator's own permission,
	// break-glass) and the two legacy names. The list is written out rather
	// than derived from the rule, so widening the rule, or a new seeded-only
	// or legacy row, fails here until the list is reviewed.
	never := []string{
		adminaccess.AdminAuthUsersSuperAdmin, adminaccess.IdentitySSOTrustManage,
		adminaccess.AccessBreakglassRequest, adminaccess.AccessBreakglassApprove,
		adminaccess.AccessBreakglassReview, adminaccess.AccessBreakglassManage,
		adminaccess.RuntimePlugins, adminaccess.ConfigurationGovernance,
	}
	var eligible []string
	for _, row := range adminaccess.Registry().Rows() {
		if !slices.Contains(never, row.Name) {
			eligible = append(eligible, row.Name)
		}
	}
	want("token", token, eligible...)
	for _, name := range never {
		if row, _ := adminaccess.Lookup(name); row.Token {
			t.Errorf("%s is a token scope; it must say token: never", name)
		}
	}
	if len(token) < 50 {
		t.Errorf("only %d token scopes: the opt-in lost rows", len(token))
	}
	// The two overloaded names of ADR-0059 decision 2, which end on the
	// sunset date (R0.14a). R4.7 removes both rows.
	want("legacy", legacy, adminaccess.ConfigurationGovernance, adminaccess.RuntimePlugins)
	if got := adminaccess.LegacyNames(); strings.Join(got, ",") != strings.Join(legacy, ",") {
		t.Errorf("LegacyNames() = %v, want %v", got, legacy)
	}
}

// TestCheckDeprecatedRefusesOnlyANewLegacyGrant pins the refusal of R0.14a:
// a legacy name among the new cells is a 400 permission_deprecated naming
// every legacy name and role, and a write with none passes.
func TestCheckDeprecatedRefusesOnlyANewLegacyGrant(t *testing.T) {
	t.Parallel()
	if refusal := adminaccess.CheckDeprecated([]adminaccess.Cell{
		{Role: "custom", Permission: adminaccess.LLMPolicyManage},
	}); refusal != nil {
		t.Fatalf("a narrow grant was refused: %v", refusal)
	}
	if refusal := adminaccess.CheckDeprecated(nil); refusal != nil {
		t.Fatalf("an empty write was refused: %v", refusal)
	}
	refusal := adminaccess.CheckDeprecated([]adminaccess.Cell{
		{Role: "zeta", Permission: adminaccess.RuntimePlugins},
		{Role: "alpha", Permission: adminaccess.ConfigurationGovernance},
		{Role: "alpha", Permission: adminaccess.LLMUsageView},
	})
	if refusal == nil {
		t.Fatal("a new legacy grant was not refused")
	}
	if refusal.Code != adminaccess.CodePermissionDeprecated || refusal.HTTPStatus() != 400 {
		t.Errorf("refusal = %s %d, want %s 400", refusal.Code, refusal.HTTPStatus(), adminaccess.CodePermissionDeprecated)
	}
	if strings.Join(refusal.Permissions, ",") != "configuration.governance,runtime.plugins" ||
		strings.Join(refusal.Roles, ",") != "alpha,zeta" || refusal.Role != "alpha" {
		t.Errorf("refusal names %v on %v (%s)", refusal.Permissions, refusal.Roles, refusal.Role)
	}
}

// TestSeededOnlyPermissionsAreSeeded fails when a permission is marked
// seeded_only and no shared migration grants it to an administration role.
// Such a name could then be held by nobody: the Roles matrix does not grant
// it, by definition.
//
// It reads the grants the way TestEveryGatedPermissionHasAGrant does
// (internal/api/router_permission_grant_gate_test.go): every
// `INSERT INTO public.auth_core__role_permission` statement of
// migrations/shared, comment lines removed, whose text names the
// `administration` mode. Unlike that test it cuts each block at the end of
// its statement, so a name a later statement mentions is not a grant.
func TestSeededOnlyPermissionsAreSeeded(t *testing.T) {
	t.Parallel()
	granted := administrationGrants(t)
	if len(granted) == 0 {
		t.Fatal("found no administration-mode grant in migrations/shared; the INSERT shape changed")
	}
	checked := 0
	for _, row := range adminaccess.Registry().Rows() {
		if row.Grantable != adminaccess.GrantableSeededOnly {
			continue
		}
		checked++
		if !granted[row.Name] {
			t.Errorf("%q is seeded_only, but no migration in migrations/shared grants it to an "+
				"administration role. Nobody can hold it. Grant it in a NEW shared migration.", row.Name)
		}
	}
	if checked == 0 {
		t.Fatal("no row is seeded_only; this test checks nothing")
	}
}

var sqlPermissionLiteral = regexp.MustCompile(`'([a-z0-9_]+(?:\.[a-z0-9_]+)+)'`)

func administrationGrants(t *testing.T) map[string]bool {
	t.Helper()
	dir := filepath.Join("..", "..", "migrations", "shared")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations/shared: %v", err)
	}
	const marker = "INSERT INTO public.auth_core__role_permission"
	granted := map[string]bool{}
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
		blocks := strings.Split(strings.Join(kept, "\n"), marker)
		for _, block := range blocks[1:] {
			// One statement only: a later DELETE or UPDATE that names the
			// permission, or a later mode, is not this grant.
			if end := strings.Index(block, ";"); end >= 0 {
				block = block[:end]
			}
			if !strings.Contains(block, "mode = 'administration'") {
				continue
			}
			for _, match := range sqlPermissionLiteral.FindAllStringSubmatch(block, -1) {
				name := match[1]
				if strings.HasPrefix(name, "public.") || strings.Contains(name, "auth_core__") {
					continue
				}
				granted[name] = true
			}
		}
	}
	return granted
}
