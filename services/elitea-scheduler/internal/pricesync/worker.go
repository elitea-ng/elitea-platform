package pricesync

import (
	"context"
	"log/slog"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
)

// Worker runs the price-sync pass (design §8.8: ~24h cadence, driven by the
// routine registry loop). The per-replica advisory lock inside Syncer.Sync makes
// concurrent workers safe.
type Worker struct {
	syncer   *Syncer
	interval time.Duration
	logger   *slog.Logger
}

// NewWorker builds a Worker. A non-positive interval falls back to 24h.
func NewWorker(syncer *Syncer, interval time.Duration, logger *slog.Logger) *Worker {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{syncer: syncer, interval: interval, logger: logger}
}

// runOnce executes a single sync pass with its own bounded timeout so a hung
// source fetch cannot wedge the worker until the next tick.
func (w *Worker) runOnce(ctx context.Context) (string, error) {
	passCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	n, err := w.syncer.Sync(passCtx)
	if err != nil {
		w.logger.Error("pricesync: sync pass failed", "err", err)
		return routines.OutcomeError, err
	}
	if n > 0 {
		w.logger.Info("pricesync: sync pass complete", "models", n)
	}
	return routines.OutcomeOK, nil
}

// Pass runs one pass and reports its outcome word; the routine registry loop
// (libs/go/routines) calls it on the operator-configured schedule.
func (w *Worker) Pass(ctx context.Context) (string, error) { return w.runOnce(ctx) }
