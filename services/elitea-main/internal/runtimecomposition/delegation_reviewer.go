package runtimecomposition

import (
	"log/slog"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/delegationnotice"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// delegationReviewerFor builds what stamps a viewer's review of a refused or
// narrowed delegation onto the run's event stream (ADR-0044 Amendment 1, #2161):
// the project's edit permission, and the agent's owners to ask. It answers nil
// (an interface that holds nothing, never a typed nil) when a side is missing,
// so notices are served unstamped and clients show them without Review
// delegation. The cause is logged: an absent surface reads as a quiet one.
func delegationReviewerFor(dependencies Dependencies) delegationnotice.Reviewer {
	if dependencies.PermissionResolver == nil || dependencies.ReplayPool == nil {
		slog.Warn("delegation notices are served unstamped: no permission resolver or replay pool")
		return nil
	}
	editors, err := repos.NewDelegationEditors(dependencies.ReplayPool)
	if err != nil {
		slog.Error("delegation notices are served unstamped", "error", err)
		return nil
	}
	service := delegationnotice.NewService(permissionCheckerAdapter{resolver: dependencies.PermissionResolver}, editors)
	if service == nil {
		return nil
	}
	return service
}
