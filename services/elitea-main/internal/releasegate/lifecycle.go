package releasegate

import (
	"context"
	"errors"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
)

// LifecycleGate is the gate as the change request flow takes it
// (agentlifecycle.ReleaseGate, ADR-0040 decisions 3 and 4). The gate's
// errors become the flow's typed refusals.
type LifecycleGate struct{ Gate *Gate }

var _ agentlifecycle.ReleaseGate = LifecycleGate{}

// StartGate starts a change gate. An agent with no gate suite gets a gate
// that passed with no suite, so the answer is never 0.
func (l LifecycleGate) StartGate(ctx context.Context, tenantProjectID, applicationID, candidateVersionID, currentReleaseID int64) (int64, error) {
	var current *int64
	if currentReleaseID > 0 {
		current = &currentReleaseID
	}
	id, err := l.Gate.StartGate(ctx, tenantProjectID, applicationID, candidateVersionID, current)
	return id, lifecycleError(err)
}

// GateResult reads the gate, settling it when its runs are terminal.
func (l LifecycleGate) GateResult(ctx context.Context, tenantProjectID, gateID int64) (agentlifecycle.GateResult, error) {
	gate, err := l.Gate.Result(ctx, tenantProjectID, gateID)
	if err != nil {
		return agentlifecycle.GateResult{}, lifecycleError(err)
	}
	failures := make([]string, 0, len(gate.Failures))
	for _, failure := range gate.Failures {
		failures = append(failures, failure.Code)
	}
	return agentlifecycle.GateResult{
		State: agentlifecycle.GateState(gate.State), Failures: failures, Overridden: gate.Overridden(),
	}, nil
}

// Override records a governance override of a failed gate.
func (l LifecycleGate) Override(ctx context.Context, tenantProjectID, gateID, actorID int64, reason string) error {
	return lifecycleError(l.Gate.Override(ctx, tenantProjectID, gateID, actorID, reason))
}

// RegressedSince answers the failed scheduled gate of the published
// release, 0 when none.
func (l LifecycleGate) RegressedSince(ctx context.Context, tenantProjectID, applicationID int64, since time.Time) (int64, error) {
	gate, err := l.Gate.RegressedSince(ctx, tenantProjectID, applicationID, since)
	if err != nil || gate == nil {
		return 0, err
	}
	return gate.ID, nil
}

func lifecycleError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrGateNotFailed), errors.Is(err, ErrAlreadyOverridden):
		return agentlifecycle.Refuse(agentlifecycle.GateNotOverridableCode, "The change request has no failed gate to override.")
	case errors.Is(err, ErrReasonRequired), errors.Is(err, ErrInvalidActor):
		return agentlifecycle.Refuse(agentlifecycle.InvalidCode, "An override needs the acting user and a reason of 1 to 200 characters.")
	case errors.Is(err, ErrVersionNotFound), errors.Is(err, ErrReleaseNotFound), errors.Is(err, ErrGateNotFound):
		return agentlifecycle.Refuse(agentlifecycle.NotFoundCode, "The release gate's candidate, release or gate does not exist.")
	}
	return err
}
