package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
)

// parkedMetadataKey is the parked terminal frame's response_metadata member
// (fanout-interrupt-decisions §8, §9).
const parkedMetadataKey = "fanout_parked_v1"

// currentAgentFanoutParked is a paused terminal frame (agent_hitl_interrupt)
// that carries fanout_parked_v1 instead of root pause cards.
type currentAgentFanoutParked struct {
	ThreadID      string
	Parked        domain.Parked
	InvokedSkills json.RawMessage
	// Settlement is set by persistCurrentAgentFanoutParked, inside the
	// terminal transaction; the caller acts on it only after commit.
	Settlement ParkSettlement
}

// errInvalidParkedTerminal refuses a parked terminal frame. It is both the
// projection's typed result refusal and the ledger's parked-payload refusal.
var errInvalidParkedTerminal = fmt.Errorf("%w: %w", outputapp.ErrAgentExecutionResultMismatch, domain.ErrInvalidParked)

// decodeCurrentAgentFanoutParked reports whether a paused terminal frame is
// a parked fan-out frame, and decodes it strictly when it is. A frame that
// carries fanout_parked_v1 together with a root pause member, or whose
// payload fails fanout-parked.schema.json, is refused: one frame is one
// pause authority, and a malformed park is never stored as a silent park.
func decodeCurrentAgentFanoutParked(contentJSON, responseMetadata json.RawMessage) (currentAgentFanoutParked, bool, error) {
	var metadata map[string]json.RawMessage
	if json.Unmarshal(responseMetadata, &metadata) != nil {
		return currentAgentFanoutParked{}, false, outputapp.ErrAgentExecutionResultMismatch
	}
	raw, parked := metadata[parkedMetadataKey]
	if !parked {
		return currentAgentFanoutParked{}, false, nil
	}
	for _, key := range legacyPauseMembers {
		if _, mixed := metadata[key]; mixed {
			return currentAgentFanoutParked{}, true, errInvalidParkedTerminal
		}
	}
	var content, threadID string
	if json.Unmarshal(contentJSON, &content) != nil || content == "" || len(content) > 64*1024 ||
		strings.ContainsRune(content, '\x00') ||
		json.Unmarshal(metadata["thread_id"], &threadID) != nil || threadID == "" || len(threadID) > 256 ||
		strings.ContainsRune(threadID, '\x00') {
		return currentAgentFanoutParked{}, true, errInvalidParkedTerminal
	}
	payload, err := domain.ParseParked(raw)
	if err != nil {
		return currentAgentFanoutParked{}, true, errInvalidParkedTerminal
	}
	invokedSkills, err := mergeCurrentAgentInvokedSkills(nil, metadata["invoked_skills"])
	if err != nil {
		return currentAgentFanoutParked{}, true, outputapp.ErrAgentExecutionResultMismatch
	}
	return currentAgentFanoutParked{ThreadID: threadID, Parked: payload, InvokedSkills: invokedSkills}, true, nil
}

// persistCurrentAgentFanoutParked records the parked response and settles
// the park in the terminal transaction, under the response row lock the
// caller already holds (LockCurrentAgentResponseForTerminal), so it
// serializes with Decide (contract §8). Every pending entry must be a card
// of this response; the response row keeps the canonical payload for Web to
// reconcile from.
func persistCurrentAgentFanoutParked(ctx context.Context, tx sqlExecutor, expected outputapp.ExpectedAgentExecution, messageGroupID int64, existingSkills string, terminal currentAgentTerminal) error {
	parked := terminal.FanoutParked
	projectID, err := parseProjectID(expected.ProjectionProjectID)
	if err != nil || expected.Generation == 0 || expected.Generation > math.MaxInt64 {
		return outputapp.ErrAgentExecutionResultMismatch
	}
	skills, err := mergeCurrentAgentInvokedSkills([]byte(existingSkills), parked.InvokedSkills)
	if err != nil {
		return outputapp.ErrAgentExecutionResultMismatch
	}
	keys := make([]string, 0, len(parked.Parked.Pending))
	ids := make([]string, 0, len(parked.Parked.Pending))
	for _, card := range parked.Parked.Pending {
		keys, ids = append(keys, card.InterruptKey), append(ids, card.InterruptID)
	}
	var matched int
	if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM elitea_runtime.execution_interrupts card
JOIN unnest($3::text[], $4::text[]) AS pending(interrupt_key, interrupt_id)
  ON card.interrupt_key = pending.interrupt_key AND card.interrupt_id = pending.interrupt_id
WHERE card.root_response_id = $1::uuid AND card.project_id = $2`,
		expected.ClientMessageID, projectID, keys, ids).Scan(&matched); err != nil {
		return fmt.Errorf("check parked interrupt cards: %w", err)
	}
	if matched != len(keys) {
		return errInvalidParkedTerminal
	}
	tag, err := tx.Exec(ctx, `
UPDATE chat_message_group
SET is_streaming = FALSE,
    meta = (meta - 'hitl_interrupt' - 'hitl_interrupts' - 'authorization_requests')
        || jsonb_build_object(
            'thread_id', $2::text,
            'fanout_parked_v1', $3::jsonb,
            'is_error', FALSE,
            'error', '',
            'invoked_skills', $4::jsonb)
        || CASE WHEN jsonb_typeof($5::jsonb) = 'object'
            THEN jsonb_build_object('skipped_capabilities', $5::jsonb)
            ELSE '{}'::jsonb END,
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
WHERE id = $1`, messageGroupID, parked.ThreadID, string(parked.Parked.Canonical), string(skills),
		string(cloneJSONOrDefault(terminal.SkippedCapabilities, []byte("null"))))
	if err != nil {
		return fmt.Errorf("persist current agent fan-out park: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("persist current agent fan-out park: response row is unavailable")
	}
	settlement, err := settleParkedResponse(ctx, tx, projectID, expected.ClientMessageID, expected.ExecutionID,
		int64(expected.Generation), parked.Parked.DecisionRevisionSeen)
	switch {
	case err == nil:
		parked.Settlement = settlement
		return nil
	case errors.Is(err, domain.ErrLedgerFault), errors.Is(err, domain.ErrInvalidRaise):
		// A park the ledger cannot have produced: a revision the Worker
		// cannot have seen, or an execution outside the ledger's identity.
		return errInvalidParkedTerminal
	default:
		return err
	}
}
