package admin_test

// Identity split by action (ADR-0059 R0.4c).
//
// Each route is mounted behind the gate router.go gives it (the same
// RequireCentralPermissions with the same names; the router-level table in
// internal/api/admin_identity_gates_test.go holds the router to them), and
// the caller's administration set is exactly the names a test grants. So a
// 403 here is either the route gate's or the handler's trust refusal, and the
// tests say which.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/scimclient"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/scimdirectory"
)

// splitMarker is a secret value no response may carry, except the one-time
// answer of the create that minted it.
const splitMarker = "R04C-MARKER-SECRET-7f3a9"

// The holders.
var (
	settingsOnly = []string{adminaccess.IdentitySSOSettingsManage}
	viewOnly     = []string{adminaccess.IdentitySSOView}
	// The built-in `admin` role's names on these routes: every legacy name,
	// no trust name.
	plainAdministrator = []string{
		adminaccess.RuntimePlugins, adminaccess.AdminAuthUsers,
		adminaccess.ConfigurationRolesUserProjectPermsView, adminaccess.ConfigurationRolesUserProjectPermsEdit,
		adminaccess.IdentitySSOView, adminaccess.IdentitySSOSettingsManage,
	}
	globalAdministrator = append([]string{adminaccess.SuperAdminPermission, adminaccess.IdentitySSOTrustManage},
		plainAdministrator...)
)

// splitResolver grants exactly the names in every mode.
type splitResolver []string

func (s splitResolver) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 7, Permissions: append([]string(nil), s...)}, nil
}

/* ── fakes ─────────────────────────────────────────────────────────────── */

type splitProviderStore struct{ rows map[string]identityproviders.Provider }

func (s *splitProviderStore) List(context.Context) ([]identityproviders.Provider, error) {
	out := []identityproviders.Provider{}
	for _, row := range s.rows {
		out = append(out, row)
	}
	return out, nil
}

func (s *splitProviderStore) Lookup(_ context.Context, key string) (identityproviders.Provider, error) {
	row, ok := s.rows[key]
	if !ok {
		return identityproviders.Provider{}, identityproviders.ErrNotFound
	}
	return row, nil
}

func (s *splitProviderStore) UpsertAtRevision(
	_ context.Context, provider identityproviders.Provider, expected int,
) (identityproviders.Provider, error) {
	if s.rows[provider.Key].Revision != expected {
		return identityproviders.Provider{}, identityproviders.ErrRevisionConflict
	}
	provider.Revision = expected + 1
	s.rows[provider.Key] = provider
	return provider, nil
}

func (s *splitProviderStore) Delete(_ context.Context, key string) (string, error) {
	row, ok := s.rows[key]
	if !ok {
		return "", identityproviders.ErrNotFound
	}
	delete(s.rows, key)
	return row.SecretRef, nil
}

type splitVault struct{ values map[string]string }

func (v *splitVault) StoreAdminHiddenSecret(_ context.Context, name, value string) error {
	v.values[name] = value
	return nil
}

func (v *splitVault) DeleteAdminHiddenSecret(_ context.Context, name string) error {
	delete(v.values, name)
	return nil
}

type splitSCIMClients struct{ clients []scimclient.Client }

func (s *splitSCIMClients) List(context.Context) ([]scimclient.Client, error) { return s.clients, nil }

func (s *splitSCIMClients) Create(
	_ context.Context, name, method string, _ *time.Time, _ *int,
) (scimclient.Issued, error) {
	client := scimclient.Client{ID: int64(len(s.clients) + 1), Name: name, AuthMethod: method,
		ClientID: "scim-" + name, SecretHint: "…9a", CreatedAt: time.Unix(1, 0)}
	s.clients = append(s.clients, client)
	return scimclient.Issued{Client: client, Secret: splitMarker}, nil
}

func (s *splitSCIMClients) Rotate(_ context.Context, id int64) (scimclient.Issued, error) {
	return scimclient.Issued{Client: scimclient.Client{ID: id}, Secret: splitMarker}, nil
}

