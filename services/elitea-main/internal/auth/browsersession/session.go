// Package browsersession holds the server-side browser session: the row that
// migrations/shared/0117_browser_sessions.sql creates, the opaque identifier
// the `elitea_session` cookie carries, and the two deadlines that decide
// whether the row still authenticates anybody.
//
// WHY IT EXISTS. The OIDC and SAML planes used to answer a login with a
// SELF-CONTAINED signed cookie: `uid`, `email` and a 24-hour `exp`, HMAC'd with
// APPLICATION_SECRET_KEY. Nothing on the server recorded the session, so
// logout could only delete the browser's copy, there was no idle deadline, and
// no plane could ever perform a federated single logout. See the migration
// header for the full statement of the three properties.
//
// WHAT THE COOKIE CARRIES. `s1.` and 32 random bytes in unpadded base64url.
// Nothing else. No claim, no user id, no signature — there is nothing to sign,
// because the value proves only membership of a table this service owns.
//
// WHY THE SAME COOKIE NAME. `elitea_session` is read by apimw.Auth, by the
// SPA's boot probe and by three test suites, and it is written by four
// handlers. A new name would need every one of them changed at once, and a
// deployment mid-rollout would hold both. The VALUE format changed instead,
// and the two formats cannot be confused: a legacy value is
// `<base64url JSON>.<hex>`, and base64url of a JSON object always begins `ey`,
// never `s1.`. LooksServerSide is that discriminator, stated once.
//
// THE LEGACY WINDOW. A deployment that upgrades holds unexpired legacy cookies
// for up to 24 hours. Rejecting them would sign every active user out at the
// moment of the deploy, so apimw.Auth keeps reading them for one release.
// `ELITEA_SESSION_REJECT_LEGACY_COOKIES=true` turns that off for an operator
// who would rather force one re-login than keep a stateless credential alive.
package browsersession

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// CookieName is the one name every plane writes and apimw.Auth reads.
const CookieName = "elitea_session"

// CookieValuePrefix marks a value as a server-side session identifier.
//
// The version digit is part of it on purpose. A later format can be told from
// this one by its prefix alone, which is what lets a reader refuse a shape it
// does not understand instead of guessing.
const CookieValuePrefix = "s1."

// IDRandomBytes is the entropy of one session identifier. The identifier IS
// the credential, so it is sized as one.
const IDRandomBytes = 32

// Provider names the plane that created a session. The migration's check
// constraint lists exactly these three.
const (
	ProviderOIDC = "oidc"
	ProviderSAML = "saml"
	ProviderForm = "form"
)

// The reasons a session does not authenticate anybody. They are separate
// values because an operator reading a log needs to tell "this browser has no
// session" from "this browser had one and it aged out" from "somebody signed
// this session out".
var (
	// ErrNotFound — no row carries this identifier. A forged value, a value
	// from another deployment, or a row the sweeper removed.
	ErrNotFound = errors.New("browsersession: no such session")
	// ErrRevoked — logout, or an administrative revocation.
	ErrRevoked = errors.New("browsersession: session is revoked")
	// ErrExpired — the ABSOLUTE deadline passed. Activity does not move it.
	ErrExpired = errors.New("browsersession: session reached its absolute deadline")
	// ErrIdle — the session was not used inside its idle window.
	ErrIdle = errors.New("browsersession: session was idle for too long")
	// ErrMalformedCookie — the value is not a server-side identifier at all.
	ErrMalformedCookie = errors.New("browsersession: cookie value is not a session identifier")
)

// Session is one row of elitea_auth.browser_sessions.
type Session struct {
	ID                   string
	UserID               int64
	Email                string
	Provider             string
	ProviderSessionIndex string
	CreatedAt            time.Time
	LastSeenAt           time.Time
	ExpiresAt            time.Time
	IdleTimeout          time.Duration
	RevokedAt            *time.Time
}

// Usable reports whether this row still authenticates its user at `now`, and
// names the reason when it does not.
//
// The order is revocation, then the absolute deadline, then the idle one. It
// is the order of certainty: a revoked session is a decision somebody made,
// and reporting it as "idle" would tell an operator the wrong story about a
// sign-out they performed.
func (s Session) Usable(now time.Time) error {
	if s.RevokedAt != nil {
		return ErrRevoked
	}
	if !now.Before(s.ExpiresAt) {
		return ErrExpired
	}
	if s.IdleTimeout > 0 && now.Sub(s.LastSeenAt) >= s.IdleTimeout {
		return ErrIdle
	}
	return nil
}

