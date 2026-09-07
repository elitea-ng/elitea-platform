package api

import (
	"errors"
	"net/http"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

var ErrInvalidProductionRuntimeRoutes = errors.New("invalid production runtime routes")

// ProductionRuntimeRoutes is an opaque route pair. Construction binds both
// runtime handlers to the existing trusted-peer and active-principal checks so
// production routing cannot mount either handler without that verification.
type ProductionRuntimeRoutes struct {
	validation      http.Handler
	executionEvents http.Handler
}

// browser carries the BROWSER half of the credential set these routes accept,
// and it is the /api/v2 group's own apimw.AuthConfig. Only the cookie fields
// are read from it; the token validators are deliberately dropped, so these
// two routes stay narrower than the group.
//
// It is a whole AuthConfig rather than the loose fields it used to be —
// `sessionSecret string` — because a browser cookie must be read the SAME WAY
// on every route that a browser reaches, and the only way to keep that true is
// to take the value the group already composed instead of building a second
// one here. The second one is what shipped: when the browser session became a
// server-side row (migrations/shared/0117), the group's AuthConfig gained
// SessionStore and this literal did not, so an `elitea_session` cookie holding
// an opaque `s1.` identifier reached the LEGACY HMAC reader on this path
// alone. It answered 401, and the events stream — the one surface an
// EventSource can authenticate on nothing but a cookie — became unreadable for
// every browser while every other route worked. The index journey saw a run
// admitted with 200 and then no terminal event at all.
//
// An edge-only deployment still passes the zero AuthConfig and behaves exactly
// as before: no cookie of any shape is read.
//
// The browser is admitted here at all because the chat surface reads the
// execution-events stream with an EventSource, which can send a cookie and
// nothing else — no bearer, no forwarded identity (#93). Composing these routes
// for forwarded identity alone made that stream unreadable by the product's own
// UI while every server-side hop worked, which is the shape #248's audit kept
// finding.
//
// Accepting a session here does not widen what a caller may SEE. The routes
// still require a runtime principal, and auth.RuntimePrincipalFromContext
// already admits AuthenticationSourceSession alongside forwarded/token/API-key
// — a session-authenticated user was always a valid runtime principal, it just
// had no way to prove it here. The events handler then authorizes per request
// against the execution's project and capability
// (runtimecomposition.postgresPublicAuthorizer.AuthorizeExecutionEvents), so
// membership and permission checks are unchanged.
func NewProductionRuntimeRoutes(
	validation http.Handler,
	executionEvents http.Handler,
	principalValidator apimw.PrincipalValidator,
	forwardedIdentityVerifier apimw.ForwardedIdentityPeerVerifier,
	browser apimw.AuthConfig,
) (*ProductionRuntimeRoutes, error) {
	if validation == nil || executionEvents == nil || principalValidator == nil || forwardedIdentityVerifier == nil {
		return nil, ErrInvalidProductionRuntimeRoutes
	}

	authenticate := apimw.Auth(apimw.AuthConfig{
		PrincipalValidator:        principalValidator,
		ForwardedIdentityVerifier: forwardedIdentityVerifier,
		// The three cookie fields, copied together. apimw.Auth reads exactly
		// these when it finds an `elitea_session` cookie; a copy that took
		// fewer of them is the defect described above.
		SessionSecret:              browser.SessionSecret,
		SessionStore:               browser.SessionStore,
		RejectLegacySessionCookies: browser.RejectLegacySessionCookies,
	})
	return &ProductionRuntimeRoutes{
		validation:      authenticate(requireRuntimePrincipal(validation)),
		executionEvents: authenticate(requireRuntimePrincipal(executionEvents)),
	}, nil
}

func requireRuntimePrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, ok := auth.RuntimePrincipalFromContext(request.Context()); !ok {
			apierr.WriteStatus(writer, http.StatusUnauthorized, "runtime authentication required")
			return
		}
		next.ServeHTTP(writer, request)
	})
}
