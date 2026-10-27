package control

import (
	"context"
	"log/slog"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// ReasonAuthorityRevoked is the stable reason of a governed run refused at
// ClaimCommand, or cancelled at RenewLease, because an epoch its stored
// security context pins is stale (#1121): its agent binding, mandate or
// delegation was revoked, suspended, expired or changed since admission.
const ReasonAuthorityRevoked = "AUTHORITY_REVOKED"

// authorityRevokedMessage is the safe message of the claim refusal.
const authorityRevokedMessage = "The governed execution's authority was revoked."

// GovernedRunChecker answers whether a run is governed and, if so, which of
// the epochs its stored security context pins are stale
// (*epoch.RunChecker).
type GovernedRunChecker interface {
	Check(ctx context.Context, executionID string) (governed bool, stale []epoch.SecurityEpoch, err error)
}

// AuthorityRevoker durably cancels a governed run whose authority was
// revoked: desired state CANCELLED, so the worker stops and settlement
// writes the terminal state (*epoch.PostgresAuthorityRevoker).
type AuthorityRevoker interface {
	RevokeAuthority(ctx context.Context, executionID string, generation uint64) error
}

type governedEpochs struct {
	checker GovernedRunChecker
	revoker AuthorityRevoker
	revoked metric.Int64Counter
}

// WithGovernedEpochs checks the epochs of a governed run at ClaimCommand
// and RenewLease (#1121). Composed only with the governance flag on; nil
// keeps both paths free of a new statement.
func (s *Server) WithGovernedEpochs(checker GovernedRunChecker, revoker AuthorityRevoker) (*Server, error) {
	if checker == nil || revoker == nil {
		s.epochs = nil
		return s, nil
	}
	meter := s.config.Meter
	if meter == nil {
		meter = otel.Meter("elitea-main")
	}
	revoked, err := meter.Int64Counter("elitea.runtime.governed.authority_revoked",
		metric.WithDescription("Governed runs refused at claim or cancelled at lease renewal because an epoch is stale, by point."))
	if err != nil {
		return nil, err
	}
	s.epochs = &governedEpochs{checker: checker, revoker: revoker, revoked: revoked}
	return s, nil
}

func (g *governedEpochs) record(ctx context.Context, point, executionID string, generation uint64, stale []epoch.SecurityEpoch) {
	g.revoked.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(attribute.String("point", point)))
	kinds := make([]string, 0, len(stale))
	for _, key := range stale {
		kinds = append(kinds, string(key.Kind))
	}
	slog.WarnContext(ctx, "governed run authority revoked",
		"point", point, "reason", ReasonAuthorityRevoked, "execution_id", executionID,
		"generation", generation, "stale_kinds", kinds)
}

// claimRejection refuses a governed command whose run's epochs are stale,
// before any claim is attempted, and durably cancels the run so its job
// settles instead of waiting for a claim that never comes.
func (g *governedEpochs) claimRejection(ctx context.Context, command *runtimev1.WorkerCommandV1) *runtimev1.ClaimCommandResponseV1 {
	if g == nil {
		return nil
	}
	governed, stale, err := g.checker.Check(ctx, command.GetExecutionId())
	if err != nil {
		logClaimRejection(ctx, "governed_epochs", command, err)
		return claimRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The durable claim state is temporarily unavailable.", true)
	}
	if !governed {
		// Field 65 without a stored context: the context admission recorded
		// is gone.
		return claimRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_GOVERNED_CONTEXT_MISSING, "The governed execution's command carries no security context.", false)
	}
	if len(stale) == 0 {
		return nil
	}
	g.record(ctx, "claim", command.GetExecutionId(), command.GetGeneration(), stale)
	if err := g.revoker.RevokeAuthority(ctx, command.GetExecutionId(), command.GetGeneration()); err != nil {
		slog.WarnContext(ctx, "governed run cancellation failed", "point", "claim", "execution_id", command.GetExecutionId())
	}
	return claimRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED, authorityRevokedMessage, false)
}

// renewDesiredState answers CANCELLED for a governed run whose epochs are
// stale, after cancelling it durably; otherwise desired unchanged. A read
// failure keeps the lease's answer: renewal is not where an outage should
// kill a run, and every redemption re-checks the epochs and denies.
func (g *governedEpochs) renewDesiredState(ctx context.Context, fence runtimedomain.Fence, desired runtimedomain.DesiredState) runtimedomain.DesiredState {
	if g == nil || desired != runtimedomain.DesiredRunning {
		return desired
	}
	governed, stale, err := g.checker.Check(ctx, fence.ExecutionID)
	if err != nil {
		slog.WarnContext(ctx, "governed run epoch check failed at lease renewal", "execution_id", fence.ExecutionID)
		return desired
	}
	if !governed || len(stale) == 0 {
		return desired
	}
	g.record(ctx, "renew", fence.ExecutionID, fence.Generation, stale)
	if err := g.revoker.RevokeAuthority(ctx, fence.ExecutionID, fence.Generation); err != nil {
		slog.WarnContext(ctx, "governed run cancellation failed", "point", "renew", "execution_id", fence.ExecutionID)
	}
	return runtimedomain.DesiredCancelled
}
