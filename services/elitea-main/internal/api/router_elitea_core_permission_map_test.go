package api_test

// The two source-level invariants behind the /elitea_core per-route permission
// gates (#302, #313). Neither is observable from a running router, and each one
// failing produces a defect nothing else in this suite would notice.
//
//  1. EVERY permission string the gates name has to be one pylon actually
//     declares. The names were transcribed by hand from
//     testdata/legacy/legacy-rbac-static-catalog.json, and a typo — or a name
//     invented because a route had no obvious counterpart — produces a gate
//     that is 403-for-everyone forever, since nothing anywhere grants a string
//     that exists in no catalogue and no matrix.
//
//  2. EVERY permission the gates name has to be GRANTED by the shared migration
//     history. This is the trap #313 was filed for: on a Go-bootstrapped
//     database `legacyrbac`'s central fallback is the only source of
//     default-mode permissions, and before 0068 it carried seven. Gating fifty
//     routes on strings nothing grants "reads as a broken page rather than as a
//     missing grant" (0063's header), which is the failure mode #93 shipped.
//
// Both read the SOURCE — router.go, the catalogue JSON and the migration
// files — because both are properties of the wiring rather than of any
// behaviour a request could exercise. A behavioural test would need a live
// database seeded with the very grants under test, which is exactly the
// assumption being checked.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	applicationskillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applicationskills"
	indextypesapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indextypes"
)

// reviewedRoutePermissions are the /elitea_core gates that do NOT live in
// router.go.
//
// internal/api/production_router.go registers the reviewed routes at their
// exact contract paths, and each of those routes builds its own
// RequireResolvedPermissionsForProject middleware inside its package. The
// regex below reads router.go only, so without this list a reviewed route
// reads as "granted by 0068 and gating nothing".
//
// That is not hypothetical: #394 deleted the prototype index-types mount from
// router.go, so `models.applications.index_types.details` moved entirely into
// internal/api/v2/indextypes. #395 did the same for the attached-skills read.
// The constants are read from the packages that declare them, so a rename
// cannot make this list stale.
var reviewedRoutePermissions = []string{
	indextypesapi.CurrentIndexTypesPermission,
	applicationskillsapi.CurrentApplicationSkillsPermission,
}

// projectPermissionCall matches the router's own gate helper, including the
// toolkit block's `toolkitGate` alias, and captures the literal it is given.
// Constant-valued calls (projectPermission(v2analytics.ViewPermission)) are not
// matched here; they are covered by their own packages' tests and are resolved
// below through constantPermissions.
var projectPermissionCall = regexp.MustCompile(`(?:projectPermission|toolkitGate)\("([^"]+)"\)`)

// constantPermissions are the gates that pass a package constant rather than a
// literal, resolved here so the checks below cover the whole surface. Each is
// asserted against its package's declaration by that package's own tests; the
// value is repeated once, here, so this file can see it.
var constantPermissions = map[string]string{
	"v2analytics.ViewPermission":       "models.monitoring.tracing.view",
	"v2messagetraces.ListPermission":   "models.chat.messages.list",
	"v2messagetraces.DetailPermission": "models.chat.messages.details",
}

func gatedPermissions(t *testing.T) []string {
	t.Helper()

	root := repoRootFrom(t)
	src := readFile(t, filepath.Join(root, "internal/api/router.go"))

	unique := map[string]struct{}{}
	for _, match := range projectPermissionCall.FindAllStringSubmatch(src, -1) {
		unique[match[1]] = struct{}{}
	}
	for expression, permission := range constantPermissions {
		if strings.Contains(src, "projectPermission("+expression+")") {
			unique[permission] = struct{}{}
		}
	}
	for _, permission := range reviewedRoutePermissions {
		unique[permission] = struct{}{}
	}

	if len(unique) < 40 {
		t.Fatalf("router.go names only %d distinct /elitea_core permissions; #313 gated roughly sixty, "+
			"so either the helper was renamed out from under this regex or the gates were removed. "+
			"Do not relax this without replacing the guarantee.", len(unique))
	}

	permissions := make([]string, 0, len(unique))
	for permission := range unique {
		permissions = append(permissions, permission)
	}
	sort.Strings(permissions)
	return permissions
}

