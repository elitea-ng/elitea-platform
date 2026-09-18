package repos

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDecodeCurrentAgentTraceDeltaPreservesCurrentPartialMessageContract(t *testing.T) {
	raw := json.RawMessage(`{
  "type":"partial_message",
  "stream_id":"10000000-0000-4000-8000-000000000001",
  "message_id":"20000000-0000-4000-8000-000000000001",
  "execution_generation":"30000000-0000-4000-8000-000000000001",
  "sio_event":"chat_predict",
  "response_metadata":{
    "tool_calls":{
      "run-a":{"tool_name":"first","tool_run_id":"run-a"},
      "run-b":{"tool_name":"second","tool_run_id":"run-b"}
    },
    "thinking_steps":[{"tool_run_id":"think-1","text":"reasoning"}],
    "invoked_skills":[{"skill_id":7,"name":"Release notes","icon_meta":{"name":"book"},"instructions":"worker only"}]
  }
}`)

	delta, recognized, err := decodeCurrentAgentTraceDelta(raw)
	if err != nil || !recognized {
		t.Fatalf("decode trace delta: recognized=%t err=%v", recognized, err)
	}
	if delta.streamID != "10000000-0000-4000-8000-000000000001" ||
		delta.messageID != "20000000-0000-4000-8000-000000000001" ||
		delta.executionGeneration != "30000000-0000-4000-8000-000000000001" ||
		delta.sioEvent != "chat_predict" {
		t.Fatalf("current correlation changed: %#v", delta)
	}
	if len(delta.toolCalls) != 2 || delta.toolCalls[0].key != "run-a" ||
		delta.toolCalls[1].key != "run-b" || len(delta.thinkingSteps) != 1 {
		t.Fatalf("current ordered deltas changed: %#v", delta)
	}
	if string(delta.invokedSkills) != `[{"skill_id":7,"name":"Release notes","icon_meta":{"name":"book"}}]` {
		t.Fatalf("compact invoked skills changed: %s", delta.invokedSkills)
	}
}

func TestDecodeCurrentAgentTraceDeltaRejectsMissingRegenerationFence(t *testing.T) {
	_, recognized, err := decodeCurrentAgentTraceDelta(json.RawMessage(`{
  "type":"partial_message",
  "stream_id":"stream",
  "message_id":"message",
  "sio_event":"chat_predict",
  "response_metadata":{"tool_calls":{},"thinking_steps":[]}
}`))
	if err == nil || recognized {
		t.Fatalf("missing execution generation accepted: recognized=%t err=%v", recognized, err)
	}
}

func TestDecodeCurrentAgentTraceDeltaIgnoresUnrelatedNodeEvents(t *testing.T) {
	_, recognized, err := decodeCurrentAgentTraceDelta(
		json.RawMessage(`{"type":"agent_llm_chunk","content":"hello"}`),
	)
	if err != nil || recognized {
		t.Fatalf("unrelated event entered trace accumulator: recognized=%t err=%v", recognized, err)
	}
}

func TestCurrentAgentTraceMergeUpdatesRowsByRunAndPreservesLineage(t *testing.T) {
	started := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	oldOutput := ""
	existing := []currentAgentTraceRow{{
		id: 71, messageGroupID: 9, kind: "tool_call", runID: "tool-run",
		startedAt: &started, hasVisibleContent: true, toolName: "child-agent",
		toolOutput: &oldOutput,
		attrs: map[string]any{
			"metadata": map[string]any{
				"parent_agent_name": "orchestrator",
				"parent_agent_path": []any{
					map[string]any{"name": "orchestrator", "call_id": "outer-call"},
				},
			},
		},
	}}
	delta := currentAgentTraceDelta{toolCalls: []currentAgentToolCall{{
		key: "tool-run",
		entry: map[string]any{
			"tool_name":        "child-agent",
			"tool_run_id":      "tool-run",
			"run_id":           "tool-run",
			"tool_inputs":      map[string]any{"task": "inspect"},
			"tool_output":      "done",
			"finish_reason":    "stop",
			"timestamp_start":  started.Format(time.RFC3339Nano),
			"timestamp_finish": started.Add(time.Second).Format(time.RFC3339Nano),
			"metadata": map[string]any{
				"parent_agent_name":    "orchestrator",
				"parent_agent_call_id": "outer-call",
				"parent_agent_path": []any{
					map[string]any{"name": "orchestrator", "call_id": "outer-call"},
				},
			},
		},
	}}}

	desired, err := mergeCurrentAgentTraceRows(9, existing, delta)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 1 {
		t.Fatalf("tool start/end became %d rows", len(desired))
	}
	row := desired[0]
	if row.runID != "tool-run" || row.parentAgentName != "orchestrator" ||
		row.parentAgentCallID != "outer-call" || row.toolOutput == nil ||
		*row.toolOutput != "done" || row.finishedAt == nil ||
		row.finishReason != "stop" {
		t.Fatalf("tool completion mapping changed: %#v", row)
	}
	metadata := currentAgentMap(row.attrs, "metadata")
	if metadata["parent_agent_name"] != "orchestrator" ||
		metadata["parent_agent_call_id"] != "outer-call" {
		t.Fatalf("nested agent lineage was lost: %#v", row.attrs)
	}
}

