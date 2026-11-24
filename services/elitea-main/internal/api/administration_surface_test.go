package api_test

// The administration surface, found by driving the composed router (ADR-0059
// decision 1, story R0.1b). internal/api/adminsurface states the definition
// and does the driving; this file holds the three tests the story asks for.
//
// The fixtures come first in each test. They prove the helper sees what the
// composed router must be checked for: an in-handler check, a route outside
// /admin, a permission outside the registry and a route it could not reach.
// Without them a helper that recorded nothing would pass every composed
// assertion vacuously.

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/adminsurface"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// declaredWithoutDecision are routes under an administration prefix that make
// no administration decision on purpose: no decision at all, or a
// project-scoped gate in the default mode. Each needs a reason a reader can
// check against the router; an entry added to silence the test hides a route
// from the surface.
var declaredWithoutDecision = map[string]string{
	"GET /api/v2/admin/moderation_status/{mode}/{projectID}/{entityID}": "a project's own moderation " +
		"requests, gated on admin.moderation.view in the DEFAULT mode against {projectID}; the central " +
		"queue is GET /moderation_statuses/administration",
	"POST /api/v2/admin/moderation_status/{mode}/{projectID}/{entityID}": "a project member files a " +
		"moderation request; admin.moderation.create in the DEFAULT mode against {projectID}",
	"DELETE /api/v2/admin/moderation_status/{mode}/{projectID}/{entityID}": "a project member withdraws " +
		"their own request (#544); admin.moderation.create in the DEFAULT mode, rows scoped to the caller",
	"GET /api/v2/admin/system_info/prompt_lib": "the Help Center's version lookup; " +
		"open to every signed-in user like the legacy PromptLibAPI (router.go, /admin block)",
	"GET /api/v2/admin/plugin_config_values/prompt_lib/resources": "the Help Center's one public " +
		"Configuration section; open to every signed-in user (router.go, /admin block)",
	"POST /api/v2/admin/moderation_status/project_request": "self-service project request (#871): " +
		"any signed-in user may ask; the approval is the gated PUT /moderation_status/administration",
	"GET /api/v2/admin/moderation_status/project_requests/mine": "the caller's own project " +
		"requests (#871); no permission decision",
	"GET /api/v2/admin/console/items/{item_id}": "a console item and the roles that grant it, by role " +
		"name (ADR-0059 R3.1a); any signed-in user may read it, so it resolves nobody's permissions",
	"GET /api/v2/projects/project/{mode}/{projectID}": "lists the caller's own projects; " +
		"no permission decision whatever the mode segment says",
	"POST /api/v2/secrets/secrets/{mode}/{projectID}": "bulk replacement of the vault answers 501 " +
		"to every caller before any decision (secrets.notImplementedBulkReplace)",
}

// invisibleChecks are administration-mode checks (adminsurface.InvisibleKey)
// whose outcome no registered name changes in the composed router, each with
// the reason. An entry must say why the check cannot be seen and what holds
// its permission to the registry instead.
var invisibleChecks = map[string]string{
	adminsurface.InvisibleKey(
		adminsurface.Route{Method: http.MethodDelete, Pattern: "/api/v2/admin/scim_group_bindings/administration/{id}"},
		"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess.ResolveCaller",
	): "guardBindingWrite compares the caller's set with the role of the STORED binding, which the " +
		"unconnected pool cannot read, so no grant changes the answer; the rule reads role rows, not a " +
		"literal, and its one literal (admin.auth.users.super_admin) is seen on the SCIM client routes",
}

