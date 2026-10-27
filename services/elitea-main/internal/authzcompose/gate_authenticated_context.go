package authzcompose

import (
	"context"
	"errors"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// AuthSourcePolicy answers whether a policy revision admits an
// authentication source.
type AuthSourcePolicy interface {
	AllowsAuthSource(policyRevisionID string, source auth.AuthenticationSource) bool
}

// DefaultAuthSources admits API keys, access tokens and browser sessions
// under every revision, and never the development bypass. Policy document
// v1 has no auth-source list; a revision-backed policy replaces this when it
// does.
type DefaultAuthSources struct{}

// AllowsAuthSource implements AuthSourcePolicy.
func (DefaultAuthSources) AllowsAuthSource(_ string, source auth.AuthenticationSource) bool {
	switch source {
	case auth.AuthenticationSourceAPIKey, auth.AuthenticationSourceToken, auth.AuthenticationSourceSession:
		return true
	}
	return false
}

// AuthenticatedContextGate is check 1: the principal and the caller workload
// are present and the authentication source is allowed by the revision.
type AuthenticatedContextGate struct{ policy AuthSourcePolicy }

// NewAuthenticatedContextGate builds the gate.
func NewAuthenticatedContextGate(policy AuthSourcePolicy) (*AuthenticatedContextGate, error) {
	if policy == nil {
		return nil, errors.New("authzcompose: authenticated context gate needs an auth source policy")
	}
	return &AuthenticatedContextGate{policy: policy}, nil
}

// Name implements Gate.
func (*AuthenticatedContextGate) Name() string { return "authenticated_context" }

// Evaluate implements Gate.
func (g *AuthenticatedContextGate) Evaluate(_ context.Context, r OperationRequest) (GateResult, error) {
	var reasons []ReasonCode
	if r.Subject.PrincipalRef == "" {
		reasons = append(reasons, ReasonSubjectMissing)
	}
	if r.Subject.WorkloadID == "" {
		reasons = append(reasons, ReasonWorkloadMissing)
	}
	if !g.policy.AllowsAuthSource(r.Context.PolicyRevisionID, r.Subject.AuthSource) {
		reasons = append(reasons, ReasonAuthSourceNotAllowed)
	}
	return GateResult{Permit: len(reasons) == 0, Reasons: reasons}, nil
}
