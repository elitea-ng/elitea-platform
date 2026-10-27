package authzcompose

import (
	"context"
	"errors"
)

// EntityAccess answers check 2b: the user is a member of the tenant project
// and the agent (application) is not hidden from them by folder visibility.
type EntityAccess interface {
	AgentAccessible(ctx context.Context, userID, tenantProjectID, applicationID int64) (bool, error)
}

// EntityAccessGate is check 2b. Project membership alone never creates a
// provider right (gate D4): the other gates still apply.
type EntityAccessGate struct{ access EntityAccess }

// NewEntityAccessGate builds the gate.
func NewEntityAccessGate(access EntityAccess) (*EntityAccessGate, error) {
	if access == nil {
		return nil, errors.New("authzcompose: entity access gate needs an entity access reader")
	}
	return &EntityAccessGate{access: access}, nil
}

// Name implements Gate.
func (*EntityAccessGate) Name() string { return "entity_access" }

// Evaluate implements Gate.
func (g *EntityAccessGate) Evaluate(ctx context.Context, r OperationRequest) (GateResult, error) {
	if r.Subject.UserID <= 0 {
		return GateResult{Reasons: []ReasonCode{ReasonSubjectMissing}}, nil
	}
	if r.Agent.ApplicationID <= 0 || r.Context.TenantProjectID <= 0 {
		return GateResult{Reasons: []ReasonCode{ReasonAgentMissing}}, nil
	}
	ok, err := g.access.AgentAccessible(ctx, r.Subject.UserID, r.Context.TenantProjectID, r.Agent.ApplicationID)
	if err != nil {
		return GateResult{}, err
	}
	if !ok {
		return GateResult{Reasons: []ReasonCode{ReasonEntityAccessDenied}}, nil
	}
	return GateResult{Permit: true}, nil
}
