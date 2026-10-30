package breaker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
)

// RoutineID is the routines registry row of the breaker pass.
const RoutineID = "main.agent_breaker"

// DefaultSchedule runs the pass every minute (ADR-0041 decision 2).
const DefaultSchedule = "1m"

// The store's windows.
const (
	// IngestLookback is how far back each pass looks for ended runs that
	// are not in the breaker's window yet. Passes overlap, so a run whose
	// settlement committed late, or a pass that failed, does not lose a run.
	// A run already in the window is not read again.
	IngestLookback = 10 * time.Minute
	// IngestSettleGrace is how long a run waits after it settled before a
	// pass copies it: a step report that passed the claim check just before
	// the settlement commits in this time. After it, nothing about the run
	// changes, so the pass computes each run once.
	IngestSettleGrace = 30 * time.Second
	// Retention is how long the breaker's window keeps a run.
	Retention = 7 * 24 * time.Hour
)

// Store is the durable half (repos.AgentBreaker).
type Store interface {
	// IngestRuns copies the agent runs that ended within lookback, and are
	// not in the window yet, into the window and answers the agents it
	// touched. A run stopped at a run cap is marked: it is not a failure.
	IngestRuns(ctx context.Context, lookback time.Duration) ([]AgentKey, error)
	// SpendOverLimit answers the agents whose spend in the last hour
	// (counted from the later of an hour ago and the agent's last resume)
	// is over their own spend limit, with that spend. An agent's limit is
	// tierLimits[its tier] tightened by its override (Effective); 0 is no
	// limit. One read for all agents.
	SpendOverLimit(ctx context.Context, tierLimits map[agentlifecycle.Tier]float64) (map[AgentKey]float64, error)
	// AnySpendOverride reports whether any agent's override sets a spend
	// limit. The pass asks only when no tier sets one.
	AnySpendOverride(ctx context.Context) (bool, error)
	// Observe reads one agent: tier, override, suspension, its latest
	// window ended runs, and the hourly figures.
	Observe(ctx context.Context, key AgentKey, window int) (Observation, error)
	// Trip writes the breaker suspension (which cancels the agent's runs in
	// flight) and its security audit event in one transaction. It answers
	// *safety.AlreadySuspendedError when a suspension got there first.
	Trip(ctx context.Context, trip Trip) (safety.Suspension, error)
	// NotifyTrip tells the agent's owners and the project admins. Best
	// effort: it answers how many were notified.
	NotifyTrip(ctx context.Context, trip Trip, suspension safety.Suspension) (int, error)
	// Prune deletes window rows older than retention.
	Prune(ctx context.Context, retention time.Duration) (int64, error)
}

// Summary counts what one pass did.
type Summary struct {
	Enabled   bool
	Ingested  int
	Evaluated int
	Tripped   int
	Storm     bool
	Notified  int
	Pruned    int64
	Errors    int
}

func (s Summary) String() string {
	return fmt.Sprintf("enabled=%t agents=%d evaluated=%d tripped=%d storm=%t notified=%d pruned=%d errors=%d",
		s.Enabled, s.Ingested, s.Evaluated, s.Tripped, s.Storm, s.Notified, s.Pruned, s.Errors)
}

// Pass is the breaker routine's work.
type Pass struct {
	Store    Store
	Settings func(context.Context) (Settings, error)
	// Notify is told every suspension a pass wrote (the kill switch gate's
	// invalidation, local and over NATS).
	Notify  func(suspensionID int64)
	Metrics *Metrics
	Logger  *slog.Logger
}

// Run runs one pass.
func (p *Pass) Run(ctx context.Context) (Summary, error) {
	summary, err := p.run(ctx)
	if err != nil {
		p.Metrics.AddPassFailure()
	}
	return summary, err
}

