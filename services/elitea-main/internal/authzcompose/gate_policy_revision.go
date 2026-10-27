package authzcompose

import (
	"context"
	"errors"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

// PolicyRevisionGate is check 8: the execution was admitted governed, and
// the request's policy revision id and digest equal the execution's pin.
type PolicyRevisionGate struct{ pins profile.PinReader }

// NewPolicyRevisionGate builds the gate.
func NewPolicyRevisionGate(pins profile.PinReader) (*PolicyRevisionGate, error) {
	if pins == nil {
		return nil, errors.New("authzcompose: policy revision gate needs a pin reader")
	}
	return &PolicyRevisionGate{pins: pins}, nil
}

// Name implements Gate.
func (*PolicyRevisionGate) Name() string { return "policy_revision" }

// Evaluate implements Gate. A legacy or missing pin is a deny.
func (g *PolicyRevisionGate) Evaluate(ctx context.Context, r OperationRequest) (GateResult, error) {
	if r.Context.PolicyRevisionID == "" {
		return GateResult{Reasons: []ReasonCode{ReasonPolicyRevisionMissing}}, nil
	}
	if r.Context.ExecutionID == "" {
		return GateResult{Reasons: []ReasonCode{ReasonPolicyRevisionNotPin}}, nil
	}
	pin, err := g.pins.ReadPin(ctx, r.Context.ExecutionID)
	if errors.Is(err, profile.ErrPinNotFound) {
		return GateResult{Reasons: []ReasonCode{ReasonPolicyRevisionNotPin}}, nil
	}
	if err != nil {
		return GateResult{}, err
	}
	if pin.Profile != profile.ProfileGoverned || pin.PolicyRevisionID != r.Context.PolicyRevisionID ||
		pin.PolicyDigest != r.Context.PolicyDigest {
		return GateResult{Reasons: []ReasonCode{ReasonPolicyRevisionNotPin}}, nil
	}
	return GateResult{Permit: true}, nil
}
