package admin

// The super_admin gate on identity trust (issue 2558, ADR-0059 R3B-1): an
// `admin` who could repoint a provider's issuer, metadata or certificates, or
// mint a SCIM client, could sign in as (or provision into) any account. Each
// refusal is a 403 with a stable code, written before anything is sealed or
// stored, and each refusal and each allowed change names its domain action on
// the request's audit row.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/rolepolicy"
)

// superAdmin and plainAdmin are callers resolved from the database.
var (
	superAdmin = WithPermissionResolver(fixedResolver{rolepolicy.SuperAdminPermission, "runtime.plugins"})
	plainAdmin = WithPermissionResolver(fixedResolver{"runtime.plugins", "admin.auth.users"})
)

// audited runs call with the audit middleware's annotation slot and returns
// the annotation the handler left.
func audited(request *http.Request, call func(http.ResponseWriter, *http.Request)) (*httptest.ResponseRecorder, audit.Annotation) {
	ctx, slot := audit.ContextWithAnnotationSlot(request.Context())
	recorder := httptest.NewRecorder()
	call(recorder, request.WithContext(ctx))
	annotation, _ := slot.Read()
	return recorder, annotation
}

func decodedBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), recorder.Body.String())
	return body
}

func storeWithCorporate() *recordingProviderStore {
	store := newRecordingProviderStore()
	stored, err := identityproviders.Validate(identityproviders.Provider{
		Key: "corporate", Kind: identityproviders.KindOIDC, DisplayName: "Corporate SSO",
		OIDC: &identityproviders.OIDCDocument{
			Issuer: "https://idp.example.com", ClientID: "elitea",
			RedirectURI: "https://elitea.example.com/auth/oidc/callback",
		},
	})
	if err != nil {
		panic(err)
	}
	stored.Enabled, stored.Revision = true, 1
	store.rows["corporate"] = stored
	return store
}

const attackerIssuerBody = `{
	"kind": "oidc",
	"display_name": "Corporate SSO",
	"oidc": {
		"issuer": "https://attacker.example",
		"client_id": "elitea",
		"redirect_uri": "https://elitea.example.com/auth/oidc/callback"
	}
}`

// The acceptance case: an `admin` changing the issuer gets a 403, nothing is
// sealed or stored, and the refusal is on the audit row.
func TestAnAdminCannotChangeAProvidersIssuer(t *testing.T) {
	store, vault := storeWithCorporate(), newRecordingVault()
	handler := NewHandler(nil, WithIdentityProviders(store, vault), plainAdmin)

	recorder, annotation := audited(
		providerRequest(http.MethodPut, "corporate", attackerIssuerBody), handler.IdentityProviderSave)

	require.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
	body := decodedBody(t, recorder)
	require.Equal(t, CodeIdentityTrustSuperAdminRequired, body["code"])
	require.Equal(t, []any{"issuer"}, body["fields"])
	require.Empty(t, store.upserted)
	require.Empty(t, vault.stored)
	require.Equal(t, AuditIdentityTrustRefused, annotation.Action)
	require.Equal(t, "identity_provider", annotation.EntityType)
	require.Equal(t, "corporate", annotation.EntityName)
	require.Equal(t, "https://idp.example.com", store.rows["corporate"].OIDC.Issuer)
}

// Creating a provider is all trust: an `admin` cannot author one.
func TestAnAdminCannotCreateAProvider(t *testing.T) {
	store := newRecordingProviderStore()
	handler := NewHandler(nil, WithIdentityProviders(store, newRecordingVault()), plainAdmin)

	recorder, annotation := audited(providerRequest(http.MethodPut, "corporate", validOIDCBody), handler.IdentityProviderSave)

	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Equal(t, []any{"created"}, decodedBody(t, recorder)["fields"])
	require.Empty(t, store.upserted)
	require.Equal(t, AuditIdentityTrustRefused, annotation.Action)
}

