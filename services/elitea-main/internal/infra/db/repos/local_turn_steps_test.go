package repos

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A computer call's step record keeps its allowed keys only: whatever a
// client sends, no typed text and no screenshot bytes reach run_steps
// (ADR-0047 decision 9).
func TestLocalTurnStepRecordsKeepNoTypedText(t *testing.T) {
	t.Parallel()
	calls, err := decodeOrderedCurrentAgentToolCalls(json.RawMessage(`{
		"run-1": {"tool_name": "shell", "tool_inputs": {"command": "ls"}},
		"run-2": {"tool_name": "computer_type", "timestamp_start": "2026-10-08T10:00:06Z",
			"timestamp_finish": "2026-10-08T10:00:07Z",
			"computer_step": {"v": 1, "tool": "computer_type", "outcome": "ok", "bundle_id": "com.apple.textedit",
				"text": {"chars": 6, "digest": "sha256:ab", "value": "hunter2"}, "typed": "hunter2",
				"screenshot": {"taken": true, "bytes": 10, "image_base64": "AAAA"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	scope := localTurnStepScope{executionID: strings.Repeat("a", 32), projectID: 1, actorUserID: 7}
	records, err := localTurnStepRecords(scope, calls, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want only the computer call", len(records))
	}
	got := records[0]
	summary := string(got.Summary)
	if strings.Contains(summary, "hunter2") || strings.Contains(summary, "AAAA") || strings.Contains(summary, "typed") {
		t.Fatalf("summary keeps what it must not: %s", summary)
	}
	if !strings.Contains(summary, `"chars":6`) || !strings.Contains(summary, `"taken":true`) {
		t.Fatalf("summary = %s", summary)
	}
	if got.Name != "computer_type" || got.NodeID != "run-2" || got.Actor != "user:7" ||
		!got.EndedAt.After(got.StartedAt) {
		t.Fatalf("record = %+v", got)
	}
	again, _ := localTurnStepRecords(scope, calls, time.Now())
	if again[0].StepID != got.StepID {
		t.Fatal("a retried commit names another step")
	}
	if _, err := localTurnStepRecords(scope, []currentAgentToolCall{{key: "x", entry: map[string]any{"computer_step": "text"}}}, time.Now()); err == nil {
		t.Fatal("a step record that is not an object was accepted")
	}
}
