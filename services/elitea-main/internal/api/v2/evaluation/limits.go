package evaluation

import (
	"context"
	"fmt"
	"log/slog"
)

// Run limits (issue 2013, step 1).
//
// The orchestrator enforces a run's token and cost limits from the usage the
// gateway already attributed to the run (attribution.go): every model call of a
// run carries `eval:<run>:...` and the gateway logs its tokens. Usage is read
// BETWEEN calls, so a limit is reached by the call that crosses it, not
// prevented before it, and a gateway log written a moment late is counted at
// the next check.
//
//   - per RUN, checked before each case. When the limit is reached the run
//     either stops (errored, with the reason; the cases scored so far stay) or,
//     with on_run_limit=report, carries on and only records the limit on the
//     row (Run.LimitReached).
//   - per CASE, checked before each judge call. A case that reached its limit
//     is not judged further: the remaining judge bindings of that case are
//     stored as `skipped` with the reason, and the run goes on.
//
// A usage read that fails is logged and treated as "unknown, under the limit":
// a missing request log must not fail every run, and the failure is visible.
// The cost is the catalogue's estimate; a call the catalogue does not price
// adds tokens and no cost, so a token limit is the exact one.

// Usage is the spend attributed to a run or to one case of it.
type Usage struct {
	// Tokens are prompt plus completion tokens.
	Tokens int64
	// CostUSD is the estimated cost.
	CostUSD float64
}

// UsageReader reads the usage attributed to a run (caseID empty) or to one
// case (the agent turn and every judge call of it).
type UsageReader interface {
	EvaluationUsage(ctx context.Context, projectID, runID, caseID string) (Usage, error)
}

// LimitMarker records which run limit a run reached. The repository
// implements it; the orchestrator type-asserts it, so a test double that does
// not care is not forced to.
type LimitMarker interface {
	MarkLimitReached(ctx context.Context, projectID, runID, limit string) error
}

// WithUsage lets the orchestrator enforce run limits. Without a reader a run
// with limits runs unlimited, which Execute logs.
func (o *Orchestrator) WithUsage(usage UsageReader) *Orchestrator {
	o.usage = usage
	return o
}

// runLimitReached answers which run limit usage has reached ("" for none) and
// a readable account of it.
func runLimitReached(limits RunLimits, usage Usage) (string, string) {
	if limits.TokenLimitPerRun != nil && usage.Tokens >= *limits.TokenLimitPerRun {
		return LimitReachedTokens, fmt.Sprintf("the run's token limit was reached (%d of %d tokens)",
			usage.Tokens, *limits.TokenLimitPerRun)
	}
	if limits.CostLimitPerRun != nil && usage.CostUSD >= *limits.CostLimitPerRun {
		return LimitReachedCost, fmt.Sprintf("the run's cost limit was reached (%.4f of %.4f USD)",
			usage.CostUSD, *limits.CostLimitPerRun)
	}
	return "", ""
}

// caseLimitReason answers why a case may not make another model call, or "".
func caseLimitReason(limits RunLimits, usage Usage) string {
	if limits.TokenLimitPerCase != nil && usage.Tokens >= *limits.TokenLimitPerCase {
		return fmt.Sprintf("the case's token limit was reached (%d of %d tokens)", usage.Tokens, *limits.TokenLimitPerCase)
	}
	if limits.CostLimitPerCase != nil && usage.CostUSD >= *limits.CostLimitPerCase {
		return fmt.Sprintf("the case's cost limit was reached (%.4f of %.4f USD)", usage.CostUSD, *limits.CostLimitPerCase)
	}
	return ""
}

// limitGuard is one run's limit state during a walk.
type limitGuard struct {
	o       *Orchestrator
	ref     RunRef
	limits  RunLimits
	enabled bool
	// reached is the run limit already recorded, so it is written once.
	reached string
	warned  bool
}

func (o *Orchestrator) newLimitGuard(ref RunRef, run Run) *limitGuard {
	guard := &limitGuard{o: o, ref: ref}
	if run.Snapshot.Limits == nil || !run.Snapshot.Limits.HasAny() {
		return guard
	}
	guard.limits = *run.Snapshot.Limits
	if o.usage == nil {
		o.logger.Warn("evaluation: the run has limits but no usage reader is composed; it runs unlimited",
			"project_id", ref.ProjectID, "run_id", ref.RunID)
		return guard
	}
	guard.enabled = true
	return guard
}

func (g *limitGuard) read(ctx context.Context, caseID string) (Usage, bool) {
	usage, err := g.o.usage.EvaluationUsage(context.WithoutCancel(ctx), g.ref.ProjectID, g.ref.RunID, caseID)
	if err != nil {
		if !g.warned {
			g.warned = true
			g.o.logger.Warn("evaluation: could not read the run's usage; its limits are not enforced until it can",
				slog.String("project_id", g.ref.ProjectID), slog.String("run_id", g.ref.RunID), slog.Any("error", err))
		}
		return Usage{}, false
	}
	return usage, true
}

// beforeCase checks the run limit. It returns an error only when the run must
// stop; with on_run_limit=report it records the limit and returns nil.
func (g *limitGuard) beforeCase(ctx context.Context, done, total int) error {
	if !g.enabled || !g.limits.hasRunLimit() {
		return nil
	}
	usage, ok := g.read(ctx, "")
	if !ok {
		return nil
	}
	which, reason := runLimitReached(g.limits, usage)
	if which == "" {
		return nil
	}
	if g.reached == "" {
		g.reached = which
		if marker, ok := g.o.repo.(LimitMarker); ok {
			if err := marker.MarkLimitReached(context.WithoutCancel(ctx), g.ref.ProjectID, g.ref.RunID, which); err != nil {
				g.o.logger.Error("evaluation: could not record the reached limit",
					"project_id", g.ref.ProjectID, "run_id", g.ref.RunID, "error", err)
			}
		}
	}
	if g.limits.OnRunLimit == RunLimitActionReport {
		return nil
	}
	return fmt.Errorf("%s: the run stopped after %d of %d cases", reason, done, total)
}

// caseBlocked answers why the case may not be judged further, or "".
func (g *limitGuard) caseBlocked(ctx context.Context, caseID string) string {
	if !g.enabled || !g.limits.hasCaseLimit() {
		return ""
	}
	usage, ok := g.read(ctx, caseID)
	if !ok {
		return ""
	}
	return caseLimitReason(g.limits, usage)
}
