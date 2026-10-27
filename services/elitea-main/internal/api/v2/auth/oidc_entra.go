package auth

// Entra mode of the OIDC plane (ADR-0039 decision 4, issue #1117).
//
// It exists only when the composition root attached custody with
// WithEntraGovernance, which it does only with the credential governance
// flag on. Without it the `entra`
// marker on a stored provider is ignored and every sign-in is byte-for-byte
// the path in oidc.go.
//
// With it, a stored provider marked `entra: true`:
//
//   - is discovered with the multi-tenant issuer template allowed, and its
//     ID tokens are accepted only from <authority>/<tid>/v2.0 for an allowed
//     tid, with `tid` and `oid` present;
//   - asks for the middle-tier API scope and the cp1 client capability;
//   - after the browser session exists, retains the access token whose `aud`
//     is the middle tier as a sign-in assertion bound to that session. A
//     token that fails the assertion checks is not retained, and the sign-in
//     still succeeds: governed runs then pause for authorization.
//
// The account link stays keyed by `sub`, as for every OIDC provider, so
// turning the flag on does not orphan existing accounts. The governed
// identity, the one delegations record, is (tid, oid).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	identityoidc "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/oidc"
)

// EntraCustody is the governed custody the OIDC plane writes to.
type EntraCustody interface {
	RetainSignIn(ctx context.Context, signIn delegationstore.SignIn) (string, error)
	RevokeSession(ctx context.Context, sessionID string) (int, error)
}

// WithEntraGovernance turns Entra mode on for stored providers marked
// `entra: true`. The composition root calls it only with credential
// governance on; nil leaves the handler unchanged.
func (h *OIDCHandler) WithEntraGovernance(custody EntraCustody) *OIDCHandler {
	if h == nil || custody == nil {
		return h
	}
	h.entraCustody = custody
	return h
}

// WithEntraGovernance makes logout revoke the delegations the session
// created. nil leaves the handler unchanged.
func (h *SessionHandler) WithEntraGovernance(custody EntraCustody) *SessionHandler {
	if h == nil || custody == nil {
		return h
	}
	h.entraCustody = custody
	return h
}

// entraRuntime is the Entra half of one provider runtime.
type entraRuntime struct {
	policy     identityoidc.EntraPolicy
	assertions *identityoidc.EntraAssertionVerifier
}

// buildEntraRuntime discovers an Entra provider. It returns the runtime the
// generic path would build, with the Entra checks attached.
func (h *OIDCHandler) buildEntraRuntime(
	ctx context.Context,
	provider identityproviders.Provider,
	clientSecret string,
	scopes []string,
) (*oidcRuntime, error) {
	document := provider.OIDC
	policy, err := identityoidc.NewEntraPolicy(document.Issuer, document.ClientID, document.EntraTenants, document.EntraAPIScope)
	if err != nil {
		return nil, fmt.Errorf("the Entra settings of provider %q are invalid: %w", provider.Key, err)
	}
	// Entra's multi-tenant discovery publishes the {tenantid} template as
	// its issuer. The discovered issuer is checked below, and every token
	// issuer is checked per tid by the policy.
	discovered, err := oidc.NewProvider(oidc.InsecureIssuerURLContext(ctx, policy.IssuerTemplate()), document.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery failed for %s: %w", document.Issuer, err)
	}
	var metadata struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := discovered.Claims(&metadata); err != nil || metadata.JWKSURI == "" {
		return nil, fmt.Errorf("the Entra discovery document of %s is unusable", document.Issuer)
	}
	allowedIssuer := metadata.Issuer == policy.IssuerTemplate()
	for _, tid := range policy.AllowedTenants() {
		allowedIssuer = allowedIssuer || metadata.Issuer == policy.IssuerFor(tid)
	}
	if !allowedIssuer {
		return nil, fmt.Errorf("the Entra discovery issuer of %s is not an allowed tenant", document.Issuer)
	}
	keys := oidc.NewRemoteKeySet(ctx, metadata.JWKSURI)
	assertions, err := identityoidc.NewEntraAssertionVerifier(policy, keys, nil)
	if err != nil {
		return nil, err
	}
	scopes = slices.Clone(scopes)
	if !slices.Contains(scopes, policy.APIScope()) {
		scopes = append(scopes, policy.APIScope())
	}
	return &oidcRuntime{
		oauth2Cfg: &oauth2.Config{
			ClientID:     document.ClientID,
			ClientSecret: clientSecret,
			RedirectURL:  document.RedirectURI,
			Endpoint:     discovered.Endpoint(),
			Scopes:       scopes,
		},
		verifier: oidc.NewVerifier("", keys, &oidc.Config{
			ClientID:        document.ClientID,
			SkipIssuerCheck: true,
		}),
		requireEmailVerified: document.RequireEmailVerified,
		adoptSCIMUsers:       document.AdoptSCIMUsers,
		origin:               fmt.Sprintf("provider %s revision %d (entra)", provider.Key, provider.Revision),
		entra:                &entraRuntime{policy: policy, assertions: assertions},
	}, nil
}

