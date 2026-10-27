package credentialgrant

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Store persists grants. Every write runs in one transaction that also
// appends a securityaudit.TypeGrantChanged event; an append error rolls the
// change back.
//
// CreateGrant refuses an agent binding, credential reference or target that
// is not in the grant's tenant project, and a credential reference bound to
// another target (ErrReferenceNotUsable).
//
// TransitionGrant locks the row of id in tenantProjectID (SELECT … FOR
// UPDATE), passes it to apply and writes apply's result in the same
// transaction. A row of another tenant project is ErrGrantNotFound.
type Store interface {
	CreateGrant(ctx context.Context, grant CredentialUseGrant) (CredentialUseGrant, error)
	ListGrants(ctx context.Context, filter ListFilter) ([]CredentialUseGrant, error)
	TransitionGrant(ctx context.Context, tenantProjectID int64, id string, apply func(CredentialUseGrant) (CredentialUseGrant, error)) (CredentialUseGrant, error)
}

// Service is the grant decision surface the admin API calls.
type Service struct {
	store Store
	now   func() time.Time
	newID func() (string, error)
}

// ServiceConfig is the service's dependencies. Store is required.
type ServiceConfig struct {
	Store Store
	Now   func() time.Time
	NewID func() (string, error)
}

// NewService builds the service. A missing store is refused.
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("credential grant: a store is required")
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
	return &Service{store: cfg.Store, now: now, newID: newID}, nil
}

// ProposeGrant writes a proposed grant. It authorises nothing until another
// user approves it.
func (s *Service) ProposeGrant(ctx context.Context, proposal Proposal, proposedBy string) (CredentialUseGrant, error) {
	id, err := s.newID()
	if err != nil {
		return CredentialUseGrant{}, fmt.Errorf("new grant id: %w", err)
	}
	grant, err := NewGrant(id, proposal, proposedBy, s.now())
	if err != nil {
		return CredentialUseGrant{}, err
	}
	return s.store.CreateGrant(ctx, grant)
}

// ListGrants lists the grants of one tenant project.
func (s *Service) ListGrants(ctx context.Context, filter ListFilter) ([]CredentialUseGrant, error) {
	if filter.TenantProjectID <= 0 {
		return nil, fmt.Errorf("%w: tenant_project_id must be positive", ErrInvalidGrant)
	}
	return s.store.ListGrants(ctx, filter)
}

// TransitionGrant applies action by actor to grant id of tenantProjectID.
// An approval of an expired grant is refused: it could never be used.
func (s *Service) TransitionGrant(ctx context.Context, tenantProjectID int64, id string, action Action, actor string) (CredentialUseGrant, error) {
	return s.store.TransitionGrant(ctx, tenantProjectID, id, func(current CredentialUseGrant) (CredentialUseGrant, error) {
		now := s.now()
		if action == ActionApprove && current.ExpiresAt != nil && !now.Before(*current.ExpiresAt) {
			return CredentialUseGrant{}, fmt.Errorf("%w: the grant has expired", ErrInvalidTransition)
		}
		return current.Transition(action, actor, now)
	})
}
