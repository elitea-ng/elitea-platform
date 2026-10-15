package runtimecomposition

import (
	"log/slog"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
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
type WorkspaceDependencies struct {
	Store workspaceruntime.Store
}

func (d WorkspaceDependencies) validate() error {
	return nil
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
