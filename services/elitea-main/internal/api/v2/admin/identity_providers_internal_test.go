package admin

// What these tests hold in place.
//
// This surface decides how a deployment authenticates people, and its two
// dangerous failure modes are both silent: a refused save that has already
// overwritten a live credential, and a read that hands a client secret back to
// whoever can open the admin page. Every test below names the one it stops.
//
// The store is a FAKE that records what it was asked to do. The point of these
// tests is an ORDER — which refusals happen before the database is read and
// before the vault is written — and a store that answered "no pool" to
// everything could only prove the refusals that come first.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/identityproviders"
)

// providerRequest builds a request whose chi context carries {key}.
func providerRequest(method, key, body string) *http.Request {
	req := httptest.NewRequest(method, "/identity_providers/administration/"+key, strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("key", key)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
}

// recordingProviderStore answers from `rows` and remembers every write, so a
// test can assert that a refused save reached neither the vault nor the table.
type recordingProviderStore struct {
	rows      map[string]identityproviders.Provider
	upserted  []identityproviders.Provider
	deleted   []string
	lookupErr error
	upsertErr error
}

func newRecordingProviderStore() *recordingProviderStore {
	return &recordingProviderStore{rows: map[string]identityproviders.Provider{}}
}

func (s *recordingProviderStore) List(context.Context) ([]identityproviders.Provider, error) {
	providers := make([]identityproviders.Provider, 0, len(s.rows))
	for _, provider := range s.rows {
		providers = append(providers, provider)
	}
	return providers, nil
}

func (s *recordingProviderStore) Lookup(_ context.Context, key string) (identityproviders.Provider, error) {
	if s.lookupErr != nil {
		return identityproviders.Provider{}, s.lookupErr
	}
	provider, ok := s.rows[key]
	if !ok {
		return identityproviders.Provider{}, identityproviders.ErrNotFound
	}
	return provider, nil
}

func (s *recordingProviderStore) Upsert(
	_ context.Context,
	provider identityproviders.Provider,
) (identityproviders.Provider, error) {
	if s.upsertErr != nil {
		return identityproviders.Provider{}, s.upsertErr
	}
	stored, err := identityproviders.Validate(provider)
	if err != nil {
		return identityproviders.Provider{}, err
	}
	stored.Revision = s.rows[stored.Key].Revision + 1
	stored.SecretRef = provider.SecretRef
	s.rows[stored.Key] = stored
	s.upserted = append(s.upserted, stored)
	return stored, nil
}

func (s *recordingProviderStore) Delete(_ context.Context, key string) (string, error) {
	provider, ok := s.rows[key]
	if !ok {
		return "", identityproviders.ErrNotFound
	}
	delete(s.rows, key)
	s.deleted = append(s.deleted, key)
	return provider.SecretRef, nil
}

func wiredProviderHandler(vault *recordingVault) *Handler {
	return NewHandler(nil, WithIdentityProviders(newRecordingProviderStore(), vault))
}

func handlerWithStore(store *recordingProviderStore, vault *recordingVault) *Handler {
	return NewHandler(nil, WithIdentityProviders(store, vault))
}

const validOIDCBody = `{
	"kind": "oidc",
	"display_name": "Corporate SSO",
	"oidc": {
		"issuer": "https://idp.example.com",
		"client_id": "elitea",
		"redirect_uri": "https://elitea.example.com/forward-auth/auth_oidc/callback"
	}
}`

// An unwired surface answers 503 and never pretends to have saved anything.
// This is the #128 shape the repository keeps rediscovering: a route that
// answers 200 while nothing behind it is composed.
func TestIdentityProviderRoutesRefuseWhenUnwired(t *testing.T) {
	handler := NewHandler(nil)

	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"list":   handler.IdentityProviderList,
		"save":   handler.IdentityProviderSave,
		"delete": handler.IdentityProviderDelete,
	} {
		recorder := httptest.NewRecorder()
		call(recorder, providerRequest(http.MethodPut, "corporate", validOIDCBody))
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code, "route %s", name)
	}
}

