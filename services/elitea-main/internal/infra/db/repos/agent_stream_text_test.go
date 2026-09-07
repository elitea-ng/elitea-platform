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
