package runtimecomposition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// pipelineApprovalChildren reads the saved child versions the pipeline hitl
// check covers (ADR-0041 decision 5) through the same tenant-schema reader
// the worker's nested child route uses. The start service freezes each one
// with its own freezer.
type pipelineApprovalChildren struct {
	versions storage.CurrentApplicationVersionSource
}

var _ agentexecutionapp.PipelineChildVersionSource = pipelineApprovalChildren{}

func (c pipelineApprovalChildren) ReadPipelineChildVersion(
	ctx context.Context, projectID, applicationID, versionID int64,
) (json.RawMessage, error) {
	record, err := c.versions.ReadCurrentApplicationVersion(ctx, projectID, applicationID, versionID)
	if err != nil {
		if errors.Is(err, storage.ErrContentNotFound) && ctx.Err() == nil {
			// The worker refuses a run whose child does not exist.
			return nil, fmt.Errorf("%w: saved child %d version %d does not exist",
				agentexecutionapp.ErrUnsupportedCurrentAgentStart, applicationID, versionID)
		}
		return nil, err
	}
	if record.ApplicationID != applicationID || record.VersionID != versionID {
		return nil, fmt.Errorf("%w: saved child %d version %d does not exist",
			agentexecutionapp.ErrUnsupportedCurrentAgentStart, applicationID, versionID)
	}
	return record.VersionDetails, nil
}