// The central branch of the guarded project settings (ADR-0059 R0.6b) admits
// only a request that names a guarded setting type: the `type` query of the
// list, the `type` of the create's body. The driver's empty request names
// none, so no grant changes the answer on these four. Its two permissions
// come from adminaccess.OverrideRoutePermissions, and the same check is seen
// on the detail, update and delete routes, where the type is the stored row's.
func init() {
	const settingsOverride = "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware.ResolveCentralPermission"
	for _, route := range []adminsurface.Route{
		{Method: http.MethodGet, Pattern: "/api/v2/configurations/configurations/{projectID}"},
		{Method: http.MethodGet, Pattern: "/api/v2/configurations/configurations/{mode}/{projectID}"},
		{Method: http.MethodPost, Pattern: "/api/v2/configurations/configurations/{projectID}"},
		{Method: http.MethodPost, Pattern: "/api/v2/configurations/configurations/{mode}/{projectID}"},
	} {
		invisibleChecks[adminsurface.InvisibleKey(route, settingsOverride)] = "the central settings branch " +
			"decides on the configuration type the request names, and the driver's empty request names none"
	}
}

type drivenSurface struct {
	result   adminsurface.Result
	sections []adminsurface.Section
}

var composedSurface = sync.OnceValues(func() (drivenSurface, error) {
	composed, err := adminsurface.ComposedRouter()
	if err != nil {
		return drivenSurface{}, err
	}
	sections, err := composed.ConfigurationSections()
	if err != nil {
		return drivenSurface{}, err
	}
	opts, err := composed.Options()
	if err != nil {
		return drivenSurface{}, err
	}
	return drivenSurface{
		result:   adminsurface.Drive(composed.Handler, composed.Resolver, composed.Routes, opts),
		sections: sections,
	}, nil
})

func drivenComposedSurface(t *testing.T) drivenSurface {
	t.Helper()
	driven, err := composedSurface()
	if err != nil {
		t.Fatal(err)
	}
	if driven.result.Driven < 500 {
		t.Fatalf("drove %d (route, variant) pairs; the composed router mounts more than 500 routes", driven.result.Driven)
	}
	return driven
}

// unregisteredFixturePermission is a name internal/adminaccess does not
// register.
const unregisteredFixturePermission = "fixture.not.registered"

// fixtureRouter mounts one route of each shape the helper must see. The
// principal is set by a stand-in for the Auth middleware.
func fixtureRouter(resolver *adminsurface.Resolver) (http.Handler, []adminsurface.Route) {
	central := func(permission string) func(http.Handler) http.Handler {
		return apimw.RequireCentralPermissions(resolver, auth.PermissionModeAdministration, permission)
	}
	inHandler := func(permission string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			user, _ := auth.UserFromContext(r.Context())
			resolution, err := resolver.ResolvePermissions(r.Context(), user, auth.PermissionModeAdministration, "")
			if err != nil || !slices.Contains(resolution.Permissions, permission) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), adminsurface.Principal())))
		})
	})
	// A route gate under /admin, and one built by another gate constructor.
	r.With(central(adminaccess.RuntimePlugins)).Get("/api/v2/admin/gated", ok)
	r.With(apimw.RequireResolvedMembershipPermissions(resolver, auth.PermissionModeAdministration,
		adminaccess.AdminAuthUsers)).Get("/api/v2/admin/membership_gated", ok)
	// An in-handler check on a route outside /admin, behind a gate that is not
	// in the administration mode.
	r.With(apimw.RequireResolvedPermissions(resolver, auth.PermissionModeDefault, "models.fixture.view")).
		Put("/api/v2/elitea_core/budget/{projectID}", inHandler(adminaccess.ConfigurationGovernance))
	// A check, gate and in-handler, that names a permission outside the registry.
	r.With(central(unregisteredFixturePermission)).Get("/api/v2/admin/unregistered_gate", ok)
	r.Get("/api/v2/tracing/unregistered_check", inHandler(unregisteredFixturePermission))
	// A route under /admin that answers before any decision, and one that
	// decides in the default mode only.
	r.Get("/api/v2/admin/unreached", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	r.With(apimw.RequireResolvedPermissions(resolver, auth.PermissionModeDefault, "models.fixture.view")).
		Get("/api/v2/admin/default_gated/{projectID}", ok)

	var routes []adminsurface.Route
	_ = chi.Walk(r, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, adminsurface.Route{Method: method, Pattern: pattern})
		return nil
	})
	return r, routes
}

