package admin_test

// The Configuration routes of ADR-0059 R0.5, driven through the production
// router (api.NewRouter), so both gates under test are the ones router.go
// composes: the route gate and the section check in the handler. The resolver
// grants exactly the permissions a test names, in every mode.
//
// testdata/config_sections_legacy.golden pins the answers of the two legacy
// holders over every section, captured on main before the narrow path was
// added: a holder of configuration.governance alone, and a holder of
// runtime.plugins and configuration.governance.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const goldenConfigSectionsLegacy = "testdata/config_sections_legacy.golden"

const (
	configSchemasPath     = "/api/v2/admin/plugin_config_schemas/administration"
	configValuesPath      = "/api/v2/admin/plugin_config_values/administration/"
	configSuggestionsPath = "/api/v2/admin/plugin_config_suggestions/administration/"
	configTestToken       = "config-sections-token"
)

// configSuggestionKeys are the suggestion sources the sections declare, plus
// one that no section declares.
var configSuggestionKeys = []string{
	"toolkit_names", "toolkit_tools?toolkit=fixture", "projects", "gateway_providers", "unknown_source",
}

type configTokenValidator struct{}

func (configTokenValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	if token != configTestToken {
		return auth.User{}, auth.ErrPermissionDenied
	}
	return auth.User{ID: "1", UserID: "1", Email: "admin@test.local", AuthType: "token"}, nil
}

type configPrincipalValidator struct{}

func (configPrincipalValidator) ValidatePrincipal(_ context.Context, user auth.User) (auth.User, error) {
	return user, nil
}

// configGrantResolver grants exactly granted in every mode.
type configGrantResolver struct{ granted []string }

func (c configGrantResolver) ResolvePermissions(
	context.Context, auth.User, string, string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 1, Permissions: append([]string(nil), c.granted...)}, nil
}

func (c configGrantResolver) ResolveMembershipPermissions(
	ctx context.Context, user auth.User, mode string,
) (auth.PermissionResolution, error) {
	return c.ResolvePermissions(ctx, user, mode, "")
}

func newConfigRouter(granted ...string) http.Handler {
	return api.NewRouter(api.RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      configTokenValidator{},
		PrincipalValidator: configPrincipalValidator{},
		PermissionResolver: configGrantResolver{granted: granted},
	})
}

