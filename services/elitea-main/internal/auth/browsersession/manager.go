package browsersession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// Store is the persistence this package needs, declared at the consumer.
//
// PostgresStore is the only implementation. The interface exists so the
// middleware and handler tests can drive create/validate/expire/revoke without
// a database, which is what makes the touch throttle testable at all.
type Store interface {
	Insert(ctx context.Context, session Session) error
	Get(ctx context.Context, id string) (Session, error)
	Touch(ctx context.Context, id string, seenAt time.Time) error
	Revoke(ctx context.Context, id string, revokedAt time.Time) error
	RevokeAllForUser(ctx context.Context, userID int64, revokedAt time.Time) (int64, error)
}

// TouchInterval is the smallest gap between two writes of last_seen_at.
//
// WITHOUT IT EVERY AUTHENTICATED REQUEST IS A WRITE. The SPA issues tens of
// requests per screen and polls in the background, so a per-request UPDATE
// would put the whole browser traffic of the deployment onto one row per user
// and would make an idle timeout cost more than the feature is worth. A minute
// is far below the shortest idle window an operator would configure, so the
// coarser clock cannot expire a session that is in use.
const TouchInterval = time.Minute

// Manager is the whole lifecycle: mint, validate, touch, revoke.
//
// A nil *Manager is a working "no server sessions" value on every method, so a
// composition that could not build one needs no branch at the call site. That
// matters because the same handlers serve deployments with and without a pool.
type Manager struct {
	store  Store
	policy Policy
	now    func() time.Time
	logger *slog.Logger
}

// NewManager builds the manager. A nil store is refused rather than tolerated:
// a manager that cannot persist would mint cookies that authenticate nobody.
func NewManager(store Store, policy Policy, options ...ManagerOption) (*Manager, error) {
	if store == nil {
		return nil, errors.New("browsersession: a store is required")
	}
	if policy.AbsoluteLifetime <= 0 {
		policy.AbsoluteLifetime = DefaultAbsoluteLifetime
	}
	manager := &Manager{store: store, policy: policy, now: time.Now, logger: slog.Default()}
	for _, option := range options {
		option(manager)
	}
	return manager, nil
}

// ManagerOption configures a Manager at construction.
type ManagerOption func(*Manager)

// WithClock replaces the clock. Only a test uses it; expiry and the touch
// throttle are both clock behaviour and cannot be proved against time.Now.
func WithClock(now func() time.Time) ManagerOption {
	return func(m *Manager) {
		if now != nil {
			m.now = now
		}
	}
}

// WithLogger replaces the logger.
func WithLogger(logger *slog.Logger) ManagerOption {
	return func(m *Manager) {
		if logger != nil {
			m.logger = logger
		}
	}
}

// Policy reports the lifetimes this manager stamps. Nil answers the zero
// policy, which is what "no server sessions" means.
func (m *Manager) Policy() Policy {
	if m == nil {
		return Policy{}
	}
	return m.policy
}

// NewSession is what a login plane knows at the moment it signs somebody in.
type NewSession struct {
	UserID               int64
	Email                string
	Provider             string
	ProviderSessionIndex string
}

// Create writes one session row and returns the COOKIE VALUE for it.
//
// It returns the cookie value rather than the identifier so that no caller has
// to remember the prefix. Four handlers set this cookie; four spellings of the
// same concatenation is how one of them would end up writing a value the
// reader refuses.
func (m *Manager) Create(ctx context.Context, request NewSession) (string, error) {
	if m == nil {
		return "", errors.New("browsersession: no manager is configured")
	}
	if request.UserID <= 0 {
		return "", fmt.Errorf("browsersession: user id %d is invalid", request.UserID)
	}
	switch request.Provider {
	case ProviderOIDC, ProviderSAML, ProviderForm:
	default:
		return "", fmt.Errorf("browsersession: provider %q is not one this store accepts", request.Provider)
	}

	id, err := NewID()
	if err != nil {
		return "", err
	}
	now := m.now().UTC()
	session := Session{
		ID:                   id,
		UserID:               request.UserID,
		Email:                request.Email,
		Provider:             request.Provider,
		ProviderSessionIndex: request.ProviderSessionIndex,
		CreatedAt:            now,
		LastSeenAt:           now,
		ExpiresAt:            now.Add(m.policy.AbsoluteLifetime),
		IdleTimeout:          m.policy.IdleTimeout,
	}
	if err := m.store.Insert(ctx, session); err != nil {
		return "", err
	}
	m.logger.InfoContext(ctx, "browser session created",
		"user_id", request.UserID, "provider", request.Provider,
		"expires_at", session.ExpiresAt, "idle_timeout", m.policy.IdleTimeout)
	return CookieValue(id), nil
}

