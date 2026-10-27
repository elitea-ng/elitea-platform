package agentidentity

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// BindingState is where an AgentIdentityBinding is in its life cycle.
// `revoked` is terminal.
type BindingState string

const (
	// BindingProposed is a binding written by an operator and not yet
	// approved. It authorises nothing.
	BindingProposed BindingState = "proposed"
	// BindingActive is an approved binding: decision 7 check 3 passes for it.
	BindingActive BindingState = "active"
	// BindingSuspended is a binding stopped without being withdrawn. A new
	// approval reactivates it.
	BindingSuspended BindingState = "suspended"
	// BindingRevoked is withdrawn for good. No action leaves it.
	BindingRevoked BindingState = "revoked"
)

// Valid reports whether s is a modelled state.
func (s BindingState) Valid() bool {
	switch s {
	case BindingProposed, BindingActive, BindingSuspended, BindingRevoked:
		return true
	}
	return false
}

// Action is a state change an operator asks for.
type Action string

const (
	// ActionApprove moves proposed → active, and suspended → active. Both are
	// an approval decision: the approver must not be the proposer, and the
	// mode and the external identity are checked again.
	ActionApprove Action = "approve"
	// ActionSuspend moves active → suspended.
	ActionSuspend Action = "suspend"
	// ActionRevoke moves any non-revoked state → revoked.
	ActionRevoke Action = "revoke"
)

// Actions is every modelled action, in a stable order.
func Actions() []Action { return []Action{ActionApprove, ActionSuspend, ActionRevoke} }

// BindingStates is every modelled state, in a stable order.
func BindingStates() []BindingState {
	return []BindingState{BindingProposed, BindingActive, BindingSuspended, BindingRevoked}
}

// Typed errors. Each message is safe to return to a caller: it names the
// rule, never a stored value.
var (
	// ErrInvalidTransition refuses an action the current state does not allow,
	// including a second approval that lost a race.
	ErrInvalidTransition = errors.New("agent identity binding: transition not allowed from the current state")
	// ErrSelfApproval refuses an approval by the binding's proposer.
	ErrSelfApproval = errors.New("agent identity binding: the approver must not be the proposer")
	// ErrIdentityModeNotAllowed refuses a mode the active policy revision does
	// not list (gate D2: the default revision lists none).
	ErrIdentityModeNotAllowed = errors.New("agent identity binding: identity mode is not allowed by the active policy revision")
	// ErrExternalIdentityNotActive refuses an activation whose external agent
	// identity is absent or not active (issue 1112 addendum, rule 3).
	ErrExternalIdentityNotActive = errors.New("agent identity binding: the external agent identity is not active")
	// ErrInvalidBinding refuses a malformed proposal.
	ErrInvalidBinding = errors.New("agent identity binding: invalid binding")
	// ErrBindingNotFound is an id with no row (in the caller's scope).
	ErrBindingNotFound = errors.New("agent identity binding: not found")
	// ErrDuplicateBinding refuses a second non-revoked binding for one
	// tenant project, application version and definition digest.
	ErrDuplicateBinding = errors.New("agent identity binding: a non-revoked binding already exists for this version and digest")
)

// maxWorkloadIdentityBytes bounds the SPIFFE id pattern.
const maxWorkloadIdentityBytes = 512

