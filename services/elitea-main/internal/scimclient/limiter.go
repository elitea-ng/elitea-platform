package scimclient

import (
	"sync"
	"time"
)

// FailureLimiter counts FAILED client authentications at the token endpoint
// and refuses a key that failed too often in the current window.
//
// It is in memory and per replica. A client secret has 256 bits of entropy, so
// the limiter does not protect the secret from search; it bounds the database
// work and the log volume an attacker can cause with wrong guesses. With N
// replicas the effective limit is N times the stated one. The browser
// attempt limiter (internal/infra/authattempt) is shared in Redis, but its
// stages are fixed to the browser sign-in flows, so it is not reused here.
//
// A success does not reset the counter: an attacker who also holds one valid
// credential must not be able to clear the window.
type FailureLimiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	windows map[string]*failureWindow
}

type failureWindow struct {
	start    time.Time
	failures int
}

// maxLimiterKeys bounds the map. A flood of distinct keys resets it rather than
// growing it without limit; that briefly forgets counters, which is the safe
// direction for availability and costs at most one more window of guesses.
const maxLimiterKeys = 50_000

// NewFailureLimiter allows max failures per key per window.
func NewFailureLimiter(max int, window time.Duration) *FailureLimiter {
	return &FailureLimiter{max: max, window: window, now: time.Now, windows: map[string]*failureWindow{}}
}

// Blocked reports whether any of keys is over its limit, and when to retry.
func (l *FailureLimiter) Blocked(keys ...string) (bool, time.Duration) {
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
func (l *FailureLimiter) Fail(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.windows) > maxLimiterKeys {
		l.windows = map[string]*failureWindow{}
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
