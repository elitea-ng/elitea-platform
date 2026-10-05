package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"strings"
	"testing"
)

func debugTraceFixture(status string) (currentAgentCodeDebug, currentAgentToolCall) {
	p := currentAgentCodeDebug{Revision: 1, OriginalVisit: code.OriginalVisitRef{VisitID: strings.Repeat("1", 64), Revision: 1, DigestSHA256: strings.Repeat("2", 64)}, Attempt: 1, ExecutionID: "execution-1", Generation: "7", NodeID: "Code_1", ActivationID: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64), Status: status}
	if status == "committed" {
		p.Artifact = &storage.CodeDebugArtifactReference{SchemaVersion: storage.CodeDebugArtifactSchema, ProjectID: 7, Bucket: "code-debug", Name: strings.Repeat("c", 64) + ".json", MediaType: "application/json", ByteLength: 10, SHA256: strings.Repeat("d", 64)}
	}
	return p, debugTraceEntry(p)
}
func debugTraceEntry(p currentAgentCodeDebug) currentAgentToolCall {
	raw, _ := json.Marshal(p)
	var proof map[string]any
	_ = json.Unmarshal(raw, &proof)
	metadata := map[string]any{"langgraph_node": p.NodeID, "original_name": p.NodeID, "node_type": "code", currentAgentCodeDebugKey: proof}
	e := map[string]any{"run_id": p.runID(), "tool_run_id": p.runID(), "tool_name": p.NodeID + " / debug export", "tool_meta": map[string]any{"name": p.NodeID + " / debug export", "metadata": sanitizeCurrentAgentJSON(metadata)}, "metadata": metadata, "tool_inputs": map[string]any{}, "tool_output": nil, "error": nil, "finish_reason": "stop", "timestamp_start": "2026-10-04T12:00:00Z", "timestamp_finish": "2026-10-04T12:00:00Z"}
	return currentAgentToolCall{key: p.runID(), entry: e}
}
func TestCodeDebugWarningTraceRequiresSignedIdentityAndNoPayload(t *testing.T) {
	_, call := debugTraceFixture("unavailable")
	if err := validateCurrentAgentCodeDebugDelta(context.Background(), nil, currentAgentTraceDelta{toolCalls: []currentAgentToolCall{call}}, codeTraceFrame()); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(map[string]any){"state-output": func(e map[string]any) { e["tool_output"] = map[string]any{"secret": "value"} }, "state-input": func(e map[string]any) { e["tool_inputs"] = map[string]any{"secret": "value"} }, "error": func(e map[string]any) { e["error"] = "private" }, "tool-name": func(e map[string]any) { currentAgentMap(e, "tool_meta")["name"] = "other" }, "unfinished": func(e map[string]any) { e["timestamp_finish"] = nil }, "source-proof": func(e map[string]any) {
		currentAgentMap(currentAgentMap(e, "metadata"), currentAgentCodeDebugKey)["source"] = "private"
	}}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			_, call := debugTraceFixture("unavailable")
			mutate(call.entry)
			if validateCurrentAgentCodeDebugDelta(context.Background(), nil, currentAgentTraceDelta{toolCalls: []currentAgentToolCall{call}}, codeTraceFrame()) == nil {
				t.Fatal("invalid trace accepted")
			}
		})
	}
}
func TestCodeDebugReplayKeepsCommittedArtifactAndStableOriginalIdentity(t *testing.T) {
	p, committed := debugTraceFixture("committed")
	id := p.runID()
	p.Status = "unavailable"
	p.Artifact = nil
	warning := debugTraceEntry(p)
	if id != warning.key {
		t.Fatal("replacement duplicated run")
	}
	merged, err := mergeCurrentAgentCodeDebug(committed.entry, warning.entry)
	if err != nil {
		t.Fatal(err)
	}
	proof, _, err := currentAgentCodeDebugProof(merged)
	if err != nil || proof.Artifact == nil || proof.Generation != "7" {
		t.Fatal("replacement erased immutable receipt")
	}
	q, _ := debugTraceFixture("committed")
	q.Artifact.Name = strings.Repeat("e", 64) + ".json"
	changed := debugTraceEntry(q)
	if _, err = mergeCurrentAgentCodeDebug(committed.entry, changed.entry); err == nil {
		t.Fatal("artifact replay changed key")
	}
	q.Generation = "8"
	changed = debugTraceEntry(q)
	if id == changed.key {
		t.Fatal("new generation inherited trace identity")
	}
	if _, err = mergeCurrentAgentCodeDebug(committed.entry, changed.entry); err == nil {
		t.Fatal("new generation inherited immutable receipt")
	}
	q.Generation = "7"
	q.ActivationID = strings.Repeat("f", 64)
	changed = debugTraceEntry(q)
	if _, err = mergeCurrentAgentCodeDebug(committed.entry, changed.entry); err == nil {
		t.Fatal("another visit replaced receipt")
	}
	if _, err = mergeCurrentAgentCodeDebug(warning.entry, committed.entry); err != nil {
		t.Fatal("later commit did not replace warning")
	}
}

