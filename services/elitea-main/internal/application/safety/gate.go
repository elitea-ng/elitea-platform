package safety

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// ActiveLoader reads every active suspension: one indexed read of the
// partial unique index of shared 0194.
type ActiveLoader interface {
	LoadActiveSuspensions(ctx context.Context) ([]Suspension, error)
}

// Transport carries invalidations between replicas (core NATS in
// production, subject elitea.rt.v1.safety.suspension).
type Transport interface {
	Publish(data []byte) error
	Subscribe(deliver func([]byte)) (unsubscribe func() error, err error)
}

const (
	// DefaultMaxAge bounds how long a replica trusts its snapshot without an
	// invalidation. It is the backstop for a lost NATS message: a missed
	// suspension reaches this replica's admission within this long, so it
	// equals the 10-second stop target (DefaultStopSLO) even without NATS.
	DefaultMaxAge = 10 * time.Second
	// failedRefreshRetry is how soon a replica that kept its last snapshot
	// after a failed refresh tries again.
	failedRefreshRetry = time.Second

	gateQueueSize        = 256
	gateRetryMin         = 250 * time.Millisecond
	gateRetryMax         = 30 * time.Second
	gateMessageVersion   = 1
	gateMaxMessageBytes  = 512
	gateMessageKindDelta = "suspension"
)

// GateOptions configures a Gate. Every field is optional.
type GateOptions struct {
	MaxAge time.Duration
	Now    func() time.Time
	Logger *slog.Logger
}

// Gate is this replica's admission check: a cached snapshot of the active
// suspensions.
//
// Freshness: an invalidation (local or over NATS) marks the snapshot stale
// and the next Check reloads it; without one the snapshot is reloaded after
// MaxAge.
//
// Failure (decided here, ADR-0041 leaves it open):
//   - a refresh that fails KEEPS the last snapshot and retries a second
//     later. Refusing every start because one read failed would turn a
//     database blip into a platform outage that looks like a kill switch
//     nobody pulled; the admission that follows writes the same database
//     and fails on its own if it is really down.
//   - a replica that has NEVER loaded a snapshot reads through to the
//     database on every Check, and refuses the start (ErrStateUnavailable)
//     when that read fails. It has no evidence that nothing is suspended, so
//     it does not admit.
type Gate struct {
	loader ActiveLoader
	maxAge time.Duration
	now    func() time.Time
	logger *slog.Logger
	origin string

	generation atomic.Uint64

	mu        sync.RWMutex
	snapshot  []Suspension
	loaded    bool
	loadedGen uint64
	validTill time.Time
	// retryAfter holds a refresh back after a failed one.
	retryAfter time.Time

	loadMu sync.Mutex

	queue chan int64
}

// NewGate builds a gate over loader.
func NewGate(loader ActiveLoader, opts GateOptions) (*Gate, error) {
	if loader == nil {
		return nil, errors.New("safety: the suspension loader is required")
	}
	g := &Gate{
		loader: loader, maxAge: opts.MaxAge, now: opts.Now, logger: opts.Logger,
		origin: uuid.NewString(), queue: make(chan int64, gateQueueSize),
	}
	if g.maxAge <= 0 {
		g.maxAge = DefaultMaxAge
	}
	if g.now == nil {
		g.now = time.Now
	}
	if g.logger == nil {
		g.logger = slog.Default()
	}
	return g, nil
}

// Check refuses a subject an active suspension covers, with a
// *SuspendedError. It returns ErrStateUnavailable (wrapped) when the state
// cannot be read at all.
func (g *Gate) Check(ctx context.Context, subject Subject) error {
	snapshot, err := g.Active(ctx)
	if err != nil {
		return err
	}
	return CheckSnapshot(snapshot, subject)
}

// CheckSnapshot is Check over a given list. The most specific scope wins,
// so the refusal names the narrowest suspension a caller can ask about.
func CheckSnapshot(active []Suspension, subject Subject) error {
	var found *Suspension
	for i := range active {
		s := active[i]
		if !s.Active() || !s.Covers(subject) {
			continue
		}
		if found == nil || specificity(s.Target.Scope) > specificity(found.Target.Scope) {
			found = &active[i]
		}
	}
	if found == nil {
		return nil
	}
	return refusal(*found)
}