// A SAML certificate added, or a new IdP entity id, is refused the same way.
func TestAnAdminCannotChangeSAMLTrust(t *testing.T) {
	store := newRecordingProviderStore()
	certificate := `"` + testCertificateBody(t) + `"`
	saml := func(entity, certificates string) string {
		return `{"kind":"saml","display_name":"ADFS","saml":{"idp_entity_id":"` + entity +
			`","idp_sso_url":"https://adfs.example.com/sso","idp_certificates":[` + certificates +
			`],"sp_entity_id":"https://elitea.example.com","acs_url":"https://elitea.example.com/auth/saml/acs"}}`
	}
	recorder := httptest.NewRecorder()
	NewHandler(nil, WithIdentityProviders(store, newRecordingVault()), superAdmin).
		IdentityProviderSave(recorder, providerRequest(http.MethodPut, "adfs", saml("urn:adfs", certificate)))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	handler := NewHandler(nil, WithIdentityProviders(store, newRecordingVault()), plainAdmin)
	for name, body := range map[string]string{
		"entity id":   saml("urn:attacker", certificate),
		"certificate": saml("urn:adfs", certificate+`,"`+testCertificateBody(t)+`"`),
	} {
		recorder := httptest.NewRecorder()
		handler.IdentityProviderSave(recorder, providerRequest(http.MethodPut, "adfs", body))
		require.Equal(t, http.StatusForbidden, recorder.Code, "%s: %s", name, recorder.Body.String())
	}
}

// What an `admin` keeps: a save that touches no trust field.
func TestAnAdminStillEditsTheNonTrustFields(t *testing.T) {
	store := storeWithCorporate()
	handler := NewHandler(nil, WithIdentityProviders(store, newRecordingVault()), plainAdmin)
	body := strings.Replace(validOIDCBody, `"Corporate SSO"`, `"Sign in with Corp"`, 1)

	recorder, annotation := audited(providerRequest(http.MethodPut, "corporate", body), handler.IdentityProviderSave)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "Sign in with Corp", store.rows["corporate"].DisplayName)
	require.Empty(t, annotation.Action, "an ordinary edit is not a trust change")
}

// A super_admin changes the issuer: allowed, annotated as a trust change, and
// told which issuer the existing accounts are still bound to.
func TestASuperAdminChangesTheIssuerAndIsToldWhatToRebind(t *testing.T) {
	store := storeWithCorporate()
	handler := NewHandler(nil, WithIdentityProviders(store, newRecordingVault()), superAdmin)

	recorder, annotation := audited(
		providerRequest(http.MethodPut, "corporate", attackerIssuerBody), handler.IdentityProviderSave)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "https://idp.example.com", decodedBody(t, recorder)["previous_issuer"])
	require.Equal(t, AuditIdentityTrustChanged, annotation.Action)
	require.Equal(t, "corporate", annotation.EntityName)
}

// A caller whose own permissions cannot be read is refused, never allowed.
func TestTheTrustGateFailsClosed(t *testing.T) {
	handler := NewHandler(nil, WithIdentityProviders(storeWithCorporate(), newRecordingVault()))
	recorder := httptest.NewRecorder()
	handler.IdentityProviderSave(recorder, providerRequest(http.MethodPut, "corporate", attackerIssuerBody))
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Equal(t, rolepolicy.CodeCallerUnresolved, decodedBody(t, recorder)["code"])
}

