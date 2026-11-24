package browsersession

// The `sealed` session kind (ADR-0059 decision 5, "Path 2: a sealed
// credential when single sign-on is down", decision 7, the credentials table;
// story R2.7a, issue 2521).
//
// A sealed sign-in must not look like an ordinary session:
//
//   - It is a row of elitea_auth.browser_sessions with the provider `sealed`
//     (shared 0272), created ONLY by StartSealedSession, in one transaction
//     with its elevation. Manager.Create refuses the provider.
//   - It has a cookie of its own, SealedCookieName, with the `__Host-`
//     prefix, Secure, HttpOnly and SameSite=Strict. The prefix requires
//     Path=/, so the path cannot limit it: apimw.Auth reads it on routes of
//     the administration surface only (adminaccess.OnAdministrationSurface)
//     and treats it as no credential everywhere else. The ordinary cookie
//     never authenticates a sealed row (Manager.Validate answers
//     ErrNotFound), and the sealed cookie never authenticates an ordinary
//     one (ValidateSealed).
//   - It never refreshes or introspects against an identity provider: it is
//     validated against this database only.
//   - Its absolute expiry IS its elevation's expiry, and it authenticates only
//     while that elevation is active: when the elevation ends (by its holder,
//     an approver, `break-glass end` or the reconciler), the next request of
//     the session is unauthenticated. adminaccess.EndElevation also revokes
//     the row in the same savepoint. The reverse is not true yet: the
//     session policy's idle maximum can end the session first, and its
//     elevation then grants nothing (no session can use it) but stays open
//     in the ledger until its expiry; ending an elevation with its session
//     is R2.11 (issue 2525).
//   - Its administration set is the elevation only (auth.CredentialKindSealed):
//     the owner's standing roles add nothing.
//
// A `__Host-` cookie needs HTTPS: in a development stack without TLS the
// browser refuses it, and the sealed path is unavailable there. The cookie is
// not weakened for it.
//
// No route calls StartSealedSession yet: the sign-in page is R2.7b.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// SealedCookieName is the cookie of a sealed session. The `__Host-` prefix
// makes the browser refuse it unless it is Secure, has Path=/ and no Domain,
// so no subdomain and no plain-HTTP page can set or overwrite it.
const SealedCookieName = "__Host-elitea_sealed"

// SealedElevationReader is the store read ValidateSealed needs: whether the
// session reference holds an active sealed elevation of its person at now.
// PostgresStore implements it; a store without it authenticates no sealed
// session (fail closed).
type SealedElevationReader interface {
	SealedElevationActive(ctx context.Context, sessionRef, userID int64, now time.Time) (bool, error)
}

// ErrSealedUnavailable is ValidateSealed's answer when the store cannot tell
// whether the elevation is active: no sealed session is admitted.
var ErrSealedUnavailable = errors.New("browsersession: the store cannot read sealed elevations")

