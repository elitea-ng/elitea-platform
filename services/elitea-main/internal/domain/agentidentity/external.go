package agentidentity

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The external identity states beyond the four reader.go declares (#1151).
// `retired` and `replaced` are terminal.
const (
	// ExternalIdentityProvisioning is a record written by an operator whose
	// provider object does not exist yet, or is still being created.
	ExternalIdentityProvisioning ExternalIdentityState = "provisioning"
	// ExternalIdentityPendingActivation is a provisioned object that is not
	// usable yet. Provisioning never activates.
	ExternalIdentityPendingActivation ExternalIdentityState = "pending_activation"
	// ExternalIdentityRetiring is a retirement whose provider revocation has
	// not been confirmed. A repeated retire completes it.
	ExternalIdentityRetiring ExternalIdentityState = "retiring"
)

// ExternalIdentityStateValues is every modelled state, in a stable order.
func ExternalIdentityStateValues() []ExternalIdentityState {
	return []ExternalIdentityState{
		ExternalIdentityProvisioning, ExternalIdentityPendingActivation, ExternalIdentityActive,
		ExternalIdentitySuspended, ExternalIdentityRetiring, ExternalIdentityRetired, ExternalIdentityReplaced,
	}
}

// Valid reports whether s is a modelled state.
func (s ExternalIdentityState) Valid() bool {
	for _, v := range ExternalIdentityStateValues() {
		if s == v {
			return true
		}
	}
	return false
}

// Terminal reports whether no action leaves s.
func (s ExternalIdentityState) Terminal() bool {
	return s == ExternalIdentityRetired || s == ExternalIdentityReplaced
}

// ExternalAction is a state change of an external agent identity.
type ExternalAction string

const (
	// ExternalActionProvision moves provisioning → pending_activation once the
	// provider object exists. It records the provider's ExternalRef.
	ExternalActionProvision ExternalAction = "provision"
	// ExternalActionActivate moves pending_activation → active. The caller
	// checks for an approved binding and the policy mode first.
	ExternalActionActivate ExternalAction = "activate"
	// ExternalActionSuspend moves active → suspended.
	ExternalActionSuspend ExternalAction = "suspend"
	// ExternalActionResume moves suspended → active.
	ExternalActionResume ExternalAction = "resume"
	// ExternalActionRetire moves any non-terminal state except retiring →
	// retiring. The provider object is revoked next.
	ExternalActionRetire ExternalAction = "retire"
	// ExternalActionCompleteRetire moves retiring → retired once the provider
	// confirmed the revocation.
	ExternalActionCompleteRetire ExternalAction = "complete_retire"
	// ExternalActionReplace moves any non-terminal state → replaced: the
	// provider object was recreated or its generation no longer matches.
	ExternalActionReplace ExternalAction = "replace"
)

// ExternalActions is every modelled action, in a stable order.
func ExternalActions() []ExternalAction {
	return []ExternalAction{
		ExternalActionProvision, ExternalActionActivate, ExternalActionSuspend, ExternalActionResume,
		ExternalActionRetire, ExternalActionCompleteRetire, ExternalActionReplace,
	}
}

// Typed errors of the external identity record. Each message names a rule,
// never a stored value.
var (
	ErrInvalidExternalIdentity    = errors.New("external agent identity: invalid external identity")
	ErrExternalTransition         = errors.New("external agent identity: transition not allowed from the current state")
	ErrExternalIdentityNotFound   = errors.New("external agent identity: not found")
	ErrDuplicateExternalIdentity  = errors.New("external agent identity: a live external identity already exists for this application, provider and provider tenant")
	ErrNoApprovedBinding          = errors.New("external agent identity: activation needs an approved binding that references it")
	ErrExternalIdentityTerminated = errors.New("external agent identity: retired or replaced")
)

var (
	providerNamePattern   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	providerTenantPattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)
	externalKindPattern   = regexp.MustCompile(`^[a-z][a-z0-9._]{0,63}$`)
	configKeyPattern      = regexp.MustCompile(`^[A-Za-z0-9._:/-]{0,128}$`)
)

const maxExternalIDBytes = 256

// ValidProviderName reports whether name is a provider name: lower case, at
// most 32 bytes. Registry and record share the rule.
func ValidProviderName(name string) bool { return providerNamePattern.MatchString(name) }