func (s *splitSCIMClients) Revoke(_ context.Context, id int64) (scimclient.Client, error) {
	now := time.Unix(2, 0)
	return scimclient.Client{ID: id, RevokedAt: &now}, nil
}

func (s *splitSCIMClients) Delete(context.Context, int64) error { return nil }
func (s *splitSCIMClients) AccessTokenTTL() time.Duration       { return time.Hour }

type splitBindings struct{}

func (splitBindings) ListGroups(context.Context, scimdirectory.Filter, int, int) ([]scimdirectory.Group, int, error) {
	return []scimdirectory.Group{{ID: 1, DisplayName: "Team", ProjectID: 1, RoleName: "viewer"}}, 1, nil
}

func (splitBindings) ProjectRoleNames(context.Context, int) ([]string, error) {
	return []string{"viewer"}, nil
}

func (splitBindings) CreateBinding(context.Context, string, int, string) (scimdirectory.Group, error) {
	return scimdirectory.Group{ID: 2}, nil
}

func (splitBindings) UpdateBinding(context.Context, int64, string, int, string) (scimdirectory.Group, error) {
	return scimdirectory.Group{ID: 1}, nil
}

func (splitBindings) DeleteGroup(context.Context, int64) error { return nil }

/* ── the routes ────────────────────────────────────────────────────────── */

// splitEnvironment is one deployment: one provider store, vault and SCIM
// client store, shared by every caller's router.
type splitEnvironment struct {
	providers *splitProviderStore
	vault     *splitVault
	clients   *splitSCIMClients
}

const splitCertificateKey = "corporate-saml"

func newSplitEnvironment(t *testing.T) splitEnvironment {
	t.Helper()
	oidc, err := identityproviders.Validate(identityproviders.Provider{
		Key: "corporate", Kind: identityproviders.KindOIDC, DisplayName: "Corporate SSO",
		OIDC: &identityproviders.OIDCDocument{
			Issuer: "https://idp.example.com", ClientID: "elitea",
			RedirectURI: "https://elitea.example.com/auth/oidc/callback",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	oidc.Enabled, oidc.Revision, oidc.SecretRef = true, 1, splitMarker
	saml, err := identityproviders.Validate(identityproviders.Provider{
		Key: splitCertificateKey, Kind: identityproviders.KindSAML, DisplayName: "Corporate SAML",
		SAML: samlDocument(t, splitCertificate(t)),
	})
	if err != nil {
		t.Fatal(err)
	}
	saml.Revision = 1
	return splitEnvironment{
		providers: &splitProviderStore{rows: map[string]identityproviders.Provider{"corporate": oidc, splitCertificateKey: saml}},
		vault:     &splitVault{values: map[string]string{}},
		clients:   &splitSCIMClients{},
	}
}

// as mounts the identity routes for a caller who holds exactly names, each
// behind its router.go gate.
func (e splitEnvironment) as(names []string) chi.Router {
	resolver := splitResolver(names)
	handler := admin.NewHandler(nil,
		admin.WithPermissionResolver(resolver),
		admin.WithIdentityProviders(e.providers, e.vault),
		admin.WithSCIMClients(e.clients),
		admin.WithSCIMGroupBindings(splitBindings{}))
	return identityRoutes(handler, resolver, "7")
}

// identityRoutes mounts the identity routes with the gates of router.go.
func identityRoutes(handler *admin.Handler, resolver auth.PermissionResolver, userID string) chi.Router {
	central := func(names ...string) func(http.Handler) http.Handler {
		return apimw.RequireCentralPermissions(resolver, auth.PermissionModeAdministration, names...)
	}
	providersView := central(adminaccess.IdentitySSOView, adminaccess.RuntimePlugins)
	providersEdit := central(adminaccess.IdentitySSOSettingsManage, adminaccess.IdentitySSOTrustManage,
		adminaccess.RuntimePlugins)
	clientsView := central(adminaccess.IdentitySSOView, adminaccess.AdminAuthUsers)
	clientsEdit := central(adminaccess.IdentitySSOSettingsManage, adminaccess.IdentitySSOTrustManage,
		adminaccess.AdminAuthUsers)
	bindingsView := central(adminaccess.IdentitySSOView, admin.UserProjectPermissionsViewPermission)
	bindingsEdit := central(adminaccess.IdentitySSOSettingsManage, admin.UserProjectPermissionsEditPermission)

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: userID, UserID: userID})))
		})
	})
	r.With(providersView).Get("/identity_providers/administration", handler.IdentityProviderList)
	r.With(providersEdit).Put("/identity_providers/administration/{key}", handler.IdentityProviderSave)
	r.With(providersEdit).Delete("/identity_providers/administration/{key}", handler.IdentityProviderDelete)
	r.With(providersEdit).Post("/identity_providers/administration/{key}/rebind", handler.IdentityProviderRebind)
	r.With(clientsView).Get("/scim_clients/administration", handler.SCIMClientList)
	r.With(clientsEdit).Post("/scim_clients/administration", handler.SCIMClientCreate)
	r.With(clientsEdit).Post("/scim_clients/administration/{id}/rotate", handler.SCIMClientRotate)
	r.With(clientsEdit).Post("/scim_clients/administration/{id}/revoke", handler.SCIMClientRevoke)
	r.With(clientsEdit).Delete("/scim_clients/administration/{id}", handler.SCIMClientDelete)
	r.With(bindingsView).Get("/scim_group_bindings/administration", handler.SCIMGroupBindingList)
	r.With(bindingsView).Get("/scim_group_bindings/administration/project_roles/{projectID}",
		handler.SCIMGroupBindingProjectRoles)
	r.With(bindingsEdit).Post("/scim_group_bindings/administration", handler.SCIMGroupBindingCreate)
	r.With(bindingsEdit).Put("/scim_group_bindings/administration/{id}", handler.SCIMGroupBindingSave)
	r.With(bindingsEdit).Delete("/scim_group_bindings/administration/{id}", handler.SCIMGroupBindingDelete)
	return r
}

