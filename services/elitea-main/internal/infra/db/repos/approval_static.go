package repos

// The approval ledger for static pipeline pauses (ADR-0041 decision 5,
// issue #1491; shared 0211).
//
// A pipeline node with interrupt_before or interrupt_after stops the run
// until a person continues it. The worker reports the stop as a completed
// message that carries pipeline_static_v1 (one root pause) or
// pipeline_static_tools_v1 (the pauses of pipelines that agent tools ran).
// These waits are approvals too:
//
//   - the terminal projection writes one pause row per wait, and applies the
//     unattended action of a schedule or trigger run, as for a HITL pause.
//     A static pause offers only "continue", so it has no refusal: skip
//     falls back to fail, as for a mixed pause.
//   - the static continuation writes the decision row of each wait it
//     continues, in its admission transaction.
//
// The interrupt id of a static wait is its pause id
// ("pipeline-static:sha256:<hex>"). A static wait names no tool and has no
// arguments, so its row has no tool name and no argument digest.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

// staticApprovalGuardrail is the guardrail type of a static wait (the
// fanout-interrupt-card contract uses the same name).
const staticApprovalGuardrail = "pipeline_static"

// staticApprovalAction is the one action a static wait offers.
const staticApprovalAction = "continue"

// staticApprovalPauses answers the waits of a static full message as a JSON
// array of root interrupts, in the shape projectRootApprovalPause reads. It
// answers nil for a message that is not a static pause.
func staticApprovalPauses(message *currentAgentFullMessage) (json.RawMessage, error) {
	if message == nil {
		return nil, nil
	}
	var ids []string
	if len(message.PipelineStaticProof) != 0 {
		var proof agentexecutionapp.CurrentPipelineStaticProof
		if err := json.Unmarshal(message.PipelineStaticProof, &proof); err != nil || proof.PauseID == "" {
			return nil, outputapp.ErrAgentExecutionResultMismatch
		}
		ids = append(ids, proof.PauseID)
	}
	if len(message.PipelineStaticTools) != 0 {
		var inventory agentexecutionapp.CurrentStaticToolInventory
		if err := json.Unmarshal(message.PipelineStaticTools, &inventory); err != nil {
			return nil, outputapp.ErrAgentExecutionResultMismatch
		}
		for _, pause := range inventory.Pauses {
			if pause.Proof.PauseID == "" {
				return nil, outputapp.ErrAgentExecutionResultMismatch
			}
			ids = append(ids, pause.Proof.PauseID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	pauses := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		pauses = append(pauses, map[string]any{
			"interrupt_id":      id,
			"guardrail_type":    staticApprovalGuardrail,
			"available_actions": []string{staticApprovalAction},
		})
	}
	return json.Marshal(pauses)
}

// projectStaticApprovalPause records the pause rows of a static full message
// on the terminal transaction and applies the run's unattended action. It
// answers rootPauseFailed when the response must end with
// approval_required_unattended instead of the pause.
func projectStaticApprovalPause(
	ctx context.Context, tx sqlExecutor, expected outputapp.ExpectedAgentExecution, message *currentAgentFullMessage,
) (rootPauseOutcome, error) {
	if skipApprovalLedger(tx) {
		return rootPauseWaits, nil
	}
	pauses, err := staticApprovalPauses(message)
	if err != nil || pauses == nil {
		return rootPauseWaits, err
	}
	// mixed is false: a static wait offers no refusal, so skipRefusal finds
	// none and an unattended skip becomes fail.
	return projectRootApprovalPause(ctx, tx, expected, pauses, false)
}

// finalizeStaticApprovalFailure marks a finalized static response as the
// typed error approval_required_unattended. The caller finalized the message
// without its static pause, so nothing can continue it.
func finalizeStaticApprovalFailure(ctx context.Context, tx sqlExecutor, messageGroupID int64) error {
	tag, err := tx.Exec(ctx, `
UPDATE chat_message_group
SET meta = (meta - 'pipeline_static_v1' - 'pipeline_static_tools_v1')
        || jsonb_build_object('is_error', TRUE, 'error', $2::text, 'error_code', $3::text),
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
WHERE id = $1`, messageGroupID, approvalFailedMessage, ApprovalRequiredUnattendedCode)
	if err != nil || tag.RowsAffected() != 1 {
		return fmt.Errorf("end the unattended static pause: %w", terminalWriteError(err))
	}
	return nil
}

// resumeCurrentAgentStaticWithLedger continues a static pause and, in the
// same admission transaction, writes the decision row of each wait it
// continues. It reads the paused execution first and locks the response
// row, which is the ledger's lock order (the response, then the ledger).
func resumeCurrentAgentStaticWithLedger(
	ctx context.Context, tx pgx.Tx, queries *sqlcgen.Queries, executionID string,
	turn agentexecutionapp.CurrentContinueTurn,
) error {
	executor := pgxExecutor{queryer: tx}
	pausedExecution, err := lockStaticPausedExecution(ctx, executor, turn.ResponseMessageID)
	if err != nil {
		return err
	}
	var ids []string
	if turn.PipelineStaticTools != nil {
		err = resumeCurrentAgentStaticTools(ctx, queries, executionID, turn)
		for _, decision := range turn.PipelineStaticTools.Selected {
			ids = append(ids, decision.PauseID)
		}
	} else {
		err = resumeCurrentAgentStatic(ctx, queries, executionID, turn)
		if turn.PipelineStaticPause != nil {
			ids = append(ids, turn.PipelineStaticPause.Proof.PauseID)
		}
	}
	if err != nil {
		return err
	}
	return recordStaticApprovalDecisions(ctx, executor, turn, pausedExecution, ids)
}

// lockStaticPausedExecution reads the execution that paused the response,
// and locks the response row.
func lockStaticPausedExecution(ctx context.Context, tx sqlExecutor, responseMessageID string) (string, error) {
	var taskID string
	err := tx.QueryRow(ctx, `
SELECT COALESCE(task_id, '') FROM chat_message_group WHERE uuid = $1::uuid FOR UPDATE`, responseMessageID).
		Scan(&taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The resume below refuses a response that does not exist.
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the paused static execution: %w", err)
	}
	return taskID, nil
}

// recordStaticApprovalDecisions writes one decision row (action continue,
// outcome approved) per continued wait. A wait that already has a decision
// is the racing approver that lost.
func recordStaticApprovalDecisions(
	ctx context.Context, tx sqlExecutor, turn agentexecutionapp.CurrentContinueTurn, pausedExecution string,
	ids []string,
) error {
	if skipApprovalLedger(tx) || len(ids) == 0 {
		return nil
	}
	if pausedExecution == "" {
		pausedExecution = "response:" + turn.ResponseMessageID
	}
	origin, err := executionTriggerOriginOf(ctx, tx, pausedExecution)
	if err != nil {
		return err
	}
	for _, id := range ids {
		inserted, err := recordApproval(ctx, tx, approvalEntry{
			ProjectID: turn.ProjectID, ExecutionID: pausedExecution, ResponseMessageID: turn.ResponseMessageID,
			ConversationUUID: turn.ConversationUUID, InterruptID: id, Path: approvalPathRoot,
			Phase: approvalPhaseDecision, Outcome: approvalOutcomeOfAction(staticApprovalAction),
			Action: staticApprovalAction, ActorUserID: turn.ActorUserID, TriggerOrigin: origin,
			Call: approvalCall{GuardrailType: staticApprovalGuardrail},
		})
		if err != nil {
			return err
		}
		if !inserted {
			return agentexecutionapp.ErrCurrentAgentHITLAlreadyResolved
		}
		if err := finishUnattendedAction(ctx, tx, pausedExecution, id, "DONE"); err != nil {
			return err
		}
	}
	return nil
}
