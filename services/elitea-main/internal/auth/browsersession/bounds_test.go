package browsersession

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// endingStore is a memoryStore that also records MarkEnded (EndMarker).
type endingStore struct {
	*memoryStore
	mu    sync.Mutex
	ended map[string]string
	at    map[string]time.Time
}

func (s *endingStore) MarkEnded(_ context.Context, id, reason string, endedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, done := s.ended[id]; done {
		return nil
	}
	s.ended[id], s.at[id] = reason, endedAt
	// The row records the end, as the Postgres store's UPDATE does.
	s.memoryStore.mu.Lock()
	defer s.memoryStore.mu.Unlock()
	if row, ok := s.rows[id]; ok && row.EndedAt == nil && row.RevokedAt == nil {
		row.EndedAt, row.EndReason = &endedAt, reason
		s.rows[id] = row
	}
	return nil
}

func newBoundedManager(t *testing.T, policy Policy, bounds *Bounds) (*Manager, *endingStore, *clock) {
	t.Helper()
	store := &endingStore{memoryStore: newMemoryStore(), ended: map[string]string{}, at: map[string]time.Time{}}
	dial := &clock{now: time.Date(2026, time.October, 29, 9, 0, 0, 0, time.UTC)}
	manager, err := NewManager(store, policy, WithClock(dial.Now),
		WithBounds(BoundsFunc(func(context.Context) (Bounds, error) { return *bounds, nil })))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager, store, dial
}

func TestBoundsClamp(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                   string
		bounds                 Bounds
		idle, lifetime         time.Duration
		wantIdle, wantLifetime time.Duration
	}{
		{"no bounds keep the policy", Bounds{}, 0, 7 * 24 * time.Hour, 0, 7 * 24 * time.Hour},
		{"no idle limit becomes the maximum", Bounds{MaxIdleTimeout: time.Hour}, 0, 24 * time.Hour, time.Hour, 24 * time.Hour},
		{"a longer idle is shortened", Bounds{MaxIdleTimeout: time.Hour}, 8 * time.Hour, 24 * time.Hour, time.Hour, 24 * time.Hour},
		{"a shorter idle is kept", Bounds{MaxIdleTimeout: 8 * time.Hour}, time.Hour, 24 * time.Hour, time.Hour, 24 * time.Hour},
		{"a longer lifetime is shortened", Bounds{MaxAbsoluteLifetime: 12 * time.Hour}, time.Hour, 7 * 24 * time.Hour, time.Hour, 12 * time.Hour},
		{"a shorter lifetime is kept", Bounds{MaxAbsoluteLifetime: 30 * 24 * time.Hour}, time.Hour, 24 * time.Hour, time.Hour, 24 * time.Hour},
	} {
		idle, lifetime := test.bounds.Clamp(test.idle, test.lifetime)
		if idle != test.wantIdle || lifetime != test.wantLifetime {
			t.Errorf("%s: Clamp = (%s, %s), want (%s, %s)", test.name, idle, lifetime, test.wantIdle, test.wantLifetime)
		}
	}
}

func TestDeadlineNamesTheEarlierDeadline(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	row := Session{CreatedAt: created, LastSeenAt: created.Add(time.Hour), ExpiresAt: created.Add(7 * 24 * time.Hour)}

	if at, reason := row.Deadline(Bounds{}); reason != EndAbsolute || !at.Equal(row.ExpiresAt) {
		t.Errorf("unbounded, no idle: (%s, %s)", at, reason)
	}
	if at, reason := row.Deadline(Bounds{MaxIdleTimeout: 30 * time.Minute}); reason != EndIdle ||
		!at.Equal(created.Add(90*time.Minute)) {
		t.Errorf("idle maximum on a no-idle row: (%s, %s)", at, reason)
	}
	if at, reason := row.Deadline(Bounds{MaxAbsoluteLifetime: 2 * time.Hour}); reason != EndAbsolute ||
		!at.Equal(created.Add(2*time.Hour)) {
		t.Errorf("lifetime maximum: (%s, %s)", at, reason)
	}
	// A raised maximum never lengthens the row's own deadline.
	if at, _ := row.Deadline(Bounds{MaxAbsoluteLifetime: 30 * 24 * time.Hour}); !at.Equal(row.ExpiresAt) {
		t.Errorf("a longer maximum moved the row's deadline to %s", at)
	}
}

// TestNewSessionIsClampedToTheBounds: the row states the clamped lifetimes,
// and the cookie lifetime is the clamped one.
func TestNewSessionIsClampedToTheBounds(t *testing.T) {
	t.Parallel()
	bounds := Bounds{MaxIdleTimeout: 30 * time.Minute, MaxAbsoluteLifetime: 12 * time.Hour}
	manager, store, dial := newBoundedManager(t, Policy{IdleTimeout: 0, AbsoluteLifetime: 7 * 24 * time.Hour}, &bounds)

	value, lifetime, err := manager.CreateCookie(context.Background(),
		NewSession{UserID: 7, Email: "a@example.com", Provider: ProviderOIDC})
	if err != nil {
		t.Fatalf("CreateCookie: %v", err)
	}
	if lifetime != 12*time.Hour {
		t.Fatalf("cookie lifetime = %s, want 12h", lifetime)
	}
	id, _ := ParseCookieValue(value)
	row := store.rows[id]
	if row.IdleTimeout != 30*time.Minute || !row.ExpiresAt.Equal(dial.Now().Add(12*time.Hour)) {
		t.Fatalf("row stamped idle %s expires %s", row.IdleTimeout, row.ExpiresAt)
	}
}

