package repos

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func codeTraceFixture(status string) (currentAgentCodeLifecycle, currentAgentToolCall) {
	proof := currentAgentCodeLifecycle{Revision: 1, ExecutionID: "execution-1", Generation: "7", ActivationID: strings.Repeat("a", 64), NodeID: "Code_1", GraphThreadID: "root/child/grandchild", GraphStep: "4", Language: "python", Phase: "execution", Status: status}
	entry := codeTraceEntry(proof)
	return proof, currentAgentToolCall{key: proof.runID(), entry: entry}
}
func codeTraceEntry(proof currentAgentCodeLifecycle) map[string]any {
	encoded, _ := json.Marshal(proof)
	var metadataProof map[string]any
	_ = json.Unmarshal(encoded, &metadataProof)
	metadata := map[string]any{"langgraph_node": proof.NodeID, "original_name": proof.NodeID, "node_type": "code", "language": proof.Language, currentAgentCodeTraceKey: metadataProof,
		"parent_agent_call_id": "original-parent-call", "parent_agent_path": []any{map[string]any{"name": "root-agent", "call_id": "original-parent-call"}}}
	entry := map[string]any{"run_id": proof.runID(), "tool_run_id": proof.runID(), "tool_name": proof.NodeID + " / " + proof.Phase,
		"metadata": metadata, "tool_meta": map[string]any{"name": proof.NodeID + " / " + proof.Phase, "metadata": sanitizeCurrentAgentJSON(metadata)},
		"tool_inputs": map[string]any{}, "tool_output": nil, "timestamp_start": "2026-10-02T12:00:00Z", "timestamp_finish": nil, "finish_reason": nil, "error": nil}
	if proof.Status != "started" {
		entry["timestamp_finish"] = "2026-10-02T12:00:01Z"
		entry["finish_reason"] = "stop"
	}
	if proof.Status == "failed" {
		entry["finish_reason"] = "error"
		entry["error"] = "The Code phase could not be completed."
	}
	return entry
}
func codeTraceFrame() outputapp.NodeEventFrame {
	return outputapp.NodeEventFrame{Fence: runtimedomain.Fence{ExecutionID: "execution-1", Generation: 7}}
}

func TestCodeTracePhaseMetadataMatchesSignedFrameForAllLanguages(t *testing.T) {
	for _, language := range []string{"python", "javascript", "typescript", "rust"} {
		for _, phase := range []string{"preparation", "hydration", "execution"} {
			for _, status := range []string{"started", "completed", "failed"} {
				t.Run(language+"/"+phase+"/"+status, func(t *testing.T) {
					proof, _ := codeTraceFixture(status)
					proof.Language = language
					proof.Phase = phase
					delta := currentAgentTraceDelta{toolCalls: []currentAgentToolCall{{key: proof.runID(), entry: codeTraceEntry(proof)}}}
					if err := validateCurrentAgentCodeTraceDelta(delta, codeTraceFrame()); err != nil {
						t.Fatal(err)
					}
					rows, err := mergeCurrentAgentTraceRows(17, nil, delta)
					if err != nil || len(rows) != 1 || rows[0].kind != "tool_call" || rows[0].toolOutput != nil {
						t.Fatalf("rows=%#v err=%v", rows, err)
					}
					stored := currentAgentMap(rows[0].attrs, "metadata")
					parsed, err := decodeCurrentAgentCodeLifecycle(stored[currentAgentCodeTraceKey])
					if err != nil || parsed != proof {
						t.Fatalf("proof dropped or changed: %#v %v", parsed, err)
					}
					if stored["parent_agent_call_id"] != "original-parent-call" {
						t.Fatal("scoped ancestry lost")
					}
				})
			}
		}
	}
}

func TestCodeTraceRejectsForgedFrameAndPayloadMetadata(t *testing.T) {
	mutations := map[string]func(*currentAgentCodeLifecycle, map[string]any){
		"execution":            func(p *currentAgentCodeLifecycle, _ map[string]any) { p.ExecutionID = "other-execution" },
		"generation":           func(p *currentAgentCodeLifecycle, _ map[string]any) { p.Generation = "8" },
		"zero-generation":      func(p *currentAgentCodeLifecycle, _ map[string]any) { p.Generation = "0" },
		"noncanonical-step":    func(p *currentAgentCodeLifecycle, _ map[string]any) { p.GraphStep = "04" },
		"long-thread":          func(p *currentAgentCodeLifecycle, _ map[string]any) { p.GraphThreadID = strings.Repeat("x", 513) },
		"node-control":         func(p *currentAgentCodeLifecycle, _ map[string]any) { p.NodeID = "Code\n1" },
		"uppercase-activation": func(p *currentAgentCodeLifecycle, _ map[string]any) { p.ActivationID = strings.Repeat("A", 64) },
		"language":             func(p *currentAgentCodeLifecycle, _ map[string]any) { p.Language = "sh" },
		"source-field": func(_ *currentAgentCodeLifecycle, e map[string]any) {
			currentAgentMap(currentAgentMap(e, "metadata"), currentAgentCodeTraceKey)["source"] = "private-source"
		},
		"metadata-conflict": func(_ *currentAgentCodeLifecycle, e map[string]any) {
			currentAgentMap(currentAgentMap(currentAgentMap(e, "tool_meta"), "metadata"), currentAgentCodeTraceKey)["graph_step"] = "5"
		},
		"state-input": func(_ *currentAgentCodeLifecycle, e map[string]any) {
			e["tool_inputs"] = map[string]any{"private_state": "secret"}
		},
		"payload-output":         func(_ *currentAgentCodeLifecycle, e map[string]any) { e["tool_output"] = "private output" },
		"unconfirmed-completion": func(_ *currentAgentCodeLifecycle, e map[string]any) { e["timestamp_finish"] = nil },
		"raw-error":              func(_ *currentAgentCodeLifecycle, e map[string]any) { e["error"] = "registry credentials" },
		"wrong-run":              func(_ *currentAgentCodeLifecycle, e map[string]any) { e["run_id"] = "another-run" },
	}
	for name, mutation := range mutations {
		t.Run(name, func(t *testing.T) {
			proof, _ := codeTraceFixture("completed")
			entry := codeTraceEntry(proof)
			mutation(&proof, entry)
			if name == "execution" || name == "generation" || name == "zero-generation" || name == "noncanonical-step" || name == "long-thread" || name == "node-control" || name == "uppercase-activation" || name == "language" {
				entry = codeTraceEntry(proof)
			}
			err := validateCurrentAgentCodeTraceDelta(currentAgentTraceDelta{toolCalls: []currentAgentToolCall{{key: proof.runID(), entry: entry}}}, codeTraceFrame())
			if err == nil {
				t.Fatal("invalid Code trace accepted")
			}
		})
	}
}

