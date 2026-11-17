package api

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"

	appgrants "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationgrants"
	platformauth "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// delegationWiring is the delegation grant store, composed with the agent
// lifecycle store that records the governed grant's change request, and the
// use case the API serves (ADR-0044 amendment 1, delegation epic 2156 story
// S2b).
type delegationWiring struct {
	store   *dbrepos.DelegationGrants
	service *appgrants.Service
}

// composeDelegationGrants builds the grant store over the pool and composes it
// with the lifecycle store: a governed grant records its change request
// through the lifecycle store, and the lifecycle store's final approval
// approves the grant through this one. governed reports whether the governed
// profile is on; nil means it never is. It answers nil when the stores cannot
// be built (no pool).
func composeDelegationGrants(cfg RouterConfig, lifecycle *dbrepos.AgentLifecycle, resolver platformauth.PermissionResolver) *delegationWiring {
	if cfg.Pool == nil || lifecycle == nil || resolver == nil {
		return nil
	}
	// A construction error leaves the routes and the attach hook out: it is
	// logged, because an absent surface reads as a quiet one.
	fail := func(step string, err error) *delegationWiring {
		slog.Error("delegation grants are not composed", "step", step, "error", err)
		return nil
	}
	appender, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return fail("audit appender", err)
	}
	sources, err := dbrepos.NewWorkerEgressSources(cfg.Pool)
	if err != nil {
		return fail("egress sources", err)
	}
	callees, err := dbrepos.NewDelegationCallees(cfg.Pool, sources)
	if err != nil {
		return fail("callee reader", err)
	}
	access, err := dbrepos.NewDelegationAccess(permissionChecker{resolver: resolver})
	if err != nil {
		return fail("approver access", err)
	}
	governed := cfg.DelegationGoverned
	if governed == nil {
		governed = func(context.Context) bool { return false }
	}
	store, err := dbrepos.NewDelegationGrants(cfg.Pool, appender,
		dbrepos.WithDelegationChanges(lifecycle), dbrepos.WithDelegationAccess(access),
		dbrepos.WithDelegationCallees(callees), dbrepos.WithDelegationGoverned(governed))
	if err != nil {
		return fail("grant store", err)
	}
	lifecycle.ComposeDelegationApplier(store)
	service, err := appgrants.New(store, callees, lifecycle, access, governed)
	if err != nil {
		return fail("service", err)
	}
	return &delegationWiring{store: store, service: service}
}

// permissionChecker answers what the REST routes' RBAC middleware asks: the
// person's resolved permissions in the project, in the default mode. A person
// the resolver refuses (no membership, a suspended user) holds nothing.
type permissionChecker struct {
	resolver platformauth.PermissionResolver
}

func (c permissionChecker) HasProjectPermission(ctx context.Context, userID, projectID int64, permission string) (bool, error) {
	id := strconv.FormatInt(userID, 10)
	resolution, err := c.resolver.ResolvePermissions(ctx,
		platformauth.User{ID: id, UserID: id}, platformauth.PermissionModeDefault, strconv.FormatInt(projectID, 10))
	if err != nil {
		if errors.Is(err, platformauth.ErrPermissionDenied) {
			return false, nil
		}
		return false, err
	}
	return slices.Contains(resolution.Permissions, permission), nil
}
