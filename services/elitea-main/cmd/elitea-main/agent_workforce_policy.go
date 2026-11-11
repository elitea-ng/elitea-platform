package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/jackc/pgx/v5/pgxpool"

	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// agentWorkforcePolicyRoutineID is the routines registry row of the
// workforce policy pass (ADR-0050 decisions 5c and 5k, #1617).
const agentWorkforcePolicyRoutineID = "main.agent_workforce_policy"

// startAgentWorkforcePolicy runs the workforce policy pass on a
// routines.Loop, hourly by default: one replica per pass, paused in
// maintenance. For each project that turned a policy on, it hires the open
// agent proposals the auto_hire policy admits, applies the auto_dismiss
// rules to the latest daily scorecards (main.agent_scorecard), applies the
// retire notices whose grace period ended and nobody objected to, and flags
// orphaned agents. It never demotes and never deletes. A project with no
// policy (the default) is never touched.
func startAgentWorkforcePolicy(ctx context.Context, pool *pgxpool.Pool, ks killSwitch, logger *slog.Logger) error {
	store, err := dbrepos.ComposeAgentWorkforce(pool, ks.gate.Notify)
	if err != nil {
		return fmt.Errorf("compose the agent workforce store: %w", err)
	}
	loop := &routines.Loop{
		Backend: routines.NewStore(pool),
		Def: routines.Definition{
			ID:              agentWorkforcePolicyRoutineID,
			Owner:           routines.OwnerMain,
			Description:     "Applies each project's automatic hiring and dismissal policy (off by default): hires low-tier read-only proposals on probation, moves agents to watch, suspend or retire after the grace period, flags orphaned agents (ADR-0050).",
			DefaultSchedule: "1h",
			MinInterval:     5 * time.Minute,
		},
		Logger: logger,
	}
	go loop.Run(ctx, func(ctx context.Context) (string, error) {
		state, err := platformconfig.LoadMaintenance(ctx, pool)
		if err != nil {
			logger.Error("agent workforce policy: maintenance switch unreadable", "err", err)
			return routines.OutcomeError, err
		}
		if state.Enabled {
			return routines.OutcomeMaintenance, nil
		}
		summary, err := store.WorkforcePolicyPass(ctx)
		if err != nil {
			logger.Error("agent workforce policy pass failed", "err", err, "summary", summary.String())
			return routines.OutcomeError, err
		}
		logger.Info("agent workforce policy pass", "summary", summary.String())
		return routines.OutcomeOK, nil
	})
	return nil
}
