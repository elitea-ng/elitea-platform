package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentexecution"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// steeringHandOffRoutineID is the routine registry row of the server's
// steering hand-off (elitea_runtime.platform_routines). Its lease makes one
// replica at a time run a pass.
const steeringHandOffRoutineID = "main.steering_handoff"

// startSteeringHandOff runs the server's hand-off of leftover steering
// messages (agentexecution.SteeringHandOffRoutine) on a routines.Loop: the
// registry row holds the schedule (10s by default) and the lease (#1414,
// #1430), so the pass runs on one replica at a time. Without the start use
// case or the steering store, nothing runs: the clients still hand off by
// themselves.
func startSteeringHandOff(
	ctx context.Context,
	pool *pgxpool.Pool,
	starter agentexecutionapi.StartUseCase,
	permissions auth.PermissionResolver,
	logger *slog.Logger,
) error {
	if pool == nil || starter == nil || permissions == nil {
		return nil
	}
	store, err := dbrepos.NewSteeringRepository(pool)
	if err != nil {
		return fmt.Errorf("construct steering hand-off store: %w", err)
	}
	routine, err := agentexecutionapp.NewSteeringHandOffRoutine(store, starter, steeringAuthorCheck(permissions), logger)
	if err != nil {
		return err
	}
	loop := &routines.Loop{
		Backend: routines.NewStore(pool),
		Def: routines.Definition{
			ID:              steeringHandOffRoutineID,
			Owner:           routines.OwnerMain,
			Description:     "Starts the next chat turn with steering messages that a turn ended before it used, when no client did within 10 seconds.",
			DefaultSchedule: "10s",
			MinInterval:     5 * time.Second,
		},
		Poll:   5 * time.Second,
		Logger: logger,
	}
	go loop.Run(ctx, func(ctx context.Context) (string, error) {
		result, passErr := routine.Pass(ctx)
		if passErr != nil {
			return routines.OutcomeError, passErr
		}
		if result.Started+result.Dropped+result.Retried > 0 {
			logger.Info("steering hand-off pass", "started", result.Started, "dropped", result.Dropped, "retried", result.Retried)
		}
		return routines.OutcomeOK, nil
	})
	return nil
}

// steeringAuthorCheck resolves the author's permission again at start time, as
// the pipeline triggers do for an unattended run: the author must still hold
// the send permission in the project.
func steeringAuthorCheck(permissions auth.PermissionResolver) agentexecutionapp.SteeringAuthorCheck {
	return func(ctx context.Context, projectID, userID int64) (bool, error) {
		id := strconv.FormatInt(userID, 10)
		resolution, err := permissions.ResolvePermissions(ctx, auth.User{ID: id, UserID: id},
			auth.PermissionModeDefault, strconv.FormatInt(projectID, 10))
		if err != nil {
			return false, err
		}
		for _, permission := range resolution.Permissions {
			if permission == agentexecutionapp.SteeringHandOffPermission {
				return true, nil
			}
		}
		return false, nil
	}
}
