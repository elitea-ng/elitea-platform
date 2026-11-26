package auth

// A PROVIDER WHOSE ISSUER CHANGED CANNOT SIGN IN TO AN ACCOUNT LINKED UNDER THE
// OLD ONE (issue 2558, ADR-0059 R3B-1).
//
// The attack: an `admin` repoints the enabled OIDC provider at an identity
// provider they control, which asserts the `sub` of a standing super_admin.
// The link was `oidc:<sub>` and nothing else, so the callback handed over the
// account. This drives the REAL callback against two fake identity providers
// (the corporate one and the attacker's) and a real PostgreSQL holding the
// shipped migration corpus, shared 0258 included.
//
// The same callback serves desktop and mobile: a native sign-in
// (/api/v2/auth/native/authorize) sends the system browser through
// /auth/login with the native continue path as `target_to`, and the device is
// approved only from the browser session this callback issues. So the native
// case is the callback with that target, and it must issue no session either.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identityrepo"
)

// nativeContinueTarget is where the native authorize flow resumes after the
// browser sign-in (internal/api/nativeauth ContinuePath).
const nativeContinueTarget = "/api/v2/auth/native/authorize/continue?request=device-handle"

// switchableProviderSource is the stored-provider seam with a document that a
// test repoints, as the admin save does: same key, new issuer, new revision.
type switchableProviderSource struct {
	mu       sync.Mutex
	provider identityproviders.Provider
}

func (s *switchableProviderSource) Enabled(context.Context, identityproviders.Kind) (identityproviders.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.provider, nil
}

func (s *switchableProviderSource) point(issuer *fakeIdentityProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provider = identityproviders.Provider{
		Key: "corporate", Kind: identityproviders.KindOIDC, DisplayName: "Corporate", Enabled: true,
		Revision: s.provider.Revision + 1,
		OIDC: &identityproviders.OIDCDocument{
			Issuer: issuer.server.URL, ClientID: issuer.clientID,
			RedirectURI: "http://elitea.test/auth/oidc/callback", Scopes: []string{"openid", "email"},
		},
	}
}

type noSecrets struct{}

func (noSecrets) LookupAdminHiddenSecret(context.Context, string) (string, error) { return "", nil }

// signInWithTarget is fakeIdentityProvider.signIn with a `target_to`, and it
// also reports whether a session cookie was issued.
func (p *fakeIdentityProvider) signInWithTarget(
	handler *OIDCHandler, subject, email, target string,
) (code int, location string, sessionIssued bool) {
	loginRecorder := httptest.NewRecorder()
	handler.Login(loginRecorder, httptest.NewRequest(http.MethodGet,
		"/auth/oidc/login?target_to="+url.QueryEscape(target), nil))
	if loginRecorder.Code != http.StatusFound {
		return loginRecorder.Code, "", false
	}
	authorizeURL, err := url.Parse(loginRecorder.Header().Get("Location"))
	if err != nil {
		return 0, "", false
	}
	authorizationCode := fmt.Sprintf("code-%s-%d", subject, time.Now().UnixNano())
	p.mu.Lock()
	p.codes[authorizationCode] = fakeAuthorization{
		subject: subject, email: email, nonce: authorizeURL.Query().Get("nonce"),
	}
	p.mu.Unlock()

	callback := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code="+
		url.QueryEscape(authorizationCode)+"&state="+url.QueryEscape(authorizeURL.Query().Get("state")), nil)
	for _, cookie := range loginRecorder.Result().Cookies() {
		callback.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.Callback(recorder, callback)
	return recorder.Code, recorder.Header().Get("Location"), issuedSessionCookie(recorder.Header()) != ""
}

func storedProviderHandler(t *testing.T, pool *pgxpool.Pool, source *switchableProviderSource) *OIDCHandler {
	t.Helper()
	// The fake providers are loopback httptest servers.
	t.Setenv(IdentityProviderAllowLoopbackEnv, "true")
	handler, err := NewOIDCHandler(context.Background(), nil, pool, "test-secret-key")
	require.NoError(t, err)
	return handler.WithProviderStore(source, noSecrets{})
}

