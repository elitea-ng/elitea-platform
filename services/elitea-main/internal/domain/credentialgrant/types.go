// Package credentialgrant holds the credential grant model of ADR-0039
// (decisions 7 and 8, issue #1116): the credential reference, the target it
// is bound to, the toolkit an agent binding may run, and the exact
// CredentialUseGrant that binds them, with its state machine.
//
// It stores references only. No type here holds a secret value, a token or
// a provider response.
package credentialgrant

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Capability is one separately granted right over a governed credential.
// One never implies another (ADR-0039 decision 8): a `use` grant does not
// let a caller read the value (`reveal`).
type Capability string

const (
	CapUse     Capability = "use"
	CapReveal  Capability = "reveal"
	CapManage  Capability = "manage"
	CapProbe   Capability = "probe"
	CapApprove Capability = "approve"
)

// Capabilities is every modelled capability, in a stable order.
func Capabilities() []Capability {
	return []Capability{CapUse, CapReveal, CapManage, CapProbe, CapApprove}
}

// Valid reports whether c is a modelled capability.
func (c Capability) Valid() bool {
	switch c {
	case CapUse, CapReveal, CapManage, CapProbe, CapApprove:
		return true
	}
	return false
}

// CredentialClass is the kind of governed credential (gate D1).
type CredentialClass string

const (
	ClassStaticSecret   CredentialClass = "static_secret"
	ClassOAuthDelegated CredentialClass = "oauth_delegated"
	ClassOAuthService   CredentialClass = "oauth_service"
	ClassDynamicLease   CredentialClass = "dynamic_lease"
)

// Valid reports whether c is a modelled class.
func (c CredentialClass) Valid() bool {
	switch c {
	case ClassStaticSecret, ClassOAuthDelegated, ClassOAuthService, ClassDynamicLease:
		return true
	}
	return false
}

// OAuthAuthority is who issued the consent an oauth_delegated credential
// rests on (ADR-0039 decision 3). The provider adapter descriptor states it.
type OAuthAuthority string

const (
	// OAuthAuthorityEntraOBO: the user's Entra delegation, exchanged on
	// behalf of the user for the provider resource (SharePoint: Graph).
	OAuthAuthorityEntraOBO OAuthAuthority = "entra_obo"
	// OAuthAuthorityProvider: the user consented to the provider directly
	// (Atlassian 3LO, Aha! OAuth2); the provider grant is the authority.
	OAuthAuthorityProvider OAuthAuthority = "provider"
)

// Valid reports whether a is a modelled authority.
func (a OAuthAuthority) Valid() bool {
	return a == OAuthAuthorityEntraOBO || a == OAuthAuthorityProvider
}

// ReferenceState is where a credential reference is in its rotation.
type ReferenceState string

const (
	ReferenceStaged   ReferenceState = "staged"
	ReferenceActive   ReferenceState = "active"
	ReferenceRetiring ReferenceState = "retiring"
	ReferenceRetired  ReferenceState = "retired"
)

// Usable reports whether a credential in state s may be released for use.
// `retiring` stays usable so a rotation has an overlap window.
func (s ReferenceState) Usable() bool {
	return s == ReferenceActive || s == ReferenceRetiring
}

// CredentialReference names where a governed credential lives. Locator is a
// backend-relative path, never a value.
type CredentialReference struct {
	ID              string
	TenantProjectID int64
	Class           CredentialClass
	Backend         string // "pgvault" | "vault"
	Locator         string
	ProviderType    string // adapter id, e.g. "jira"
	TargetBindingID string
	Generation      int64
	State           ReferenceState
}

// TargetBinding is the canonical provider target a credential is for.
type TargetBinding struct {
	ID              string
	TenantProjectID int64
	ProviderType    string
	// TargetIdentity is canonical: scheme+host+base path, or the provider
	// tenant/site id.
	TargetIdentity string
	// Audience is the OAuth resource/audience when relevant.
	Audience string
}

// AgentToolkitBinding is the toolkit, at one digest, an agent binding may run.
type AgentToolkitBinding struct {
	AgentBindingID  string
	TenantProjectID int64
	ToolkitID       int64
	ToolkitDigest   [32]byte
}

// GrantState is where a CredentialUseGrant is in its life cycle. `revoked`
// is terminal.
type GrantState string

const (
	// GrantProposed authorises nothing until a second person approves it.
	GrantProposed  GrantState = "proposed"
	GrantActive    GrantState = "active"
	GrantSuspended GrantState = "suspended"
	GrantRevoked   GrantState = "revoked"
)

// Valid reports whether s is a modelled state.
func (s GrantState) Valid() bool {
	switch s {
	case GrantProposed, GrantActive, GrantSuspended, GrantRevoked:
		return true
	}
	return false
}

// Action is a grant state change an operator asks for.
type Action string

const (
	// ActionApprove moves proposed → active and suspended → active. The
	// approver must not be the proposer.
	ActionApprove Action = "approve"
	ActionSuspend Action = "suspend"
	ActionRevoke  Action = "revoke"
)