func driveFixtures(t *testing.T) adminsurface.Result {
	t.Helper()
	resolver := &adminsurface.Resolver{}
	handler, routes := fixtureRouter(resolver)
	return adminsurface.Drive(handler, resolver, routes, adminsurface.Options{
		Registry:   adminaccess.Names(),
		OtherModes: []string{"models.fixture.view"},
	})
}

func TestAdministrationSurfaceNamesOnlyRegisteredPermissions(t *testing.T) {
	t.Parallel()

	t.Run("fixtures", func(t *testing.T) {
		t.Parallel()
		result := driveFixtures(t)

		budget := result.Find(http.MethodPut, "/api/v2/elitea_core/budget/{projectID}")
		if len(budget) != 1 || len(budget[0].Checks) != 1 {
			t.Fatalf("the in-handler check outside /admin: got %+v, want one entry with one check", budget)
		}
		check := budget[0].Checks[0]
		if check.Gate || check.Kind != adminsurface.KindAnyOf ||
			!slices.Equal(check.Permissions, []string{adminaccess.ConfigurationGovernance}) ||
			!strings.HasSuffix(check.Caller, "fixtureRouter") || budget[0].Prefix != "/elitea_core" {
			t.Errorf("the in-handler check outside /admin: got %+v under %q, "+
				"want an in-handler any_of check for %q under /elitea_core",
				check, budget[0].Prefix, adminaccess.ConfigurationGovernance)
		}
		for route, permission := range map[string]string{
			"/api/v2/admin/gated":            adminaccess.RuntimePlugins,
			"/api/v2/admin/membership_gated": adminaccess.AdminAuthUsers,
		} {
			gated := result.Find(http.MethodGet, route)
			if len(gated) != 1 || len(gated[0].Checks) != 1 || !gated[0].Checks[0].Gate ||
				!slices.Equal(gated[0].Checks[0].Permissions, []string{permission}) {
				t.Errorf("the route gate on %s: got %+v, want a gate check for %q", route, gated, permission)
			}
		}

		failures := result.RegistryFailures(adminaccess.Names(), nil)
		for _, route := range []string{"GET /api/v2/admin/unregistered_gate", "GET /api/v2/tracing/unregistered_check"} {
			if !slices.ContainsFunc(failures, func(f string) bool { return strings.HasPrefix(f, route+" ") }) {
				t.Errorf("no registry failure names %s, whose check asks for %q; got %q",
					route, unregisteredFixturePermission, failures)
			}
		}
		if len(failures) != 2 {
			t.Errorf("registry failures: got %q, want exactly the two unregistered checks", failures)
		}
	})

	t.Run("composed router", func(t *testing.T) {
		t.Parallel()
		result := drivenComposedSurface(t).result
		for _, failure := range result.RegistryFailures(adminaccess.Names(), invisibleChecks) {
			t.Error(failure)
		}
	})
}

func TestAdministrationSurfaceSpansEveryMount(t *testing.T) {
	t.Parallel()
	driven := drivenComposedSurface(t)
	result := driven.result

	prefixes := map[string]int{}
	for _, entry := range result.Surface {
		prefixes[entry.Prefix]++
	}
	for _, prefix := range []string{"/admin", "/elitea_core", "/projects", "/secrets", "/tracing"} {
		if prefixes[prefix] == 0 {
			t.Errorf("the surface has no route under %s; got prefixes %v", prefix, prefixes)
		}
	}

	// The three Configuration routes. The schema list and the suggestions are
	// decided by the route gate; the value routes, read and write, also
	// decide in the handler, by the section's own permission.
	for _, route := range []adminsurface.Route{
		{Method: http.MethodGet, Pattern: adminsurface.ConfigurationSchemas},
		{Method: http.MethodGet, Pattern: adminsurface.ConfigurationSuggestions},
	} {
		if len(result.Find(route.Method, route.Pattern)) == 0 {
			t.Errorf("%s is not in the surface", route)
		}
	}
	declaring := 0
	for _, section := range driven.sections {
		if section.RequiredPermission == "" {
			continue
		}
		declaring++
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			var named []string
			for _, entry := range result.Find(method, adminsurface.ConfigurationValues) {
				if entry.Variant != section.Variant().Name {
					continue
				}
				for _, check := range entry.Checks {
					if !check.Gate && strings.HasSuffix(check.Caller, ".hasAdministrationPermission") {
						named = append(named, check.Permissions...)
					}
				}
			}
			if !slices.Equal(named, []string{section.RequiredPermission}) {
				t.Errorf("%s %s, section %s: the in-handler section check named %v; want [%s]",
					method, adminsurface.ConfigurationValues, section.ID, named, section.RequiredPermission)
			}
		}
	}
	// 17 of today's sections declare one; the floor only guards the schema
	// read itself.
	if declaring < 3 {
		t.Errorf("%d Configuration sections declare a required_permission; the schema read found too few", declaring)
	}

	// Project create and delete.
	for _, route := range []adminsurface.Route{
		{Method: http.MethodPost, Pattern: "/api/v2/projects/project/{mode}"},
		{Method: http.MethodDelete, Pattern: "/api/v2/projects/project/{mode}/{projectID}"},
	} {
		if len(result.Find(route.Method, route.Pattern)) == 0 {
			t.Errorf("%s (project create or delete) is not in the surface", route)
		}
	}
}

