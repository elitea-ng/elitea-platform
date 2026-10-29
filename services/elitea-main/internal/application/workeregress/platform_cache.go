package workeregress

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// PlatformSettingsTTL is how long a read of the platform `worker_egress`
// section is reused. Every command Main prepares reads it, so without the
// cache the common open-mode dispatch would cost a Postgres round trip for a
// row that changes a few times a year.
//
// The propagation delay is the price, and it is bounded: an administrator's
// save (a mode switch or an allowlist change) reaches the commands each Main
// replica prepares at most PlatformSettingsTTL later. A command already
// prepared keeps the policy it was signed with.
const PlatformSettingsTTL = 5 * time.Second

// Bounds of one refresh of the platform section.
const (
	// platformRefreshTimeout bounds one read, which runs detached from the
	// caller that started it (its result serves every caller).
	platformRefreshTimeout = 5 * time.Second
	// platformRefreshBackoff is the wait after a first failed read; each
	// further failure doubles it, up to platformRefreshMaxBackoff.
	platformRefreshBackoff    = 500 * time.Millisecond
	platformRefreshMaxBackoff = 30 * time.Second
)

// CachedPlatformReader reuses the platform section for PlatformSettingsTTL.
//
// The read never runs under the cache's lock, and at most one runs at a time
// (singleflight). While a stale value is refreshed, other callers get the
// last value read at once instead of waiting for the database. A failed
// refresh backs off (platformRefreshBackoff, doubling): until the next
// attempt is due, callers get the last value read, or, with none yet, the
// last error, which the resolver treats as transient. So a slow or failing
// database costs one bounded read per backoff, not one per dispatch.
type CachedPlatformReader struct {
	next    PlatformReader
	ttl     time.Duration
	now     func() time.Time
	timeout time.Duration
	group   singleflight.Group

	mu         sync.Mutex
	value      platformconfig.WorkerEgressSettings
	readAt     time.Time
	hasRead    bool
	refreshing bool
	lastErr    error
	failures   int
	retryAt    time.Time
}

var _ PlatformReader = (*CachedPlatformReader)(nil)

// NewCachedPlatformReader caches next for PlatformSettingsTTL.
func NewCachedPlatformReader(next PlatformReader) *CachedPlatformReader {
	return &CachedPlatformReader{next: next, ttl: PlatformSettingsTTL, now: time.Now, timeout: platformRefreshTimeout}
}

// WorkerEgressSettings answers the cached section, refreshing it when stale.
func (c *CachedPlatformReader) WorkerEgressSettings(ctx context.Context) (platformconfig.WorkerEgressSettings, error) {
	c.mu.Lock()
	now := c.now()
	value, hasRead, lastErr := c.value, c.hasRead, c.lastErr
	switch {
	case hasRead && now.Sub(c.readAt) < c.ttl:
		c.mu.Unlock()
		return cloneWorkerEgressSettings(value), nil
	case now.Before(c.retryAt):
		// A refresh failed recently: no read until the backoff is over.
		c.mu.Unlock()
		if hasRead {
			return cloneWorkerEgressSettings(value), nil
		}
		return platformconfig.WorkerEgressSettings{}, lastErr
	case hasRead && c.refreshing:
		// Another caller refreshes the stale value: serve it meanwhile.
		c.mu.Unlock()
		return cloneWorkerEgressSettings(value), nil
	}
	c.refreshing = true
	c.mu.Unlock()

	// With no value yet, every caller waits for the one read in flight.
	result := c.group.DoChan("worker_egress", func() (any, error) {
		return c.refresh(context.WithoutCancel(ctx))
	})
	select {
	case done := <-result:
		if done.Err != nil {
			if hasRead {
				return cloneWorkerEgressSettings(value), nil
			}
			return platformconfig.WorkerEgressSettings{}, done.Err
		}
		settings, _ := done.Val.(platformconfig.WorkerEgressSettings)
		return cloneWorkerEgressSettings(settings), nil
	case <-ctx.Done():
		if hasRead {
			return cloneWorkerEgressSettings(value), nil
		}
		return platformconfig.WorkerEgressSettings{}, ctx.Err()
	}
}

// refresh reads the section once and records the outcome. A caller that
// joined late, after the read it waited for had finished, gets that read.
func (c *CachedPlatformReader) refresh(ctx context.Context) (platformconfig.WorkerEgressSettings, error) {
	c.mu.Lock()
	if c.hasRead && c.now().Sub(c.readAt) < c.ttl {
		value := cloneWorkerEgressSettings(c.value)
		c.mu.Unlock()
		return value, nil
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	settings, err := c.next.WorkerEgressSettings(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshing = false
	now := c.now()
	if err != nil {
		c.failures++
		backoff := platformRefreshBackoff
		for i := 1; i < c.failures && backoff < platformRefreshMaxBackoff; i++ {
			backoff *= 2
		}
		c.lastErr, c.retryAt = err, now.Add(min(backoff, platformRefreshMaxBackoff))
		if c.hasRead {
			slog.WarnContext(ctx, "worker egress: the platform setting could not be refreshed; using the last value read",
				"read_at", c.readAt, "retry_at", c.retryAt, "err", err)
		}
		return platformconfig.WorkerEgressSettings{}, err
	}
	c.value, c.readAt, c.hasRead = cloneWorkerEgressSettings(settings), now, true
	c.lastErr, c.failures, c.retryAt = nil, 0, time.Time{}
	return cloneWorkerEgressSettings(settings), nil
}

func cloneWorkerEgressSettings(settings platformconfig.WorkerEgressSettings) platformconfig.WorkerEgressSettings {
	settings.Allowlist = append([]string(nil), settings.Allowlist...)
	return settings
}
