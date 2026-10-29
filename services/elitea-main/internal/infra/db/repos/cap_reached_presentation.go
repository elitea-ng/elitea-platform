package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The typed outcome of a run Main cancelled at a run cap (ADR-0041 decision
// 3): "Reaching a cap ends the run with cap_reached and the cap's name. The
// run is not silently truncated."
//
// The durable outcome stays CANCELLED: the worker's signed cancellation and
// Main's canonical one are both CANCELLED, and settlement must not change.
// What changes is the PRESENTATION, the same split runtime_failure.go makes
// for RESOURCE_EXHAUSTED: when the cancelled job carries cancel_reason
// cap_reached (shared 0201), the replay event the browser reads says
// CAP_REACHED with the cap's name, and the agent's response is marked as an
// error with the same code and text instead of a quiet stop.
const (
	CapReachedCode    = "CAP_REACHED"
	CapMaxWallSeconds = "max_wall_seconds"
	capReachedMessage = "The run stopped because it reached the maximum run time an administrator set. " +
		"The task may be incomplete. Check what it completed before you continue."
)

// capReachedBrowserData is the replay event of a run stopped at the cap.
func capReachedBrowserData() ([]byte, error) {
	return json.Marshal(struct {
		Code        string `json:"code"`
		Cap         string `json:"cap"`
		SafeMessage string `json:"safe_message"`
		Retryable   bool   `json:"retryable"`
	}{Code: CapReachedCode, Cap: CapMaxWallSeconds, SafeMessage: capReachedMessage})
}

// cancelledAtCap reports whether Main cancelled this job generation at a
// run cap.
func cancelledAtCap(ctx context.Context, tx sqlExecutor, executionID string, generation uint64) (bool, error) {
	var reason *string
	err := tx.QueryRow(ctx, `
SELECT cancel_reason FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = $2`, executionID, int64(generation)).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the run's cancel reason: %w", err)
	}
	return reason != nil && *reason == CapReasonWallCap, nil
}

// presentCapReachedAgentResponse marks the agent response of a run stopped
// at the cap: streaming ends, and the response is an error with the cap's
// code and text. A run with no response group left (an ad hoc run whose
// group a Stop removed, or a non-agent run) updates nothing.
func presentCapReachedAgentResponse(ctx context.Context, tx sqlExecutor, record outputRecord) error {
	schema, err := currentProjectSchema(record.ProjectionProjectID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s AS message_group
SET is_streaming = FALSE,
    meta = message_group.meta || jsonb_build_object(
        'is_error', TRUE,
        'error', $3::text,
        'error_code', $4::text,
        'cap', $5::text
    ),
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
FROM elitea_runtime.agent_execution_jobs AS agent,
     %s AS conversation
WHERE agent.execution_id = $1
  AND agent.generation = $2
  AND message_group.uuid::text = agent.client_message_id
  AND conversation.id = message_group.conversation_id
  AND conversation.uuid::text = agent.client_stream_id
  AND message_group.task_id = agent.execution_id
  AND message_group.meta->>'execution_generation' =
      agent.client_execution_generation`,
		schema+".chat_message_group",
		schema+".chat_conversations",
	), record.ExecutionID, int64(record.Generation), capReachedMessage, CapReachedCode, CapMaxWallSeconds); err != nil {
		return fmt.Errorf("mark the agent response stopped at the run cap: %w", err)
	}
	return nil
}
