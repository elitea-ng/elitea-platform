package api

// The browser authentication URLs, and the deprecated aliases.
//
// Every browser-facing authentication route is served under `/auth`
// (canonical) and under `/forward-auth` (the prefix it had before). The
// `/forward-auth` routes are a DEPRECATED COMPATIBILITY ALIAS, kept because
// identity provider registrations (OIDC redirect URIs, SAML ACS URLs and
// entity metadata), bookmarks and edge configurations in the field name them.
// Both prefixes mount the SAME handlers. The alias is not a redirect: a SAML
// assertion arrives as a POST, and an OIDC callback carries a one-time code.
//
// Login state survives a change of prefix in the middle of a login. Every
// state cookie (OIDC state, nonce and PKCE; the SAML request; the session) has
// Path "/", and the return target is carried in that state, not in the URL
// prefix. A login begun at `/auth/oidc/login` can return at
// `/forward-auth/auth_oidc/callback`, and the reverse.
//
// `/auth` itself, with no further segment, is the edge's forward-auth check
// (router.go). Every route here is below it, so they never collide. Each
// canonical route is registered by its full path rather than through
// r.Route("/auth"), because a mount at "/auth" would take over that check.
//
// `/internal/forward-auth/main` is not browser-facing and is not renamed.

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
)

// ssoBrowserPaths names one prefix's routes of the single sign-on plane.
type ssoBrowserPaths struct {
	Login, Continue, Logout, Info, FormLogout    string
	OIDCLogin, OIDCCallback, OIDCLogout          string
	SAMLMetadata, SAMLLogin, SAMLACS, SAMLLogout string
}

var canonicalSSOPaths = ssoBrowserPaths{
	Login:        v2auth.SignInPath,
	Continue:     browserauth.BasePath + browserauth.ChooserContinuePath,
	Logout:       browserauth.BasePath + browserauth.LogoutPath,
	Info:         browserauth.BasePath + "/info",
	FormLogout:   browserauth.BasePath + browserauth.FormLogoutPath,
	OIDCLogin:    v2auth.OIDCLoginPath,
	OIDCCallback: v2auth.OIDCCallbackPath,
	OIDCLogout:   browserauth.BasePath + "/oidc/logout",
	SAMLMetadata: v2auth.SAMLMetadataPath,
	SAMLLogin:    v2auth.SAMLLoginPath,
	SAMLACS:      v2auth.SAMLACSPath,
	SAMLLogout:   browserauth.BasePath + "/saml/logout",
}

// legacySSOPaths are the deprecated `/forward-auth` aliases.
var legacySSOPaths = ssoBrowserPaths{
	Login:        browserauth.LegacyBasePath + browserauth.LoginPath,
	Continue:     browserauth.LegacyBasePath + browserauth.ChooserContinuePath,
	Logout:       browserauth.LegacyBasePath + browserauth.LogoutPath,
	Info:         browserauth.LegacyBasePath + "/info",
	FormLogout:   browserauth.LegacyBasePath + browserauth.LegacyFormLogoutPath,
	OIDCLogin:    browserauth.LegacyBasePath + "/auth_oidc/login",
	OIDCCallback: browserauth.LegacyBasePath + "/auth_oidc/callback",
	OIDCLogout:   browserauth.LegacyBasePath + "/auth_oidc/logout",
	SAMLMetadata: browserauth.LegacyBasePath + "/auth_saml/metadata",
	SAMLLogin:    browserauth.LegacyBasePath + "/auth_saml/login",
	SAMLACS:      browserauth.LegacyBasePath + "/auth_saml/acs",
	SAMLLogout:   browserauth.LegacyBasePath + "/auth_saml/logout",
}

// mountSSOBrowserRoutes registers the single sign-on plane's browser routes
// under one prefix.
func mountSSOBrowserRoutes(
	r chi.Router,
	paths ssoBrowserPaths,
	session *v2auth.SessionHandler,
	oidc *v2auth.OIDCHandler,
	saml *v2auth.SAMLHandler,
	chooser *browserauth.SSOChooser,
) {
	// The sign-in page (browserauth/chooser.go). One usable provider keeps
	// the old behaviour, a redirect to its login route, with `target_to`
	// preserved. Two show the provider buttons and the work email field.
	if chooser != nil {
		r.Get(paths.Login, chooser.Login)
		r.Get(paths.Continue, chooser.Continue)
		r.Post(paths.Continue, chooser.Continue)
	}
	r.Get(paths.Logout, session.Logout)
	r.Get(paths.Info, session.Info)
	r.Get(paths.FormLogout, session.Logout)
	if oidc != nil {
		r.Get(paths.OIDCLogin, oidc.Login)
		r.Get(paths.OIDCCallback, oidc.Callback)
	}
	r.Get(paths.OIDCLogout, session.Logout)
	// SAML 2.0. Metadata and login are GET navigations, and the assertion
	// consumer service is a POST because the authentication request asks for
	// the HTTP-POST binding.
	//
	// The SAML logout route clears the local session, as the OIDC one does.
	// Federated single logout — sending a LogoutRequest to the identity
	// provider's SLO endpoint — is NOT mounted: it needs the session index of
	// the assertion that started the session, and this deployment's session
	// cookie does not carry one. Mounting a route that silently only cleared
	// the local session would tell an operator their users were signed out
	// everywhere.
	if saml != nil {
		r.Get(paths.SAMLMetadata, saml.Metadata)
		r.Get(paths.SAMLLogin, saml.Login)
		r.Post(paths.SAMLACS, saml.ACS)
		r.Get(paths.SAMLLogout, session.Logout)
	}
}

// mountFormBrowserRoutes serves the Form plane's router under both prefixes.
//
// The router is mounted at the legacy prefix whole, as before. Under the
// canonical prefix only browserauth.CanonicalFormPaths are served, each by its
// full path, for the reason the file comment gives about `/auth`.
func mountFormBrowserRoutes(r chi.Router, browser http.Handler) {
	r.Mount(browserauth.LegacyBasePath, browser)
	canonical := stripRoutingPrefix(browserauth.BasePath, browser)
	for _, path := range browserauth.CanonicalFormPaths() {
		r.Handle(browserauth.BasePath+path, canonical)
	}
}

// stripRoutingPrefix serves next as if it were mounted at prefix: it removes
// the prefix from the path and starts next with a fresh chi routing context,
// so the child router matches its own relative routes.
func stripRoutingPrefix(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		child := r.Clone(context.WithValue(r.Context(), chi.RouteCtxKey, nil))
		child.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		if child.URL.RawPath != "" {
			child.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, prefix)
		}
		next.ServeHTTP(w, child)
	})
}
