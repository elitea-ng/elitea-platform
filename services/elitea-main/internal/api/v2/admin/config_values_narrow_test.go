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
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
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