// ExternalRef is the provider-side key of an external agent identity
// (ADR-0039 decision 1): provider + provider tenant + immutable id +
// generation.
type ExternalRef struct {
	Provider       string // "entra" | "elitea" | "keycloak" | ...
	ProviderTenant string // Entra tid, realm, account; validated, not free text
	Kind           string // "entra.agent_identity", "elitea.agent", "kc.client", ...
	ID             string // immutable object id; empty until provisioned
	Generation     int64  // bumps on recreate or rotation
}

// Validate checks the ref. provisioned requires Kind and ID.
func (r ExternalRef) Validate(provisioned bool) error {
	switch {
	case !ValidProviderName(r.Provider):
		return fmt.Errorf("%w: provider must be a lower-case name of at most 32 bytes", ErrInvalidExternalIdentity)
	case !providerTenantPattern.MatchString(r.ProviderTenant):
		return fmt.Errorf("%w: provider_tenant must be 1..128 of [A-Za-z0-9._:/-]", ErrInvalidExternalIdentity)
	case r.Generation < 0:
		return fmt.Errorf("%w: generation must not be negative", ErrInvalidExternalIdentity)
	case len(r.ID) > maxExternalIDBytes || strings.ContainsAny(r.ID, " \t\r\n"):
		return fmt.Errorf("%w: the external id must be at most %d bytes without whitespace", ErrInvalidExternalIdentity, maxExternalIDBytes)
	case r.Kind != "" && !externalKindPattern.MatchString(r.Kind):
		return fmt.Errorf("%w: kind must be a lower-case dotted name", ErrInvalidExternalIdentity)
	}
	if provisioned && (r.Kind == "" || r.ID == "") {
		return fmt.Errorf("%w: a provisioned ref needs a kind and an id", ErrInvalidExternalIdentity)
	}
	return nil
}

