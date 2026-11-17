package repos

import (
	"context"
	"errors"
	"strconv"
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

// DelegationVisibility reads what the project hides from a person and which
// credential references it stands behind: the per-toolkit and per-credential
// half of the approver's access. WorkerEgressSources is the production reader.
type DelegationVisibility interface {
	// HiddenToolkitsForUser lists the toolkits of the project the folder access
	// rules hide from the user.
	HiddenToolkitsForUser(ctx context.Context, projectID, userID int64) ([]int64, error)
	// UsableCredentialReferencesInProject lists the credential references an
	// active ADR-0039 use grant of the project covers.
	UsableCredentialReferencesInProject(ctx context.Context, projectID int64) ([]string, error)
}

var _ DelegationVisibility = (*WorkerEgressSources)(nil)

// DelegationAccess resolves what an approver holds access to (ADR-0044
// amendment 1, A1.2 rule 2), item by item:
//
//   - a toolkit is held when the person holds the permission that lets them use
//     the project's toolkits AND the project's folder access rules do not hide
//     that toolkit from them (the overlay the toolkit reader applies);
//   - a credential reference is held when the person holds the governed
//     credential use permission, and, under the governed profile, an active
//     ADR-0039 use grant of the project covers the reference (a reference no
//     grant covers can never be used, so nobody can stand behind it).
//
// A built-in toolkit with no row holds no secret and is open to every approver.
type DelegationAccess struct {
	permissions DelegationPermissionChecker
	visibility  DelegationVisibility
	governed    func(context.Context) bool
}

// DelegationAccessOption configures NewDelegationAccess.
type DelegationAccessOption func(*DelegationAccess)

// WithDelegationVisibility adds the per-toolkit and per-credential checks.
// governed reports whether the governed profile is on (nil: never). Without
// it the resolver answers by the two project permissions alone.
func WithDelegationVisibility(visibility DelegationVisibility, governed func(context.Context) bool) DelegationAccessOption {
	return func(a *DelegationAccess) {
		a.visibility = visibility
		a.governed = governed
	}
}

// NewDelegationAccess builds the resolver.
func NewDelegationAccess(permissions DelegationPermissionChecker, options ...DelegationAccessOption) (*DelegationAccess, error) {
	if permissions == nil {
		return nil, errors.New("delegation access: a permission checker is required")
	}
	access := &DelegationAccess{permissions: permissions}
	for _, option := range options {
		option(access)
	}
	return access, nil
}

var _ DelegationAccessResolver = (*DelegationAccess)(nil)

type approverAccess struct {
	toolkits, credentials bool
	// hiddenToolkits are toolkit ids the person may not see (their folder is
	// closed to them); usableReferences is nil when no per-reference check
	// applies.
	hiddenToolkits   map[int64]bool
	usableReferences map[string]bool
}

// HoldsToolkit fails closed: only a built-in toolkit, or a well-formed
// `toolkit:<id>` the folder access rules do not hide, is held.
func (a approverAccess) HoldsToolkit(key string) bool {
	if strings.HasPrefix(key, "builtin:") {
		return true
	}
	if !a.toolkits || !strings.HasPrefix(key, "toolkit:") {
		return false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(key, "toolkit:"), 10, 64)
	return err == nil && !a.hiddenToolkits[id]
}

// HoldsCredential compares references in lower case: a toolkit may spell its
// reference id in upper case, the use grant table answers canonical lower case.
//
// Under the governed profile the covering use grants are the project's, not
// the toolkit's (the grant carries no toolkit for a reference), so a reference
// some toolkit's grant covers is held by an approver of any toolkit that names
// it. The broker still decides the exact grant at every call.
func (a approverAccess) HoldsCredential(reference string) bool {
	if !a.credentials {
		return false
	}
	return a.usableReferences == nil || a.usableReferences[strings.ToLower(reference)]
}

// AccessFor reads the person's two permissions in the project, and what the
// project hides from them.
func (r *DelegationAccess) AccessFor(ctx context.Context, projectID, userID int64) (dg.Access, error) {
	toolkits, err := r.permissions.HasProjectPermission(ctx, userID, projectID, DelegationToolkitPermission)
	if err != nil {
		return nil, err
	}
	credentials, err := r.permissions.HasProjectPermission(ctx, userID, projectID, credentialgrant.PermissionUse)
	if err != nil {
		return nil, err
	}
	access := approverAccess{toolkits: toolkits, credentials: credentials}
	if r.visibility == nil {
		return access, nil
	}
	if toolkits {
		hidden, err := r.visibility.HiddenToolkitsForUser(ctx, projectID, userID)
		if err != nil {
			return nil, err
		}
		access.hiddenToolkits = make(map[int64]bool, len(hidden))
		for _, id := range hidden {
			access.hiddenToolkits[id] = true
		}
	}
	if credentials && r.governed != nil && r.governed(ctx) {
		references, err := r.visibility.UsableCredentialReferencesInProject(ctx, projectID)
		if err != nil {
			return nil, err
		}
		access.usableReferences = make(map[string]bool, len(references))
		for _, reference := range references {
			access.usableReferences[strings.ToLower(reference)] = true
		}
	}
	return access, nil
}
