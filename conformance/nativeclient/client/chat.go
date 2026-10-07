package client

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// AdhocContract is the execution contract of a plain model turn.
const AdhocContract = "agent.execute.adhoc.v1"

// ApplicationContract is the execution contract of a turn answered by an
// agent participant (participant_id > 0).
const ApplicationContract = "agent.execute.application.v1"

// ChatAdmission is the answer to a chat send (200, `created` false on a
// replay of the same question_id).
type ChatAdmission struct {
	Created           bool   `json:"created"`
	ExecutionID       string `json:"execution_id"`
	ResponseMessageID string `json:"response_message_id"`
	EventsURL         string `json:"events_url"`
}

// ConversationPath is the per-project conversation list.
func ConversationPath(projectID int64) string {
	return fmt.Sprintf("/api/v2/elitea_core/conversations/prompt_lib/%d", projectID)
}

// ConversationItemPath is one conversation.
func ConversationItemPath(projectID int64, conversationID string) string {
	return fmt.Sprintf("/api/v2/elitea_core/conversation/prompt_lib/%d/%s", projectID, conversationID)
}

// MessagesPath is a conversation's message list (and the chat send).
func MessagesPath(projectID int64, conversationID string) string {
	return fmt.Sprintf("/api/v2/elitea_core/messages/prompt_lib/%d/%s", projectID, conversationID)
}

// NotificationsPath is the per-project notification list.
func NotificationsPath(projectID int64) string {
	return fmt.Sprintf("/api/v2/notifications/notifications/prompt_lib/%d", projectID)
}

// NotificationPath is one notification.
func NotificationPath(projectID int64, notificationID string) string {
	return fmt.Sprintf("/api/v2/notifications/notification/prompt_lib/%d/%s", projectID, notificationID)
}

// ProjectsPath is the project list; the trailing 1 is a literal segment.
const ProjectsPath = "/api/v2/projects/project/default/1"

// DevicesPath is the caller's device list.
const DevicesPath = "/api/v2/auth/native/devices"

// Frame is one decoded execution event.
type Frame struct {
	Cursor uint64
	Event  string
	Type   string
	Data   map[string]any
}

// DecodeFrame reads an execution event. A frame whose id is not a uint64 is
// an error: the contract promises a strictly increasing integer cursor.
func DecodeFrame(event Event) (Frame, error) {
	cursor, err := strconv.ParseUint(event.ID, 10, 64)
	if err != nil {
		return Frame{}, fmt.Errorf("event id %q is not a uint64 cursor", event.ID)
	}
	frame := Frame{Cursor: cursor, Event: event.Event}
	if err := json.Unmarshal([]byte(event.Data), &frame.Data); err != nil {
		return Frame{}, fmt.Errorf("event %d data is not a JSON object: %w", cursor, err)
	}
	frame.Type, _ = frame.Data["type"].(string)
	if frame.Type == "" {
		frame.Type = event.Event
	}
	return frame, nil
}

// IsTerminal applies the contract's terminal-frame rules (API_CONTRACT.md,
// "Execution event stream"): the server does not close the stream at the end
// of a turn, the client recognises the last frame.
func IsTerminal(frame Frame) bool {
	if frame.Event == "execution.failed" {
		return true
	}
	metadata, _ := frame.Data["response_metadata"].(map[string]any)
	switch frame.Type {
	case "pipeline_finish", "error", "llm_error", "agent_exception":
		return true
	case "agent_response":
		reason, _ := metadata["finish_reason"].(string)
		return reason != ""
	case "mcp_authorization_required":
		_, ok := metadata["authorization_requests"].([]any)
		return ok
	case "agent_hitl_interrupt":
		inner, _ := metadata["metadata"].(map[string]any)
		// A fan-out child pause names both its parent and its own thread;
		// its siblings keep streaming, so it does not end the turn.
		_, parent := inner["parent_agent_name"]
		_, child := inner["child_thread_id"]
		return !parent || !child
	}
	return false
}

// IsFailure reports a terminal frame that ends the turn in error.
func IsFailure(frame Frame) bool {
	switch {
	case frame.Event == "execution.failed":
		return true
	case frame.Type == "error", frame.Type == "llm_error", frame.Type == "agent_exception":
		return true
	}
	return false
}
