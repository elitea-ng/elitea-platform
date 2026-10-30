package platformconfig

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"
)

// SectionSessionPolicy is the administrator's bound on browser sessions
// (ADR-0042 decision 5, #1496). The admin Configuration page writes it;
// browsersession.Manager reads it when it creates a session and every time
// it validates one.
//
// Each key is a MAXIMUM. 0 (the default) sets no maximum, so a deployment
// that never saves the section keeps the lifetimes its environment gives
// (ELITEA_SESSION_IDLE_TIMEOUT, ELITEA_SESSION_ABSOLUTE_LIFETIME).
const SectionSessionPolicy = "session_policy"

// Field keys of SectionSessionPolicy.
const (
	KeySessionMaxIdleTimeoutSeconds      = "max_idle_timeout_seconds"
	KeySessionMaxAbsoluteLifetimeSeconds = "max_absolute_lifetime_seconds"
)

// Bounds of SectionSessionPolicy. A minute is the smallest maximum: a
// smaller one signs every user out between two clicks. The largest values
// are a month idle and a year absolute.
const (
	MinSessionBoundSeconds            = 60
	MaxSessionIdleTimeoutSeconds      = 30 * 24 * 60 * 60
	MaxSessionAbsoluteLifetimeSeconds = 365 * 24 * 60 * 60
)

// SessionPolicy is the section. A zero duration is "no maximum".
type SessionPolicy struct {
	MaxIdleTimeout      time.Duration
	MaxAbsoluteLifetime time.Duration
}

// SessionPolicyFromValues is the overlay. An absent key or 0 is no maximum.
// A present value out of range is clamped into range, and an unreadable one
// is the smallest maximum: a hand-written row can tighten a bound and never
// silently remove it.
func SessionPolicyFromValues(values Values) SessionPolicy {
	return SessionPolicy{
		MaxIdleTimeout: boundSeconds(values, KeySessionMaxIdleTimeoutSeconds,
			MaxSessionIdleTimeoutSeconds),
		MaxAbsoluteLifetime: boundSeconds(values, KeySessionMaxAbsoluteLifetimeSeconds,
			MaxSessionAbsoluteLifetimeSeconds),
	}
}

// LoadSessionPolicy reads the section. A database without the table never
// saved it and answers no maximum.
func LoadSessionPolicy(ctx context.Context, pool *pgxpool.Pool) (SessionPolicy, error) {
	values, err := Load(ctx, pool, SectionSessionPolicy)
	if UndefinedTable(err) {
		return SessionPolicy{}, nil
	}
	if err != nil {
		return SessionPolicy{}, err
	}
	return SessionPolicyFromValues(values), nil
}

// SectionRunCaps holds the platform's run-level caps (ADR-0041 decision 3,
// #1496 and #1498). This release has one: the whole-run wall cap.
const SectionRunCaps = "run_caps"

// KeyRunMaxWallSeconds is the whole-run wall cap of an agent run, in
// seconds, measured from the run's original start (a resumed run counts
// from the start of the run it continues). 0 (the default) is off.
const KeyRunMaxWallSeconds = "max_wall_seconds"

// Bounds of KeyRunMaxWallSeconds: at least a minute, at most a week.
const (
	MinRunMaxWallSeconds = 60
	MaxRunMaxWallSeconds = 7 * 24 * 60 * 60
)

// RunCaps is the section. A zero duration is off.
type RunCaps struct {
	MaxWall time.Duration
}

// RunCapsFromValues is the overlay, with the same clamping rule as
// SessionPolicyFromValues.
func RunCapsFromValues(values Values) RunCaps {
	return RunCaps{MaxWall: boundSeconds(values, KeyRunMaxWallSeconds, MaxRunMaxWallSeconds)}
}

// LoadRunCaps reads the section. A database without the table never saved
// it and answers off.
func LoadRunCaps(ctx context.Context, pool *pgxpool.Pool) (RunCaps, error) {
	values, err := Load(ctx, pool, SectionRunCaps)
	if UndefinedTable(err) {
		return RunCaps{}, nil
	}
	if err != nil {
		return RunCaps{}, err
	}
	return RunCapsFromValues(values), nil
}

// ValidBoundSeconds reports whether a saved value is 0 (no bound) or a whole
// number of seconds from MinSessionBoundSeconds to limit. JSON numbers
// arrive as float64.
func ValidBoundSeconds(raw any, minimum, limit int64) bool {
	number, ok := raw.(float64)
	if !ok || math.IsNaN(number) || number != math.Trunc(number) {
		return false
	}
	return number == 0 || (number >= float64(minimum) && number <= float64(limit))
}

// boundSeconds reads one maximum: absent or 0 is no maximum; a present
// value is clamped to [MinSessionBoundSeconds, limit].
func boundSeconds(values Values, key string, limit int64) time.Duration {
	raw, present := values[key]
	if !present {
		return 0
	}
	number, ok := raw.(float64)
	if !ok || math.IsNaN(number) {
		// Present but unreadable: the tightest bound, never none.
		return time.Duration(MinSessionBoundSeconds) * time.Second
	}
	seconds := int64(math.Trunc(number))
	switch {
	case seconds == 0:
		return 0
	case seconds < MinSessionBoundSeconds:
		seconds = MinSessionBoundSeconds
	case seconds > limit:
		seconds = limit
	}
	return time.Duration(seconds) * time.Second
}