func TestAdministrationSurfaceFailsOnAnUnreachedRoute(t *testing.T) {
	t.Parallel()

	t.Run("fixtures", func(t *testing.T) {
		t.Parallel()
		failures := driveFixtures(t).UnreachedFailures(nil)
		if len(failures) != 2 ||
			!strings.HasPrefix(failures[0], "GET /api/v2/admin/default_gated/{projectID} ") ||
			!strings.Contains(failures[0], "decided only in the default mode") ||
			!strings.HasPrefix(failures[1], "GET /api/v2/admin/unreached ") ||
			!strings.Contains(failures[1], "made no permission decision") {
			t.Fatalf("got %q, want one failure naming GET /api/v2/admin/default_gated/{projectID} "+
				"(default mode only) and one naming GET /api/v2/admin/unreached (no decision)", failures)
		}
		stale := driveFixtures(t).UnreachedFailures(map[string]string{
			"GET /api/v2/admin/unreached":                 "fixture",
			"GET /api/v2/admin/default_gated/{projectID}": "fixture",
			"GET /api/v2/admin/gated":                     "reached, so the entry is stale",
		})
		if len(stale) != 1 || !strings.HasPrefix(stale[0], "GET /api/v2/admin/gated ") {
			t.Fatalf("got %q, want one failure naming the stale entry GET /api/v2/admin/gated", stale)
		}
	})

	t.Run("composed router", func(t *testing.T) {
		t.Parallel()
		for _, failure := range drivenComposedSurface(t).result.UnreachedFailures(declaredWithoutDecision) {
			t.Error(failure)
		}
	})
}

// actionPermissionExceptions are action entries whose permission the checks
// Drive sees on the route do not name, each with the reason. The entry names
// the permission the action requires; the drive's empty body stops before the
// check that names it.
var actionPermissionExceptions = map[string]string{
	// ADR-0059 R0.6b: the central settings branch reads the type of the
	// create's body, and the drive's empty body names none (invisibleChecks).
	"POST /api/v2/configurations/configurations/{projectID}": "the central settings branch decides " +
		"on the body's configuration type; the empty body names none",
	"POST /api/v2/configurations/configurations/{mode}/{projectID}": "the central settings branch decides " +
		"on the body's configuration type; the empty body names none",
}

