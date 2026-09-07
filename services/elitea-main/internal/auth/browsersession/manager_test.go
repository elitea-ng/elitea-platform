package browsersession

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// memoryStore is the Store an in-process test drives. It records every write so
// the touch throttle can be counted, which is the one property of this package
// that a database test could only observe indirectly.
type memoryStore struct {
	mu       sync.Mutex
	rows     map[string]Session
	touches  int
	inserts  int
	getErr   error
	touchErr error
}

func newMemoryStore() *memoryStore { return &memoryStore{rows: map[string]Session{}} }

func (s *memoryStore) Insert(_ context.Context, session Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inserts++
	s.rows[session.ID] = session
	return nil
}

func (s *memoryStore) Get(_ context.Context, id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return Session{}, s.getErr
	}
	row, ok := s.rows[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	return row, nil
}

func (s *memoryStore) Touch(_ context.Context, id string, seenAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.touchErr != nil {
		return s.touchErr
	}
	s.touches++
	row, ok := s.rows[id]
	if !ok {
		return nil
	}
	row.LastSeenAt = seenAt
	s.rows[id] = row
	return nil
}

func (s *memoryStore) Revoke(_ context.Context, id string, revokedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok || row.RevokedAt != nil {
		return nil
	}
	row.RevokedAt = &revokedAt
	s.rows[id] = row
	return nil
}

func (s *memoryStore) RevokeAllForUser(_ context.Context, userID int64, revokedAt time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int64
	for id, row := range s.rows {
		if row.UserID != userID || row.RevokedAt != nil {
			continue
		}
		row.RevokedAt = &revokedAt
		s.rows[id] = row
		count++
	}
	return count, nil
}

// clock is a hand-wound clock. Expiry and the touch throttle are both clock
// behaviour, and neither can be proved against time.Now without sleeping.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestManager(t *testing.T, policy Policy) (*Manager, *memoryStore, *clock) {
	t.Helper()
	store := newMemoryStore()
	dial := &clock{now: time.Date(2026, time.September, 7, 9, 0, 0, 0, time.UTC)}
	manager, err := NewManager(store, policy, WithClock(dial.Now))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager, store, dial
}

// TestSessionLifecycle is the whole contract in one table: a fresh session
// authenticates, an idle one does not, one past its absolute deadline does not,
// and a revoked one does not — and each refusal NAMES itself, because the
// middleware and `/forward-auth/info` both switch on which one it was.
func TestSessionLifecycle(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		advance time.Duration
		revoke  bool
		want    error
	}{
		{name: "a fresh session authenticates", advance: 0, want: nil},
		{name: "activity inside the idle window keeps it", advance: 30 * time.Minute, want: nil},
		{name: "a session left alone past the idle window is refused", advance: 2 * time.Hour, want: ErrIdle},
		{name: "a revoked session is refused", revoke: true, want: ErrRevoked},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			manager, _, dial := newTestManager(t, Policy{
				IdleTimeout: time.Hour, AbsoluteLifetime: 24 * time.Hour,
			})
			value, err := manager.Create(context.Background(), NewSession{
				UserID: 7, Email: "owner@example.test", Provider: ProviderOIDC,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if !strings.HasPrefix(value, CookieValuePrefix) {
				t.Fatalf("cookie value %q does not carry the server-side prefix", value)
			}
			if test.revoke {
				if err := manager.Revoke(context.Background(), value); err != nil {
					t.Fatalf("Revoke: %v", err)
				}
			}
			dial.advance(test.advance)

			session, err := manager.Validate(context.Background(), value)
			if !errors.Is(err, test.want) {
				t.Fatalf("Validate error = %v, want %v", err, test.want)
			}
			if test.want != nil {
				return
			}
			if session.UserID != 7 || session.Email != "owner@example.test" {
				t.Fatalf("session = %+v, want user 7", session)
			}
			if user := session.User(); user.UserID != "7" || user.AuthType != "session" {
				t.Fatalf("principal = %+v, want user id 7 from a session", user)
			}
		})
	}
}