func TestTrustChangesNamesOnlyTheWideningDirection(t *testing.T) {
	base := identityproviders.Provider{Key: "k", Kind: identityproviders.KindOIDC, OIDC: &identityproviders.OIDCDocument{
		Issuer: "https://a", ClientID: "c", RequireEmailVerified: true, EntraTenants: []string{"t1", "t2"},
	}}
	next := func(edit func(*identityproviders.OIDCDocument)) identityproviders.Provider {
		document := *base.OIDC
		edit(&document)
		return identityproviders.Provider{Key: "k", Kind: identityproviders.KindOIDC, OIDC: &document}
	}
	cases := map[string]struct {
		after identityproviders.Provider
		want  []string
	}{
		"nothing":          {next(func(*identityproviders.OIDCDocument) {}), nil},
		"issuer":           {next(func(d *identityproviders.OIDCDocument) { d.Issuer = "https://b" }), []string{"issuer"}},
		"client":           {next(func(d *identityproviders.OIDCDocument) { d.ClientID = "x" }), []string{"client_id"}},
		"tenant removed":   {next(func(d *identityproviders.OIDCDocument) { d.EntraTenants = []string{"t2"} }), nil},
		"tenant added":     {next(func(d *identityproviders.OIDCDocument) { d.EntraTenants = []string{"t1", "t3"} }), []string{"entra_tenants"}},
		"verify relaxed":   {next(func(d *identityproviders.OIDCDocument) { d.RequireEmailVerified = false }), []string{"require_email_verified"}},
		"adoption enabled": {next(func(d *identityproviders.OIDCDocument) { d.AdoptSCIMUsers = true }), []string{"adopt_scim_users"}},
		"login domains":    {next(func(d *identityproviders.OIDCDocument) { d.LoginDomains = []string{"corp.example"} }), nil},
		"redirect":         {next(func(d *identityproviders.OIDCDocument) { d.RedirectURI = "https://evil/cb" }), []string{"redirect_uri"}},
		"token file":       {next(func(d *identityproviders.OIDCDocument) { d.EntraFederatedTokenFile = "/x" }), []string{"entra_federated_token_file"}},
		"api scope":        {next(func(d *identityproviders.OIDCDocument) { d.EntraAPIScope = "api://x/.default" }), []string{"entra_api_scope"}},
	}
	for name, testCase := range cases {
		require.Equal(t, testCase.want, identityproviders.TrustChanges(base, testCase.after), name)
	}
	require.Equal(t, []string{"created"}, identityproviders.TrustChanges(identityproviders.Provider{}, base))

	// Turning a provider on retires the live one of its kind; turning it off
	// stops sign-in. Either way it decides which identity provider is believed.
	enabled := base
	enabled.Enabled = true
	require.Equal(t, []string{"enabled"}, identityproviders.TrustChanges(base, enabled))
	require.Equal(t, []string{"enabled"}, identityproviders.TrustChanges(enabled, base))
}

func TestSAMLTrustChangesCoverTheServiceProviderSide(t *testing.T) {
	base := identityproviders.Provider{Key: "k", Kind: identityproviders.KindSAML, SAML: &identityproviders.SAMLDocument{
		IDPEntityID: "urn:idp", SPEntityID: "urn:sp", ACSURL: "https://e/acs", ClockSkewSeconds: 60,
	}}
	next := func(edit func(*identityproviders.SAMLDocument)) identityproviders.Provider {
		document := *base.SAML
		edit(&document)
		return identityproviders.Provider{Key: "k", Kind: identityproviders.KindSAML, SAML: &document}
	}
	for name, testCase := range map[string]struct {
		after identityproviders.Provider
		want  []string
	}{
		"audience":     {next(func(d *identityproviders.SAMLDocument) { d.SPEntityID = "urn:other" }), []string{"sp_entity_id"}},
		"acs":          {next(func(d *identityproviders.SAMLDocument) { d.ACSURL = "https://evil/acs" }), []string{"acs_url"}},
		"nameid":       {next(func(d *identityproviders.SAMLDocument) { d.NameIDFormat = "email" }), []string{"name_id_format"}},
		"signing":      {next(func(d *identityproviders.SAMLDocument) { d.SignAuthnRequests = true }), []string{"sign_authn_requests"}},
		"skew raised":  {next(func(d *identityproviders.SAMLDocument) { d.ClockSkewSeconds = 86400 }), []string{"clock_skew_seconds"}},
		"skew lowered": {next(func(d *identityproviders.SAMLDocument) { d.ClockSkewSeconds = 30 }), nil},
		"name attr":    {next(func(d *identityproviders.SAMLDocument) { d.NameAttribute = "cn" }), nil},
	} {
		require.Equal(t, testCase.want, identityproviders.TrustChanges(base, testCase.after), name)
	}
}

