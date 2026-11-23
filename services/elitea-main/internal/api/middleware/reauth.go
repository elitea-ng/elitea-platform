package middleware

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

// Recent authentication (ADR-0059 decision 5, story R1.10a).
//
// Three rules of ADR-0059 need "this person authenticated within the last 15
// minutes": administration scopes on tokens (R1.3a), starting an elevation
// (R2.2) and critical actions (R4.1). This file is the one check they share.
// It reads the authentication time the server-side browser session row
// records (browser_sessions.authenticated_at, shared 0261), which a refresh
// never moves, so a session kept alive by polling does not stay "recent".
//
// ONLY A SESSION WHOSE AUTHENTICATION TIME IS PROVEN SATISFIES IT. Today that
// is an OIDC session with a known authentication time: the provider's
// `auth_time`, which a limited sign-in (`max_age`) requires. A personal access
// token, a native session and a run token carry no authentication time and
// are refused whatever they claim. So are the pre-0117 signed cookie, the Form
// session cookie, a SAML session (its sign-in time is not proof of a fresh
// authentication until ForceAuthn, R1.10b #2594, teaches this file the SAML
// kind) and a row whose authentication time is unknown. The refusal is the
// same 401 for all of them: the remedy is a fresh browser sign-in, and a
// client that cannot do one learns that the route is not for it.

// CodeReauthenticationRequired is the error code of the refusal. The web
// client (R1.3b) reacts to it by sending the person through sign-in with a
// limit (`max_age`) and back to the page they were on.
const CodeReauthenticationRequired = "reauthentication_required"

// HeaderReauthenticationMaxAge carries the limit, in whole seconds, on a
// `reauthentication_required` refusal.
const HeaderReauthenticationMaxAge = "X-Elitea-Reauthentication-Max-Age"

// SessionKind names the kind of browser session that authenticated a
// request, in the words of ADR-0059 decision 7 ("session", "form",
// "sealed"). A caller that admits only some kinds (token scopes refuse a
// sealed session) tests BrowserAuthentication.Kind.
type SessionKind string

// SessionKindSession is a server-side `elitea_session` row written by a
// single sign-on plane.
const SessionKindSession SessionKind = "session"

// BrowserAuthentication is what the helper knows about the authentication
// behind a request.
type BrowserAuthentication struct {
	Kind SessionKind
	// Provider is the plane that wrote the row (browsersession.Provider*).
	Provider string
	// AuthenticatedAt is the person's last authentication.
	AuthenticatedAt time.Time
}

// BrowserAuthenticationOf reports the browser authentication behind the
// request, and false when the request was not authenticated by a session
// this helper can vouch for: a token, a native session, a run token, the
// legacy signed cookie, the Form session cookie, a provider whose sign-in
// does not prove a fresh authentication yet, a row with no known
// authentication time, or nobody.
func BrowserAuthenticationOf(r *http.Request) (BrowserAuthentication, bool) {
	source, ok := auth.AuthenticationSourceFromContext(r.Context())
	if !ok || source != auth.AuthenticationSourceSession {
		return BrowserAuthentication{}, false
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok || user.TokenID != "" || user.NativeClientID != "" ||
		user.BrowserSessionRef <= 0 || user.BrowserSessionAuthenticatedAt.IsZero() {
		return BrowserAuthentication{}, false
	}
	switch user.BrowserSessionProvider {
	case browsersession.ProviderOIDC:
	default:
		// SAML and Form rows record the sign-in, which a silent single
		// sign-on or a remembered password produces without a fresh proof
		// (R1.10b adds ForceAuthn and the password re-entry). A provider
		// this file does not know (the sealed session of R2.7) cannot be
		// told apart from a kind a caller must refuse.
		return BrowserAuthentication{}, false
	}
	return BrowserAuthentication{
		Kind:            SessionKindSession,
		Provider:        user.BrowserSessionProvider,
		AuthenticatedAt: user.BrowserSessionAuthenticatedAt,
	}, true
}

// RecentAuthentication is the check. The zero value is the deployment's
// check: it applies the limit of the session manager that authenticated the
// request (ELITEA_SESSION_REAUTHENTICATION_MAX_AGE, carried by apimw.Auth),
// or browsersession.DefaultReauthenticationMaxAge, against time.Now.
type RecentAuthentication struct {
	// MaxAge overrides the deployment's limit. Zero or negative keeps it.
	MaxAge time.Duration
	// Now replaces the clock. Only a test sets it.
	Now func() time.Time
}

// reauthenticationLimitKey carries the deployment's limit on the request
// context, from apimw.Auth to the check.
type reauthenticationLimitKey struct{}

// sessionPolicySource is the session manager's view of its policy
// (*browsersession.Manager).
type sessionPolicySource interface {
	Policy() browsersession.Policy
}

// withReauthenticationLimit records the limit of the composed session
// manager on the request, so a route's check needs no copy of the setting.
// It is the store apimw.Auth already holds, so no AuthConfig literal can
// forget it.
func withReauthenticationLimit(r *http.Request, store BrowserSessionValidator) *http.Request {
	source, ok := store.(sessionPolicySource)
	if !ok {
		return r
	}
	limit := source.Policy().ReauthenticationMaxAge
	if limit <= 0 {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), reauthenticationLimitKey{}, limit))
}

// MaxAgeFor is the limit the check applies to r.
func (c RecentAuthentication) MaxAgeFor(r *http.Request) time.Duration {
	if c.MaxAge > 0 {
		return c.MaxAge
	}
	if limit, ok := r.Context().Value(reauthenticationLimitKey{}).(time.Duration); ok && limit > 0 {
		return limit
	}
	return browsersession.DefaultReauthenticationMaxAge
}

func (c RecentAuthentication) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Check answers the browser authentication behind the request when it is
// recent enough, and false otherwise. A caller that refuses some session
// kinds tests the answered Kind itself.
func (c RecentAuthentication) Check(r *http.Request) (BrowserAuthentication, bool) {
	authentication, ok := BrowserAuthenticationOf(r)
	if !ok {
		return BrowserAuthentication{}, false
	}
	if c.now().Sub(authentication.AuthenticatedAt) > c.MaxAgeFor(r) {
		return BrowserAuthentication{}, false
	}
	return authentication, true
}

// Refuse writes the 401. The limit is stated in seconds, in a header, so a
// client can start the sign-in with the same `max_age` without knowing the
// deployment's setting.
func (c RecentAuthentication) Refuse(w http.ResponseWriter, r *http.Request) {
	logCredentialRefusal(r, refusalSourceOf(r), CodeReauthenticationRequired)
	w.Header().Set(HeaderReauthenticationMaxAge, strconv.FormatInt(int64(c.MaxAgeFor(r)/time.Second), 10))
	writeJSONError(w, http.StatusUnauthorized, "authentication_error", CodeReauthenticationRequired,
		"this action needs a recent sign-in; sign in again and retry")
}

// Require is the route middleware form: it refuses with Refuse unless Check
// passes.
func (c RecentAuthentication) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := c.Check(r); !ok {
			c.Refuse(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refusalSourceOf names the credential that was refused, in the words the
// other refusal lines use.
func refusalSourceOf(r *http.Request) string {
	source, _ := auth.AuthenticationSourceFromContext(r.Context())
	switch source {
	case auth.AuthenticationSourceAPIKey:
		return sourceAPIKey
	case auth.AuthenticationSourceToken:
		return sourceToken
	default:
		return sourceSession
	}
}
