package agentexecution

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
)

// AgentMemoryRecallResolver reads an agent's own memory for a turn
// (ADR-0050 decision 1b, #1607): the agent's active entries, ranked against
// the turn's input under the version's recall budget, with entries about a
// person kept to turns where that person is in the conversation.
type AgentMemoryRecallResolver interface {
	ResolveAgentMemoryRecall(ctx context.Context, request AgentMemoryRecallRequest) (AgentMemoryRecall, error)
}

// AgentMemoryRecall is what a turn recalled: the prompt block and, in the
// same order, each entry's id and stored content (#1826), which the run
// input carries so a spawn_agent brief can name an entry recalled here.
type AgentMemoryRecall struct {
	Text    string
	Entries []agentmemory.RecalledEntry
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

// resolveAgentMemory is the agent memory a turn recalls, or none. Like
// personal memory it fails open: a store hiccup costs the turn its recalled
// notes, never the turn. A pipeline gets none (appendCurrentApplicationMemories
// skips a graph). A version whose block cannot be read recalls nothing.
func (service *CurrentApplicationStartService) resolveAgentMemory(
	ctx context.Context,
	projectID, applicationID int64,
	conversationUUID, userInput string,
	versionDetails json.RawMessage,
) AgentMemoryRecall {
	if service.agentMemory == nil || applicationID <= 0 || currentApplicationInstructionsAreAGraph(versionDetails) {
		return AgentMemoryRecall{}
	}
	settings, err := versionMemorySettings(versionDetails)
	if err != nil {
		slog.WarnContext(ctx, "agent memory: the version's memory block is not readable; nothing is recalled", "error", err)
		return AgentMemoryRecall{}
	}
	if !settings.Enabled {
		return AgentMemoryRecall{}
	}
	recall, err := service.agentMemory.ResolveAgentMemoryRecall(ctx, AgentMemoryRecallRequest{
		ProjectID: projectID, ApplicationID: applicationID, ConversationUUID: conversationUUID,
		UserInput: userInput, Settings: settings,
	})
	if err != nil {
		slog.WarnContext(ctx, "agent memory: recall failed; the turn runs without the agent's notes", "error", err)
		return AgentMemoryRecall{}
	}
	if recall.Text == "" {
		return AgentMemoryRecall{}
	}
	return recall
}

// recalledMemoryInput is the run input's recalled_memory (#1826): each
// entry the instructions show, by id, with its stored content.
func recalledMemoryInput(entries []agentmemory.RecalledEntry) []*runtimev1.RecalledMemoryEntryV1 {
	if len(entries) == 0 {
		return nil
	}
	out := make([]*runtimev1.RecalledMemoryEntryV1, 0, len(entries))
	for _, entry := range entries {
		if entry.ID <= 0 {
			continue
		}
		out = append(out, &runtimev1.RecalledMemoryEntryV1{
			EntryId: strconv.FormatInt(entry.ID, 10), Content: entry.Content,
		})
	}
	return out
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
