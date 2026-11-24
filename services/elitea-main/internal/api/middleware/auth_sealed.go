package middleware

// The sealed break-glass session in Auth (ADR-0059 decision 5, "Path 2: a
// sealed credential when single sign-on is down"; story R2.7a, issue 2521).
// browsersession/sealed.go states what the session is; this file states where
// it is read:
//
//   - only when the request names no Authorization and no X-API-Key header,
//     like every cookie;
//   - only on a route of the administration surface
//     (adminaccess.OnAdministrationSurface), found with chi's Find on the root
//     router before the route runs. Off the surface the cookie is ignored, as
//     if it were absent: the request is decided by the other credentials it
//     carries, and a request with none is unauthenticated;
//   - on the surface a LIVE sealed session wins over the Form and
//     `elitea_session` cookies of the same browser, so an administration
//     request is never decided by a session the person did not choose for
//     it. A sealed cookie that is not a live sealed session (unknown, an
//     ordinary session's identifier, revoked, expired, or its elevation
//     ended) is cleared and counts as absent: the other cookies decide, and
//     a request with none is unauthenticated. A sealed session that ended
//     must not sign a person out of a single sign-on session they still
//     hold (the web client treats a 401 as a sign-out), and falling through
//     grants nothing the other cookie does not already grant.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

// SealedSessionValidator resolves a sealed session cookie
// (browsersession.Manager.ValidateSealed). AuthConfig.SessionStore provides
// it; a store that does not reads no sealed cookie.
type SealedSessionValidator interface {
	ValidateSealed(ctx context.Context, cookieValue string) (browsersession.Session, error)
}

// reasonSealedSessionUnsupported — a sealed cookie reached a deployment whose
// session store cannot read sealed sessions.
const reasonSealedSessionUnsupported = "sealed_session_unsupported"

// OnAdministrationSurface reports whether r is routed to a route of the
// administration surface, in the administration mode for a `{mode}` route.
// It finds the route on the root router of the request (chi sets it on the
// request before any middleware runs); a request outside a chi router is
// off the surface.
func OnAdministrationSurface(r *http.Request) bool {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil || rctx.Routes == nil {
		return false
	}
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.Path
	}
	found := chi.NewRouteContext()
	return adminaccess.OnAdministrationSurface(r.Method, rctx.Routes.Find(found, r.Method, path), found.URLParam)
}

// serveSealedSession serves r with the sealed session its cookie names and
// reports true when it answered the request (served, or 503 when the store
// did not answer). false means the request carries no sealed cookie that
// counts here, and the other sources decide; a refused cookie is cleared
// first.
func serveSealedSession(next http.Handler, w http.ResponseWriter, r *http.Request, cfg AuthConfig) bool {
	cookie, err := r.Cookie(browsersession.SealedCookieName)
	if err != nil {
		return false
	}
	if !OnAdministrationSurface(r) {
		return false
	}
	sealed, ok := cfg.SessionStore.(SealedSessionValidator)
	if !ok || sealed == nil {
		browsersession.ClearSealedCookie(w)
		logCredentialRefusal(r, sourceSealed, reasonSealedSessionUnsupported)
		return false
	}
	session, err := sealed.ValidateSealed(r.Context(), cookie.Value)
	if err != nil {
		reason, classified := serverSessionRefusal(err)
		if !classified && errors.Is(err, browsersession.ErrSealedUnavailable) {
			reason, classified = reasonSealedSessionUnsupported, true
		}
		if !classified {
			// The store did not answer: 503, and the cookie is kept, as
			// for the ordinary server-side session.
			logCredentialRefusal(r, sourceSealed, reason)
			slog.ErrorContext(r.Context(), "the sealed session store could not be read", "err", err)
			w.Header().Set("Retry-After", "5")
			writeJSONError(w, http.StatusServiceUnavailable,
				"server_error", "session_store_unavailable", "session store unavailable")
			return true
		}
		browsersession.ClearSealedCookie(w)
		logCredentialRefusal(r, sourceSealed, reason)
		return false
	}
	user, err := validatePrincipal(r.Context(), cfg, session.User())
	if err != nil {
		writePrincipalRefusal(w, r, sourceSealed, err)
		return true
	}
	// From the row, after validation. The authentication time is not
	// proven to the recent-authentication check (BrowserAuthenticationOf
	// refuses the provider), so a route that asks for a recent sign-in
	// refuses a sealed session.
	user.BrowserSessionRef = session.Ref
	user.BrowserSessionAuthenticatedAt = session.AuthenticatedAt
	user.BrowserSessionProvider = browsersession.ProviderSealed
	user.BrowserSessionAuthenticationProven = false
	user.CredentialKind = auth.CredentialKindSealed
	// A cookie authenticated it: the cross-site write check applies.
	serveAuthenticated(next, w, r, user, auth.AuthenticationSourceSession)
	return true
}
