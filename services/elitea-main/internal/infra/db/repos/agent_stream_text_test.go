package repos

import (
	"fmt"
	"testing"
)

func TestDecodeCurrentAgentTextDeltaAcceptsVisibleModelChunk(t *testing.T) {
	delta, recognized, err := decodeCurrentAgentTextDelta([]byte(`{
  "type":"agent_llm_chunk",
  "stream_id":"conversation-1",
  "message_id":"message-1",
  "execution_generation":"generation-1",
  "sio_event":"chat_predict",
  "content":"partial answer"
}`))
	if err != nil || !recognized || delta.content != "partial answer" ||
		delta.streamID != "conversation-1" || delta.messageID != "message-1" {
		t.Fatalf("decoded text delta = %+v recognized=%t error=%v", delta, recognized, err)
	}
}

func TestDecodeCurrentAgentTextDeltaDoesNotPublishChildTextAsParentAnswer(t *testing.T) {
	for name, metadata := range map[string]string{
		"path":            `{"parent_agent_path":[{"name":"Name Resolver","call_id":"name-call"}]}`,
		"name":            `{"parent_agent_name":"Name Resolver"}`,
		"nested metadata": `{"metadata":{"parent_agent_path":[{"name":"Name Resolver","call_id":"name-call"}]}}`,
		"tool metadata":   `{"tool_meta":{"metadata":{"parent_agent_name":"Name Resolver"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			event := fmt.Sprintf(`{"type":"agent_llm_chunk","stream_id":"conversation-1",
"message_id":"message-1","execution_generation":"generation-1","sio_event":"chat_predict",
"content":"child result","response_metadata":%s}`, metadata)
			if _, recognized, err := decodeCurrentAgentTextDelta([]byte(event)); err != nil || recognized {
				t.Fatalf("child text recognized=%t error=%v", recognized, err)
			}
		})
	}
}

func TestDecodeCurrentAgentTextDeltaIgnoresOtherEventsAndNullContent(t *testing.T) {
	if _, recognized, err := decodeCurrentAgentTextDelta([]byte(`{"type":"agent_llm_start"}`)); err != nil || recognized {
		t.Fatalf("non-text event recognized=%t error=%v", recognized, err)
	}
	delta, recognized, err := decodeCurrentAgentTextDelta([]byte(`{
  "type":"agent_llm_chunk",
  "stream_id":"conversation-1",
  "message_id":"message-1",
  "execution_generation":"generation-1",
  "sio_event":"chat_continue_predict",
  "content":null
}`))
	if err != nil || !recognized || delta.content != "" {
		t.Fatalf("null-content delta = %+v recognized=%t error=%v", delta, recognized, err)
	}
}

func TestDecodeCurrentAgentTextDeltaRejectsInvalidCorrelationAndContent(t *testing.T) {
	for name, event := range map[string]string{
		"missing binding": `{"type":"agent_llm_chunk","content":"text"}`,
		"object content": `{
  "type":"agent_llm_chunk",
  "stream_id":"conversation-1",
  "message_id":"message-1",
  "execution_generation":"generation-1",
  "sio_event":"chat_predict",
  "content":{"text":"not a string"}
}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeCurrentAgentTextDelta([]byte(event)); err == nil {
				t.Fatal("invalid text event was accepted")
			}
		})
	}
}

func TestDecodeCurrentAgentResultChunkRequiresDistinctTypeAndNonemptyContent(t *testing.T) {
	for _, event := range []string{
		`{"type":"agent_llm_chunk","content":"x","response_metadata":{"result_chunk_v1":{}}}`,
		`{"type":"agent_result_chunk","content":"x"}`,
		`{"type":"agent_result_chunk","content":null,"response_metadata":{"result_chunk_v1":{}}}`,
		`{"type":"agent_result_chunk","content":"","response_metadata":{"result_chunk_v1":{}}}`,
	} {
		if _, _, err := decodeCurrentAgentTextDelta([]byte(event)); err == nil {
			t.Fatal("invalid result event was accepted")
		}
	}
}

func TestDecodeCurrentAgentReplacementStartRequiresExplicitRootDecision(t *testing.T) {
	for _, test := range []struct {
		name, metadata string
		reset          bool
	}{
		{"replacement", `{"should_continue":false}`, true},
		{"continuation", `{"should_continue":true}`, false},
		{"legacy", `{}`, false},
		{"null", `{"should_continue":null}`, false},
		{"child", `{"should_continue":false,"parent_agent_name":"child"}`, false},
		{"child path", `{"should_continue":false,"parent_agent_path":[{"call_id":"child"}]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"type":"agent_start","stream_id":"conversation-1","message_id":"message-1",
"execution_generation":"generation-1","sio_event":"chat_predict","content":null,"response_metadata":%s}`, test.metadata)
			delta, recognized, err := decodeCurrentAgentTextDelta([]byte(raw))
			if err != nil || recognized != test.reset || delta.resetProvisional != test.reset {
				t.Fatalf("reset=%t recognized=%t err=%v", delta.resetProvisional, recognized, err)
			}
		})
	}
	if _, _, err := decodeCurrentAgentTextDelta([]byte(`{"type":"agent_start","response_metadata":{"should_continue":false}}`)); err == nil {
		t.Fatal("uncorrelated reset accepted")
	}
}