// Validate answers the session a cookie value names, and touches it.
//
// THE TOUCH IS BEST EFFORT. It moves last_seen_at, which is the idle clock, and
// a write that fails must not refuse a caller whose session is valid: the row
// was READ successfully, so the answer about the caller is already known. The
// failure is logged and the request proceeds; the worst case is that a session
// in continuous use expires on its idle window, which is a sign-out, not an
// unsafe admission.
//
// It returns ErrNotFound for a value that is not a server-side identifier at
// all, so a caller that already chose this reader cannot be told "malformed"
// for a cookie it decided to bring here.
func (m *Manager) Validate(ctx context.Context, cookieValue string) (Session, error) {
	if m == nil {
		return Session{}, ErrNotFound
	}
	id, err := ParseCookieValue(cookieValue)
	if err != nil {
		return Session{}, err
	}
	session, err := m.store.Get(ctx, id)
	if err != nil {
		return Session{}, err
	}
	now := m.now().UTC()
	if err := session.Usable(now); err != nil {
		return Session{}, err
	}
	if now.Sub(session.LastSeenAt) >= TouchInterval {
		if touchErr := m.store.Touch(ctx, id, now); touchErr != nil {
			m.logger.WarnContext(ctx, "browser session last_seen_at was not updated",
				"user_id", session.UserID, "err", touchErr)
		} else {
			session.LastSeenAt = now
		}
	}
	return session, nil
}

// User is the principal a validated session names.
//
// It carries no permission and no token id. apimw.Auth hands it to the
// PrincipalValidator, which is what decides whether the account behind it is
// still active — the session row says the session is alive, not that the user
// is.
func (s Session) User() auth.User {
	id := fmt.Sprintf("%d", s.UserID)
	return auth.User{
		ID:       id,
		UserID:   id,
		Email:    s.Email,
		AuthType: "session",
	}
}

// Revoke ends the session a cookie value names. It is idempotent: revoking an
// unknown or already-revoked session is not an error, because logout must
// never fail on the state it is trying to reach.
func (m *Manager) Revoke(ctx context.Context, cookieValue string) error {
	if m == nil {
		return nil
	}
	id, err := ParseCookieValue(cookieValue)
	if err != nil {
		return nil
	}
	return m.store.Revoke(ctx, id, m.now().UTC())
}

// RevokeUser ends every session of one account and reports how many it ended.
// It is what an operator's "sign this person out everywhere" needs, and what a
// suspension should call.
func (m *Manager) RevokeUser(ctx context.Context, userID int64) (int64, error) {
	if m == nil {
		return 0, nil
	}
	return m.store.RevokeAllForUser(ctx, userID, m.now().UTC())
}

// ClearCookie writes the expiry of the session cookie.
//
// The attributes must match the ones Create's caller set, or the browser keeps
// the original cookie beside the expired one. Both spellings live here so they
// cannot drift.
func ClearCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// SetCookie writes a freshly created session's cookie.
//
// MaxAge is the ABSOLUTE lifetime, not the idle one. A browser that discards
// the cookie at the idle deadline would sign a user out who was about to come
// back inside their idle window, and the server decides idle expiry anyway.
func SetCookie(w http.ResponseWriter, value string, secure bool, lifetime time.Duration) {
	if lifetime <= 0 {
		lifetime = DefaultAbsoluteLifetime
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(lifetime / time.Second),
	})
}
