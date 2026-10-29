package workeregress

import (
	"context"
	"log/slog"
	"sync"
	"time"

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

// CachedPlatformReader reuses the platform section for PlatformSettingsTTL.
// A failed refresh answers the last value read, when there is one (and tries
// again on the next call); with none yet, the error, which the resolver
// treats as transient.
type CachedPlatformReader struct {
	next PlatformReader
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	value   platformconfig.WorkerEgressSettings
	readAt  time.Time
	hasRead bool
}

var _ PlatformReader = (*CachedPlatformReader)(nil)

// NewCachedPlatformReader caches next for PlatformSettingsTTL.
func NewCachedPlatformReader(next PlatformReader) *CachedPlatformReader {
	return &CachedPlatformReader{next: next, ttl: PlatformSettingsTTL, now: time.Now}
}

// WorkerEgressSettings answers the cached section, refreshing it when stale.
// Concurrent callers of a stale entry share one refresh.
func (c *CachedPlatformReader) WorkerEgressSettings(ctx context.Context) (platformconfig.WorkerEgressSettings, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.hasRead && now.Sub(c.readAt) < c.ttl {
		return cloneWorkerEgressSettings(c.value), nil
	}
	settings, err := c.next.WorkerEgressSettings(ctx)
	if err != nil {
		if c.hasRead {
			slog.WarnContext(ctx, "worker egress: the platform setting could not be refreshed; using the last value read",
				"read_at", c.readAt, "err", err)
			return cloneWorkerEgressSettings(c.value), nil
		}
		return platformconfig.WorkerEgressSettings{}, err
	}
	c.value, c.readAt, c.hasRead = cloneWorkerEgressSettings(settings), now, true
	return cloneWorkerEgressSettings(settings), nil
}

func cloneWorkerEgressSettings(settings platformconfig.WorkerEgressSettings) platformconfig.WorkerEgressSettings {
	settings.Allowlist = append([]string(nil), settings.Allowlist...)
	return settings
}