func splitDo(router http.Handler, method, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// refusal answers "gate" for the route gate's bare 403, the code for the
// handler's, and "" for anything but a 403.
func refusal(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	if recorder.Code != http.StatusForbidden {
		return ""
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	if body.Code == "" {
		return "gate"
	}
	return body.Code
}

/* ── the bodies ────────────────────────────────────────────────────────── */

func oidcBody(displayName, issuer string) string {
	return fmt.Sprintf(`{"kind":"oidc","display_name":%q,"oidc":{"issuer":%q,"client_id":"elitea",`+
		`"redirect_uri":"https://elitea.example.com/auth/oidc/callback"}}`, displayName, issuer)
}

func samlDocument(t *testing.T, certificates ...string) *identityproviders.SAMLDocument {
	t.Helper()
	return &identityproviders.SAMLDocument{
		IDPEntityID: "https://idp.example.com/saml", IDPSSOURL: "https://idp.example.com/sso",
		IDPCertificates: certificates,
		SPEntityID:      "https://elitea.example.com/saml", ACSURL: "https://elitea.example.com/auth/saml/acs",
	}
}

func samlBody(t *testing.T, document *identityproviders.SAMLDocument) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"kind": "saml", "display_name": "Corporate SAML", "saml": document})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func splitCertificate(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "split-idp"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return "-----BEGIN CERTIFICATE-----\n" + base64.StdEncoding.EncodeToString(der) + "\n-----END CERTIFICATE-----"
}

/* ── the tests ─────────────────────────────────────────────────────────── */