// TestExistingSessionIsHeldToANewMaximum: a row issued with no idle limit
// before the administrator set one is refused once it passes the maximum,
// and its end is recorded at the deadline with the reason.
func TestExistingSessionIsHeldToANewMaximum(t *testing.T) {
	t.Parallel()
	bounds := Bounds{}
	manager, store, dial := newBoundedManager(t, Policy{IdleTimeout: 0, AbsoluteLifetime: 7 * 24 * time.Hour}, &bounds)
	value, err := manager.Create(context.Background(), NewSession{UserID: 7, Provider: ProviderOIDC})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id, _ := ParseCookieValue(value)
	created := dial.Now()

	dial.advance(3 * time.Hour)
	if _, err := manager.Validate(context.Background(), value); err != nil {
		t.Fatalf("an unbounded session with no idle limit was refused: %v", err)
	}

	bounds = Bounds{MaxIdleTimeout: time.Hour}
	dial.advance(2 * time.Hour)
	if _, err := manager.Validate(context.Background(), value); !errors.Is(err, ErrIdle) {
		t.Fatalf("Validate after the new maximum = %v, want ErrIdle", err)
	}
	if store.ended[id] != EndIdle || !store.at[id].Equal(created.Add(4*time.Hour)) {
		t.Fatalf("end recorded as %q at %s", store.ended[id], store.at[id])
	}

	// The absolute maximum applies to an old row too.
	bounds = Bounds{MaxAbsoluteLifetime: time.Hour}
	value2, err := manager.Create(context.Background(), NewSession{UserID: 8, Provider: ProviderSAML})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bounds = Bounds{MaxAbsoluteLifetime: 30 * time.Minute}
	dial.advance(31 * time.Minute)
	if _, err := manager.Validate(context.Background(), value2); !errors.Is(err, ErrExpired) {
		t.Fatalf("Validate past a tightened lifetime = %v, want ErrExpired", err)
	}
}

// TestAnEndedSessionStaysEnded: a session ended under a bound (and audited
// as expired) does not authenticate again when the administrator later
// raises or removes the bound, although its own row deadlines are still
// ahead.
func TestAnEndedSessionStaysEnded(t *testing.T) {
	t.Parallel()
	bounds := Bounds{MaxIdleTimeout: 30 * time.Minute}
	manager, store, dial := newBoundedManager(t, Policy{IdleTimeout: 0, AbsoluteLifetime: 7 * 24 * time.Hour}, &bounds)
	value, err := manager.Create(context.Background(), NewSession{UserID: 7, Provider: ProviderOIDC})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id, _ := ParseCookieValue(value)

	dial.advance(time.Hour)
	if _, err := manager.Validate(context.Background(), value); !errors.Is(err, ErrIdle) {
		t.Fatalf("Validate past the idle maximum = %v, want ErrIdle", err)
	}
	if store.ended[id] != EndIdle {
		t.Fatalf("end recorded as %q, want %q", store.ended[id], EndIdle)
	}

	bounds = Bounds{}
	if _, err := manager.Validate(context.Background(), value); !errors.Is(err, ErrIdle) {
		t.Fatalf("Validate after the bound was removed = %v, want ErrIdle (ended stays ended)", err)
	}

	// The reason maps to the refusal it was ended for.
	ended := dial.Now()
	for reason, want := range map[string]error{
		EndIdle: ErrIdle, EndAbsolute: ErrExpired, EndLogout: ErrRevoked, EndRevoked: ErrRevoked,
	} {
		row := Session{ExpiresAt: ended.Add(time.Hour), LastSeenAt: ended, EndedAt: &ended, EndReason: reason}
		if err := row.UsableUnder(ended, Bounds{}); !errors.Is(err, want) {
			t.Errorf("ended %q = %v, want %v", reason, err, want)
		}
	}
}

// TestUnreadableBoundsFailClosed: a policy that cannot be read refuses the
// login and the validation instead of dropping the bound.
func TestUnreadableBoundsFailClosed(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	broken := errors.New("platform_config unreachable")
	manager, err := NewManager(store, Policy{AbsoluteLifetime: time.Hour},
		WithBounds(BoundsFunc(func(context.Context) (Bounds, error) { return Bounds{}, broken })))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.Create(context.Background(), NewSession{UserID: 1, Provider: ProviderForm}); !errors.Is(err, broken) {
		t.Fatalf("Create = %v, want the read error", err)
	}
	if store.inserts != 0 {
		t.Fatal("a session was written with an unread policy")
	}
}

func TestSessionUserCarriesTheReference(t *testing.T) {
	t.Parallel()
	if got := (Session{UserID: 3, Ref: 42}).User().BrowserSessionRef; got != 42 {
		t.Fatalf("BrowserSessionRef = %d, want 42", got)
	}
}
