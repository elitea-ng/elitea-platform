package repos

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/nodeevent"
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
	merged := mergeCurrentAgentThinkingSteps(old, []map[string]any{
		{"tool_run_id": "thinking-a", "text": "new", "thinking": "private"},
	})
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

// chunkedToolOutputEvent renders one chunk exactly as a worker emits it: an
// ordinary node event, bound to the same turn as the partial messages around
// it, whose content is a slice of one tool call's output.
func chunkedToolOutputEvent(t *testing.T, chunk nodeevent.ToolOutputChunk) json.RawMessage {
	t.Helper()
	metadata, err := nodeevent.EncodeToolOutputChunkMetadata(chunk)
	if err != nil {
		t.Fatalf("encode chunk metadata: %v", err)
	}
	raw, err := json.Marshal(map[string]any{
		"type":                 nodeevent.ToolOutputChunkEventType,
		"stream_id":            "10000000-0000-4000-8000-000000000001",
		"message_id":           "20000000-0000-4000-8000-000000000001",
		"execution_generation": "30000000-0000-4000-8000-000000000001",
		"sio_event":            "chat_predict",
		"content":              chunk.Text,
		"response_metadata":    json.RawMessage(metadata),
	})
	if err != nil {
		t.Fatalf("encode chunk event: %v", err)
	}
	return raw
}

