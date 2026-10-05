package repos

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
)

// The accepted NodeEvent transaction owns the current claim and sequence locks.
func projectNodeRecovery(ctx context.Context, tx sqlExecutor, projectID int64, frame outputapp.NodeEventFrame, claimID string) error {
	var event struct {
		Type                string                     `json:"type"`
		StreamID            string                     `json:"stream_id"`
		MessageID           string                     `json:"message_id"`
		ExecutionGeneration string                     `json:"execution_generation"`
		SIOEvent            string                     `json:"sio_event"`
		ResponseMetadata    map[string]json.RawMessage `json:"response_metadata"`
	}
	if json.Unmarshal(frame.BrowserData, &event) != nil {
		return outputapp.ErrInvalidNodeEventOutput
	}
	if event.Type != "agent_node_recovery_required" {
		return nil
	}
	for _, key := range []string{"hitl_interrupt", "hitl_interrupts", "authorization_requests", "parallel_pause_v1", "child_thread_id", "via_call_id", "_via_call_id"} {
		if _, ok := event.ResponseMetadata[key]; ok {
			return outputapp.ErrInvalidNodeEventOutput
		}
	}
	raw, ok := event.ResponseMetadata["node_recovery_required_v1"]
	if !ok {
		return outputapp.ErrInvalidNodeEventOutput
	}
	canonical, canonicalErr := domain.CanonicalReceipt(raw)
	if canonicalErr != nil {
		return outputapp.ErrInvalidNodeEventOutput
	}
	raw = canonical
	receipt, err := domain.DecodeReceipt(raw)
	if err != nil {
		return outputapp.ErrInvalidNodeEventOutput
	}
	projectDatabaseID, valid := currentAgentDatabaseID(projectID)
	if !valid || claimID == "" {
		return outputapp.ErrInvalidNodeEventOutput
	}
	binding, bound, err := loadCurrentAgentTraceBinding(ctx, tx, frame.Fence.ExecutionID, frame.Fence.Generation, projectDatabaseID)
	if err != nil {
		return err
	}
	if !bound || event.StreamID != binding.streamID || event.MessageID != binding.messageID || event.ExecutionGeneration != binding.executionGeneration || event.SIOEvent != binding.sioEvent {
		return errors.New("node recovery conflicts with immutable response binding")
	}
	digest := sha256.Sum256(raw)
	tag, err := tx.Exec(ctx, `INSERT INTO elitea_runtime.node_recovery_visits
(execution_id,generation,activation_id,journal_revision,node_id,graph_thread,step,attempt,receipt_json,receipt_digest,source_event_id,source_claim_id,status)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'SUSPENDED')`, frame.Fence.ExecutionID, int64(frame.Fence.Generation), receipt.ActivationID, int64(receipt.JournalRevision), receipt.NodeID, receipt.GraphThread, int64(receipt.Step), receipt.Attempt, []byte(raw), digest[:], frame.EventID, claimID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return outputapp.ErrNodeEventOutputConflict
	}
	tag, err = tx.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET desired_state='SUSPENDED'
WHERE execution_id=$1 AND generation=$2 AND command_id=$3 AND state='RUNNING' AND desired_state='RUNNING' AND invocation_state='MAY_HAVE_STARTED'`, frame.Fence.ExecutionID, int64(frame.Fence.Generation), frame.Fence.CommandID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return outputapp.ErrNodeEventOutputConflict
	}
	schema, err := currentProjectSchema(projectID)
	if err != nil {
		return err
	}
	groupID, err := lockCurrentAgentMessageGroup(ctx, tx, schema, frame.Fence.ExecutionID, binding)
	if err != nil {
		return err
	}
	tag, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET meta=jsonb_set(COALESCE(meta,'{}'::jsonb),'{node_recovery_required_v1}',$2::jsonb),updated_at=clock_timestamp() WHERE id=$1 AND is_streaming=TRUE`, schema+".chat_message_group"), groupID, []byte(raw))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return outputapp.ErrNodeEventOutputConflict
	}
	return nil
}