// TestAbsoluteDeadlineIsNotMovedByActivity is the property an idle window alone
// cannot give. A browser used every minute for a week still has to sign in
// again, which is the whole point of having two deadlines.
func TestAbsoluteDeadlineIsNotMovedByActivity(t *testing.T) {
	t.Parallel()
	manager, _, dial := newTestManager(t, Policy{
		IdleTimeout: time.Hour, AbsoluteLifetime: 3 * time.Hour,
	})
	value, err := manager.Create(context.Background(), NewSession{
		UserID: 7, Provider: ProviderSAML,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Used continuously — every half hour, well inside the idle window.
	for range 5 {
		dial.advance(30 * time.Minute)
		if _, err := manager.Validate(context.Background(), value); err != nil {
			t.Fatalf("Validate inside the absolute lifetime: %v", err)
		}
	}
	dial.advance(time.Hour)
	if _, err := manager.Validate(context.Background(), value); !errors.Is(err, ErrExpired) {
		t.Fatalf("Validate error = %v, want ErrExpired", err)
	}
}

// TestTouchIsThrottled is why the SPA's request volume does not become one
// UPDATE per request. Ten validations inside a minute write once.
func TestTouchIsThrottled(t *testing.T) {
	t.Parallel()
	manager, store, dial := newTestManager(t, Policy{
		IdleTimeout: time.Hour, AbsoluteLifetime: 24 * time.Hour,
	})
	value, err := manager.Create(context.Background(), NewSession{UserID: 7, Provider: ProviderOIDC})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for range 10 {
		dial.advance(2 * time.Second)
		if _, err := manager.Validate(context.Background(), value); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	}
	if store.touches != 0 {
		t.Fatalf("touches inside the throttle window = %d, want 0", store.touches)
	}
	dial.advance(TouchInterval)
	if _, err := manager.Validate(context.Background(), value); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if store.touches != 1 {
		t.Fatalf("touches after the throttle window = %d, want 1", store.touches)
	}
}

// TestATouchFailureDoesNotRefuseAValidSession. The row was READ, so the answer
// about the caller is already known; a failed write of last_seen_at must not
// turn that answer into a sign-out.
func TestATouchFailureDoesNotRefuseAValidSession(t *testing.T) {
	t.Parallel()
	manager, store, dial := newTestManager(t, Policy{
		IdleTimeout: time.Hour, AbsoluteLifetime: 24 * time.Hour,
	})
	value, err := manager.Create(context.Background(), NewSession{UserID: 7, Provider: ProviderOIDC})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store.touchErr = errors.New("connection refused")
	dial.advance(2 * TouchInterval)
	if _, err := manager.Validate(context.Background(), value); err != nil {
		t.Fatalf("Validate = %v, want the session to stay usable", err)
	}
}

// TestAStoreOutageIsNotASignOut. ErrNotFound would make the middleware answer
// 401 and sign out every browser for as long as the database is unreachable.
func TestAStoreOutageIsNotASignOut(t *testing.T) {
	t.Parallel()
	manager, store, _ := newTestManager(t, Policy{AbsoluteLifetime: 24 * time.Hour})
	value, err := manager.Create(context.Background(), NewSession{UserID: 7, Provider: ProviderForm})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store.getErr = errors.New("connection refused")
	_, err = manager.Validate(context.Background(), value)
	switch {
	case err == nil:
		t.Fatal("a store outage authenticated the caller")
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrExpired),
		errors.Is(err, ErrIdle), errors.Is(err, ErrRevoked):
		t.Fatalf("a store outage was classified as %v, which reads as a sign-out", err)
	}
}

// TestRevokeUserEndsEverySessionOfOneAccount is what "sign this person out
// everywhere" needs, and what a suspension should call.
func TestRevokeUserEndsEverySessionOfOneAccount(t *testing.T) {
	t.Parallel()
	manager, _, _ := newTestManager(t, Policy{AbsoluteLifetime: 24 * time.Hour})
	first, err := manager.Create(context.Background(), NewSession{UserID: 7, Provider: ProviderOIDC})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := manager.Create(context.Background(), NewSession{UserID: 7, Provider: ProviderOIDC})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	other, err := manager.Create(context.Background(), NewSession{UserID: 8, Provider: ProviderOIDC})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	count, err := manager.RevokeUser(context.Background(), 7)
	if err != nil || count != 2 {
		t.Fatalf("RevokeUser = %d, %v; want 2, nil", count, err)
	}
	for _, value := range []string{first, second} {
		if _, err := manager.Validate(context.Background(), value); !errors.Is(err, ErrRevoked) {
			t.Fatalf("Validate after revocation = %v, want ErrRevoked", err)
		}
	}
	if _, err := manager.Validate(context.Background(), other); err != nil {
		t.Fatalf("another account's session was revoked too: %v", err)
	}
}

// TestCookieFormatsCannotBeConfused is the discriminator the middleware and
// `/forward-auth/info` both depend on. A legacy value must never look like a
// server-side identifier, or the wrong reader runs and every browser is
// refused.
func TestCookieFormatsCannotBeConfused(t *testing.T) {
	t.Parallel()

	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if !LooksServerSide(CookieValue(id)) {
		t.Fatal("a freshly minted cookie value is not recognised as server-side")
	}
	parsed, err := ParseCookieValue(CookieValue(id))
	if err != nil || parsed != id {
		t.Fatalf("ParseCookieValue = %q, %v; want %q", parsed, err, id)
	}

	// The legacy shape: base64url of a JSON object, a dot, and a hex MAC. Its
	// first segment always begins `ey`, which the prefix test cannot match.
	for _, legacy := range []string{
		"eyJ1aWQiOiI3IiwiZXhwIjoxfQ.deadbeef",
		"",
		"s1.not-base64!!",
		"s1." + id[:8],
	} {
		if legacy != "" && LooksServerSide(legacy) {
			if _, err := ParseCookieValue(legacy); err == nil {
				t.Fatalf("%q was accepted as a session identifier", legacy)
			}
			continue
		}
		if _, err := ParseCookieValue(legacy); !errors.Is(err, ErrMalformedCookie) {
			t.Fatalf("ParseCookieValue(%q) = %v, want ErrMalformedCookie", legacy, err)
		}
	}
}

// TestPolicyFromEnvRefusesAMistypedLifetime. A deployment that meant to set a
// lifetime and mistyped it must not run for a week believing it did.
func TestPolicyFromEnvRefusesAMistypedLifetime(t *testing.T) {
	for _, test := range []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(*testing.T, Policy)
	}{
		{
			name: "the defaults",
			check: func(t *testing.T, policy Policy) {
				if policy.IdleTimeout != DefaultIdleTimeout ||
					policy.AbsoluteLifetime != DefaultAbsoluteLifetime ||
					policy.RejectLegacyCookies {
					t.Fatalf("policy = %+v, want the documented defaults", policy)
				}
			},
		},
		{
			name: "an authored pair",
			env: map[string]string{
				"ELITEA_SESSION_IDLE_TIMEOUT":          "30m",
				"ELITEA_SESSION_ABSOLUTE_LIFETIME":     "12h",
				"ELITEA_SESSION_REJECT_LEGACY_COOKIES": "true",
			},
			check: func(t *testing.T, policy Policy) {
				if policy.IdleTimeout != 30*time.Minute ||
					policy.AbsoluteLifetime != 12*time.Hour || !policy.RejectLegacyCookies {
					t.Fatalf("policy = %+v, want the authored values", policy)
				}
			},
		},
		{
			name:    "minutes are not a duration",
			env:     map[string]string{"ELITEA_SESSION_IDLE_TIMEOUT": "30"},
			wantErr: true,
		},
		{
			name:    "an absolute lifetime under a minute",
			env:     map[string]string{"ELITEA_SESSION_ABSOLUTE_LIFETIME": "5s"},
			wantErr: true,
		},
		{
			name: "an idle window that can never fire",
			env: map[string]string{
				"ELITEA_SESSION_IDLE_TIMEOUT":      "48h",
				"ELITEA_SESSION_ABSOLUTE_LIFETIME": "24h",
			},
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for name, value := range test.env {
				t.Setenv(name, value)
			}
			policy, err := PolicyFromEnv()
			if test.wantErr {
				if err == nil {
					t.Fatalf("PolicyFromEnv accepted %v", test.env)
				}
				return
			}
			if err != nil {
				t.Fatalf("PolicyFromEnv: %v", err)
			}
			test.check(t, policy)
		})
	}
}

// TestANilManagerIsAWorkingNoValue. Every consumer holds a possibly-nil
// *Manager and none of them branches on it.
func TestANilManagerIsAWorkingNoValue(t *testing.T) {
	t.Parallel()
	var manager *Manager
	if manager.Policy().RejectLegacyCookies {
		t.Fatal("a nil manager reported a policy")
	}
	if err := manager.Revoke(context.Background(), "s1.anything"); err != nil {
		t.Fatalf("Revoke on a nil manager: %v", err)
	}
	if _, err := manager.Validate(context.Background(), "s1.anything"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Validate on a nil manager = %v, want ErrNotFound", err)
	}
	if _, err := manager.Create(context.Background(), NewSession{UserID: 1, Provider: ProviderOIDC}); err == nil {
		t.Fatal("a nil manager minted a session")
	}
}
