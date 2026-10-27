// Package mandate holds the stored, revocable authority records of ADR-0039
// decision 3: the Mandate behind an unattended run and the Delegation
// metadata behind interactive delegation. A mandate is not a principal.
//
// Neither type holds token, assertion, secret or key material: token custody
// is #1117, and mandate_fields_test.go pins the field sets.
package mandate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Kind is the authority a mandate grants (ADR-0039 decision 3).
//
// Interactive delegation is NOT a mandate kind: ADR-0039 lists it as its own
// authority source, held as a Delegation (#1117). Issue 1112's
// `interactive_delegation` mandate kind is dropped for that reason; the ADR
// wins.
type Kind string

const (
	// KindAutonomous: the agent acts as itself, with no user.
	KindAutonomous Kind = "autonomous"
	// KindDelegatedBackground: an unattended run acts for a named user within
	// the mandate's bounds. The user evidence is `asserted` unless a
	// Delegation supplies verified evidence.
	KindDelegatedBackground Kind = "delegated_background"
)

// Valid reports whether k is a modelled kind.
func (k Kind) Valid() bool { return k == KindAutonomous || k == KindDelegatedBackground }

// State is a mandate's life-cycle state. `revoked` is terminal; `expired` is
// derived from ExpiresAt (EffectiveState) and is terminal for every action
// except revoke.
type State string

const (
	StateProposed  State = "proposed"
	StateActive    State = "active"
	StateSuspended State = "suspended"
	StateRevoked   State = "revoked"
	StateExpired   State = "expired"
)

// Valid reports whether s is a modelled state.
func (s State) Valid() bool {
	switch s {
	case StateProposed, StateActive, StateSuspended, StateRevoked, StateExpired:
		return true
	}
	return false
}

// States is every modelled state, in a stable order.
func States() []State {
	return []State{StateProposed, StateActive, StateSuspended, StateRevoked, StateExpired}
}

// Action is a state change an operator asks for.
type Action string

const (
	ActionApprove Action = "approve"
	ActionSuspend Action = "suspend"
	ActionRevoke  Action = "revoke"
)

// Actions is every modelled action, in a stable order.
func Actions() []Action { return []Action{ActionApprove, ActionSuspend, ActionRevoke} }

// Typed errors with safe messages.
var (
	ErrInvalidTransition = errors.New("mandate: transition not allowed from the current state")
	ErrSelfApproval      = errors.New("mandate: the approver must not be the issuer")
	ErrInvalidMandate    = errors.New("mandate: invalid mandate")
	// ErrExpiresAtRequired refuses a mandate with no expiry.
	ErrExpiresAtRequired = errors.New("mandate: expires_at is required")
	// ErrTTLExceedsPolicy refuses an expiry beyond the active policy
	// revision's max_mandate_ttl_seconds.
	ErrTTLExceedsPolicy = errors.New("mandate: expires_at is beyond the policy maximum lifetime")
	ErrMandateNotFound  = errors.New("mandate: not found")
	// ErrBindingNotUsable refuses a mandate for a binding that is revoked, in
	// another tenant project, or (at approval) not active.
	ErrBindingNotUsable = errors.New("mandate: the agent identity binding is not usable")
	// ErrDelegationNotUsable refuses a mandate whose delegation is absent,
	// in another tenant project, for another binding, or not active.
	ErrDelegationNotUsable = errors.New("mandate: the delegation is not usable")
)

const (
	maxScopes     = 64
	maxScopeBytes = 256
)

