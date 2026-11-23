package middleware

import (
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
// ONLY A SERVER-SIDE BROWSER SESSION CAN SATISFY IT. A personal access token,
// a native session and a run token carry no authentication time and are
// refused whatever they claim; so are the pre-0117 signed cookie and the Form
// session cookie, which record none (the Form plane's password re-entry is
// R1.10b). The refusal is the same 401 for all of them: the remedy is a fresh
// browser sign-in, and a client that cannot do one learns that the route is
// not for it.

// CodeReauthenticationRequired is the error code of the refusal. The web
// client (R1.3b) reacts to it by sending the person through sign-in with a
// limit (`max_age`) and back to the page they were on.
const CodeReauthenticationRequired = "reauthentication_required"

// SessionKind names the kind of browser session that authenticated a
// request, in the words of ADR-0059 decision 7 ("session", "form",
// "sealed"). Callers that admit only some kinds (token scopes refuse a
// sealed session) read it from BrowserAuthentication.Kind.
type SessionKind string

const (
	// SessionKindSession is a server-side `elitea_session` row written by an
	// OIDC or SAML sign-in.
	SessionKindSession SessionKind = "session"
	// SessionKindForm is a server-side row written by the Form plane.
	SessionKindForm SessionKind = "form"
)

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
// request, and false when the request was not authenticated by a
// server-side browser session at all: a token, a native session, a run
// token, the legacy signed cookie, the Form session cookie, or nobody.
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
	kind := SessionKindSession
	switch user.BrowserSessionProvider {
	case browsersession.ProviderOIDC, browsersession.ProviderSAML:
	case browsersession.ProviderForm:
		kind = SessionKindForm
	default:
		// A provider this file does not know cannot be told apart from a
		// kind a caller must refuse, so it is not a browser session here.
		return BrowserAuthentication{}, false
	}
	return BrowserAuthentication{
		Kind:            kind,
		Provider:        user.BrowserSessionProvider,
		AuthenticatedAt: user.BrowserSessionAuthenticatedAt,
	}, true
}

// RecentAuthentication is the check. The zero value is usable: it applies
// browsersession.DefaultReauthenticationMaxAge against time.Now.
type RecentAuthentication struct {
	// MaxAge is the limit (browsersession.Policy.ReauthenticationMaxAge).
	// Zero or negative means the default.
	MaxAge time.Duration
	// Now replaces the clock. Only a test sets it.
	Now func() time.Time
}

func (c RecentAuthentication) maxAge() time.Duration {
	if c.MaxAge <= 0 {
		return browsersession.DefaultReauthenticationMaxAge
	}
	return c.MaxAge
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
	if c.now().Sub(authentication.AuthenticatedAt) > c.maxAge() {
		return BrowserAuthentication{}, false
	}
	return authentication, true
}

// Refuse writes the 401. The limit is stated in seconds, in a header, so a
// client can start the sign-in with the same `max_age` without knowing the
// deployment's setting.
func (c RecentAuthentication) Refuse(w http.ResponseWriter, r *http.Request) {
	logCredentialRefusal(r, sourceSession, CodeReauthenticationRequired)
	w.Header().Set(HeaderReauthenticationMaxAge, formatSeconds(c.maxAge()))
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

// HeaderReauthenticationMaxAge carries the limit, in whole seconds, on a
// `reauthentication_required` refusal.
const HeaderReauthenticationMaxAge = "X-Elitea-Reauthentication-Max-Age"

func formatSeconds(d time.Duration) string {
	seconds := int64(d / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	return strconv.FormatInt(seconds, 10)
}