func (p *Pass) run(ctx context.Context) (Summary, error) {
	var summary Summary
	if p.Store == nil || p.Settings == nil {
		return summary, errors.New("breaker: the store and the settings reader are required")
	}
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	settings, err := p.Settings(ctx)
	if err != nil {
		return summary, fmt.Errorf("breaker: read the circuit_breaker section: %w", err)
	}
	summary.Enabled = settings.Enabled
	if !settings.Enabled {
		return summary, nil
	}

	candidates := map[AgentKey]bool{}
	touched, err := p.Store.IngestRuns(ctx, IngestLookback)
	if err != nil {
		return summary, fmt.Errorf("breaker: copy the ended runs: %w", err)
	}
	for _, key := range touched {
		candidates[key] = true
	}
	summary.Ingested = len(touched)

	// Only the agents over their own spend limit are candidates by spend:
	// an agent that spends under its limit is not read.
	spend := map[AgentKey]float64{}
	tierLimits, tierSpend := spendLimits(settings)
	readSpend := tierSpend
	if !tierSpend {
		if readSpend, err = p.Store.AnySpendOverride(ctx); err != nil {
			return summary, fmt.Errorf("breaker: read the spend overrides: %w", err)
		}
	}
	if readSpend {
		spend, err = p.Store.SpendOverLimit(ctx, tierLimits)
		if err != nil {
			return summary, fmt.Errorf("breaker: read the spend of the last hour: %w", err)
		}
		for key := range spend {
			candidates[key] = true
		}
	}

	window := settings.Tier(agentlifecycle.TierLow).Rules.ErrorRateWindowRuns
	var trips []Trip
	for key := range candidates {
		observation, err := p.Store.Observe(ctx, key, window)
		if err != nil {
			summary.Errors++
			logger.WarnContext(ctx, "breaker: read an agent", "project_id", key.ProjectID,
				"application_id", key.ApplicationID, "err", err)
			continue
		}
		observation.SpendLastHourUSD = spend[key]
		summary.Evaluated++
		if trip, ok := Evaluate(settings, observation); ok {
			trips = append(trips, trip)
		}
	}

	// The storm limit: when more agents would trip on a rule a failing
	// dependency can cause than the limit allows, trip none of them.
	sensitive := 0
	for _, trip := range trips {
		if trip.Rule.dependencySensitive() {
			sensitive++
		}
	}
	if settings.StormLimit > 0 && sensitive > settings.StormLimit {
		summary.Storm = true
		p.Metrics.AddStorm()
		logger.WarnContext(ctx, "breaker: storm, no agent tripped on error_rate or tool_failure_streak",
			"would_trip", sensitive, "storm_limit", settings.StormLimit)
		kept := trips[:0]
		for _, trip := range trips {
			if !trip.Rule.dependencySensitive() {
				kept = append(kept, trip)
			}
		}
		trips = kept
	}

	for _, trip := range trips {
		suspension, err := p.Store.Trip(ctx, trip)
		var already *safety.AlreadySuspendedError
		if errors.As(err, &already) {
			continue
		}
		if err != nil {
			summary.Errors++
			logger.ErrorContext(ctx, "breaker: write the suspension", "project_id", trip.Key.ProjectID,
				"application_id", trip.Key.ApplicationID, "rule", trip.Rule, "err", err)
			continue
		}
		summary.Tripped++
		p.Metrics.AddTrip(trip.Rule)
		if p.Notify != nil {
			p.Notify(suspension.ID)
		}
		logger.WarnContext(ctx, "breaker: agent suspended", "project_id", trip.Key.ProjectID,
			"application_id", trip.Key.ApplicationID, "rule", trip.Rule, "observed", trip.Observed,
			"threshold", trip.Threshold, "suspension_id", suspension.ID, "cancelled_runs", suspension.CancelledRuns)
		notified, err := p.Store.NotifyTrip(ctx, trip, suspension)
		summary.Notified += notified
		if err != nil {
			summary.Errors++
			logger.WarnContext(ctx, "breaker: notify the owners", "suspension_id", suspension.ID, "err", err)
		}
	}

	pruned, err := p.Store.Prune(ctx, Retention)
	if err != nil {
		summary.Errors++
		logger.WarnContext(ctx, "breaker: prune the window", "err", err)
	}
	summary.Pruned = pruned
	if summary.Errors > 0 {
		return summary, fmt.Errorf("breaker: %d errors in the pass", summary.Errors)
	}
	return summary, nil
}

// spendLimits answers each tier's spend limit, and whether any tier has
// one.
func spendLimits(settings Settings) (map[agentlifecycle.Tier]float64, bool) {
	limits, any := map[agentlifecycle.Tier]float64{}, false
	for _, tier := range agentlifecycle.Tiers {
		usd := settings.Tier(tier).Rules.SpendPerHourUSD
		limits[tier] = usd
		any = any || usd > 0
	}
	return limits, any
}