// A tool result too large for one output frame arrives as chunks and is
// REASSEMBLED onto the row (#956) — and the completed tool call, which carries
// the count and the digest instead of the text, must not wipe it out.
func TestCurrentAgentTraceReassemblesChunkedToolOutput(t *testing.T) {
	output := ""
	for range 4_000 {
		output += "AUTOTESTMED the quick brown fox jumps over the lazy dog 0123456789\n"
	}
	chunks, err := nodeevent.SplitToolOutput("tool-run", output)
	if err != nil {
		t.Fatalf("split tool output: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("this case needs a multi-chunk output; got %d", len(chunks))
	}

	started := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	empty := ""
	rows := []currentAgentTraceRow{{
		id: 71, messageGroupID: 9, kind: "tool_call", runID: "tool-run",
		startedAt: &started, hasVisibleContent: true, toolName: "read_file",
		toolOutput: &empty,
	}}

	for _, chunk := range chunks {
		delta, recognized, err := decodeCurrentAgentTraceDelta(chunkedToolOutputEvent(t, chunk))
		if err != nil || !recognized {
			t.Fatalf("chunk %d: recognized=%t err=%v", chunk.Index, recognized, err)
		}
		if delta.outputChunk == nil || delta.outputChunk.Index != chunk.Index {
			t.Fatalf("chunk %d did not decode as a chunk delta: %#v", chunk.Index, delta)
		}
		desired, err := mergeCurrentAgentTraceRows(9, rows, delta)
		if err != nil {
			t.Fatalf("merge chunk %d: %v", chunk.Index, err)
		}
		if len(desired) != 1 {
			t.Fatalf("a chunk created %d rows", len(desired))
		}
		rows = []currentAgentTraceRow{desired[0]}
		rows[0].id = 71
	}
	if rows[0].toolOutput == nil || *rows[0].toolOutput != output {
		t.Fatalf("the reassembled output is %d bytes, want %d",
			len(stringOrEmpty(rows[0].toolOutput)), len(output))
	}

	// RE-PROJECTING an already-applied chunk must be a no-op: node events are
	// replayable, and a blind append would duplicate the text.
	replay, _, err := decodeCurrentAgentTraceDelta(chunkedToolOutputEvent(t, chunks[0]))
	if err != nil {
		t.Fatal(err)
	}
	afterReplay, err := mergeCurrentAgentTraceRows(9, rows, replay)
	if err != nil {
		t.Fatal(err)
	}
	if afterReplay[0].toolOutput == nil || *afterReplay[0].toolOutput != output {
		t.Fatalf("replaying a chunk changed the output (%d bytes, want %d)",
			len(stringOrEmpty(afterReplay[0].toolOutput)), len(output))
	}

	// The COMPLETED call names the chunk count and digest and carries no text.
	// The row must keep what the chunks delivered and record that it is whole.
	completion := currentAgentTraceDelta{
		streamID:            "10000000-0000-4000-8000-000000000001",
		messageID:           "20000000-0000-4000-8000-000000000001",
		executionGeneration: "30000000-0000-4000-8000-000000000001",
		sioEvent:            "chat_predict",
		toolCalls: []currentAgentToolCall{{
			key: "tool-run",
			entry: map[string]any{
				"tool_name": "read_file", "tool_run_id": "tool-run", "run_id": "tool-run",
				"tool_output":      "",
				"finish_reason":    "stop",
				"timestamp_start":  started.Format(time.RFC3339Nano),
				"timestamp_finish": started.Add(time.Second).Format(time.RFC3339Nano),
				"tool_output_chunks": map[string]any{
					"total":              int64(len(chunks)),
					"tool_output_sha256": nodeevent.ToolOutputDigest(output),
				},
			},
		}},
	}
	desired, err := mergeCurrentAgentTraceRows(9, rows, completion)
	if err != nil {
		t.Fatal(err)
	}
	if desired[0].toolOutput == nil || *desired[0].toolOutput != output {
		t.Fatalf("the completed call dropped the reassembled output (%d bytes, want %d)",
			len(stringOrEmpty(desired[0].toolOutput)), len(output))
	}
	if desired[0].finishReason != "stop" || desired[0].finishedAt == nil {
		t.Fatalf("the completion did not land: %#v", desired[0])
	}
	progress := currentAgentMap(desired[0].attrs, "tool_output_chunks")
	if complete, _ := progress["complete"].(bool); !complete {
		t.Fatalf("a whole reassembled output was not recorded as complete: %#v", progress)
	}
}

// A chunk that never arrives leaves a row that SAYS the output is partial,
// rather than one that looks like a complete short result.
func TestCurrentAgentTraceMarksAnIncompleteChunkedOutput(t *testing.T) {
	output := ""
	for range 4_000 {
		output += "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"
	}
	chunks, err := nodeevent.SplitToolOutput("tool-run", output)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	empty := ""
	rows := []currentAgentTraceRow{{
		id: 71, messageGroupID: 9, kind: "tool_call", runID: "tool-run",
		startedAt: &started, hasVisibleContent: true, toolName: "read_file",
		toolOutput: &empty,
	}}
	// Everything but the last chunk.
	for _, chunk := range chunks[:len(chunks)-1] {
		delta, _, err := decodeCurrentAgentTraceDelta(chunkedToolOutputEvent(t, chunk))
		if err != nil {
			t.Fatal(err)
		}
		desired, err := mergeCurrentAgentTraceRows(9, rows, delta)
		if err != nil {
			t.Fatal(err)
		}
		rows = []currentAgentTraceRow{desired[0]}
		rows[0].id = 71
	}
	completion := currentAgentTraceDelta{
		toolCalls: []currentAgentToolCall{{
			key: "tool-run",
			entry: map[string]any{
				"tool_name": "read_file", "tool_run_id": "tool-run", "run_id": "tool-run",
				"tool_output": "",
				"tool_output_chunks": map[string]any{
					"total":              int64(len(chunks)),
					"tool_output_sha256": nodeevent.ToolOutputDigest(output),
				},
			},
		}},
	}
	desired, err := mergeCurrentAgentTraceRows(9, rows, completion)
	if err != nil {
		t.Fatal(err)
	}
	progress := currentAgentMap(desired[0].attrs, "tool_output_chunks")
	if complete, _ := progress["complete"].(bool); complete {
		t.Fatalf("a short output was recorded as complete: %#v", progress)
	}
	if desired[0].toolOutput == nil || len(*desired[0].toolOutput) >= len(output) {
		t.Fatalf("the partial output was not kept as what it is: %#v", progress)
	}
}

func stringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
