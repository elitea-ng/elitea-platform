package refresh

import (
	"context"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"time"

	"golang.org/x/sync/singleflight"
)

// ErrWaitExceeded means the partition was still being refreshed by another
// holder when the caller's bounded wait ran out.
var ErrWaitExceeded = errors.New("refresh: waited too long for another replica's refresh")

// LeaseStore is what the coordinator needs from the lease table (*Leases).
type LeaseStore interface {
	Acquire(ctx context.Context, partition PartitionHash, holder string, ttl time.Duration, generation int64) (Lease, bool, error)
	Release(ctx context.Context, lease Lease) error
}

// Config tunes the coordinator. Zero values take the defaults below; every
// default is a test value, not a customer requirement (gate D5).
type Config struct {
	// Holder names this replica in the lease row (for example the pod name
	// plus a random suffix). Required.
	Holder string
	// LeaseTTL bounds one holder's refresh. It must exceed the provider call
	// timeout, or a slow holder loses its fence while the provider rotates
	// the refresh token. Default 30s.
	LeaseTTL time.Duration
	// MaxWait bounds how long one call waits for a refresh, its own or
	// another replica's. Default 45s.
	MaxWait time.Duration
	// InitialBackoff and MaxBackoff bound the poll interval of a waiting
	// caller. Defaults 25ms and 1s.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// Coordinator joins refreshes of one partition inside the process
// (singleflight) and across replicas (the lease).
type Coordinator struct {
	leases LeaseStore
	cfg    Config
	group  singleflight.Group
	sleep  func(context.Context, time.Duration) error
	jitter func(time.Duration) time.Duration
}

// NewCoordinator builds the coordinator.
func NewCoordinator(leases LeaseStore, cfg Config) (*Coordinator, error) {
	if leases == nil || cfg.Holder == "" || len(cfg.Holder) > maxHolderBytes {
		return nil, errors.New("refresh: a lease store and a holder name are required")
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 30 * time.Second
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 45 * time.Second
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = 25 * time.Millisecond
	}
	if cfg.MaxBackoff < cfg.InitialBackoff {
		cfg.MaxBackoff = time.Second
		if cfg.MaxBackoff < cfg.InitialBackoff {
			cfg.MaxBackoff = cfg.InitialBackoff
		}
	}
	return &Coordinator{
		leases: leases, cfg: cfg, sleep: sleepContext,
		// Full jitter: a uniform wait in [0, d].
		jitter: func(d time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(d) + 1)) },
	}, nil
}

// LeaseTTL is the configured lease lifetime.
func (c *Coordinator) LeaseTTL() time.Duration { return c.cfg.LeaseTTL }

// State is one read of the partition's state.
type State[T any] struct {
	Value T
	// Fresh means Value is usable and no refresh is needed.
	Fresh bool
	// Generation is the state's write generation.
	Generation int64
}

// Flight is one refresh of one partition.
type Flight[T any] struct {
	Partition PartitionHash
	// Read reads the current state. It runs before every acquire attempt
	// and again once the lease is held, so a refresh another holder just
	// committed is never repeated.
	Read func(ctx context.Context) (State[T], error)
	// Refresh runs under the lease. It calls the provider and writes back
	// through Leases.Commit with the lease; it answers the new value.
	Refresh func(ctx context.Context, lease Lease) (T, error)
}

// Do answers a fresh value for the partition, refreshing it at most once
// across replicas. Callers in this process share one flight; the flight
// runs detached from any one caller's cancellation and is bounded by
// MaxWait.
func Do[T any](ctx context.Context, c *Coordinator, f Flight[T]) (T, error) {
	var zero T
	if f.Read == nil || f.Refresh == nil {
		return zero, errors.New("refresh: a flight needs Read and Refresh")
	}
	key := hex.EncodeToString(f.Partition[:])
	ch := c.group.DoChan(key, func() (any, error) {
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.MaxWait)
		defer cancel()
		return run(flightCtx, c, f)
	})
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case result := <-ch:
		if result.Err != nil {
			return zero, result.Err
		}
		return result.Val.(T), nil
	}
}

func run[T any](ctx context.Context, c *Coordinator, f Flight[T]) (T, error) {
	var zero T
	backoff := c.cfg.InitialBackoff
	for {
		state, err := f.Read(ctx)
		if err != nil {
			return zero, err
		}
		if state.Fresh {
			return state.Value, nil
		}
		lease, acquired, err := c.leases.Acquire(ctx, f.Partition, c.cfg.Holder, c.cfg.LeaseTTL, state.Generation)
		if err != nil {
			return zero, err
		}
		if acquired {
			return holdAndRefresh(ctx, c, f, lease)
		}
		// Another replica holds the lease: wait for its write-back.
		if err := c.sleep(ctx, c.jitter(backoff)); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return zero, ErrWaitExceeded
			}
			return zero, err
		}
		backoff = min(backoff*2, c.cfg.MaxBackoff)
	}
}

func holdAndRefresh[T any](ctx context.Context, c *Coordinator, f Flight[T], lease Lease) (T, error) {
	var zero T
	release := func() {
		// Best effort, on a context the flight's deadline cannot cancel: an
		// unreleased lease only delays the next holder until it expires.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = c.leases.Release(releaseCtx, lease)
	}
	// Re-read under the lease: the previous holder may have committed
	// between our read and our acquire.
	state, err := f.Read(ctx)
	if err != nil {
		release()
		return zero, err
	}
	if state.Fresh {
		release()
		return state.Value, nil
	}
	lease.Generation = state.Generation
	value, err := f.Refresh(ctx, lease)
	if err != nil {
		// A committed write released the lease already; a failed one did
		// not, and a stale fence is not ours to release.
		if !errors.Is(err, ErrStaleFence) {
			release()
		}
		return zero, err
	}
	return value, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
