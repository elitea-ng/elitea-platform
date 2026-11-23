package api

// The identity routes of ADR-0059 R0.4c: identity providers, SCIM clients and
// SCIM group bindings, split by action.
//
//   - Reads accept identity.sso.view or their legacy name.
//   - Settings writes accept identity.sso.settings.manage or their legacy name.
//   - Trust actions pass a route gate that also admits the settings name and
//     the legacy name, and are then refused in the handler unless the caller
//     holds identity.sso.trust.manage or admin.auth.users.super_admin, so the
//     refusal carries its code and its audit action (issue 2558).
//
// Every request goes through the production router (NewRouter) with no
// database and a resolver that grants exactly the holder's names, so the gate
// under test is the one router.go composes. testdata/admin_identity_legacy.golden
// pins what a holder of one legacy name gets on every route; it was captured
// on main before the change. Without a database many answers are the
// handler's 503: what the file holds is that the gate let the request through,
// or refused it, as before. The handler behaviour behind the gates (a
// rename lands, an issuer change is refused, a binding needs the role) is
// proved in internal/api/v2/admin/identity_split_test.go.

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
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const goldenAdminIdentityLegacy = "testdata/admin_identity_legacy.golden"

// identityGateRoute is one route of the story. admits lists every name its
// route gate accepts; trust marks a trust action, refused in the handler to a
// caller who holds neither trust name.
type identityGateRoute struct {
	method, path, body string
	admits             []string
	trust              bool
}

const identityOIDCBody = `{"kind":"oidc","display_name":"Fixture","oidc":{"issuer":"https://idp.example.com",` +
	`"client_id":"elitea","redirect_uri":"https://elitea.example.com/auth/oidc/callback"}}`

var (
	identityProviderRead   = []string{adminaccess.IdentitySSOView, adminaccess.RuntimePlugins}
	identityProviderWrite  = []string{adminaccess.IdentitySSOSettingsManage, adminaccess.IdentitySSOTrustManage, adminaccess.RuntimePlugins}
	identitySCIMClientRead = []string{adminaccess.IdentitySSOView, adminaccess.AdminAuthUsers}
	identitySCIMClientEdit = []string{adminaccess.IdentitySSOSettingsManage, adminaccess.IdentitySSOTrustManage, adminaccess.AdminAuthUsers}
	identityBindingRead    = []string{adminaccess.IdentitySSOView, adminaccess.ConfigurationRolesUserProjectPermsView}
	identityBindingEdit    = []string{adminaccess.IdentitySSOSettingsManage, adminaccess.ConfigurationRolesUserProjectPermsEdit}
)

// identityGateRoutes are the routes of R0.4c, in the order of the golden file.
var identityGateRoutes = []identityGateRoute{
	{http.MethodGet, "/api/v2/admin/identity_providers/administration", "", identityProviderRead, false},
	{http.MethodPut, "/api/v2/admin/identity_providers/administration/fixture", identityOIDCBody, identityProviderWrite, false},
	{http.MethodDelete, "/api/v2/admin/identity_providers/administration/fixture", "", identityProviderWrite, true},
	{http.MethodPost, "/api/v2/admin/identity_providers/administration/fixture/rebind",
		`{"from_issuer":"https://old.example.com"}`, identityProviderWrite, true},
	{http.MethodGet, "/api/v2/admin/scim_clients/administration", "", identitySCIMClientRead, false},
	{http.MethodPost, "/api/v2/admin/scim_clients/administration", `{"name":"entra","auth_method":"bearer"}`,
		identitySCIMClientEdit, true},
	{http.MethodPost, "/api/v2/admin/scim_clients/administration/1/rotate", "", identitySCIMClientEdit, true},
	{http.MethodPost, "/api/v2/admin/scim_clients/administration/1/revoke", "", identitySCIMClientEdit, false},
	{http.MethodDelete, "/api/v2/admin/scim_clients/administration/1", "", identitySCIMClientEdit, true},
	{http.MethodGet, "/api/v2/admin/scim_group_bindings/administration", "", identityBindingRead, false},
	{http.MethodGet, "/api/v2/admin/scim_group_bindings/administration/project_roles/1", "", identityBindingRead, false},
	{http.MethodPost, "/api/v2/admin/scim_group_bindings/administration",
		`{"display_name":"Team","project_id":1,"role_name":"viewer"}`, identityBindingEdit, false},
	{http.MethodPut, "/api/v2/admin/scim_group_bindings/administration/1",
		`{"display_name":"Team","project_id":1,"role_name":"viewer"}`, identityBindingEdit, false},
	{http.MethodDelete, "/api/v2/admin/scim_group_bindings/administration/1", "", identityBindingEdit, false},
}

// identityLegacyNames are the names the routes accepted before R0.4c.
var identityLegacyNames = []string{
	adminaccess.RuntimePlugins,
	adminaccess.AdminAuthUsers,
	adminaccess.ConfigurationRolesUserProjectPermsView,
	adminaccess.ConfigurationRolesUserProjectPermsEdit,
}