// TestEveryWriteRouteOfTheSurfaceHasAnActionEntry is the ratchet of the action
// table (ADR-0059 R0.1c): a write route of the surface with no entry fails, an
// entry whose route the surface lacks fails, and an entry must require a
// permission the route's checks accept.
func TestEveryWriteRouteOfTheSurfaceHasAnActionEntry(t *testing.T) {
	t.Parallel()

	t.Run("fixtures", func(t *testing.T) {
		t.Parallel()
		resolver := &adminsurface.Resolver{}
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), adminsurface.Principal())))
			})
		})
		ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
		gate := apimw.RequireCentralPermissions(resolver, auth.PermissionModeAdministration, adminaccess.RuntimePlugins)
		r.With(gate).Get("/api/v2/admin/fixture", ok)
		r.With(gate).Post("/api/v2/admin/fixture", ok)
		r.With(gate).Put("/api/v2/admin/fixture", ok)
		routes := []adminsurface.Route{
			{Method: http.MethodGet, Pattern: "/api/v2/admin/fixture"},
			{Method: http.MethodPost, Pattern: "/api/v2/admin/fixture"},
			{Method: http.MethodPut, Pattern: "/api/v2/admin/fixture"},
		}
		result := adminsurface.Drive(r, resolver, routes, adminsurface.Options{Registry: adminaccess.Names()})

		table, err := adminaccess.NewActions([]adminaccess.Action{
			// The PUT has an entry, with a permission the gate does not name.
			{Method: http.MethodPut, Pattern: "/api/v2/admin/fixture", Permission: adminaccess.ConfigurationBranding},
			// An entry whose route is not mounted.
			{Method: http.MethodDelete, Pattern: "/api/v2/admin/fixture", Permission: adminaccess.RuntimePlugins},
		}, adminaccess.Registry())
		if err != nil {
			t.Fatal(err)
		}
		failures := result.ActionFailures(table, nil)
		for _, want := range []string{
			`POST /api/v2/admin/fixture (variant "") can change something on the administration surface and has no action entry`,
			`PUT /api/v2/admin/fixture requires "configuration.branding"`,
			"DELETE /api/v2/admin/fixture has an action entry, but the administration surface has no such route",
		} {
			if !slices.ContainsFunc(failures, func(f string) bool { return strings.HasPrefix(f, want) }) {
				t.Errorf("no failure starts with %q; got %q", want, failures)
			}
		}
		if len(failures) != 3 {
			t.Errorf("got %d failures %q; want exactly the three above (the GET is read)", len(failures), failures)
		}
	})

	t.Run("composed router", func(t *testing.T) {
		t.Parallel()
		result := drivenComposedSurface(t).result
		for _, failure := range result.ActionFailures(
			adminaccess.ActionTable(), actionPermissionExceptions) {
			t.Error(failure)
		}
		// The multiplexed user route is driven once per action.
		for _, action := range []string{"action delete", "action set_admin_role"} {
			found := slices.ContainsFunc(result.Find(http.MethodPost, adminaccess.AuthUsersActionPattern),
				func(e adminsurface.Entry) bool { return e.Variant == action })
			if !found {
				t.Errorf("POST %s was not driven with the variant %q", adminaccess.AuthUsersActionPattern, action)
			}
		}
	})
}

// maintenanceBypassNames are the registry rows marked `maintenance`: the names
// the in-application maintenance bypass accepts.
func maintenanceBypassNames() []string {
	var names []string
	for _, row := range adminaccess.Registry().Rows() {
		if row.Maintenance {
			names = append(names, row.Name)
		}
	}
	return names
}

// sessionAndWindowCheck is one check TestAdministrationSurfaceReachesSessionAndWindowChecks
// expects.
type sessionAndWindowCheck struct {
	route   adminsurface.Route
	variant string
	// caller is the suffix of Check.Caller, from the package directory.
	caller      string
	gate        bool
	kind        adminsurface.Kind
	permissions []string
}

