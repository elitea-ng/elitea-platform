package authzcompose

import (
	"fmt"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

// Dependencies are the readers the eight gates of decision 7 read. Every
// field except Now is required.
type Dependencies struct {
	AuthSources  AuthSourcePolicy
	Permissions  auth.PermissionResolver
	EntityAccess EntityAccess
	Bindings     BindingGetter
	Toolkits     credentialgrant.ToolkitBindingReader
	Grants       credentialgrant.GrantReader
	References   credentialgrant.ReferenceReader
	Operations   credentialgrant.OperationRegistry
	Mandates     mandate.Reader
	Delegations  DelegationReader
	Pins         profile.PinReader
	Now          func() time.Time
}

// NewGovernedDecider builds the decider of decision 7 with its eight gates,
// in the order the ADR lists the checks. Any missing dependency is an error,
// which refuses a governed boot.
func NewGovernedDecider(deps Dependencies) (*Decider, error) {
	authenticated, err := NewAuthenticatedContextGate(deps.AuthSources)
	if err != nil {
		return nil, err
	}
	rbac, err := NewRBACGate(deps.Permissions)
	if err != nil {
		return nil, err
	}
	entity, err := NewEntityAccessGate(deps.EntityAccess)
	if err != nil {
		return nil, err
	}
	binding, err := NewAgentBindingGate(deps.Bindings, deps.Toolkits)
	if err != nil {
		return nil, err
	}
	grant, err := NewGrantGate(deps.Grants, deps.References, deps.Now)
	if err != nil {
		return nil, err
	}
	scope, err := NewScopeGate(deps.Operations, deps.Grants, deps.Mandates)
	if err != nil {
		return nil, err
	}
	authority, err := NewAuthorityGate(deps.Mandates, deps.Delegations, deps.Now)
	if err != nil {
		return nil, err
	}
	revision, err := NewPolicyRevisionGate(deps.Pins)
	if err != nil {
		return nil, err
	}
	decider, err := NewDecider(authenticated, rbac, entity, binding, grant, scope, authority, revision)
	if err != nil {
		return nil, fmt.Errorf("build the governed decider: %w", err)
	}
	return decider, nil
}
