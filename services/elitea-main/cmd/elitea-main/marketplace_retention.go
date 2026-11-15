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

// marketplaceRetentionRoutineID is the routines registry row of the Marketplace
// snapshot purge (ADR-0051 decision 2, ADR-0042).
const marketplaceRetentionRoutineID = "main.marketplace_retention"

// startMarketplaceRetention runs the snapshot purge on a routines.Loop, daily:
// one replica per pass, paused in maintenance. It deletes withdrawn listing
// versions that no project has installed or as a fork origin, and the listings
// left empty.
func startMarketplaceRetention(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) {
	loop := &routines.Loop{
		Backend: routines.NewStore(pool),
		Def: routines.Definition{
			ID:              marketplaceRetentionRoutineID,
			Owner:           routines.OwnerMain,
			Description:     "Deletes withdrawn Marketplace snapshots that no project has installed or as a fork origin, after the retention period (ADR-0051, ADR-0042).",
			DefaultSchedule: "24h",
			MinInterval:     time.Hour,
		},
		Logger: logger,
	}
	go loop.Run(ctx, func(ctx context.Context) (string, error) {
		return marketplaceRetentionRun(ctx, pool, logger)
	})
}

// marketplaceRetentionRun is one pass of the routine.
func marketplaceRetentionRun(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) (string, error) {
	state, err := platformconfig.LoadMaintenance(ctx, pool)
	if err != nil {
		logger.Error("marketplace retention: maintenance switch unreadable", "err", err)
		return routines.OutcomeError, err
	}
	if state.Enabled {
		return routines.OutcomeMaintenance, nil
	}
	if _, err := marketplace.RetentionPass(ctx, pool, logger); err != nil {
		logger.Error("marketplace retention pass failed", "err", err)
		return routines.OutcomeError, err
	}
	return routines.OutcomeOK, nil
}