// identityTrustCodes are the codes of the handler's trust refusal.
var identityTrustCodes = []string{
	"identity_provider_trust_super_admin_required",
	"identity_binding_super_admin_required",
	"scim_client_super_admin_required",
}

func newIdentityGateRouter(t *testing.T, granted ...string) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		PermissionResolver: identityGateResolver{granted: granted},
	})
}

// identityGateResolver grants exactly granted in every mode.
type identityGateResolver struct{ granted []string }

func (p identityGateResolver) ResolvePermissions(
	context.Context, auth.User, string, string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 1, Permissions: append([]string(nil), p.granted...)}, nil
}

func (p identityGateResolver) ResolveMembershipPermissions(
	ctx context.Context, user auth.User, mode string,
) (auth.PermissionResolution, error) {
	return p.ResolvePermissions(ctx, user, mode, "")
}

// identityAnswer is the status and, for a 403, the refusal: the route gate's
// bare "insufficient permissions" or the handler's code.
func identityAnswer(t *testing.T, router http.Handler, route identityGateRoute) string {
	t.Helper()
	req := testAuthHeader(httptest.NewRequestWithContext(context.Background(), route.method, route.path,
		bytes.NewReader([]byte(route.body))))
	if route.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		return fmt.Sprint(rec.Code)
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Code != "" {
		return "403 " + body.Code
	}
	return "403 gate"
}

// identityLegacyClosed are the rows of the golden file whose answer changes
// on purpose, with the answer they have now. A holder of runtime.plugins
// deleting an identity provider is refused in the handler: before R0.4c the
// route gate was the only check (the R0.1c action table noted it). Every
// other trust action was already refused to these holders by the issue 2558
// fix, so its row is unchanged.
var identityLegacyClosed = map[string]string{
	adminaccess.RuntimePlugins + "\tDELETE\t/api/v2/admin/identity_providers/administration/fixture": "403 identity_provider_trust_super_admin_required",
}

// TestIdentityRoutesAnswerTheLegacyHoldersAsBefore holds every route's answer
// to a holder of each legacy name alone to the golden file, captured on main
// before R0.4c, except the rows identityLegacyClosed lists. The file is a
// record of main and is not regenerated.
func TestIdentityRoutesAnswerTheLegacyHoldersAsBefore(t *testing.T) {
	raw, err := os.ReadFile(goldenAdminIdentityLegacy)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		at := strings.LastIndex(line, "\t")
		want[line[:at]] = line[at+1:]
	}
	for row, answer := range identityLegacyClosed {
		if _, ok := want[row]; !ok {
			t.Errorf("identityLegacyClosed names %q, which the golden file does not have", row)
		}
		want[row] = answer
	}
	rows := 0
	for _, holder := range identityLegacyNames {
		router := newIdentityGateRouter(t, holder)
		for _, route := range identityGateRoutes {
			row := holder + "\t" + route.method + "\t" + route.path
			rows++
			if got := identityAnswer(t, router, route); got != want[row] {
				t.Errorf("%s = %q, want %q", row, got, want[row])
			}
		}
	}
	if rows != len(want) {
		t.Errorf("drove %d rows, the golden file has %d", rows, len(want))
	}
}

// TestIdentityRoutesAdmitTheirNamesOnly: on every route, a holder of one
// name is refused by the route gate exactly when the gate does not list it.
// A holder the gate admits reaches the handler, which refuses a trust action
// with its own code unless the holder has a trust name. A holder of nothing
// is refused everywhere.
func TestIdentityRoutesAdmitTheirNamesOnly(t *testing.T) {
	holders := append([]string{"", adminaccess.IdentitySSOView, adminaccess.IdentitySSOSettingsManage,
		adminaccess.IdentitySSOTrustManage}, identityLegacyNames...)
	for _, holder := range holders {
		router := newIdentityGateRouter(t, holder)
		for _, route := range identityGateRoutes {
			got := identityAnswer(t, router, route)
			admitted := slices.Contains(route.admits, holder)
			trusted := holder == adminaccess.IdentitySSOTrustManage
			switch {
			case !admitted:
				if got != "403 gate" {
					t.Errorf("holder %q: %s %s = %s, want the route gate's 403", holder, route.method, route.path, got)
				}
			case route.trust && !trusted:
				// Without a database the SCIM client store is not composed,
				// so those handlers answer 503 before their trust check;
				// identity_split_test.go proves the refusal behind it.
				unwired := got == "503" && strings.Contains(route.path, "/scim_clients/")
				if !unwired && !slices.Contains(identityTrustCodes, strings.TrimPrefix(got, "403 ")) {
					t.Errorf("holder %q: %s %s = %s, want the handler's trust refusal", holder, route.method, route.path, got)
				}
			default:
				// Past the identity gates. A binding write still meets the
				// role rule, which refuses with its own code without a
				// database; that is not this story's gate.
				if got == "403 gate" || slices.Contains(identityTrustCodes, strings.TrimPrefix(got, "403 ")) {
					t.Errorf("holder %q: %s %s = %s, want past every identity gate", holder, route.method, route.path, got)
				}
			}
		}
	}
}
