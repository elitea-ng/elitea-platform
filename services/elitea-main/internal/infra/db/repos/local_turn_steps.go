package repos

// The step records of a desktop turn's computer and browser calls (ADR-0047
// decision 9, #1585; ADR-0048 decision 9, #1591): each tool step of the
// commit that carries a `computer_step` or `browser_step` object becomes an
// ADR-0042 run_steps row (kind tool_call) of the turn's execution, in the
// commit's transaction. The run_step_anchor sweep anchors the run like
// any settled run: a local turn has no execution_jobs row that keeps it live.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
)

// LocalTurnStepRecordKey is the tool step key the desktop host writes a
// computer call's step record under.
const LocalTurnStepRecordKey = "computer_step"

// LocalTurnBrowserStepKey is the tool step key the desktop host writes a
// browser pane call's step record under (client contract 1.42).
const LocalTurnBrowserStepKey = "browser_step"

// localTurnStepKinds is each step record key with the fields it keeps and
// the toolkit its run step names.
var localTurnStepKinds = []struct {
	key     string
	toolkit string
	fields  map[string]bool
}{
	{key: LocalTurnStepRecordKey, toolkit: "computer", fields: localTurnStepFields},
	{key: LocalTurnBrowserStepKey, toolkit: "browser", fields: localTurnBrowserStepFields},
}

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
	// A browser call's record: the host and the URL path (no query or
	// fragment), the ref with its role and name, the outcome; typed text as
	// its length and digest, a screenshot as its size.
	localTurnBrowserStepFields = map[string]bool{
		"v": true, "tool": true, "outcome": true, "host": true, "path": true,
		"ref": true, "role": true, "name": true, "text": true, "screenshot": true,
	}
	localTurnStepTextFields       = map[string]bool{"chars": true, "digest": true}
	localTurnStepScreenshotFields = map[string]bool{
		"taken": true, "mime": true, "bytes": true, "width": true, "height": true, "zoom": true,
		"digest": true, "uploaded": true, "artifact": true,
	}
)

// localTurnStepRecords builds the run_steps records of the commit's computer
// calls, in tool call order. A step record that is not an object, or is
// still larger than a step summary may be once its strings are cut, is
// dropped (and named in dropped): the turn itself is committed.
func localTurnStepRecords(record localTurnStepScope, toolCalls []currentAgentToolCall, now time.Time) (records []runevidence.Record, dropped []string) {
	for _, call := range toolCalls {
		var raw any
		kind := localTurnStepKinds[0]
		for _, candidate := range localTurnStepKinds {
			if value, present := call.entry[candidate.key]; present && value != nil {
				raw, kind = value, candidate
				break
			}
		}
		if raw == nil {
			continue
		}
		object, ok := raw.(map[string]any)
		if !ok {
			dropped = append(dropped, call.key)
			continue
		}
		summary, err := json.Marshal(cleanLocalTurnStep(object, kind.fields))
		if err != nil || len(summary) > runevidence.MaxSummaryBytes {
			dropped = append(dropped, call.key)
			continue
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
			ToolkitID:        kind.toolkit,
			StartedAt:        started,
			EndedAt:          ended,
			Summary:          summary,
			CitedStepIDs:     []string{},
			CitedDocumentIDs: []string{},
		})
	}
	return records, dropped
}

// cleanLocalTurnStep keeps the step record's allowed keys (and those of its
// `text` and `screenshot` objects); strings lose NUL bytes.
func cleanLocalTurnStep(object map[string]any, fields map[string]bool) map[string]any {
	out := make(map[string]any, len(object))
	for key, value := range object {
		if !fields[key] {
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
// most 512 bytes: the record's 16 strings then fit a step summary), a
// number or a boolean.
func localTurnStepScalar(value any) (any, bool) {
	switch typed := value.(type) {
	case string:
		return truncateEvidence(currentAgentString(typed), 512), true
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

// appendLocalTurnSteps writes the commit's computer and browser step records
// into tx. A
// record it cannot keep is dropped with a warning, never the turn.
func appendLocalTurnSteps(ctx context.Context, tx pgx.Tx, scope localTurnStepScope, toolCalls []currentAgentToolCall) error {
	records, dropped := localTurnStepRecords(scope, toolCalls, time.Now().UTC())
	if len(dropped) > 0 {
		slog.WarnContext(ctx, "local turn: step records dropped (not an object, or too large)",
			"execution_id", scope.executionID, "tool_calls", dropped)
	}
	if len(records) == 0 {
		return nil
	}
	_, err := appendRunSteps(ctx, tx, runevidence.RunContext{
		RunID: scope.executionID, Generation: 1, ProjectID: scope.projectID,
		Actor:       "user:" + strconv.FormatInt(scope.actorUserID, 10),
		ActorUserID: &scope.actorUserID, ConversationUUID: scope.conversationUUID, MessageID: scope.messageID,
	}, records)
	return err
}
