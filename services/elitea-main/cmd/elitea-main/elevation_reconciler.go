package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// elevationReconcilerAuditSource is the CloudEvents source of the
// reconciler's admin.elevation events.
const elevationReconcilerAuditSource = "elitea-main/elevation-reconciler"

// startElevationReconciler runs the break-glass elevation reconciler (ADR-0059
// R2.16, adminaccess.ElevationReconciler) on a routines.Loop: one replica per
// pass, every minute by default, changeable on the admin Schedules page. Each
// row is also taken with FOR UPDATE SKIP LOCKED, so the routine registry's
// local fallback during an outage cannot record a row twice. It runs in
// maintenance too: an audit record is not deferred work, and it never decides
// access (the resolver's predicate does). The loop stops when ctx ends; a
// pass cut short by the shutdown rolls back and the next process repeats it,
// so the shutdown does not wait for it.
func startElevationReconciler(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	if pool == nil {
		return nil
	}
	appender, err := securityaudit.NewPostgresAppender(elevationReconcilerAuditSource)
	if err != nil {
		return fmt.Errorf("compose the elevation reconciler appender: %w", err)
	}
	reconciler, err := adminaccess.NewElevationReconciler(pool, appender,
		func(ctx context.Context) (time.Duration, error) {
			settings, err := platformconfig.LoadBreakGlassSettings(ctx, pool)
			if err != nil {
				return 0, err
			}
			return settings.RequestLifetime, nil
		}, time.Now)
	if err != nil {
		return fmt.Errorf("compose the elevation reconciler: %w", err)
	}
	runElevationReconciler(ctx, routines.NewStore(pool), reconciler.Pass, logger)
	return nil
}

// runElevationReconciler runs pass on the routine's loop until ctx ends; the
// returned channel closes when the loop has stopped.
func runElevationReconciler(ctx context.Context, backend routines.Backend,
	pass func(context.Context) (adminaccess.ReconcileResult, error), logger *slog.Logger,
) <-chan struct{} {
	loop := &routines.Loop{
		Backend: backend,
		Def: routines.Definition{
			ID:              adminaccess.ElevationReconcilerRoutineID,
			Owner:           routines.OwnerMain,
			Description:     "Records break-glass elevations that ran out as expired and moves them to review, and expires elevation requests nobody answered.",
			DefaultSchedule: adminaccess.ElevationReconcilerDefaultSchedule,
			MinInterval:     15 * time.Second,
		},
		Logger: logger,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		loop.Run(ctx, func(ctx context.Context) (string, error) {
			result, err := pass(ctx)
			if result.Expired > 0 || result.RequestsExpired > 0 {
				logger.Info("elevation reconciler pass",
					"expired", result.Expired, "requests_expired", result.RequestsExpired)
			}
			if err != nil {
				logger.Error("elevation reconciler pass failed", "err", err)
				return routines.OutcomeError, err
			}
			return routines.OutcomeOK, nil
		})
	}()
	return done
}
