package main

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation/runtimeplane"
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
	return runtimeplane.New(store, start, cancel, steps, content, logger)
}
