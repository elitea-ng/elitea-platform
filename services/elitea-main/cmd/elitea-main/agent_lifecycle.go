package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/releasegate"
)

// agentRecertificationRoutineID is the routines registry row of the
// recertification pass (ADR-0040 decision 5).
const agentRecertificationRoutineID = "main.agent_recertification"

// startAgentRecertification runs the recertification pass on a
// routines.Loop, hourly by default: one replica per pass. It moves
// certified agents to due (the 30-day window, a new or rolled-back
// release, a drifted toolkit pin, a failed scheduled re-evaluation) and
// lapses them after recertify_by; with lapse_action suspend it writes the
// certification suspension the kill switch refuses runs with
// (agent_certification_lapsed).
func startAgentRecertification(
	ctx context.Context,
	pool *pgxpool.Pool,
	gate *releasegate.Gate,
	ks killSwitch,
	logger *slog.Logger,
) error {
	var lifecycleGate agentlifecycle.ReleaseGate = agentlifecycle.NoGate{}
	if gate != nil {
		lifecycleGate = releasegate.LifecycleGate{Gate: gate}
	}
	store, err := dbrepos.ComposeAgentLifecycle(pool, lifecycleGate, ks.gate.Notify)
	if err != nil {
		return fmt.Errorf("compose the agent lifecycle store: %w", err)
	}
	releases, err := dbrepos.NewAgentReleases(pool)
	if err != nil {
		return fmt.Errorf("compose the agent releases store: %w", err)
	}
	suspensions, err := dbrepos.NewExecutionSuspensionRepository(pool)
	if err != nil {
		return fmt.Errorf("compose the kill switch store: %w", err)
	}
	deps := dbrepos.RecertificationDeps{Drift: releases, Suspender: suspensions, Notify: ks.gate.Notify}
	loop := &routines.Loop{
		Backend: routines.NewStore(pool),
		Def: routines.Definition{
			ID:              agentRecertificationRoutineID,
			Owner:           routines.OwnerMain,
			Description:     "Moves certified agents to due and lapses overdue certifications; with lapse_action suspend it suspends a lapsed agent (ADR-0040).",
			DefaultSchedule: "1h",
			MinInterval:     5 * time.Minute,
		},
		Logger: logger,
	}
	go loop.Run(ctx, func(ctx context.Context) (string, error) {
		summary, err := store.RecertificationPass(ctx, deps)
		if err != nil {
			logger.Error("agent recertification pass failed", "err", err, "summary", summary.String())
			return routines.OutcomeError, err
		}
		if summary != (dbrepos.RecertificationSummary{}) {
			logger.Info("agent recertification pass", "summary", summary.String())
		}
		return routines.OutcomeOK, nil
	})
	return nil
}