// ExternalAgentIdentity is one logical agent's identity in an external
// identity provider, shared by every version of the agent (ADR-0039
// decision 1). A fork or an import gets a new one.
//
// It holds references only. ProviderConfigKey names operator configuration
// (for example the Entra blueprint); it is never a credential.
type ExternalAgentIdentity struct {
	ID                string // uuid v7
	TenantProjectID   int64
	ApplicationID     int64
	Mode              IdentityMode
	Ref               ExternalRef
	ProviderConfigKey string
	AgentUserRef      string // optional (Entra agent user oid)
	OwnerRef          string
	State             ExternalIdentityState
	// RevocationEpoch increases by exactly one on every transition.
	RevocationEpoch  int64
	CreatedBy        string
	LastTransitionBy string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ExternalIdentityRequest is what an operator supplies to create a record.
type ExternalIdentityRequest struct {
	TenantProjectID   int64
	ApplicationID     int64
	Mode              IdentityMode
	Provider          string
	ProviderTenant    string
	ProviderConfigKey string
	OwnerRef          string
}

// Validate checks the shape of a request.
func (r ExternalIdentityRequest) Validate() error {
	switch {
	case r.TenantProjectID <= 0:
		return fmt.Errorf("%w: tenant_project_id must be positive", ErrInvalidExternalIdentity)
	case r.ApplicationID <= 0:
		return fmt.Errorf("%w: application_id must be positive", ErrInvalidExternalIdentity)
	case !r.Mode.Valid():
		return fmt.Errorf("%w: unknown identity mode", ErrInvalidExternalIdentity)
	case !configKeyPattern.MatchString(r.ProviderConfigKey):
		return fmt.Errorf("%w: provider_config_key must be at most 128 of [A-Za-z0-9._:/-]", ErrInvalidExternalIdentity)
	case len(r.OwnerRef) > 128:
		return fmt.Errorf("%w: owner_ref must be at most 128 bytes", ErrInvalidExternalIdentity)
	}
	return ExternalRef{Provider: r.Provider, ProviderTenant: r.ProviderTenant}.Validate(false)
}

// NewExternalIdentity builds a record in `provisioning` from a validated
// request. The mode must be allowed by policy. Epoch starts at 0.
func NewExternalIdentity(id string, r ExternalIdentityRequest, policy ModePolicy, createdBy string, now time.Time) (ExternalAgentIdentity, error) {
	if err := r.Validate(); err != nil {
		return ExternalAgentIdentity{}, err
	}
	if policy == nil || !policy.AllowsIdentityMode(r.Mode) {
		return ExternalAgentIdentity{}, ErrIdentityModeNotAllowed
	}
	if strings.TrimSpace(createdBy) == "" {
		return ExternalAgentIdentity{}, fmt.Errorf("%w: the creator is unknown", ErrInvalidExternalIdentity)
	}
	if _, err := uuid.Parse(id); err != nil {
		return ExternalAgentIdentity{}, fmt.Errorf("%w: id must be a uuid", ErrInvalidExternalIdentity)
	}
	return ExternalAgentIdentity{
		ID: id, TenantProjectID: r.TenantProjectID, ApplicationID: r.ApplicationID, Mode: r.Mode,
		Ref:               ExternalRef{Provider: r.Provider, ProviderTenant: r.ProviderTenant},
		ProviderConfigKey: r.ProviderConfigKey, OwnerRef: r.OwnerRef,
		State: ExternalIdentityProvisioning, CreatedBy: createdBy, LastTransitionBy: createdBy,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// NextExternalState answers the state an action leads to, or
// ErrExternalTransition. It is the whole transition table:
//
//	provisioning       --provision-->       pending_activation
//	pending_activation --activate-->        active
//	active             --suspend-->         suspended
//	suspended          --resume-->          active
//	provisioning|pending_activation|active|suspended --retire--> retiring
//	retiring           --complete_retire--> retired
//	any non-terminal   --replace-->         replaced
//
// Everything else, and every action from retired or replaced, is refused.
func NextExternalState(state ExternalIdentityState, action ExternalAction) (ExternalIdentityState, error) {
	if !state.Valid() || state.Terminal() {
		return "", ErrExternalTransition
	}
	switch action {
	case ExternalActionProvision:
		if state == ExternalIdentityProvisioning {
			return ExternalIdentityPendingActivation, nil
		}
	case ExternalActionActivate:
		if state == ExternalIdentityPendingActivation {
			return ExternalIdentityActive, nil
		}
	case ExternalActionSuspend:
		if state == ExternalIdentityActive {
			return ExternalIdentitySuspended, nil
		}
	case ExternalActionResume:
		if state == ExternalIdentitySuspended {
			return ExternalIdentityActive, nil
		}
	case ExternalActionRetire:
		if state != ExternalIdentityRetiring {
			return ExternalIdentityRetiring, nil
		}
	case ExternalActionCompleteRetire:
		if state == ExternalIdentityRetiring {
			return ExternalIdentityRetired, nil
		}
	case ExternalActionReplace:
		return ExternalIdentityReplaced, nil
	}
	return "", ErrExternalTransition
}

// Transition applies action by actor at now. The epoch increases by exactly
// one. A provision records ref, which must be a provisioned ref of the same
// provider and provider tenant; every other action ignores ref.
func (e ExternalAgentIdentity) Transition(action ExternalAction, actor string, ref ExternalRef, agentUserRef string, now time.Time) (ExternalAgentIdentity, error) {
	next, err := NextExternalState(e.State, action)
	if err != nil {
		return ExternalAgentIdentity{}, err
	}
	if strings.TrimSpace(actor) == "" {
		return ExternalAgentIdentity{}, fmt.Errorf("%w: the actor is unknown", ErrInvalidExternalIdentity)
	}
	out := e
	if action == ExternalActionProvision {
		if err := ref.Validate(true); err != nil {
			return ExternalAgentIdentity{}, err
		}
		if ref.Provider != e.Ref.Provider || ref.ProviderTenant != e.Ref.ProviderTenant {
			return ExternalAgentIdentity{}, fmt.Errorf("%w: the provider answered for another provider or tenant", ErrInvalidExternalIdentity)
		}
		if len(agentUserRef) > maxExternalIDBytes {
			return ExternalAgentIdentity{}, fmt.Errorf("%w: agent_user_ref is too long", ErrInvalidExternalIdentity)
		}
		out.Ref, out.AgentUserRef = ref, agentUserRef
	}
	out.State = next
	out.LastTransitionBy = actor
	out.RevocationEpoch = e.RevocationEpoch + 1
	out.UpdatedAt = now
	return out, nil
}

// ExternalListFilter narrows a listing. Zero fields do not filter.
type ExternalListFilter struct {
	TenantProjectID int64
	ApplicationID   int64
	State           ExternalIdentityState
	Limit           int
	Offset          int
}

// UsableBinding reports whether b may authorise anything: the binding is
// active and its external identity is active (issue 1112 addendum, rule 3).
func UsableBinding(b AgentIdentityBinding, external ExternalIdentityState) bool {
	return b.State == BindingActive && b.ExternalAgentIdentityID != "" && external == ExternalIdentityActive
}