// CachedSection is a per-replica copy of one section, read at most once per
// TTL. The hot paths that need these sections (browser session validation,
// worker lease renewal) run many times a second; the section changes when
// an administrator saves it. A save invalidates this replica at once; other
// replicas see it within the TTL.
//
// The read never runs under the cache's lock, and at most one runs at a
// time (singleflight). While a stale value is refreshed, other callers get
// that value at once instead of queueing behind the database; only callers
// with no value at all wait for the read in flight. A failed read keeps the
// last good value and backs off (sectionRefreshBackoff, doubling up to
// sectionRefreshMaxBackoff), so a slow or failing database costs one bounded
// read per backoff, not one per call. Before the first good read, the error
// is returned and the caller decides (the session and lease paths fail
// closed).
type CachedSection[T any] struct {
	load    func(context.Context) (T, error)
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time
	group   singleflight.Group

	mu         sync.Mutex
	value      T
	loadedAt   time.Time
	loaded     bool
	refreshing bool
	lastErr    error
	failures   int
	retryAt    time.Time
	// generation counts Invalidate calls. A read that started before an
	// Invalidate may predate the save that caused it, so its value is kept
	// but not treated as fresh.
	generation uint64
}

// DefaultSectionTTL is how long a replica keeps a cached section.
const DefaultSectionTTL = 15 * time.Second

// Bounds of one refresh of a cached section.
const (
	// sectionRefreshTimeout bounds one read, which runs detached from the
	// caller that started it (its result serves every caller).
	sectionRefreshTimeout = 5 * time.Second
	// sectionRefreshBackoff is the wait after a first failed read; each
	// further failure doubles it, up to sectionRefreshMaxBackoff.
	sectionRefreshBackoff    = 500 * time.Millisecond
	sectionRefreshMaxBackoff = 30 * time.Second
)

// NewCachedSection builds the cache. A ttl of 0 is DefaultSectionTTL.
func NewCachedSection[T any](load func(context.Context) (T, error), ttl time.Duration) *CachedSection[T] {
	if ttl <= 0 {
		ttl = DefaultSectionTTL
	}
	return &CachedSection[T]{load: load, ttl: ttl, timeout: sectionRefreshTimeout, now: time.Now}
}

// Get answers the cached value, reading it again once the TTL passed.
func (c *CachedSection[T]) Get(ctx context.Context) (T, error) {
	var zero T
	c.mu.Lock()
	now := c.now()
	value, loaded, lastErr := c.value, c.loaded, c.lastErr
	switch {
	case loaded && now.Sub(c.loadedAt) < c.ttl:
		c.mu.Unlock()
		return value, nil
	case now.Before(c.retryAt):
		// A read failed recently: no read until the backoff is over.
		c.mu.Unlock()
		if loaded {
			return value, nil
		}
		return zero, lastErr
	case loaded && c.refreshing:
		// Another caller refreshes the stale value: serve it meanwhile.
		c.mu.Unlock()
		return value, nil
	}
	c.refreshing = true
	c.mu.Unlock()

	// With no value yet, every caller waits for the one read in flight.
	result := c.group.DoChan("section", func() (any, error) {
		return c.refresh(context.WithoutCancel(ctx))
	})
	select {
	case done := <-result:
		if done.Err != nil {
			if loaded {
				return value, nil
			}
			return zero, done.Err
		}
		fresh, _ := done.Val.(T)
		return fresh, nil
	case <-ctx.Done():
		if loaded {
			return value, nil
		}
		return zero, ctx.Err()
	}
}

// refresh reads the section once and records the outcome. A caller that
// joined late, after the read it waited for had finished, gets that read.
func (c *CachedSection[T]) refresh(ctx context.Context) (T, error) {
	c.mu.Lock()
	if c.loaded && c.now().Sub(c.loadedAt) < c.ttl {
		value := c.value
		c.refreshing = false
		c.mu.Unlock()
		return value, nil
	}
	generation := c.generation
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	value, err := c.load(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshing = false
	now := c.now()
	if err != nil {
		c.failures++
		backoff := sectionRefreshBackoff
		for i := 1; i < c.failures && backoff < sectionRefreshMaxBackoff; i++ {
			backoff *= 2
		}
		c.lastErr, c.retryAt = err, now.Add(min(backoff, sectionRefreshMaxBackoff))
		var zero T
		return zero, err
	}
	c.value, c.loaded = value, true
	if generation == c.generation {
		c.loadedAt = now
	}
	c.lastErr, c.failures, c.retryAt = nil, 0, time.Time{}
	return value, nil
}

// Invalidate makes the next Get read the section again, even inside a
// failure backoff: a save on this replica is a reason to look now.
func (c *CachedSection[T]) Invalidate() {
	c.mu.Lock()
	c.loadedAt = time.Time{}
	c.retryAt = time.Time{}
	c.generation++
	c.mu.Unlock()
}
