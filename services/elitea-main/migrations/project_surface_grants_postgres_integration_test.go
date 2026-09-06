package migrations_test

// The three project-scoped surfaces #496 gates, measured against a CLEAN
// database.
//
//	/api/v2/configurations              22 routes over the per-project credentials
//	/api/v2/webhooks/prompt_lib/{id}     5 routes over a url and a rotating secret
//	/api/v2/events/prompt_lib/{id}       the project SSE bus
//
// # THIS CHANGE ADDS NO MIGRATION, AND THIS FILE IS WHY THAT IS SAFE
//
// A gate on a string no migration grants answers 403 to every caller on a clean
// database — the shape of #354, #359, #386 and #402 — and the usual remedy is a
// new numbered migration. #496 needed none, because every string its gates name
// is already in the corpus:
//
//	configurations.configurations.list      shared/0072
//	configurations.configuration.details    shared/0072
//	configurations.configuration.create     shared/0072
//	configurations.configuration.update     shared/0072
//	configurations.configuration.delete     shared/0072
//	models.project_context.view             shared/0062
//
// "No migration needed" is a claim, and an unmeasured claim of that shape is
// exactly what ships a 403-for-everyone page. So this file states it as a test
// against the corpus a real deployment applies, and against nothing else.
// newMigratedPool CREATEs an empty database, applies 001_initial.sql, then
// applies every shared and tenant migration through the real runner. No seed
// script runs here. Delete shared/0062 or shared/0072 and this file fails and
// names the surface that breaks.
//
// # WHY IT IS SEPARATE FROM THE 0072 FILE
//
// agent_cancel_and_configuration_grant_postgres_integration_test.go measures the
// same five configuration strings, and it must keep doing so: it is 0072's own
// regression test, written for the routes 0072 was added for. This file measures
// something that file cannot express — that the strings reach THESE routes, and
// that the sixth string, the SSE one, is granted at all. A reader who deletes
// one of the two is deleting a claim that the other does not make.
//
// # BOTH DIRECTIONS, PER SURFACE
//
// Only the pair discriminates. A grant that reached nobody and a grant that
// reached everybody both pass a one-sided check, and a gate that refuses every
// caller reads as a working gate. So every role is measured in both directions
// in ONE resolution: the strings it must resolve, and the strings it must not.
//
// Every assertion reads the grant ROWS or the resolver, never a status code, so
// a change to the HTTP surface cannot mask a lost grant. The HTTP directions are
// proved separately, in internal/api/router_project_surface_gates_test.go and in
// each of the three packages.

