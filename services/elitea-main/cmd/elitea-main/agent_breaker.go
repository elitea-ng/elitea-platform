package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/breaker"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// startAgentBreaker runs the agent circuit breaker (ADR-0041 decision 2,
// #1498) on a routines.Loop: the registry row main.agent_breaker holds the
// schedule (every minute by default) and the lease, so one replica runs a
// pass. The pass reads the circuit_breaker section each time, so a save
// applies at the next pass; it does nothing while maintenance mode is on
// (dispatch is paused then) or while the section turns the breaker off. A
// trip writes a breaker suspension through the kill switch store, so the
// agent's runs in flight are cancelled and every replica's admission gate
// learns of it over NATS.
func startAgentBreaker(ctx context.Context, pool *pgxpool.Pool, ks killSwitch, metrics *breaker.Metrics, logger *slog.Logger) error {
	if pool == nil {
		return nil
	}
	appender, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return fmt.Errorf("agent breaker: security audit appender: %w", err)
	}
	store, err := dbrepos.NewAgentBreaker(pool, appender)
	if err != nil {
		return fmt.Errorf("compose the agent breaker store: %w", err)
	}
	pass := &breaker.Pass{
		Store: store,
		Settings: func(ctx context.Context) (breaker.Settings, error) {
			values, err := platformconfig.Load(ctx, pool, platformconfig.SectionCircuitBreaker)
			if platformconfig.UndefinedTable(err) {
				return breaker.DefaultSettings(), nil
			}
			if err != nil {
				return breaker.Settings{}, err
			}
			return breaker.SettingsFromValues(values), nil
		},
		Notify:  ks.gate.Notify,
		Metrics: metrics,
		Logger:  logger,
	}
	loop := &routines.Loop{
		Backend: routines.NewStore(pool),
		Def: routines.Definition{
			ID:              breaker.RoutineID,
			Owner:           routines.OwnerMain,
			Description:     "Checks each agent with recent runs against the circuit breaker rules (error rate, spend per hour, the same tool failing in a row, runs per hour) and suspends an agent whose rule trips (ADR-0041).",
			DefaultSchedule: breaker.DefaultSchedule,
			MinInterval:     time.Minute,
		},
		Logger: logger,
	}
	go loop.Run(ctx, func(ctx context.Context) (string, error) {
		state, err := platformconfig.LoadMaintenance(ctx, pool)
		if err != nil {
			logger.Error("agent breaker: maintenance switch unreadable", "err", err)
			return routines.OutcomeError, err
		}
		if state.Enabled {
			return routines.OutcomeMaintenance, nil
		}
		summary, err := pass.Run(ctx)
		if err != nil {
			logger.Error("agent breaker pass failed", "err", err, "summary", summary.String())
			return routines.OutcomeError, err
		}
		if summary.Tripped > 0 || summary.Storm {
			logger.Info("agent breaker pass", "summary", summary.String())
		}
		return routines.OutcomeOK, nil
	})
	return nil
}
