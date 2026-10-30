package control

import (
	"context"
	"errors"
	"log/slog"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/remoteagent"
)

// DelegationAssertions mints the delegation assertions of claimed runs
// (ADR-0044 decision 4, *remoteagent.MintService).
type DelegationAssertions interface {
	Mint(ctx context.Context, executionID string, generation uint64, audience string) (remoteagent.MintResult, error)
}

// WithDelegationAssertions serves MintDelegationAssertion. Nil answers
// UNSUPPORTED_CAPABILITY: the worker then calls without an assertion, or
// refuses the call on a governed run.
func (s *Server) WithDelegationAssertions(assertions DelegationAssertions) *Server {
	s.delegationAssertions = assertions
	return s
}

// MintDelegationAssertion signs one delegation assertion for a live claim.
// The peer is authenticated and the fence verified live, as
// ReportEgressSummary does; a released or stale claim is refused, and so is
// an audience that is not an MCP endpoint admitted in the run.
func (s *Server) MintDelegationAssertion(ctx context.Context, request *runtimev1.MintDelegationAssertionRequestV1) (*runtimev1.MintDelegationAssertionResponseV1, error) {
	if request == nil || request.GetIdentity() == nil || request.GetFence() == nil || hasUnknownFields(request.ProtoReflect()) ||
		request.GetAudience() == "" || len(request.GetAudience()) > eliteanative.MaxDelegationAudienceBytes {
		return mintRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The delegation assertion request is malformed.", false, false), nil
	}
	if s.delegationAssertions == nil {
		return mintRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY, "Delegation assertions are not served by this deployment.", false, false), nil
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || workloadIdentity == "" {
		return mintRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false, false), nil
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), workloadIdentity)
	if err != nil {
		return mintRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false, false), nil
	}
	if err := s.claims.VerifyActive(ctx, fence); err != nil {
		code, message, retryable := safeRuntimeError(err)
		return mintRejection(code, message, retryable, false), nil
	}
	result, err := s.delegationAssertions.Mint(ctx, fence.ExecutionID, fence.Generation, request.GetAudience())
	switch {
	case errors.Is(err, remoteagent.ErrAudienceNotAdmitted):
		return mintRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED,
			"The audience is not an endpoint admitted in this run.", false, result.Required), nil
	case errors.Is(err, remoteagent.ErrSignerUnavailable):
		return mintRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY,
			"No agent assertion key is configured.", false, result.Required), nil
	case errors.Is(err, remoteagent.ErrScopeTooLarge):
		return mintRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_RESOURCE_EXHAUSTED,
			"The run's scope is too large for an assertion.", false, result.Required), nil
	case err != nil:
		slog.WarnContext(ctx, "delegation assertion not minted", "execution_id", fence.ExecutionID, "err", err)
		return mintRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE,
			"The delegation assertion could not be minted; retry.", true, result.Required), nil
	}
	switch result.Disposition {
	case remoteagent.Disabled:
		return &runtimev1.MintDelegationAssertionResponseV1{
			Disposition: runtimev1.DelegationAssertionDispositionV1_DELEGATION_ASSERTION_DISPOSITION_V1_DISABLED,
		}, nil
	case remoteagent.Issued:
		return &runtimev1.MintDelegationAssertionResponseV1{
			Disposition:         runtimev1.DelegationAssertionDispositionV1_DELEGATION_ASSERTION_DISPOSITION_V1_ISSUED,
			Assertion:           result.Token,
			ExpiresAtUnixMillis: result.ExpiresAt.UnixMilli(),
			Required:            result.Required,
		}, nil
	}
	return mintRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The delegation assertion was not minted.", false, result.Required), nil
}

func mintRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable, required bool) *runtimev1.MintDelegationAssertionResponseV1 {
	return &runtimev1.MintDelegationAssertionResponseV1{Rejection: runtimeError(code, message, retryable), Required: required}
}
