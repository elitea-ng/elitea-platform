package repos

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentToolOutputChunksSurviveTraceReconstruction(t *testing.T) {
	output := `{"text":"` + strings.Repeat("界\\\"", 16000) + `"}`
	if !json.Valid([]byte(output)) {
		t.Fatal("invalid fixture")
	}
	sum := sha256.Sum256([]byte(output))
	hash := hex.EncodeToString(sum[:])
	rows, err := mergeCurrentAgentTraceRows(1, nil, currentAgentTraceDelta{toolCalls: []currentAgentToolCall{{key: "call-1", entry: map[string]any{"run_id": "call-1", "tool_name": "lookup", "tool_inputs": map[string]any{"query": "keep"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(output); {
		end := offset + 6000
		if end > len(output) {
			end = len(output)
		}
		for end < len(output) && output[end]&0xc0 == 0x80 {
			end--
		}
		chunk := map[string]any{"offset_bytes": offset, "total_bytes": len(output), "sha256": hash, "final": end == len(output)}
		entry := map[string]any{"run_id": "call-1", "tool_name": "lookup", "tool_output": output[offset:end], "tool_output_chunk_v1": chunk}
		delta := currentAgentTraceDelta{toolCalls: []currentAgentToolCall{{key: "call-1", entry: entry}}}
		rows, err = mergeCurrentAgentTraceRows(1, rows, delta)
		if err != nil {
			t.Fatal(err)
		}
		rows, err = mergeCurrentAgentTraceRows(1, rows, delta)
		if err != nil {
			t.Fatalf("duplicate: %v", err)
		}
		offset = end
	}
	if len(rows) != 1 || rows[0].toolOutput == nil || *rows[0].toolOutput != output {
		t.Fatal("result lost or duplicated")
	}
	if rows[0].toolInputs.(map[string]any)["query"] != "keep" {
		t.Fatal("input lost")
	}
}

func TestAgentToolOutputChunksPreserveFinalErrorOnReplay(t *testing.T) {
	output := `{"error":"` + strings.Repeat("x", 10000) + `"}`
	sum := sha256.Sum256([]byte(output))
	hash := hex.EncodeToString(sum[:])
	var rows []currentAgentTraceRow
	for offset := 0; offset < len(output); offset += 6000 {
		end := min(offset+6000, len(output))
		entry := map[string]any{
			"run_id": "failed-call", "tool_name": "lookup", "tool_output": output[offset:end],
			"tool_output_chunk_v1": map[string]any{"offset_bytes": offset, "total_bytes": len(output), "sha256": hash, "final": end == len(output)},
		}
		if end == len(output) {
			entry["error"] = "Tool execution failed. See tool output."
			entry["finish_reason"] = "error"
		}
		delta := currentAgentTraceDelta{toolCalls: []currentAgentToolCall{{key: "failed-call", entry: entry}}}
		for range 2 {
			var err error
			rows, err = mergeCurrentAgentTraceRows(1, rows, delta)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(rows) != 1 || rows[0].toolOutput == nil || *rows[0].toolOutput != output || !rows[0].isError || rows[0].finishReason != "error" {
		t.Fatal("complete error output or terminal error state lost")
	}
}

func TestAgentToolOutputChunksRejectGapsConflictsAndWrongDigest(t *testing.T) {
	sum := sha256.Sum256([]byte(`{"ok":true}`))
	hash := hex.EncodeToString(sum[:])
	for _, tc := range []struct {
		name   string
		offset int
		digest string
		data   string
		final  bool
	}{
		{"gap", 2, hash, `ok`, false},
		{"wrong digest", 0, strings.Repeat("0", 64), `{"ok":true}`, true},
		{"premature final", 0, hash, `{`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mergeAgentToolOutputChunk(nil, map[string]any{"tool_output": tc.data, "tool_output_chunk_v1": map[string]any{"offset_bytes": tc.offset, "total_bytes": 11, "sha256": tc.digest, "final": tc.final}})
			if err == nil {
				t.Fatal("invalid chunk accepted")
			}
		})
	}
}

func TestPostgresAgentToolOutputChunksPersistAcrossTransactions(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentActivitySchemas(t, pool)
	const conversationID = "10000000-0000-4000-8000-000000000011"
	const responseID = "20000000-0000-4000-8000-000000000011"
	const generation = "30000000-0000-4000-8000-000000000011"
	admitted := admitPostgresAgentExecution(t, pool, conversationID, responseID, generation)
	seedCurrentAgentResponseGroup(t, pool, conversationID, responseID, generation, admitted.ExecutionID)
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	output := `{"value":"` + strings.Repeat("x", 100000) + `"}`
	sum := sha256.Sum256([]byte(output))
	hash := hex.EncodeToString(sum[:])
	for offset := 0; offset < len(output); offset += 8000 {
		end := offset + 8000
		if end > len(output) {
			end = len(output)
		}
		entry := map[string]any{"run_id": "large-result", "tool_run_id": "large-result", "tool_name": "lookup", "tool_output": output[offset:end], "tool_output_chunk_v1": map[string]any{"offset_bytes": offset, "total_bytes": len(output), "sha256": hash, "final": end == len(output)}}
		metadata, err := json.Marshal(map[string]any{"tool_calls": map[string]any{"large-result": entry}, "thinking_steps": []any{}})
		if err != nil {
			t.Fatal(err)
		}
		frame := currentAgentPartialFrame(admitted.ExecutionID, conversationID, responseID, generation, string(metadata))
		projectCurrentAgentFrame(t, store, &postgresCurrentAgentTraceProjector{}, frame)
		projectCurrentAgentFrame(t, store, &postgresCurrentAgentTraceProjector{}, frame)
	}
	var saved string
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*),max(tool_output) FROM p_1.chat_message_trace_step WHERE run_id='large-result'`).Scan(&count, &saved); err != nil {
		t.Fatal(err)
	}
	if count != 1 || saved != output {
		t.Fatal("database loses or duplicates tool result")
	}
}
