package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runcaps"
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
//
// Main cancels at two caps: the whole-run wall cap and the spend cap
// (execution_jobs.cancel_cap, shared 0212; NULL on a row written before
// 0212 is the wall cap). A run the WORKER stopped at a cap is a runtime
// failure that Main's output plane already maps to CAP_REACHED; its cap is
// written by presentWorkerCapReached.
const (
	CapReachedCode    = runcaps.Code
	CapMaxWallSeconds = runcaps.MaxWallSeconds
)

// capReachedBrowserData is the replay event of a run stopped at cap.
func capReachedBrowserData(cap string) ([]byte, error) {
	return json.Marshal(struct {
		Code        string `json:"code"`
		Cap         string `json:"cap"`
		SafeMessage string `json:"safe_message"`
		Retryable   bool   `json:"retryable"`
	}{Code: CapReachedCode, Cap: cap, SafeMessage: runcaps.Message(cap)})
}

// cancelledAtCap answers the cap Main cancelled this job generation at, or
// "" when Main did not cancel it at a cap. A database before 0212 has no
// cancel_cap column, and only the wall cap.
func cancelledAtCap(ctx context.Context, tx sqlExecutor, executionID string, generation uint64) (string, error) {
	var reason, cap *string
	err := tx.QueryRow(ctx, `
SELECT cancel_reason, to_jsonb(j)->>'cancel_cap' FROM elitea_runtime.execution_jobs AS j
WHERE execution_id = $1 AND generation = $2`, executionID, int64(generation)).Scan(&reason, &cap)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the run's cancel reason: %w", err)
	}
	if reason == nil || *reason != CapReasonWallCap {
		return "", nil
	}
	if cap == nil || !runcaps.Valid(*cap) {
		return CapMaxWallSeconds, nil
	}
	return *cap, nil
}

// presentCapReachedAgentResponse marks the agent response of a run stopped
// at cap: streaming ends, and the response is an error with the cap's code,
// name and text. A run with no response group left (an ad hoc run whose
// group a Stop removed, or a non-agent run) updates nothing.
func presentCapReachedAgentResponse(ctx context.Context, tx sqlExecutor, record outputRecord, cap string) error {
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
	), record.ExecutionID, int64(record.Generation), runcaps.Message(cap), CapReachedCode, cap); err != nil {
		return fmt.Errorf("mark the agent response stopped at the run cap: %w", err)
	}
	return nil
}

// presentWorkerCapReached writes the cap's name on the agent response of a
// run the worker stopped at a cap (a runtime failure Main mapped to
// CAP_REACHED; persistCurrentAgentRuntimeTerminal wrote the code and text).
func presentWorkerCapReached(ctx context.Context, tx sqlExecutor, record outputRecord, cap string) error {
	if !runcaps.Valid(cap) {
		return nil
	}
	return presentCapReachedAgentResponse(ctx, tx, record, cap)
}
