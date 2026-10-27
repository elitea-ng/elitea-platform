package agentidentity

import (
	"context"
)

// BindingReader is the lookup later issues (#1115, #1116, #1117) read
// bindings through. The repository implements it.
//
// ActiveForDefinition answers only a binding whose stored state is active.
// A binding whose external identity is not active (provisioning,
// pending_activation, suspended, retiring, retired, replaced) answers with
// State BindingSuspended (issue 1112 addendum, rule 3; UsableBinding).
type BindingReader interface {
	ActiveForDefinition(ctx context.Context, tenantProjectID, versionID int64, digest [32]byte) (AgentIdentityBinding, error)
	Get(ctx context.Context, id string) (AgentIdentityBinding, error)
}

// ExternalIdentityState is the state of an external agent identity (#1151).
// external.go declares the rest of the states and the transition table.
type ExternalIdentityState string

const (
	ExternalIdentityActive    ExternalIdentityState = "active"
	ExternalIdentitySuspended ExternalIdentityState = "suspended"
	ExternalIdentityRetired   ExternalIdentityState = "retired"
	ExternalIdentityReplaced  ExternalIdentityState = "replaced"
)

// ExternalIdentityStates answers the state of the external agent identity a
// binding references. Binding approval requires ExternalIdentityActive or
// ExternalIdentityPendingActivation; any error or other state refuses it.
// repos.ExternalAgentIdentities implements it.
type ExternalIdentityStates interface {
	ExternalIdentityState(ctx context.Context, tenantProjectID int64, externalAgentIdentityID string) (ExternalIdentityState, error)
}

// FailClosedExternalIdentities answers "not active" for every id. Tests
// use it to prove nothing activates without the external identity record.
type FailClosedExternalIdentities struct{}

// ExternalIdentityState always refuses.
func (FailClosedExternalIdentities) ExternalIdentityState(context.Context, int64, string) (ExternalIdentityState, error) {
	return "", ErrExternalIdentityNotActive
}

// ListFilter narrows a binding listing. Zero fields do not filter.
type ListFilter struct {
	TenantProjectID int64
	State           BindingState
	Limit           int
	Offset          int
}