// Actions is every modelled action, in a stable order.
func Actions() []Action { return []Action{ActionApprove, ActionSuspend, ActionRevoke} }

// Obligation kinds a grant may carry. An enforcement point that cannot
// fulfil one denies (authzcompose.Fulfil).
const (
	ObligationAuditSync       = "audit.sync"
	ObligationRequireApproval = "require.approval"
)

// KnownObligation reports whether kind is a modelled obligation.
func KnownObligation(kind string) bool {
	return kind == ObligationAuditSync || kind == ObligationRequireApproval
}

// Typed errors. Each message is safe to return to a caller.
var (
	ErrInvalidGrant      = errors.New("credential grant: invalid grant")
	ErrInvalidTransition = errors.New("credential grant: transition not allowed from the current state")
	ErrSelfApproval      = errors.New("credential grant: the approver must not be the proposer")
	ErrGrantNotFound     = errors.New("credential grant: not found")
	// ErrDuplicateGrant refuses a second non-revoked grant for one agent
	// binding, toolkit, credential reference and target.
	ErrDuplicateGrant = errors.New("credential grant: a non-revoked grant already exists for this agent, toolkit, credential and target")
	// ErrReferenceNotUsable refuses a grant whose agent binding, credential
	// reference or target does not exist in the tenant project, or whose
	// credential is bound to another target.
	ErrReferenceNotUsable = errors.New("credential grant: the agent binding, credential reference or target is not usable")
	ErrReferenceNotFound  = errors.New("credential grant: credential reference not found")
	ErrToolkitNotBound    = errors.New("credential grant: the toolkit is not bound to the agent")
	ErrTargetNotFound     = errors.New("credential grant: target binding not found")
)

const (
	maxScopes      = 64
	maxScopeBytes  = 256
	maxObligations = 16
)

