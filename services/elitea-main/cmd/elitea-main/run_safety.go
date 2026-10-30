package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// runSafety holds the per-replica caches of the two run-safety
// Configuration sections: the administrator's session policy (ADR-0042
// decision 5) and the run caps (ADR-0041 decision 3). One cache per section
// per replica, shared by every reader and dropped by the admin save.
type runSafety struct {
	sessionPolicy *platformconfig.CachedSection[platformconfig.SessionPolicy]
	runCaps       *platformconfig.CachedSection[platformconfig.RunCaps]
	// delegation is the delegation policy (ADR-0044 decision 3) every
	// agent execution command carries.
	delegation *platformconfig.CachedSection[platformconfig.DelegationSettings]
}

func newRunSafety(pool *pgxpool.Pool) runSafety {
	return runSafety{
		sessionPolicy: platformconfig.NewCachedSection(func(ctx context.Context) (platformconfig.SessionPolicy, error) {
			return platformconfig.LoadSessionPolicy(ctx, pool)
		}, 0),
		runCaps: platformconfig.NewCachedSection(func(ctx context.Context) (platformconfig.RunCaps, error) {
			return platformconfig.LoadRunCaps(ctx, pool)
		}, 0),
		delegation: platformconfig.NewCachedSection(func(ctx context.Context) (platformconfig.DelegationSettings, error) {
			return platformconfig.LoadDelegationSettings(ctx, pool)
		}, 0),
	}
}

// sessionBounds adapts the session policy to the browser session manager.
func (r runSafety) sessionBounds() browsersession.BoundsSource {
	return browsersession.BoundsFunc(func(ctx context.Context) (browsersession.Bounds, error) {
		policy, err := r.sessionPolicy.Get(ctx)
		if err != nil {
			return browsersession.Bounds{}, err
		}
		return browsersession.Bounds{
			MaxIdleTimeout:      policy.MaxIdleTimeout,
			MaxAbsoluteLifetime: policy.MaxAbsoluteLifetime,
		}, nil
	})
}

// maxRunWall is the whole-run wall cap the control plane enforces at lease
// renewal; 0 is off.
func (r runSafety) maxRunWall(ctx context.Context) (time.Duration, error) {
	caps, err := r.runCaps.Get(ctx)
	if err != nil {
		return 0, err
	}
	return caps.MaxWall, nil
}

// startBrowserSessionLifecycle runs the session sweep and audit relay
// (browsersession.Lifecycle) on a routines.Loop: one replica per pass, every
// minute by default, changeable in the admin Routines screen. It runs in
// maintenance too: an audit record is not deferred work.
func startBrowserSessionLifecycle(
	ctx context.Context,
	pool *pgxpool.Pool,
	safety runSafety,
	logger *slog.Logger,
) error {
	if pool == nil {
		return nil
	}
	lifecycle, err := browsersession.NewLifecycle(pool, safety.sessionBounds())
	if err != nil {
		return fmt.Errorf("compose the browser session lifecycle: %w", err)
	}
	loop := &routines.Loop{
		Backend: routines.NewStore(pool),
		Def: routines.Definition{
			ID:              browsersession.LifecycleRoutineID,
			Owner:           routines.OwnerMain,
			Description:     "Records browser session expiry and writes session start, expiry, logout and revocation audit events.",
			DefaultSchedule: browsersession.LifecycleDefaultSchedule,
			MinInterval:     15 * time.Second,
		},
		Logger: logger,
	}
	go loop.Run(ctx, func(ctx context.Context) (string, error) {
		result, err := lifecycle.Pass(ctx)
		if err != nil {
			logger.Error("browser session lifecycle pass failed", "err", err)
			return routines.OutcomeError, err
		}
		if result.Expired > 0 || result.Events > 0 {
			logger.Info("browser session lifecycle pass",
				"expired", result.Expired, "audit_events", result.Events)
		}
		return routines.OutcomeOK, nil
	})
	return nil
}
