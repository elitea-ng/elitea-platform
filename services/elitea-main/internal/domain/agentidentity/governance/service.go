// Package governance applies the active credential policy revision to agent
// identity bindings and mandates (issue 1112): it is the one place that
// combines the pure state machines in agentidentity and mandate with the
// policy, the external identity states and the stores.
//
// It lives below agentidentity, not in it, because profile imports
// agentidentity for IdentityMode.
package governance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

// ErrPolicyUnavailable means the active policy revision could not be read.
// Every decision then fails closed.
var ErrPolicyUnavailable = errors.New("agent identity governance: the active policy revision is unavailable")

// RevisionSource returns the active policy revision. With the governance
// flag on, boot already validated it (profile.Boot).
type RevisionSource interface {
	ActivePolicyRevision(ctx context.Context) (profile.PolicyRevision, error)
}

// FixedRevision is a RevisionSource over the one revision boot returned.
type FixedRevision profile.PolicyRevision

// ActivePolicyRevision returns the revision.
func (r FixedRevision) ActivePolicyRevision(context.Context) (profile.PolicyRevision, error) {
	return profile.PolicyRevision(r), nil
}

// BindingStore persists bindings. Every write runs in one transaction that
// also appends a securityaudit.TypeBindingChanged event; an append error
// rolls the change back.
//
// TransitionBinding locks the row (SELECT … FOR UPDATE), passes it to apply
// and writes apply's result in the same transaction. When the result is
// revoked it also revokes every non-revoked mandate on the binding, and when
// it is suspended it suspends every active one (mandate.FollowBinding), each
// with its epoch bumped and its own TypeMandateChanged event.
type BindingStore interface {
	CreateBinding(ctx context.Context, binding agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error)
	Get(ctx context.Context, id string) (agentidentity.AgentIdentityBinding, error)
	ListBindings(ctx context.Context, filter agentidentity.ListFilter) ([]agentidentity.AgentIdentityBinding, error)
	TransitionBinding(ctx context.Context, id string, apply func(agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error)) (agentidentity.AgentIdentityBinding, error)
}

// MandateStore persists mandates. Every write runs in one transaction that
// locks the mandate's binding (SELECT … FOR SHARE) BEFORE the mandate row,
// so a binding revoke or suspend and a mandate write serialise, and appends
// a securityaudit.TypeMandateChanged event; an append error rolls the change
// back.
//
// CreateMandate passes the locked binding and the delegation it read for
// m.DelegationID (nil when absent or unset), both inside the transaction, to
// check; a check error writes nothing. A binding that does not exist is
// mandate.ErrBindingNotUsable.
//
// TransitionMandate passes the locked mandate and its locked binding to apply.
type MandateStore interface {
	CreateMandate(ctx context.Context, m mandate.Mandate, check func(binding agentidentity.AgentIdentityBinding, delegation *mandate.Delegation) error) (mandate.Mandate, error)
	Get(ctx context.Context, id string) (mandate.Mandate, error)
	ListMandates(ctx context.Context, filter mandate.ListFilter) ([]mandate.Mandate, error)
	TransitionMandate(ctx context.Context, id string, apply func(current mandate.Mandate, binding agentidentity.AgentIdentityBinding) (mandate.Mandate, error)) (mandate.Mandate, error)
}

// Config is the service's dependencies. Every field except Now and NewID is
// required.
type Config struct {
	Revisions          RevisionSource
	Bindings           BindingStore
	Mandates           MandateStore
	ExternalIdentities agentidentity.ExternalIdentityStates
	Now                func() time.Time
	NewID              func() (string, error)
}

// Service is the binding and mandate decision surface the admin API calls.
type Service struct {
	revisions RevisionSource
	bindings  BindingStore
	mandates  MandateStore
	external  agentidentity.ExternalIdentityStates
	now       func() time.Time
	newID     func() (string, error)
}

// New builds the service. It refuses a missing dependency rather than
// failing open later.
func New(cfg Config) (*Service, error) {
	if cfg.Revisions == nil || cfg.Bindings == nil || cfg.Mandates == nil || cfg.ExternalIdentities == nil {
		return nil, errors.New("agent identity governance: revisions, bindings, mandates and external identities are required")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	newID := cfg.NewID
	if newID == nil {
		newID = func() (string, error) {
			id, err := uuid.NewV7()
			if err != nil {
				return "", err
			}
			return id.String(), nil
		}
	}
	return &Service{
		revisions: cfg.Revisions, bindings: cfg.Bindings, mandates: cfg.Mandates,
		external: cfg.ExternalIdentities, now: now, newID: newID,
	}, nil
}

func (s *Service) revision(ctx context.Context) (profile.PolicyRevision, error) {
	revision, err := s.revisions.ActivePolicyRevision(ctx)
	if err != nil {
		return profile.PolicyRevision{}, fmt.Errorf("%w: %w", ErrPolicyUnavailable, err)
	}
	return revision, nil
}

// ProposeBinding writes a proposed binding. A mode the active revision does
// not allow is agentidentity.ErrIdentityModeNotAllowed.
func (s *Service) ProposeBinding(ctx context.Context, proposal agentidentity.Proposal, proposedBy string) (agentidentity.AgentIdentityBinding, error) {
	revision, err := s.revision(ctx)
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, err
	}
	id, err := s.newID()
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, fmt.Errorf("new binding id: %w", err)
	}
	binding, err := agentidentity.NewBinding(id, proposal, revision.Document, revision.ID, proposedBy, s.now())
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, err
	}
	return s.bindings.CreateBinding(ctx, binding)
}

