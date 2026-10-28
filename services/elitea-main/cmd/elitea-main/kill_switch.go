package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/jackc/pgx/v5/pgxpool"

	v2safety "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// killSwitch is the composed kill switch (ADR-0041 decision 1, #1493): one
// admission gate per replica, shared by the runtime plane's admission stores
// and the service the admin and project routes call, so a suspension set on
// this replica refuses the next start here at once and on every other
// replica after the NATS invalidation (or the snapshot age).
type killSwitch struct {
	gate    *safety.Gate
	service *safety.Service
	handler *v2safety.Handler
}

func newKillSwitch(pool *pgxpool.Pool, logger *slog.Logger) (killSwitch, error) {
	store, err := dbrepos.NewExecutionSuspensionRepository(pool)
	if err != nil {
		return killSwitch{}, fmt.Errorf("compose the kill switch store: %w", err)
	}
	gate, err := safety.NewGate(store, safety.GateOptions{Logger: logger})
	if err != nil {
		return killSwitch{}, fmt.Errorf("compose the kill switch gate: %w", err)
	}
	service, err := safety.NewService(store, gate)
	if err != nil {
		return killSwitch{}, fmt.Errorf("compose the kill switch service: %w", err)
	}
	handler, err := v2safety.NewHandler(service)
	if err != nil {
		return killSwitch{}, fmt.Errorf("compose the kill switch routes: %w", err)
	}
	return killSwitch{gate: gate, service: service, handler: handler}, nil
}

// startKillSwitchSelfTest runs the scheduled self-test on a routines.Loop:
// the registry row (main.kill_switch_selftest) holds the schedule, daily by
// default, and the lease, so one replica runs a pass. An operator changes the
// schedule or switches it off in the admin Routines screen. The pass does
// nothing while maintenance mode is on.
func startKillSwitchSelfTest(
	ctx context.Context,
	pool *pgxpool.Pool,
	ks killSwitch,
	recorder audit.Recorder,
	logger *slog.Logger,
) error {
	selfTest, err := safety.NewSelfTest(ks.service, ks.gate, safety.SelfTestOptions{
		Maintenance: func(ctx context.Context) (bool, error) {
			state, err := platformconfig.LoadMaintenance(ctx, pool)
			return state.Enabled, err
		},
		Record: killSwitchSelfTestRecorder(recorder, logger),
	})
	if err != nil {
		return fmt.Errorf("compose the kill switch self-test: %w", err)
	}
	loop := &routines.Loop{
		Backend: routines.NewStore(pool),
		Def: routines.Definition{
			ID:              safety.SelfTestRoutineID,
			Owner:           routines.OwnerMain,
			Description:     "Suspends a synthetic agent, checks that admission refuses it within the stop SLO, then resumes it (the kill switch self-test).",
			DefaultSchedule: safety.SelfTestDefaultSchedule,
			MinInterval:     time.Minute,
		},
		Logger: logger,
	}
	go loop.Run(ctx, selfTest.Pass)
	return nil
}

// killSwitchSelfTestRecorder writes each pass to centry.audit_events as a
// `schedule` event (the system tab of the audit trail), action
// safety.kill_switch.selftest, with is_error set on a failure.
func killSwitchSelfTestRecorder(recorder audit.Recorder, logger *slog.Logger) safety.SelfTestRecorder {
	return func(ctx context.Context, result safety.SelfTestResult) {
		duration := float64(result.Duration.Milliseconds())
		event := audit.Event{
			Timestamp:  time.Now().UTC(),
			EventType:  "schedule",
			Action:     "safety.kill_switch.selftest",
			DurationMS: &duration,
			IsError:    result.Err != nil,
			EntityType: "execution_suspension",
			EntityName: fmt.Sprintf("refused=%t resumed=%t stop_latency_ms=%d",
				result.Refused, result.Resumed, result.StopLatency.Milliseconds()),
		}
		if result.SuspensionID > 0 {
			id := result.SuspensionID
			event.EntityID = &id
		}
		if recorder != nil {
			recorder.Record(ctx, event)
		}
		if result.Err != nil {
			logger.Error("kill switch self-test failed", "err", result.Err,
				"refused", result.Refused, "resumed", result.Resumed)
			return
		}
		logger.Info("kill switch self-test passed", "stop_latency", result.StopLatency)
	}
}
