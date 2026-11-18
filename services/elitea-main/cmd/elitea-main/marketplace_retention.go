package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The routines registry rows of the Marketplace's daily jobs.
const (
	// marketplaceRetentionRoutineID is the snapshot purge (ADR-0051 decision 2,
	// ADR-0042).
	marketplaceRetentionRoutineID = "main.marketplace_retention"
	// marketplaceSignalsRoutineID computes the daily listing signals (ADR-0051
	// decision 5).
	marketplaceSignalsRoutineID = "main.marketplace_signals"
)

// marketplacePass is the work of one run of a Marketplace routine.
type marketplacePass func(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error

// startMarketplaceRoutine runs pass on a routines.Loop, daily: one replica per
// pass, paused in maintenance.
func startMarketplaceRoutine(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, id, description string, pass marketplacePass) {
	loop := &routines.Loop{
		Backend: routines.NewStore(pool),
		Def: routines.Definition{
			ID:              id,
			Owner:           routines.OwnerMain,
			Description:     description,
			DefaultSchedule: "24h",
			MinInterval:     time.Hour,
		},
		Logger: logger,
	}
	go loop.Run(ctx, func(ctx context.Context) (string, error) {
		return marketplaceRun(ctx, pool, logger, id, pass)
	})
}

// startMarketplaceRetention runs the snapshot purge. It deletes withdrawn
// listing versions that no project has installed or as a fork origin, and the
// listings left empty.
func startMarketplaceRetention(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) {
	startMarketplaceRoutine(ctx, pool, logger, marketplaceRetentionRoutineID,
		"Deletes withdrawn Marketplace snapshots that no project has installed or as a fork origin, after the retention period (ADR-0051, ADR-0042).",
		marketplaceRetentionPass)
}

func marketplaceRetentionPass(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	_, err := marketplace.RetentionPass(ctx, pool, logger)
	return err
}

// marketplaceRetentionRun is one run of the snapshot purge.
func marketplaceRetentionRun(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) (string, error) {
	return marketplaceRun(ctx, pool, logger, marketplaceRetentionRoutineID, marketplaceRetentionPass)
}

// startMarketplaceSignals computes the signals of every listing that has a hire:
// active hires, 30-day retention, dismissal rate and upgrade adoption. It also
// recomputes the ratings from the visible reviews.
func startMarketplaceSignals(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) {
	startMarketplaceRoutine(ctx, pool, logger, marketplaceSignalsRoutineID,
		"Computes the daily Marketplace listing signals (active hires, 30-day retention, dismissal rate, upgrade adoption) and refreshes the ratings (ADR-0051).",
		marketplaceSignalsPass)
}

func marketplaceSignalsPass(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	_, err := marketplace.SignalsPass(ctx, pool, logger)
	return err
}

// marketplaceSignalsRun is one run of the signals routine.
func marketplaceSignalsRun(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) (string, error) {
	return marketplaceRun(ctx, pool, logger, marketplaceSignalsRoutineID, marketplaceSignalsPass)
}

// marketplaceRun is one run of a routine: it does nothing in maintenance.
func marketplaceRun(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, id string, pass marketplacePass) (string, error) {
	state, err := platformconfig.LoadMaintenance(ctx, pool)
	if err != nil {
		logger.Error("marketplace routine: maintenance switch unreadable", "routine", id, "err", err)
		return routines.OutcomeError, err
	}
	if state.Enabled {
		return routines.OutcomeMaintenance, nil
	}
	if err := pass(ctx, pool, logger); err != nil {
		logger.Error("marketplace routine failed", "routine", id, "err", err)
		return routines.OutcomeError, err
	}
	return routines.OutcomeOK, nil
}
