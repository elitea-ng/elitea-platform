// Package failurelimit is the in-memory, per-replica failure limiter the
// anonymous credential endpoints share: the SCIM token endpoint (shared 0135)
// and the native authorization endpoints (ADR-0025 WP2). It moved here from
// internal/scimclient so the native endpoints do not import the SCIM package;
// scimclient keeps an alias.
package failurelimit

import (
	"sync"
	"time"
)

// Limiter counts FAILED client authentications at the token endpoint
// and refuses a key that failed too often in the current window.
//
// It is in memory and per replica. A client secret has 256 bits of entropy, so
// the limiter does not protect the secret from search. It bounds the database
// work an attacker can cause with wrong guesses: the token endpoint checks it
// BEFORE the secret lookup, so a blocked key costs no database read, and the
// endpoint logs no line for a refused attempt. The key is (client id, caller
// address), so a caller can block only itself. With N replicas the effective
// limit is N times the stated one. The browser
// attempt limiter (internal/infra/authattempt) is shared in Redis, but its
// stages are fixed to the browser sign-in flows, so it is not reused here.
//
// A success does not reset the counter: an attacker who also holds one valid
// credential must not be able to clear the window.
type Limiter struct {
	max     int
	window  time.Duration
	now     func() time.Time
	maxKeys int

	mu      sync.Mutex
	windows map[string]*failureWindow
}

type failureWindow struct {
	start    time.Time
	failures int
}

// defaultMaxKeys bounds the map. Random client ids or addresses cannot grow
// it without limit: when it is full, Fail first sweeps every window that has
// ended (a TTL sweep), and only if the map is still full does it start again
// from empty. Starting again briefly forgets counters, which is the safe
// direction for availability and costs at most one more window of guesses.
const defaultMaxKeys = 50_000

// New allows max failures per key per window.
func New(max int, window time.Duration) *Limiter {
	return &Limiter{
		max: max, window: window, now: time.Now, maxKeys: defaultMaxKeys,
		windows: map[string]*failureWindow{},
	}
}

// SetClock replaces the limiter's clock. Tests only.
func (l *Limiter) SetClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// Size is the number of keys the limiter holds.
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.windows)
}

// Blocked reports whether any of keys is over its limit, and when to retry.
func (l *Limiter) Blocked(keys ...string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, key := range keys {
		window, ok := l.windows[key]
		if !ok || now.Sub(window.start) >= l.window {
			continue
		}
		if window.failures >= l.max {
			return true, l.window - now.Sub(window.start)
		}
	}
	return false, 0
}

// Fail records one failure for each key.
func (l *Limiter) Fail(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.windows) >= l.maxKeys {
		for key, window := range l.windows {
			if now.Sub(window.start) >= l.window {
				delete(l.windows, key)
			}
		}
		if len(l.windows) >= l.maxKeys {
			l.windows = map[string]*failureWindow{}
		}
	}
	for _, key := range keys {
		window, ok := l.windows[key]
		if !ok || now.Sub(window.start) >= l.window {
			l.windows[key] = &failureWindow{start: now, failures: 1}
			continue
		}
		window.failures++
	}
}
