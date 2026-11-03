package unattendedchain

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Sweep bounds (ADR-0045 decision 4).
const (
	// SweepInterval is how often the sweeper looks for chains to end.
	SweepInterval = time.Minute
	// SweepQuiet is how long a chain waits for a continuation after its
	// latest run ended.
	SweepQuiet = 10 * time.Minute
	// SweepBatch bounds one pass.
	SweepBatch = 64
)

// SweepStore ends the chains that nothing else will end.
type SweepStore interface {
	// SweepChains ends at most limit chains and answers how many.
	SweepChains(ctx context.Context, quiet time.Duration, limit int) (int, error)
}

// Sweeper ends a chain whose deadline passed, or whose latest run ended
// with no continuation started within SweepQuiet: cancelled when the latest
// local turn expired uncommitted, otherwise failed.
type Sweeper struct {
	store    SweepStore
	logger   *slog.Logger
	interval time.Duration
}

// NewSweeper builds the sweeper.
func NewSweeper(store SweepStore, logger *slog.Logger) (*Sweeper, error) {
	if store == nil || logger == nil {
		return nil, errors.New("unattended chain sweeper dependencies are required")
	}
	return &Sweeper{store: store, logger: logger, interval: SweepInterval}, nil
}

// Run sweeps until ctx ends.
func (s *Sweeper) Run(ctx context.Context) error {
	if s == nil || ctx == nil {
		return errors.New("unattended chain sweeper is not configured")
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			s.logger.Error("unattended chain sweep failed", "err", err)
		}
	}
}

// Sweep runs passes until one ends fewer than SweepBatch chains.
func (s *Sweeper) Sweep(ctx context.Context) error {
	for ctx.Err() == nil {
		ended, err := s.store.SweepChains(ctx, SweepQuiet, SweepBatch)
		if err != nil {
			return err
		}
		if ended < SweepBatch {
			return nil
		}
	}
	return ctx.Err()
}