func TestCodeDebugCommittedTraceRequiresExactGenerationScopedReceipt(t *testing.T) {
	p, call := debugTraceFixture("committed")
	a := storage.CodeDebugAdmission{OriginalVisit: code.OriginalVisitRef{VisitID: strings.Repeat("1", 64), Revision: 1, DigestSHA256: strings.Repeat("2", 64)}, Attempt: p.Attempt, SchemaVersion: storage.CodeDebugAdmissionSchema, NodeID: p.NodeID, GraphThreadID: "thread", GraphStep: "2", ActivationID: p.ActivationID, DefinitionSHA256: strings.Repeat("e", 64), YAMLSHA256: strings.Repeat("f", 64), ConfigurationJSON: `{"id":"Code_1","type":"code","language":"python","code":{"type":"fixed","value":"7"},"input":[],"output":[],"structured_output":false,"debug":true,"transition":null}`, RequestSHA256: p.RequestSHA256, SourceSHA256: strings.Repeat("3", 64), InputSHA256: strings.Repeat("4", 64), SnapshotSHA256: p.Artifact.SHA256, ByteLength: p.Artifact.ByteLength}
	raw, _ := json.Marshal(a)
	frame := codeTraceFrame()
	frame.TenantID = "7"
	frame.ResourceProjectID = "7"
	tx := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{p.Artifact.Name, raw}}}}
	if err := validateCurrentAgentCodeDebugDelta(context.Background(), tx, currentAgentTraceDelta{toolCalls: []currentAgentToolCall{call}}, frame); err != nil {
		t.Fatal(err)
	}
	args := tx.rowCalls[0].args
	if len(args) != 7 || args[0] != "7" || args[1] != int64(7) || args[2] != p.ExecutionID || args[3] != int64(7) || !bytes.Equal(args[4].([]byte), codeDebugBytes(p.OriginalVisit.VisitID)) || args[5] != int64(p.OriginalVisit.Revision) || !bytes.Equal(args[6].([]byte), codeDebugBytes(p.OriginalVisit.DigestSHA256)) {
		t.Fatal("receipt lookup lost exact original generation/scope")
	}
	for name, row := range map[string]scriptedRow{"missing": {err: errors.New("absent")}, "other-key": {values: []any{strings.Repeat("e", 64) + ".json", raw}}, "corrupt": {values: []any{p.Artifact.Name, []byte("null")}}} {
		t.Run(name, func(t *testing.T) {
			tx := &scriptedExecutor{rowResults: []scriptedRow{row}}
			if validateCurrentAgentCodeDebugDelta(context.Background(), tx, currentAgentTraceDelta{toolCalls: []currentAgentToolCall{call}}, frame) == nil {
				t.Fatal("invalid committed receipt accepted")
			}
		})
	}
	changed := a
	changed.RequestSHA256 = strings.Repeat("5", 64)
	wrong, _ := json.Marshal(changed)
	tx = &scriptedExecutor{rowResults: []scriptedRow{{values: []any{p.Artifact.Name, wrong}}}}
	if validateCurrentAgentCodeDebugDelta(context.Background(), tx, currentAgentTraceDelta{toolCalls: []currentAgentToolCall{call}}, frame) == nil {
		t.Fatal("receipt rebound to another original request")
	}
}

func TestCodeDebugDistinctRetryKeepsHistoricalReferenceOutOfCurrentVisit(t *testing.T) {
	first, firstCall := debugTraceFixture("committed")
	retry := first
	retry.OriginalVisit.VisitID = strings.Repeat("3", 64)
	retry.OriginalVisit.DigestSHA256 = strings.Repeat("4", 64)
	retry.Attempt = 2
	retry.Status = "unavailable"
	retry.Artifact = nil
	retryCall := debugTraceEntry(retry)
	if firstCall.key == retryCall.key {
		t.Fatal("new retry reused historical trace identity")
	}
	if _, err := mergeCurrentAgentCodeDebug(firstCall.entry, retryCall.entry); err == nil {
		t.Fatal("new retry displayed old committed artifact as current")
	}
	proof, _, err := currentAgentCodeDebugProof(retryCall.entry)
	if err != nil || proof.Artifact != nil || proof.Attempt != 2 || proof.OriginalVisit != retry.OriginalVisit {
		t.Fatal("retry label or empty publication changed")
	}
	// A separate trace entry retains the old immutable reference as history.
	history, _, err := currentAgentCodeDebugProof(firstCall.entry)
	if err != nil || history.Artifact == nil || history.Attempt != 1 || history.OriginalVisit != first.OriginalVisit {
		t.Fatal("history was erased or relabeled")
	}
	for name, mutate := range map[string]func(*storage.CodeDebugAdmission){
		"visit":   func(a *storage.CodeDebugAdmission) { a.OriginalVisit = retry.OriginalVisit },
		"attempt": func(a *storage.CodeDebugAdmission) { a.Attempt = retry.Attempt },
	} {
		t.Run(name, func(t *testing.T) {
			a := storage.CodeDebugAdmission{OriginalVisit: first.OriginalVisit, Attempt: 1, SchemaVersion: storage.CodeDebugAdmissionSchema, NodeID: first.NodeID, GraphThreadID: "thread", GraphStep: "2", ActivationID: first.ActivationID, DefinitionSHA256: strings.Repeat("e", 64), YAMLSHA256: strings.Repeat("f", 64), ConfigurationJSON: `{"id":"Code_1","type":"code","debug":true}`, RequestSHA256: first.RequestSHA256, SourceSHA256: strings.Repeat("5", 64), InputSHA256: strings.Repeat("6", 64), SnapshotSHA256: first.Artifact.SHA256, ByteLength: first.Artifact.ByteLength}
			mutate(&a)
			raw, _ := json.Marshal(a)
			tx := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{first.Artifact.Name, raw}}}}
			frame := codeTraceFrame()
			frame.TenantID = "7"
			frame.ResourceProjectID = "7"
			if validateCurrentAgentCodeDebugDelta(context.Background(), tx, currentAgentTraceDelta{toolCalls: []currentAgentToolCall{firstCall}}, frame) == nil {
				t.Fatal("receipt from another visit/attempt accepted")
			}
		})
	}
}