// A holder of only identity.sso.settings.manage renames a provider, and is
// refused (by the handler, with the trust code) a change of its issuer, of
// its metadata address and of its certificates, and a SCIM client create. An
// `admin` gets the same four refusals.
func TestTheSettingsHalfRenamesAndCannotChangeTrust(t *testing.T) {
	for name, holder := range map[string][]string{"settings only": settingsOnly, "admin": plainAdministrator} {
		t.Run(name, func(t *testing.T) {
			env := newSplitEnvironment(t)
			router := env.as(holder)
			const providers = "/identity_providers/administration/"

			renamed := splitDo(router, http.MethodPut, providers+"corporate",
				oidcBody("Sign in with Corporate", "https://idp.example.com"))
			if renamed.Code != http.StatusOK {
				t.Fatalf("rename = %d %s, want 200", renamed.Code, renamed.Body)
			}
			if got := env.providers.rows["corporate"].DisplayName; got != "Sign in with Corporate" {
				t.Fatalf("stored display name = %q", got)
			}

			stored := env.providers.rows[splitCertificateKey]
			issuer := *stored.SAML
			issuer.IDPEntityID = "https://attacker.example/saml"
			certificate := *stored.SAML
			certificate.IDPCertificates = append([]string{splitCertificate(t)}, stored.SAML.IDPCertificates...)
			for change, call := range map[string]struct{ key, body string }{
				// A SAML provider's issuer is its IdP entity id.
				"issuer": {splitCertificateKey, samlBody(t, &issuer)},
				// An OIDC provider's issuer is also its discovery (metadata)
				// address.
				"metadata URL": {"corporate", oidcBody("Sign in with Corporate", "https://attacker.example")},
				"certificate":  {splitCertificateKey, samlBody(t, &certificate)},
			} {
				recorder := splitDo(router, http.MethodPut, providers+call.key, call.body)
				if got := refusal(t, recorder); got != admin.CodeIdentityTrustSuperAdminRequired {
					t.Errorf("%s change = %d %q, want 403 %s", change, recorder.Code, got,
						admin.CodeIdentityTrustSuperAdminRequired)
				}
			}
			if env.providers.rows[splitCertificateKey].Revision != 1 {
				t.Error("a refused trust change reached the store")
			}

			created := splitDo(router, http.MethodPost, "/scim_clients/administration",
				`{"name":"entra","auth_method":"bearer"}`)
			if got := refusal(t, created); got != admin.CodeSCIMClientSuperAdminRequired {
				t.Errorf("SCIM client create = %d %q, want 403 %s", created.Code, got, admin.CodeSCIMClientSuperAdminRequired)
			}
			if len(env.clients.clients) != 0 {
				t.Error("a refused SCIM client create reached the store")
			}
		})
	}
}

// The other trust actions are refused to the settings half too, and a revoke
// is a setting.
func TestTheSettingsHalfCannotDeleteRebindRotateOrToggle(t *testing.T) {
	env := newSplitEnvironment(t)
	router := env.as(settingsOnly)
	for _, call := range []struct{ method, target, body, code string }{
		{http.MethodDelete, "/identity_providers/administration/corporate", "", admin.CodeIdentityTrustSuperAdminRequired},
		{http.MethodPut, "/identity_providers/administration/corporate",
			`{"kind":"oidc","display_name":"Corporate SSO","enabled":false,"oidc":{"issuer":"https://idp.example.com",` +
				`"client_id":"elitea","redirect_uri":"https://elitea.example.com/auth/oidc/callback"}}`,
			admin.CodeIdentityTrustSuperAdminRequired},
		{http.MethodPut, "/identity_providers/administration/new-provider",
			oidcBody("New", "https://idp.example.com"), admin.CodeIdentityTrustSuperAdminRequired},
		{http.MethodPost, "/identity_providers/administration/corporate/rebind",
			`{"from_issuer":"https://old.example.com"}`, admin.CodeIdentityRebindSuperAdminRequired},
		{http.MethodPost, "/scim_clients/administration/1/rotate", "", admin.CodeSCIMClientSuperAdminRequired},
		{http.MethodDelete, "/scim_clients/administration/1", "", admin.CodeSCIMClientSuperAdminRequired},
	} {
		recorder := splitDo(router, call.method, call.target, call.body)
		if got := refusal(t, recorder); got != call.code {
			t.Errorf("%s %s = %d %q, want 403 %s", call.method, call.target, recorder.Code, got, call.code)
		}
	}
	if _, ok := env.providers.rows["corporate"]; !ok {
		t.Error("a refused delete removed the provider")
	}
	if !env.providers.rows["corporate"].Enabled {
		t.Error("a refused disable reached the store")
	}
	if revoked := splitDo(router, http.MethodPost, "/scim_clients/administration/1/revoke", ""); revoked.Code != http.StatusOK {
		t.Errorf("SCIM client revoke = %d %s, want 200", revoked.Code, revoked.Body)
	}
}

