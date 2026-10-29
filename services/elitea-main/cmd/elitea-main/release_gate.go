package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/jackc/pgx/v5/pgxpool"

	v2evaluation "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/releasegate"
)

// releaseGateRoutineID is the routine registry row of the release gate's
// pass (elitea_runtime.platform_routines).
const releaseGateRoutineID = "main.release_gate"

// composeReleaseGate builds the evaluation release gate (ADR-0040 decision
// 4) over the evaluation run storage and the same run starter the run
// route uses, so its runs execute on the one orchestrator. It starts the
// gate's routine: every pass evaluates the pending gates whose runs are
// terminal and starts the scheduled re-evaluations that are due (decision
// 5). The routine runs on one replica at a time, every 5 minutes by
// default, changeable in the admin Routines screen. No pool or no run
// storage: no gate, and the gate routes answer 503.
func composeReleaseGate(
	ctx context.Context,
	pool *pgxpool.Pool,
	runs v2evaluation.RunRepository,
	dimensions v2evaluation.Repository,
	orchestrator v2evaluation.Enqueuer,
	permissions auth.PermissionResolver,
	logger *slog.Logger,
) (*releasegate.Gate, error) {
	if pool == nil || runs == nil || dimensions == nil {
		return nil, nil
	}
	store, err := dbrepos.NewReleaseGates(pool)
	if err != nil {
		return nil, fmt.Errorf("compose the release gate store: %w", err)
	}
	gate, err := releasegate.New(store, runs, v2evaluation.NewRunStarter(runs, dimensions, orchestrator), logger)
	if err != nil {
		return nil, err
	}
	if permissions != nil {
		gate.WithActorCheck(releaseGateActorCheck(permissions))
	}

	loop := &routines.Loop{
		Backend: routines.NewStore(pool),
		Def: routines.Definition{
			ID:              releaseGateRoutineID,
			Owner:           routines.OwnerMain,
			Description:     "Evaluates release gates whose evaluation runs finished, and starts the scheduled re-evaluation of released agents.",
			DefaultSchedule: "5m",
			MinInterval:     time.Minute,
		},
		Logger: logger,
	}
	go loop.Run(ctx, func(ctx context.Context) (string, error) {
		result, err := gate.Pass(ctx)
		if err != nil {
			logger.Error("release gate pass failed", "err", err)
			return routines.OutcomeError, err
		}
		if result.Settled+result.Started > 0 {
			logger.Info("release gate pass", "settled", result.Settled, "started", result.Started)
		}
		return routines.OutcomeOK, nil
	})
	return gate, nil
}

// releaseGateActorCheck resolves again that the user who set a scheduled
// re-evaluation may still start evaluation runs in the project: the runs'
// model calls are made for that user.
func releaseGateActorCheck(permissions auth.PermissionResolver) releasegate.ActorCheck {
	return func(ctx context.Context, tenantProjectID, userID int64) (bool, error) {
		id := strconv.FormatInt(userID, 10)
		resolution, err := permissions.ResolvePermissions(ctx, auth.User{ID: id, UserID: id},
			auth.PermissionModeDefault, strconv.FormatInt(tenantProjectID, 10))
		if err != nil {
			return false, err
		}
		for _, permission := range resolution.Permissions {
			if permission == v2evaluation.PermissionRunCreate {
				return true, nil
			}
		}
		return false, nil
	}
}
