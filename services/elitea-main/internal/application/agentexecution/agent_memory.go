package agentexecution

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"

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
// Tag is the turn's mark tag the text was rendered with (#1900); the
// resolver need not set it, resolveAgentMemory does.
type AgentMemoryRecall struct {
	Text    string
	Entries []agentmemory.RecalledEntry
	Tag     string
}

// AgentMemoryRecallRequest is one turn's recall question. MarkTag is the
// turn's mark tag (agentmemory.NewMarkTag) the resolver renders the marks
// with (#1900).
type AgentMemoryRecallRequest struct {
	ProjectID        int64
	ApplicationID    int64
	ConversationUUID string
	UserInput        string
	Settings         agentmemory.Settings
	MarkTag          string
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
	// #1900: a fresh tag for every turn, so no authored text can carry a
	// real mark.
	tag := agentmemory.NewMarkTag()
	recall, err := service.agentMemory.ResolveAgentMemoryRecall(ctx, AgentMemoryRecallRequest{
		ProjectID: projectID, ApplicationID: applicationID, ConversationUUID: conversationUUID,
		UserInput: userInput, Settings: settings, MarkTag: tag,
	})
	if err != nil {
		slog.WarnContext(ctx, "agent memory: recall failed; the turn runs without the agent's notes", "error", err)
		return AgentMemoryRecall{}
	}
	if recall.Text == "" {
		return AgentMemoryRecall{}
	}
	recall.Tag = tag
	return recall
}

// recalledMemoryInput is the run input's recalled_memory (#1826) and
// recalled_memory_tag (#1900): each entry the instructions show, by id,
// with its stored content, and the tag of their marks. None when the splice
// did not land the block in the frozen instructions (a version whose
// instructions are not prose): a brief may name only what the model was
// shown. Both are frozen with the instructions, so an input derived from
// this one keeps them.
func recalledMemoryInput(recall AgentMemoryRecall, application []byte) ([]*runtimev1.RecalledMemoryEntryV1, *string) {
	if len(recall.Entries) == 0 || !agentmemory.ValidMarkTag(recall.Tag) ||
		!frozenInstructionsShow(application, recall.Text) {
		return nil, nil
	}
	out := make([]*runtimev1.RecalledMemoryEntryV1, len(recall.Entries))
	for i, entry := range recall.Entries {
		out[i] = &runtimev1.RecalledMemoryEntryV1{
			EntryId: strconv.FormatInt(entry.ID, 10), Content: entry.Content,
		}
	}
	tag := recall.Tag
	return out, &tag
}

// frozenInstructionsShow reports whether the input's frozen
// version_details.instructions holds block.
func frozenInstructionsShow(application []byte, block string) bool {
	var fields struct {
		VersionDetails struct {
			Instructions string `json:"instructions"`
		} `json:"version_details"`
	}
	return block != "" && json.Unmarshal(application, &fields) == nil &&
		strings.Contains(fields.VersionDetails.Instructions, block)
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