// authOptions are the Entra additions to the authorization request.
func (e *entraRuntime) authOptions() []oauth2.AuthCodeOption {
	if e == nil {
		return nil
	}
	return []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("claims", identityoidc.EntraClientCapabilities)}
}

// verifyIdentity applies the Entra iss/tid/oid rules to a verified ID token.
func (e *entraRuntime) verifyIdentity(idToken *oidc.IDToken) (identityoidc.EntraIdentity, error) {
	var claims identityoidc.EntraClaims
	if err := idToken.Claims(&claims); err != nil {
		return identityoidc.EntraIdentity{}, identityoidc.ErrEntraIdentityClaims
	}
	return e.policy.VerifyIdentity(claims)
}

// retainAssertion stores the middle-tier access token for the session the
// sign-in created. It never fails the sign-in and never logs the token.
func (h *OIDCHandler) retainAssertion(
	ctx context.Context,
	runtime *oidcRuntime,
	identity identityoidc.EntraIdentity,
	accessToken string,
	sessionCookie string,
	userID int64,
) {
	if runtime.entra == nil || h.entraCustody == nil {
		return
	}
	sessionID, err := browsersession.ParseCookieValue(sessionCookie)
	if err != nil {
		// No server-side session: nothing logout could revoke by.
		slog.Warn("OIDC Entra: no server-side session, the delegation assertion is not retained", "user_id", userID)
		return
	}
	verified, err := runtime.entra.assertions.Verify(ctx, accessToken)
	if err != nil {
		slog.Warn("OIDC Entra: the access token is not a middle-tier assertion and is not retained",
			"user_id", userID, "reason", entraRefusalReason(err))
		return
	}
	if verified.Identity != identity {
		slog.Warn("OIDC Entra: the access token names another user and is not retained", "user_id", userID)
		return
	}
	if _, err := h.entraCustody.RetainSignIn(ctx, delegationstore.SignIn{
		SessionID: sessionID, UserID: userID, Issuer: verified.Issuer,
		TenantID: verified.Identity.TenantID, ObjectID: verified.Identity.ObjectID,
		Audience: verified.Audience, Scopes: verified.Scopes, Token: verified.Token, ExpiresAt: verified.ExpiresAt,
	}); err != nil {
		slog.Error("OIDC Entra: the delegation assertion could not be stored", "user_id", userID, "err", err)
	}
}

// revokeEntraSession revokes the delegations a session created. Logout
// proceeds whatever it returns.
func (h *SessionHandler) revokeEntraSession(ctx context.Context, cookieValue string) {
	if h.entraCustody == nil {
		return
	}
	sessionID, err := browsersession.ParseCookieValue(cookieValue)
	if err != nil {
		return
	}
	if _, err := h.entraCustody.RevokeSession(ctx, sessionID); err != nil {
		slog.Error("logout could not revoke the session's delegations", "err", err)
	}
}

// entraRefusalReason is a stable reason code for a log line.
func entraRefusalReason(err error) string {
	switch {
	case errors.Is(err, identityoidc.ErrEntraTenantNotAllowed):
		return "tenant_not_allowed"
	case errors.Is(err, identityoidc.ErrEntraIdentityClaims):
		return "identity_claims"
	case errors.Is(err, identityoidc.ErrEntraNotAnAssertion):
		return "not_an_assertion"
	case errors.Is(err, identityoidc.ErrProviderUnavailable):
		return "keys_unavailable"
	default:
		return "rejected"
	}
}

// issuedSessionCookie is the session cookie value issueBrowserSession set
// on this response, or "".
func issuedSessionCookie(header http.Header) string {
	for _, cookie := range (&http.Response{Header: header}).Cookies() {
		if cookie.Name == browsersession.CookieName && cookie.Value != "" {
			return cookie.Value
		}
	}
	return ""
}
