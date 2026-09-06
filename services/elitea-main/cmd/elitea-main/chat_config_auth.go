package main

import (
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/authcomposition"
)

// chatConfigAuthConfig composes the credential set for the ungated
// chat_config / project-context pair.
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
// is nil, so a deactivated or suspended user's unexpired cookie was accepted
// on GET /api/v2/elitea_core/chat_config/prompt_lib/{projectID} — the
// per-project permission gate in front of the handler does not help, because a
// deactivated user's RBAC rows survive deactivation (#301).
//
// sessionPrincipals is a parameter rather than something this function builds,
// and formGraph stays a concrete *authcomposition.FormGraph rather than the
// apimw.TokenValidator interface, for the same reason: both make the nil
// handling visible at the call site. A nil *FormGraph assigned to an interface
// field yields a non-nil interface holding a nil pointer, and `!= nil`
// downstream then reads as "configured".
func chatConfigAuthConfig(
	formGraph *authcomposition.FormGraph,
	principalValidator apimw.PrincipalValidator,
	forwardedIdentityVerifier apimw.ForwardedIdentityPeerVerifier,
	sessionPrincipals apimw.PrincipalValidator,
	sessionSecret string,
) apimw.AuthConfig {
	if formGraph != nil {
		return apimw.AuthConfig{
			Validator:                 formGraph,
			PrincipalValidator:        principalValidator,
			ForwardedIdentityVerifier: forwardedIdentityVerifier,
			// SessionSecret, and this branch had none (F3).
			//
			// apimw.Auth's cookie branch is inert without it, so a browser
			// holding a valid `elitea_session` had NO credential this route
			// accepts and got `401 missing authorization header` — on the one
			// path that only a browser ever calls, in the one deployment shape
			// that sets both a form config and OIDC (the standalone stack).
			// Every other /api/v2 route answered 200 to the same request,
			// because apiGroupAuthConfig's form branch has always carried it.
			//
			// The SPA reads that 401 as an expired session and starts a fresh
			// OIDC round-trip, so every visit to /app/artifacts — which reads
			// this endpoint for its upload limit — re-authenticated.
			//
			// This is the SAME correction, for the same reason, that the
			// notification routes needed: see main.go's `formSessionSecret`
			// comment. It is not a new trust decision. It is the same cookie,
			// verified with the same APPLICATION_SECRET_KEY and the same
			// principal validator, and the per-project permission gate in
			// front of the handler is unchanged.
			SessionSecret: sessionSecret,
		}
	}
	return apimw.AuthConfig{
		SessionSecret:      sessionSecret,
		PrincipalValidator: sessionPrincipals,
	}
}
