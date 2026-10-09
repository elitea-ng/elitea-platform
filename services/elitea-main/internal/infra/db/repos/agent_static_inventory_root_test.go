package repos

import (
	"encoding/json"
	"strings"
	"testing"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
)

func TestStaticInventoryFullMessageRootKinds(t *testing.T) {
	var source map[string]json.RawMessage
	if json.Unmarshal([]byte(staticMessageMetadataFixture()), &source) != nil {
		t.Fatal("invalid fixture")
	}
	proof := source["pipeline_static_v1"]
	delete(source, "pipeline_static_v1")
	source["pipeline_static_tools_v1"] = json.RawMessage(`{"revision":1,"pauses":[{"tool_call_id":"original-call","child_thread_id":"original-child","original_batch_event_id":"original-batch","original_ordinal":16,"proof":` + string(proof) + `}]}`)
	for _, tc := range []struct {
		name  string
		app   string
		allow bool
	}{
		{"agent", `{"agent_type":"agent"}`, true},
		{"pipeline", `{"agent_type":"pipeline"}`, true},
		{"pipeline version only", `{"version_details":{"agent_type":"pipeline"}}`, true},
		{"agent version only", `{"version_details":{"agent_type":"agent"}}`, true},
		{"consistent pipeline", `{"agent_type":"pipeline","version_details":{"agent_type":"pipeline"}}`, true},
		{"contradictory", `{"agent_type":"pipeline","version_details":{"agent_type":"agent"}}`, false},
		{"unknown outer", `{"agent_type":"openai","version_details":{"agent_type":"agent"}}`, false},
		{"unknown version", `{"agent_type":"pipeline","version_details":{"agent_type":"openai"}}`, false},
		{"missing", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source["application_details"] = json.RawMessage(tc.app)
			raw, err := json.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			message, err := decodeCurrentAgentFullMessage([]byte(`"paused"`), []byte(`[]`), raw)
			if !tc.allow {
				if err == nil {
					t.Fatal("unsupported metadata accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			writer := &currentAgentTerminalWriterStub{existingSkills: `[]`}
			if err := persistCurrentAgentTerminal(t.Context(), writer, outputapp.ExpectedAgentExecution{ExecutionID: "original-execution", Generation: 3}, currentAgentTerminal{FullMessage: &message}); err != nil {
				t.Fatal(err)
			}
			if string(writer.full.PipelineStaticTools) != string(source["pipeline_static_tools_v1"]) || !strings.Contains(string(writer.full.PipelineStaticTools), `"original_ordinal":16`) {
				t.Fatal("original inventory changed during persistence")
			}
			if message.ReplacePipelineProvisional != strings.Contains(tc.app, `"pipeline"`) {
				t.Fatal("graph provisional cleanup changed")
			}
		})
	}
}

func TestStaticRootFullMessageKindConsistencyPreservesOrdinaryMetadata(t *testing.T) {
	for _, app := range []string{
		`{"agent_type":"pipeline","version_details":{"agent_type":"agent"}}`,
		`{"agent_type":"agent","version_details":{"agent_type":"pipeline"}}`,
		`{"agent_type":"pipeline","version_details":{"agent_type":"unknown"}}`,
	} {
		var source map[string]json.RawMessage
		if json.Unmarshal([]byte(staticMessageMetadataFixture()), &source) != nil {
			t.Fatal("bad fixture")
		}
		source["application_details"] = json.RawMessage(app)
		raw, _ := json.Marshal(source)
		if _, err := decodeCurrentAgentFullMessage([]byte(`"paused"`), []byte(`[]`), raw); err == nil {
			t.Fatal("contradictory root proof accepted")
		}
		delete(source, "pipeline_static_v1")
		raw, _ = json.Marshal(source)
		if _, err := decodeCurrentAgentFullMessage([]byte(`"ordinary output"`), []byte(`[]`), raw); err != nil {
			t.Fatalf("ordinary metadata compatibility changed: %v", err)
		}
	}
}
