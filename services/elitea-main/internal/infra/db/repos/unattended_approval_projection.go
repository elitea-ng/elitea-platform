package repos

// The cloud half of a grant approval (ADR-0045 decision 2, #1436). The
// worker approves a granted sensitive call in-line and reports it in one
// agent_unattended_approval node event. Main checks the event against the
// stored grant and writes the ledger decision row in the transaction that
// accepts the frame.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

const (
	unattendedApprovalEventType   = "agent_unattended_approval"
	unattendedApprovalMetadataKey = "unattended_approval_v1"
)

// sqlRowQueryer adapts a sqlExecutor to the chain helpers' rowQueryer.
type sqlRowQueryer struct{ executor sqlExecutor }

func (q sqlRowQueryer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return q.executor.QueryRow(ctx, sql, args...)
}

type unattendedApprovalMetadata struct {
	GrantID        string `json:"grant_id"`
	InterruptID    string `json:"interrupt_id"`
	ToolkitName    string `json:"toolkit_name"`
	ToolkitType    string `json:"toolkit_type"`
	ToolName       string `json:"tool_name"`
	ArgumentDigest string `json:"argument_digest"`
}

// projectUnattendedApproval writes the ledger row of one grant approval. A
// frame of another type is not its business. A grant approval that the
// stored grant does not cover is invalid output: the frame is refused.
func projectUnattendedApproval(ctx context.Context, tx sqlExecutor, projectID int64, frame outputapp.NodeEventFrame) error {
	var event struct {
		Type             string                     `json:"type"`
		StreamID         string                     `json:"stream_id"`
		MessageID        string                     `json:"message_id"`
		ResponseMetadata map[string]json.RawMessage `json:"response_metadata"`
	}
	if json.Unmarshal(frame.BrowserData, &event) != nil {
		return outputapp.ErrInvalidNodeEventOutput
	}
	if event.Type != unattendedApprovalEventType {
		return nil
	}
	raw, ok := event.ResponseMetadata[unattendedApprovalMetadataKey]
	if !ok {
		return outputapp.ErrInvalidNodeEventOutput
	}
	var approval unattendedApprovalMetadata
	if json.Unmarshal(raw, &approval) != nil || approval.InterruptID == "" ||
		(approval.ArgumentDigest != "" && !argumentDigestShape.MatchString(approval.ArgumentDigest)) {
		return outputapp.ErrInvalidNodeEventOutput
	}
	chain, found, err := readRunChainLink(ctx, sqlRowQueryer{executor: tx}, frame.Fence.ExecutionID)
	if err != nil {
		return err
	}
	// The run must carry this grant, and the grant must name the pair.
	if !found || chain.ProjectID != projectID || chain.GrantID != approval.GrantID ||
		!chain.Grant.HasTool(approval.ToolkitName, approval.ToolName) {
		return outputapp.ErrInvalidNodeEventOutput
	}
	entry := approvalEntry{
		ProjectID: projectID, ExecutionID: frame.Fence.ExecutionID, ResponseMessageID: event.MessageID,
		ConversationUUID: chain.ConversationUUID, InterruptID: approval.InterruptID, Path: approvalPathRoot,
		Phase: approvalPhaseDecision, Outcome: ApprovalOutcomeApproved, Action: "approve",
		ActorUserID: chain.OwnerUserID, TriggerOrigin: chain.TriggerOrigin,
		Reason: unattended.ReasonUnattendedGrant, GrantID: chain.GrantID,
		Call: approvalCall{
			GuardrailType: "sensitive_tool", ToolName: approval.ToolName, ToolkitName: approval.ToolkitName,
			ToolkitType: approval.ToolkitType, ArgumentDigest: approval.ArgumentDigest,
		},
	}
	if entry.ResponseMessageID == "" {
		entry.ResponseMessageID = chain.ResponseMessageID
	}
	if _, err := recordApproval(ctx, tx, entry); err != nil {
		return fmt.Errorf("record the grant approval: %w", err)
	}
	return nil
}