// The vault is half the dependency. A handler given a store and NO vault must
// refuse, rather than storing a definition whose client secret silently
// vanished — which would authenticate nothing and fail at the identity
// provider with a message naming neither this service nor the credential.
func TestIdentityProviderRefusesWithAStoreButNoVault(t *testing.T) {
	handler := NewHandler(nil, WithIdentityProviders(identityproviders.NewStore(nil), nil))
	recorder := httptest.NewRecorder()
	handler.IdentityProviderSave(recorder, providerRequest(http.MethodPut, "corporate", validOIDCBody))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

/* ── a refused save must not touch the credential ───────────────────────── */

// The ordering rule this surface exists to keep. The document is invalid, so
// the save is refused — and the secret that came with it must not have been
// sealed on the way to the refusal. Sealing first would overwrite a working
// provider's credential every time an operator mistyped a URL.
func TestARefusedDocumentSealsNothing(t *testing.T) {
	vault := newRecordingVault()
	recorder := httptest.NewRecorder()

	wiredProviderHandler(vault).IdentityProviderSave(recorder, providerRequest(http.MethodPut, "corporate", `{
		"kind": "oidc",
		"display_name": "Corporate SSO",
		"secret": "the-live-client-secret",
		"oidc": {
			"issuer": "http://idp.example.com",
			"client_id": "elitea",
			"redirect_uri": "https://elitea.example.com/callback"
		}
	}`))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Empty(t, vault.stored, "a refused save must not seal a secret")
	require.Empty(t, vault.deleted, "a refused save must not clear the sealed secret either")
}

// The inherited fields. A save that omits `enabled` must not retire a live
// provider, and one that omits `secret` must not erase its credential.
func TestASaveThatOmitsEnabledAndSecretInheritsBoth(t *testing.T) {
	store := newRecordingProviderStore()
	store.rows["corporate"] = identityproviders.Provider{
		Key: "corporate", Kind: identityproviders.KindOIDC, DisplayName: "Corporate SSO",
		Enabled: true, Revision: 4, SecretRef: "identity_provider__corporate_ab12__secret",
		OIDC: &identityproviders.OIDCDocument{
			Issuer: "https://idp.example.com", ClientID: "elitea",
			RedirectURI: "https://elitea.example.com/forward-auth/auth_oidc/callback",
		},
	}
	vault := newRecordingVault()
	recorder := httptest.NewRecorder()

	handlerWithStore(store, vault).IdentityProviderSave(
		recorder, providerRequest(http.MethodPut, "corporate", validOIDCBody))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, store.upserted, 1)
	require.True(t, store.upserted[0].Enabled, "an omitted `enabled` retired a live provider")
	require.Equal(t, "identity_provider__corporate_ab12__secret", store.upserted[0].SecretRef,
		"an omitted `secret` erased the sealed credential")
	require.Empty(t, vault.stored)
	require.Empty(t, vault.deleted)
}

// A row cannot change protocol under its own key: the previous kind's secret
// would stay sealed under a name the new document never reads.
func TestChangingTheKindUnderOneKeyIsRefused(t *testing.T) {
	store := newRecordingProviderStore()
	store.rows["corporate"] = identityproviders.Provider{
		Key: "corporate", Kind: identityproviders.KindSAML, DisplayName: "Corporate SAML",
	}
	vault := newRecordingVault()
	recorder := httptest.NewRecorder()

	handlerWithStore(store, vault).IdentityProviderSave(
		recorder, providerRequest(http.MethodPut, "corporate", validOIDCBody))

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Empty(t, store.upserted)
	require.Empty(t, vault.stored)
}

// An unreadable table is a 503, not a 400. Rendering it as the operator's
// typing mistake would send them to re-check a document that is correct.
func TestAnUnreadableTableIsNotReportedAsABadRequest(t *testing.T) {
	store := newRecordingProviderStore()
	store.lookupErr = errors.New("connection refused")
	recorder := httptest.NewRecorder()

	handlerWithStore(store, newRecordingVault()).IdentityProviderSave(
		recorder, providerRequest(http.MethodPut, "corporate", validOIDCBody))

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

// Deleting a provider removes its sealed secret too. Leaving it would keep a
// credential in the vault that nothing names and nobody can find.
func TestDeletingAProviderAlsoRemovesItsSealedSecret(t *testing.T) {
	store := newRecordingProviderStore()
	store.rows["corporate"] = identityproviders.Provider{
		Key: "corporate", Kind: identityproviders.KindOIDC, DisplayName: "Corporate SSO",
		SecretRef: "identity_provider__corporate_ab12__secret",
	}
	vault := newRecordingVault()
	recorder := httptest.NewRecorder()

	handlerWithStore(store, vault).IdentityProviderDelete(
		recorder, providerRequest(http.MethodDelete, "corporate", ""))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []string{"identity_provider__corporate_ab12__secret"}, vault.deleted)
}

