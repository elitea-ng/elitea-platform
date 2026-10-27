package agentidentity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity/governance"
)

// Errors of the external identity life cycle. Messages are safe to return.
var (
	// ErrProviderNotConfigured means the registry has no adapter for the
	// record's provider.
	ErrProviderNotConfigured = errors.New("agent identity: no provider adapter is configured for this provider")
	// ErrProviderCall wraps an adapter failure. The adapter's own message is
	// not passed on.
	ErrProviderCall = errors.New("agent identity: the provider call failed")
)

// ExternalIdentityStore persists external agent identities. Every write runs
// in one transaction that also appends a
// securityaudit.TypeExternalIdentityChanged event; an append error rolls the
// change back.
//
// TransitionExternalIdentity locks the row (SELECT … FOR UPDATE), counts the
// active bindings that reference it (FOR SHARE), passes both to apply and
// writes apply's result guarded by the epoch it read.
type ExternalIdentityStore interface {
	CreateExternalIdentity(ctx context.Context, e identity.ExternalAgentIdentity) (identity.ExternalAgentIdentity, error)
	GetExternalIdentity(ctx context.Context, id string) (identity.ExternalAgentIdentity, error)
	ListExternalIdentities(ctx context.Context, filter identity.ExternalListFilter) ([]identity.ExternalAgentIdentity, error)
	TransitionExternalIdentity(ctx context.Context, id string, apply func(current identity.ExternalAgentIdentity, activeBindings int) (identity.ExternalAgentIdentity, error)) (identity.ExternalAgentIdentity, error)
	// ActiveBindingCount counts the active bindings of the tenant project
	// that reference the record, without a lock.
	ActiveBindingCount(ctx context.Context, tenantProjectID int64, id string) (int, error)
}

// ExternalIdentityConfig is the service's dependencies. Now and NewID are
// optional.
type ExternalIdentityConfig struct {
	Revisions governance.RevisionSource
	Store     ExternalIdentityStore
	Registry  *Registry
	Now       func() time.Time
	NewID     func() (string, error)
}

// ExternalIdentityService is the decision surface of the external identity
// admin API. Provider calls that start or widen use (provision, activate,
// resume) run before the write; calls that stop use (suspend, retire) run
// after it, so a provider failure never leaves an identity usable that an
// operator stopped.
type ExternalIdentityService struct {
	revisions governance.RevisionSource
	store     ExternalIdentityStore
	registry  *Registry
	now       func() time.Time
	newID     func() (string, error)
}

// NewExternalIdentityService builds the service. A nil registry is refused;
// an empty one is valid.
func NewExternalIdentityService(cfg ExternalIdentityConfig) (*ExternalIdentityService, error) {
	if cfg.Revisions == nil || cfg.Store == nil || cfg.Registry == nil {
		return nil, errors.New("external agent identity service: revisions, store and registry are required")
	}
	s := &ExternalIdentityService{revisions: cfg.Revisions, store: cfg.Store, registry: cfg.Registry, now: cfg.Now, newID: cfg.NewID}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.newID == nil {
		s.newID = newUUIDv7
	}
	return s, nil
}

func newUUIDv7() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func (s *ExternalIdentityService) modePolicy(ctx context.Context) (identity.ModePolicy, error) {
	revision, err := s.revisions.ActivePolicyRevision(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", governance.ErrPolicyUnavailable, err)
	}
	return revision.Document, nil
}

// Create writes a record in `provisioning`. No provider is called.
func (s *ExternalIdentityService) Create(ctx context.Context, req identity.ExternalIdentityRequest, actor string) (identity.ExternalAgentIdentity, error) {
	policy, err := s.modePolicy(ctx)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	id, err := s.newID()
	if err != nil {
		return identity.ExternalAgentIdentity{}, fmt.Errorf("new external identity id: %w", err)
	}
	record, err := identity.NewExternalIdentity(id, req, policy, actor, s.now())
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	return s.store.CreateExternalIdentity(ctx, record)
}

