package runtimecomposition

// The index registry tombstone sweeper's runner.
//
// Deleting a rust-mode index tombstones its registry row and then asks the
// vector store to delete the index namespace. When that call fails, or no
// vector store client exists yet, the tombstone stays, and this reconciler
// finishes the deletion: it claims due tombstones (FOR UPDATE SKIP LOCKED, a
// bounded batch, the claim itself pushes the row's next attempt out), retries
// the idempotent deletion and purges the ones that succeed. The backoff is
// stored in the row (attempts, next_attempt_at), so a restart or another
// replica keeps it. Shape: project_deletion_reconciler.go, a jittered interval
// so replicas do not tick in step.
//
// While the deleter is the Deferred one nothing can succeed, so the reconciler
// logs that once at startup and then waits for shutdown: it does not poll the
// table and it counts no attempts.

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"
)

const (
	// DefaultIndexTombstoneInterval is the pause between sweeps.
	DefaultIndexTombstoneInterval = time.Minute
	// DefaultIndexTombstoneBatch bounds the tombstones one sweep claims.
	DefaultIndexTombstoneBatch = 10
)

type indexTombstoneSweeper interface {
	Deferred() bool
	RunOnce(context.Context) (int, error)
}

type indexTombstoneReconciler struct {
	sweeper  indexTombstoneSweeper
	interval time.Duration
	logger   *slog.Logger
	wait     func(context.Context, time.Duration) error
}

func newIndexTombstoneReconciler(
	sweeper indexTombstoneSweeper,
	interval time.Duration,
	logger *slog.Logger,
) (*indexTombstoneReconciler, error) {
	if sweeper == nil || interval <= 0 || logger == nil {
		return nil, errors.New("index tombstone reconciler configuration is invalid")
	}
	return &indexTombstoneReconciler{
		sweeper: sweeper, interval: interval, logger: logger,
		wait: waitCurrentIndexMetaTerminalReconciler,
	}, nil
}

func (r *indexTombstoneReconciler) Run(ctx context.Context) error {
	if r == nil || r.sweeper == nil || r.wait == nil || ctx == nil {
		return errors.New("index tombstone reconciler is incomplete")
	}
	if r.sweeper.Deferred() {
		r.logger.InfoContext(ctx,
			"index vector deletion is deferred until elitea-vector is wired: deleted indexes keep their tombstones and the sweeper is idle")
		<-ctx.Done()
		return ctx.Err()
	}
	for {
		if _, err := r.sweeper.RunOnce(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			r.logger.ErrorContext(ctx, "index tombstone sweep failed", "err", err)
		}
		jitter := 0.8 + 0.4*rand.Float64()
		if err := r.wait(ctx, time.Duration(float64(r.interval)*jitter)); err != nil {
			return err
		}
	}
}

var _ publisherRunner = (*indexTombstoneReconciler)(nil)
