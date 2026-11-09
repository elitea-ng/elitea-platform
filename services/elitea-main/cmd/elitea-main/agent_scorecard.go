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

// agentScorecardRoutineID is the routines registry row of the daily agent
// scorecard (ADR-0050 decision 5h, #1616).
const agentScorecardRoutineID = "main.agent_scorecard"

// startAgentScorecard runs the scorecard pass on a routines.Loop, daily by
// default: one replica per pass, paused in maintenance. It scores every
// active agent from platform data (run outcomes, breaker trips, approval
// rejections, guardrail hits, spend, people's ratings) and prunes the
// scorecards past retention. It never proposes or applies a ladder stage:
// a person, or an HR agent's proposal a person decides, does.
func startAgentScorecard(ctx context.Context, pool *pgxpool.Pool, ks killSwitch, logger *slog.Logger) error {
	store, err := dbrepos.ComposeAgentWorkforce(pool, ks.gate.Notify)
	if err != nil {
		return fmt.Errorf("compose the agent workforce store: %w", err)
	}
	loop := &routines.Loop{
		Backend: routines.NewStore(pool),
		Def: routines.Definition{
			ID:              agentScorecardRoutineID,
			Owner:           routines.OwnerMain,
			Description:     "Scores every active agent once a day from platform data (runs, failures, breaker trips, approval rejections, guardrail hits, spend, ratings) for the workforce view (ADR-0050).",
			DefaultSchedule: "24h",
			MinInterval:     time.Hour,
		},
		Logger: logger,
	}
	go loop.Run(ctx, func(ctx context.Context) (string, error) {
		state, err := platformconfig.LoadMaintenance(ctx, pool)
		if err != nil {
			logger.Error("agent scorecard: maintenance switch unreadable", "err", err)
			return routines.OutcomeError, err
		}
		if state.Enabled {
			return routines.OutcomeMaintenance, nil
		}
		summary, err := store.ScorecardPass(ctx)
		if err != nil {
			logger.Error("agent scorecard pass failed", "err", err, "summary", summary.String())
			return routines.OutcomeError, err
		}
		logger.Info("agent scorecard pass", "summary", summary.String())
		return routines.OutcomeOK, nil
	})
	return nil
}