func serveConfig(t *testing.T, router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+configTestToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// listedSections answers the section ids of the schema list and its status.
func listedSections(t *testing.T, router http.Handler) ([]string, int) {
	t.Helper()
	rec := serveConfig(t, router, http.MethodGet, configSchemasPath, "")
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	var schemas struct {
		Sections []struct {
			ID string `json:"id"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &schemas); err != nil {
		t.Fatalf("decode the schema list: %v", err)
	}
	ids := make([]string, 0, len(schemas.Sections))
	for _, s := range schemas.Sections {
		ids = append(ids, s.ID)
	}
	return ids, rec.Code
}

// everySection answers every section id, read by a holder of every name.
func everySection(t *testing.T) []string {
	t.Helper()
	ids, code := listedSections(t, newConfigRouter(adminaccess.Names()...))
	if code != http.StatusOK || len(ids) < 40 {
		t.Fatalf("the schema list for a holder of every name answered %d with %d sections", code, len(ids))
	}
	return ids
}

// configAnswers drives the three routes for every section and answers one
// line per request: the schema list, GET and PUT of each section's values,
// and each suggestion source.
func configAnswers(t *testing.T, sections []string, granted ...string) string {
	t.Helper()
	router := newConfigRouter(granted...)
	var b strings.Builder
	_, code := listedSections(t, router)
	fmt.Fprintf(&b, "GET\t%s\t%d\n", configSchemasPath, code)
	for _, id := range sections {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			body := ""
			if method == http.MethodPut {
				body = `{"values":{}}`
			}
			rec := serveConfig(t, router, method, configValuesPath+id, body)
			fmt.Fprintf(&b, "%s\t%s%s\t%d\n", method, configValuesPath, id, rec.Code)
		}
	}
	for _, key := range configSuggestionKeys {
		rec := serveConfig(t, router, http.MethodGet, configSuggestionsPath+key, "")
		fmt.Fprintf(&b, "GET\t%s%s\t%d\n", configSuggestionsPath, key, rec.Code)
	}
	return b.String()
}

// TestConfigurationRoutesAnswerTheLegacyHoldersAsBefore pins the answers of
// the two legacy holders over every section. Regenerate the file with
// UPDATE_GOLDEN=1 only for a change that means to alter what a legacy holder
// sees.
func TestConfigurationRoutesAnswerTheLegacyHoldersAsBefore(t *testing.T) {
	sections := everySection(t)
	var b strings.Builder
	for _, holder := range [][]string{
		{adminaccess.ConfigurationGovernance},
		{adminaccess.RuntimePlugins, adminaccess.ConfigurationGovernance},
	} {
		fmt.Fprintf(&b, "# holder: %s\n", strings.Join(holder, " "))
		b.WriteString(configAnswers(t, sections, holder...))
	}
	got := b.String()

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenConfigSectionsLegacy, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenConfigSectionsLegacy)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_GOLDEN=1)", goldenConfigSectionsLegacy, err)
	}
	if got != string(want) {
		t.Errorf("a legacy holder gets different answers than before:\n got:\n%s\nwant:\n%s", got, want)
	}
}

// sectionAnswer is the status a holder gets on one section's values.
func sectionAnswer(t *testing.T, router http.Handler, method, id string) int {
	t.Helper()
	body := ""
	if method == http.MethodPut {
		body = `{"values":{}}`
	}
	return serveConfig(t, router, method, configValuesPath+id, body).Code
}

// legacyAdministratorNames hold every section on the legacy path.
var legacyAdministratorNames = []string{
	adminaccess.RuntimePlugins, adminaccess.ConfigurationGovernance, adminaccess.ConfigurationBranding,
	adminaccess.ConfigurationNativeClients, adminaccess.ConfigurationMicrosoftApps,
	adminaccess.ConfigurationServiceDescriptors, adminaccess.EntitlementsExport,
}

// A holder of only llm.guardrails.manage passes both gates on the guardrails
// section, read and write, and gets 403 on the maintenance section. The
// router has no store, so a passed request answers what the legacy
// administrator gets (500 from the store); the save itself is proven against
// a database by TestAGuardrailsManagerSavesTheGuardrailsSection.
func TestAGuardrailsManagerReachesGuardrailsAndNotMaintenance(t *testing.T) {
	legacy := newConfigRouter(legacyAdministratorNames...)
	router := newConfigRouter(adminaccess.LLMGuardrailsManage)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		got, want := sectionAnswer(t, router, method, "guardrails"), sectionAnswer(t, legacy, method, "guardrails")
		if got == http.StatusForbidden || got != want {
			t.Errorf("%s guardrails = %d, want the legacy administrator's %d", method, got, want)
		}
		if got := sectionAnswer(t, router, method, "maintenance"); got != http.StatusForbidden {
			t.Errorf("%s maintenance = %d, want 403", method, got)
		}
	}
}

// The save itself, against a database (ELITEA_TEST_DATABASE_URL): a holder of
// only llm.guardrails.manage saves the guardrails section through the route
// gate and the handler router.go composes, reads it back, and is refused the
// maintenance section.
func TestAGuardrailsManagerSavesTheGuardrailsSection(t *testing.T) {
	pool := newConfigPool(t)
	handler := admin.NewHandler(pool, admin.WithPermissionResolver(grantingResolver(adminaccess.LLMGuardrailsManage)))
	principal := auth.User{ID: "7", UserID: "7", Email: "operator@example.com"}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.With(handler.RequireConfigurationAccess).
		Get("/admin/plugin_config_values/administration/{plugin}", handler.AdministrationPluginConfigValues)
	router.With(handler.RequireConfigurationAccess).
		Put("/admin/plugin_config_values/administration/{plugin}", handler.AdministrationPluginConfigValuesSave)

	const guardrails = "/admin/plugin_config_values/administration/guardrails"
	saved := configDo(t, router, http.MethodPut, guardrails,
		map[string]any{"values": map[string]any{"blocked_toolkits": []any{"shell"}}})
	if saved.Code != http.StatusOK {
		t.Fatalf("PUT guardrails = %d, want 200 (body %s)", saved.Code, saved.Body.String())
	}
	read := configDo(t, router, http.MethodGet, guardrails, nil)
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"shell"`) {
		t.Fatalf("GET guardrails = %d %s, want 200 with the saved toolkit", read.Code, read.Body.String())
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		var body any
		if method == http.MethodPut {
			body = map[string]any{"values": map[string]any{"enabled": true}}
		}
		if got := configDo(t, router, method, "/admin/plugin_config_values/administration/maintenance", body); got.Code != http.StatusForbidden {
			t.Errorf("%s maintenance = %d, want 403 (body %s)", method, got.Code, got.Body.String())
		}
	}
}

// The route gate fails closed when no resolver is wired.
func TestTheConfigurationRouteGateFailsClosedWithoutAResolver(t *testing.T) {
	handler := admin.NewHandler(nil)
	gated := handler.RequireConfigurationAccess(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, configSchemasPath, nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "7", UserID: "7"}))
	rec := httptest.NewRecorder()
	gated.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no resolver: %d, want 403", rec.Code)
	}
}

