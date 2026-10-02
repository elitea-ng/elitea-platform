package api

// The browser authentication URLs.
//
// Every browser-facing authentication route is under `/auth/`. `/auth`
// itself, with no further segment, is the edge auth check (router.go). Every
// route here is below it, so they never collide. Each route is registered by
// its full path rather than through r.Route("/auth"), because a mount at
// "/auth" would take over that check.
//
// Every login state cookie (OIDC state, nonce and PKCE; the SAML request; the
// session) has Path "/", and the return target is carried in that state.

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
)

// ssoBrowserPaths names the routes of the single sign-on plane.
type ssoBrowserPaths struct {
	Login, Continue, Logout, Info, FormLogout    string
	OIDCLogin, OIDCCallback, OIDCLogout          string
	SAMLMetadata, SAMLLogin, SAMLACS, SAMLLogout string
}

var ssoPaths = ssoBrowserPaths{
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

// mountSSOBrowserRoutes registers the single sign-on plane's browser routes.
func mountSSOBrowserRoutes(
	r chi.Router,
	paths ssoBrowserPaths,
	session *v2auth.SessionHandler,
	oidc *v2auth.OIDCHandler,
	saml *v2auth.SAMLHandler,
	chooser *browserauth.SSOChooser,
) {
	// The sign-in page (browserauth/chooser.go). One usable provider gives a
	// redirect to its login route, with `target_to` preserved. Two show the
	// provider buttons and the work email field.
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

// mountFormBrowserRoutes serves the Form plane's router under `/auth`.
//
// Each of browserauth.FormPaths is registered by its full path and reaches
// the Form router with the prefix stripped, for the reason the file comment
// gives about `/auth`.
func mountFormBrowserRoutes(r chi.Router, browser http.Handler) {
	stripped := stripRoutingPrefix(browserauth.BasePath, browser)
	for _, path := range browserauth.FormPaths() {
		r.Handle(browserauth.BasePath+path, stripped)
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
