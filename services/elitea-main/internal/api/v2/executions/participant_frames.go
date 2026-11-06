package executions

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// The participant stamp and the `agent_state` frame (client contract 1.37,
// ADR-0050 decision 7b and 7e).
//
// Each node event of an agent run is served with `participant_id` (the chat
// participant the run answers as) and `addressed_to` (the answer's
// addressees; empty is everyone), the same values the persisted message
// carries as its author and chat_message_group.addressed_to. The keys are
// added here, on the way out, and never stored: the stored bytes are digested
// and a terminal frame's digest is the worker's result artifact.
//
// After a node event that moves the participant to a new state, the handler
// writes one `agent_state` frame. It is derived, so it has no cursor of its
// own: it carries its cause's, and a client that resumes after the cause
// gets the state again with the next transition.

const (
	nodeEventType       = "execution.node_event"
	executionFailedType = "execution.failed"
	agentStateFrameType = "agent_state"
)

// The agent_state states. `idle` is the state before a run's first frame
// and is never derived.
const (
	AgentStateIdle     = "idle"
	AgentStateThinking = "thinking"
	AgentStateSpeaking = "speaking"
	AgentStateWorking  = "working"
	AgentStateWaiting  = "waiting"
	AgentStateSuccess  = "success"
	AgentStateError    = "error"
	AgentStateWaking   = "waking"
)

// participantStates is the last state written per participant on one
// stream. The zero value is ready.
type participantStates struct {
	last map[int64]string
}

type participantFrameFields struct {
	Type                string          `json:"type"`
	StreamID            json.RawMessage `json:"stream_id"`
	MessageID           json.RawMessage `json:"message_id"`
	ExecutionGeneration json.RawMessage `json:"execution_generation"`
	SIOEvent            string          `json:"sio_event"`
	ResponseMetadata    struct {
		ParentAgentName string `json:"parent_agent_name"`
		FinishReason    string `json:"finish_reason"`
		IsError         bool   `json:"is_error"`
	} `json:"response_metadata"`
}

// stamp returns the event as it is served and the state frame to write
// after it, if the participant's state changed.
func (s *participantStates) stamp(event DurableEvent) (DurableEvent, *DurableEvent) {
	if event.Participant == nil || event.Participant.ID <= 0 {
		return event, nil
	}
	participant := *event.Participant
	addressedTo := participant.AddressedTo
	if addressedTo == nil {
		addressedTo = []int64{}
	}
	var fields participantFrameFields
	if event.Type == nodeEventType {
		if json.Unmarshal(event.Data, &fields) != nil {
			return event, nil
		}
		// The node event codec refuses unknown keys at ingest, so a stored
		// frame never names a participant itself.
		if stamped, ok := appendParticipantKeys(event.Data, participant.ID, addressedTo); ok {
			event.Data = stamped
		}
	} else if event.Type != executionFailedType {
		return event, nil
	}

	state := frameAgentState(event.Type, fields)
	if state == "" {
		return event, nil
	}
	if s.last == nil {
		s.last = make(map[int64]string, 1)
	}
	if s.last[participant.ID] == state {
		return event, nil
	}
	s.last[participant.ID] = state
	frame := map[string]any{
		"type":              agentStateFrameType,
		"participant_id":    participant.ID,
		"addressed_to":      addressedTo,
		"state":             state,
		"response_metadata": map[string]any{},
	}
	copyRawField(frame, "stream_id", fields.StreamID)
	copyRawField(frame, "message_id", fields.MessageID)
	copyRawField(frame, "execution_generation", fields.ExecutionGeneration)
	data, err := json.Marshal(frame)
	if err != nil {
		return event, nil
	}
	return event, &DurableEvent{Cursor: event.Cursor, Type: nodeEventType, Data: data}
}

// seed is the state frame a resumed stream starts with: the state the
// newest of the events before the resume cursor left the participant in
// (oldest first). It has no SSE id, so the client's resume cursor does not
// move back; nil when none of them moves a participant.
func (s *participantStates) seed(before []DurableEvent) *DurableEvent {
	var state *DurableEvent
	for _, event := range before {
		if _, next := s.stamp(event); next != nil {
			state = next
		}
	}
	if state != nil {
		state.Cursor = 0
	}
	return state
}

// frameAgentState is the state a frame moves its participant to; empty for a
// frame that does not move it.
func frameAgentState(eventType string, fields participantFrameFields) string {
	if eventType == executionFailedType {
		return AgentStateError
	}
	// A pause waits on a person, whoever in the agent tree asked.
	switch fields.Type {
	case "agent_hitl_interrupt", "agent_requires_confirmation", "agent_tool_paused", "mcp_authorization_required":
		return AgentStateWaiting
	}
	// Any other sub-agent frame: the participant has delegated and is
	// working, and a sub-agent's answer is not the participant's.
	if fields.ResponseMetadata.ParentAgentName != "" {
		if fields.Type == "full_message" {
			return ""
		}
		return AgentStateWorking
	}
	switch fields.Type {
	case "agent_start":
		// A continuation resumes a paused answer.
		if fields.SIOEvent == "chat_continue_predict" {
			return AgentStateWaking
		}
		return AgentStateThinking
	case "agent_llm_start", "agent_tool_end", "agent_tool_error":
		return AgentStateThinking
	case "agent_llm_chunk", "agent_result_chunk", "agent_response":
		return AgentStateSpeaking
	case "agent_tool_start":
		return AgentStateWorking
	case "agent_exception", "error", "llm_error":
		return AgentStateError
	case "full_message":
		if fields.ResponseMetadata.IsError || fields.ResponseMetadata.FinishReason == "error" {
			return AgentStateError
		}
		return AgentStateSuccess
	}
	return ""
}

// appendParticipantKeys adds the two keys at the end of a JSON object.
func appendParticipantKeys(data json.RawMessage, participantID int64, addressedTo []int64) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return nil, false
	}
	addressed, err := json.Marshal(addressedTo)
	if err != nil {
		return nil, false
	}
	body := bytes.TrimSpace(trimmed[1 : len(trimmed)-1])
	out := make([]byte, 0, len(trimmed)+48+len(addressed))
	out = append(out, '{')
	out = append(out, body...)
	if len(body) > 0 {
		out = append(out, ',')
	}
	out = append(out, `"participant_id":`...)
	out = strconv.AppendInt(out, participantID, 10)
	out = append(out, `,"addressed_to":`...)
	out = append(out, addressed...)
	out = append(out, '}')
	return out, true
}

func copyRawField(frame map[string]any, key string, value json.RawMessage) {
	if len(value) > 0 && !bytes.Equal(value, []byte("null")) {
		frame[key] = value
	}
}