// A holder of a view permission reads its sections and is refused their PUT.
func TestAViewHolderReadsAndCannotSave(t *testing.T) {
	legacy := newConfigRouter(legacyAdministratorNames...)
	for holder, sections := range map[string][]string{
		adminaccess.LLMPolicyView:      {"guardrails", "circuit_breaker", "governance"},
		adminaccess.PlatformConfigView: {"analytics", "resources", "dedicated_banner"},
		adminaccess.IdentitySSOView:    {"auth", "session_policy"},
		// #3026: the export permission reads its section and does not save it.
		adminaccess.EntitlementsExport: {"entitlement_export"},
	} {
		router := newConfigRouter(holder)
		for _, id := range sections {
			got, want := sectionAnswer(t, router, http.MethodGet, id), sectionAnswer(t, legacy, http.MethodGet, id)
			if got == http.StatusForbidden || got != want {
				t.Errorf("%s: GET %s = %d, want the legacy administrator's %d", holder, id, got, want)
			}
			if got := sectionAnswer(t, router, http.MethodPut, id); got != http.StatusForbidden {
				t.Errorf("%s: PUT %s = %d, want 403", holder, id, got)
			}
		}
	}
}

// #3026: entitlements.export.manage saves the entitlement_export section, as
// the legacy administrator does, and reaches no other section.
func TestTheEntitlementExportManagerSavesOnlyItsSection(t *testing.T) {
	legacy := newConfigRouter(legacyAdministratorNames...)
	router := newConfigRouter(adminaccess.EntitlementsExportManage)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		got := sectionAnswer(t, router, method, "entitlement_export")
		want := sectionAnswer(t, legacy, method, "entitlement_export")
		if got == http.StatusForbidden || got != want {
			t.Errorf("%s entitlement_export = %d, want the legacy administrator's %d", method, got, want)
		}
		if got := sectionAnswer(t, router, method, "auth"); got != http.StatusForbidden {
			t.Errorf("%s auth = %d, want 403", method, got)
		}
	}
}

