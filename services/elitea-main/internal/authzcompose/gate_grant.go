package authzcompose

import (
	"context"
	"errors"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// GrantGate is check 4: an exact CredentialUseGrant for agent × toolkit ×
// credential reference × target exists, is active and unexpired, and lists
// the requested capability; and the credential reference is usable and
// bound to that same target. Its obligations become the gate's.
type GrantGate struct {
	grants     credentialgrant.GrantReader
	references credentialgrant.ReferenceReader
	now        func() time.Time
}

// NewGrantGate builds the gate. now may be nil (wall clock).
func NewGrantGate(grants credentialgrant.GrantReader, references credentialgrant.ReferenceReader, now func() time.Time) (*GrantGate, error) {
	if grants == nil || references == nil {
		return nil, errors.New("authzcompose: grant gate needs grant and credential reference readers")
	}
	if now == nil {
		now = time.Now
	}
	return &GrantGate{grants: grants, references: references, now: now}, nil
}

// Name implements Gate.
func (*GrantGate) Name() string { return "grant" }

func grantKey(r OperationRequest) credentialgrant.GrantKey {
	return credentialgrant.GrantKey{
		TenantProjectID:       r.Context.TenantProjectID,
		AgentBindingID:        r.Agent.BindingID,
		ToolkitID:             r.Resource.ToolkitID,
		CredentialReferenceID: r.Resource.CredentialReferenceID,
		TargetBindingID:       r.Resource.TargetBindingID,
	}
}

// Evaluate implements Gate.
func (g *GrantGate) Evaluate(ctx context.Context, r OperationRequest) (GateResult, error) {
	grant, err := snapshotGrant(ctx, g.grants, grantKey(r))
	if errors.Is(err, credentialgrant.ErrGrantNotFound) {
		return GateResult{Reasons: []ReasonCode{ReasonGrantMissing}}, nil
	}
	if err != nil {
		return GateResult{}, err
	}
	result := GateResult{Epochs: map[string]int64{"grant": grant.Epoch}, Refs: map[string]string{"grant": grant.ID}}
	// The reader matched the key; a row that does not repeat it is refused
	// rather than trusted.
	if grant.TenantProjectID != r.Context.TenantProjectID || grant.AgentBindingID != r.Agent.BindingID ||
		grant.ToolkitID != r.Resource.ToolkitID || grant.CredentialReferenceID != r.Resource.CredentialReferenceID ||
		grant.TargetBindingID != r.Resource.TargetBindingID {
		return GateResult{Reasons: []ReasonCode{ReasonGrantMissing}}, nil
	}
	if !grant.ActiveAt(g.now()) {
		result.Reasons = append(result.Reasons, ReasonGrantNotActive)
	}
	if !grant.Allows(r.Action.Capability) {
		result.Reasons = append(result.Reasons, ReasonCapabilityNotGranted)
	}
	reference, err := g.references.CredentialReference(ctx, r.Context.TenantProjectID, r.Resource.CredentialReferenceID)
	switch {
	case errors.Is(err, credentialgrant.ErrReferenceNotFound):
		result.Reasons = append(result.Reasons, ReasonCredentialNotUsable)
	case err != nil:
		return GateResult{}, err
	default:
		result.Epochs["credential_reference"] = reference.Generation
		if !reference.State.Usable() || reference.TenantProjectID != r.Context.TenantProjectID {
			result.Reasons = append(result.Reasons, ReasonCredentialNotUsable)
		}
		if reference.TargetBindingID != r.Resource.TargetBindingID {
			result.Reasons = append(result.Reasons, ReasonCredentialTargetDiffer)
		}
	}
	if len(result.Reasons) > 0 {
		return result, nil
	}
	result.Permit = true
	for _, kind := range grant.Obligations {
		result.Obligations = append(result.Obligations, Obligation{Kind: kind})
	}
	return result, nil
}