// ValidateSealed answers the sealed session a SealedCookieName value names,
// and touches it. It refuses, as for an ordinary session, a value that is not
// an identifier, an unknown, revoked, ended, expired or idle row; and in
// addition a row that is not sealed (ErrNotFound: an ordinary session's
// identifier in the sealed cookie is no credential) and a sealed row whose
// elevation is no longer active (ErrRevoked). Nothing here calls out of the
// process.
func (m *Manager) ValidateSealed(ctx context.Context, cookieValue string) (Session, error) {
	if m == nil {
		return Session{}, ErrNotFound
	}
	reader, ok := m.store.(SealedElevationReader)
	if !ok {
		return Session{}, ErrSealedUnavailable
	}
	id, err := ParseCookieValue(cookieValue)
	if err != nil {
		return Session{}, err
	}
	session, err := m.store.Get(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if session.Provider != ProviderSealed {
		return Session{}, ErrNotFound
	}
	session, err = m.usable(ctx, id, session)
	if err != nil {
		return Session{}, err
	}
	active, err := reader.SealedElevationActive(ctx, session.Ref, session.UserID, m.now().UTC())
	if err != nil {
		return Session{}, fmt.Errorf("browsersession: read the sealed elevation: %w", err)
	}
	if !active {
		// The session ends with its elevation, whatever its own deadline.
		return Session{}, ErrRevoked
	}
	return session, nil
}

// SealedElevationActive reports whether the session reference holds an
// active elevation on the sealed path of the person: the resolver's
// predicate (approved, started, not ended, not expired at now).
func (s *PostgresStore) SealedElevationActive(ctx context.Context, sessionRef, userID int64, now time.Time) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM elitea_auth.admin_elevations
    WHERE browser_session_ref = $1 AND user_id = $2 AND path = 'sealed'
      AND decision = 'approved' AND started_at IS NOT NULL AND started_at <= $3
      AND ended_at IS NULL AND expires_at > $3)`, sessionRef, userID, now).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("browsersession: read the sealed elevation: %w", err)
	}
	return active, nil
}

// SealedStart is what starts a sealed session.
type SealedStart struct {
	// UserID is the person: the credential's bound user, or the reserved
	// account of an unbound credential (R2.6b).
	UserID int64
	// Level is the level of the elevation: the credential's.
	Level string
	// ReasonCode and Reason are the elevation's reason (the sign-in page's).
	ReasonCode string
	Reason     string
	Ticket     string
	// At is the start, from the caller's clock. Required.
	At time.Time
	// Duration is the elevation's lifetime, at most the sealed ceiling
	// (adminaccess.MaxElevationDurationSealed); the session's absolute
	// expiry is the same instant.
	Duration time.Duration
	// Source is the source of the `started` event; "" is console.
	Source        string
	CorrelationID string
}

// SealedSession is a started sealed session.
type SealedSession struct {
	// CookieValue is what SetSealedCookie writes; it is the credential of
	// the session and is shown to nobody else.
	CookieValue string
	Session     Session
	Elevation   adminaccess.ElevationRecord
}

// StartSealedSession creates a sealed session for a person and starts its
// elevation, at the given level, bound to it, in the caller's transaction:
// the session row, the elevation row and the elevation's `started` event are
// one savepoint, so a failure of any (the audit appender included) leaves
// neither a session nor an elevation, even when the caller commits after the
// error. The session's absolute expiry is the elevation's expiry, it has no
// idle limit of its own (the administrator's session policy can still
// shorten it), and it is never touched by an identity provider.
//
// No route calls it before the sign-in page (R2.7b), which consumes the
// credential in the same transaction.
func StartSealedSession(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, start SealedStart) (SealedSession, error) {
	if tx == nil {
		return SealedSession{}, errors.New("browsersession: start a sealed session: no transaction")
	}
	if appender == nil {
		return SealedSession{}, adminaccess.ErrElevationNoAppender
	}
	if start.UserID <= 0 {
		return SealedSession{}, fmt.Errorf("browsersession: user id %d is invalid", start.UserID)
	}
	if start.At.IsZero() {
		return SealedSession{}, adminaccess.ErrElevationNoTimestamp
	}
	at := start.At.UTC()
	elevationStart := adminaccess.ElevationStart{
		UserID: start.UserID, BrowserSessionRef: 1, Level: start.Level, Path: adminaccess.ElevationPathSealed,
		ReasonCode: start.ReasonCode, Reason: start.Reason, Ticket: start.Ticket,
		At: at, Duration: start.Duration, Source: start.Source, CorrelationID: start.CorrelationID,
	}
	// Everything the ledger would refuse, refused before any row is written.
	if err := elevationStart.Validate(); err != nil {
		return SealedSession{}, err
	}
	id, err := NewID()
	if err != nil {
		return SealedSession{}, err
	}
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return SealedSession{}, fmt.Errorf("browsersession: sealed session savepoint: %w", err)
	}
	session, elevation, err := startSealed(ctx, savepoint, appender, id, at, elevationStart)
	if err != nil {
		if rollbackErr := savepoint.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
			return SealedSession{}, errors.Join(err, fmt.Errorf("browsersession: roll back the sealed session: %w", rollbackErr))
		}
		return SealedSession{}, err
	}
	if err := savepoint.Commit(ctx); err != nil {
		return SealedSession{}, fmt.Errorf("browsersession: release the sealed session savepoint: %w", err)
	}
	return SealedSession{CookieValue: CookieValue(id), Session: session, Elevation: elevation}, nil
}

func startSealed(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, id string, at time.Time,
	start adminaccess.ElevationStart,
) (Session, adminaccess.ElevationRecord, error) {
	session := Session{
		ID: id, UserID: start.UserID, Provider: ProviderSealed,
		CreatedAt: at, AuthenticatedAt: at, LastSeenAt: at, ExpiresAt: at.Add(start.Duration),
	}
	// The sealed sign-in is an authentication at `at`, but not one the
	// recent-authentication check of a single sign-on plane can vouch for:
	// authentication_proven stays false.
	err := tx.QueryRow(ctx, `
INSERT INTO elitea_auth.browser_sessions (
    id, user_id, email, provider, provider_session_index,
    created_at, last_seen_at, expires_at, idle_timeout_seconds, authenticated_at, authentication_proven)
SELECT $1, account.id, COALESCE(account.email, ''), $3, '', $4, $4, $5, 0, $4, false
FROM public.auth_core__user account WHERE account.id = $2
RETURNING ref, email`, id, start.UserID, ProviderSealed, at, session.ExpiresAt).Scan(&session.Ref, &session.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, adminaccess.ElevationRecord{}, adminaccess.ErrElevationPerson
	}
	if err != nil {
		return Session{}, adminaccess.ElevationRecord{}, fmt.Errorf("browsersession: insert the sealed session: %w", err)
	}
	start.BrowserSessionRef = session.Ref
	elevation, err := adminaccess.StartElevation(ctx, tx, appender, start)
	if err != nil {
		return Session{}, adminaccess.ElevationRecord{}, err
	}
	if !elevation.ExpiresAt.Equal(session.ExpiresAt) {
		return Session{}, adminaccess.ElevationRecord{}, fmt.Errorf(
			"browsersession: the sealed session expires at %s and its elevation at %s",
			session.ExpiresAt.Format(time.RFC3339Nano), elevation.ExpiresAt.Format(time.RFC3339Nano))
	}
	return session, elevation, nil
}

// SetSealedCookie writes a sealed session's cookie: `__Host-`, Secure,
// HttpOnly, SameSite=Strict, Path=/, no Domain, and a MaxAge that ends with
// the elevation. Secure is not a parameter: the prefix requires it.
func SetSealedCookie(w http.ResponseWriter, value string, lifetime time.Duration) {
	maxAge := int(lifetime / time.Second)
	if maxAge <= 0 {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SealedCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	})
}

// ClearSealedCookie writes the expiry of the sealed cookie, with the same
// attributes, so the browser replaces the cookie rather than keeping it.
func ClearSealedCookie(w http.ResponseWriter) {
	SetSealedCookie(w, "", 0)
}

// IsSealedCookie reports whether a cookie name is the sealed session's.
func IsSealedCookie(name string) bool { return strings.EqualFold(name, SealedCookieName) }