// The trust half: identity.sso.trust.manage or super_admin opens every trust
// action. The registry seeds both on super_admin alone.
func TestTheTrustHalfOpensTrustActions(t *testing.T) {
	env := newSplitEnvironment(t)
	router := env.as(globalAdministrator)
	if recorder := splitDo(router, http.MethodPut, "/identity_providers/administration/corporate",
		oidcBody("Corporate SSO", "https://idp2.example.com")); recorder.Code != http.StatusOK {
		t.Errorf("issuer change = %d %s, want 200", recorder.Code, recorder.Body)
	}
	if recorder := splitDo(router, http.MethodPost, "/scim_clients/administration",
		`{"name":"entra","auth_method":"bearer"}`); recorder.Code != http.StatusCreated {
		t.Errorf("SCIM client create = %d %s, want 201", recorder.Code, recorder.Body)
	}
	if recorder := splitDo(router, http.MethodDelete, "/identity_providers/administration/corporate", ""); recorder.Code != http.StatusOK {
		t.Errorf("delete = %d %s, want 200", recorder.Code, recorder.Body)
	}

	// The trust name without super_admin, the shape an elevation would carry.
	trustOnly := newSplitEnvironment(t)
	if recorder := splitDo(trustOnly.as([]string{adminaccess.IdentitySSOTrustManage}), http.MethodPut,
		"/identity_providers/administration/corporate",
		oidcBody("Corporate SSO", "https://idp2.example.com")); recorder.Code != http.StatusOK {
		t.Errorf("issuer change by identity.sso.trust.manage = %d %s, want 200", recorder.Code, recorder.Body)
	}
}

