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
// SAML ForceAuthn and the Form password re-entry are R1.10b (#2594).

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
	if err != nil || seconds < 0 || time.Duration(seconds)*time.Second > maxRequestedAuthenticationLimit ||
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
// so a login started by the previous release still completes.
const oidcStateLimitSeparator = ":"

func encodeOIDCStateHead(nonce string, limit time.Duration, limited bool) string {
	if !limited {
		return nonce
	}
	return nonce + oidcStateLimitSeparator + strconv.FormatInt(int64(limit/time.Second), 10)
}

// decodeOIDCStateHead reads the limit from a VERIFIED state head. A head
// whose limit does not parse cannot come from Login, which signed it; it is
// read as the strictest limit rather than as none, so a defect here can only
// refuse a sign-in, never admit an old one.
func decodeOIDCStateHead(head string) (time.Duration, bool) {
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
// read as a fresh authentication. Without `auth_time` the sign-in itself is
// the authentication (the zero time, which the session manager reads as now).
//
// A limited sign-in completes only with an `auth_time` that is within the
// limit, allowing oidcAuthTimeClockSkew either way.
func checkAuthTime(authTime *float64, login oidcLoginState, now time.Time) (time.Time, bool) {
	var at time.Time
	if authTime != nil && !math.IsNaN(*authTime) && !math.IsInf(*authTime, 0) && *authTime > 0 {
		whole, fraction := math.Modf(*authTime)
		at = time.Unix(int64(whole), int64(fraction*float64(time.Second))).UTC()
	}
	if !login.limited {
		return at, true
	}
	if at.IsZero() {
		return time.Time{}, false
	}
	if at.After(now.Add(oidcAuthTimeClockSkew)) {
		return time.Time{}, false
	}
	if now.Sub(at) > login.limit+oidcAuthTimeClockSkew {
		return time.Time{}, false
	}
	return at, true
}