// TestEliteaCoreGatesNameOnlyRealLegacyPermissions is invariant 1.
//
// The catalogue is machine-generated from the pylon `check_api` declarations by
// scripts/security/export_legacy_rbac_catalog.py, so "appears in
// literal_permissions" means "some pylon handler declares this exact string".
// A name that does not is either a typo or an invention, and both ship as a
// permanent 403.
func TestEliteaCoreGatesNameOnlyRealLegacyPermissions(t *testing.T) {
	t.Parallel()

	declared := legacyDeclaredPermissions(t)
	for _, permission := range gatedPermissions(t) {
		if _, ok := declared[permission]; !ok {
			t.Errorf("router.go gates an /elitea_core route on %q, which no pylon handler declares.\n"+
				"  testdata/legacy/legacy-rbac-static-catalog.json is generated from the `check_api`\n"+
				"  calls themselves, so a string missing from it is a name this platform has never had.\n"+
				"  Nothing grants it, so the route answers 403 to every caller including the operator.",
				permission)
		}
	}
}

// TestEliteaCoreGatedPermissionsAreSeeded is invariant 2, and it is the one
// #313 exists for: the gating and the seeding have to land together.
//
// It reads the migration FILES rather than a transcription of their contents,
// so removing a permission from 0068 — or editing which roles get it, which
// changes the file and therefore this search — fails here instead of shipping a
// page that answers 403 with no way for an operator to tell why.
func TestEliteaCoreGatedPermissionsAreSeeded(t *testing.T) {
	t.Parallel()

	granted := sharedMigrationGrants(t)
	for _, permission := range gatedPermissions(t) {
		if _, ok := granted[permission]; !ok {
			t.Errorf("router.go gates an /elitea_core route on %q, which no migration in\n"+
				"  migrations/shared grants to any default-mode role.\n"+
				"  On a Go-bootstrapped database legacyrbac's central fallback is the ONLY source of\n"+
				"  default-mode permissions, so this route is 403-for-everyone — which reads as a broken\n"+
				"  page rather than as a missing grant (0063's header, and the shape #93 shipped).\n"+
				"  Add it to shared/0068_elitea_core_route_permissions.sql with the role split the\n"+
				"  legacy matrix gives it.", permission)
		}
	}
}

// TestEliteaCoreSeedingMigrationGrantsNothingUngated is the reverse direction,
// and it is what keeps 0068 from quietly becoming a grab-bag. A permission
// granted here but gating nothing is a widening of what every project member
// can do, bought for no route — the opposite of the parity restoration the file
// claims to be.
func TestEliteaCoreSeedingMigrationGrantsNothingUngated(t *testing.T) {
	t.Parallel()

	gated := map[string]struct{}{}
	for _, permission := range gatedPermissions(t) {
		gated[permission] = struct{}{}
	}

	root := repoRootFrom(t)
	src := stripSQLComments(readFile(t, filepath.Join(root, "migrations/shared/0068_elitea_core_route_permissions.sql")))
	for _, permission := range permissionLiterals(src) {
		if _, ok := gated[permission]; !ok {
			t.Errorf("shared/0068 grants %q to every default-mode project role, but no /elitea_core\n"+
				"  route is gated on it. A grant with no gate widens what a member can do and buys\n"+
				"  nothing; drop it, or gate the route it was added for.", permission)
		}
	}
}

/* ── sources ───────────────────────────────────────────────────────────── */

