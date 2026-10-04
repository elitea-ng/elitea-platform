package configurations

import (
	"sync"
	"time"
)

// model_probe_limiter.go bounds the llm_model test (model_connection_check.go).
//
// The test sends one REAL, billed completion. The gateway sends it directly to
// the provider: the budget gate, the request log and the analytics do not see
// it. A caller who holds the create and update strings can call the route in a
// loop. So the route limits each project and user here, before the credential
// is resolved:
//
//   - a token bucket: modelProbeBurst tests at once, then one test each
//     modelProbeRefill;
//   - at most modelProbeInFlight tests of one project and user at the same
//     time, because each one can hold this request for up to 40 s.
//
// The gateway also bounds how many model probes it runs at the same time. The
// state is in memory and per replica. That is enough for its purpose: the
// bound makes a loop of tests slow and visible, it is not a budget.

const (
	modelProbeBurst    = 5
	modelProbeRefill   = 12 * time.Second
	modelProbeInFlight = 2
	// modelProbeMaxKeys is the size at which idle entries are dropped.
	modelProbeMaxKeys = 4096
)

type modelProbeLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]*modelProbeBucket
}

type modelProbeBucket struct {
	tokens   float64
	updated  time.Time
	inFlight int
}

func newModelProbeLimiter() *modelProbeLimiter {
	return &modelProbeLimiter{now: time.Now, buckets: map[string]*modelProbeBucket{}}
}

// acquire takes one test for key. It returns a release function, or nil when
// the key is over its rate or its concurrency bound.
func (l *modelProbeLimiter) acquire(key string) func() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.buckets) >= modelProbeMaxKeys {
		l.dropIdle(now)
	}
	bucket, ok := l.buckets[key]
	if !ok {
		bucket = &modelProbeBucket{tokens: modelProbeBurst, updated: now}
		l.buckets[key] = bucket
	}
	bucket.refill(now)
	if bucket.tokens < 1 || bucket.inFlight >= modelProbeInFlight {
		return nil
	}
	bucket.tokens--
	bucket.inFlight++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			bucket.inFlight--
		})
	}
}

func (b *modelProbeBucket) refill(now time.Time) {
	elapsed := now.Sub(b.updated)
	if elapsed <= 0 {
		return
	}
	b.tokens += float64(elapsed) / float64(modelProbeRefill)
	if b.tokens > modelProbeBurst {
		b.tokens = modelProbeBurst
	}
	b.updated = now
}

// dropIdle removes every entry that is full again and has no test running:
// such an entry holds no state a new entry would not have.
func (l *modelProbeLimiter) dropIdle(now time.Time) {
	for key, bucket := range l.buckets {
		bucket.refill(now)
		if bucket.inFlight == 0 && bucket.tokens >= modelProbeBurst {
			delete(l.buckets, key)
		}
	}
}
