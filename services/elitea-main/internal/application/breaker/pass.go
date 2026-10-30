package breaker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
	// IngestLookback is how far back each pass copies ended runs into the
	// breaker's window. Passes overlap, so a run whose settlement committed
	// late, or whose step records arrived after it ended, is still counted
	// (the copy is idempotent).
	IngestLookback = 10 * time.Minute
	// Retention is how long the breaker's window keeps a run.
	Retention = 7 * 24 * time.Hour
)

// Store is the durable half (repos.AgentBreaker).
type Store interface {
	// IngestRuns copies the agent runs that ended within lookback into the
	// window and answers the agents it touched.
	IngestRuns(ctx context.Context, lookback time.Duration) ([]AgentKey, error)
	// SpendLastHour answers each agent's spend in the last hour, counted
	// from the later of an hour ago and the agent's last resume.
	SpendLastHour(ctx context.Context) (map[AgentKey]float64, error)
	// AnySpendOverride reports whether any agent's override sets a spend
	// limit (the spend read runs only when some limit can apply).
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

	spend := map[AgentKey]float64{}
	lowest, tierSpend := lowestSpendLimit(settings)
	overrides, err := p.Store.AnySpendOverride(ctx)
	if err != nil {
		return summary, fmt.Errorf("breaker: read the spend overrides: %w", err)
	}
	if tierSpend || overrides {
		spend, err = p.Store.SpendLastHour(ctx)
		if err != nil {
			return summary, fmt.Errorf("breaker: read the spend of the last hour: %w", err)
		}
		if overrides {
			// An override can set a limit below every tier's.
			lowest = 0
		}
		for key, usd := range spend {
			if usd > lowest {
				candidates[key] = true
			}
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

// lowestSpendLimit answers the lowest tier spend limit, and whether any
// tier has one.
func lowestSpendLimit(settings Settings) (float64, bool) {
	lowest, any := math.Inf(1), false
	for _, tier := range agentlifecycle.Tiers {
		if usd := settings.Tier(tier).Rules.SpendPerHourUSD; usd > 0 {
			lowest, any = math.Min(lowest, usd), true
		}
	}
	return lowest, any
}
