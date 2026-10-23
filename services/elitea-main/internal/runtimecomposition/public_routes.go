package runtimecomposition

import (
	"context"
	"errors"
	"net/http"
	"time"

	agentexecutionapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentexecution"
	configurationapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	executionapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/executions"
	indexingapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	toolkitrun "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/toolkitrun"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	discovery "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitdiscovery"
	toolkitexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

type PublicRoutes struct {
	NodeRecovery agentexecutionapi.CurrentNodeRecoveryUseCase
	// ExecutionInterrupts is the per-interrupt HITL ledger. It is composed
	// whenever agent execution dispatch is (no separate flag), so the routes
	// exist exactly when the ledger can be written.
	ExecutionInterrupts agentexecutionapi.ExecutionInterruptUseCase
	Validation          http.Handler
	ExecutionEvents     http.Handler
	// IndexStart is composed only when the complete index control/data plane is
	// enabled. Main binds it to the current route's existing authentication and
	// project-RBAC middleware before mounting it.
	IndexStart indexingapi.StartUseCase
	// AgentStart owns only the initial, same-project configured-application
	// turn. Unsupported advanced turns stay on the current Socket.IO path.
	AgentStart agentexecutionapi.StartUseCase
	// ToolkitCallTool runs ONE tool of ONE saved toolkit, synchronously
	// (#340/#616). It is composed only with index ingest, because the worker
	// that runs an index is the worker that can run that toolkit's tools.
	ToolkitCallTool  toolkitrun.UseCase
	ToolkitDiscovery discovery.UseCase
	// AgentCancel preserves the current DELETE contract while atomically
	// cancelling the exact durable execution and its current chat projection.
	AgentCancel agentexecutionapi.CurrentAgentCanceller
	// ToolkitExecuteRead runs one explicitly MCP-exposed, selected, non-sensitive
	// toolkit operation through the durable Rust worker path. It is nil whenever
	// the agent/runtime plane is disabled.
	ToolkitExecuteRead *toolkitexecutionapp.CurrentReadToolExecutionService
	// AgentTaskStatus is the READ half of the legacy application_task surface.
	// It is composed beside AgentCancel because the two resolve the same
	// durable execution from the same response message and share one ownership
	// predicate; a reader that outlived its canceller would disclose the state
	// of runs the caller may not stop.
	AgentTaskStatus agentexecutionapi.CurrentAgentTaskStatusReader
	// IndexCancel preserves the current UI DELETE contract while selecting only
	// Go-owned execution IDs at the compatibility edge.
	IndexCancel indexingapi.CurrentIndexCanceller
	// IndexMeta reads the current raw-array UI contract from the index
	// registry.
	IndexMeta indexingapi.CurrentIndexMetaReader
	// IndexMetaDelete preserves the current synchronous two-commit contract. It
	// is exposed only with the coordinated Go index owner and receives
	// production authentication and project RBAC at the Main composition edge.
	IndexMetaDelete indexingapi.CurrentIndexMetaDeleter
	// IndexConfiguration persists one index's configuration without running
	// it. It shares IndexMetaDelete's exposure rule: the Go index owner must
	// be composed, because the field it writes is the same field the run path
	// and the scheduler read.
	IndexConfiguration indexingapi.CurrentIndexConfigurationSaver
	// IndexScheduleUpdate/Delete preserve the current UI contracts and are
	// exposed only when the distributed Go schedule owner is enabled.
	IndexScheduleUpdate indexingapi.CurrentIndexScheduleUpdater
	IndexScheduleDelete indexingapi.CurrentIndexScheduleDeleter
	// ClientApplicationVersions serves the desktop's resolved definition
	// (ADR-0029 decision 5a). It is built from the agent plane's own freezer
	// and nested-version reader, so it is nil exactly when that plane is.
	ClientApplicationVersions *storage.ClientApplicationVersionService
	// AttachmentExtraction is the attachment text reader the content plane
	// serves turns from. The upload route uses it to warm an attachment's
	// extraction (issue 1062). It is nil exactly when that reader is: no
	// agent plane or no object store.
	AttachmentExtraction *storage.RuntimeAttachmentObjectService
}

// Durable PostgreSQL replay remains authoritative. The advisory waiter wakes
// streams after a committed projection; this interval is the bounded fallback
// for a lost wake and the idle heartbeat/reauthorization cadence.
const phaseOneReplayPollInterval = 2 * time.Second

type validationSubmitter interface {
	Submit(context.Context, configurationapp.SubmitValidationRequest) (executionapp.AdmissionOutcome, error)
}

func newPublicRoutes(
	authorizer *postgresPublicAuthorizer,
	submitter validationSubmitter,
	replay executionapi.EventRepository,
	waiter executionapi.ReplayWaiter,
	indexStart indexingapi.StartUseCase,
	agentStart agentexecutionapi.StartUseCase,
	toolkitCallTool toolkitrun.UseCase,
	replayCapacity int,
	streamLimits executionapi.SSEStreamLimits,
) (PublicRoutes, error) {
	validation, err := configurationapi.NewValidationHandler(authorizer, submitter)
	if err != nil {
		return PublicRoutes{}, err
	}
	events, err := executionapi.NewEventHandlerWithStreamLimits(
		authorizer,
		replay,
		waiter,
		replayCapacity,
		streamLimits,
	)
	if err != nil {
		return PublicRoutes{}, err
	}
	return PublicRoutes{
		Validation:      http.HandlerFunc(validation.Submit),
		ExecutionEvents: http.HandlerFunc(events.Stream),
		IndexStart:      indexStart,
		AgentStart:      agentStart,
		ToolkitCallTool: toolkitCallTool,
	}, nil
}

type pollingReplayWaiter struct {
	interval time.Duration
}

func (w pollingReplayWaiter) Wait(ctx context.Context, _, _ string, _ uint64) (bool, error) {
	if w.interval <= 0 {
		return false, errors.New("runtime replay polling interval is invalid")
	}
	timer := time.NewTimer(w.interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return true, nil
	}
}