// An `admin` cannot switch which provider is live, nor replace its secret:
// enabling retires the live provider of the kind, and the secret is a
// credential of the trust relationship.
func TestAnAdminCannotEnableAProviderOrReplaceItsSecret(t *testing.T) {
	for name, body := range map[string]string{
		"disable": strings.Replace(validOIDCBody, `"kind": "oidc",`, `"kind": "oidc", "enabled": false,`, 1),
		"secret":  strings.Replace(validOIDCBody, `"kind": "oidc",`, `"kind": "oidc", "secret": "replaced",`, 1),
	} {
		store := storeWithCorporate()
		vault := newRecordingVault()
		handler := NewHandler(nil, WithIdentityProviders(store, vault), plainAdmin)
		recorder, annotation := audited(providerRequest(http.MethodPut, "corporate", body), handler.IdentityProviderSave)
		require.Equal(t, http.StatusForbidden, recorder.Code, name+": "+recorder.Body.String())
		require.Equal(t, AuditIdentityTrustRefused, annotation.Action, name)
		require.Empty(t, store.upserted, name)
		require.True(t, store.rows["corporate"].Enabled, name)
	}
}

// A save decided against a row that changed in between is refused, so it
// cannot silently revert a super_admin's trust change.
func TestASaveOverANewerRevisionIsRefused(t *testing.T) {
	store := storeWithCorporate()
	stale := &staleLookupStore{recordingProviderStore: store, revision: store.rows["corporate"].Revision}
	current := store.rows["corporate"]
	current.Revision++
	store.rows["corporate"] = current
	handler := NewHandler(nil, WithIdentityProviders(stale, newRecordingVault()), plainAdmin)
	body := strings.Replace(validOIDCBody, `"Corporate SSO"`, `"Sign in with Corp"`, 1)

	recorder, _ := audited(providerRequest(http.MethodPut, "corporate", body), handler.IdentityProviderSave)

	require.Equal(t, http.StatusConflict, recorder.Code, recorder.Body.String())
	require.Empty(t, store.upserted)
}

// staleLookupStore answers Lookup with the revision the caller read before a
// concurrent save moved the row on.
type staleLookupStore struct {
	*recordingProviderStore
	revision int
}

func (s *staleLookupStore) Lookup(ctx context.Context, key string) (identityproviders.Provider, error) {
	provider, err := s.recordingProviderStore.Lookup(ctx, key)
	provider.Revision = s.revision
	return provider, err
}

/* ── SCIM clients ──────────────────────────────────────────────────────── */

// An `admin` cannot create, rotate or delete a SCIM client; each refusal is
// on the audit row and reaches no store. Listing and revoking stay open.
func TestAnAdminCannotMintRotateOrDeleteASCIMClient(t *testing.T) {
	store := &stubSCIMClientStore{}
	router := scimClientRouter(NewHandler(nil, WithSCIMClients(store), plainAdmin))
	for _, call := range []struct{ method, target, body string }{
		{http.MethodPost, "/scim_clients/administration", `{"name":"entra","auth_method":"bearer"}`},
		{http.MethodPost, "/scim_clients/administration/1/rotate", ""},
		{http.MethodDelete, "/scim_clients/administration/1", ""},
	} {
		request := httptest.NewRequest(call.method, call.target, strings.NewReader(call.body))
		ctx, slot := audit.ContextWithAnnotationSlot(request.Context())
		request = request.WithContext(ctx)
		response := callSCIMClientsWith(router, request)
		require.Equal(t, http.StatusForbidden, response.Code, "%s %s", call.method, call.target)
		require.Equal(t, CodeSCIMClientSuperAdminRequired, decodedBody(t, response)["code"])
		annotation, _ := slot.Read()
		require.Equal(t, AuditSCIMClientRefused, annotation.Action)
	}
	require.Empty(t, store.clients, "a refused create reached the store")

	require.Equal(t, http.StatusOK, callSCIMClients(router, http.MethodGet, "/scim_clients/administration", "").Code)
}

