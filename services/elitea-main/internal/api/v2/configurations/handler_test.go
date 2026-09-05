package configurations_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type permissionResolverFunc func(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error)

func (f permissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	principal auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	return f(ctx, principal, mode, projectID)
}

// AllConfigurationPermissions is what an entitled caller resolves. It is the
// full set the routes gate on, so a test that is about a handler's BODY is not
// also a test of its gate.
//
// It is a variable rather than a literal in each helper so a route gated on a
// string absent from this list fails visibly, as a 403 in the body test, rather
// than being silently admitted.
var allConfigurationPermissions = []string{
	handler.CurrentConfigurationListPermission,
	handler.CurrentConfigurationGetPermission,
	handler.CurrentConfigurationCreatePermission,
	handler.CurrentConfigurationUpdatePermission,
	handler.CurrentConfigurationDeletePermission,
}

// entitledResolver admits every configuration permission for any project.
func entitledResolver() permissionResolverFunc {
	return func(
		_ context.Context,
		_ auth.User,
		_ string,
		_ string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 1, Permissions: allConfigurationPermissions}, nil
	}
}

// withTestUser stands in for apimw.Auth, which production wraps this mount in.
// Every gate answers 401 without a user in the request context, so a body test
// would otherwise measure the missing credential instead of the handler.
func withTestUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "1"})))
	})
}

// setupConfigRouter creates a router that mounts the handler routes under the given base path.
// Because configurations.NewHandler requires a *pgxpool.Pool (no Repository interface),
// we can only test the pool-independent handlers (Available, CheckConnection,
// BatchCheckConnections, ListModels, SetDefaultModel, ListTypes, TTSVoices) and confirm
// that DB-backed handlers return a graceful response when the pool is nil (they recover
// internally via error checks and return empty results or 404).
//
// DB-backed endpoints (List, Get, Create, Update, Delete) are not tested here because
// they require a live or mock pgxpool.Pool; integration tests cover those paths.
//
// The router carries an entitled caller and an admitting resolver: since #496
// every project-scoped route in Routes() is gated, and the gate is the subject
// of TestEveryConfigurationRouteIsGated below, not of these body tests.
func setupConfigRouter() *chi.Mux {
	// NewHandler panics if pool is nil only when it tries to use it.
	// For static handlers the pool is never accessed, so we can safely pass nil.
	h := handler.NewHandler(nil, handler.WithPermissionResolver(entitledResolver()))
	r := chi.NewRouter()
	r.Use(withTestUser)
	r.Mount("/api/v2", h.Routes())
	return r
}

// checkerCall records one invocation of fakeConnectionChecker.Check, so tests
// can assert not just the HTTP response but that a real round trip actually
// happened — the property #319 requires ("must FAIL if the implementation
// reports success without a provider round trip").
type checkerCall struct {
	configType string
	data       map[string]any
}

// fakeConnectionChecker is a test double for handler.ConnectionChecker. A
// handler bug that reports success without calling Check (the exact defect
// #319 fixes) is caught by asserting len(calls) rather than trusting the
// canned result field.
type fakeConnectionChecker struct {
	// mu guards calls: BatchCheckStoredConnections fans Check out over
	// goroutines, so an unguarded append is a real data race under -race —
	// in the test double, not in production, which is why only the
	// postgres-enabled batch test ever tripped it.
	mu     sync.Mutex
	calls  []checkerCall
	result handler.ConnectionCheckResult
	err    error
}

func (f *fakeConnectionChecker) Check(_ context.Context, configType string, data map[string]any) (handler.ConnectionCheckResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, checkerCall{configType: configType, data: data})
	return f.result, f.err
}

// recordedCalls snapshots the recorder under the same lock the writer takes.
func (f *fakeConnectionChecker) recordedCalls() []checkerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]checkerCall(nil), f.calls...)
}

func setupConfigRouterWithChecker(checker handler.ConnectionChecker) *chi.Mux {
	h := handler.NewHandler(nil,
		handler.WithConnectionChecker(checker),
		handler.WithPermissionResolver(entitledResolver()),
	)
	r := chi.NewRouter()
	r.Use(withTestUser)
	r.Mount("/api/v2", h.Routes())
	return r
}

// configurationRoute is one registration in Routes(), with the permission its
// gate names.
//
// The table is the ledger this file is measured against: every route the mounted
// subrouter registers appears exactly once, so a route added without a gate
// makes TestEveryConfigurationRouteIsGated fail rather than pass quietly. That
// is the property #496 needed and did not have — the surface shipped ungated
// while every body test passed.
type configurationRoute struct {
	method     string
	path       string
	permission string
}

