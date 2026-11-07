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
	records, dropped := localTurnStepRecords(scope, calls, time.Now())
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
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
}

// A step record the commit cannot keep is dropped, and only it: the other
// calls' records, and the turn, are kept.
func TestLocalTurnStepRecordsDropABadRecordNotTheTurn(t *testing.T) {
	t.Parallel()
	huge := map[string]any{"v": 1, "tool": "computer_type"}
	for _, key := range []string{"window_title", "action", "ref", "bundle_id", "outcome", "tool"} {
		huge[key] = strings.Repeat("w", 64*1024)
	}
	huge["text"] = map[string]any{"chars": 1, "digest": strings.Repeat("d", 1024)}
	huge["screenshot"] = map[string]any{"mime": strings.Repeat("m", 1024), "digest": strings.Repeat("d", 1024),
		"artifact": strings.Repeat("p", 1024)}
	calls := []currentAgentToolCall{
		{key: "bad", entry: map[string]any{"computer_step": "text"}},
		{key: "good", entry: map[string]any{"computer_step": map[string]any{"v": 1, "tool": "computer_apps"}}},
	}
	scope := localTurnStepScope{executionID: strings.Repeat("a", 32), projectID: 1, actorUserID: 7}
	records, dropped := localTurnStepRecords(scope, calls, time.Now())
	if len(records) != 1 || records[0].NodeID != "good" || len(dropped) != 1 || dropped[0] != "bad" {
		t.Fatalf("records = %+v, dropped = %v", records, dropped)
	}
	// Bounded strings keep even a record of long values within a summary.
	records, dropped = localTurnStepRecords(scope, []currentAgentToolCall{{key: "huge", entry: map[string]any{"computer_step": huge}}}, time.Now())
	if len(dropped) != 0 || len(records) != 1 {
		t.Fatalf("a long record was dropped: %v", dropped)
	}
}
