package agentidentity

import (
	"context"
)

// BindingReader is the lookup later issues (#1115, #1116, #1117) read
// bindings through. The repository implements it.
//
// ActiveForDefinition answers only a binding whose stored state is active.
// When #1151 adds the external identity table, a binding whose external
// identity is suspended, retired or replaced must answer as suspended here
// (issue 1112 addendum, rule 3); until then no binding can become active
// without ExternalIdentityStates answering active, which the composition root
// wires fail-closed.
type BindingReader interface {
	ActiveForDefinition(ctx context.Context, tenantProjectID, versionID int64, digest [32]byte) (AgentIdentityBinding, error)
	Get(ctx context.Context, id string) (AgentIdentityBinding, error)
}

// ExternalIdentityState is the state of an external agent identity (#1151).
type ExternalIdentityState string

const (
	ExternalIdentityActive    ExternalIdentityState = "active"
	ExternalIdentitySuspended ExternalIdentityState = "suspended"
	ExternalIdentityRetired   ExternalIdentityState = "retired"
	ExternalIdentityReplaced  ExternalIdentityState = "replaced"
)

// ExternalIdentityStates answers the state of the external agent identity a
// binding references. Activation requires ExternalIdentityActive; any error
// or other state refuses it.
//
// The table behind it arrives in #1151. Until then the composition root wires
// FailClosedExternalIdentities, so no binding becomes active.
type ExternalIdentityStates interface {
	ExternalIdentityState(ctx context.Context, tenantProjectID int64, externalAgentIdentityID string) (ExternalIdentityState, error)
}

// FailClosedExternalIdentities answers "not active" for every id. It is the
// wiring until #1151 adds the external identity record.
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
