package api

// The platform routes of ADR-0059 R0.4a: e-mail, operations and the prebuilt
// MCP catalogue each accept a narrow permission beside runtime.plugins.
//
// Every request goes through the production router (NewRouter), so the gate
// under test is the one router.go composes, not a hand-mounted copy. The
// resolver grants exactly one administration permission, the holder's.
//
// Two kinds of proof live here:
//
//   - The gate table (no database). For each touched route and each holder,
//     the answer is the gate's 403 or whatever the handler answers behind it.
//     The legacy holder's answers are pinned in
//     testdata/admin_platform_legacy.golden, captured on main before the
//     change: a holder of runtime.plugins sees no change.
//   - The saves (a real database, ELITEA_TEST_DATABASE_URL). A narrow holder's
//     write lands with 200, so "passes the gate" is not mistaken for "works".

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
)

const goldenAdminPlatformLegacy = "testdata/admin_platform_legacy.golden"

// platformGateRoute is one route this story touches, with the narrow
// permission it now accepts beside runtime.plugins.
type platformGateRoute struct {
	method, path, body string
	narrow             string
}

// platformGateRoutes are the routes of R0.4a, in the order of the golden file.
// The bodies are the smallest a handler accepts as JSON; the gate never reads
// them.
var platformGateRoutes = []platformGateRoute{
	{http.MethodGet, "/api/v2/admin/system_info/administration", "", adminaccess.PlatformOperationsView},
	{http.MethodGet, "/api/v2/tracing/status/administration", "", adminaccess.PlatformOperationsView},
	{http.MethodGet, "/api/v2/admin/background_jobs/administration", "", adminaccess.PlatformOperationsView},
	{http.MethodPost, "/api/v2/admin/background_jobs/administration/agent/job-1:cancel", "", adminaccess.PlatformOperationsManage},
	{http.MethodPost, "/api/v2/admin/background_jobs/administration/pat_expiry_notices:run", "", adminaccess.PlatformOperationsManage},
	{http.MethodGet, "/api/v2/admin/email/administration", "", adminaccess.PlatformEmailManage},
	{http.MethodPut, "/api/v2/admin/email/administration", "{}", adminaccess.PlatformEmailManage},
	{http.MethodPost, "/api/v2/admin/email/test/administration", "{}", adminaccess.PlatformEmailManage},
	{http.MethodGet, "/api/v2/admin/mcp_prebuilt_servers/administration", "", adminaccess.MCPCatalogueManage},
	{http.MethodPut, "/api/v2/admin/mcp_prebuilt_servers/administration/fixture", "{}", adminaccess.MCPCatalogueManage},
	{http.MethodDelete, "/api/v2/admin/mcp_prebuilt_servers/administration/fixture", "", adminaccess.MCPCatalogueManage},
}

// newPlatformGateRouter is the production router with no database and a
// resolver that grants exactly granted in every mode.
func newPlatformGateRouter(t *testing.T, granted ...string) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		PermissionResolver: platformGateResolver{granted: granted},
	})
}

// platformGateResolver grants exactly granted in every mode, with or without
// a project.
type platformGateResolver struct{ granted []string }

func (p platformGateResolver) ResolvePermissions(
	context.Context, auth.User, string, string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 1, Permissions: append([]string(nil), p.granted...)}, nil
}

func (p platformGateResolver) ResolveMembershipPermissions(
	ctx context.Context, user auth.User, mode string,
) (auth.PermissionResolution, error) {
	return p.ResolvePermissions(ctx, user, mode, "")
}

func servePlatformGate(t *testing.T, router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := testAuthHeader(httptest.NewRequestWithContext(context.Background(), method, path,
		bytes.NewReader([]byte(body))))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestPlatformRoutesAnswerTheLegacyHolderAsBefore pins every touched route's
// answer to a holder of runtime.plugins alone. The file was captured on main
// before the gates were widened; regenerate it with UPDATE_GOLDEN=1 only for
// a change that means to alter what a legacy holder sees.
func TestPlatformRoutesAnswerTheLegacyHolderAsBefore(t *testing.T) {
	router := newPlatformGateRouter(t, adminaccess.RuntimePlugins)
	var b strings.Builder
	for _, route := range platformGateRoutes {
		rec := servePlatformGate(t, router, route.method, route.path, route.body)
		fmt.Fprintf(&b, "%s\t%s\t%d\n", route.method, route.path, rec.Code)
	}
	got := b.String()

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenAdminPlatformLegacy), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenAdminPlatformLegacy, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenAdminPlatformLegacy)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_GOLDEN=1)", goldenAdminPlatformLegacy, err)
	}
	if got != string(want) {
		t.Errorf("a holder of runtime.plugins gets different answers than before:\n got:\n%s\nwant:\n%s", got, want)
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.HasSuffix(line, "\t403") {
			t.Errorf("%s: the legacy holder is refused", line)
		}
	}
}

// platformNarrowNames are the four names this story wires.
var platformNarrowNames = []string{
	adminaccess.PlatformEmailManage,
	adminaccess.PlatformOperationsView,
	adminaccess.PlatformOperationsManage,
	adminaccess.MCPCatalogueManage,
}