// ListBindings lists bindings.
func (s *Service) ListBindings(ctx context.Context, filter agentidentity.ListFilter) ([]agentidentity.AgentIdentityBinding, error) {
	return s.bindings.ListBindings(ctx, filter)
}

// TransitionBinding applies action by actor. An approval also requires the
// mode to be allowed by the CURRENT revision and the external identity to be
// active; suspend and revoke never depend on policy, so a withdrawn revision
// can still stop an agent.
func (s *Service) TransitionBinding(ctx context.Context, id string, action agentidentity.Action, actor string) (agentidentity.AgentIdentityBinding, error) {
	var revision profile.PolicyRevision
	if action == agentidentity.ActionApprove {
		var err error
		if revision, err = s.revision(ctx); err != nil {
			return agentidentity.AgentIdentityBinding{}, err
		}
	}
	return s.bindings.TransitionBinding(ctx, id, func(current agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
		next, err := current.Transition(action, actor, revision.ID, s.now())
		if err != nil || action != agentidentity.ActionApprove {
			return next, err
		}
		if !revision.Document.AllowsIdentityMode(current.Mode) {
			return agentidentity.AgentIdentityBinding{}, agentidentity.ErrIdentityModeNotAllowed
		}
		if current.ExternalAgentIdentityID == "" {
			return agentidentity.AgentIdentityBinding{}, agentidentity.ErrExternalIdentityNotActive
		}
		state, err := s.external.ExternalIdentityState(ctx, current.TenantProjectID, current.ExternalAgentIdentityID)
		if err != nil || state != agentidentity.ExternalIdentityActive {
			return agentidentity.AgentIdentityBinding{}, agentidentity.ErrExternalIdentityNotActive
		}
		return next, nil
	})
}

// IssueMandate writes a proposed mandate for a binding in the same tenant
// project that is not revoked. ExpiresAt is bounded by the active revision.
// A DelegationID must name an active delegation of the same tenant project
// and the same binding (mandate.ErrDelegationNotUsable otherwise). Both
// checks read rows the store locked in the mandate's transaction.
func (s *Service) IssueMandate(ctx context.Context, req mandate.Request, issuedBy string) (mandate.Mandate, error) {
	revision, err := s.revision(ctx)
	if err != nil {
		return mandate.Mandate{}, err
	}
	id, err := s.newID()
	if err != nil {
		return mandate.Mandate{}, fmt.Errorf("new mandate id: %w", err)
	}
	now := s.now()
	m, err := mandate.New(id, req, revision.Document, revision.ID, issuedBy, now)
	if err != nil {
		return mandate.Mandate{}, err
	}
	return s.mandates.CreateMandate(ctx, m, func(binding agentidentity.AgentIdentityBinding, delegation *mandate.Delegation) error {
		if binding.TenantProjectID != m.TenantProjectID || binding.State == agentidentity.BindingRevoked {
			return mandate.ErrBindingNotUsable
		}
		return mandate.CheckDelegation(m, delegation, now)
	})
}

// ListMandates lists mandates.
func (s *Service) ListMandates(ctx context.Context, filter mandate.ListFilter) ([]mandate.Mandate, error) {
	return s.mandates.ListMandates(ctx, filter)
}

// TransitionMandate applies action by actor. An approval requires the
// binding, locked in the same transaction, to be active, and the expiry to
// still fit the current revision's maximum.
func (s *Service) TransitionMandate(ctx context.Context, id string, action mandate.Action, actor string) (mandate.Mandate, error) {
	var revision profile.PolicyRevision
	if action == mandate.ActionApprove {
		var err error
		if revision, err = s.revision(ctx); err != nil {
			return mandate.Mandate{}, err
		}
	}
	return s.mandates.TransitionMandate(ctx, id, func(current mandate.Mandate, binding agentidentity.AgentIdentityBinding) (mandate.Mandate, error) {
		now := s.now()
		next, err := current.Transition(action, actor, revision.ID, now)
		if err != nil || action != mandate.ActionApprove {
			return next, err
		}
		if limit := revision.Document.MaxMandateTTL(); limit <= 0 || current.ExpiresAt.Sub(now) > limit {
			return mandate.Mandate{}, mandate.ErrTTLExceedsPolicy
		}
		if binding.ID != current.AgentBindingID || binding.State != agentidentity.BindingActive ||
			binding.TenantProjectID != current.TenantProjectID {
			return mandate.Mandate{}, mandate.ErrBindingNotUsable
		}
		return next, nil
	})
}
