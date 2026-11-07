package agentexecution

import (
	"context"
	"encoding/json"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
)

// AgentMemoryRecallResolver reads an agent's own memory for a turn
// (ADR-0050 decision 1b, #1607): the agent's active entries, ranked against
// the turn's input under the version's recall budget, with entries about a
// person kept to turns where that person is in the conversation.
type AgentMemoryRecallResolver interface {
	ResolveAgentMemoryRecall(ctx context.Context, request AgentMemoryRecallRequest) (string, error)
}

// AgentMemoryRecallRequest is one turn's recall question.
type AgentMemoryRecallRequest struct {
	ProjectID        int64
	ApplicationID    int64
	ConversationUUID string
	UserInput        string
	Settings         agentmemory.Settings
}

// WithAgentMemory attaches the agent memory recall. Without it no agent
// memory reaches a turn.
func (service *CurrentApplicationStartService) WithAgentMemory(memory AgentMemoryRecallResolver) *CurrentApplicationStartService {
	service.agentMemory = memory
	return service
}

// resolveAgentMemoryText is the agent memory block of a turn, or "". Like
// personal memory it fails open: a store hiccup costs the turn its recalled
// notes, never the turn. A pipeline gets none (appendCurrentApplicationMemories
// skips a graph). A version whose block cannot be read recalls nothing.
func (service *CurrentApplicationStartService) resolveAgentMemoryText(
	ctx context.Context,
	projectID, applicationID int64,
	conversationUUID, userInput string,
	versionDetails json.RawMessage,
) string {
	if service.agentMemory == nil || applicationID <= 0 {
		return ""
	}
	settings, err := versionMemorySettings(versionDetails)
	if err != nil || !settings.Enabled {
		return ""
	}
	text, err := service.agentMemory.ResolveAgentMemoryRecall(ctx, AgentMemoryRecallRequest{
		ProjectID: projectID, ApplicationID: applicationID, ConversationUUID: conversationUUID,
		UserInput: userInput, Settings: settings,
	})
	if err != nil {
		return ""
	}
	return text
}

// versionMemorySettings reads meta.memory of a frozen version document.
func versionMemorySettings(versionDetails json.RawMessage) (agentmemory.Settings, error) {
	if len(versionDetails) == 0 {
		return agentmemory.Off, nil
	}
	var version struct {
		Meta map[string]any `json:"meta"`
	}
	if err := json.Unmarshal(versionDetails, &version); err != nil {
		return agentmemory.Settings{}, err
	}
	return agentmemory.Parse(version.Meta[agentmemory.MetaKey])
}

// joinRecallText puts the agent's own notes after the person's memories:
// both ride the same splice onto the instructions.
func joinRecallText(personal, agent string) string {
	switch {
	case personal == "":
		return agent
	case agent == "":
		return personal
	}
	return personal + "\n\n" + agent
}