// CredentialUseGrant is the exact grant of decision 7 check 4: one agent
// binding × toolkit × credential reference × target, with the capabilities,
// scopes and obligations it allows.
type CredentialUseGrant struct {
	ID                    string // uuid v7
	TenantProjectID       int64
	AgentBindingID        string
	ToolkitID             int64
	CredentialReferenceID string
	TargetBindingID       string
	Capabilities          []Capability
	// OperationScopes are canonical operation ids. Required.
	OperationScopes []string
	// ResourceScopes are resource refs. Empty means the grant does not narrow
	// resources; the mandate still may.
	ResourceScopes []string
	Obligations    []string
	State          GrantState
	// Epoch increases by exactly one on every transition.
	Epoch      int64
	ExpiresAt  *time.Time
	ProposedBy string // user id; never equal to ApprovedBy
	ApprovedBy string
	// LastTransitionBy is the actor of the latest change.
	LastTransitionBy string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Allows reports whether the grant lists capability c.
func (g CredentialUseGrant) Allows(c Capability) bool {
	return slices.Contains(g.Capabilities, c)
}

// ActiveAt reports whether the grant is active and unexpired at now.
func (g CredentialUseGrant) ActiveAt(now time.Time) bool {
	return g.State == GrantActive && (g.ExpiresAt == nil || now.Before(*g.ExpiresAt))
}

// Proposal is what an operator supplies to propose a grant.
type Proposal struct {
	TenantProjectID       int64
	AgentBindingID        string
	ToolkitID             int64
	CredentialReferenceID string
	TargetBindingID       string
	Capabilities          []Capability
	OperationScopes       []string
	ResourceScopes        []string
	Obligations           []string
	ExpiresAt             *time.Time
}

func validUUID(name, value string) error {
	if _, err := uuid.Parse(value); err != nil {
		return fmt.Errorf("%w: %s must be a uuid", ErrInvalidGrant, name)
	}
	return nil
}

func validScopes(name string, scopes []string, required bool, check func(string) bool) error {
	if required && len(scopes) == 0 {
		return fmt.Errorf("%w: %s must not be empty", ErrInvalidGrant, name)
	}
	if len(scopes) > maxScopes {
		return fmt.Errorf("%w: %s lists more than %d values", ErrInvalidGrant, name, maxScopes)
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if scope == "" || len(scope) > maxScopeBytes || strings.ContainsAny(scope, " \t\r\n") || (check != nil && !check(scope)) {
			return fmt.Errorf("%w: a %s value is malformed", ErrInvalidGrant, name)
		}
		if _, dup := seen[scope]; dup {
			return fmt.Errorf("%w: %s repeats a value", ErrInvalidGrant, name)
		}
		seen[scope] = struct{}{}
	}
	return nil
}

// Validate checks the shape of a proposal against now.
func (p Proposal) Validate(now time.Time) error {
	if p.TenantProjectID <= 0 {
		return fmt.Errorf("%w: tenant_project_id must be positive", ErrInvalidGrant)
	}
	if p.ToolkitID <= 0 {
		return fmt.Errorf("%w: toolkit_id must be positive", ErrInvalidGrant)
	}
	for name, value := range map[string]string{
		"agent_binding_id":        p.AgentBindingID,
		"credential_reference_id": p.CredentialReferenceID,
		"target_binding_id":       p.TargetBindingID,
	} {
		if err := validUUID(name, value); err != nil {
			return err
		}
	}
	if len(p.Capabilities) == 0 {
		return fmt.Errorf("%w: capabilities must not be empty", ErrInvalidGrant)
	}
	seen := map[Capability]bool{}
	for _, c := range p.Capabilities {
		if !c.Valid() || seen[c] {
			return fmt.Errorf("%w: capabilities must be distinct modelled values", ErrInvalidGrant)
		}
		seen[c] = true
	}
	if err := validScopes("operation_scopes", p.OperationScopes, true, func(s string) bool { return OperationID(s).WellFormed() }); err != nil {
		return err
	}
	if err := validScopes("resource_scopes", p.ResourceScopes, false, nil); err != nil {
		return err
	}
	if len(p.Obligations) > maxObligations {
		return fmt.Errorf("%w: obligations lists more than %d values", ErrInvalidGrant, maxObligations)
	}
	if err := validScopes("obligations", p.Obligations, false, KnownObligation); err != nil {
		return err
	}
	if p.ExpiresAt != nil && !p.ExpiresAt.After(now) {
		return fmt.Errorf("%w: expires_at must be in the future", ErrInvalidGrant)
	}
	return nil
}

// NewGrant builds a proposed grant from a valid proposal. Epoch starts at 0.
func NewGrant(id string, p Proposal, proposedBy string, now time.Time) (CredentialUseGrant, error) {
	if err := validUUID("id", id); err != nil {
		return CredentialUseGrant{}, err
	}
	if err := p.Validate(now); err != nil {
		return CredentialUseGrant{}, err
	}
	if strings.TrimSpace(proposedBy) == "" {
		return CredentialUseGrant{}, fmt.Errorf("%w: the proposer is unknown", ErrInvalidGrant)
	}
	resources := p.ResourceScopes
	if resources == nil {
		resources = []string{}
	}
	obligations := p.Obligations
	if obligations == nil {
		obligations = []string{}
	}
	return CredentialUseGrant{
		ID:                    id,
		TenantProjectID:       p.TenantProjectID,
		AgentBindingID:        p.AgentBindingID,
		ToolkitID:             p.ToolkitID,
		CredentialReferenceID: p.CredentialReferenceID,
		TargetBindingID:       p.TargetBindingID,
		Capabilities:          slices.Clone(p.Capabilities),
		OperationScopes:       slices.Clone(p.OperationScopes),
		ResourceScopes:        slices.Clone(resources),
		Obligations:           slices.Clone(obligations),
		State:                 GrantProposed,
		ExpiresAt:             p.ExpiresAt,
		ProposedBy:            proposedBy,
		LastTransitionBy:      proposedBy,
		CreatedAt:             now,
		UpdatedAt:             now,
	}, nil
}

// NextState answers the state an action leads to from state:
//
//	proposed  --approve--> active
//	active    --suspend--> suspended
//	suspended --approve--> active
//	proposed|active|suspended --revoke--> revoked
//
// Everything else, and every action from revoked, is ErrInvalidTransition.
func NextState(state GrantState, action Action) (GrantState, error) {
	switch {
	case action == ActionRevoke && state != GrantRevoked && state.Valid():
		return GrantRevoked, nil
	case action == ActionApprove && (state == GrantProposed || state == GrantSuspended):
		return GrantActive, nil
	case action == ActionSuspend && state == GrantActive:
		return GrantSuspended, nil
	}
	return "", ErrInvalidTransition
}

// Transition applies action by actor at now. The epoch increases by exactly
// one. An approval records the approver and refuses the proposer (the
// two-person rule; the database CHECK refuses it as well).
func (g CredentialUseGrant) Transition(action Action, actor string, now time.Time) (CredentialUseGrant, error) {
	next, err := NextState(g.State, action)
	if err != nil {
		return CredentialUseGrant{}, err
	}
	if strings.TrimSpace(actor) == "" {
		return CredentialUseGrant{}, fmt.Errorf("%w: the actor is unknown", ErrInvalidGrant)
	}
	out := g
	if action == ActionApprove {
		if actor == g.ProposedBy {
			return CredentialUseGrant{}, ErrSelfApproval
		}
		out.ApprovedBy = actor
	}
	out.State = next
	out.LastTransitionBy = actor
	out.Epoch = g.Epoch + 1
	out.UpdatedAt = now
	return out, nil
}

// GrantKey is the exact key of decision 7 check 4.
type GrantKey struct {
	TenantProjectID       int64
	AgentBindingID        string
	ToolkitID             int64
	CredentialReferenceID string
	TargetBindingID       string
}

// ListFilter narrows a grant listing. Zero fields do not filter except
// TenantProjectID, which is required.
type ListFilter struct {
	TenantProjectID int64
	AgentBindingID  string
	State           GrantState
	Limit           int
	Offset          int
}
