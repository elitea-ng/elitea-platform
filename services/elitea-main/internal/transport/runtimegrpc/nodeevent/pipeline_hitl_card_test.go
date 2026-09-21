package nodeevent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The card a PIPELINE attached to an agent as a tool raises (#973).
//
// The pipeline's own `hitl` node pauses inside the child graph, and the worker
// projects it through the nested projector — so the card looks like a
// `pipeline_hitl` guardrail wearing the `parent_agent_path` of the tool call it
// came from. Go stores and replays this frame verbatim; this pins that it
// survives the protobuf hop unchanged, and that the private pending checkpoint
// the worker keeps on its own persisted event is nowhere in it.
const nestedPipelineHITLCard = `{
  "type": "agent_hitl_interrupt",
  "stream_id": "conversation-1",
  "message_id": "message-1",
  "question_id": null,
  "content": "Review: ship it",
  "thinking": null,
  "response_metadata": {
    "thread_id": "thread-1",
    "chat_project_id": 7,
    "message": "Review: ship it",
    "interaction_type": "pipeline_hitl_node",
    "history_contract_version": 1,
    "node_name": "review",
    "available_actions": ["approve", "reject"],
    "routes": {"approve": "approved", "reject": "rejected"},
    "edit_state_key": null,
    "parent_agent_call_id": "call-1",
    "parent_agent_path": [{"name": "review-pipeline", "call_id": "call-1", "sibling_ordinal": 1}],
    "hitl_interrupt": {
      "type": "hitl",
      "interaction_type": "pipeline_hitl_node",
      "history_contract_version": 1,
      "interrupt_id": "pipeline-hitl-1",
      "call_digest": "sha256:0f",
      "guardrail_type": "pipeline_hitl",
      "node_name": "review",
      "message": "Review: ship it",
      "available_actions": ["approve", "reject"],
      "routes": {"approve": "approved", "reject": "rejected"},
      "edit_state_key": null,
      "definition_digest": "sha256:0e"
    },
    "hitl_interrupts": [{
      "type": "hitl",
      "interaction_type": "pipeline_hitl_node",
      "history_contract_version": 1,
      "interrupt_id": "pipeline-hitl-1",
      "call_digest": "sha256:0f",
      "guardrail_type": "pipeline_hitl",
      "node_name": "review",
      "message": "Review: ship it",
      "available_actions": ["approve", "reject"],
      "routes": {"approve": "approved", "reject": "rejected"},
      "edit_state_key": null,
      "definition_digest": "sha256:0e"
    }]
  },
  "references": [],
  "sio_event": "chat_predict",
  "created_at": "2026-09-21T12:00:00Z",
  "parent_message_id": null,
  "agent_name": "review-pipeline",
  "execution_generation": "generation-1"
}`

func TestNestedPipelineHITLCardRoundTripsWithoutLosingItsRouting(t *testing.T) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(nestedPipelineHITLCard)); err != nil {
		t.Fatalf("compact card: %v", err)
	}
	decoded, err := DecodeCurrentJSON(compact.Bytes())
	if err != nil {
		t.Fatalf("DecodeCurrentJSON() error = %v", err)
	}
	encoded, err := EncodeCurrentJSON(decoded)
	if err != nil {
		t.Fatalf("EncodeCurrentJSON() error = %v", err)
	}
	if !bytes.Equal(encoded, compact.Bytes()) {
		t.Fatalf("round trip changed the card:\n got %s\nwant %s", encoded, compact.Bytes())
	}

	var card struct {
		ResponseMetadata struct {
			HITLInterrupts []struct {
				InterruptID      string   `json:"interrupt_id"`
				GuardrailType    string   `json:"guardrail_type"`
				AvailableActions []string `json:"available_actions"`
			} `json:"hitl_interrupts"`
			ParentAgentCallID string `json:"parent_agent_call_id"`
		} `json:"response_metadata"`
	}
	if err := json.Unmarshal(encoded, &card); err != nil {
		t.Fatalf("re-read card: %v", err)
	}
	interrupts := card.ResponseMetadata.HITLInterrupts
	if len(interrupts) != 1 || interrupts[0].GuardrailType != "pipeline_hitl" ||
		interrupts[0].InterruptID != "pipeline-hitl-1" ||
		len(interrupts[0].AvailableActions) != 2 {
		t.Fatalf("the pipeline's own routing did not survive: %+v", interrupts)
	}
	// The card is what the browser answers with, so the identity that binds the
	// answer back to the paused tool call has to be on it.
	if card.ResponseMetadata.ParentAgentCallID != "call-1" {
		t.Fatalf("the card lost the tool call it belongs to: %+v", card)
	}
}

// The child's pending graph checkpoint is worker-private: it rides the
// worker's own persisted event so a later turn can re-enter the child graph,
// and it must never reach a browser frame. Asserted here as well as in the
// worker, because this codec is the last hop before the browser sees it.
func TestNestedPipelineHITLCardCarriesNoPendingCheckpoint(t *testing.T) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(nestedPipelineHITLCard)); err != nil {
		t.Fatalf("compact card: %v", err)
	}
	decoded, err := DecodeCurrentJSON(compact.Bytes())
	if err != nil {
		t.Fatalf("DecodeCurrentJSON() error = %v", err)
	}
	encoded, err := EncodeCurrentJSON(decoded)
	if err != nil {
		t.Fatalf("EncodeCurrentJSON() error = %v", err)
	}
	for _, forbidden := range []string{
		"elitea.pipeline_tool.pending",
		"pending_nodes",
		"checkpoint_id",
		"child_thread_id",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("the browser card carried %q: %s", forbidden, encoded)
		}
	}
}