func specificity(scope Scope) int {
	switch scope {
	case ScopeAgent, ScopeToolkitType:
		return 3
	case ScopeProject:
		return 2
	}
	return 1
}

// Active returns the cached active suspensions, reloading them when stale.
func (g *Gate) Active(ctx context.Context) ([]Suspension, error) {
	if snapshot, fresh := g.cached(); fresh {
		return snapshot, nil
	}
	g.loadMu.Lock()
	defer g.loadMu.Unlock()
	if snapshot, fresh := g.cached(); fresh {
		return snapshot, nil
	}
	generation := g.generation.Load()
	loaded, err := g.loader.LoadActiveSuspensions(ctx)
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		if !g.loaded {
			return nil, fmt.Errorf("%w: %v", ErrStateUnavailable, err)
		}
		g.logger.WarnContext(ctx, "safety: suspension refresh failed; keeping the last snapshot", "err", err)
		g.retryAfter = now.Add(failedRefreshRetry)
		return g.snapshot, nil
	}
	g.snapshot, g.loaded, g.loadedGen = loaded, true, generation
	g.validTill, g.retryAfter = now.Add(g.maxAge), time.Time{}
	return loaded, nil
}

func (g *Gate) cached() ([]Suspension, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.loaded {
		return nil, false
	}
	now := g.now()
	if now.Before(g.retryAfter) {
		// A refresh failed less than a second ago: keep serving the last
		// snapshot instead of hammering the database.
		return g.snapshot, true
	}
	return g.snapshot, g.loadedGen == g.generation.Load() && now.Before(g.validTill)
}

// Invalidate marks this replica's snapshot stale.
func (g *Gate) Invalidate() {
	if g != nil {
		g.generation.Add(1)
	}
}

// Notify invalidates this replica at once and queues the change for the
// other replicas. It never blocks: a full queue drops the remote copy, and
// the other replicas see the change within MaxAge.
func (g *Gate) Notify(suspensionID int64) {
	if g == nil {
		return
	}
	g.Invalidate()
	select {
	case g.queue <- suspensionID:
	default:
		g.logger.Warn("safety: suspension invalidation queue is full; replicas fall back to the snapshot age")
	}
}

type gateMessage struct {
	Version int    `json:"v"`
	Origin  string `json:"origin"`
	Kind    string `json:"kind"`
	ID      int64  `json:"id"`
}

// Run subscribes through t (retrying with backoff until the first subscribe
// succeeds) and publishes queued changes until ctx ends.
func (g *Gate) Run(ctx context.Context, t Transport) error {
	if g == nil || t == nil || ctx == nil {
		return errors.New("safety: the gate and its transport are required")
	}
	go g.publish(ctx, t)
	delay := gateRetryMin
	for {
		unsubscribe, err := t.Subscribe(g.receive)
		if err == nil {
			<-ctx.Done()
			_ = unsubscribe()
			return ctx.Err()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		g.logger.Warn("safety: suspension invalidation subscription failed; replicas fall back to the snapshot age", "err", err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, gateRetryMax)
	}
}

func (g *Gate) publish(ctx context.Context, t Transport) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-g.queue:
			data, err := json.Marshal(gateMessage{Version: gateMessageVersion, Origin: g.origin, Kind: gateMessageKindDelta, ID: id})
			if err != nil {
				continue
			}
			if err := t.Publish(data); err != nil && ctx.Err() == nil {
				g.logger.Warn("safety: suspension invalidation publish failed; replicas fall back to the snapshot age", "err", err)
			}
		}
	}
}

// receive handles one remote message. Malformed, foreign-version and own
// messages are dropped.
func (g *Gate) receive(data []byte) {
	if len(data) == 0 || len(data) > gateMaxMessageBytes {
		return
	}
	var m gateMessage
	if json.Unmarshal(data, &m) != nil || m.Version != gateMessageVersion || m.Origin == g.origin ||
		m.Kind != gateMessageKindDelta {
		return
	}
	g.Invalidate()
}
