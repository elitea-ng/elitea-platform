package control

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// ReasonCapReached is the stable reason of an agent run cancelled at lease
// renewal because it passed the whole-run wall cap (ADR-0041 decision 3,
// run_caps.max_wall_seconds). The projection shows the run as CAP_REACHED.
const ReasonCapReached = "cap_reached"

// RunWallCapEnforcer durably cancels a run past the cap
// (*repos.RunWallCapRepository) and reports whether it did.
type RunWallCapEnforcer interface {
	EnforceWallCap(ctx context.Context, executionID string, generation uint64, limit time.Duration) (bool, error)
}

// RunWallCapLimit answers the platform's cap; 0 is off.
type RunWallCapLimit func(ctx context.Context) (time.Duration, error)

var capAttribute = attribute.String("cap", "max_wall_seconds")

type runWallCap struct {
	limit    RunWallCapLimit
	enforcer RunWallCapEnforcer
	reached  metric.Int64Counter
}

// WithRunWallCap enforces the whole-run wall cap at RenewLease and
// ObserveDesiredState. The worker learns of the cancel on the same call,
// like a kill switch cancel; no worker change is needed. With the cap off
// (0, the default) the calls run no new statement.
func (s *Server) WithRunWallCap(limit RunWallCapLimit, enforcer RunWallCapEnforcer) (*Server, error) {
	if limit == nil || enforcer == nil {
		s.wallCap = nil
		return s, nil
	}
	meter := s.config.Meter
	if meter == nil {
		meter = otel.Meter("elitea-main")
	}
	reached, err := meter.Int64Counter("elitea.runtime.run_caps.reached",
		metric.WithDescription("Agent runs cancelled because they reached a run cap, by cap."))
	if err != nil {
		return nil, err
	}
	s.wallCap = &runWallCap{limit: limit, enforcer: enforcer, reached: reached}
	return s, nil
}

// desiredState answers CANCELLED for an agent run this call cancelled at
// the cap; otherwise desired unchanged. An unreadable cap or a failed write
// keeps the answer: the next renewal tries again.
func (c *runWallCap) desiredState(
	ctx context.Context, fence runtimedomain.Fence, capabilityID string, desired runtimedomain.DesiredState,
) runtimedomain.DesiredState {
	if c == nil || desired != runtimedomain.DesiredRunning {
		return desired
	}
	if capabilityID != executiondomain.AgentApplicationCapability &&
		capabilityID != executiondomain.AgentAdhocCapability {
		return desired
	}
	limit, err := c.limit(ctx)
	if err != nil {
		slog.WarnContext(ctx, "the run caps could not be read", "execution_id", fence.ExecutionID)
		return desired
	}
	if limit <= 0 {
		return desired
	}
	cancelled, err := c.enforcer.EnforceWallCap(ctx, fence.ExecutionID, fence.Generation, limit)
	if err != nil {
		slog.WarnContext(ctx, "the run wall cap could not be enforced", "execution_id", fence.ExecutionID)
		return desired
	}
	if !cancelled {
		return desired
	}
	c.reached.Add(context.WithoutCancel(ctx), 1,
		metric.WithAttributes(capAttribute))
	slog.WarnContext(ctx, "agent run reached the whole-run wall cap",
		"reason", ReasonCapReached, "cap", "max_wall_seconds", "limit_seconds", int64(limit/time.Second),
		"execution_id", fence.ExecutionID, "generation", fence.Generation)
	return runtimedomain.DesiredCancelled
}