// legacyDeclaredPermissions reads the machine-generated catalogue's
// literal_permissions list — every string a pylon `check_api` names.
func legacyDeclaredPermissions(t *testing.T) map[string]struct{} {
	t.Helper()

	// The catalogue lives at the REPOSITORY root, not the service root: it is
	// generated for the whole pylon plugin corpus and is read by
	// scripts/security/check_legacy_rbac_drift.py too.
	path := filepath.Join(repoRootFrom(t), "../..", "testdata/legacy/legacy-rbac-static-catalog.json")
	raw, err := os.ReadFile(path) //nolint:gosec // fixed, test-local path
	if err != nil {
		t.Fatalf("read the legacy RBAC catalogue: %v", err)
	}
	var catalog struct {
		LiteralPermissions []string `json:"literal_permissions"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("parse the legacy RBAC catalogue: %v", err)
	}
	if len(catalog.LiteralPermissions) == 0 {
		t.Fatal("the legacy RBAC catalogue lists no literal permissions; this test's premise is gone")
	}

	declared := make(map[string]struct{}, len(catalog.LiteralPermissions))
	for _, permission := range catalog.LiteralPermissions {
		declared[permission] = struct{}{}
	}
	return declared
}

// sharedMigrationGrants collects every permission string any shared migration
// inserts for a DEFAULT-mode role. The mode matters: 0060 and 0061 grant
// administration- and developer-mode strings that a project-scoped gate can
// never resolve, so counting those would let an unreachable gate pass.
func sharedMigrationGrants(t *testing.T) map[string]struct{} {
	t.Helper()

	dir := filepath.Join(repoRootFrom(t), "migrations/shared")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations/shared: %v", err)
	}

	granted := map[string]struct{}{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		// Split on the INSERT keyword rather than on `;`: these files are
		// PL/pgSQL DO blocks whose header comments and guards carry semicolons
		// of their own, so a statement split mis-attributes each INSERT's WHERE
		// clause to the chunk before it — which silently reports every grant as
		// missing.
		for _, block := range insertBlocks(stripSQLComments(readFile(t, filepath.Join(dir, entry.Name())))) {
			// Only INSERTs whose WHERE clause selects the default mode. One
			// file can carry several modes (0060 does), so this is per block.
			if !strings.Contains(block, "mode = 'default'") {
				continue
			}
			for _, permission := range permissionLiterals(block) {
				granted[permission] = struct{}{}
			}
		}
	}
	if len(granted) == 0 {
		t.Fatal("no default-mode grant was found in migrations/shared; either the INSERT shape " +
			"changed or the grants were removed. Do not delete this without replacing the guarantee.")
	}
	return granted
}

// insertBlocks returns one segment per `INSERT INTO …auth_core__role_permission`,
// each running to the next INSERT or to the end of the file, so a block always
// carries its own WHERE clause.
func insertBlocks(sql string) []string {
	const marker = "INSERT INTO public.auth_core__role_permission"
	var blocks []string
	for {
		start := strings.Index(sql, marker)
		if start < 0 {
			return blocks
		}
		sql = sql[start+len(marker):]
		if next := strings.Index(sql, marker); next >= 0 {
			blocks = append(blocks, sql[:next])
			continue
		}
		blocks = append(blocks, sql)
		return blocks
	}
}

// stripSQLComments drops `--` lines so a permission name discussed in prose is
// never mistaken for one the file grants.
func stripSQLComments(sql string) string {
	lines := strings.Split(sql, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// permissionLiterals pulls the dotted permission strings out of SQL, ignoring
// the quoted table names ("public.auth_core__role") that share the shape. A
// permission never carries the `public.` prefix and a table name never carries
// a `models.`/`configuration.` one, so the discriminator is the leading segment
// rather than the punctuation.
//
// DIGITS ARE PART OF A SEGMENT. The character class used to be `[a-z_]`, which
// silently skipped every permission with a digit in it —
// `configuration.artifacts.s3_credentials.view` and `.edit` are the first two
// this corpus grants. A permission the pattern cannot see is a permission the
// grant gate reports as ungranted while the migration beside it grants it, so
// the failure was a false alarm that the only available fix (an allowlist
// entry) would have made permanent.
var sqlStringLiteral = regexp.MustCompile(`'([a-z0-9_]+(?:\.[a-z0-9_]+)+)'`)

func permissionLiterals(sql string) []string {
	var permissions []string
	for _, match := range sqlStringLiteral.FindAllStringSubmatch(sql, -1) {
		literal := match[1]
		if strings.HasPrefix(literal, "public.") || strings.Contains(literal, "auth_core__") {
			continue
		}
		permissions = append(permissions, literal)
	}
	return permissions
}
