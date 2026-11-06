package executions

import (
	"encoding/json"
	"testing"
)

func agentEvent(cursor uint64, data string) DurableEvent {
	return DurableEvent{
		Cursor: cursor, Type: nodeEventType, Data: json.RawMessage(data),
		Participant: &EventParticipant{ID: 7, AddressedTo: []int64{}},
	}
}

func decode(t *testing.T, data json.RawMessage) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("frame is not a JSON object: %v: %s", err, data)
	}
	return value
}

func TestParticipantStampAddsKeysAndDerivesStates(t *testing.T) {
	var states participantStates
	frames := []struct {
		data  string
		state string
	}{
		{`{"type":"agent_start","stream_id":"s","message_id":"m","sio_event":"chat_predict","response_metadata":{}}`, AgentStateThinking},
		{`{"type":"agent_llm_start","response_metadata":{}}`, ""},
		{`{"type":"agent_llm_chunk","content":"Hi","response_metadata":{}}`, AgentStateSpeaking},
		{`{"type":"agent_llm_chunk","content":" there","response_metadata":{}}`, ""},
		{`{"type":"agent_tool_start","response_metadata":{}}`, AgentStateWorking},
		{`{"type":"agent_llm_chunk","response_metadata":{"parent_agent_name":"sub"}}`, ""},
		{`{"type":"full_message","response_metadata":{"parent_agent_name":"sub"}}`, ""},
		{`{"type":"agent_tool_end","response_metadata":{}}`, AgentStateThinking},
		{`{"type":"agent_hitl_interrupt","response_metadata":{}}`, AgentStateWaiting},
		{`{"type":"full_message","response_metadata":{"finish_reason":"stop"}}`, AgentStateSuccess},
	}
	for index, frame := range frames {
		served, state := states.stamp(agentEvent(uint64(index+1), frame.data))
		body := decode(t, served.Data)
		if body["participant_id"] != float64(7) {
			t.Fatalf("frame %d: participant_id = %v", index, body["participant_id"])
		}
		if addressed, ok := body["addressed_to"].([]any); !ok || len(addressed) != 0 {
			t.Fatalf("frame %d: addressed_to = %v", index, body["addressed_to"])
		}
		if frame.state == "" {
			if state != nil {
				t.Fatalf("frame %d: unexpected state frame %s", index, state.Data)
			}
			continue
		}
		if state == nil {
			t.Fatalf("frame %d: no state frame, want %s", index, frame.state)
		}
		stateBody := decode(t, state.Data)
		if stateBody["type"] != agentStateFrameType || stateBody["state"] != frame.state ||
			stateBody["participant_id"] != float64(7) || state.Cursor != served.Cursor || state.Type != nodeEventType {
			t.Fatalf("frame %d: state frame %s at %d, want %s", index, state.Data, state.Cursor, frame.state)
		}
	}
}

func TestParticipantStampCarriesStreamIdentity(t *testing.T) {
	var states participantStates
	_, state := states.stamp(agentEvent(1, `{"type":"agent_start","stream_id":"s-1","message_id":"m-1","execution_generation":"g","response_metadata":{}}`))
	body := decode(t, state.Data)
	if body["stream_id"] != "s-1" || body["message_id"] != "m-1" || body["execution_generation"] != "g" {
		t.Fatalf("state frame = %v", body)
	}
}

func TestParticipantStampContinuationWakes(t *testing.T) {
	var states participantStates
	_, state := states.stamp(agentEvent(1, `{"type":"agent_start","sio_event":"chat_continue_predict","response_metadata":{}}`))
	if state == nil || decode(t, state.Data)["state"] != AgentStateWaking {
		t.Fatalf("a continuation start is not waking: %v", state)
	}
}

func TestParticipantStampFailureIsError(t *testing.T) {
	var states participantStates
	failed := DurableEvent{
		Cursor: 3, Type: executionFailedType, Data: json.RawMessage(`{"code":"CANCELLED","safe_message":"x","retryable":false}`),
		Participant: &EventParticipant{ID: 7},
	}
	served, state := states.stamp(failed)
	if string(served.Data) != string(failed.Data) {
		t.Fatalf("a failure event was rewritten: %s", served.Data)
	}
	if state == nil || decode(t, state.Data)["state"] != AgentStateError {
		t.Fatalf("a failed run is not in error: %v", state)
	}
}

func TestParticipantStampLeavesOtherEventsAlone(t *testing.T) {
	var states participantStates
	plain := DurableEvent{Cursor: 1, Type: nodeEventType, Data: json.RawMessage(`{"type":"agent_start"}`)}
	if served, state := states.stamp(plain); string(served.Data) != string(plain.Data) || state != nil {
		t.Fatalf("an event without a participant was changed")
	}
	stamped := agentEvent(2, `{"type":"agent_llm_chunk","participant_id":9,"response_metadata":{}}`)
	served, _ := states.stamp(stamped)
	if string(served.Data) != string(stamped.Data) {
		t.Fatalf("a worker-stamped frame was rewritten: %s", served.Data)
	}
	reset := DurableEvent{Cursor: 3, Type: "execution.replay_reset", Data: json.RawMessage(`{}`), Participant: &EventParticipant{ID: 7}}
	if served, state := states.stamp(reset); string(served.Data) != "{}" || state != nil {
		t.Fatalf("a reset event was changed")
	}
}

func TestAppendParticipantKeysEmptyObject(t *testing.T) {
	out, ok := appendParticipantKeys(json.RawMessage(`{ }`), 4, []int64{2, 3})
	if !ok || string(out) != `{"participant_id":4,"addressed_to":[2,3]}` {
		t.Fatalf("got %s %v", out, ok)
	}
}
