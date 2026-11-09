package main

// The heartbeat of proactive agents (ADR-0050 decision 2, issue 1618): the
// agent.heartbeat.scan.v1 job on elitea-main's platform scheduler. Cloud
// only: desktop agents have no heartbeat in v1.

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	v2pipelinetriggers "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/heartbeat"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	schedulingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/scheduling"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// agentHeartbeatDeps is what the heartbeat composition takes. Start, Mandates
// and Permissions may be absent: then every act degrades to notify and says
// why.
type agentHeartbeatDeps struct {
	Pool        *pgxpool.Pool
	Start       heartbeat.Starter
	Mandates    *runmandate.Service
	Gate        heartbeat.Gate
	Completer   runbrief.Completer
	Models      runbrief.ModelResolver
	Permissions auth.PermissionResolver
	InstanceID  string
	Logger      *slog.Logger
}

// startAgentHeartbeatRunner registers the job and starts its runner.
func startAgentHeartbeatRunner(ctx context.Context, deps agentHeartbeatDeps) error {
	if deps.Pool == nil || deps.Gate == nil || deps.Completer == nil {
		return nil
	}
	config := heartbeat.Config{
		Store: dbrepos.NewAgentHeartbeats(deps.Pool), Gate: deps.Gate,
		Completer: deps.Completer, Models: deps.Models, Logger: deps.Logger,
	}
	// Typed nils stay out of the interfaces (the #86 trap).
	if deps.Start != nil {
		config.Start = deps.Start
	}
	if deps.Mandates != nil {
		config.Mandates = deps.Mandates
	}
	if deps.Permissions != nil {
		config.CanRun = ownerCanRun(deps.Permissions)
	}
	service, err := heartbeat.New(config)
	if err != nil {
		return err
	}
	job, err := heartbeat.NewScanJob(service)
	if err != nil {
		return err
	}
	cadence, err := schedulingapp.ParseCron(heartbeat.ScanJobCadence)
	if err != nil {
		return fmt.Errorf("parse the heartbeat cadence: %w", err)
	}
	runnerConfig := schedulingapp.Config{
		InstanceID:      pipelineScheduleInstanceID(deps.InstanceID),
		LeaseDuration:   2 * time.Minute,
		MaxParallel:     1,
		PageSize:        1,
		MaxPagesPerTick: 2,
	}
	registry, err := schedulingapp.NewRegistry(runnerConfig.LeaseDuration, schedulingapp.Job{
		ID:          heartbeat.ScanJobID,
		Revision:    heartbeat.ScanJobRevision,
		Mode:        schedulingapp.ModeDurableAdmission,
		Schedule:    cadence,
		Timeout:     heartbeat.ScanJobTimeout,
		Handler:     job,
		Description: "Wakes the proactive agents whose heartbeat is due.",
		Cadence:     heartbeat.ScanJobCadence,
		MinInterval: time.Minute,
	})
	if err != nil {
		return fmt.Errorf("register the heartbeat job: %w", err)
	}
	occurrences, err := dbrepos.NewScheduleOccurrenceRepository(deps.Pool)
	if err != nil {
		return err
	}
	runner, err := schedulingapp.NewRunner(occurrences, registry, runnerConfig, deps.Logger)
	if err != nil {
		return fmt.Errorf("construct the heartbeat runner: %w", err)
	}
	controls, err := dbrepos.NewPlatformRoutineControls(deps.Pool)
	if err != nil {
		return err
	}
	runner.WithControls(controls)
	go func() {
		if err := runner.Run(ctx); err != nil && ctx.Err() == nil {
			deps.Logger.Error("the agent heartbeat runner stopped", "err", err)
		}
	}()
	return nil
}

// ownerCanRun re-reads the owner's run permission at every act, as a
// pipeline schedule re-reads its run identity's.
func ownerCanRun(permissions auth.PermissionResolver) heartbeat.RunPermission {
	return func(ctx context.Context, projectID, userID int64) (bool, error) {
		id := strconv.FormatInt(userID, 10)
		resolution, err := permissions.ResolvePermissions(ctx, auth.User{ID: id, UserID: id},
			v2pipelinetriggers.RunPermissionMode, strconv.FormatInt(projectID, 10))
		if err != nil {
			// Fail closed: no decision is not "allowed".
			return false, nil
		}
		for _, permission := range resolution.Permissions {
			if permission == v2pipelinetriggers.RunPermission {
				return true, nil
			}
		}
		return false, nil
	}
}
