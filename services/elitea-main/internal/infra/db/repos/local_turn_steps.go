package repos

// The step records of a desktop turn's computer calls (ADR-0047 decision 9,
// #1585): each tool step of the commit that carries a `computer_step` object
// becomes an ADR-0042 run_steps row (kind tool_call) of the turn's execution,
// in the commit's transaction. The run_step_anchor sweep anchors the run like
// any settled run: a local turn has no execution_jobs row that keeps it live.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
)

// LocalTurnStepRecordKey is the tool step key the desktop host writes a
// computer call's step record under.
const LocalTurnStepRecordKey = "computer_step"

// localTurnStepNamespace names a local turn step's step_id: one per
// (execution, tool call key), so a step is the same row however often the
// commit is retried.
var localTurnStepNamespace = uuid.MustParse("6f1c55a2-3c0e-4f43-9a8b-1585a0470009")

// localTurnStepFields are the keys a step record keeps, and of its `text` and
// `screenshot` objects. Any other key is dropped: the record never holds
// typed text or screenshot bytes, whatever a client sends.
var (
	localTurnStepFields = map[string]bool{
		"v": true, "tool": true, "outcome": true, "bundle_id": true, "window_title": true,
		"action": true, "ref": true, "text": true, "screenshot": true,
	}
	localTurnStepTextFields       = map[string]bool{"chars": true, "digest": true}
	localTurnStepScreenshotFields = map[string]bool{
		"taken": true, "mime": true, "bytes": true, "width": true, "height": true, "zoom": true,
		"digest": true, "uploaded": true, "artifact": true,
	}
)

// localTurnStepRecords builds the run_steps records of the commit's computer
// calls, in tool call order. A step record that is not an object, or is
// larger than a step summary may be, refuses the commit.
func localTurnStepRecords(record localTurnStepScope, toolCalls []currentAgentToolCall, now time.Time) ([]runevidence.Record, error) {
	var records []runevidence.Record
	for _, call := range toolCalls {
		raw, present := call.entry[LocalTurnStepRecordKey]
		if !present || raw == nil {
			continue
		}
		object, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("a computer step record is not an object")
		}
		summary, err := json.Marshal(cleanLocalTurnStep(object))
		if err != nil || len(summary) > runevidence.MaxSummaryBytes {
			return nil, errors.New("a computer step record is too large")
		}
		started, ended := now, now
		if at := parseCurrentAgentTime(call.entry["timestamp_start"]); at != nil {
			started = *at
		}
		if at := parseCurrentAgentTime(call.entry["timestamp_finish"]); at != nil && !at.Before(started) {
			ended = *at
		} else if ended.Before(started) {
			ended = started
		}
		name := truncateEvidence(currentAgentString(call.entry["tool_name"]), runevidence.MaxNameBytes)
		records = append(records, runevidence.Record{
			StepID:           uuid.NewSHA1(localTurnStepNamespace, []byte(record.executionID+"\x00"+call.key)).String(),
			Generation:       1,
			Kind:             runevidence.KindToolCall,
			Actor:            "user:" + strconv.FormatInt(record.actorUserID, 10),
			ActorUserID:      &record.actorUserID,
			ConversationUUID: record.conversationUUID,
			NodeID:           truncateEvidence(call.key, runevidence.MaxNodeIDBytes),
			Name:             name,
			ToolkitID:        "computer",
			StartedAt:        started,
			EndedAt:          ended,
			Summary:          summary,
			CitedStepIDs:     []string{},
			CitedDocumentIDs: []string{},
		})
	}
	return records, nil
}

// cleanLocalTurnStep keeps the step record's allowed keys (and those of its
// `text` and `screenshot` objects); strings lose NUL bytes.
func cleanLocalTurnStep(object map[string]any) map[string]any {
	out := make(map[string]any, len(object))
	for key, value := range object {
		if !localTurnStepFields[key] {
			continue
		}
		switch key {
		case "text":
			if nested, ok := value.(map[string]any); ok {
				out[key] = keepScalarFields(nested, localTurnStepTextFields)
			}
		case "screenshot":
			if nested, ok := value.(map[string]any); ok {
				out[key] = keepScalarFields(nested, localTurnStepScreenshotFields)
			}
		default:
			if scalar, ok := localTurnStepScalar(value); ok {
				out[key] = scalar
			}
		}
	}
	return out
}

func keepScalarFields(object map[string]any, allowed map[string]bool) map[string]any {
	out := make(map[string]any, len(object))
	for key, value := range object {
		if !allowed[key] {
			continue
		}
		if scalar, ok := localTurnStepScalar(value); ok {
			out[key] = scalar
		}
	}
	return out
}

// localTurnStepScalar is value when it is a string (NUL bytes removed, at
// most 1 KiB), a number or a boolean.
func localTurnStepScalar(value any) (any, bool) {
	switch typed := value.(type) {
	case string:
		return truncateEvidence(currentAgentString(typed), 1024), true
	case json.Number, bool, float64:
		return typed, true
	}
	return nil, false
}

// localTurnStepScope is what the commit knows of the turn's run.
type localTurnStepScope struct {
	executionID      string
	projectID        int64
	actorUserID      int64
	conversationUUID string
	messageID        string
}

// appendLocalTurnSteps writes the commit's computer step records into tx.
func appendLocalTurnSteps(ctx context.Context, tx pgx.Tx, scope localTurnStepScope, toolCalls []currentAgentToolCall) error {
	records, err := localTurnStepRecords(scope, toolCalls, time.Now().UTC())
	if err != nil {
		return errors.Join(localturn.ErrInvalid, err)
	}
	if len(records) == 0 {
		return nil
	}
	_, err = appendRunSteps(ctx, tx, runevidence.RunContext{
		RunID: scope.executionID, Generation: 1, ProjectID: scope.projectID,
		Actor:       "user:" + strconv.FormatInt(scope.actorUserID, 10),
		ActorUserID: &scope.actorUserID, ConversationUUID: scope.conversationUUID, MessageID: scope.messageID,
	}, records)
	return err
}
