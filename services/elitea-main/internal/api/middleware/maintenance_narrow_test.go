package middleware_test

// The maintenance gate by the permission of the route (ADR-0059 R0.4b, #2583),
// on the composed production router of internal/api/adminsurface. Its pool
// reaches no database, so the window is opened per request through
// RouterConfig.MaintenanceState (adminsurface.Variant.Maintenance), and every
// administration-mode resolution answers the set a test grants
// (adminsurface.Send).

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/adminsurface"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
)

var composedRouter = sync.OnceValues(adminsurface.ComposedRouter)

func composed(t *testing.T) adminsurface.Composed {
	t.Helper()
	c, err := composedRouter()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// request is one request of a holder of a set, with the window open or not.
type request struct {
	route  adminsurface.Route
	params map[string]string
	body   string
	// others are the grants in every mode but administration.
	others []string
}

func (q request) send(t *testing.T, window bool, grant ...string) adminsurface.Answer {
	t.Helper()
	c := composed(t)
	opts, err := c.Options()
	if err != nil {
		t.Fatal(err)
	}
	variant := adminsurface.Variant{Body: q.body, Params: q.params, OtherModes: q.others, Maintenance: window}
	if variant.OtherModes == nil {
		variant.OtherModes = []string{}
	}
	return adminsurface.Send(c.Handler, q.route, variant, opts, grant)
}

// llmManager is the permission list of the llm_manager role (ADR-0059
// decision 3). The role itself is R0.7; only its set matters here.
var llmManager = []string{
	adminaccess.LLMCatalogueView, adminaccess.LLMCatalogueManage,
	adminaccess.LLMPolicyView, adminaccess.LLMPolicyManage,
	adminaccess.LLMUsageView, adminaccess.LLMAlertsManage,
	adminaccess.ModelsAdminProjectBudgetsView, adminaccess.ModelsAdminProjectBudgetsEdit,
	adminaccess.ProjectsProjectsView,
}

var (
	budgetSave = request{route: adminsurface.MaintenanceAdministrationRoute, body: `{"budget_limit":10}`}
	// userRoute is outside the administration surface: the agent list of a
	// project, behind its default-mode gate.
	userRoute = request{route: adminsurface.MaintenanceUserRoute,
		others: []string{"models.applications.applications.list"}}
	maintenanceRead = request{route: adminsurface.Route{Method: http.MethodGet,
		Pattern: "/api/v2/admin/maintenance/{mode}"}}
	maintenanceClear = request{
		route:  adminsurface.Route{Method: http.MethodPut, Pattern: adminaccess.ConfigurationValuesPattern},
		params: map[string]string{"plugin": "maintenance"},
		body:   `{"values":{"maintenance_enabled":false}}`,
	}
)

// sameAnswer reports whether a request answered the same with the window open
// as with it closed: the status and the resolutions it made. Bodies are not
// compared, since a 500 carries a per-request support reference.
func sameAnswer(open, closed adminsurface.Answer) bool {
	return open.Status == closed.Status && slices.Equal(adminOnly(open.Calls), adminOnly(closed.Calls))
}

// adminOnly drops the maintenance gate's own resolution, which only the
// request with the window open makes.
func adminOnly(calls []adminsurface.Call) []adminsurface.Call {
	return slices.DeleteFunc(slices.Clone(calls), func(c adminsurface.Call) bool {
		return strings.HasSuffix(c.Caller, "/internal/api/middleware.(*maintenanceGate).admits")
	})
}

func TestMaintenanceAdmitsAnLLMManagerToABudgetUnderEliteaCore(t *testing.T) {
	t.Parallel()
	open := budgetSave.send(t, true, llmManager...)
	closed := budgetSave.send(t, false, llmManager...)
	if open.Maintenance() || open.Status == http.StatusForbidden || !sameAnswer(open, closed) {
		t.Errorf("an llm_manager saving a project budget during a window: got %d %s, want the answer "+
			"outside a window (%d %s)", open.Status, open.Body, closed.Status, closed.Body)
	}
	// The budget handler ran: its gate admitted the request.
	if !slices.ContainsFunc(open.Calls, func(c adminsurface.Call) bool {
		return strings.HasSuffix(c.Caller, "/internal/api/middleware.RequireResolvedPermissionsForProject")
	}) {
		t.Errorf("the budget route gate did not decide the request; calls %+v", open.Calls)
	}

	if none := budgetSave.send(t, true); !none.Maintenance() {
		t.Errorf("a caller with no administration permission saving a budget during a window: got %d %s, "+
			"want the maintenance answer", none.Status, none.Body)
	}
}

func TestMaintenanceManageReadsAndClearsTheWindowAndUsesTheApplication(t *testing.T) {
	t.Parallel()
	read := maintenanceRead.send(t, true, adminaccess.PlatformMaintenanceManage)
	if read.Status != http.StatusOK || !strings.Contains(read.Body, `"enabled":true`) {
		t.Errorf("GET /admin/maintenance during a window, holder of %s: got %d %s, want 200 with enabled true",
			adminaccess.PlatformMaintenanceManage, read.Status, read.Body)
	}
	// The Maintenance section's save. The composed router's pool reaches no
	// database, so the save itself fails; the request must reach it, as it
	// does outside a window.
	clearOpen := maintenanceClear.send(t, true, adminaccess.PlatformMaintenanceManage)
	clearClosed := maintenanceClear.send(t, false, adminaccess.PlatformMaintenanceManage)
	if clearOpen.Maintenance() || clearOpen.Status == http.StatusForbidden || !sameAnswer(clearOpen, clearClosed) {
		t.Errorf("clearing the window, holder of %s: got %d %s, want the answer outside a window (%d %s)",
			adminaccess.PlatformMaintenanceManage, clearOpen.Status, clearOpen.Body, clearClosed.Status, clearClosed.Body)
	}
	use := userRoute.send(t, true, adminaccess.PlatformMaintenanceManage)
	if use.Maintenance() || !sameAnswer(use, userRoute.send(t, false, adminaccess.PlatformMaintenanceManage)) {
		t.Errorf("a route outside the surface during a window, holder of %s: got %d %s, want its answer outside a window",
			adminaccess.PlatformMaintenanceManage, use.Status, use.Body)
	}

	if narrow := userRoute.send(t, true, adminaccess.LLMUsageView); !narrow.Maintenance() {
		t.Errorf("a route outside the surface during a window, holder of only %s: got %d %s, want the maintenance answer",
			adminaccess.LLMUsageView, narrow.Status, narrow.Body)
	}
}

// TestDuringAWindowANarrowHolderReachesNoRouteOutsideTheSurface drives every
// route of the composed router with a window open, once per administration
// permission that is not a maintenance bypass, as a holder of that permission
// alone. A route outside the administration surface (what Drive finds with
// the window closed) must answer the maintenance refusal, unless the window
// leaves its path open. A route of the surface on a path the window does not
// leave open must not, for a holder of a permission its gate accepts: that
// holder gets what the route gate gives.
func TestDuringAWindowANarrowHolderReachesNoRouteOutsideTheSurface(t *testing.T) {
	t.Parallel()
	c := composed(t)
	opts, err := c.Options()
	if err != nil {
		t.Fatal(err)
	}
	inSurface := map[adminsurface.Route]adminsurface.Entry{}
	closed := opts
	closed.Variants = map[string][]adminsurface.Variant{}
	for route, variants := range opts.Variants {
		for _, v := range variants {
			if !v.Maintenance {
				closed.Variants[route] = append(closed.Variants[route], v)
			}
		}
	}
	for _, entry := range adminsurface.Drive(c.Handler, c.Resolver, c.Routes, closed).Surface {
		inSurface[entry.Route] = entry
	}
	var narrow []string
	for _, row := range adminaccess.Registry().Rows() {
		if !row.Maintenance {
			narrow = append(narrow, row.Name)
		}
	}

	window := adminsurface.Variant{Maintenance: true, OtherModes: []string{}}
	api, closedByWindow, outside, reachable := 0, 0, 0, 0
	visited := map[adminsurface.Route]bool{}
	for _, route := range c.Routes {
		path := adminsurface.RequestPath(route.Pattern, nil)
		if !strings.HasPrefix(path, "/api/v2/") || apimw.MaintenanceExempt(path) {
			continue
		}
		api++
		// A route the window does not close to a caller with no
		// administration permission is not behind the gate at all (the
		// branding pack, a shared chat link): no permission changes it.
		if !adminsurface.Send(c.Handler, route, window, opts, nil).Maintenance() {
			continue
		}
		closedByWindow++
		entry, ok := inSurface[route]
		visited[route] = ok
		if !ok {
			outside++
			for _, name := range narrow {
				if got := adminsurface.Send(c.Handler, route, window, opts, []string{name}); !got.Maintenance() {
					t.Errorf("%s during a window, holder of only %s: got %d %s, want the maintenance answer "+
						"(the route is outside the administration surface)", route, name, got.Status, firstLine(got.Body))
				}
			}
			continue
		}
		var accepted []string
		for _, check := range entry.Checks {
			accepted = append(accepted, check.Permissions...)
		}
		for _, name := range accepted {
			if name == adminaccess.RuntimePlugins {
				continue
			}
			reachable++
			if got := adminsurface.Send(c.Handler, route, window, opts, []string{name}); got.Maintenance() {
				t.Errorf("%s during a window, holder of only %s, which its check accepts: got the maintenance answer",
					route, name)
			}
		}
	}
	// A route that picks its gate from {mode} (the secrets routes) keeps a
	// narrow holder in the administration mode only: the project vault of
	// the same pattern is outside the surface.
	vault := adminsurface.Route{Method: http.MethodGet, Pattern: "/api/v2/secrets/secrets/{mode}/{projectID}"}
	for mode, refused := range map[string]bool{"administration": false, "default": true} {
		q := request{route: vault, params: map[string]string{"mode": mode}}
		if got := q.send(t, true, adminaccess.ConfigurationSecretsSecretView); got.Maintenance() != refused {
			t.Errorf("GET %s in mode %s during a window, holder of only %s: maintenance answer %v, want %v",
				vault.Pattern, mode, adminaccess.ConfigurationSecretsSecretView, got.Maintenance(), refused)
		}
	}
	// The counts come from the router and the surface, not from a number
	// written here: every surface route the window closes was checked with
	// each permission its check accepts, and the window closes nearly every
	// route under /api/v2 (the gate sits in front of the router, so a route
	// it does not close is one of the few public ones).
	for route, entry := range inSurface {
		path := adminsurface.RequestPath(route.Pattern, nil)
		if !strings.HasPrefix(path, "/api/v2/") || apimw.MaintenanceExempt(path) {
			continue
		}
		if _, seen := visited[route]; !seen && adminsurface.Send(c.Handler, route, window, opts, nil).Maintenance() {
			t.Errorf("surface route %s (%d checks) was not visited", route, len(entry.Checks))
		}
	}
	if api == 0 || outside == 0 || reachable == 0 || closedByWindow*10 < api*9 {
		t.Fatalf("of %d routes under /api/v2 the window closed %d; %d were outside the surface and %d (route, permission) "+
			"pairs inside it: the drive or the window lost routes", api, closedByWindow, outside, reachable)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// goldenRuntimePlugins is the answer of each request below to a holder of only
// runtime.plugins, with the window open and closed, captured before R0.4b
// changed the gate. UPDATE_GOLDEN=1 rewrites it; a change is a change of
// behaviour for every existing administrator.
const goldenRuntimePlugins = "testdata/maintenance_runtime_plugins.golden"

func TestARuntimePluginsHolderGetsTheSameAnswersAsBefore(t *testing.T) {
	t.Parallel()
	requests := []request{
		maintenanceRead,
		{route: adminsurface.Route{Method: http.MethodPut, Pattern: "/api/v2/admin/maintenance/{mode}"}},
		maintenanceClear,
		{route: adminsurface.PlatformSettings},
		budgetSave,
		userRoute,
		{route: userRoute.route},
	}
	var lines []string
	for _, q := range requests {
		for _, window := range []bool{false, true} {
			got := q.send(t, window, adminaccess.RuntimePlugins)
			bypass := ""
			if q.route == adminsurface.PlatformSettings {
				bypass = fmt.Sprintf("\tbypass=%v", strings.Contains(got.Body, `"bypass":true`))
			}
			lines = append(lines, fmt.Sprintf("%s\t%s\tothers=%d\twindow=%v\t%d\tmaintenance=%v%s",
				q.route.Method, adminsurface.RequestPath(q.route.Pattern, q.params), len(q.others), window,
				got.Status, got.Maintenance(), bypass))
		}
	}
	got := strings.Join(lines, "\n") + "\n"
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(goldenRuntimePlugins), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenRuntimePlugins, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenRuntimePlugins)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_GOLDEN=1)", goldenRuntimePlugins, err)
	}
	if got != string(want) {
		t.Errorf("a holder of only runtime.plugins answers differently:\n got:\n%s\nwant:\n%s", got, want)
	}

	// Every route: with the window open the holder gets what it gets with
	// the window closed, as before (runtime.plugins was the one name the
	// gate admitted everywhere).
	c := composed(t)
	opts, err := c.Options()
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range c.Routes {
		closed := adminsurface.Send(c.Handler, route, adminsurface.Variant{OtherModes: []string{}}, opts,
			[]string{adminaccess.RuntimePlugins})
		open := adminsurface.Send(c.Handler, route, adminsurface.Variant{OtherModes: []string{}, Maintenance: true}, opts,
			[]string{adminaccess.RuntimePlugins})
		if open.Maintenance() || open.Status != closed.Status {
			t.Errorf("%s, holder of only runtime.plugins: %d with the window open, %d with it closed",
				route, open.Status, closed.Status)
		}
	}
}

// TestTheMaintenanceGateNamesNoPermission is the source test: the gate reads
// the registry's maintenance attribute, not a name.
func TestTheMaintenanceGateNamesNoPermission(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("maintenance.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, literal := range []string{`"runtime.plugins"`, "adminaccess.RuntimePlugins", "adminaccess.PlatformMaintenanceManage"} {
		if strings.Contains(string(source), literal) {
			t.Errorf("internal/api/middleware/maintenance.go names %s; the gate must read the registry's "+
				"maintenance attribute (adminaccess.AdmitsDuringMaintenance)", literal)
		}
	}
}