// List lists records.
func (s *ExternalIdentityService) List(ctx context.Context, filter identity.ExternalListFilter) ([]identity.ExternalAgentIdentity, error) {
	return s.store.ListExternalIdentities(ctx, filter)
}

func (s *ExternalIdentityService) provider(name string) (Provider, error) {
	p, ok := s.registry.Provider(name)
	if !ok {
		return nil, ErrProviderNotConfigured
	}
	return p, nil
}

// guarded applies action only when the row still has the epoch the caller
// read before its provider call.
func (s *ExternalIdentityService) guarded(ctx context.Context, read identity.ExternalAgentIdentity, action identity.ExternalAction, actor string, ref identity.ExternalRef, agentUserRef string, check func(activeBindings int) error) (identity.ExternalAgentIdentity, error) {
	return s.store.TransitionExternalIdentity(ctx, read.ID, func(current identity.ExternalAgentIdentity, activeBindings int) (identity.ExternalAgentIdentity, error) {
		if current.RevocationEpoch != read.RevocationEpoch {
			return identity.ExternalAgentIdentity{}, identity.ErrExternalTransition
		}
		if check != nil {
			if err := check(activeBindings); err != nil {
				return identity.ExternalAgentIdentity{}, err
			}
		}
		return current.Transition(action, actor, ref, agentUserRef, s.now())
	})
}

func (s *ExternalIdentityService) read(ctx context.Context, id string, action identity.ExternalAction) (identity.ExternalAgentIdentity, error) {
	current, err := s.store.GetExternalIdentity(ctx, id)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	if _, err := identity.NextExternalState(current.State, action); err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	return current, nil
}

// Provision asks the provider for the object and moves the record to
// pending_activation. An asynchronous provider that answers Pending leaves
// the record in provisioning; a repeat call is idempotent.
func (s *ExternalIdentityService) Provision(ctx context.Context, id, actor string) (identity.ExternalAgentIdentity, error) {
	current, err := s.read(ctx, id, identity.ExternalActionProvision)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	p, err := s.provider(current.Ref.Provider)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	result, err := p.Provision(ctx, ProvisionSpec{
		ExternalAgentIdentityID: current.ID, TenantProjectID: current.TenantProjectID,
		ApplicationID: current.ApplicationID, Mode: current.Mode, ProviderTenant: current.Ref.ProviderTenant,
		ProviderConfigKey: current.ProviderConfigKey, OwnerRef: current.OwnerRef,
	})
	if err != nil {
		return identity.ExternalAgentIdentity{}, fmt.Errorf("%w: provision", ErrProviderCall)
	}
	if result.Pending {
		return current, nil
	}
	return s.guarded(ctx, current, identity.ExternalActionProvision, actor, result.Ref, result.AgentUserRef, nil)
}

// Activate moves pending_activation → active. It needs an active (approved)
// binding that references the record, read under lock in the write's
// transaction, and the active policy revision to allow the mode.
func (s *ExternalIdentityService) Activate(ctx context.Context, id, actor string) (identity.ExternalAgentIdentity, error) {
	current, err := s.read(ctx, id, identity.ExternalActionActivate)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	policy, err := s.modePolicy(ctx)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	if !policy.AllowsIdentityMode(current.Mode) {
		return identity.ExternalAgentIdentity{}, identity.ErrIdentityModeNotAllowed
	}
	p, err := s.provider(current.Ref.Provider)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	// The binding check runs before the provider call as well, so an
	// activation that cannot succeed never reaches the provider. The check
	// inside the write's transaction is the one that counts.
	count, err := s.store.ActiveBindingCount(ctx, current.TenantProjectID, current.ID)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	if count == 0 {
		return identity.ExternalAgentIdentity{}, identity.ErrNoApprovedBinding
	}
	if err := p.Activate(ctx, current.Ref); err != nil {
		return identity.ExternalAgentIdentity{}, fmt.Errorf("%w: activate", ErrProviderCall)
	}
	return s.guarded(ctx, current, identity.ExternalActionActivate, actor, identity.ExternalRef{}, "", func(activeBindings int) error {
		if activeBindings == 0 {
			return identity.ErrNoApprovedBinding
		}
		return nil
	})
}

