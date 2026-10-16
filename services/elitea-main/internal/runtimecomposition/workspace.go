package runtimecomposition

import (
	"errors"
	"log/slog"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/workspace"
)

// WorkspaceDependencies is the cloud executor's seam (ADR-0034 decisions 9,
// 11 and 14). The handoff service (workspacestore / workspacehandoff)
// provides all three; until it is composed they stay nil and:
//
//   - the WorkspaceStoreService is registered but answers every call
//     UNSUPPORTED_CAPABILITY ("feature unavailable");
//   - the runtime context serves no `workspace` field, so a handed-off
//     thread's turn never starts a workspace run on the worker.
//
// All three or none: a worker told it runs a workspace turn must be able to
// fetch and publish, and a store without the turn resolver would never be
// reached. A half-composed seam is refused at start.
type WorkspaceDependencies struct {
	Store    workspaceruntime.Store
	Turns    workspaceruntime.TurnResolver
	Policies workspaceruntime.PolicyEvaluator
	// Egress is how a project host allowlist is enforced (decision 9, the
	// egress proxy). Independent of the three above: its zero value serves
	// every allowlist turn egress mode `unavailable`, which the worker
	// refuses.
	Egress workspaceruntime.Egress
}

var errWorkspaceHalfComposed = errors.New("the workspace store, turn resolver and policy evaluator must be composed together")

func (d WorkspaceDependencies) validate() error {
	if (d.Store == nil) != (d.Turns == nil) || (d.Turns == nil) != (d.Policies == nil) {
		return errWorkspaceHalfComposed
	}
	return d.Egress.Validate()
}

func newWorkspaceStoreServer(authorizer workspace.WorkloadAuthorizer, claims workspace.ClaimVerifier, dependencies Dependencies) (*workspace.Server, error) {
	if err := dependencies.Workspace.validate(); err != nil {
		return nil, err
	}
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return workspace.NewServer(authorizer, claims, dependencies.Workspace.Store, logger)
}

// newWorkspaceContextResolver returns a nil interface (never a typed nil)
// when the seam is not composed.
func newWorkspaceContextResolver(dependencies Dependencies) storage.WorkspaceContextResolver {
	resolver := workspaceruntime.NewContextResolver(dependencies.Workspace.Turns, dependencies.Workspace.Policies)
	if resolver == nil {
		return nil
	}
	return resolver.WithEgress(dependencies.Workspace.Egress, dependencies.Logger)
}