// TestAdministrationSurfaceReachesSessionAndWindowChecks holds the checks the
// default request never reaches (ADR-0059 #2925) to the surface: the
// maintenance bypass, which runs only while a window is open; the token mint,
// which asks the administration mode of a native device session only; and the
// Marketplace moderation check, behind a project gate in the default mode.
// Each must appear with the function that asks and what it accepts.
func TestAdministrationSurfaceReachesSessionAndWindowChecks(t *testing.T) {
	t.Parallel()
	result := drivenComposedSurface(t).result

	bypass := maintenanceBypassNames()
	for _, w := range []sessionAndWindowCheck{
		{adminsurface.TokenMint, adminsurface.VariantNativeSession,
			"/internal/api/v2/auth.(*Handler).ownerHoldsAdministration", false, adminsurface.KindWholeSet, nil},
		{adminsurface.MarketplaceListingUpdate, adminsurface.VariantModeration,
			"/internal/api/v2/eliteacore.(*Handler).holdsModerationPermission", false, adminsurface.KindAnyOf,
			[]string{adminaccess.RuntimeAdminPublishedAgents}},
		{adminsurface.PlatformSettings, adminsurface.VariantMaintenance,
			"/internal/api/v2/eliteacore.(*Handler).holdsMaintenanceBypass", false, adminsurface.KindAnyOf, bypass},
		{adminsurface.MaintenanceUserRoute, adminsurface.VariantMaintenance,
			"/internal/api/middleware.(*maintenanceGate).admits", true, adminsurface.KindAnyOf, bypass},
		// On a route an administration gate decides, the maintenance gate
		// admits a holder of any administration permission and the route
		// gate decides (ADR-0059 R0.4b).
		{adminsurface.MaintenanceAdministrationRoute, adminsurface.VariantMaintenance,
			"/internal/api/middleware.(*maintenanceGate).admits", true, adminsurface.KindWholeSet, nil},
		{adminsurface.MaintenanceAdministrationRoute, adminsurface.VariantMaintenance,
			"/internal/api/middleware.RequireResolvedPermissionsForProject", true, adminsurface.KindAnyOf,
			[]string{adminaccess.ModelsAdminProjectBudgetsEdit}},
	} {
		var found []adminsurface.Check
		for _, entry := range result.Find(w.route.Method, w.route.Pattern) {
			if entry.Variant != w.variant {
				continue
			}
			for _, check := range entry.Checks {
				if strings.HasSuffix(check.Caller, w.caller) {
					found = append(found, check)
				}
			}
		}
		if len(found) != 1 {
			t.Errorf("%s (variant %q): %d checks by %s in the surface, want 1", w.route, w.variant, len(found), w.caller)
			continue
		}
		check := found[0]
		if check.Gate != w.gate || check.Kind != w.kind || !slices.Equal(check.Permissions, w.permissions) {
			t.Errorf("%s (variant %q), %s: got gate=%v %s %v, want gate=%v %s %v", w.route, w.variant, w.caller,
				check.Gate, check.Kind, check.Permissions, w.gate, w.kind, w.permissions)
		}
	}

	// A write whose every check reads the whole answer needs no action entry
	// (ActionFailures skips it). The list is pinned so that a new one is a
	// decision, not an accident of the probe.
	var wholeSet []string
	for _, entry := range result.Surface {
		if entry.IsWrite() && entry.NamesNoPermission() {
			wholeSet = append(wholeSet, entry.String()+" "+entry.Variant)
		}
	}
	want := []string{adminsurface.TokenMint.String() + " " + adminsurface.VariantNativeSession}
	if !slices.Equal(wholeSet, want) {
		t.Errorf("writes that name no permission: got %q, want %q", wholeSet, want)
	}
}

// TestTheConsoleCapabilityDocumentNamesNoPermission: GET /admin/console is on
// the composed router and on the administration surface, and its one check
// reads the caller's whole administration set (ADR-0059 R3.1a): it requires
// no permission, every signed-in caller gets an answer, and what it answers
// depends on everything the caller holds.
func TestTheConsoleCapabilityDocumentNamesNoPermission(t *testing.T) {
	t.Parallel()
	entries := drivenComposedSurface(t).result.Find(http.MethodGet, "/api/v2/admin/console")
	if len(entries) != 1 {
		t.Fatalf("GET /api/v2/admin/console: %d surface entries, want 1 (is it mounted?)", len(entries))
	}
	entry := entries[0]
	if !entry.NamesNoPermission() || len(entry.Checks) != 1 || entry.Checks[0].Gate {
		t.Errorf("GET /api/v2/admin/console: got checks %+v, want one in-handler check of the whole set", entry.Checks)
	}
}
