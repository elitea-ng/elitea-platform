package repos

import (
	"context"
	"errors"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

// DelegationToolkitPermission is the project permission that lets a person see
// and use the toolkits of a project (the toolkit list's permission).
const DelegationToolkitPermission = "models.applications.tools.list"

// DelegationPermissionChecker answers whether a person holds a permission in a
// project, resolved as the REST routes resolve it (default mode).
type DelegationPermissionChecker interface {
	HasProjectPermission(ctx context.Context, userID, projectID int64, permission string) (bool, error)
}

// DelegationAccess resolves what an approver holds access to (ADR-0044
// amendment 1, A1.2 rule 2): the toolkits of the project, by the permission
// that lets them use a toolkit, and the credential references of the project,
// by the governed credential use permission (credentialgrant.PermissionUse).
// A built-in toolkit with no row holds no secret and is open to every approver.
type DelegationAccess struct {
	permissions DelegationPermissionChecker
}

// NewDelegationAccess builds the resolver.
func NewDelegationAccess(permissions DelegationPermissionChecker) (*DelegationAccess, error) {
	if permissions == nil {
		return nil, errors.New("delegation access: a permission checker is required")
	}
	return &DelegationAccess{permissions: permissions}, nil
}

var _ DelegationAccessResolver = (*DelegationAccess)(nil)

type approverAccess struct{ toolkits, credentials bool }

func (a approverAccess) HoldsToolkit(key string) bool {
	if strings.HasPrefix(key, "builtin:") {
		return true
	}
	return a.toolkits
}

func (a approverAccess) HoldsCredential(string) bool { return a.credentials }

// AccessFor reads the two permissions of the person in the project.
func (r *DelegationAccess) AccessFor(ctx context.Context, projectID, userID int64) (dg.Access, error) {
	toolkits, err := r.permissions.HasProjectPermission(ctx, userID, projectID, DelegationToolkitPermission)
	if err != nil {
		return nil, err
	}
	credentials, err := r.permissions.HasProjectPermission(ctx, userID, projectID, credentialgrant.PermissionUse)
	if err != nil {
		return nil, err
	}
	return approverAccess{toolkits: toolkits, credentials: credentials}, nil
}