// AgentIdentityBinding ties one agent version (application version + frozen
// definition digest) to an external agent identity (ADR-0039 decision 1).
//
// It holds references only. ExternalAgentIdentityID names a record #1151
// adds; the provider, provider tenant and immutable external id live there,
// not here, so versions of one agent re-bind to one external identity.
//
// No field holds a token, a secret, an assertion or a key
// (binding_fields_test.go pins the field set).
type AgentIdentityBinding struct {
	ID                   string // uuid v7
	TenantProjectID      int64
	ApplicationID        int64
	ApplicationVersionID int64
	DefinitionDigest     [32]byte // frozen definition digest
	Mode                 IdentityMode
	// ExternalAgentIdentityID references elitea_identity.external_agent_identities
	// (#1151). Empty means none yet; activation then fails.
	ExternalAgentIdentityID string
	// WorkloadIdentity is the SPIFFE id pattern of the runtime domain. Required
	// for runtime_domain_binding, empty otherwise.
	WorkloadIdentity string
	State            BindingState
	// RevocationEpoch increases by exactly one on every transition. A later
	// check that read an older epoch is stale.
	RevocationEpoch int64
	// PolicyRevisionID is the revision the last decision (proposal or
	// approval) was taken under.
	PolicyRevisionID string
	ProposedBy       string // user id; never equal to ApprovedBy
	ApprovedBy       string // empty until the first approval
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Proposal is what an operator supplies to propose a binding.
type Proposal struct {
	TenantProjectID         int64
	ApplicationID           int64
	ApplicationVersionID    int64
	DefinitionDigest        [32]byte
	Mode                    IdentityMode
	ExternalAgentIdentityID string
	WorkloadIdentity        string
}

// Validate checks the shape of a proposal. The policy check is separate
// (ModePolicy), because the domain does not read the policy revision.
func (p Proposal) Validate() error {
	switch {
	case p.TenantProjectID <= 0:
		return fmt.Errorf("%w: tenant_project_id must be positive", ErrInvalidBinding)
	case p.ApplicationID <= 0:
		return fmt.Errorf("%w: application_id must be positive", ErrInvalidBinding)
	case p.ApplicationVersionID <= 0:
		return fmt.Errorf("%w: application_version_id must be positive", ErrInvalidBinding)
	case p.DefinitionDigest == [32]byte{}:
		return fmt.Errorf("%w: definition_digest must be a sha256 digest", ErrInvalidBinding)
	case !p.Mode.Valid():
		return fmt.Errorf("%w: unknown identity mode", ErrInvalidBinding)
	}
	if p.ExternalAgentIdentityID != "" {
		if _, err := uuid.Parse(p.ExternalAgentIdentityID); err != nil {
			return fmt.Errorf("%w: external_agent_identity_id must be a uuid", ErrInvalidBinding)
		}
	}
	if p.Mode == IdentityModeRuntimeDomainBinding {
		if !strings.HasPrefix(p.WorkloadIdentity, "spiffe://") || len(p.WorkloadIdentity) > maxWorkloadIdentityBytes ||
			strings.ContainsAny(p.WorkloadIdentity, " \t\r\n") {
			return fmt.Errorf("%w: workload_identity must be a spiffe:// id pattern of at most %d bytes", ErrInvalidBinding, maxWorkloadIdentityBytes)
		}
	} else if p.WorkloadIdentity != "" {
		return fmt.Errorf("%w: workload_identity is only for runtime_domain_binding", ErrInvalidBinding)
	}
	return nil
}

// ModePolicy is the part of the active policy revision a binding decision
// reads. profile.PolicyDocument satisfies it.
type ModePolicy interface {
	AllowsIdentityMode(mode IdentityMode) bool
}

// NewBinding builds a proposed binding from a validated proposal. The mode
// must be allowed by policy. Epoch starts at 0.
func NewBinding(id string, p Proposal, policy ModePolicy, policyRevisionID, proposedBy string, now time.Time) (AgentIdentityBinding, error) {
	if err := p.Validate(); err != nil {
		return AgentIdentityBinding{}, err
	}
	if policy == nil || !policy.AllowsIdentityMode(p.Mode) {
		return AgentIdentityBinding{}, ErrIdentityModeNotAllowed
	}
	if strings.TrimSpace(proposedBy) == "" {
		return AgentIdentityBinding{}, fmt.Errorf("%w: the proposer is unknown", ErrInvalidBinding)
	}
	if _, err := uuid.Parse(id); err != nil {
		return AgentIdentityBinding{}, fmt.Errorf("%w: id must be a uuid", ErrInvalidBinding)
	}
	return AgentIdentityBinding{
		ID:                      id,
		TenantProjectID:         p.TenantProjectID,
		ApplicationID:           p.ApplicationID,
		ApplicationVersionID:    p.ApplicationVersionID,
		DefinitionDigest:        p.DefinitionDigest,
		Mode:                    p.Mode,
		ExternalAgentIdentityID: p.ExternalAgentIdentityID,
		WorkloadIdentity:        p.WorkloadIdentity,
		State:                   BindingProposed,
		RevocationEpoch:         0,
		PolicyRevisionID:        policyRevisionID,
		ProposedBy:              proposedBy,
		CreatedAt:               now,
		UpdatedAt:               now,
	}, nil
}

// NextState answers the state an action leads to from state, or
// ErrInvalidTransition. It is the whole transition table:
//
//	proposed  --approve--> active
//	active    --suspend--> suspended
//	suspended --approve--> active
//	proposed|active|suspended --revoke--> revoked
//
// Everything else, and every action from revoked, is refused.
func NextState(state BindingState, action Action) (BindingState, error) {
	switch {
	case action == ActionRevoke && state != BindingRevoked && state.Valid():
		return BindingRevoked, nil
	case action == ActionApprove && (state == BindingProposed || state == BindingSuspended):
		return BindingActive, nil
	case action == ActionSuspend && state == BindingActive:
		return BindingSuspended, nil
	}
	return "", ErrInvalidTransition
}

// Transition applies action by actor at now and returns the next binding.
// The epoch increases by exactly one. An approval records the approver,
// refuses the proposer and records the revision it was decided under.
//
// It checks only what the binding itself can answer. The caller checks the
// policy mode and the external identity before an approval
// (governance.Service does both).
func (b AgentIdentityBinding) Transition(action Action, actor, policyRevisionID string, now time.Time) (AgentIdentityBinding, error) {
	next, err := NextState(b.State, action)
	if err != nil {
		return AgentIdentityBinding{}, err
	}
	if strings.TrimSpace(actor) == "" {
		return AgentIdentityBinding{}, fmt.Errorf("%w: the actor is unknown", ErrInvalidBinding)
	}
	out := b
	if action == ActionApprove {
		if actor == b.ProposedBy {
			return AgentIdentityBinding{}, ErrSelfApproval
		}
		out.ApprovedBy = actor
		out.PolicyRevisionID = policyRevisionID
	}
	out.State = next
	out.RevocationEpoch = b.RevocationEpoch + 1
	out.UpdatedAt = now
	return out, nil
}
