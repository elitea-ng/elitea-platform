package storage

import (
	"context"
	"errors"
	"log/slog"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
)

// runtimeContextStageWorkspace labels a failed `workspace` field resolution.
const runtimeContextStageWorkspace = "workspace_context"

// WorkspaceContextResolver builds the runtime-context `workspace` field for
// a claimed execution (workspaceruntime.ContextResolver). It returns nil and
// no error for an execution that is not a cloud turn of a handed-off thread.
type WorkspaceContextResolver interface {
	ResolveWorkspaceContext(ctx context.Context, claim workspaceruntime.TurnClaim) (*workspaceruntime.RuntimeContext, error)
}

// WithWorkspaceContext lets the service serve the `workspace` field. A nil
// resolver (or a typed nil) leaves it off.
func (s *EliteaClientTokenService) WithWorkspaceContext(resolver WorkspaceContextResolver) *EliteaClientTokenService {
	if s == nil {
		return s
	}
	if typed, ok := resolver.(*workspaceruntime.ContextResolver); ok && typed == nil {
		resolver = nil
	}
	s.workspace = resolver
	return s
}

// RuntimeContextAccept is what the worker listed in
// X-Elitea-Runtime-Context-Accept.
type RuntimeContextAccept struct {
	SecretsHeaderValue bool
	Workspace          bool
}

// ResolveAccepting resolves the runtime context with only the optional
// fields the worker accepted.
//
// The `workspace` field is resolved only when accepted, so an older worker
// never causes the handoff lookup. For a cloud turn the field is part of the
// turn's authority: when it cannot be built (the store is down, or the
// policy recomputed at this claim disables the cloud executor) the whole
// redemption fails, and the turn fails before any tool runs. A worker that
// did not accept the field cannot run a workspace turn either; that is the
// rolling-deploy case and Main does not change what it serves for it.
func (s *EliteaClientTokenService) ResolveAccepting(
	ctx context.Context,
	claim ContentClaim,
	accept RuntimeContextAccept,
) (EliteaClientTokenContext, error) {
	value, authorization, err := s.resolve(ctx, claim)
	if err != nil {
		return EliteaClientTokenContext{}, err
	}
	if !accept.SecretsHeaderValue {
		value.SecretsHeaderValue = ""
	}
	if !accept.Workspace || s.workspace == nil {
		return value, nil
	}
	workspace, err := s.workspace.ResolveWorkspaceContext(ctx, workspaceruntime.TurnClaim{
		ExecutionID:      claim.ExecutionID,
		Generation:       claim.Generation,
		ProjectID:        authorization.ResourceProjectID,
		ActorID:          authorization.ActorID,
		ConversationUUID: authorization.ConversationID,
	})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return EliteaClientTokenContext{}, contextErr
		}
		if errors.Is(err, workspaceruntime.ErrCloudExecutorDisabled) {
			slog.WarnContext(ctx, "the cloud executor is disabled for this project; the cloud turn is refused",
				"execution_id", claim.ExecutionID, "project_id", authorization.ResourceProjectID)
			return EliteaClientTokenContext{}, ErrContentUnauthorized
		}
		return EliteaClientTokenContext{}, runtimeContextUnavailable(runtimeContextStageWorkspace)
	}
	value.Workspace = workspace
	return value, nil
}