// Mandate is a stored, revocable authority for an unattended run: who
// authorised it, for which agent binding, operations, resources and time
// bounds. Scopes only (gate D4): a missing scope is a deny.
type Mandate struct {
	ID              string
	TenantProjectID int64
	Kind            Kind
	AgentBindingID  string
	SubjectUserID   *int64  // nil for autonomous
	DelegationID    *string // optional for delegated_background (#1117); nil for autonomous
	OperationScopes []string
	ResourceScopes  []string
	NotBefore       time.Time
	ExpiresAt       time.Time // required; at most the policy maximum from issue time
	State           State     // stored state; see EffectiveState
	RevocationEpoch int64
	// PolicyRevisionID is the revision the last decision was taken under.
	PolicyRevisionID string
	IssuedBy         string
	ApprovedBy       string
	// LastTransitionBy is the user who made the latest change: the issuer at
	// creation, then the actor of every approve, suspend and revoke,
	// including one cascaded from the binding (FollowBinding).
	LastTransitionBy string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Delegation is interactive-delegation metadata (#1117 adds token custody in
// separate ciphertext columns; none exist here).
type Delegation struct {
	ID                string
	TenantProjectID   int64
	Issuer            string
	TenantID          string
	SubjectObjectID   string
	AgentBindingID    string
	Audiences         []string
	Scopes            []string
	ConsentRecordedAt time.Time
	ExpiresAt         time.Time
	State             State
	RevocationEpoch   int64
}

// Request is what an operator supplies to issue a mandate.
type Request struct {
	TenantProjectID int64
	Kind            Kind
	AgentBindingID  string
	SubjectUserID   *int64
	DelegationID    *string
	OperationScopes []string
	ResourceScopes  []string
	NotBefore       time.Time // zero means now
	ExpiresAt       time.Time
}

// TTLPolicy is the part of the active policy revision a mandate decision
// reads. profile.PolicyDocument satisfies it.
type TTLPolicy interface {
	MaxMandateTTL() time.Duration
}

func validScopes(name string, scopes []string, required bool) error {
	if required && len(scopes) == 0 {
		return fmt.Errorf("%w: %s must not be empty", ErrInvalidMandate, name)
	}
	if len(scopes) > maxScopes {
		return fmt.Errorf("%w: %s lists more than %d values", ErrInvalidMandate, name, maxScopes)
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if scope == "" || len(scope) > maxScopeBytes || strings.ContainsAny(scope, " \t\r\n") {
			return fmt.Errorf("%w: each %s value must be 1..%d bytes without whitespace", ErrInvalidMandate, name, maxScopeBytes)
		}
		if _, dup := seen[scope]; dup {
			return fmt.Errorf("%w: %s repeats a value", ErrInvalidMandate, name)
		}
		seen[scope] = struct{}{}
	}
	return nil
}

// New builds a proposed mandate. ExpiresAt is required, must follow
// NotBefore and now, and must be at most policy.MaxMandateTTL() after now.
func New(id string, req Request, policy TTLPolicy, policyRevisionID, issuedBy string, now time.Time) (Mandate, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Mandate{}, fmt.Errorf("%w: id must be a uuid", ErrInvalidMandate)
	}
	if req.TenantProjectID <= 0 {
		return Mandate{}, fmt.Errorf("%w: tenant_project_id must be positive", ErrInvalidMandate)
	}
	if !req.Kind.Valid() {
		return Mandate{}, fmt.Errorf("%w: unknown kind", ErrInvalidMandate)
	}
	if _, err := uuid.Parse(req.AgentBindingID); err != nil {
		return Mandate{}, fmt.Errorf("%w: agent_binding_id must be a uuid", ErrInvalidMandate)
	}
	switch req.Kind {
	case KindAutonomous:
		if req.SubjectUserID != nil || req.DelegationID != nil {
			return Mandate{}, fmt.Errorf("%w: an autonomous mandate names no subject user and no delegation", ErrInvalidMandate)
		}
	case KindDelegatedBackground:
		if req.SubjectUserID == nil || *req.SubjectUserID <= 0 {
			return Mandate{}, fmt.Errorf("%w: a delegated_background mandate names its subject user", ErrInvalidMandate)
		}
	}
	if req.DelegationID != nil {
		if _, err := uuid.Parse(*req.DelegationID); err != nil {
			return Mandate{}, fmt.Errorf("%w: delegation_id must be a uuid", ErrInvalidMandate)
		}
	}
	if err := validScopes("operation_scopes", req.OperationScopes, true); err != nil {
		return Mandate{}, err
	}
	if err := validScopes("resource_scopes", req.ResourceScopes, false); err != nil {
		return Mandate{}, err
	}
	if strings.TrimSpace(issuedBy) == "" {
		return Mandate{}, fmt.Errorf("%w: the issuer is unknown", ErrInvalidMandate)
	}
	if req.ExpiresAt.IsZero() {
		return Mandate{}, ErrExpiresAtRequired
	}
	notBefore := req.NotBefore
	if notBefore.IsZero() {
		notBefore = now
	}
	if !req.ExpiresAt.After(notBefore) || !req.ExpiresAt.After(now) {
		return Mandate{}, fmt.Errorf("%w: expires_at must be after not_before and in the future", ErrInvalidMandate)
	}
	if policy == nil || policy.MaxMandateTTL() <= 0 || req.ExpiresAt.Sub(now) > policy.MaxMandateTTL() {
		return Mandate{}, ErrTTLExceedsPolicy
	}
	resources := append([]string{}, req.ResourceScopes...)
	return Mandate{
		ID:               id,
		TenantProjectID:  req.TenantProjectID,
		Kind:             req.Kind,
		AgentBindingID:   req.AgentBindingID,
		SubjectUserID:    req.SubjectUserID,
		DelegationID:     req.DelegationID,
		OperationScopes:  append([]string{}, req.OperationScopes...),
		ResourceScopes:   resources,
		NotBefore:        notBefore.UTC(),
		ExpiresAt:        req.ExpiresAt.UTC(),
		State:            StateProposed,
		PolicyRevisionID: policyRevisionID,
		IssuedBy:         issuedBy,
		LastTransitionBy: issuedBy,
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

// EffectiveState is the stored state, except that a non-revoked mandate past
// ExpiresAt is expired.
func (m Mandate) EffectiveState(now time.Time) State {
	if m.State != StateRevoked && !now.Before(m.ExpiresAt) {
		return StateExpired
	}
	return m.State
}

// NextState is the whole transition table:
//
//	proposed  --approve--> active
//	active    --suspend--> suspended
//	suspended --approve--> active
//	proposed|active|suspended|expired --revoke--> revoked
//
// Everything else, and every action from revoked, is refused. Expired
// accepts only revoke, which bumps the epoch for readers that cached it.
func NextState(state State, action Action) (State, error) {
	switch {
	case action == ActionRevoke && state != StateRevoked && state.Valid():
		return StateRevoked, nil
	case action == ActionApprove && (state == StateProposed || state == StateSuspended):
		return StateActive, nil
	case action == ActionSuspend && state == StateActive:
		return StateSuspended, nil
	}
	return "", ErrInvalidTransition
}

// Transition applies action by actor at now. The epoch increases by exactly
// one. An approval refuses the issuer and records the approver and the
// revision it was decided under.
func (m Mandate) Transition(action Action, actor, policyRevisionID string, now time.Time) (Mandate, error) {
	next, err := NextState(m.EffectiveState(now), action)
	if err != nil {
		return Mandate{}, err
	}
	if strings.TrimSpace(actor) == "" {
		return Mandate{}, fmt.Errorf("%w: the actor is unknown", ErrInvalidMandate)
	}
	out := m
	if action == ActionApprove {
		if actor == m.IssuedBy {
			return Mandate{}, ErrSelfApproval
		}
		out.ApprovedBy = actor
		out.PolicyRevisionID = policyRevisionID
	}
	out.State = next
	out.LastTransitionBy = actor
	out.RevocationEpoch = m.RevocationEpoch + 1
	out.UpdatedAt = now
	return out, nil
}

// FollowBinding answers what happens to m when its binding is revoked
// (action ActionRevoke) or suspended (ActionSuspend), in the same
// transaction as the binding change:
//
//   - revoke: every non-revoked mandate is revoked.
//   - suspend: a mandate whose effective state is active is suspended.
//     Proposed, suspended and expired mandates stay as they are; approval
//     requires an active binding, so none of them can become active while
//     the binding is suspended.
//
// Reactivating the binding does NOT reactivate a mandate it suspended: each
// needs a fresh approval. changed is false when m is left as it is; a
// changed mandate has its epoch bumped by one and records actor.
func (m Mandate) FollowBinding(action Action, actor string, now time.Time) (next Mandate, changed bool, err error) {
	switch action {
	case ActionRevoke:
		if m.State == StateRevoked {
			return m, false, nil
		}
	case ActionSuspend:
		if m.EffectiveState(now) != StateActive {
			return m, false, nil
		}
	default:
		return Mandate{}, false, ErrInvalidTransition
	}
	next, err = m.Transition(action, actor, m.PolicyRevisionID, now)
	if err != nil {
		return Mandate{}, false, err
	}
	return next, true, nil
}

// CheckDelegation refuses a delegation that m cannot rest on. delegation is
// what the store read, inside the mandate's transaction, for m.DelegationID;
// nil means no row. A mandate without a delegation passes.
func CheckDelegation(m Mandate, delegation *Delegation, now time.Time) error {
	if m.DelegationID == nil {
		return nil
	}
	switch {
	case delegation == nil,
		delegation.ID != *m.DelegationID,
		delegation.TenantProjectID != m.TenantProjectID,
		delegation.AgentBindingID != m.AgentBindingID,
		delegation.State != StateActive,
		!now.Before(delegation.ExpiresAt):
		return ErrDelegationNotUsable
	}
	return nil
}

// Reader is the lookup later issues read mandates through.
type Reader interface {
	Get(ctx context.Context, id string) (Mandate, error)
}

// ListFilter narrows a mandate listing. Zero fields do not filter.
type ListFilter struct {
	TenantProjectID int64
	AgentBindingID  string
	State           State
	Limit           int
	Offset          int
}
