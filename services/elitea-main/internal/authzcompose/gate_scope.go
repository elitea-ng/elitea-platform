package authzcompose

import (
	"context"
	"errors"
	"slices"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

// ScopeGate is check 5: the operation is a known canonical operation and is
// listed by the grant's operation scopes and, for a mandate, by the
// mandate's; the resource ref is listed by every resource scope list that
// is not empty. Matching is exact: a scope is never a prefix.
//
// A delegation's scopes are provider OAuth scopes, not operation ids, so a
// delegation narrows nothing here; the broker checks them at release (#1118).
type ScopeGate struct {
	registry credentialgrant.OperationRegistry
	grants   credentialgrant.GrantReader
	mandates mandate.Reader
}

// NewScopeGate builds the gate.
func NewScopeGate(registry credentialgrant.OperationRegistry, grants credentialgrant.GrantReader, mandates mandate.Reader) (*ScopeGate, error) {
	if registry == nil || grants == nil || mandates == nil {
		return nil, errors.New("authzcompose: scope gate needs an operation registry, a grant reader and a mandate reader")
	}
	return &ScopeGate{registry: registry, grants: grants, mandates: mandates}, nil
}

// Name implements Gate.
func (*ScopeGate) Name() string { return "scope" }

func withinResources(scopes []string, ref string) bool {
	return len(scopes) == 0 || (ref != "" && slices.Contains(scopes, ref))
}

// Evaluate implements Gate.
func (g *ScopeGate) Evaluate(ctx context.Context, r OperationRequest) (GateResult, error) {
	op := r.Action.OperationID
	if !op.WellFormed() || !g.registry.Known(op) {
		return GateResult{Reasons: []ReasonCode{ReasonOperationUnknown}}, nil
	}
	grant, err := snapshotGrant(ctx, g.grants, grantKey(r))
	if errors.Is(err, credentialgrant.ErrGrantNotFound) {
		return GateResult{Reasons: []ReasonCode{ReasonGrantMissing}}, nil
	}
	if err != nil {
		return GateResult{}, err
	}
	var reasons []ReasonCode
	if !slices.Contains(grant.OperationScopes, string(op)) {
		reasons = append(reasons, ReasonOperationOutOfScope)
	}
	if !withinResources(grant.ResourceScopes, r.Resource.ResourceRef) {
		reasons = append(reasons, ReasonResourceOutOfScope)
	}
	switch r.Authority.Kind {
	case AuthorityMandate:
		m, err := snapshotMandate(ctx, g.mandates, r.Authority.ID)
		if errors.Is(err, mandate.ErrMandateNotFound) {
			reasons = append(reasons, ReasonAuthorityNotActive)
			break
		}
		if err != nil {
			return GateResult{}, err
		}
		if !slices.Contains(m.OperationScopes, string(op)) {
			reasons = append(reasons, ReasonOperationOutOfScope)
		}
		if !withinResources(m.ResourceScopes, r.Resource.ResourceRef) {
			reasons = append(reasons, ReasonResourceOutOfScope)
		}
	case AuthorityDelegation:
	default:
		reasons = append(reasons, ReasonAuthorityUnknown)
	}
	return GateResult{Permit: len(reasons) == 0, Reasons: reasons}, nil
}
