package indexschedule

// Governed toolkit credentials of a scheduled index run (ADR-0039, #1473).
//
// A governed index run gets its toolkit credential only from the broker
// (RuntimeContentService.RedeemCredential, capability index.ingest.v1),
// under the mandate attached to the index. At the fire, after the mandate
// check, the executor freezes the toolkit's exact credential reference
// under the mandate's agent binding. The admission transaction writes the
// freeze into the run's security context, so the broker releases that one
// reference for that one toolkit, and nothing else.

import (
	"context"
	"errors"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

// CredentialFreezer freezes one toolkit's governed credential under an
// agent binding (*agentexecution.GovernedCredentials).
type CredentialFreezer interface {
	FreezeToolkit(ctx context.Context, tenantProjectID int64, agentBindingID string, toolkitID int64) (*agentexecutionapp.GovernedCredentialFreeze, error)
}

// governedCredentialRefusalPrefix leads the initialization failure reason
// of an occurrence whose toolkit credential cannot be frozen.
const governedCredentialRefusalPrefix = "GOVERNED_CREDENTIAL_REFUSED"

// WithCredentialFreezer makes a mandate-bound executor freeze the indexed
// toolkit's governed credential at each fire. A nil freezer leaves the
// executor as it was.
func (executor *CurrentExecutor) WithCredentialFreezer(freezer CredentialFreezer) *CurrentExecutor {
	if executor != nil && freezer != nil {
		executor.freezer = freezer
	}
	return executor
}

// freezeCredentials freezes the candidate's toolkit credential under the
// mandate admitMandate put in ctx. It answers the context the admission
// must run in, or the outcome of a refused occurrence. An error is a
// dependency failure.
func (executor *CurrentExecutor) freezeCredentials(
	ctx context.Context, candidate Candidate,
) (context.Context, *ExecutionOutcome, error) {
	authority, ok := securitycontext.RunAuthorityFrom(ctx)
	if executor.freezer == nil || !ok || authority.AgentBindingID == "" {
		// Nothing is frozen: the broker then denies every credential use of
		// the run, so the run fails closed.
		return ctx, nil, nil
	}
	freeze, err := executor.freezer.FreezeToolkit(ctx, candidate.ProjectID, authority.AgentBindingID, candidate.ToolkitID)
	var refusal *agentexecutionapp.GovernedCredentialAdmissionError
	if errors.As(err, &refusal) {
		refused := scheduleInitializationFailure(governedCredentialRefusalPrefix + ": " + refusal.Code)
		return ctx, &refused, nil
	}
	if err != nil {
		return ctx, nil, err
	}
	return agentexecutionapp.WithUnattendedCredentialFreeze(ctx, freeze), nil, nil
}