func TestCurrentAgentHITLReplayDedupUsesCompletionEpochs(t *testing.T) {
	base := func(runID string, output any) currentAgentToolCall {
		return currentAgentToolCall{key: runID, entry: map[string]any{
			"tool_name":   "child-agent",
			"tool_run_id": runID,
			"tool_inputs": map[string]any{
				"task":           "review",
				"hitl_decisions": []any{map[string]any{"action": runID}},
			},
			"metadata": map[string]any{
				"parent_agent_name": "orchestrator",
				"checkpoint_ns":     "child-agent:" + runID,
			},
			"tool_output": output,
		}}
	}
	values := []currentAgentToolCall{
		base("replay-1", nil),
		base("replay-2", ""),
		base("completed-1", "first result"),
		base("completed-2", "second result"),
	}

	deduped := dedupeCurrentAgentToolCalls(values)
	if len(deduped) != 2 {
		t.Fatalf("two genuine invocations became %d rows: %#v", len(deduped), deduped)
	}
	if deduped[0].key != "completed-1" || deduped[1].key != "completed-2" {
		t.Fatalf("HITL replay survivor changed: %#v", deduped)
	}
}

func TestCurrentAgentHITLDedupKeepsParallelRootInstancesSeparate(t *testing.T) {
	call := func(runID, childThread string) currentAgentToolCall {
		return currentAgentToolCall{key: runID, entry: map[string]any{
			"tool_name":   "child-agent",
			"tool_run_id": runID,
			"tool_inputs": map[string]any{"task": "same"},
			"metadata": map[string]any{
				"parent_agent_name": "orchestrator",
				"child_thread_id":   childThread,
			},
		}}
	}
	deduped := dedupeCurrentAgentToolCalls([]currentAgentToolCall{
		call("parallel-a", "thread-a"),
		call("parallel-b", "thread-b"),
	})
	if len(deduped) != 2 {
		t.Fatalf("parallel root instances collapsed: %#v", deduped)
	}
}

func TestCurrentAgentThinkingDeltaReplacesRunAndPreservesSeparateRuns(t *testing.T) {
	old := []map[string]any{
		{"tool_run_id": "thinking-a", "text": "old"},
		{"tool_run_id": "thinking-b", "text": "other"},
	}
	merged, mergeErr := mergeCurrentAgentThinkingSteps(old, []map[string]any{
		{"tool_run_id": "thinking-a", "text": "new", "thinking": "private"},
	})
	if mergeErr != nil {
		t.Fatal(mergeErr)
	}
	if len(merged) != 2 || merged[0]["text"] != "new" || merged[1]["text"] != "other" {
		t.Fatalf("thinking delta accumulation changed: %#v", merged)
	}
	rows, err := mergeCurrentAgentTraceRows(7, nil, currentAgentTraceDelta{
		thinkingSteps: merged,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].kind != "thinking_step" ||
		rows[0].thinking == nil || *rows[0].thinking != "private" {
		t.Fatalf("thinking row mapping changed: %#v", rows)
	}
}