// Policy is the pair of lifetimes a new session is stamped with.
//
// It is copied ONTO the row rather than read at validation time. A deployment
// that shortens its idle window must not retroactively shorten sessions issued
// under the old one, and a deployment that lengthens it must not silently
// extend them. The row states the rule it was issued under.
type Policy struct {
	// IdleTimeout is measured from last_seen_at. Zero means no idle limit.
	IdleTimeout time.Duration
	// AbsoluteLifetime is measured from creation and never moves.
	AbsoluteLifetime time.Duration
	// RejectLegacyCookies makes apimw.Auth refuse the pre-0117 signed cookie.
	// See the package header for the window this closes.
	RejectLegacyCookies bool
}

// The defaults. Eight hours of inactivity signs a browser out; a session lives
// at most a week whatever the user does. Both are the values the Helm chart and
// deploy/docker-compose.yml document.
const (
	DefaultIdleTimeout      = 8 * time.Hour
	DefaultAbsoluteLifetime = 7 * 24 * time.Hour
)

// minAbsoluteLifetime keeps a mistyped value from issuing sessions that are
// already expired. A minute is short enough for a test to use deliberately and
// long enough that no real deployment reaches it by accident.
const minAbsoluteLifetime = time.Minute

// PolicyFromEnv reads the deployment's lifetimes.
//
// Keep the three names as LITERALS inside these os.Getenv calls.
// services/elitea-llm-gateway/scripts/env-drift-check.sh greps for a quoted
// name in an os.Getenv call; a named constant hides the read and the gate then
// reports a false green.
//
// An unparseable or non-positive value is REFUSED rather than defaulted. A
// deployment that meant to set a lifetime and mistyped it must not run for a
// week believing it did.
func PolicyFromEnv() (Policy, error) {
	policy := Policy{
		IdleTimeout:      DefaultIdleTimeout,
		AbsoluteLifetime: DefaultAbsoluteLifetime,
	}
	if raw := strings.TrimSpace(os.Getenv("ELITEA_SESSION_IDLE_TIMEOUT")); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil || value < 0 {
			return Policy{}, fmt.Errorf(
				"ELITEA_SESSION_IDLE_TIMEOUT must be a non-negative Go duration, for example 8h: %q", raw)
		}
		policy.IdleTimeout = value
	}
	if raw := strings.TrimSpace(os.Getenv("ELITEA_SESSION_ABSOLUTE_LIFETIME")); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil || value < minAbsoluteLifetime {
			return Policy{}, fmt.Errorf(
				"ELITEA_SESSION_ABSOLUTE_LIFETIME must be a Go duration of at least 1m, for example 168h: %q", raw)
		}
		policy.AbsoluteLifetime = value
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("ELITEA_SESSION_REJECT_LEGACY_COOKIES")), "true") {
		policy.RejectLegacyCookies = true
	}
	if policy.IdleTimeout > policy.AbsoluteLifetime {
		// An idle window longer than the absolute lifetime cannot ever fire.
		// Saying so is better than silently having one deadline.
		return Policy{}, fmt.Errorf(
			"ELITEA_SESSION_IDLE_TIMEOUT (%s) exceeds ELITEA_SESSION_ABSOLUTE_LIFETIME (%s)",
			policy.IdleTimeout, policy.AbsoluteLifetime)
	}
	return policy, nil
}

// NewID mints one session identifier from crypto/rand.
func NewID() (string, error) {
	raw := make([]byte, IDRandomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("browsersession: generate identifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// CookieValue is what the browser is given for a session identifier.
func CookieValue(id string) string { return CookieValuePrefix + id }

// LooksServerSide is the ONE discriminator between the two cookie formats.
//
// It is deliberately a prefix test and not a parse. A caller that has to decide
// WHICH reader to use cannot afford to run both, and the legacy format cannot
// produce this prefix: its first segment is base64url of a JSON object, which
// always begins `ey`.
func LooksServerSide(value string) bool {
	return strings.HasPrefix(value, CookieValuePrefix)
}

// ParseCookieValue returns the identifier a cookie carries.
func ParseCookieValue(value string) (string, error) {
	if !LooksServerSide(value) {
		return "", ErrMalformedCookie
	}
	id := strings.TrimPrefix(value, CookieValuePrefix)
	decoded, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(decoded) != IDRandomBytes ||
		base64.RawURLEncoding.EncodeToString(decoded) != id {
		return "", ErrMalformedCookie
	}
	return id, nil
}