import (
	"context"
	"slices"
	"testing"
	"time"

	v2budgets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/budgets"
	configurationapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	v2events "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/events"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// The permissions are read from the production constants, not copied, so a
// rename cannot leave this file measuring a string nothing uses.
const (
	surfaceListPermission   = configurationapi.CurrentConfigurationListPermission
	surfaceGetPermission    = configurationapi.CurrentConfigurationGetPermission
	surfaceCreatePermission = configurationapi.CurrentConfigurationCreatePermission
	surfaceUpdatePermission = configurationapi.CurrentConfigurationUpdatePermission
	surfaceDeletePermission = configurationapi.CurrentConfigurationDeletePermission
	surfaceStreamPermission = v2events.StreamPermission
)

// The strings a default-mode VIEWER holds. The legacy matrix gives all three to
// admin, editor and viewer alike.
var projectSurfaceReadStrings = []string{
	surfaceListPermission,
	surfaceGetPermission,
	surfaceStreamPermission,
}

// The strings the legacy matrix withholds from the default-mode viewer.
var projectSurfaceWriteStrings = []string{
	surfaceCreatePermission,
	surfaceUpdatePermission,
	surfaceDeletePermission,
}

/* ── the ledger: what a clean database grants ──────────────────────────── */

// A clean database grants each of the six to exactly the roles the legacy matrix
// records, which is what makes the "no new migration" decision checkable.
func TestCleanDatabaseGrantsEveryStringTheThreeProjectSurfacesGateOn(t *testing.T) {
	pool := newMigratedPool(t)

	for _, permission := range projectSurfaceReadStrings {
		// The configuration listing is also on the machine role's callback
		// surface (shared/0088): the worker reads the model catalogue with the
		// project-system PAT.
		want := defaultModeRoles
		if permission == surfaceListPermission {
			want = withMachineRole(defaultModeRoles...)
		}
		holders := defaultModeGrantHolders(t, pool, permission)
		if !slices.Equal(holders, want) {
			t.Errorf("default-mode holders of %s = %v, want %v.\n"+
				"  #496 gates a route on this string and adds no migration for it. Without the\n"+
				"  grant that route answers 403 to every caller on a clean deployment.",
				permission, holders, want)
		}
	}
	for _, permission := range projectSurfaceWriteStrings {
		holders := defaultModeGrantHolders(t, pool, permission)
		if !slices.Equal(holders, writeGateHolders) {
			t.Errorf("default-mode holders of %s = %v, want %v: the legacy matrix withholds "+
				"this string from the viewer", permission, holders, writeGateHolders)
		}
	}
}

/* ── the entitled direction ────────────────────────────────────────────── */

// An editor of a Go-provisioned project resolves all six, so every gate on the
// three surfaces admits them: the AI-configuration page lists and saves
// credentials, the webhook routes answer, and the SSE stream opens.
func TestAnEditorReachesAllThreeProjectSurfacesOnACleanDatabase(t *testing.T) {
	pool := newMigratedPool(t)
	seedRoleMembership(t, pool, 4961, "editor", 1)

	resolution := resolveDefaultModeFor(t, pool, "1", "1")

	for _, permission := range slices.Concat(projectSurfaceReadStrings, projectSurfaceWriteStrings) {
		if !slices.Contains(resolution.Permissions, permission) {
			t.Errorf("an editor does not resolve %s on a clean database, so the route it gates "+
				"refuses the caller (resolved %v)", permission, resolution.Permissions)
		}
	}
}

/* ── the refused direction, against a REAL role ────────────────────────── */

// A default-mode viewer is the discriminating role, and it is measured in BOTH
// directions in one resolution.
//
// The viewer reads the credential list, reads one credential, and opens the
// project stream. The viewer creates, updates and deletes nothing. A change that
// granted the write strings more widely fails the second half; a change that
// granted nothing fails the first.
func TestAViewerReadsTheThreeSurfacesAndWritesNone(t *testing.T) {
	pool := newMigratedPool(t)
	seedRoleMembership(t, pool, 4962, "viewer", 1)

	resolution := resolveDefaultModeFor(t, pool, "1", "1")

	for _, permission := range projectSurfaceReadStrings {
		if !slices.Contains(resolution.Permissions, permission) {
			t.Errorf("a viewer does not resolve %s; the legacy matrix grants it to the "+
				"default-mode viewer (resolved %v)", permission, resolution.Permissions)
		}
	}
	for _, permission := range projectSurfaceWriteStrings {
		if slices.Contains(resolution.Permissions, permission) {
			t.Errorf("a viewer resolves %s. On the webhook surface that string rotates another "+
				"member's signing secret, and on the configuration surface it replaces a "+
				"credential.", permission)
		}
	}
}

// A caller who is a member of a project that carries its OWN per-project grant
// rows resolves only those rows, and therefore holds none of the six.
//
// This is the shape of every pylon-backed database, every legacy dump and the
// end-to-end stack. It is what proves the two tests above are measuring the
// corpus grants and not something ambient, and it is the refused direction for
// all six at once.
func TestACallerWithoutTheseGrantsReachesNoneOfTheThreeSurfaces(t *testing.T) {
	pool := newMigratedPool(t)
	roleID := seedRoleMembership(t, pool, 4963, "editor", 1)

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
	for _, permission := range slices.Concat(projectSurfaceReadStrings, projectSurfaceWriteStrings) {
		if slices.Contains(resolution.Permissions, permission) {
			t.Errorf("%s resolves for a caller nothing granted it", permission)
		}
	}
}

/* ── the mode, and the string the SSE gate borrows ─────────────────────── */

// Every gate on the three surfaces resolves the DEFAULT mode, which is the mode
// 0062 and 0072 grant in.
//
// This matters most for the `{mode}` twins of the configuration surface. Their
// path segment can say `administration`, and the reference resolves such a URL
// against the caller's CENTRAL roles. No migration in this corpus grants any of
// the five strings in the administration mode, so a gate that read the segment
// would answer 403 to every caller there — and if one were granted, it would let
// an operator who is a member of no project read that project's credentials.
// Neither is wanted, and the gate reads the mode from neither.
//
// This test needs no database, so it runs on every machine.
func TestTheThreeProjectSurfaceGatesResolveTheDefaultMode(t *testing.T) {
	t.Parallel()

	for name, mode := range map[string]string{
		"configuration read":     configurationapi.CurrentConfigurationReadMode,
		"configuration mutation": configurationapi.CurrentConfigurationMutationMode,
		"configuration types":    configurationapi.CurrentConfigurationTypesMode,
		"model catalogue":        configurationapi.CurrentModelCatalogMode,
		"model default":          configurationapi.CurrentModelDefaultMode,
	} {
		if mode != auth.PermissionModeDefault {
			t.Errorf("the %s route resolves the %q mode, but 0072 grants in %q",
				name, mode, auth.PermissionModeDefault)
		}
	}
}

// The SSE stream borrows the budget surface's project-scoped string, and this
// pins the two together.
//
// internal/api/v2/events states the reason: the only publisher a shipped stack
// has on the project channel is the LLM gateway's budget.soft_alert, which
// carries the project's accrued cost, and the REST reads of the same figures are
// gated on v2budgets.ProjectViewPermission. If that surface ever moves to
// another string, the stream would be the softer way to the same data, and this
// test is what says so.
func TestTheProjectStreamSharesTheBudgetReadString(t *testing.T) {
	t.Parallel()

	if surfaceStreamPermission != v2budgets.ProjectViewPermission {
		t.Fatalf("the project event stream gates on %q and the project-scoped budget reads on "+
			"%q. The stream carries the same cost figures, so it must not be the softer way in.",
			surfaceStreamPermission, v2budgets.ProjectViewPermission)
	}
}
