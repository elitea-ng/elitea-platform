package auth

// Recent authentication, the OIDC half (ADR-0059 decision 5, story R1.10a).
//
// A route that needs a recent sign-in answers 401 `reauthentication_required`
// (apimw.RecentAuthentication). The console then starts a LIMITED sign-in:
//
//	GET /auth/oidc/login?max_age=<seconds>&target_to=<same-origin path>
//
// Login sends `max_age` to the identity provider and binds the limit to the
// signed state cookie; the callback reads it back from there (never from its
// own query) and refuses an id_token whose `auth_time` is absent or older than
// the limit (OpenID Connect Core 3.1.2.1 makes `auth_time` REQUIRED once
// `max_age` was requested). The session the callback writes records
// `auth_time` as its authentication time, so the helper then passes.
//
// The SAML half (R1.10b, #2594) is the same shape:
//
//	GET /auth/saml/login?max_age=<seconds>&target_to=<same-origin path>
//
// Login sets ForceAuthn="true" on the AuthnRequest and binds the limit to the
// signed request cookie (the same `<id>:<seconds>|<target>` head). The ACS
// reads it back from there and refuses an assertion whose AuthnInstant is
// absent or older than the limit, with the authored clock skew of the
// assertion's other time checks. Only such a session is PROVEN
// (browser_sessions.authentication_proven): a SAML sign-in without ForceAuthn
// records no authentication time at all, because an identity provider's
// single sign-on answers it without asking the person for anything.
//
// The Form plane has no identity provider to ask: its re-entry is a password
// prompt of its own (internal/api/browserauth, /auth/form/reauthenticate).