// A holder of only identity.sso.view lists providers, SCIM clients and
// bindings, is refused every write by the route gate, and no answer carries a
// client secret or a SCIM secret.
func TestTheViewHalfReadsAndWritesNothing(t *testing.T) {
	env := newSplitEnvironment(t)
	// A global administrator seals a provider secret and mints a SCIM client,
	// both carrying the marker. The mint's own answer carries the secret
	// once, by design.
	global := env.as(globalAdministrator)
	sealed := splitDo(global, http.MethodPut, "/identity_providers/administration/corporate",
		`{"kind":"oidc","display_name":"Corporate SSO","secret":"`+splitMarker+`","oidc":{"issuer":"https://idp.example.com",`+
			`"client_id":"elitea","redirect_uri":"https://elitea.example.com/auth/oidc/callback"}}`)
	if sealed.Code != http.StatusOK || strings.Contains(sealed.Body.String(), splitMarker) {
		t.Fatalf("seal = %d %s, want 200 without the secret", sealed.Code, sealed.Body)
	}
	if minted := splitDo(global, http.MethodPost, "/scim_clients/administration",
		`{"name":"entra","auth_method":"bearer"}`); minted.Code != http.StatusCreated {
		t.Fatalf("mint = %d %s", minted.Code, minted.Body)
	}

	router := env.as(viewOnly)
	for _, target := range []string{
		"/identity_providers/administration",
		"/scim_clients/administration",
		"/scim_group_bindings/administration",
		"/scim_group_bindings/administration/project_roles/1",
	} {
		recorder := splitDo(router, http.MethodGet, target, "")
		if recorder.Code != http.StatusOK {
			t.Errorf("GET %s = %d %s, want 200", target, recorder.Code, recorder.Body)
		}
		if strings.Contains(recorder.Body.String(), splitMarker) {
			t.Errorf("GET %s answers the secret: %s", target, recorder.Body)
		}
	}
	if !strings.Contains(splitDo(router, http.MethodGet, "/scim_clients/administration", "").Body.String(), `"entra"`) {
		t.Error("the SCIM client listing does not show the minted client")
	}

	for _, call := range []struct{ method, target, body string }{
		{http.MethodPut, "/identity_providers/administration/corporate", oidcBody("Renamed", "https://idp.example.com")},
		{http.MethodDelete, "/identity_providers/administration/corporate", ""},
		{http.MethodPost, "/identity_providers/administration/corporate/rebind", `{"from_issuer":"https://old.example.com"}`},
		{http.MethodPost, "/scim_clients/administration", `{"name":"entra","auth_method":"bearer"}`},
		{http.MethodPost, "/scim_clients/administration/1/rotate", ""},
		{http.MethodPost, "/scim_clients/administration/1/revoke", ""},
		{http.MethodDelete, "/scim_clients/administration/1", ""},
		{http.MethodPost, "/scim_group_bindings/administration", `{"display_name":"Team","project_id":1,"role_name":"viewer"}`},
		{http.MethodPut, "/scim_group_bindings/administration/1", `{"display_name":"Team","project_id":1,"role_name":"viewer"}`},
		{http.MethodDelete, "/scim_group_bindings/administration/1", ""},
	} {
		recorder := splitDo(router, call.method, call.target, call.body)
		if got := refusal(t, recorder); got != "gate" {
			t.Errorf("%s %s = %d %q, want the route gate's 403", call.method, call.target, recorder.Code, got)
		}
		if strings.Contains(recorder.Body.String(), splitMarker) {
			t.Errorf("%s %s answers the secret", call.method, call.target)
		}
	}
}

// A holder of only identity.sso.settings.manage who is not a member of the
// project is refused a group binding there: the central name opens the route,
// and the role rule of #2555 still decides the binding.
func TestTheSettingsHalfCannotBindARoleItDoesNotHold(t *testing.T) {
	pool := newRolesPool(t)
	prepareRolesFixture(t, pool)
	t.Setenv("AI_PROJECT_ID", fmt.Sprint(publicProjectID))
	ctx := context.Background()
	for _, file := range []string{
		"../../../../migrations/shared/0096_scim_provisioning.sql",
		"../../../../migrations/shared/0098_scim_group_bindings.sql",
	} {
		migration, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("apply %s: %v", file, err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('sso_settings', 'administration')`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT id, 'identity.sso.settings.manage' FROM public.auth_core__role
			WHERE name = 'sso_settings' AND mode = 'administration'`,
		`INSERT INTO public.auth_core__user (email, name) VALUES ('sso-settings@autotest.local', 'SSO')`,
		`INSERT INTO public.auth_core__user_role (user_id, role_id)
			SELECT account.id, role.id FROM public.auth_core__user account, public.auth_core__role role
			WHERE account.email = 'sso-settings@autotest.local' AND role.name = 'sso_settings'
			  AND role.mode = 'administration'`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	id := fmt.Sprint(userID(t, pool, "sso-settings@autotest.local"))
	resolver := legacyrbac.NewPostgresResolver(pool)
	handler := admin.NewHandler(pool, admin.WithPermissionResolver(resolver),
		admin.WithSCIMGroupBindings(scimdirectory.NewStore(pool)))
	router := identityRoutes(handler, resolver, id)

	recorder := splitDo(router, http.MethodPost, "/scim_group_bindings/administration",
		fmt.Sprintf(`{"display_name":"Team viewer","project_id":%d,"role_name":"viewer"}`, publicProjectID))
	expectRefusal(t, recorder, adminaccess.CodePermissionNotHeld)
	var bindings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_auth.scim_group_bindings`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 {
		t.Fatalf("a refused binding was written: %d rows", bindings)
	}
}
