package auth

// Entra mode of the OIDC callback (issue #1117), driven end to end: the
// handler's own Login, the in-process Entra mock (entratest) for discovery,
// keys and the code exchange, and a real PostgreSQL for the account rows.
// Custody is a recording fake; the custody store has its own integration
// test. Requires ELITEA_TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/entratest"
)

const (
	entraTenant      = "aaaaaaaa-0000-4000-8000-000000000001"
	entraOtherTenant = "bbbbbbbb-0000-4000-8000-000000000002"
)

type recordingCustody struct {
	mu       sync.Mutex
	retained []delegationstore.SignIn
	revoked  []string
}

func (c *recordingCustody) RetainSignIn(_ context.Context, signIn delegationstore.SignIn) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retained = append(c.retained, signIn)
	return "0192f5e2-7c3a-7b1e-8a9d-000000000001", nil
}

func (c *recordingCustody) RevokeSession(_ context.Context, sessionID string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revoked = append(c.revoked, sessionID)
	return 0, nil
}

type fixedProvider struct{ provider identityproviders.Provider }

func (f fixedProvider) Enabled(context.Context, identityproviders.Kind) (identityproviders.Provider, error) {
	return f.provider, nil
}

type noSecret struct{}

func (noSecret) LookupAdminHiddenSecret(context.Context, string) (string, error) { return "", nil }

func entraProvider(mock *entratest.Server, issuer string) identityproviders.Provider {
	return identityproviders.Provider{
		Key: "entra", Kind: identityproviders.KindOIDC, Enabled: true, Revision: 1,
		OIDC: &identityproviders.OIDCDocument{
			Issuer: issuer, ClientID: mock.MiddleTierClientID,
			RedirectURI: "https://elitea.test/auth/oidc/callback",
			Scopes:      []string{"openid", "profile", "email"},
			Entra:       true, EntraTenants: []string{entraTenant},
		},
	}
}

type entraHarness struct {
	t       *testing.T
	mock    *entratest.Server
	handler *OIDCHandler
	ctx     context.Context
}

func newEntraHarness(t *testing.T, issuer func(*entratest.Server) string, custody EntraCustody) *entraHarness {
	t.Helper()
	pool := newSignInProvisioningPool(t)
	mock := entratest.New(t)
	manager, _ := newTestSessions(t)
	handler, err := NewOIDCHandler(context.Background(), nil, pool, "test-secret-key")
	require.NoError(t, err)
	handler = handler.WithSessionManager(manager).
		WithProviderStore(fixedProvider{provider: entraProvider(mock, issuer(mock))}, noSecret{}).
		WithEntraGovernance(custody)
	return &entraHarness{
		t: t, mock: mock, handler: handler,
		ctx: oidc.ClientContext(context.Background(), mock.Client()),
	}
}

// login runs Login and returns the authorize URL and the cookies.
func (h *entraHarness) login() (*url.URL, []*http.Cookie) {
	recorder := httptest.NewRecorder()
	h.handler.Login(recorder, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil).WithContext(h.ctx))
	require.Equal(h.t, http.StatusFound, recorder.Code, recorder.Body.String())
	authorize, err := url.Parse(recorder.Header().Get("Location"))
	require.NoError(h.t, err)
	return authorize, recorder.Result().Cookies()
}