// Suspend moves active → suspended, then tells the provider. The record is
// suspended even when the provider call fails or no provider is configured;
// the error then says so.
func (s *ExternalIdentityService) Suspend(ctx context.Context, id, actor string) (identity.ExternalAgentIdentity, error) {
	current, err := s.read(ctx, id, identity.ExternalActionSuspend)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	next, err := s.guarded(ctx, current, identity.ExternalActionSuspend, actor, identity.ExternalRef{}, "", nil)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	p, err := s.provider(next.Ref.Provider)
	if err != nil {
		return next, err
	}
	if err := p.Suspend(ctx, next.Ref); err != nil {
		return next, fmt.Errorf("%w: suspend", ErrProviderCall)
	}
	return next, nil
}

// Resume moves suspended → active. The policy must still allow the mode.
func (s *ExternalIdentityService) Resume(ctx context.Context, id, actor string) (identity.ExternalAgentIdentity, error) {
	current, err := s.read(ctx, id, identity.ExternalActionResume)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	policy, err := s.modePolicy(ctx)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	if !policy.AllowsIdentityMode(current.Mode) {
		return identity.ExternalAgentIdentity{}, identity.ErrIdentityModeNotAllowed
	}
	p, err := s.provider(current.Ref.Provider)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	// Resume returns the record to active, so it needs an approved binding
	// exactly as Activate does: before the provider call, and again inside
	// the write's transaction.
	count, err := s.store.ActiveBindingCount(ctx, current.TenantProjectID, current.ID)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	if count == 0 {
		return identity.ExternalAgentIdentity{}, identity.ErrNoApprovedBinding
	}
	if err := p.Resume(ctx, current.Ref); err != nil {
		return identity.ExternalAgentIdentity{}, fmt.Errorf("%w: resume", ErrProviderCall)
	}
	return s.guarded(ctx, current, identity.ExternalActionResume, actor, identity.ExternalRef{}, "", func(activeBindings int) error {
		if activeBindings == 0 {
			return identity.ErrNoApprovedBinding
		}
		return nil
	})
}

// Retire moves the record to retiring, revokes the provider object and
// completes to retired. A record never provisioned completes without a
// provider call. A failed revocation leaves it retiring; calling Retire
// again retries the revocation.
func (s *ExternalIdentityService) Retire(ctx context.Context, id, actor string) (identity.ExternalAgentIdentity, error) {
	current, err := s.store.GetExternalIdentity(ctx, id)
	if err != nil {
		return identity.ExternalAgentIdentity{}, err
	}
	if current.State != identity.ExternalIdentityRetiring {
		if current, err = s.guarded(ctx, current, identity.ExternalActionRetire, actor, identity.ExternalRef{}, "", nil); err != nil {
			return identity.ExternalAgentIdentity{}, err
		}
	}
	if current.Ref.ID != "" {
		p, err := s.provider(current.Ref.Provider)
		if err != nil {
			return current, err
		}
		if err := p.Revoke(ctx, RevokeSpec{Ref: current.Ref, Reason: "retired"}); err != nil {
			return current, fmt.Errorf("%w: revoke", ErrProviderCall)
		}
	}
	return s.guarded(ctx, current, identity.ExternalActionCompleteRetire, actor, identity.ExternalRef{}, "", nil)
}

// markReplaced moves a record to replaced. The broker calls it when a
// provider answers ErrIdentityReplaced.
func markReplaced(ctx context.Context, store ExternalIdentityStore, id, actor string, now time.Time) error {
	_, err := store.TransitionExternalIdentity(ctx, id, func(current identity.ExternalAgentIdentity, _ int) (identity.ExternalAgentIdentity, error) {
		return current.Transition(identity.ExternalActionReplace, actor, identity.ExternalRef{}, "", now)
	})
	if errors.Is(err, identity.ErrExternalTransition) {
		// Already terminal: nothing to record.
		return nil
	}
	return err
}