func TestARepointedIssuerCannotSignInToAnAccountLinkedUnderTheOldOne(t *testing.T) {
	pool := newSignInProvisioningPool(t)
	ctx := context.Background()
	corporate, attacker := newFakeIdentityProvider(t), newFakeIdentityProvider(t)
	source := &switchableProviderSource{}
	source.point(corporate)
	handler := storedProviderHandler(t, pool, source)

	// The super_admin signs in through the real provider: account and link.
	code, _, issued := corporate.signInWithTarget(handler, "root-sub", "root@corp.example", "/")
	require.Equal(t, http.StatusFound, code)
	require.True(t, issued)
	var rootID int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT user_id FROM public.auth_core__user_provider WHERE provider_ref = 'oidc:root-sub'`).Scan(&rootID))
	var boundIssuer string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT issuer FROM elitea_auth.identity_bindings
		 WHERE provider_ref = 'oidc:root-sub' AND provider_key = 'corporate'`).Scan(&boundIssuer))
	require.Equal(t, corporate.server.URL, boundIssuer, "the link must be bound to the issuer that asserted it")

	// The provider is repointed at the attacker's identity provider, which
	// asserts the same subject. Browser and native sign-ins are both refused,
	// and neither gets a session.
	source.point(attacker)
	for _, target := range []string{"/", nativeContinueTarget} {
		code, location, issued := attacker.signInWithTarget(handler, "root-sub", "root@corp.example", target)
		require.Equal(t, http.StatusConflict, code, "target %s", target)
		require.False(t, issued, "target %s: a refused sign-in issued a session", target)
		require.Empty(t, location, "target %s", target)
	}
	// Nor did it learn another address or a second link.
	var email string
	require.NoError(t, pool.QueryRow(ctx, `SELECT email FROM public.auth_core__user WHERE id = $1`, rootID).Scan(&email))
	require.Equal(t, "root@corp.example", email)
	var bindings int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM elitea_auth.identity_bindings WHERE provider_ref = 'oidc:root-sub'`).Scan(&bindings))
	require.Equal(t, 1, bindings)

	// A super_admin who rotated the issuer ON PURPOSE re-binds the links, and
	// sign-in works through the new issuer, the native path included.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	moved, err := identityrepo.RebindLinks(ctx, tx, identityrepo.RebindRequest{
		Prefix: identityrepo.OIDCProviderRefPrefix,
		Target: identityrepo.ProviderBinding{Key: "corporate", Issuer: attacker.server.URL},
		From:   &identityrepo.ProviderBinding{Key: "corporate", Issuer: corporate.server.URL},
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	require.EqualValues(t, 1, moved)

	code, location, issued := attacker.signInWithTarget(handler, "root-sub", "root@corp.example", nativeContinueTarget)
	require.Equal(t, http.StatusFound, code)
	require.True(t, issued)
	require.Equal(t, nativeContinueTarget, location)

	// The retired issuer no longer matches.
	source.point(corporate)
	code, _, _ = corporate.signInWithTarget(handler, "root-sub", "root@corp.example", "/")
	require.Equal(t, http.StatusConflict, code)
}

// The environment provider (OIDC_ISSUER_URL) keeps a pre-0258 deployment
// signing in: an unbound link is bound on its next sign-in. A stored provider
// may not claim the same unbound link.
func TestAnUnboundLinkSignsInOnlyThroughTheEnvironmentProvider(t *testing.T) {
	pool := newSignInProvisioningPool(t)
	ctx := context.Background()
	var userID int
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO public.auth_core__user (email, name) VALUES ('legacy@corp.example', 'Legacy') RETURNING id`,
	).Scan(&userID))
	_, err := pool.Exec(ctx,
		`INSERT INTO public.auth_core__user_provider (user_id, provider_ref) VALUES ($1, 'oidc:legacy-sub')`, userID)
	require.NoError(t, err)

	stored := newFakeIdentityProvider(t)
	source := &switchableProviderSource{}
	source.point(stored)
	code, _, issued := stored.signInWithTarget(storedProviderHandler(t, pool, source),
		"legacy-sub", "legacy@corp.example", "/")
	require.Equal(t, http.StatusConflict, code)
	require.False(t, issued)

	environment := newFakeIdentityProvider(t)
	code, _, issued = environment.signInWithTarget(environment.handler(t, pool),
		"legacy-sub", "legacy@corp.example", "/")
	require.Equal(t, http.StatusFound, code)
	require.True(t, issued)
	var key, issuer string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT provider_key, issuer FROM elitea_auth.identity_bindings WHERE provider_ref = 'oidc:legacy-sub'`,
	).Scan(&key, &issuer))
	require.Equal(t, identityrepo.EnvironmentProviderKey, key)
	require.Equal(t, environment.server.URL, issuer)
}

// A link written by a replica of the previous release while the upgrade rolls
// out has no binding. It is bound at its next sign-in through the stored
// provider that was live through the upgrade (not saved since 0258 was
// applied), and refused once that provider has been saved since: a provider
// repointed after the upgrade never takes an unbound link.
func TestAnUpgradeWindowLinkBindsOnlyToAProviderUnchangedSinceTheUpgrade(t *testing.T) {
	pool := newSignInProvisioningPool(t)
	ctx := context.Background()
	stored := newFakeIdentityProvider(t)
	tag, err := pool.Exec(ctx, `
INSERT INTO elitea_auth.identity_providers (provider_key, kind, display_name, enabled, config, updated_at)
SELECT 'corporate', 'oidc', 'Corporate', true,
       jsonb_build_object('issuer', $1::text, 'client_id', 'elitea', 'redirect_uri', 'https://e/cb'),
       ledger.applied_at - interval '1 minute'
FROM elitea_runtime.schema_migrations AS ledger WHERE ledger.target_kind = 'shared' AND ledger.version = 258`,
		stored.server.URL)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "the corpus must ledger 0258")
	unboundLink := func(subject string) {
		t.Helper()
		var userID int
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO public.auth_core__user (email, name) VALUES ($1, 'Window') RETURNING id`,
			subject+"@corp.example").Scan(&userID))
		_, err := pool.Exec(ctx,
			`INSERT INTO public.auth_core__user_provider (user_id, provider_ref) VALUES ($1, $2)`, userID, "oidc:"+subject)
		require.NoError(t, err)
	}
	source := &switchableProviderSource{}
	source.point(stored)
	handler := storedProviderHandler(t, pool, source)

	unboundLink("window")
	code, _, issued := stored.signInWithTarget(handler, "window", "window@corp.example", "/")
	require.Equal(t, http.StatusFound, code)
	require.True(t, issued)
	var key, issuer string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT provider_key, issuer FROM elitea_auth.identity_bindings WHERE provider_ref = 'oidc:window'`,
	).Scan(&key, &issuer))
	require.Equal(t, "corporate", key)
	require.Equal(t, stored.server.URL, issuer)

	// Any save after the upgrade (here, a touch) ends the window.
	_, err = pool.Exec(ctx, `UPDATE elitea_auth.identity_providers SET updated_at = now() WHERE provider_key = 'corporate'`)
	require.NoError(t, err)
	unboundLink("late")
	code, _, issued = stored.signInWithTarget(handler, "late", "late@corp.example", "/")
	require.Equal(t, http.StatusConflict, code)
	require.False(t, issued)
}