func (h *entraHarness) signIn(authorization entratest.Authorization) *httptest.ResponseRecorder {
	authorize, cookies := h.login()
	authorization.Nonce = authorize.Query().Get("nonce")
	code := fmt.Sprintf("code-%d", time.Now().UnixNano())
	h.mock.AddCode(code, authorization)
	callback := httptest.NewRequest(http.MethodGet,
		"/auth/oidc/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(authorize.Query().Get("state")), nil).
		WithContext(h.ctx)
	for _, cookie := range cookies {
		callback.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	h.handler.Callback(recorder, callback)
	return recorder
}

func entraUser(n int) entratest.Authorization {
	return entratest.Authorization{
		TenantID: entraTenant, ObjectID: fmt.Sprintf("cccccccc-0000-4000-8000-%012d", n),
		Subject: fmt.Sprintf("pairwise-sub-%d", n), Email: fmt.Sprintf("entra-%d@autotest.local", n),
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buffer
}

func TestAGovernedEntraSignInRetainsTheMiddleTierAssertion(t *testing.T) {
	logs := captureLogs(t)
	custody := &recordingCustody{}
	h := newEntraHarness(t, func(m *entratest.Server) string { return m.URL + "/organizations/v2.0" }, custody)

	authorize, _ := h.login()
	require.Contains(t, strings.Fields(authorize.Query().Get("scope")), "api://"+h.mock.MiddleTierClientID+"/.default")
	require.JSONEq(t, `{"access_token":{"xms_cc":{"values":["cp1"]}}}`, authorize.Query().Get("claims"))

	recorder := h.signIn(entraUser(1))
	require.Equal(t, http.StatusFound, recorder.Code, recorder.Body.String())
	require.Len(t, custody.retained, 1)
	retained := custody.retained[0]
	require.Equal(t, entraTenant, retained.TenantID)
	require.Equal(t, entraUser(1).ObjectID, retained.ObjectID)
	require.Equal(t, h.mock.MiddleTierClientID, retained.Audience)
	require.Equal(t, h.mock.Issuer(entraTenant), retained.Issuer)
	require.NotEmpty(t, retained.SessionID)

	// Logout revokes what the session created.
	session := sessionCookieOf(t, recorder)
	logout := httptest.NewRequest(http.MethodGet, "/auth/logout", nil)
	logout.AddCookie(session)
	NewSessionHandler(nil, "test-secret-key").WithSessionManager(h.handler.sessions).WithEntraGovernance(custody).
		Logout(httptest.NewRecorder(), logout)
	require.Equal(t, []string{retained.SessionID}, custody.revoked)

	require.NotContains(t, logs.String(), retained.Token)
}

func TestEntraTokensFromATenantOutsideTheAllowListAreRejected(t *testing.T) {
	custody := &recordingCustody{}
	h := newEntraHarness(t, func(m *entratest.Server) string { return m.URL + "/organizations/v2.0" }, custody)

	outside := entraUser(2)
	outside.TenantID = entraOtherTenant
	require.Equal(t, http.StatusUnauthorized, h.signIn(outside).Code)

	// tid claim says an allowed tenant, iss names another one.
	lying := entraUser(3)
	lying.IssuerTenant = entraOtherTenant
	require.Equal(t, http.StatusUnauthorized, h.signIn(lying).Code)
	require.Empty(t, custody.retained)
}

func TestEntraTokensWithoutTidOrOidAreRejected(t *testing.T) {
	custody := &recordingCustody{}
	h := newEntraHarness(t, func(m *entratest.Server) string { return m.URL + "/organizations/v2.0" }, custody)
	noTID := entraUser(4)
	noTID.OmitTID = true
	require.Equal(t, http.StatusUnauthorized, h.signIn(noTID).Code)
	noOID := entraUser(5)
	noOID.OmitOID = true
	require.Equal(t, http.StatusUnauthorized, h.signIn(noOID).Code)
	require.Empty(t, custody.retained)
}

func TestAnAccessTokenThatIsNotAMiddleTierAssertionIsNotRetained(t *testing.T) {
	logs := captureLogs(t)
	custody := &recordingCustody{}
	h := newEntraHarness(t, func(m *entratest.Server) string { return m.URL + "/organizations/v2.0" }, custody)

	graph := entraUser(6)
	graph.AccessTokenAudience = entratest.GraphAudience
	require.Equal(t, http.StatusFound, h.signIn(graph).Code, "the sign-in itself succeeds")
	idLike := entraUser(7)
	idLike.AccessTokenScope = "-"
	require.Equal(t, http.StatusFound, h.signIn(idLike).Code)
	require.Empty(t, custody.retained)
	require.Contains(t, logs.String(), "not_an_assertion")
	require.NotContains(t, logs.String(), "eyJ")
}

// Flag off: the handler has no custody, so the `entra` marker is ignored.
// The authorization request is the generic one, a token from any tenant the
// issuer names is accepted as before, nothing is retained and no custody
// row is written.
func TestFlagOffIgnoresTheEntraMarker(t *testing.T) {
	pool := newSignInProvisioningPool(t)
	mock := entratest.New(t)
	manager, _ := newTestSessions(t)
	handler, err := NewOIDCHandler(context.Background(), nil, pool, "test-secret-key")
	require.NoError(t, err)
	provider := entraProvider(mock, mock.Issuer(entraOtherTenant))
	handler = handler.WithSessionManager(manager).
		WithProviderStore(fixedProvider{provider: provider}, noSecret{}).
		WithEntraGovernance(nil)
	h := &entraHarness{t: t, mock: mock, handler: handler, ctx: oidc.ClientContext(context.Background(), mock.Client())}

	authorize, _ := h.login()
	require.Equal(t, "openid profile email", authorize.Query().Get("scope"))
	require.False(t, authorize.Query().Has("claims"))

	user := entraUser(8)
	user.TenantID = entraOtherTenant // not in the allow-list, accepted: Entra mode is off
	require.Equal(t, http.StatusFound, h.signIn(user).Code)
	noOID := entraUser(9)
	noOID.TenantID, noOID.OmitOID = entraOtherTenant, true
	require.Equal(t, http.StatusFound, h.signIn(noOID).Code)

	for _, table := range []string{"elitea_identity.signin_assertions", "elitea_identity.delegations"} {
		var rows int
		require.NoError(t, pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&rows))
		require.Zero(t, rows, table)
	}
}
