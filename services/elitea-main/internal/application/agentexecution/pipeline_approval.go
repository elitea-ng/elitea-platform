package agentexecution

import (
	"context"
	"encoding/json"
)

// PipelineApprovalAdmission checks the pipeline hitl rule of the approval
// policy (ADR-0041 decision 5) when a pipeline run starts: under a policy
// that requires a `hitl` node for the agent's tier, a pipeline with a path to
// an external_send or payment tool node that passes no `hitl` node does not
// start. In production it is workerapprovalpolicy.PipelineAdmission. A
// refusal carries approvalpolicy.CodeHITLRequired; any other error is a
// source that could not be read.
type PipelineApprovalAdmission interface {
	AdmitPipeline(ctx context.Context, projectID, applicationID int64, versionDetails json.RawMessage) error
}

// WithPipelineApproval attaches the pipeline hitl check, in the same
// after-construction idiom as WithMemories. A service without it starts every
// pipeline the other checks admit.
func (service *CurrentApplicationStartService) WithPipelineApproval(
	admission PipelineApprovalAdmission,
) *CurrentApplicationStartService {
	service.pipelineApproval = admission
	return service
}

// admitPipelineApproval runs the check on the frozen version: its tools carry
// the toolkit names the nodes bind and an approved MCP server's pins.
func (service *CurrentApplicationStartService) admitPipelineApproval(
	ctx context.Context,
	projectID, applicationID int64,
	frozenVersion json.RawMessage,
) error {
	if service.pipelineApproval == nil {
		return nil
	}
	return service.pipelineApproval.AdmitPipeline(ctx, projectID, applicationID, frozenVersion)
}
