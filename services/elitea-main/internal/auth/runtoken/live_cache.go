package runtoken

import (
	"context"
	"encoding/hex"
	"strconv"
	"sync"
	"time"
)

// LiveCacheTTL is how long a live answer is reused. A model turn makes many
// /llm calls a second; without it each one also paid the claim join. It is
// the longest a released claim's token keeps working, far inside the lease
// itself (a crashed worker's token already outlives it by the lease).
const LiveCacheTTL = 2 * time.Second

// liveCacheMax bounds the entries; past it a live answer is simply not
// cached, so a burst of runs costs queries, never memory.
const liveCacheMax = 4096

// CachedLeases reuses a LIVE answer of next for LiveCacheTTL, keyed by the
// whole binding (claim, execution, generation, fence, project, actor, token
// row). A not-live answer and an error are never cached: a refusal is final
// for the token anyway, and a store fault is retried by the next request.
type CachedLeases struct {
	next LeaseReader
	now  func() time.Time

	mu    sync.Mutex
	until map[string]time.Time
}

// NewCachedLeases wraps next.
func NewCachedLeases(next LeaseReader) *CachedLeases {
	return &CachedLeases{next: next, now: time.Now, until: map[string]time.Time{}}
}

// ClaimLive implements LeaseReader.
func (c *CachedLeases) ClaimLive(ctx context.Context, binding Binding) (bool, error) {
	key := binding.ClaimID + "\x00" + binding.ExecutionID + "\x00" +
		strconv.FormatUint(binding.Generation, 10) + "\x00" + hex.EncodeToString(binding.FenceToken) + "\x00" +
		strconv.FormatInt(binding.ProjectID, 10) + "\x00" + strconv.FormatInt(binding.UserID, 10) + "\x00" +
		strconv.FormatInt(binding.TokenID, 10)
	now := c.now()
	c.mu.Lock()
	until, ok := c.until[key]
	c.mu.Unlock()
	if ok && now.Before(until) {
		return true, nil
	}
	live, err := c.next.ClaimLive(ctx, binding)
	if err != nil || !live {
		c.mu.Lock()
		delete(c.until, key)
		c.mu.Unlock()
		return live, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.until) >= liveCacheMax {
		for k, u := range c.until {
			if !now.Before(u) {
				delete(c.until, k)
			}
		}
	}
	if len(c.until) < liveCacheMax {
		c.until[key] = now.Add(LiveCacheTTL)
	}
	return true, nil
}