// configurationRoutes lists all 28 registrations, mode-less twin and `{mode}`
// twin alike. The `{mode}` rows use `administration` deliberately: that is the
// segment a caller would reach for to escape a project-scoped gate, and it must
// resolve in the DEFAULT mode like every other row.
var configurationRoutes = []configurationRoute{
	{http.MethodGet, "/api/v2/configurations/configurations/7", handler.CurrentConfigurationListPermission},
	{http.MethodGet, "/api/v2/configurations/configurations/administration/7", handler.CurrentConfigurationListPermission},
	{http.MethodPost, "/api/v2/configurations/configurations/7", handler.CurrentConfigurationCreatePermission},
	{http.MethodPost, "/api/v2/configurations/configurations/administration/7", handler.CurrentConfigurationCreatePermission},
	{http.MethodGet, "/api/v2/configurations/configuration/7/11", handler.CurrentConfigurationGetPermission},
	{http.MethodGet, "/api/v2/configurations/configuration/administration/7/11", handler.CurrentConfigurationGetPermission},
	{http.MethodPut, "/api/v2/configurations/configuration/7/11", handler.CurrentConfigurationUpdatePermission},
	{http.MethodPut, "/api/v2/configurations/configuration/administration/7/11", handler.CurrentConfigurationUpdatePermission},
	{http.MethodDelete, "/api/v2/configurations/configuration/7/11", handler.CurrentConfigurationDeletePermission},
	{http.MethodDelete, "/api/v2/configurations/configuration/administration/7/11", handler.CurrentConfigurationDeletePermission},
	{http.MethodPost, "/api/v2/configurations/check_connection/7/open_ai", handler.CurrentConfigurationCreatePermission},
	{http.MethodPost, "/api/v2/configurations/check_connection/administration/7/open_ai", handler.CurrentConfigurationCreatePermission},
	{http.MethodPost, "/api/v2/configurations/check_connections/7", handler.CurrentConfigurationCreatePermission},
	{http.MethodPost, "/api/v2/configurations/check_connections/administration/7", handler.CurrentConfigurationCreatePermission},
	// The stored checks and the revalidation address a row that already
	// exists, so they gate on the UPDATE string — the same string every other
	// write to an existing row uses. See the comment beside their registration
	// in Routes().
	{http.MethodPost, "/api/v2/configurations/check_stored_connection/7/11", handler.CurrentConfigurationUpdatePermission},
	{http.MethodPost, "/api/v2/configurations/check_stored_connection/administration/7/11", handler.CurrentConfigurationUpdatePermission},
	{http.MethodPost, "/api/v2/configurations/check_stored_connections/7", handler.CurrentConfigurationUpdatePermission},
	{http.MethodPost, "/api/v2/configurations/check_stored_connections/administration/7", handler.CurrentConfigurationUpdatePermission},
	{http.MethodPost, "/api/v2/configurations/revalidate/7/11", handler.CurrentConfigurationUpdatePermission},
	{http.MethodPost, "/api/v2/configurations/revalidate/administration/7/11", handler.CurrentConfigurationUpdatePermission},
	{http.MethodGet, "/api/v2/configurations/models/7", handler.CurrentConfigurationListPermission},
	{http.MethodGet, "/api/v2/configurations/models/administration/7", handler.CurrentConfigurationListPermission},
	{http.MethodPost, "/api/v2/configurations/models/7", handler.CurrentConfigurationUpdatePermission},
	{http.MethodPost, "/api/v2/configurations/models/administration/7", handler.CurrentConfigurationUpdatePermission},
	{http.MethodGet, "/api/v2/configurations/types/7", handler.CurrentConfigurationListPermission},
	{http.MethodGet, "/api/v2/configurations/types/administration/7", handler.CurrentConfigurationListPermission},
	{http.MethodGet, "/api/v2/configurations/tts_voices/7", handler.CurrentConfigurationListPermission},
	{http.MethodGet, "/api/v2/configurations/tts_voices/administration/7", handler.CurrentConfigurationListPermission},
}

// closedPool is a *pgxpool.Pool that is already closed, so every query returns
// an error instead of dereferencing nil.
//
// The DB-backed handlers need a non-nil pool to be reachable at all: a nil one
// panics inside pgx as soon as a gate ADMITS the request, which would make the
// entitled direction untestable. A closed pool lets the request pass the gate
// and then fail in the handler, which is all these cases measure.
func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://unused:unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatalf("build the closed pool: %v", err)
	}
	pool.Close()
	return pool
}

// gatedConfigurationRouter mounts Routes() with the supplied resolver and an
// authenticated caller, which is the shape router.go composes.
func gatedConfigurationRouter(t *testing.T, resolver auth.PermissionResolver) *chi.Mux {
	t.Helper()
	h := handler.NewHandler(closedPool(t), handler.WithPermissionResolver(resolver))
	router := chi.NewRouter()
	router.Use(withTestUser)
	router.Mount("/api/v2/configurations", h.Routes())
	return router
}

