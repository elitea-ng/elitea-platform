package main

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentexecution"
	v2evaluation "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation/runtimeplane"
	v2runevidence "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/runevidence"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// composeEvalAgentRunner builds the runner of the runtime_plane evaluation
// mode (issue 2011 step B). It is the chat start and stop use cases of the
// composed runtime plane, so an evaluation case is admitted exactly as a chat
// turn is. It answers nil when the runtime plane is not composed (no start or
// stop use case) or the run steps cannot be composed; the orchestrator then
// errors a runtime_plane run with a reason instead of running it some other
// way.
//
// The step record and its content store are optional: without an object store
// a case still has its steps, and no tool outputs.
func composeEvalAgentRunner(
	pool *pgxpool.Pool,
	objectStore storage.ObjectStore,
	start agentexecutionapi.StartUseCase,
	cancel agentexecutionapi.CurrentAgentCanceller,
	usage v2evaluation.UsageReader,
	permissions auth.PermissionResolver,
	logger *slog.Logger,
) *runtimeplane.Runner {
	if pool == nil || start == nil || cancel == nil {
		return nil
	}
	store, err := runtimeplane.NewPostgresStore(pool, logger)
	if err != nil {
		return nil
	}
	var steps runtimeplane.Evidence
	if runSteps, composeErr := dbrepos.ComposeRunSteps(pool); composeErr == nil {
		steps = runSteps
	} else {
		logger.Warn("evaluation: the run step record is not composed; runtime_plane cases have no trajectory", "error", composeErr)
	}
	var content runtimeplane.Content
	if objectStore != nil {
		if objects, contentErr := storage.NewRunEvidenceContent(objectStore); contentErr == nil {
			content = objects
		}
	}
	runner := runtimeplane.New(store, start, cancel, steps, content, logger).WithUsage(usage)
	if permissions != nil {
		runner.WithAuthorizer(evidencePermission{permissions: permissions})
	}
	return runner
}

// evidencePermission answers whether the author of an evaluation run holds
// runs.evidence.read in the project, resolved as the evidence route resolves
// it. A failed resolution is an error, which the runner reads as no.
type evidencePermission struct{ permissions auth.PermissionResolver }

func (p evidencePermission) MayReadRunEvidence(ctx context.Context, userID, projectID int64) (bool, error) {
	id := strconv.FormatInt(userID, 10)
	resolution, err := p.permissions.ResolvePermissions(ctx, auth.User{ID: id, UserID: id},
		auth.PermissionModeDefault, strconv.FormatInt(projectID, 10))
	if err != nil {
		return false, err
	}
	for _, permission := range resolution.Permissions {
		if permission == v2runevidence.Permission {
			return true, nil
		}
	}
	return false, nil
}
