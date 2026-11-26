package executions

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
)

// The participant stamp and the `agent_state` frame (client contract 1.38,
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
// own and is served without an SSE id; a client that resumes after the cause
// gets the state from the resume seed.

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

// The `activity.waiting_on` values (ADR-0061 decision 4a).
const (
	WaitingOnPerson = "person"
	WaitingOnAgent  = "agent"
)

// maxActivityTool is the longest tool display name an activity carries.
const maxActivityTool = 64

// AgentActivity is the optional detail of an `agent_state` frame (client
// contract 1.66, ADR-0061 decision 4a): which tool a `working` agent runs,
// and on whom a `waiting` agent waits. It never carries tool arguments.
//
// `handoff_to` (on the frame that ends a turn with a hand-off) is not
// derived here: the stamp sees one run's node events, while a cloud
// `hand_floor` is recorded in `agent_floor_exchanges` by the turn-taking
// listener; the terminal frame of such a turn would read it from there.
// `peer_participant_id` is not derived either: a sub-agent frame names the
// sub-agent (`parent_agent_name`), not a participant of the thread.
type AgentActivity struct {
	Tool      string `json:"tool,omitempty"`
	ToolKind  string `json:"tool_kind,omitempty"`
	WaitingOn string `json:"waiting_on,omitempty"`
}

// derivedState is what a frame moves its participant to.
type derivedState struct {
	state    string
	activity AgentActivity
}

// participantStates is the last state and activity written per
// participant on one stream. The zero value is ready.
type participantStates struct {
	last map[int64]derivedState
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
		// The tool call entry of agent_tool_start: only the name and the
		// toolkit type are read, never the arguments.
		ToolName string `json:"tool_name"`
		Metadata struct {
			ToolkitType string `json:"toolkit_type"`
		} `json:"metadata"`
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

	state, activity := frameAgentState(event.Type, fields)
	if state == "" {
		return event, nil
	}
	if s.last == nil {
		s.last = make(map[int64]derivedState, 1)
	}
	next := derivedState{state: state, activity: activity}
	if s.last[participant.ID] == next {
		return event, nil
	}
	s.last[participant.ID] = next
	frame := map[string]any{
		"type":              agentStateFrameType,
		"participant_id":    participant.ID,
		"addressed_to":      addressedTo,
		"state":             state,
		"response_metadata": map[string]any{},
	}
	if activity != (AgentActivity{}) {
		frame["activity"] = activity
	}
	copyRawField(frame, "stream_id", fields.StreamID)
	copyRawField(frame, "message_id", fields.MessageID)
	copyRawField(frame, "execution_generation", fields.ExecutionGeneration)
	data, err := json.Marshal(frame)
	if err != nil {
		return event, nil
	}
	return event, &DurableEvent{Type: nodeEventType, Data: data}
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
	return state
}

// frameAgentState is the state a frame moves its participant to, and the
// activity that goes with it; an empty state for a frame that does not move
// it. The rows are the cloud column of ADR-0061 decision 4b
// (libs/proto/contracts/agent-state-activity-v1.json).
func frameAgentState(eventType string, fields participantFrameFields) (string, AgentActivity) {
	if eventType == executionFailedType {
		return AgentStateError, AgentActivity{}
	}
	// A pause waits on a person, whoever in the agent tree asked.
	switch fields.Type {
	case "agent_hitl_interrupt", "agent_requires_confirmation", "agent_tool_paused", "mcp_authorization_required":
		return AgentStateWaiting, AgentActivity{WaitingOn: WaitingOnPerson}
	}
	// Any other sub-agent frame: the participant has delegated and waits on
	// the sub-agent, and a sub-agent's answer is not the participant's.
	if fields.ResponseMetadata.ParentAgentName != "" {
		if fields.Type == "full_message" {
			return "", AgentActivity{}
		}
		return AgentStateWaiting, AgentActivity{WaitingOn: WaitingOnAgent}
	}
	switch fields.Type {
	case "agent_start":
		// A continuation resumes a paused answer.
		if fields.SIOEvent == "chat_continue_predict" {
			return AgentStateWaking, AgentActivity{}
		}
		return AgentStateThinking, AgentActivity{}
	case "agent_llm_start", "agent_tool_end", "agent_tool_error":
		return AgentStateThinking, AgentActivity{}
	case "agent_llm_chunk", "agent_result_chunk", "agent_response":
		return AgentStateSpeaking, AgentActivity{}
	case "agent_tool_start":
		metadata := fields.ResponseMetadata
		return AgentStateWorking, toolActivity(metadata.ToolName, metadata.Metadata.ToolkitType)
	case "agent_exception", "error", "llm_error":
		return AgentStateError, AgentActivity{}
	case "full_message":
		if fields.ResponseMetadata.IsError || fields.ResponseMetadata.FinishReason == "error" {
			return AgentStateError, AgentActivity{}
		}
		return AgentStateSuccess, AgentActivity{}
	}
	return "", AgentActivity{}
}

// toolActivity is the activity of a tool call: the tool's display name,
// sanitised, and its category when the tool-categories contract lists it.
func toolActivity(toolName, toolkitType string) AgentActivity {
	tool := sanitizeToolName(toolName)
	if tool == "" {
		return AgentActivity{}
	}
	activity := AgentActivity{Tool: tool}
	if toolkitType != "" && toolcategory.Declared(toolkitType, toolName) {
		activity.ToolKind = string(toolcategory.Of(toolkitType, toolName))
	}
	return activity
}

// sanitizeToolName keeps a tool display name to [A-Za-z0-9_.:-]: any other
// character becomes `_`, and the name is cut to maxActivityTool bytes. A
// name with no letter or digit is empty.
func sanitizeToolName(name string) string {
	out := make([]byte, 0, min(len(name), maxActivityTool))
	meaningful := false
	for _, r := range name {
		if len(out) == maxActivityTool {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			meaningful = true
			out = append(out, byte(r))
		case r == '_' || r == '.' || r == ':' || r == '-':
			out = append(out, byte(r))
		default:
			out = append(out, '_')
		}
	}
	if !meaningful {
		return ""
	}
	return string(out)
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
