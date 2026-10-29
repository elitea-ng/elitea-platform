package agentlifecycle

import "context"

// ReleaseGate is the evaluation gate of ADR-0040 decision 4, as the change
// request flow sees it. The flow never runs an evaluation: it starts the
// gate when a change request is submitted, reads its result before every
// approval, and records a governance override.
//
// The default is NoGate ("no gate suites": every gate passes at once). The
// evaluation package supplies the real implementation; the composition
// root swaps it in (RouterConfig.ReleaseGate).
type ReleaseGate interface {
	// StartGate starts the gate for a candidate. candidateVersionID is the
	// draft version a release change request would publish, or the
	// version of the release a recertification certifies again.
	// currentReleaseID is the agent's published release (0 when none):
	// the regression suite runs against it. The answer is the gate's id;
	// 0 means the agent has no gate suite and nothing ran (passed).
	StartGate(ctx context.Context, tenantProjectID, applicationID, candidateVersionID, currentReleaseID int64) (gateID int64, err error)
	// GateResult reads the gate's state: pending, passed or failed. A
	// failed gate names its failures (suite, dimension or case names; no
	// model output).
	GateResult(ctx context.Context, gateID int64) (GateResult, error)
	// Override records that a governance role holder released a failed
	// gate, with the reason. The change request records the same reason
	// and the agent.change.gate_overridden audit event.
	Override(ctx context.Context, gateID, actorID int64, reason string) error
}

// GateState is the state of a release gate.
type GateState string

const (
	GatePending GateState = "pending"
	GatePassed  GateState = "passed"
	GateFailed  GateState = "failed"
)

// GateResult is the answer of ReleaseGate.GateResult.
type GateResult struct {
	State    GateState
	Failures []string
}

// NoGate is the ReleaseGate of a deployment without evaluation gates: no
// gate starts, so every change request's gate has passed.
type NoGate struct{}

// StartGate starts nothing.
func (NoGate) StartGate(context.Context, int64, int64, int64, int64) (int64, error) { return 0, nil }

// GateResult answers passed.
func (NoGate) GateResult(context.Context, int64) (GateResult, error) {
	return GateResult{State: GatePassed}, nil
}

// Override has nothing to release.
func (NoGate) Override(context.Context, int64, int64, string) error { return nil }

var _ ReleaseGate = NoGate{}
