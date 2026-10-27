package authzcompose

import (
	"context"
	"errors"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// BindingGetter reads a binding as usable for a governed decision: a binding
// whose external agent identity is not active reads as suspended
// (repos.AgentIdentityBindings.GetUsable).
type BindingGetter interface {
	GetUsable(ctx context.Context, id string) (agentidentity.AgentIdentityBinding, error)
}

// AgentBindingGate is check 3: the AgentIdentityBinding is active, in the
// tenant project, for the requested application version and definition
// digest, at the epoch the run was admitted with; and the toolkit is bound
// to it at the requested toolkit digest.
type AgentBindingGate struct {
	bindings BindingGetter
	toolkits credentialgrant.ToolkitBindingReader
}

// NewAgentBindingGate builds the gate.
func NewAgentBindingGate(bindings BindingGetter, toolkits credentialgrant.ToolkitBindingReader) (*AgentBindingGate, error) {
	if bindings == nil || toolkits == nil {
		return nil, errors.New("authzcompose: agent binding gate needs binding and toolkit binding readers")
	}
	return &AgentBindingGate{bindings: bindings, toolkits: toolkits}, nil
}

// Name implements Gate.
func (*AgentBindingGate) Name() string { return "agent_binding" }

// Evaluate implements Gate. A binding or toolkit binding with no row is a
// deny; any other read error is a gate error.
func (g *AgentBindingGate) Evaluate(ctx context.Context, r OperationRequest) (GateResult, error) {
	if r.Agent.BindingID == "" {
		return GateResult{Reasons: []ReasonCode{ReasonAgentMissing}}, nil
	}
	binding, err := g.bindings.GetUsable(ctx, r.Agent.BindingID)
	if errors.Is(err, agentidentity.ErrBindingNotFound) {
		return GateResult{Reasons: []ReasonCode{ReasonBindingNotActive}}, nil
	}
	if err != nil {
		return GateResult{}, err
	}
	result := GateResult{Epochs: map[string]int64{"agent_binding": binding.RevocationEpoch}}
	if binding.State != agentidentity.BindingActive {
		result.Reasons = append(result.Reasons, ReasonBindingNotActive)
	}
	if binding.RevocationEpoch != r.Agent.Epoch {
		result.Reasons = append(result.Reasons, ReasonBindingEpochStale)
	}
	if binding.TenantProjectID != r.Context.TenantProjectID || binding.ApplicationID != r.Agent.ApplicationID ||
		binding.ApplicationVersionID != r.Agent.ApplicationVersionID {
		result.Reasons = append(result.Reasons, ReasonBindingScopeMismatch)
	}
	if binding.DefinitionDigest != r.Agent.DefinitionDigest {
		result.Reasons = append(result.Reasons, ReasonBindingDigestMismatch)
	}
	toolkit, err := g.toolkits.AgentToolkitBinding(ctx, r.Agent.BindingID, r.Resource.ToolkitID)
	switch {
	case errors.Is(err, credentialgrant.ErrToolkitNotBound):
		result.Reasons = append(result.Reasons, ReasonToolkitNotBound)
	case err != nil:
		return GateResult{}, err
	case toolkit.TenantProjectID != r.Context.TenantProjectID:
		result.Reasons = append(result.Reasons, ReasonToolkitNotBound)
	case toolkit.ToolkitDigest != r.Resource.ToolkitDigest:
		result.Reasons = append(result.Reasons, ReasonToolkitDigestMismatch)
	}
	result.Permit = len(result.Reasons) == 0
	return result, nil
}