/* ── the refused direction ─────────────────────────────────────────────── */

// A caller who resolves NO permission is refused at every route (#496).
//
// This is the direction that was missing entirely: every route below answered
// 200 for any project id, and GET /configurations/configurations/{mode}/{id}
// answered with the project's whole `configuration` table, `data` included.
func TestEveryConfigurationRouteIsGated(t *testing.T) {
	seen := map[string]bool{}
	resolver := permissionResolverFunc(func(
		_ context.Context,
		_ auth.User,
		mode string,
		projectID string,
	) (auth.PermissionResolution, error) {
		// Every route must resolve in the DEFAULT mode against the path
		// project, including the `administration` twins. A gate that read the
		// mode SEGMENT would ask for central permissions here and let an
		// operator who is a member of no project read its credentials.
		if mode != auth.PermissionModeDefault || projectID != "7" {
			t.Errorf("resolver called with mode=%q project=%q, want %q and \"7\"",
				mode, projectID, auth.PermissionModeDefault)
		}
		return auth.PermissionResolution{UserID: 1, Permissions: []string{}}, nil
	})
	router := gatedConfigurationRouter(t, resolver)

	for _, route := range configurationRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			seen[route.method+" "+route.path] = true
			req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d. This route is reachable without %s.",
					rec.Code, http.StatusForbidden, route.permission)
			}
		})
	}

	// The ledger must cover the registrations, not a subset of them. chi.Walk
	// reports what is actually mounted, so a route added to Routes() and not to
	// the table is named here instead of shipping ungated and untested.
	assertLedgerCoversEveryRoute(t, router, seen)
}

// assertLedgerCoversEveryRoute walks the mounted router and fails for any
// project-scoped registration the table above does not exercise.
//
// `/available/` is the one exemption, and it is named rather than pattern
// matched: it carries no {projectID} and serves the credential-free catalogue.
func assertLedgerCoversEveryRoute(t *testing.T, router chi.Routes, seen map[string]bool) {
	t.Helper()
	const availableRoute = "GET /api/v2/configurations/available/"
	registered := 0
	var missing []string
	if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + route
		if key == availableRoute {
			return nil
		}
		registered++
		if !strings.Contains(route, "{projectID}") {
			missing = append(missing, key+" (names no {projectID}; decide what it is before adding it)")
			return nil
		}
		return nil
	}); err != nil {
		t.Fatalf("walk the mounted configuration routes: %v", err)
	}
	for _, entry := range missing {
		t.Errorf("unexpected configuration route %s", entry)
	}
	if registered != len(configurationRoutes) {
		t.Fatalf("Routes() registers %d project-scoped routes and configurationRoutes lists %d.\n"+
			"  Add the new route to the table with the permission it gates on. A route absent from\n"+
			"  the table is a route neither direction of this file measures.",
			registered, len(configurationRoutes))
	}
	if len(seen) != len(configurationRoutes) {
		t.Fatalf("the table exercised %d rows of %d", len(seen), len(configurationRoutes))
	}
}

/* ── the entitled direction ────────────────────────────────────────────── */

// Each route passes when the caller resolves the EXACT string it names, and no
// other.
//
// Without this direction a gate that refuses every caller reads as a working
// gate, which is the shape of #354, #359 and #402. The resolver returns one
// permission at a time, so a route gated on the wrong string of the five is
// caught as well.
//
// Every string here is granted in DEFAULT mode by
// migrations/shared/0072_agent_cancel_and_configuration_permissions.sql, and
// migrations/agent_cancel_and_configuration_grant_postgres_integration_test.go
// measures that on a database created EMPTY. So a pass here is a pass on a
// clean deployment, not only against this fake.
func TestEveryConfigurationRoutePassesWithItsOwnPermission(t *testing.T) {
	for _, route := range configurationRoutes {
		t.Run(route.method+" "+route.path+" "+route.permission, func(t *testing.T) {
			resolver := permissionResolverFunc(func(
				_ context.Context,
				_ auth.User,
				_ string,
				_ string,
			) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{
					UserID:      1,
					Permissions: []string{route.permission},
				}, nil
			})
			router := gatedConfigurationRouter(t, resolver)

			req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code == http.StatusForbidden {
				t.Fatalf("%s did not pass its own route gate", route.permission)
			}
		})
	}
}