// A super_admin mints one, and the audit row names the change.
func TestASuperAdminMintsASCIMClientAndTheChangeIsAudited(t *testing.T) {
	router := scimClientRouter(NewHandler(nil, WithSCIMClients(&stubSCIMClientStore{}), superAdmin))
	request := httptest.NewRequest(http.MethodPost, "/scim_clients/administration",
		strings.NewReader(`{"name":"entra","auth_method":"bearer"}`))
	ctx, slot := audit.ContextWithAnnotationSlot(request.Context())
	response := callSCIMClientsWith(router, request.WithContext(ctx))
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	annotation, _ := slot.Read()
	require.Equal(t, AuditSCIMClientCreated, annotation.Action)
	require.Equal(t, "scim_client", annotation.EntityType)
}

/* ── re-bind ───────────────────────────────────────────────────────────── */

// Only a super_admin re-binds accounts, and the refusal is audited. The gate
// answers before the database is needed.
func TestAnAdminCannotRebindAccounts(t *testing.T) {
	handler := NewHandler(nil, WithIdentityProviders(storeWithCorporate(), newRecordingVault()), plainAdmin)
	recorder, annotation := audited(
		providerRequest(http.MethodPost, "corporate", `{"from_issuer":"https://idp.example.com"}`),
		handler.IdentityProviderRebind)
	require.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
	require.Equal(t, CodeIdentityRebindSuperAdminRequired, decodedBody(t, recorder)["code"])
	require.Equal(t, AuditIdentityRebindRefused, annotation.Action)
	require.Equal(t, "corporate", annotation.EntityName)
}

func TestTheRebindBodyNamesExactlyOneSource(t *testing.T) {
	for name, testCase := range map[string]struct {
		body string
		ok   bool
	}{
		"issuer":          {`{"from_issuer":"https://old.example"}`, true},
		"environment":     {`{"from_provider_key":"@environment","from_issuer":"https://dex"}`, true},
		"unbound":         {`{"from_unbound":true}`, true},
		"nothing":         {`{}`, false},
		"both":            {`{"from_unbound":true,"from_issuer":"https://old.example"}`, false},
		"unknown field":   {`{"to_issuer":"https://x"}`, false},
		"malformed":       {`{`, false},
		"key without iss": {`{"from_provider_key":"other"}`, false},
		"punctuation key": {`{"from_provider_key":"!!!","from_issuer":"https://old.example"}`, false},
		"long key":        {`{"from_provider_key":"` + strings.Repeat("k", 65) + `","from_issuer":"https://old.example"}`, false},
		"long issuer":     {`{"from_issuer":"https://` + strings.Repeat("i", 2048) + `"}`, false},
	} {
		recorder := httptest.NewRecorder()
		request, ok := decodeIdentityRebind(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(testCase.body)), "corporate")
		require.Equal(t, testCase.ok, ok, name)
		if !ok {
			require.Equal(t, http.StatusBadRequest, recorder.Code, name)
			continue
		}
		switch name {
		case "issuer":
			require.Equal(t, "corporate", request.From.Key)
		case "environment":
			require.Equal(t, "@environment", request.From.Key)
		case "unbound":
			require.Nil(t, request.From)
		}
	}
}

// callSCIMClientsWith serves a prepared request as the caller callSCIMClients uses.
func callSCIMClientsWith(router http.Handler, request *http.Request) *httptest.ResponseRecorder {
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{
		ID: "42", UserID: "42", Email: "admin@test.local", AuthType: "session",
	}))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