// The schema list for a holder of only llm.policy.view names exactly the
// sections that permission reads, and the suggestions answer that holder,
// as they answer the legacy administrator, for the sources those sections
// declare.
func TestThePolicyViewerListsItsSectionsOnly(t *testing.T) {
	router := newConfigRouter(adminaccess.LLMPolicyView)
	ids, code := listedSections(t, router)
	if want := []string{"guardrails", "governance", "circuit_breaker"}; code != http.StatusOK || !slices.Equal(ids, want) {
		t.Fatalf("schema list = %d %v, want 200 %v", code, ids, want)
	}
	// guardrails declares the toolkit sources and governance declares
	// projects (501 here: no registry and no store).
	legacy := newConfigRouter(legacyAdministratorNames...)
	for _, key := range []string{"toolkit_names", "toolkit_tools?toolkit=fixture", "projects"} {
		got := serveConfig(t, router, http.MethodGet, configSuggestionsPath+key, "")
		want := serveConfig(t, legacy, http.MethodGet, configSuggestionsPath+key, "")
		if got.Code == http.StatusForbidden || got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Errorf("suggestions %s = %d %s, want the legacy administrator's %d %s",
				key, got.Code, got.Body, want.Code, want.Body)
		}
	}
}

// A holder who cannot read the section that declares a source gets exactly
// the unknown-source answer: the response names no section.
func TestSuggestionsForAnUnreadableSectionAnswerAsAnUnknownSource(t *testing.T) {
	router := newConfigRouter(adminaccess.PlatformEmailManage)
	unknown := serveConfig(t, router, http.MethodGet, configSuggestionsPath+"unknown_source", "")
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown source = %d, want 400", unknown.Code)
	}
	for _, key := range []string{"toolkit_names", "toolkit_tools?toolkit=fixture", "projects", "gateway_providers"} {
		got := serveConfig(t, router, http.MethodGet, configSuggestionsPath+key, "")
		if got.Code != unknown.Code || got.Body.String() != unknown.Body.String() {
			t.Errorf("suggestions %s = %d %s, want the unknown-source answer %d %s",
				key, got.Code, got.Body, unknown.Code, unknown.Body)
		}
	}
}

// Both administrator roles keep the whole Configuration page: their declared
// sets list every section. A section whose read half no migration grants
// would vanish from every administrator's page, which is the regression the
// service_descriptors read half avoids.
func TestBothAdministratorRolesListEverySection(t *testing.T) {
	sections := everySection(t)
	for _, name := range []string{adminaccess.RoleSuperAdmin, adminaccess.RoleAdmin} {
		role, ok := adminaccess.LookupBuiltInRole(name)
		if !ok || !role.DeclaresSet() {
			t.Fatalf("built-in role %s has no declared set", name)
		}
		listed, code := listedSections(t, newConfigRouter(role.Set()...))
		if code != http.StatusOK || !slices.Equal(listed, sections) {
			t.Errorf("%s: the schema list answers %d with %v, want every section %v", name, code, listed, sections)
		}
	}
}

// For every narrow name, the single-permission sections' names and the legacy
// holders, the schema list names exactly the sections whose values the holder
// can GET: a listed section never answers 403, and one that does not is
// listed.
func TestTheSchemaListNamesExactlyTheReadableSections(t *testing.T) {
	sections := everySection(t)
	holders := [][]string{
		{adminaccess.RuntimePlugins},
		{adminaccess.RuntimePlugins, adminaccess.ConfigurationGovernance},
		legacyAdministratorNames,
	}
	single := []string{
		adminaccess.ConfigurationBranding, adminaccess.ConfigurationNativeClients,
		adminaccess.ConfigurationMicrosoftApps, adminaccess.ConfigurationServiceDescriptors,
	}
	for _, name := range adminaccess.Names() {
		if row, _ := adminaccess.Lookup(name); len(row.Narrows) > 0 || slices.Contains(single, name) {
			holders = append(holders, []string{name})
		}
	}
	for _, holder := range holders {
		router := newConfigRouter(holder...)
		listed, code := listedSections(t, router)
		var readable []string
		for _, id := range sections {
			if sectionAnswer(t, router, http.MethodGet, id) != http.StatusForbidden {
				readable = append(readable, id)
			}
		}
		if len(readable) == 0 {
			if code != http.StatusForbidden {
				t.Errorf("holder %v reads no section, and the schema list answers %d, want 403", holder, code)
			}
			continue
		}
		if !slices.Equal(listed, readable) {
			t.Errorf("holder %v: the schema list names %v, the readable sections are %v", holder, listed, readable)
		}
	}
}