// The refusal NAMES the field. A save of twenty fields that answers only "not
// valid" sends the operator back with nothing to go on.
func TestARefusalNamesTheFieldItRefused(t *testing.T) {
	recorder := httptest.NewRecorder()

	wiredProviderHandler(newRecordingVault()).IdentityProviderSave(
		recorder, providerRequest(http.MethodPut, "corporate", `{
			"kind": "oidc",
			"display_name": "Corporate SSO",
			"oidc": {"issuer": "https://idp.example.com", "client_id": "elitea", "redirect_uri": ""}
		}`))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "redirect_uri", body["field"])
}

// A kind this service has no login path for is refused rather than defaulted.
// Treating it as OIDC would federate logins through a protocol the operator
// did not choose.
func TestAnUnknownKindIsRefused(t *testing.T) {
	recorder := httptest.NewRecorder()

	wiredProviderHandler(newRecordingVault()).IdentityProviderSave(
		recorder, providerRequest(http.MethodPut, "corporate", `{"kind":"ldap","display_name":"LDAP"}`))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

// Signing an outbound SAML request needs a sealed private key. The refusal
// happens BEFORE the seal, so an operator who clears the secret and turns
// signing on in one save is told no with the old key still in place.
func TestSigningWithoutASealedKeyIsRefusedBeforeTheVaultIsTouched(t *testing.T) {
	vault := newRecordingVault()
	recorder := httptest.NewRecorder()

	certificate := testCertificateBody(t)
	wiredProviderHandler(vault).IdentityProviderSave(recorder, providerRequest(http.MethodPut, "corporate_saml",
		fmt.Sprintf(`{
		"kind": "saml",
		"display_name": "Corporate SAML",
		"secret": "",
		"saml": {
			"idp_entity_id": "https://idp.example.com/metadata",
			"idp_sso_url": "https://idp.example.com/sso",
			"idp_certificates": [%q],
			"sp_entity_id": "https://elitea.example.com/saml",
			"acs_url": "https://elitea.example.com/acs",
			"sign_authn_requests": true,
			"sp_certificate": %q
		}
	}`, certificate, certificate)))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Empty(t, vault.deleted, "the refusal must not have already cleared the sealed key")
}

// Clearing a secret must not destroy the vault entry until the row that names
// it is written. Deleting first and then failing the write left the stored row
// pointing at an entry that no longer existed, and every login answered 503
// until somebody saved again.
func TestClearingASecretDoesNotTouchTheVaultUntilTheRowIsWritten(t *testing.T) {
	store := newRecordingProviderStore()
	store.rows["corporate"] = identityproviders.Provider{
		Key: "corporate", Kind: identityproviders.KindOIDC, DisplayName: "Corporate SSO",
		Enabled: true, Revision: 4, SecretRef: "identity_provider__corporate_ab12__secret",
		OIDC: &identityproviders.OIDCDocument{
			Issuer: "https://idp.example.com", ClientID: "elitea",
			RedirectURI: "https://elitea.example.com/forward-auth/auth_oidc/callback",
		},
	}
	store.upsertErr = errors.New("connection reset by peer")
	vault := newRecordingVault()
	recorder := httptest.NewRecorder()

	handlerWithStore(store, vault).IdentityProviderSave(recorder, providerRequest(
		http.MethodPut, "corporate", strings.Replace(validOIDCBody, `"kind": "oidc",`,
			`"kind": "oidc", "secret": "",`, 1)))

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Empty(t, vault.deleted,
		"the sealed secret was destroyed before the row that stopped naming it was written")
}

// And when the row DOES get written, the stale entry is removed.
func TestClearingASecretRemovesTheVaultEntryAfterTheRowIsWritten(t *testing.T) {
	store := newRecordingProviderStore()
	store.rows["corporate"] = identityproviders.Provider{
		Key: "corporate", Kind: identityproviders.KindOIDC, DisplayName: "Corporate SSO",
		Enabled: true, Revision: 4, SecretRef: "identity_provider__corporate_ab12__secret",
		OIDC: &identityproviders.OIDCDocument{
			Issuer: "https://idp.example.com", ClientID: "elitea",
			RedirectURI: "https://elitea.example.com/forward-auth/auth_oidc/callback",
		},
	}
	vault := newRecordingVault()
	recorder := httptest.NewRecorder()

	handlerWithStore(store, vault).IdentityProviderSave(recorder, providerRequest(
		http.MethodPut, "corporate", strings.Replace(validOIDCBody, `"kind": "oidc",`,
			`"kind": "oidc", "secret": "",`, 1)))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, store.upserted, 1)
	require.Empty(t, store.upserted[0].SecretRef, "the stored row must stop naming the secret")
	require.Equal(t, []string{"identity_provider__corporate_ab12__secret"}, vault.deleted)
}