import (
	"errors"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxRequestedAuthenticationLimit caps `max_age`. It matches the cap of
// ELITEA_SESSION_REAUTHENTICATION_MAX_AGE: a limit of more than a day is not
// a recent authentication.
const maxRequestedAuthenticationLimit = 24 * time.Hour

// oidcAuthTimeClockSkew is the clock skew allowed between this server and the
// identity provider when `auth_time` is compared with the limit. It is the
// leeway the id_token verification already applies (go-oidc allows five
// minutes on `nbf`), and no more.
const oidcAuthTimeClockSkew = 5 * time.Minute

// errAuthenticationLimit is the 400 of a malformed `max_age`.
var errAuthenticationLimit = errors.New("max_age must be a whole number of seconds between 0 and 86400")

// requestedAuthenticationLimit reads the login entry's `max_age`. Absent is an
// ordinary sign-in. A value that is present and not a whole number of seconds
// in range is refused rather than ignored: a console that asked for a limit
// must not get an unlimited sign-in because it misspelled one.
func requestedAuthenticationLimit(query url.Values) (time.Duration, bool, error) {
	if !query.Has("max_age") {
		return 0, false, nil
	}
	raw := query.Get("max_age")
	seconds, err := strconv.ParseInt(raw, 10, 64)
	// Compared in seconds BEFORE the conversion: a Duration of more than
	// about 292 years wraps, and a wrapped value would pass a range check.
	if err != nil || seconds < 0 || seconds > int64(maxRequestedAuthenticationLimit/time.Second) ||
		strconv.FormatInt(seconds, 10) != raw {
		return 0, false, errAuthenticationLimit
	}
	return time.Duration(seconds) * time.Second, true, nil
}

// oidcLoginState is what the signed state cookie carries back to the callback.
type oidcLoginState struct {
	targetTo string
	limit    time.Duration
	limited  bool
}

// oidcStateLimitSeparator separates the nonce from the limit in the state's
// head: `<nonce>[:<max_age seconds>]|<target_to>`. The nonce is unpadded
// base64url and holds no ":", and a state without one is an ordinary sign-in,
// so a login started by the previous release still completes. The SAML
// request cookie uses the same head around its request ID, which gosaml2
// mints as "_" and a UUID and which holds no ":" either.
const oidcStateLimitSeparator = ":"

func encodeLoginHead(nonce string, limit time.Duration, limited bool) string {
	if !limited {
		return nonce
	}
	return nonce + oidcStateLimitSeparator + strconv.FormatInt(int64(limit/time.Second), 10)
}

// decodeLoginHead reads the limit from a VERIFIED state head. A head
// whose limit does not parse cannot come from Login, which signed it; it is
// read as the strictest limit rather than as none, so a defect here can only
// refuse a sign-in, never admit an old one.
func decodeLoginHead(head string) (time.Duration, bool) {
	separator := strings.Index(head, oidcStateLimitSeparator)
	if separator < 0 {
		return 0, false
	}
	seconds, err := strconv.ParseInt(head[separator+1:], 10, 64)
	if err != nil || seconds < 0 {
		return 0, true
	}
	return time.Duration(seconds) * time.Second, true
}

// checkAuthTime decides the authentication time of the session a callback
// writes, and whether a limited sign-in may complete.
//
// An ordinary sign-in always completes. Its session records `auth_time` when
// the provider sent one, because that is when the person last proved who they
// are; a silent single sign-on from an hours-old provider session must not
// read as a fresh authentication. Without `auth_time` the time is UNKNOWN (the
// zero time, stored as NULL), and such a session never satisfies the
// recent-authentication check: a silent sign-on could otherwise stand in for
// the limited sign-in the check asks for.
//
// A limited sign-in completes only with an `auth_time` that is within the
// limit, allowing oidcAuthTimeClockSkew either way. The time it records is
// never older than `now - limit`: an `auth_time` the skew alone admitted is
// recorded at the edge of the limit, so the check that asked for this sign-in
// with the same limit passes it instead of asking again in a loop.
func checkAuthTime(authTime *float64, login oidcLoginState, now time.Time) (time.Time, bool) {
	var at time.Time
	if authTime != nil && !math.IsNaN(*authTime) && !math.IsInf(*authTime, 0) && *authTime > 0 {
		whole, fraction := math.Modf(*authTime)
		at = time.Unix(int64(whole), int64(fraction*float64(time.Second))).UTC()
	}
	if !login.limited {
		return at, true
	}
	return checkAuthenticationInstant(at, login.limit, oidcAuthTimeClockSkew, now)
}

// checkAuthenticationInstant holds a LIMITED sign-in's stated authentication
// time to the limit: present, not after now+skew, and not older than
// limit+skew. The time it answers is never older than `now - limit`: one the
// skew alone admitted is recorded at the edge of the limit, so the check that
// asked for the sign-in with the same limit passes it instead of asking again
// in a loop. OIDC passes `auth_time` with the id_token verifier's skew; SAML
// passes AuthnInstant with the provider's authored clock skew.
func checkAuthenticationInstant(at time.Time, limit, skew time.Duration, now time.Time) (time.Time, bool) {
	if at.IsZero() {
		return time.Time{}, false
	}
	if skew < 0 {
		skew = 0
	}
	if at.After(now.Add(skew)) {
		return time.Time{}, false
	}
	if now.Sub(at) > limit+skew {
		return time.Time{}, false
	}
	if edge := now.Add(-limit); at.Before(edge) {
		at = edge
	}
	return at.UTC(), true
}

// samlLoginRequest is what the signed SAML request cookie carries back to
// the ACS.
type samlLoginRequest struct {
	requestID string
	targetTo  string
	limit     time.Duration
	limited   bool
}

// checkAuthnInstant decides the authentication time of the session a SAML
// ACS writes, and whether a limited sign-in may complete. It answers the
// time (zero for unknown), whether that time is proof of an authentication,
// and whether the sign-in may complete.
//
// An ORDINARY sign-in (the request carried no ForceAuthn) always completes
// and records NO authentication time: without ForceAuthn the identity
// provider may answer from a session of its own without asking the person
// for anything, so neither the sign-in nor the AuthnInstant it states is
// proof of a fresh authentication. Such a session never satisfies the
// recent-authentication check, and no unsolicited or replayed response can
// move a session's time: the time is decided by the SIGNED request cookie,
// and the InResponseTo check binds the response to that request.
//
// A LIMITED sign-in completes only with an AuthnInstant within the limit,
// allowing the provider's authored clock skew, and records it as proven.
func checkAuthnInstant(authnInstant *time.Time, request samlLoginRequest, skew time.Duration, now time.Time) (time.Time, bool, bool) {
	if !request.limited {
		return time.Time{}, false, true
	}
	var at time.Time
	if authnInstant != nil {
		at = *authnInstant
	}
	at, ok := checkAuthenticationInstant(at, request.limit, skew, now)
	return at, ok, ok
}