// A route gated on one of the five is NOT reachable with a different one of the
// five.
//
// The table above pairs each route with a permission. Without this case the
// pairing is unmeasured: a router that gated every route on `list` would pass
// both directions above for every read, and would let a viewer delete a
// credential.
func TestAConfigurationRouteRefusesTheWrongPermission(t *testing.T) {
	for _, route := range configurationRoutes {
		for _, other := range allConfigurationPermissions {
			if other == route.permission {
				continue
			}
			t.Run(route.method+" "+route.path+" with "+other, func(t *testing.T) {
				resolver := permissionResolverFunc(func(
					_ context.Context,
					_ auth.User,
					_ string,
					_ string,
				) (auth.PermissionResolution, error) {
					return auth.PermissionResolution{UserID: 1, Permissions: []string{other}}, nil
				})
				router := gatedConfigurationRouter(t, resolver)

				req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`))
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d with %q alone, want %d. This route must need %q.",
						rec.Code, other, http.StatusForbidden, route.permission)
				}
			})
		}
	}
}

// A caller with no identity at all is refused before the resolver is asked.
//
// The mount sits inside router.go's authenticated /api/v2 group, so this is
// belt and braces — but the gate is what must hold if the mount ever moves,
// and RequireResolvedPermissionsForProject answers 401 rather than falling
// through when auth.UserFromContext misses.
func TestConfigurationRoutesRefuseAnUnauthenticatedCaller(t *testing.T) {
	h := handler.NewHandler(closedPool(t), handler.WithPermissionResolver(entitledResolver()))
	router := chi.NewRouter()
	router.Mount("/api/v2/configurations", h.Routes())

	for _, route := range configurationRoutes {
		req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d without a caller, want %d",
				route.method, route.path, rec.Code, http.StatusUnauthorized)
		}
	}
}

// A Handler built without WithPermissionResolver serves nothing.
//
// This is the fail-closed direction. The option is easy to omit at a
// composition site, and the omission must cost the caller the route rather than
// cost the tenant its credentials.
func TestConfigurationRoutesFailClosedWithoutAResolver(t *testing.T) {
	h := handler.NewHandler(closedPool(t))
	router := chi.NewRouter()
	router.Use(withTestUser)
	router.Mount("/api/v2/configurations", h.Routes())

	for _, route := range configurationRoutes {
		req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d with no resolver composed, want %d",
				route.method, route.path, rec.Code, http.StatusForbidden)
		}
	}
}

// The catalogue route stays open to any authenticated caller, and it is the ONLY
// one that does.
//
// Stated as its own case so that gating it later is a deliberate change with a
// failing test, and so that a reader does not have to infer the exemption from
// the absence of a row in the table.
func TestTheAvailableCatalogueIsTheOnlyUngatedConfigurationRoute(t *testing.T) {
	resolver := permissionResolverFunc(func(
		_ context.Context,
		_ auth.User,
		_ string,
		_ string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 1, Permissions: []string{}}, nil
	})
	router := gatedConfigurationRouter(t, resolver)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/configurations/available/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("the credential-free catalogue answered %d to a caller with no permission; "+
			"NewCurrentAvailableRoute serves the same snapshot with authentication only",
			rec.Code)
	}
}

// ---- Available ---------------------------------------------------------------

// availableEntry mirrors the wire shape of one /configurations/available/ row.
// It is deliberately declared here rather than reusing an exported handler
// type: the response contract is what the credential type picker parses
// (features/credentials/api/configurations.ts ConfigurationTypeDescriptor),
// and a test that decodes into the handler's own struct cannot notice a field
// being renamed on both sides at once.
type availableEntry struct {
	Type              string          `json:"type"`
	Section           string          `json:"section"`
	ConfigSchema      json.RawMessage `json:"config_schema"`
	HasTestConnection bool            `json:"has_test_connection"`
}

func decodeAvailable(t *testing.T, query string) []availableEntry {
	t.Helper()
	r := setupConfigRouter()

	req := httptest.NewRequest(http.MethodGet, "/api/v2/available/"+query, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var types []availableEntry
	if err := json.NewDecoder(rec.Body).Decode(&types); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	return types
}

// The route must serve the pinned registry snapshot, not the hardcoded
// eight-row list it used to (#131). The type names below are the snapshot's
// (`open_ai`), not the old list's (`openai`), so a regression to the hardcoded
// payload fails here rather than only in the browser.
func TestAvailableServesPinnedRegistrySnapshot(t *testing.T) {
	types := decodeAvailable(t, "")

	found := make(map[string]availableEntry, len(types))
	for _, ct := range types {
		found[ct.Type] = ct
	}
	for _, want := range []string{"open_ai", "azure_open_ai", "vertex_ai", "llm_model", "github"} {
		if _, ok := found[want]; !ok {
			t.Errorf("expected type %q in available list", want)
		}
	}
	// The old payload had eight static rows plus whatever the DB happened to
	// hold; the snapshot is a fixed 52.
	if len(types) != 52 {
		t.Errorf("expected the 52 pinned entries, got %d", len(types))
	}
	if got := found["open_ai"].Section; got != "ai_credentials" {
		t.Errorf("open_ai section = %q, want ai_credentials", got)
	}
	if !found["open_ai"].HasTestConnection {
		t.Error("open_ai must advertise has_test_connection; the form renders Test connection from it")
	}
}

// Every entry must carry a usable `config_schema`. Its absence is what crashed
// CredentialTypeSelector and made credential creation unreachable (#131).
func TestAvailableEntriesCarryConfigSchema(t *testing.T) {
	types := decodeAvailable(t, "")

	for _, ct := range types {
		if ct.Type == "" {
			t.Error("entry type must not be empty")
		}
		if ct.Section == "" {
			t.Errorf("entry section must not be empty for type %q", ct.Type)
		}
		var schema struct {
			Title      string                     `json:"title"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(ct.ConfigSchema, &schema); err != nil {
			t.Errorf("config_schema for %q is not a JSON object: %v", ct.Type, err)
			continue
		}
		if schema.Title == "" {
			t.Errorf("config_schema.title must not be empty for %q; it is the picker's tile label", ct.Type)
		}
		if _, ok := schema.Properties["data"]; !ok {
			t.Errorf("config_schema.properties.data missing for %q; the form is built from it", ct.Type)
		}
	}
}

func TestAvailableFiltersBySection(t *testing.T) {
	types := decodeAvailable(t, "?section=ai_credentials")

	if len(types) == 0 {
		t.Fatal("expected at least one ai_credentials entry")
	}
	for _, ct := range types {
		if ct.Section != "ai_credentials" {
			t.Errorf("type %q has section %q, want only ai_credentials", ct.Type, ct.Section)
		}
	}
	if len(types) == len(decodeAvailable(t, "")) {
		t.Error("section filter returned the whole catalog")
	}
}

// ---- CheckConnection --------------------------------------------------------

// TestCheckConnection_ReportsSuccessOnlyWhenCheckerCalled is the test #319
// requires: the handler must not be able to report success without actually
// invoking the connection checker (the real provider round trip). Before this
// fix, CheckConnection returned success:true unconditionally, ignoring the
// request body and never calling anything — that stub would fail this
// assertion (len(checker.recordedCalls()) would stay 0).
func TestCheckConnection_ReportsSuccessOnlyWhenCheckerCalled(t *testing.T) {
	checker := &fakeConnectionChecker{
		result: handler.ConnectionCheckResult{Success: true, Message: "Connection successful"},
	}
	r := setupConfigRouterWithChecker(checker)

	body := `{"api_base":"https://api.openai.com/v1","api_key":"sk-good"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v2/check_connection/1/open_ai", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if len(checker.recordedCalls()) != 1 {
		t.Fatalf("expected exactly one checker call, got %d — success must come from a real round trip", len(checker.recordedCalls()))
	}
	if checker.recordedCalls()[0].configType != "open_ai" {
		t.Errorf("checker called with type %q, want open_ai", checker.recordedCalls()[0].configType)
	}
	if apiBase, _ := checker.recordedCalls()[0].data["api_base"].(string); apiBase != "https://api.openai.com/v1" {
		t.Errorf("checker called with api_base %q, want the request body's value", apiBase)
	}

	var result map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if success, _ := result["success"].(bool); !success {
		t.Error("expected success=true in CheckConnection response")
	}
}

// TestCheckConnection_BadCredentialReportsFailure asserts the second half of
// #319's bar: a credential the provider rejects must surface as a failure.
// The browser (useCreateConfiguration.onTestConnection) keys its
// success/failure toast off the HTTP status, so this must be a non-2xx —
// matching legacy's check_connection.py contract exactly (400 with
// {"success":false,"message":...}).
func TestCheckConnection_BadCredentialReportsFailure(t *testing.T) {
	checker := &fakeConnectionChecker{
		result: handler.ConnectionCheckResult{Success: false, Message: "The provider rejected the credential."},
	}
	r := setupConfigRouterWithChecker(checker)

	body := `{"api_base":"https://api.openai.com/v1","api_key":"sk-bad"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v2/check_connection/1/open_ai", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if len(checker.recordedCalls()) != 1 {
		t.Fatalf("expected exactly one checker call, got %d", len(checker.recordedCalls()))
	}

	var result map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if success, _ := result["success"].(bool); success {
		t.Fatal("a credential the provider rejects must not report success")
	}
	if msg, _ := result["message"].(string); msg == "" {
		t.Error("expected a non-empty, safe failure message")
	}
}

// TestCheckConnection_CheckerTransportErrorNeverReportsSuccess proves a
// gateway-unreachable/transport failure never falls back to success — the
// exact "the routes exist, but always claim success" defect the issue
// describes must not resurface when the gateway itself is unreachable.
func TestCheckConnection_CheckerTransportErrorNeverReportsSuccess(t *testing.T) {
	checker := &fakeConnectionChecker{err: errors.New("gateway unreachable")}
	r := setupConfigRouterWithChecker(checker)

	body := `{"api_base":"https://api.openai.com/v1","api_key":"sk-x"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v2/check_connection/1/open_ai", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("a transport-level checker error must never report HTTP 200 success, got body: %s", rec.Body.String())
	}
	var result map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if success, _ := result["success"].(bool); success {
		t.Fatal("must not report success when the checker itself errors")
	}
}

// TestCheckConnection_NoCheckerConfiguredNeverReportsSuccess covers a Handler
// wired without WithConnectionChecker (e.g. LLM_GATEWAY_URL unset) — it must
// report an honest failure, not silently behave like the old stub.
func TestCheckConnection_NoCheckerConfiguredNeverReportsSuccess(t *testing.T) {
	r := setupConfigRouter() // no WithConnectionChecker

	body := `{"api_base":"https://api.openai.com/v1","api_key":"sk-x"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v2/check_connection/1/open_ai", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("no checker configured must never report success, got 200: %s", rec.Body.String())
	}
}

// TestCheckConnection_UnknownTypeReturns404WithoutCallingChecker matches
// legacy's check_connection.py: a type absent from the registry entirely is
// a 404, and nothing is called to check it.
func TestCheckConnection_UnknownTypeReturns404WithoutCallingChecker(t *testing.T) {
	checker := &fakeConnectionChecker{result: handler.ConnectionCheckResult{Success: true}}
	r := setupConfigRouterWithChecker(checker)

	req := httptest.NewRequest(http.MethodPost, "/api/v2/check_connection/1/not_a_real_type", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(checker.recordedCalls()) != 0 {
		t.Fatalf("checker must not be called for an unknown type, got %d calls", len(checker.recordedCalls()))
	}
}

// TestCheckConnection_KnownButUncheckableTypeReturns400WithoutCallingChecker
// covers every type this Go build still cannot really check (the toolkit
// credential types: github, jira, confluence, ...). It must report the honest
// "not supported yet" failure legacy's own registry fallback used — never the
// previous unconditional success.
//
// amazon_bedrock and vertex_ai used to be in that set. They are not any more:
// the gateway grew a real probe for each, so they are asserted as CHECKABLE by
// TestCheckConnection_CloudProviderTypesAreCheckable below.
func TestCheckConnection_KnownButUncheckableTypeReturns400WithoutCallingChecker(t *testing.T) {
	checker := &fakeConnectionChecker{result: handler.ConnectionCheckResult{Success: true}}
	r := setupConfigRouterWithChecker(checker)

	// "github" is a real pinned catalogue type (credentials section) but not
	// one of this build's checkable ai_credentials provider types.
	req := httptest.NewRequest(http.MethodPost, "/api/v2/check_connection/1/github", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(checker.recordedCalls()) != 0 {
		t.Fatalf("checker must not be called for a not-yet-checkable type, got %d calls", len(checker.recordedCalls()))
	}
	var result map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if success, _ := result["success"].(bool); success {
		t.Fatal("a not-yet-checkable type must never report success")
	}
}

// TestCheckConnection_CloudProviderTypesAreCheckable pins the membership of
// checkableConnectionTypes for amazon_bedrock and vertex_ai: each must reach
// the checker instead of being short-circuited into the "not supported yet"
// message.
//
// It asserts the ROUTE's behaviour rather than reading the map, because the
// map alone cannot fail usefully: what the user sees is whether the button
// tests the credential or refuses it. The checker here is a fake, so this
// proves the request is delegated — the real provider round trip lives on the
// gateway side (internal/llmproxy/checkconnection_cloud_test.go).
func TestCheckConnection_CloudProviderTypesAreCheckable(t *testing.T) {
	cases := []struct {
		configType string
		data       string
	}{
		{
			configType: "amazon_bedrock",
			data:       `{"aws_access_key_id":"AKIA","aws_secret_access_key":"secret","aws_region_name":"us-east-1"}`,
		},
		{
			configType: "vertex_ai",
			data:       `{"vertex_project":"p","vertex_location":"us-central1","vertex_credentials":"{\"type\":\"service_account\"}"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.configType, func(t *testing.T) {
			checker := &fakeConnectionChecker{
				result: handler.ConnectionCheckResult{Success: true, Message: "Connection successful"},
			}
			r := setupConfigRouterWithChecker(checker)

			req := httptest.NewRequest(http.MethodPost, "/api/v2/check_connection/1/"+c.configType, strings.NewReader(c.data))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if len(checker.recordedCalls()) != 1 {
				t.Fatalf("%s must reach the checker, got %d calls; body=%s", c.configType, len(checker.recordedCalls()), rec.Body.String())
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			var result map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if success, _ := result["success"].(bool); !success {
				t.Fatalf("expected success=true from the checker, got %v", result)
			}
		})
	}
}

// ---- BatchCheckConnections --------------------------------------------------

func TestBatchCheckConnections_Empty(t *testing.T) {
	r := setupConfigRouter()

	body := bytes.NewBufferString(`[]`)
	req := httptest.NewRequest(http.MethodPost, "/api/v2/check_connections/proj-1", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var results []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&results); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results, got %d items", len(results))
	}
}

// TestBatchCheckConnections_MixedItems proves per-item that (a) a checkable
// type reports success ONLY via a real checker call, and (b) an unknown type
// gets the legacy-parity unsupported:true flag WITHOUT any checker call —
// the batch equivalent of the single-check tests above.
func TestBatchCheckConnections_MixedItems(t *testing.T) {
	checker := &fakeConnectionChecker{
		result: handler.ConnectionCheckResult{Success: true, Message: "Connection successful"},
	}
	r := setupConfigRouterWithChecker(checker)

	payload, err := json.Marshal([]map[string]any{
		{"id": "cfg-1", "type": "open_ai", "data": map[string]any{"api_base": "https://api.openai.com/v1", "api_key": "sk-good"}},
		{"id": "cfg-2", "type": "not_a_real_type", "data": map[string]any{}},
	})
	if err != nil {
		t.Fatalf("failed to encode request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/check_connections/proj-1", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var results []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&results); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if success, _ := results[0]["success"].(bool); !success {
		t.Errorf("expected success=true for cfg-1 (checkable type + real checker success)")
	}
	if success, _ := results[1]["success"].(bool); success {
		t.Errorf("expected success=false for cfg-2 (unknown type)")
	}
	if unsupported, _ := results[1]["unsupported"].(bool); !unsupported {
		t.Errorf("expected unsupported=true for cfg-2 (unknown type), got %v", results[1])
	}
	if len(checker.recordedCalls()) != 1 {
		t.Fatalf("expected exactly one checker call (cfg-1 only), got %d — success must come from a real round trip", len(checker.recordedCalls()))
	}
}

// TestBatchCheckConnections_BadCredentialReportsFailure is the batch-path
// twin of TestCheckConnection_BadCredentialReportsFailure.
func TestBatchCheckConnections_BadCredentialReportsFailure(t *testing.T) {
	checker := &fakeConnectionChecker{
		result: handler.ConnectionCheckResult{Success: false, Message: "The provider rejected the credential."},
	}
	r := setupConfigRouterWithChecker(checker)

	payload, err := json.Marshal([]map[string]any{
		{"id": "cfg-1", "type": "open_ai", "data": map[string]any{"api_base": "https://api.openai.com/v1", "api_key": "sk-bad"}},
	})
	if err != nil {
		t.Fatalf("failed to encode request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/check_connections/proj-1", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (batch is always 200), got %d", rec.Code)
	}
	var results []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&results); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if success, _ := results[0]["success"].(bool); success {
		t.Fatal("a credential the provider rejects must not report success")
	}
	if len(checker.recordedCalls()) != 1 {
		t.Fatalf("expected exactly one checker call, got %d", len(checker.recordedCalls()))
	}
}

func TestBatchCheckConnectionsRejectsOversizedBody(t *testing.T) {
	router := setupConfigRouter()
	body := `[{"id":"` + strings.Repeat("x", (1<<20)+1) + `"}]`
	req := httptest.NewRequest(http.MethodPost, "/api/v2/check_connections/7", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestBatchCheckConnectionsRejectsTrailingJSON(t *testing.T) {
	router := setupConfigRouter()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v2/check_connections/7",
		strings.NewReader(`[] {}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// ---- ListModels -------------------------------------------------------------

func TestListModels_Success(t *testing.T) {
	r := setupConfigRouter()

	req := httptest.NewRequest(http.MethodGet, "/api/v2/models/proj-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if _, ok := body["items"]; !ok {
		t.Error("expected 'items' key in ListModels response")
	}
}

// ---- SetDefaultModel --------------------------------------------------------

// The defect: SetDefaultModel decoded the body and answered 200 with
// {"items":[],"total":0}. It wrote nothing.
//
// The evidence: the whole body was a decode and a writeJSON. The route that
// really writes a project's default model is model_default.go, and the
// production router registers it only when the composition root supplies it.
// The composition root builds it under ELITEA_CONFIGURATIONS_ENABLED, and
// deploy/helm/elitea-main/values.yaml ships that variable as "false".
//
// The failure: on a default install an administrator sets a default model and
// receives a success. Nothing is stored. The screen refetches and shows the old
// default, and no response distinguishes that from a save that worked.
//
// The old test asserted the 200, so the suite pinned the defect instead of
// catching it. TTSVoices had the same shape and was corrected the same way
// under #466.
func TestSetDefaultModelRefusesInsteadOfReportingASuccessItDidNotPerform(t *testing.T) {
	r := setupConfigRouter()

	payload, err := json.Marshal(map[string]string{"model": "gpt-4", "section": "llm"})
	if err != nil {
		t.Fatalf("failed to encode request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/models/proj-1", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var answer map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&answer); err != nil {
		t.Fatalf("failed to decode the refusal: %v", err)
	}
	reason, ok := answer["error"].(string)
	if !ok || !strings.Contains(reason, "ELITEA_CONFIGURATIONS_ENABLED") {
		t.Fatalf("the refusal must name the variable that turns the capability on; got %v", answer)
	}
	if _, present := answer["items"]; present {
		t.Fatal("the refusal must not carry an item list, which a caller reads as a result")
	}
}

// The mode-ful twin serves the same body and must refuse the same way. No
// client in the workspace calls it, but it is registered. It must therefore
// not stay the one path that still reports a success it did not perform.
func TestSetDefaultModelRefusesOnTheModeTwin(t *testing.T) {
	r := setupConfigRouter()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v2/models/administration/proj-1",
		bytes.NewReader([]byte(`{"model":"gpt-4"}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---- ListTypes --------------------------------------------------------------

func TestListTypes_Success(t *testing.T) {
	r := setupConfigRouter()

	req := httptest.NewRequest(http.MethodGet, "/api/v2/types/proj-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var types []handler.TypeDescriptor
	if err := json.NewDecoder(rec.Body).Decode(&types); err != nil {
		t.Fatalf("failed to decode type descriptors: %v", err)
	}
	if types == nil {
		t.Error("expected an empty JSON array, got null")
	}
}

// ---- TTSVoices --------------------------------------------------------------
//
// Both directions of #466. The route answered 200 with `{"voices": []}` for
// every project and every model, and the test below asserted only that the
// `voices` key was present — so it passed on the defect. The route now reports
// the missing capability: the gateway serves audio synthesis and transcription
// (#323 landed) but no voice-LISTING route in any dialect, and no code path
// fills the `meta.voices` cache the reference falls back on.

// TestTTSVoicesReportsTheMissingCapability — direction one. The refusal must
// carry a reason the caller can act on, not a bare status.
func TestTTSVoicesReportsTheMissingCapability(t *testing.T) {
	r := setupConfigRouter()

	for _, path := range []string{
		"/api/v2/tts_voices/proj-1",
		"/api/v2/tts_voices/default/proj-1",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s: expected 501, got %d; body: %s", path, rec.Code, rec.Body.String())
		}

		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("%s: failed to decode response: %v", path, err)
		}
		reason, _ := body["error"].(string)
		if reason == "" {
			t.Fatalf("%s: the refusal names no reason: %v", path, body)
		}
		if !strings.Contains(reason, "audio route") {
			t.Errorf("%s: the reason does not say why the voices are missing: %q", path, reason)
		}
	}
}

// TestTTSVoicesNeverAnswersAnEmptyVoiceList — direction two, and the guard.
//
// A caller cannot tell "this project has no voices" from "this route does no
// work" when both answers are 200 with an empty list. This fails if the stub
// comes back, whatever status it uses.
func TestTTSVoicesNeverAnswersAnEmptyVoiceList(t *testing.T) {
	r := setupConfigRouter()

	req := httptest.NewRequest(http.MethodGet, "/api/v2/tts_voices/proj-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("the route answers 200 again; body: %s", rec.Body.String())
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	voices, present := body["voices"]
	if !present {
		return
	}
	list, isList := voices.([]any)
	if isList && len(list) == 0 {
		t.Error("the route offers an empty voice list again; a caller cannot tell that from a project with no voices")
	}
}

// ---- Content-Type header checks ---------------------------------------------

func TestContentTypeJSON(t *testing.T) {
	r := setupConfigRouter()

	endpoints := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v2/available/", ""},
		{http.MethodGet, "/api/v2/types/proj-1", ""},
		{http.MethodGet, "/api/v2/tts_voices/proj-1", ""},
		{http.MethodGet, "/api/v2/models/proj-1", ""},
	}

	for _, ep := range endpoints {
		var b *bytes.Reader
		if ep.body != "" {
			b = bytes.NewReader([]byte(ep.body))
		} else {
			b = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(ep.method, ep.path, b)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		ct := rec.Header().Get("Content-Type")
		if ct == "" {
			t.Errorf("%s %s: expected Content-Type header, got empty", ep.method, ep.path)
		}
	}
}