func TestCodeTraceReplayDoesNotDuplicateOrEraseTerminalRows(t *testing.T) {
	_, start := codeTraceFixture("started")
	rows, err := mergeCurrentAgentTraceRows(17, nil, currentAgentTraceDelta{toolCalls: []currentAgentToolCall{start}})
	if err != nil {
		t.Fatal(err)
	}
	_, end := codeTraceFixture("completed")
	rows, err = mergeCurrentAgentTraceRows(17, rows, currentAgentTraceDelta{toolCalls: []currentAgentToolCall{end}})
	if err != nil {
		t.Fatal(err)
	}
	start.entry["timestamp_start"] = "2026-10-02T12:00:02Z"
	for _, replay := range []currentAgentToolCall{end, start, end} {
		rows, err = mergeCurrentAgentTraceRows(17, rows, currentAgentTraceDelta{toolCalls: []currentAgentToolCall{replay}})
		if err != nil || len(rows) != 1 || rows[0].finishedAt == nil || rows[0].isError {
			t.Fatalf("terminal replay: %#v %v", rows, err)
		}
	}
	if rows[0].startedAt.Format(time.RFC3339) != "2026-10-02T12:00:00Z" {
		t.Fatal("recovery reset start")
	}
}

func TestCodeTraceConflictingTerminalAndIdentityFailClosed(t *testing.T) {
	_, end := codeTraceFixture("completed")
	_, failure := codeTraceFixture("failed")
	if _, err := mergeCurrentAgentCodeTrace(end.entry, failure.entry); err == nil {
		t.Fatal("terminal conflict accepted")
	}
	proof, _ := codeTraceFixture("completed")
	proof.GraphStep = "5"
	if _, err := mergeCurrentAgentCodeTrace(end.entry, codeTraceEntry(proof)); err == nil {
		t.Fatal("same activation changed graph step")
	}
	if _, err := mergeCurrentAgentCodeTrace(end.entry, map[string]any{"tool_name": "other"}); err == nil {
		t.Fatal("kind replacement accepted")
	}
}

func TestCodeTraceMixedSiblingAndLoopVisitRemainDistinct(t *testing.T) {
	_, first := codeTraceFixture("completed")
	secondProof, _ := codeTraceFixture("completed")
	secondProof.ActivationID = strings.Repeat("b", 64)
	secondProof.GraphStep = "5"
	second := currentAgentToolCall{key: secondProof.runID(), entry: codeTraceEntry(secondProof)}
	ordinary := currentAgentToolCall{key: "ordinary-sibling", entry: map[string]any{"tool_name": "search", "tool_run_id": "ordinary-sibling", "tool_output": "existing response"}}
	rows, err := mergeCurrentAgentTraceRows(17, nil, currentAgentTraceDelta{toolCalls: []currentAgentToolCall{ordinary, first, second}})
	if err != nil || len(rows) != 3 {
		t.Fatalf("mixed traces: %#v %v", rows, err)
	}
	rows, err = mergeCurrentAgentTraceRows(17, rows, currentAgentTraceDelta{toolCalls: []currentAgentToolCall{first}})
	if err != nil || len(rows) != 3 || rows[0].toolOutput == nil || *rows[0].toolOutput != "existing response" || rows[1].runID == rows[2].runID {
		t.Fatalf("sibling/loop changed: %#v %v", rows, err)
	}
}

func TestCodeTraceHashMatchesCrossLanguageVector(t *testing.T) {
	proof, _ := codeTraceFixture("started")
	if got := proof.runID(); got != "code-8233f911d1d8a5ee2ab8028e7c6ff62a336823bb7ea755524e4a845d3c987cab" {
		t.Fatalf("cross-language run ID changed: %s", got)
	}
}
