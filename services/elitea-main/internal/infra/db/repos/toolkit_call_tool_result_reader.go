package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// FindToolkitCallToolExecution reuses the atomic admission ledger and its index.
func (r *ToolkitCallToolJobsRepository) FindToolkitCallToolExecution(ctx context.Context, scope, key string) (string, error) {
	outcome, _, err := loadToolRunAdmission(ctx, sqlcgen.New(r.pool), scope, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", toolkitcalltoolapp.ErrToolRunNotFound
	}
	if err != nil {
		return "", err
	}
	return outcome.ExecutionID, nil
}

var _ toolkitcalltoolapp.ResultRequestReader = (*ToolkitCallToolJobsRepository)(nil)

// ReadToolkitCallToolResultBinding uses the frozen input, not mutable toolkit settings.
func (r *ToolkitCallToolJobsRepository) ReadToolkitCallToolResultBinding(ctx context.Context, request toolkitcalltoolapp.ResultRequest) (executiondomain.ToolkitCallToolBinding, string, error) {
	if err := request.Validate(); err != nil {
		return executiondomain.ToolkitCallToolBinding{}, "", err
	}
	var content []byte
	var state string
	err := r.pool.QueryRow(ctx, `
SELECT e.content_bytes, j.state
FROM elitea_runtime.execution_jobs j
JOIN elitea_runtime.input_bundle_entries e ON e.input_bundle_id=j.input_bundle_id
WHERE j.execution_id=$1 AND j.generation=1
 AND j.capability_id='toolkit.call_tool.v1'
 AND j.resource_project_id=$2 AND j.projection_project_id=$2
 AND j.actor_id=$3 AND j.tenant_id=$4
 AND e.semantic_role='toolkit.call_tool.settings'`, request.ExecutionID, request.ProjectID, strconv.FormatInt(request.ActorUserID, 10), strconv.FormatInt(request.ProjectID, 10)).Scan(&content, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return executiondomain.ToolkitCallToolBinding{}, "", toolkitcalltoolapp.ErrToolRunNotFound
	}
	if err != nil {
		return executiondomain.ToolkitCallToolBinding{}, "", fmt.Errorf("read tool run identity: %w", err)
	}
	var saved struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	}
	if len(content) > 256*1024 || json.Unmarshal(content, &saved) != nil || saved.ID <= 0 || saved.Type == "" {
		return executiondomain.ToolkitCallToolBinding{}, "", errors.New("invalid stored tool run identity")
	}
	if saved.ID != request.ToolkitID {
		return executiondomain.ToolkitCallToolBinding{}, "", toolkitcalltoolapp.ErrToolRunNotFound
	}
	return executiondomain.ToolkitCallToolBinding{ToolkitID: saved.ID, ToolkitType: saved.Type}, state, nil
}

var _ toolkitcalltoolapp.ResultBindingReader = (*ToolkitCallToolJobsRepository)(nil)

// ReadToolkitCallToolAuthorizationRequest restores caller input after ownership validation.
func (r *ToolkitCallToolJobsRepository) ReadToolkitCallToolAuthorizationRequest(ctx context.Context, request toolkitcalltoolapp.ResultRequest) (toolkitcalltoolapp.AuthorizationRetry, error) {
	if request.Validate() != nil {
		return toolkitcalltoolapp.AuthorizationRetry{}, toolkitcalltoolapp.ErrInvalidToolRun
	}
	var envelopeBytes, arguments []byte
	err := r.pool.QueryRow(ctx, `
SELECT o.prepared_signed_envelope_bytes,e.content_bytes
FROM elitea_runtime.execution_jobs j
JOIN elitea_runtime.command_outbox o ON o.execution_id=j.execution_id AND o.generation=j.generation
JOIN elitea_runtime.input_bundle_entries e ON e.input_bundle_id=j.input_bundle_id
WHERE j.execution_id=$1 AND j.generation=1 AND j.capability_id='toolkit.call_tool.v1'
 AND j.resource_project_id=$2 AND j.projection_project_id=$2 AND j.actor_id=$3 AND j.tenant_id=$4
 AND e.semantic_role='toolkit.call_tool.arguments'`, request.ExecutionID, request.ProjectID, strconv.FormatInt(request.ActorUserID, 10), strconv.FormatInt(request.ProjectID, 10)).Scan(&envelopeBytes, &arguments)
	if errors.Is(err, pgx.ErrNoRows) {
		return toolkitcalltoolapp.AuthorizationRetry{}, toolkitcalltoolapp.ErrToolRunNotFound
	}
	if err != nil {
		return toolkitcalltoolapp.AuthorizationRetry{}, fmt.Errorf("read original tool arguments: %w", err)
	}
	var envelope runtimev1.SignedWorkerCommandEnvelopeV1
	var command runtimev1.WorkerCommandV1
	// This is the Main-owned prepared envelope, not an unverified external command.
	if len(envelopeBytes) > 256*1024 || len(arguments) > executiondomain.MaxInputEntryContentBytes || proto.Unmarshal(envelopeBytes, &envelope) != nil || proto.Unmarshal(envelope.GetWorkerCommandBytes(), &command) != nil {
		return toolkitcalltoolapp.AuthorizationRetry{}, errors.New("invalid stored tool command")
	}
	tool := command.GetToolkitCallTool()
	if tool == nil || tool.GetToolkitId() != strconv.FormatInt(request.ToolkitID, 10) || command.GetExecutionId() != request.ExecutionID || command.GetGeneration() != 1 {
		return toolkitcalltoolapp.AuthorizationRetry{}, toolkitcalltoolapp.ErrToolRunNotFound
	}
	return toolkitcalltoolapp.AuthorizationRetry{ToolName: tool.GetToolName(), ToolParams: json.RawMessage(arguments)}, nil
}
