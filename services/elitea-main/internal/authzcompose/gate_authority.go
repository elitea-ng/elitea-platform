package authzcompose

import (
	"context"
	"errors"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

// DelegationReader reads an interactive delegation. No row is
// mandate.ErrDelegationNotUsable.
type DelegationReader interface {
	Delegation(ctx context.Context, id string) (mandate.Delegation, error)
}

// NoDelegations answers "not usable" for every id. It is the wiring until
// delegation custody exists (#1117).
type NoDelegations struct{}

// Delegation always refuses.
func (NoDelegations) Delegation(context.Context, string) (mandate.Delegation, error) {
	return mandate.Delegation{}, mandate.ErrDelegationNotUsable
}

// AuthorityGate is check 6: the mandate or delegation is active, started
// and unexpired, at the epoch the run carries, for this tenant project and
// agent binding, and for this subject.
//
// Subject rule: a delegated_background mandate names its user, which must
// be the subject's. An autonomous mandate names no user, so it constrains
// none. A delegation names its IdP subject, which must equal the subject's
// ExternalSubjectID.
type AuthorityGate struct {
	mandates    mandate.Reader
	delegations DelegationReader
	now         func() time.Time
}

// NewAuthorityGate builds the gate. now may be nil (wall clock).
func NewAuthorityGate(mandates mandate.Reader, delegations DelegationReader, now func() time.Time) (*AuthorityGate, error) {
	if mandates == nil || delegations == nil {
		return nil, errors.New("authzcompose: authority gate needs mandate and delegation readers")
	}
	if now == nil {
		now = time.Now
	}
	return &AuthorityGate{mandates: mandates, delegations: delegations, now: now}, nil
}

// Name implements Gate.
func (*AuthorityGate) Name() string { return "authority" }

// Evaluate implements Gate. An unknown kind or a missing id is a deny
// (gate D4: unknown authority is denied).
func (g *AuthorityGate) Evaluate(ctx context.Context, r OperationRequest) (GateResult, error) {
	if r.Authority.ID == "" {
		return GateResult{Reasons: []ReasonCode{ReasonAuthorityMissing}}, nil
	}
	switch r.Authority.Kind {
	case AuthorityMandate:
		return g.mandate(ctx, r)
	case AuthorityDelegation:
		return g.delegation(ctx, r)
	}
	return GateResult{Reasons: []ReasonCode{ReasonAuthorityUnknown}}, nil
}

func (g *AuthorityGate) mandate(ctx context.Context, r OperationRequest) (GateResult, error) {
	m, err := snapshotMandate(ctx, g.mandates, r.Authority.ID)
	if errors.Is(err, mandate.ErrMandateNotFound) {
		return GateResult{Reasons: []ReasonCode{ReasonAuthorityNotActive}}, nil
	}
	if err != nil {
		return GateResult{}, err
	}
	now := g.now()
	result := GateResult{Epochs: map[string]int64{"mandate": m.RevocationEpoch}}
	if m.EffectiveState(now) != mandate.StateActive || now.Before(m.NotBefore) {
		result.Reasons = append(result.Reasons, ReasonAuthorityNotActive)
	}
	if m.RevocationEpoch != r.Authority.Epoch {
		result.Reasons = append(result.Reasons, ReasonAuthorityEpochStale)
	}
	if m.TenantProjectID != r.Context.TenantProjectID || m.AgentBindingID != r.Agent.BindingID {
		result.Reasons = append(result.Reasons, ReasonAuthorityScopeMismatch)
	}
	if m.SubjectUserID != nil && *m.SubjectUserID != r.Subject.UserID {
		result.Reasons = append(result.Reasons, ReasonAuthoritySubjectDiffer)
	}
	result.Permit = len(result.Reasons) == 0
	return result, nil
}

func (g *AuthorityGate) delegation(ctx context.Context, r OperationRequest) (GateResult, error) {
	d, err := g.delegations.Delegation(ctx, r.Authority.ID)
	if errors.Is(err, mandate.ErrDelegationNotUsable) {
		return GateResult{Reasons: []ReasonCode{ReasonAuthorityNotActive}}, nil
	}
	if err != nil {
		return GateResult{}, err
	}
	now := g.now()
	result := GateResult{Epochs: map[string]int64{"delegation": d.RevocationEpoch}}
	if d.State != mandate.StateActive || !now.Before(d.ExpiresAt) {
		result.Reasons = append(result.Reasons, ReasonAuthorityNotActive)
	}
	if d.RevocationEpoch != r.Authority.Epoch {
		result.Reasons = append(result.Reasons, ReasonAuthorityEpochStale)
	}
	if d.ID != r.Authority.ID || d.TenantProjectID != r.Context.TenantProjectID || d.AgentBindingID != r.Agent.BindingID {
		result.Reasons = append(result.Reasons, ReasonAuthorityScopeMismatch)
	}
	if d.SubjectObjectID == "" || d.SubjectObjectID != r.Subject.ExternalSubjectID {
		result.Reasons = append(result.Reasons, ReasonAuthoritySubjectDiffer)
	}
	result.Permit = len(result.Reasons) == 0
	return result, nil
}