// platformNeighbourRoutes stay on runtime.plugins (or their own permission)
// in this story: identity (R0.4c) and maintenance (R0.4b). A narrow holder is
// refused on every one. The Configuration page was a neighbour until R0.5
// gave its sections their own narrow path: the e-mail and the MCP servers
// sections now admit platform.email.manage and mcp.catalogue.manage, which
// v2/admin/config_values_narrow_test.go proves.
var platformNeighbourRoutes = []platformGateRoute{
	{method: http.MethodPut, path: "/api/v2/admin/identity_providers/administration/fixture", body: "{}"},
	{method: http.MethodGet, path: "/api/v2/admin/maintenance/administration"},
}

// TestPlatformRoutesAdmitTheirNarrowNameOnly: a holder of one narrow name gets
// the legacy holder's answer on the routes of that name, and the gate's 403 on
// every other touched route and on the neighbours this story leaves alone. A
// holder of nothing is refused everywhere.
func TestPlatformRoutesAdmitTheirNarrowNameOnly(t *testing.T) {
	legacy := newPlatformGateRouter(t, adminaccess.RuntimePlugins)
	for _, holder := range append([]string{""}, platformNarrowNames...) {
		router := newPlatformGateRouter(t, holder)
		for _, route := range platformGateRoutes {
			got := servePlatformGate(t, router, route.method, route.path, route.body).Code
			if holder != route.narrow {
				if got != http.StatusForbidden {
					t.Errorf("holder %q: %s %s = %d, want 403", holder, route.method, route.path, got)
				}
				continue
			}
			want := servePlatformGate(t, legacy, route.method, route.path, route.body).Code
			if got != want || got == http.StatusForbidden {
				t.Errorf("holder %q: %s %s = %d, want the legacy holder's %d", holder, route.method, route.path, got, want)
			}
		}
		for _, route := range platformNeighbourRoutes {
			if got := servePlatformGate(t, router, route.method, route.path, route.body).Code; got != http.StatusForbidden {
				t.Errorf("holder %q: %s %s = %d, want 403 (not this story's route)", holder, route.method, route.path, got)
			}
		}
	}
}

// A holder of only platform.operations.view reads the tracing status and is
// refused a job cancel.
func TestOperationsViewReadsTracingStatusAndCannotCancelAJob(t *testing.T) {
	router := newPlatformGateRouter(t, adminaccess.PlatformOperationsView)

	status := servePlatformGate(t, router, http.MethodGet, "/api/v2/tracing/status/administration", "")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"enabled"`) {
		t.Errorf("tracing status = %d %s, want 200 with the status document", status.Code, status.Body)
	}
	cancel := servePlatformGate(t, router, http.MethodPost,
		"/api/v2/admin/background_jobs/administration/agent/job-1:cancel", "")
	if cancel.Code != http.StatusForbidden {
		t.Errorf("job cancel = %d, want 403", cancel.Code)
	}
}

// newPlatformGateDatabaseRouter is the production router on a migrated
// database (ELITEA_TEST_DATABASE_URL), with the one-permission resolver.
func newPlatformGateDatabaseRouter(t *testing.T, granted string) chi.Router {
	t.Helper()
	if testpg.DSN(os.Getenv(credentialJourneyDatabaseURL)) == "" {
		t.Skipf("set %s to run the platform gate saves", credentialJourneyDatabaseURL)
	}
	pool := newCredentialJourneyPool(t)
	return NewRouter(RouterConfig{
		Pool:               pool,
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		PermissionResolver: platformGateResolver{granted: []string{granted}},
	})
}

// A holder of only platform.email.manage saves the e-mail settings and is
// refused an identity provider write.
func TestEmailManageSavesEmailSettingsAndCannotTouchIdentity(t *testing.T) {
	router := newPlatformGateDatabaseRouter(t, adminaccess.PlatformEmailManage)

	saved := servePlatformGate(t, router, http.MethodPut, "/api/v2/admin/email/administration",
		`{"host":"smtp.example.com","port":587,"tls":"starttls","from":"noreply@example.com"}`)
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), `"smtp.example.com"`) {
		t.Fatalf("e-mail save = %d %s, want 200 with the stored host", saved.Code, saved.Body)
	}
	identity := servePlatformGate(t, router, http.MethodPut,
		"/api/v2/admin/identity_providers/administration/fixture", "{}")
	if identity.Code != http.StatusForbidden {
		t.Errorf("identity provider save = %d, want 403", identity.Code)
	}
}

// A holder of only mcp.catalogue.manage saves a prebuilt MCP server and is
// refused the e-mail settings.
func TestMCPCatalogueManageSavesAPrebuiltServerAndCannotTouchEmail(t *testing.T) {
	router := newPlatformGateDatabaseRouter(t, adminaccess.MCPCatalogueManage)

	saved := servePlatformGate(t, router, http.MethodPut,
		"/api/v2/admin/mcp_prebuilt_servers/administration/r04a-fixture",
		`{"display_name":"R0.4a fixture","url":"https://mcp.example.com/mcp"}`)
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), `"r04a-fixture"`) {
		t.Fatalf("prebuilt MCP save = %d %s, want 200 with the saved key", saved.Code, saved.Body)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		if got := servePlatformGate(t, router, method, "/api/v2/admin/email/administration", "{}").Code; got != http.StatusForbidden {
			t.Errorf("%s e-mail settings = %d, want 403", method, got)
		}
	}
}
