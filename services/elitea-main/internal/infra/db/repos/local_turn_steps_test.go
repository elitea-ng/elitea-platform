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

// A browser pane call's step record (ADR-0048 decision 9, contract 1.42)
// keeps the host, the URL path, the ref with its role and name and the
// outcome; typed text only as its length and digest, never a query.
func TestLocalTurnBrowserStepRecordsKeepNoTypedTextOrQuery(t *testing.T) {
	t.Parallel()
	calls, err := decodeOrderedCurrentAgentToolCalls(json.RawMessage(`{
		"run-1": {"tool_name": "browser_type", "tool_inputs": {"ref": "e1", "text_chars": 7},
			"browser_step": {"v": 1, "tool": "browser_type", "outcome": "user_active", "host": "shop.example.com",
				"path": "/cart", "url": "https://shop.example.com/cart?token=s3cret", "ref": "e1", "role": "textbox",
				"name": "Search", "text": {"chars": 7, "digest": "hmac-sha256:ab", "value": "hunter2"}}},
		"run-2": {"tool_name": "browser_open", "computer_step": {"v": 1, "tool": "computer_apps"},
			"browser_step": {"v": 1, "tool": "browser_open"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	scope := localTurnStepScope{executionID: strings.Repeat("b", 32), projectID: 1, actorUserID: 7}
	records, dropped := localTurnStepRecords(scope, calls, time.Now())
	if len(dropped) != 0 || len(records) != 2 {
		t.Fatalf("records = %d, dropped = %v", len(records), dropped)
	}
	summary := string(records[0].Summary)
	if strings.Contains(summary, "hunter2") || strings.Contains(summary, "s3cret") || strings.Contains(summary, `"url"`) {
		t.Fatalf("summary keeps what it must not: %s", summary)
	}
	for _, want := range []string{`"host":"shop.example.com"`, `"path":"/cart"`, `"role":"textbox"`, `"name":"Search"`, `"outcome":"user_active"`, `"chars":7`} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %s lacks %s", summary, want)
		}
	}
	if records[0].ToolkitID != "browser" || records[0].Name != "browser_type" {
		t.Fatalf("record = %+v", records[0])
	}
	// One record per step: a computer record wins over a browser one.
	if records[1].ToolkitID != "computer" {
		t.Fatalf("second record toolkit = %q", records[1].ToolkitID)
	}
}