/* ── the plaintext secret never comes back out ──────────────────────────── */

// A read renders a set secret as a mask. The only way a plaintext secret leaves
// this service is not at all.
func TestAViewMasksASealedSecretAndOmitsAnAbsentOne(t *testing.T) {
	sealed := identityProviderToView(identityproviders.Provider{
		Key: "corporate", Kind: identityproviders.KindOIDC,
		DisplayName: "Corporate SSO", SecretRef: "identity_provider__corporate_ab12__secret",
		OIDC: &identityproviders.OIDCDocument{ClientID: "elitea"},
	})
	require.Equal(t, identityProviderSecretMask, sealed.Secret)

	public := identityProviderToView(identityproviders.Provider{
		Key: "public", Kind: identityproviders.KindOIDC, DisplayName: "Public client",
		OIDC: &identityproviders.OIDCDocument{ClientID: "elitea"},
	})
	require.Empty(t, public.Secret,
		"a provider with no secret must not render a mask: the two states are different")
}

// The three states of the secret field on a write are distinct, and the check
// that reads them is the same one the seal acts on — so they cannot disagree
// about what a save means.
func TestTheSecretFieldIsTriState(t *testing.T) {
	empty := ""
	value := "s3cret"

	require.True(t, identityProviderWillHoldSecret(nil, "existing_ref"),
		"an absent field leaves the sealed secret alone")
	require.False(t, identityProviderWillHoldSecret(nil, ""),
		"an absent field on a provider with no secret leaves it without one")
	require.False(t, identityProviderWillHoldSecret(&empty, "existing_ref"),
		"an empty string clears the secret")
	require.True(t, identityProviderWillHoldSecret(&value, ""),
		"a value seals one")
}

// The vault name is DERIVED from the key rather than stored, so a row and its
// secret cannot drift apart — and two providers cannot collide on one entry.
func TestSecretNamesAreDerivedAndDistinct(t *testing.T) {
	first := identityProviderSecretName("corporate")
	second := identityProviderSecretName("corporate_backup")

	require.NotEqual(t, first, second)
	require.Equal(t, first, identityProviderSecretName("corporate"),
		"the derivation must be stable, or a save would orphan the previous secret")
	require.Contains(t, first, identityProviderSecretFeature,
		"the name must be namespaced, or it could collide with another feature's credential")
}

// testCertificateBody returns a valid self-signed RSA-2048 certificate as the
// bare base64 body SAML metadata publishes.
//
// It is GENERATED rather than pasted, so the fixture cannot expire and cannot
// be mistaken for a real deployment's material. The matching private key exists
// only for the length of this test.
func testCertificateBody(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "identity provider"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(der)
}

// Login domains travel through the admin surface inside the per-kind document:
// a save stores them normalised, and the list renders them back.
func TestLoginDomainsRoundTripThroughTheAdminSurface(t *testing.T) {
	store := newRecordingProviderStore()
	handler := handlerWithStore(store, newRecordingVault())
	recorder := httptest.NewRecorder()

	handler.IdentityProviderSave(recorder, providerRequest(http.MethodPut, "github", `{
		"kind": "oidc",
		"display_name": "GitHub",
		"oidc": {
			"issuer": "https://dex.example.com",
			"client_id": "elitea",
			"redirect_uri": "https://elitea.example.com/forward-auth/auth_oidc/callback",
			"login_domains": ["Example.COM", "@users.noreply.github.com"]
		}
	}`))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, []string{"example.com", "users.noreply.github.com"},
		store.rows["github"].OIDC.LoginDomains)

	listed := httptest.NewRecorder()
	handler.IdentityProviderList(listed, httptest.NewRequest(http.MethodGet, "/identity_providers/administration", nil))
	require.Equal(t, http.StatusOK, listed.Code)
	require.Contains(t, listed.Body.String(), `"login_domains":["example.com","users.noreply.github.com"]`)

	refused := httptest.NewRecorder()
	handler.IdentityProviderSave(refused, providerRequest(http.MethodPut, "github", `{
		"kind": "oidc",
		"display_name": "GitHub",
		"oidc": {
			"issuer": "https://dex.example.com",
			"client_id": "elitea",
			"redirect_uri": "https://elitea.example.com/forward-auth/auth_oidc/callback",
			"login_domains": ["not a domain"]
		}
	}`))
	require.Equal(t, http.StatusBadRequest, refused.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(refused.Body.Bytes(), &body))
	require.Equal(t, "login_domains", body["field"])
}
