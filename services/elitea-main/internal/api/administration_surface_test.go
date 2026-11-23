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
// no permission decision on purpose. Each needs a reason a reader can check
// against the router; an entry added to silence the test hides a route from
// the surface.
var declaredWithoutDecision = map[string]string{
	"GET /api/v2/admin/system_info/prompt_lib": "the Help Center's version lookup; " +
		"open to every signed-in user like the legacy PromptLibAPI (router.go, /admin block)",
	"GET /api/v2/admin/plugin_config_values/prompt_lib/resources": "the Help Center's one public " +
		"Configuration section; open to every signed-in user (router.go, /admin block)",
	"POST /api/v2/admin/moderation_status/project_request": "self-service project request (#871): " +
		"any signed-in user may ask; the approval is the gated PUT /moderation_status/administration",
	"GET /api/v2/admin/moderation_status/project_requests/mine": "the caller's own project " +
		"requests (#871); no permission decision",
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
		"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/rolepolicy.ResolveCaller",
	): "guardBindingWrite compares the caller's set with the role of the STORED binding, which the " +
		"unconnected pool cannot read, so no grant changes the answer; the rule reads role rows, not a " +
		"literal, and its one literal (admin.auth.users.super_admin) is seen on the SCIM client routes",
}

var composedSurface = sync.OnceValues(func() (adminsurface.Result, error) {
	composed, err := adminsurface.ComposedRouter()
	if err != nil {
		return adminsurface.Result{}, err
	}
	opts, err := composed.Options()
	if err != nil {
		return adminsurface.Result{}, err
	}
	return adminsurface.Drive(composed.Handler, composed.Resolver, composed.Routes, opts), nil
})

func drivenComposedSurface(t *testing.T) adminsurface.Result {
	t.Helper()
	result, err := composedSurface()
	if err != nil {
		t.Fatal(err)
	}
	if result.Driven < 500 {
		t.Fatalf("drove %d (route, variant) pairs; the composed router mounts more than 500 routes", result.Driven)
	}
	return result
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
	// A route gate under /admin.
	r.With(central(adminaccess.RuntimePlugins)).Get("/api/v2/admin/gated", ok)
	// An in-handler check on a route outside /admin, behind a gate that is not
	// in the administration mode.
	r.With(apimw.RequireResolvedPermissions(resolver, auth.PermissionModeDefault, "models.fixture.view")).
		Put("/api/v2/elitea_core/budget/{projectID}", inHandler(adminaccess.ConfigurationGovernance))
	// A check, gate and in-handler, that names a permission outside the registry.
	r.With(central(unregisteredFixturePermission)).Get("/api/v2/admin/unregistered_gate", ok)
	r.Get("/api/v2/tracing/unregistered_check", inHandler(unregisteredFixturePermission))
	// A route under /admin that answers before any decision.
	r.Get("/api/v2/admin/unreached", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

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
		gated := result.Find(http.MethodGet, "/api/v2/admin/gated")
		if len(gated) != 1 || len(gated[0].Checks) != 1 || !gated[0].Checks[0].Gate ||
			!slices.Equal(gated[0].Checks[0].Permissions, []string{adminaccess.RuntimePlugins}) {
			t.Errorf("the route gate under /admin: got %+v, want a gate check for %q", gated, adminaccess.RuntimePlugins)
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
		result := drivenComposedSurface(t)
		for _, failure := range result.RegistryFailures(adminaccess.Names(), invisibleChecks) {
			t.Error(failure)
		}
	})
}

func TestAdministrationSurfaceSpansEveryMount(t *testing.T) {
	t.Parallel()
	result := drivenComposedSurface(t)

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
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		sectionPermissions := map[string]bool{}
		for _, entry := range result.Find(method, adminsurface.ConfigurationValues) {
			for _, check := range entry.Checks {
				if !check.Gate && strings.HasSuffix(check.Caller, ".hasAdministrationPermission") {
					for _, name := range check.Permissions {
						sectionPermissions[name] = true
					}
				}
			}
		}
		// Today's sections declare configuration.governance, .branding,
		// .native_clients, .service_descriptors, .microsoft_apps and
		// runtime.plugins.
		if len(sectionPermissions) < 3 {
			t.Errorf("%s %s: the in-handler section check named %v; want the permissions of at least three sections",
				method, adminsurface.ConfigurationValues, sectionPermissions)
		}
		for _, name := range []string{adminaccess.ConfigurationGovernance, adminaccess.ConfigurationBranding} {
			if !sectionPermissions[name] {
				t.Errorf("%s %s: no in-handler section check names %q", method, adminsurface.ConfigurationValues, name)
			}
		}
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
		if len(failures) != 1 || !strings.HasPrefix(failures[0], "GET /api/v2/admin/unreached ") {
			t.Fatalf("got %q, want one failure naming GET /api/v2/admin/unreached", failures)
		}
		stale := driveFixtures(t).UnreachedFailures(map[string]string{
			"GET /api/v2/admin/unreached": "fixture",
			"GET /api/v2/admin/gated":     "reached, so the entry is stale",
		})
		if len(stale) != 1 || !strings.HasPrefix(stale[0], "GET /api/v2/admin/gated ") {
			t.Fatalf("got %q, want one failure naming the stale entry GET /api/v2/admin/gated", stale)
		}
	})

	t.Run("composed router", func(t *testing.T) {
		t.Parallel()
		for _, failure := range drivenComposedSurface(t).UnreachedFailures(declaredWithoutDecision) {
			t.Error(failure)
		}
	})
}
