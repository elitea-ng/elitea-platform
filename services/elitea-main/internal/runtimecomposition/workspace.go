package runtimecomposition

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/workspace"
)

// WorkspaceDependencies is the cloud executor's seam (ADR-0034 decisions 7,
// 9, 11 and 14). The handoff service (workspacestore / workspacehandoff,
// adapted by workspaceexecutor) provides all four; until it is composed they
// stay nil and:
//
//   - the WorkspaceStoreService is registered but answers every call
//     UNSUPPORTED_CAPABILITY ("feature unavailable");
//   - the runtime context serves no `workspace` field, so a handed-off
//     thread's turn never starts a workspace run on the worker;
//   - the Local work fence stays shut for handed-off threads (a web turn
//     there would run without the workspace).
//
// All four or none: a worker told it runs a workspace turn must be able to
// fetch and publish, a store without the turn resolver would never be
// reached, and a turn started without the lifecycle would never end. A
// half-composed seam is refused at start.
type WorkspaceDependencies struct {
	Store     workspaceruntime.Store
	Turns     workspaceruntime.TurnResolver
	Policies  workspaceruntime.PolicyEvaluator
	Lifecycle workspaceruntime.TurnLifecycle
	// Egress is how a project host allowlist is enforced (decision 9, the
	// egress proxy). Independent of the four above: its zero value serves
	// every allowlist turn egress mode `unavailable`, which the worker
	// refuses.
	Egress workspaceruntime.Egress
}

var errWorkspaceHalfComposed = errors.New("the workspace store, turn resolver, policy evaluator and turn lifecycle must be composed together")

func (d WorkspaceDependencies) validate() error {
	composed := 0
	for _, present := range []bool{d.Store != nil, d.Turns != nil, d.Policies != nil, d.Lifecycle != nil} {
		if present {
			composed++
		}
	}
	if composed != 0 && composed != 4 {
		return errWorkspaceHalfComposed
	}
	return d.Egress.Validate()
}

// complete: the cloud executor runs turns (every adapter is registered).
// Only then does the Local work fence open for CLOUD_IDLE threads.
func (d WorkspaceDependencies) complete() bool {
	return d.validate() == nil && d.Store != nil
}

// newCurrentAgentStartRepository is the agent start resolver New composes:
// the Local work fence opens for handed-off threads only when the executor
// seam is complete.
func newCurrentAgentStartRepository(pool *pgxpool.Pool, catalogueProjectID int32, workspace WorkspaceDependencies) (*repos.CurrentAgentStartRepository, error) {
	targets, err := repos.NewCurrentAgentStartRepository(pool, catalogueProjectID)
	if err != nil {
		return nil, err
	}
	return targets.WithCloudExecutor(workspace.complete()), nil
}

// workspaceSettlementHooks ends a cloud turn when its execution settles
// (completed, paused for input, failed or cancelled). Hooks run after the
// settlement commits, on their own goroutine.
func workspaceSettlementHooks(workspace WorkspaceDependencies, logger *slog.Logger) []executionapp.SettlementOption {
	if !workspace.complete() {
		return nil
	}
	lifecycle := workspace.Lifecycle
	return []executionapp.SettlementOption{executionapp.WithAfterSettleHooks(
		func(ctx context.Context, proposal executionapp.SettlementProposal, _ executionapp.SettlementReceipt) {
			finishCloudTurn(ctx, lifecycle, proposal.Fence.ExecutionID, "settlement", logger)
		})}
}

func finishCloudTurn(ctx context.Context, lifecycle workspaceruntime.TurnLifecycle, executionID, stage string, logger *slog.Logger) {
	if err := lifecycle.FinishExecution(ctx, executionID); err != nil && logger != nil {
		// The reconciler (the handoff sweeper) ends it on its next pass.
		logger.WarnContext(ctx, "cloud turn finish failed", "stage", stage, "execution_id", executionID, "err", err)
	}
}

// finishingAgentExecutionIngestor ends a cloud turn as soon as its terminal
// result (an answer or a pause for input) is projected, before the
// settlement arrives, so the thread is CLOUD_IDLE when the browser offers the
// next message. The worker publishes the turn's commit before it sends the
// result.
type finishingAgentExecutionIngestor struct {
	next      outputAgentExecutionIngestor
	lifecycle workspaceruntime.TurnLifecycle
	logger    *slog.Logger
}

func (i finishingAgentExecutionIngestor) IngestAgent(ctx context.Context, frame outputapp.AgentExecutionFrame) (outputapp.ProjectionOutcome, error) {
	outcome, err := i.next.IngestAgent(ctx, frame)
	if err == nil {
		finishCloudTurn(ctx, i.lifecycle, frame.Fence.ExecutionID, "projection", i.logger)
	}
	return outcome, err
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
