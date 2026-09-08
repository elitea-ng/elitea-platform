package main

import (
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/authcomposition"
)

// apiGroupAuthConfig composes the credential set that api.NewRouter applies to
// the WHOLE /api/v2 group.
//
// internal/api/router.go copies these four fields into one apimw.AuthConfig and
// installs it with r.Use on the group that wraps r.Route("/api/v2", ...). It is
// therefore the broadest AuthConfig in the composition root: every route in the
// group authenticates through it.
//
// Two deployment shapes reach it. With ELITEA_AUTH_CONFIG_FILE set, formGraph
// is the token validator and the production principal validator and forwarded
// identity verifier come with it. Without it — the OIDC-only shape, which is
// what the E2E stack and any SSO-only install run — the browser session cookie
// is the only credential the deployment issues, so the config carries
// SessionSecret instead.
//
// The OIDC-only branch must STILL carry a principal validator.
// apimw.validatePrincipal returns the session's user unchanged when the field
// is nil, so a deactivated or suspended user's unexpired cookie was accepted on
// EVERY /api/v2 route. The per-project permission gate in front of each handler
// does not help, because a deactivated user's RBAC rows survive deactivation.
// This is the defect #301 fixed on chat_config and #314 fixed on the project
// list and the notification event stream, at the scope of the whole group
// (#370). See chatConfigAuthConfig.
//
// This is now the ONLY apimw.AuthConfig composition in cmd/elitea-main.
// Every per-route AuthConfig literal in main.go is gone: sixteen of the
// twenty-one left SessionSecret empty, so a browser holding a valid
// session got `401 missing authorization header` on the routes only a
// browser calls. TestNoPrivateAuthConfigLiteralsInMain fails the build if
// a second composition appears.
//
// sessionPrincipals is a parameter rather than something this function builds,
// and formGraph stays a concrete *authcomposition.FormGraph rather than the
// apimw.TokenValidator interface, for the same reason: both make the nil
// handling visible at the call site. A nil *FormGraph assigned to an interface
// field yields a non-nil interface holding a nil pointer, and `!= nil`
// downstream then reads as "configured" (#86). sessionTokens obeys the same
// rule: the call site must leave the interface nil when it cannot build a
// validator, and must never box a nil pointer into it.
//
// The call site passes authsvc.NewPrincipalValidator(pool) for
// sessionPrincipals and NOT main.go's `principalValidator` variable: that
// variable is assigned only inside the `authEnabled` block, so it is nil in
// exactly the branch that needs it. Issue #314 records that trap.
//
// The OIDC-only branch must ALSO carry a token validator. The same
// sessionSecret is the personal-access-token signing key (router.go passes it
// to v2auth.WithTokenSigningKey), so a non-empty secret turns the token
// creation route ON. Without a validator, apimw.validateToken finds both
// Validator and Client nil and answers 401 for every Bearer and X-API-Key
// credential. The deployment then issues personal access tokens that no route
// can accept. sessionTokens is the pool-backed validator that reads the same
// tokens back.
//
// This is a DELIBERATE widening, and the only one in this function. An
// OIDC-only deployment now accepts a personal access token on every /api/v2
// route. Each such credential answered 401 before.
//
// Three properties bound the new surface. sessionTokens reads a live token row
// from the database on every request. That row's owner must not be suspended.
// A session cookie carries no `uuid` claim, so no caller can replay a cookie
// as a bearer token. TestAPIGroupOIDCOnlyAuthAcceptsAPersonalAccessToken holds
// this behavior.
//
// SESSIONSTORE IS THE SERVER-SIDE SESSION (migrations/shared/0117), and it is
// carried on BOTH branches for the same reason SessionSecret is: a browser
// authenticates the same way whichever plane issued its cookie, and a branch
// that dropped the store would read a server-side identifier with the legacy
// HMAC reader and refuse every browser on it.
//
// A deployment with neither credential plane gets the zero AuthConfig, which
// admits nothing.
func apiGroupAuthConfig(
	formGraph *authcomposition.FormGraph,
	principalValidator apimw.PrincipalValidator,
	forwardedIdentityVerifier apimw.ForwardedIdentityPeerVerifier,
	sessionPrincipals apimw.PrincipalValidator,
	sessionTokens apimw.TokenValidator,
	sessionSecret string,
	oidcSessionEnabled bool,
	browserSessions *browsersession.Manager,
) apimw.AuthConfig {
	// The manager stays a CONCRETE pointer for the same reason formGraph does:
	// a nil *Manager assigned to an interface field yields a non-nil interface
	// holding a nil pointer, and `cfg.SessionStore != nil` downstream then
	// reads as "configured" (#86). apimw.Auth would take the server-side
	// branch for every cookie and refuse the whole deployment. The boxing
	// happens once, here, under a nil test.
	var sessionStore apimw.BrowserSessionValidator
	if browserSessions != nil {
		sessionStore = browserSessions
	}
	rejectLegacySessionCookies := browserSessions.Policy().RejectLegacyCookies
	if formGraph != nil {
		return apimw.AuthConfig{
			Validator:                  formGraph,
			PrincipalValidator:         principalValidator,
			ForwardedIdentityVerifier:  forwardedIdentityVerifier,
			SessionSecret:              sessionSecret,
			SessionStore:               sessionStore,
			RejectLegacySessionCookies: rejectLegacySessionCookies,
		}
	}
	if oidcSessionEnabled {
		return apimw.AuthConfig{
			Validator:                  sessionTokens,
			SessionSecret:              sessionSecret,
			PrincipalValidator:         sessionPrincipals,
			SessionStore:               sessionStore,
			RejectLegacySessionCookies: rejectLegacySessionCookies,
		}
	}
	return apimw.AuthConfig{}
}
