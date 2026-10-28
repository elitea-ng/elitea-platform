package storage

import (
	"context"
	"errors"
	"log/slog"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
)

// runtimeContextStageWorkspace labels a failed `workspace` field resolution.
const runtimeContextStageWorkspace = "workspace_context"

// WorkspaceContextResolver builds the runtime-context `workspace` field for
// a claimed execution (workspaceruntime.ContextResolver). It returns nil and
// no error for an execution that is not a cloud turn of a handed-off thread.
type WorkspaceContextResolver interface {
	ResolveWorkspaceContext(ctx context.Context, claim workspaceruntime.TurnClaim) (*workspaceruntime.RuntimeContext, error)
	// RefusesUnaccepted: the claim is a turn of a handed-off thread, which a
	// worker that cannot read the `workspace` field must not run.
	RefusesUnaccepted(ctx context.Context, claim workspaceruntime.TurnClaim) (bool, error)
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
	// GovernedRun is not a worker field: the content server sets it for a
	// run pinned to the governed profile. A governed SCHEDULED run is then
	// refused the project system token (#1122): its authority is its
	// mandate, and no system-token fallback exists.
	GovernedRun bool
}

// ResolveAccepting resolves the runtime context with only the optional
// fields the worker accepted.
//
// The `workspace` field is resolved only when accepted. For a cloud turn the
// field is part of the turn's authority: when it cannot be built (the store
// is down, the policy recomputed at this claim disables the cloud executor,
// or the thread is not the cloud's for this turn) the whole redemption
// fails, and the turn fails before any tool runs. Resolving it STARTS the
// cloud turn (workspaceruntime.TurnResolver).
//
// A worker that did not accept the field cannot run a workspace turn, so a
// turn of a handed-off thread is refused to it (the rolling-deploy case): it
// would otherwise run without the files and be recorded as a cloud turn
// with no commit. That read changes nothing.
func (s *EliteaClientTokenService) ResolveAccepting(
	ctx context.Context,
	claim ContentClaim,
	accept RuntimeContextAccept,
) (EliteaClientTokenContext, error) {
	value, authorization, err := s.resolve(ctx, claim, accept.SecretsHeaderValue, accept.GovernedRun)
	if err != nil {
		return EliteaClientTokenContext{}, err
	}
	if s.workspace == nil {
		return value, nil
	}
	turnClaim := workspaceruntime.TurnClaim{
		ExecutionID:      claim.ExecutionID,
		Generation:       claim.Generation,
		ProjectID:        authorization.ResourceProjectID,
		ActorID:          authorization.ActorID,
		ConversationUUID: authorization.ConversationID,
	}
	if !accept.Workspace {
		refused, err := s.workspace.RefusesUnaccepted(ctx, turnClaim)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return EliteaClientTokenContext{}, contextErr
			}
			return EliteaClientTokenContext{}, runtimeContextUnavailable(runtimeContextStageWorkspace)
		}
		if refused {
			slog.WarnContext(ctx, "a worker without workspace runs claimed a turn of a handed-off thread; refused",
				"execution_id", claim.ExecutionID, "project_id", authorization.ResourceProjectID)
			return EliteaClientTokenContext{}, ErrContentUnauthorized
		}
		return value, nil
	}
	workspace, err := s.workspace.ResolveWorkspaceContext(ctx, turnClaim)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return EliteaClientTokenContext{}, contextErr
		}
		if errors.Is(err, workspaceruntime.ErrCloudExecutorDisabled) {
			slog.WarnContext(ctx, "the cloud executor is disabled for this project; the cloud turn is refused",
				"execution_id", claim.ExecutionID, "project_id", authorization.ResourceProjectID)
			return EliteaClientTokenContext{}, ErrContentUnauthorized
		}
		if errors.Is(err, workspaceruntime.ErrNotCloudTurn) {
			slog.WarnContext(ctx, "the thread is not the cloud executor's for this turn; the cloud turn is refused",
				"execution_id", claim.ExecutionID, "project_id", authorization.ResourceProjectID)
			return EliteaClientTokenContext{}, ErrContentUnauthorized
		}
		return EliteaClientTokenContext{}, runtimeContextUnavailable(runtimeContextStageWorkspace)
	}
	value.Workspace = workspace
	return value, nil
}
