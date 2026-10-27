package authzcompose

import (
	"context"
	"errors"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// RBACGate is check 2a: the subject holds, in the tenant project, the
// permission of the requested capability AND every extra permission.
// Resolution is the platform's (legacyrbac) in default mode; the comparison
// is middleware.HoldsAllPermissions, never the any-overlap route check.
type RBACGate struct {
	resolver auth.PermissionResolver
	extra    []string
}

// NewRBACGate builds the gate over resolver. extra lists permissions every
// decision requires besides the capability's own.
func NewRBACGate(resolver auth.PermissionResolver, extra ...string) (*RBACGate, error) {
	if resolver == nil {
		return nil, errors.New("authzcompose: rbac gate needs a permission resolver")
	}
	return &RBACGate{resolver: resolver, extra: append([]string(nil), extra...)}, nil
}

// Name implements Gate.
func (*RBACGate) Name() string { return "rbac" }

// Evaluate implements Gate. A refusal by the resolver is a deny; any other
// resolver error is a gate error.
func (g *RBACGate) Evaluate(ctx context.Context, r OperationRequest) (GateResult, error) {
	permission := credentialgrant.Permission(r.Action.Capability)
	if permission == "" {
		return GateResult{Reasons: []ReasonCode{ReasonCapabilityUnknown}}, nil
	}
	if r.Subject.UserID <= 0 || r.Context.TenantProjectID <= 0 {
		return GateResult{Reasons: []ReasonCode{ReasonSubjectMissing}}, nil
	}
	userID := strconv.FormatInt(r.Subject.UserID, 10)
	resolution, err := g.resolver.ResolvePermissions(ctx,
		auth.User{ID: userID, UserID: userID},
		auth.PermissionModeDefault, strconv.FormatInt(r.Context.TenantProjectID, 10))
	if errors.Is(err, auth.ErrPermissionDenied) {
		return GateResult{Reasons: []ReasonCode{ReasonPermissionMissing}}, nil
	}
	if err != nil {
		return GateResult{}, err
	}
	required := append([]string{permission}, g.extra...)
	if !middleware.HoldsAllPermissions(required, resolution.Permissions) {
		return GateResult{Reasons: []ReasonCode{ReasonPermissionMissing}}, nil
	}
	return GateResult{Permit: true}, nil
}
