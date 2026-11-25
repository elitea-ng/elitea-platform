package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
)

// reviewedShadowedRoutes are the (method, pattern) pairs a reviewed runtime
// route (mountReviewedAPIRoutes) registers on the root router while the
// compatibility routes register the same pair: the reviewed one serves it
// wherever cmd/elitea-main composes it, and the compatibility route is never
// reached there (#3024). Adding a pair here is a review of both: the reviewed
// route must make every decision the compatibility route makes.
//
// The five configuration row routes are the ones whose compatibility route
// has an administration-mode check (the central settings branch, ADR-0059
// R0.6b); the others gate in the default mode only, on both sides.
var reviewedShadowedRoutes = []string{
	"DELETE /api/v2/configurations/configuration/{projectID}/{configID}",
	"DELETE /api/v2/elitea_core/index_cancel/prompt_lib/{projectID}/{toolkitID}/{indexName}/{taskID}",
	"DELETE /api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}",
	"DELETE /api/v2/notifications/notification/prompt_lib/{projectID}/{notificationID}",
	"DELETE /api/v2/notifications/notifications/prompt_lib/{projectID}",
	"GET /api/v2/configurations/available/",
	"GET /api/v2/configurations/configuration/{projectID}/{configID}",
	"GET /api/v2/configurations/configurations/{projectID}",
	"GET /api/v2/configurations/models/{projectID}",
	"GET /api/v2/configurations/types/{projectID}",
	"GET /api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}",
	"GET /api/v2/elitea_core/project_info/prompt_lib/{projectID}/project-info",
	"GET /api/v2/notifications/notifications/prompt_lib/{projectID}",
	"GET /api/v2/social/authors/{projectID}",
	"PATCH /api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}",
	"POST /api/v2/configurations/configurations/{projectID}",
	"POST /api/v2/configurations/models/{projectID}",
	"POST /api/v2/elitea_core/test_toolkit_tool/prompt_lib/{projectID}",
	"PUT /api/v2/configurations/configuration/{projectID}/{configID}",
	"PUT /api/v2/notifications/notification/prompt_lib/{projectID}/{notificationID}",
	"PUT /api/v2/notifications/notifications/prompt_lib/{projectID}",
}

// reviewedRouteFields reads the RouterConfig fields mountReviewedAPIRoutes
// mounts, from its source, so a new reviewed route is driven with no edit
// here.
func reviewedRouteFields(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "production_router.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "mountReviewedAPIRoutes" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "cfg" {
					seen[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	fields := make([]string, 0, len(seen))
	for name := range seen {
		fields = append(fields, name)
	}
	sort.Strings(fields)
	if len(fields) < 10 {
		t.Fatalf("read %d RouterConfig fields from mountReviewedAPIRoutes; the source moved", len(fields))
	}
	return fields
}

// withEveryReviewedRoute is a RouterConfig with every field of fields set: a
// handler that answers 418 for an http.Handler, and a zero value for a
// pointer to a route type.
func withEveryReviewedRoute(t *testing.T, fields []string) RouterConfig {
	t.Helper()
	var cfg RouterConfig
	value := reflect.ValueOf(&cfg).Elem()
	teapot := reflect.ValueOf(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})))
	handlerType := reflect.TypeFor[http.Handler]()
	for _, name := range fields {
		field := value.FieldByName(name)
		switch {
		case !field.IsValid():
			t.Fatalf("mountReviewedAPIRoutes reads cfg.%s, which is no RouterConfig field", name)
		case field.Type() == handlerType:
			field.Set(teapot)
		case field.Kind() == reflect.Pointer:
			field.Set(reflect.New(field.Type().Elem()))
		default:
			t.Fatalf("cfg.%s is a %s; teach withEveryReviewedRoute to set it", name, field.Type())
		}
	}
	return cfg
}

// TestReviewedRoutesShadowOnlyThePinnedCompatibilityRoutes pins the pairs a
// reviewed runtime route shadows, and fails when one shadows a compatibility
// route that has an administration-mode check (a route gate of
// internal/api/middleware built for the administration mode, or the central
// branch's MarkAdministrationRoute) while the reviewed registration carries
// none. The compatibility route is what the administration surface tests
// drive; a reviewed route without its check would serve production a
// decision those tests never see.
func TestReviewedRoutesShadowOnlyThePinnedCompatibilityRoutes(t *testing.T) {
	router := NewRouter(withEveryReviewedRoute(t, reviewedRouteFields(t)))
	type registration struct{ administration bool }
	registrations := map[string][]registration{}
	if err := chi.Walk(router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		key := gatedRouteKey(method, route)
		registrations[key] = append(registrations[key], registration{
			administration: slices.ContainsFunc(mws, apimw.IsAdministrationGate),
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var shadowed []string
	for key, regs := range registrations {
		if len(regs) < 2 {
			continue
		}
		shadowed = append(shadowed, key)
		gated := slices.ContainsFunc(regs, func(r registration) bool { return r.administration })
		every := !slices.ContainsFunc(regs, func(r registration) bool { return !r.administration })
		if gated && !every {
			t.Errorf("%s: a reviewed route shadows a compatibility route with an administration-mode check "+
				"and carries no such check; give it the compatibility route's gate (#3024)", key)
		}
	}
	sort.Strings(shadowed)
	if !slices.Equal(shadowed, reviewedShadowedRoutes) {
		t.Errorf("the reviewed routes shadow\n  %q\nwant\n  %q\nreview the reviewed route against the compatibility "+
			"route it now hides, then update reviewedShadowedRoutes", shadowed, reviewedShadowedRoutes)
	}
}
