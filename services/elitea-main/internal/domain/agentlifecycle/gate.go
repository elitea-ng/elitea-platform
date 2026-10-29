package agentlifecycle

import (
	"context"
	"time"
)

// ReleaseGate is the evaluation gate of ADR-0040 decision 4, as the change
// request flow sees it. The flow never runs an evaluation: it starts the
// gate when a change request is submitted, reads its result before every
// approval, and records a governance override.
//
// The default is NoGate ("no gate suites": every gate passes at once). The
// composition root wires releasegate.LifecycleGate (RouterConfig.ReleaseGate).
type ReleaseGate interface {
	// StartGate starts the gate for a candidate. candidateVersionID is the
	// draft version a release change request would publish, or the
	// version of the release a recertification certifies again.
	// currentReleaseID is the agent's published release (0 when none):
	// the regression suite runs against it. The answer is the gate's id;
	// 0 means nothing was started (passed). The runs' model calls are
	// made for the user of ctx.
	StartGate(ctx context.Context, tenantProjectID, applicationID, candidateVersionID, currentReleaseID int64) (gateID int64, err error)
	// GateResult reads the gate's state: pending, passed or failed. A
	// failed gate names its failures (codes and short messages; no model
	// output).
	GateResult(ctx context.Context, tenantProjectID, gateID int64) (GateResult, error)
	// Override records that a governance role holder accepted a failed
	// gate, with the reason (1 to MaxOverrideReason characters). The caller
	// checks the role and writes agent.change.gate_overridden.
	Override(ctx context.Context, tenantProjectID, gateID, actorID int64, reason string) error
	// RegressedSince answers the id of a scheduled re-evaluation of the
	// agent's published release, created at or after since, that failed
	// and was not overridden; 0 when there is none.
	RegressedSince(ctx context.Context, tenantProjectID, applicationID int64, since time.Time) (int64, error)
}

// MaxOverrideReason bounds the reason of a gate override (the gate's own
// limit).
const MaxOverrideReason = 200

// GateState is the state of a release gate.
type GateState string

const (
	GatePending GateState = "pending"
	GatePassed  GateState = "passed"
	GateFailed  GateState = "failed"
)

// GateResult is the answer of ReleaseGate.GateResult.
type GateResult struct {
	State GateState
	// Failures are the failure codes of a failed gate.
	Failures []string
	// Overridden is a failed gate a governance role holder accepted.
	Overridden bool
}

// NoGate is the ReleaseGate of a deployment without evaluation gates: no
// gate starts, so every change request's gate has passed.
type NoGate struct{}

// StartGate starts nothing.
func (NoGate) StartGate(context.Context, int64, int64, int64, int64) (int64, error) { return 0, nil }

// GateResult answers passed.
func (NoGate) GateResult(context.Context, int64, int64) (GateResult, error) {
	return GateResult{State: GatePassed}, nil
}

// Override has nothing to release.
func (NoGate) Override(context.Context, int64, int64, int64, string) error { return nil }

// RegressedSince knows of no scheduled evaluation.
func (NoGate) RegressedSince(context.Context, int64, int64, time.Time) (int64, error) { return 0, nil }

var _ ReleaseGate = NoGate{}
