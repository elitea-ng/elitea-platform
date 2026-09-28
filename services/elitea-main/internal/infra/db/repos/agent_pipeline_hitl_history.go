package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// The saved application version determines the participant type. A nested
// pipeline can emit the same interrupt contract, but owns no root chat history.
func directPipelineHITLReview(agentType string, metadataJSON []byte, interrupts []map[string]any) *agentexecutionapp.CurrentPipelineHITLReview {
	if agentType != "pipeline" || len(interrupts) != 1 {
		return nil
	}
	interrupt := interrupts[0]
	if interrupt["interaction_type"] != "pipeline_hitl_node" || interrupt["history_contract_version"] != float64(1) {
		return nil
	}
	var metadata map[string]any
	if json.Unmarshal(metadataJSON, &metadata) != nil {
		return nil
	}
	lineage, _ := metadata["metadata"].(map[string]any)
	for _, source := range []map[string]any{interrupt, metadata, lineage} {
		if pipelineHITLHasParent(source) {
			return nil
		}
	}
	if !validCurrentAgentHITLText(interrupt["interrupt_id"], 512) ||
		!validCurrentAgentHITLText(interrupt["node_name"], 256) ||
		!validCurrentAgentHITLText(interrupt["message"], 4*1024*1024) {
		return nil
	}
	return &agentexecutionapp.CurrentPipelineHITLReview{
		InterruptID: interrupt["interrupt_id"].(string),
		NodeName:    interrupt["node_name"].(string),
		Message:     interrupt["message"].(string),
	}
}

func pipelineHITLHasParent(source map[string]any) bool {
	for _, key := range []string{"parent_agent_name", "parent_agent_call_id", "child_thread_id", "via_call_id", "_via_call_id"} {
		if value := source[key]; value != nil && value != "" {
			return true
		}
	}
	if value := source["parent_agent_path"]; value != nil {
		path, ok := value.([]any)
		if !ok || len(path) != 0 {
			return true
		}
	}
	strategy := source["resume_strategy"]
	return strategy != nil && strategy != "" && strategy != "root"
}

// segmentCurrentPipelineHITL runs inside the admission transaction. Any refusal
// rolls back the pending-decision consumption and all runtime admission writes.
func segmentCurrentPipelineHITL(ctx context.Context, queries *sqlcgen.Queries, executionID string,
	turn agentexecutionapp.CurrentContinueTurn, row sqlcgen.ResumeCurrentAgentHITLRow) error {
	var metadata struct {
		Interrupts []map[string]any `json:"hitl_interrupts"`
		Interrupt  map[string]any   `json:"hitl_interrupt"`
	}
	if err := json.Unmarshal([]byte(row.PreviousMetadataJson), &metadata); err != nil {
		return fmt.Errorf("decode locked pipeline review: %w", err)
	}
	if len(metadata.Interrupts) == 0 && metadata.Interrupt != nil {
		metadata.Interrupts = []map[string]any{metadata.Interrupt}
	}
	actual := directPipelineHITLReview(row.AgentType, []byte(row.PreviousMetadataJson), metadata.Interrupts)
	expected := turn.PipelineHITLReview
	if actual == nil && expected == nil {
		return nil
	}
	if actual == nil || expected == nil || *actual != *expected {
		return agentexecutionapp.ErrCurrentAgentHITLAlreadyResolved
	}
	var decisions []agentexecutionapp.CurrentHITLDecision
	if turn.Validate() != nil || json.Unmarshal(turn.HITLDecisions, &decisions) != nil || len(decisions) != 1 {
		return agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	decisionText := decisions[0].Value
	switch decisions[0].Action {
	case "approve":
		decisionText = "Approved"
	case "reject":
		decisionText = "Rejected"
	case "edit":
	default:
		return agentexecutionapp.ErrInvalidCurrentAgentStart
	}
	decisionID, err := currentPGUUID(turn.PipelineDecisionID())
	if err != nil {
		return err
	}
	continuationID, err := currentPGUUID(turn.ProjectionResponseID())
	if err != nil {
		return err
	}
	segmented, err := queries.SegmentCurrentPipelineHITL(ctx, sqlcgen.SegmentCurrentPipelineHITLParams{
		ActorUserID: turn.ActorUserID, PausedResponseID: row.ResponseMessageGroupID,
		ExecutionID: executionID, PreviousTaskID: row.PreviousTaskID,
		InterruptID: actual.InterruptID, NodeName: actual.NodeName, ReviewMessage: actual.Message,
		DecisionAction: decisions[0].Action, DecisionText: decisionText, DecisionID: decisionID,
		ContinuationID: continuationID, ThreadID: turn.ThreadID, ExecutionGeneration: turn.ExecutionGeneration,
	})
	if err != nil {
		return fmt.Errorf("segment direct pipeline review: %w", err)
	}
	if segmented.ResponseMessageGroupID <= 0 || segmented.ResponseMessageID != continuationID {
		return errors.New("direct pipeline continuation returned an invalid response binding")
	}
	return nil
}
