package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/jackc/pgx/v5"
)

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
