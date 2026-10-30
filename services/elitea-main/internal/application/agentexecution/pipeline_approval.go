package agentexecution

import (
	"context"
	"encoding/json"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/approvalpolicy"
)

// PipelineChildVersions answers the frozen version of one saved child the
// run reaches (an application tool), in the run's project.
type PipelineChildVersions = func(ctx context.Context, applicationID, versionID int64) (json.RawMessage, error)

// PipelineApprovalAdmission checks the pipeline hitl rule of the approval
// policy (ADR-0041 decision 5) when a run is admitted: under a policy that
// requires a `hitl` node for the agent's tier, a pipeline with a path to an
// external_send or payment tool node that passes no `hitl` node does not
// start. The check covers the saved child pipelines the run reaches, which it
// reads through children. In production it is
// workerapprovalpolicy.PipelineAdmission. A refusal carries
// approvalpolicy.CodeHITLRequired; any other error is a source that could
// not be read.
type PipelineApprovalAdmission interface {
	AdmitPipeline(
		ctx context.Context,
		projectID, applicationID int64,
		versionDetails json.RawMessage,
		children PipelineChildVersions,
	) error
}

// PipelineChildVersionSource reads the SAVED version_details of one
// application version in the project's tenant schema. The check freezes it
// with the run's own freezer, as the worker's child route does.
type PipelineChildVersionSource interface {
	ReadPipelineChildVersion(ctx context.Context, projectID, applicationID, versionID int64) (json.RawMessage, error)
}

// WithPipelineApproval attaches the pipeline hitl check and the reader of
// the saved child versions it checks, in the same after-construction idiom
// as WithMemories. A service without the check admits every pipeline the
// other checks admit. A check without a reader refuses a run that reaches a
// saved child pipeline under a requiring policy: it cannot check the child.
func (service *CurrentApplicationStartService) WithPipelineApproval(
	admission PipelineApprovalAdmission,
	children PipelineChildVersionSource,
) *CurrentApplicationStartService {
	service.pipelineApproval = admission
	service.pipelineChildren = children
	return service
}

// freezeAdmitted is the ONE freeze of a run's root version in this service:
// start, ad hoc start, both regenerate arms and every continuation freeze
// through it, so no admission path skips the pipeline hitl rule. It freezes
// the version, then checks the frozen version: its tools carry the toolkit
// names the nodes bind, an approved MCP server's pins and the saved child
// pipelines the run reaches. applicationID is the saved root, 0 for ad hoc.
func (service *CurrentApplicationStartService) freezeAdmitted(
	ctx context.Context,
	request CurrentApplicationVersionFreezeRequest,
	applicationID int64,
) (json.RawMessage, error) {
	frozen, err := service.freezer.FreezeCurrentApplicationVersion(ctx, request)
	if err != nil {
		return nil, err
	}
	if service.pipelineApproval == nil {
		return frozen, nil
	}
	err = service.pipelineApproval.AdmitPipeline(
		ctx, int64(request.ProjectID), applicationID, frozen,
		service.pipelineChildVersions(request.ProjectID, request.ActorUserID),
	)
	if err != nil {
		if approvalpolicy.Refusal(err) != nil {
			return nil, unsupportedStartBecause("pipeline refused by the approval policy", err)
		}
		return nil, err
	}
	return frozen, nil
}

// pipelineChildVersions reads and freezes one saved child for the check, as
// the run's actor in the run's project. Nil without a reader.
func (service *CurrentApplicationStartService) pipelineChildVersions(projectID, actorUserID int32) PipelineChildVersions {
	if service.pipelineChildren == nil {
		return nil
	}
	return func(ctx context.Context, applicationID, versionID int64) (json.RawMessage, error) {
		saved, err := service.pipelineChildren.ReadPipelineChildVersion(ctx, int64(projectID), applicationID, versionID)
		if err != nil {
			return nil, err
		}
		if !validJSONObject(saved) {
			return nil, unsupportedStart("a saved child version is not one JSON object")
		}
		return service.freezer.FreezeCurrentApplicationVersion(ctx, CurrentApplicationVersionFreezeRequest{
			ProjectID: projectID, ActorUserID: actorUserID, VersionDetails: saved,
		})
	}
}
