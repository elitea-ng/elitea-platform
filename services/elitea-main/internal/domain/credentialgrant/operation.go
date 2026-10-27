package credentialgrant

import (
	"context"
	"regexp"
)

// OperationID is a canonical operation id, `<provider>.<resource>.<verb>`,
// for example `jira.issue.read`.
type OperationID string

var operationIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\.[a-z0-9][a-z0-9_-]{0,63}\.[a-z0-9][a-z0-9_-]{0,63}$`)

// WellFormed reports whether id has the canonical shape. It does not say the
// operation exists: OperationRegistry.Known does.
func (id OperationID) WellFormed() bool { return operationIDPattern.MatchString(string(id)) }

// OperationRegistry answers which canonical operations exist. It is built
// at composition from the provider adapter descriptors (#1120). An unknown
// id is a deny.
type OperationRegistry interface {
	Known(id OperationID) bool
	// Class is the operation class (for example "read" or "write"); empty
	// for an unknown id.
	Class(id OperationID) string
}

// StaticRegistry is an OperationRegistry over a fixed id → class map.
type StaticRegistry map[OperationID]string

// Known reports whether id is listed.
func (r StaticRegistry) Known(id OperationID) bool {
	_, ok := r[id]
	return ok
}

// Class returns the class of id.
func (r StaticRegistry) Class(id OperationID) string { return r[id] }

// EmptyRegistry knows no operation, so every governed operation is denied.
// It is the wiring until the adapter descriptors exist (#1120).
var EmptyRegistry = StaticRegistry{}

// GrantReader reads the one non-revoked grant for an exact key. No row is
// ErrGrantNotFound.
type GrantReader interface {
	GrantForKey(ctx context.Context, key GrantKey) (CredentialUseGrant, error)
}

// ReferenceReader reads a credential reference. No row (in the tenant
// project) is ErrReferenceNotFound.
type ReferenceReader interface {
	CredentialReference(ctx context.Context, tenantProjectID int64, id string) (CredentialReference, error)
}

// ToolkitBindingReader reads the toolkit an agent binding may run. No row is
// ErrToolkitNotBound.
// TargetReader reads one target binding of a tenant project. No row is
// ErrTargetNotFound. The credential broker reads the target a released
// credential is valid for (#1118).
type TargetReader interface {
	TargetBinding(ctx context.Context, tenantProjectID int64, id string) (TargetBinding, error)
}

type ToolkitBindingReader interface {
	AgentToolkitBinding(ctx context.Context, agentBindingID string, toolkitID int64) (AgentToolkitBinding, error)
}
