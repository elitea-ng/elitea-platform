package agentexecution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

// CurrentStaticInventoryRootKind comes from the original digest-checked input.
// Event metadata uses this type for validation, never as execution authority.
type CurrentStaticInventoryRootKind string

const (
	CurrentStaticInventoryAgentRoot    CurrentStaticInventoryRootKind = "agent"
	CurrentStaticInventoryPipelineRoot CurrentStaticInventoryRootKind = "pipeline"
)

// ParseCurrentStaticInventoryRootKind rejects contradictory or unsupported kinds.
// An absent outer kind preserves stored-message compatibility.
func ParseCurrentStaticInventoryRootKind(outer, version string) (CurrentStaticInventoryRootKind, error) {
	if outer != "" && version != "" && outer != version {
		return "", ErrUnsupportedCurrentAgentStart
	}
	if outer == "" {
		outer = version
	}
	kind := CurrentStaticInventoryRootKind(outer)
	if kind != CurrentStaticInventoryAgentRoot && kind != CurrentStaticInventoryPipelineRoot {
		return "", ErrUnsupportedCurrentAgentStart
	}
	return kind, nil
}

type currentStaticInventoryApplication struct {
	ID             int64  `json:"id"`
	VersionID      int64  `json:"version_id"`
	AgentType      string `json:"agent_type"`
	VersionDetails struct {
		ID            int64  `json:"id"`
		ApplicationID int64  `json:"application_id"`
		AgentType     string `json:"agent_type"`
	} `json:"version_details"`
}

func decodeStaticInventoryApplication(raw []byte) (currentStaticInventoryApplication, CurrentStaticInventoryRootKind, error) {
	var app currentStaticInventoryApplication
	if json.Unmarshal(raw, &app) != nil || app.ID <= 0 || app.VersionID <= 0 || app.VersionDetails.ID != app.VersionID || app.VersionDetails.ApplicationID != app.ID || app.VersionDetails.AgentType == "" {
		return app, "", ErrUnsupportedCurrentAgentStart
	}
	kind, err := ParseCurrentStaticInventoryRootKind(app.AgentType, app.VersionDetails.AgentType)
	return app, kind, err
}

type CurrentStaticLeafDecision struct {
	PauseID       string `json:"pause_id"`
	ChildThreadID string `json:"child_thread_id"`
	ToolCallID    string `json:"tool_call_id"`
	Action        string `json:"action"`
	Value         string `json:"value"`
}
type CurrentStaticToolPause struct {
	ToolCallID           string                     `json:"tool_call_id"`
	ChildThreadID        string                     `json:"child_thread_id"`
	OriginalBatchEventID string                     `json:"original_batch_event_id"`
	OriginalOrdinal      uint32                     `json:"original_ordinal"`
	Proof                CurrentPipelineStaticProof `json:"proof"`
}
type CurrentStaticToolInventory struct {
	Revision int                      `json:"revision"`
	Pauses   []CurrentStaticToolPause `json:"pauses"`
}
type CurrentPipelineStaticTools struct {
	RootKind             CurrentStaticInventoryRootKind
	Inventory            CurrentStaticToolInventory
	Selected             []CurrentStaticLeafDecision
	ApplicationID        int64
	ApplicationVersionID int64
	FrozenInput          *runtimev1.AgentExecutionInputV1
	InputDigest          []byte
}

func ParseCurrentStaticToolInventory(raw json.RawMessage) (*CurrentStaticToolInventory, error) {
	if len(raw) == 0 || len(raw) > 128*1024 {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	var inventory CurrentStaticToolInventory
	if strictStaticJSON(raw, &inventory) != nil || inventory.Revision != 1 || len(inventory.Pauses) == 0 || len(inventory.Pauses) > 16 {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	ids := make(map[string]struct{}, len(inventory.Pauses))
	calls := make(map[string]struct{}, len(inventory.Pauses))
	type occurrenceOrdinal struct {
		batch   string
		ordinal uint32
	}
	ordinals := make(map[occurrenceOrdinal]struct{}, len(inventory.Pauses))
	for _, pause := range inventory.Pauses {
		if !validStaticIdentity(pause.ToolCallID, 512) || !validStaticIdentity(pause.ChildThreadID, 256) || !validStaticIdentity(pause.OriginalBatchEventID, 512) || pause.OriginalOrdinal == 0 || pause.OriginalOrdinal > 16 || !pause.Proof.valid(pause.ChildThreadID) {
			return nil, ErrUnsupportedCurrentAgentStart
		}
		ordinal := occurrenceOrdinal{batch: pause.OriginalBatchEventID, ordinal: pause.OriginalOrdinal}
		if _, duplicate := ordinals[ordinal]; duplicate {
			return nil, ErrUnsupportedCurrentAgentStart
		}
		ordinals[ordinal] = struct{}{}
		key := pause.ChildThreadID + "\x00" + pause.ToolCallID
		if _, ok := ids[pause.Proof.PauseID]; ok {
			return nil, ErrUnsupportedCurrentAgentStart
		}
		ids[pause.Proof.PauseID] = struct{}{}
		if _, ok := calls[key]; ok {
			return nil, ErrUnsupportedCurrentAgentStart
		}
		calls[key] = struct{}{}
	}
	return &inventory, nil
}
func validStaticLeafDecisions(decisions []CurrentStaticLeafDecision) bool {
	if len(decisions) == 0 || len(decisions) > 16 {
		return false
	}
	seen := make(map[string]struct{}, len(decisions))
	total := 0
	for _, decision := range decisions {
		total += len(decision.Value)
		if !validStaticPauseID(decision.PauseID) || !validStaticIdentity(decision.ChildThreadID, 256) || !validStaticIdentity(decision.ToolCallID, 512) || decision.Action != "continue" || strings.TrimSpace(decision.Value) == "" || len(decision.Value) > maxCurrentStaticInputBytes || strings.ContainsRune(decision.Value, '\x00') {
			return false
		}
		if _, exists := seen[decision.PauseID]; exists {
			return false
		}
		seen[decision.PauseID] = struct{}{}
	}
	return total <= 64*1024
}
func staticLeafDecisionsMatch(decisions []CurrentStaticLeafDecision, inventory CurrentStaticToolInventory) bool {
	if !validStaticLeafDecisions(decisions) {
		return false
	}
	pending := make(map[string]CurrentStaticToolPause, len(inventory.Pauses))
	for _, pause := range inventory.Pauses {
		pending[pause.Proof.PauseID] = pause
	}
	for _, decision := range decisions {
		pause, ok := pending[decision.PauseID]
		if !ok || pause.ChildThreadID != decision.ChildThreadID || pause.ToolCallID != decision.ToolCallID {
			return false
		}
		delete(pending, decision.PauseID)
	}
	return true
}
func DecodeCurrentStaticToolInput(encoded, storedDigest []byte, inventory CurrentStaticToolInventory, conversation, generation, thread string) (*CurrentPipelineStaticTools, error) {
	if len(encoded) == 0 || len(encoded) > executiondomain.MaxAgentExecutionInputBytes {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	digest := sha256.Sum256(encoded)
	if !bytes.Equal(digest[:], storedDigest) {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	var input runtimev1.AgentExecutionInputV1
	if proto.Unmarshal(encoded, &input) != nil || input.GetSchemaRevision() != "elitea.runtime.agent-execution-input.v1" || input.GetConversationId() != conversation || input.GetExecutionGeneration() != generation || input.GetThreadId() != thread {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	app, kind, err := decodeStaticInventoryApplication(input.Application)
	if err != nil {
		return nil, err
	}
	return &CurrentPipelineStaticTools{RootKind: kind, Inventory: inventory, ApplicationID: app.ID, ApplicationVersionID: app.VersionID, FrozenInput: &input, InputDigest: bytes.Clone(storedDigest)}, nil
}
func (tools *CurrentPipelineStaticTools) clone() *CurrentPipelineStaticTools {
	if tools == nil {
		return nil
	}
	clone := *tools
	clone.Selected = append([]CurrentStaticLeafDecision{}, tools.Selected...)
	clone.InputDigest = bytes.Clone(tools.InputDigest)
	clone.Inventory.Pauses = append([]CurrentStaticToolPause{}, tools.Inventory.Pauses...)
	for index := range clone.Inventory.Pauses {
		proof := &clone.Inventory.Pauses[index].Proof
		proof.PendingNodes = append([]string{}, proof.PendingNodes...)
		proof.DescendantPath = append([]CurrentStaticDescendant{}, proof.DescendantPath...)
	}
	if tools.FrozenInput != nil {
		clone.FrozenInput = proto.Clone(tools.FrozenInput).(*runtimev1.AgentExecutionInputV1)
	}
	return &clone
}
func (target CurrentContinuationTarget) validateStaticTools() error {
	tools := target.PipelineStaticTools
	if tools == nil || tools.FrozenInput == nil || target.Kind != CurrentRegenerationApplication || target.PipelineStaticPause != nil || target.PipelineHITLReview != nil || len(target.HITLInterrupts) != 0 || len(target.AuthorizationRequests) != 0 || target.InterruptID != "" || target.ToolCallID != "" || len(target.AvailableActions) != 0 || target.OutputLimitSequence != 0 || target.TruncatedContent != "" || len(tools.InputDigest) != sha256.Size || tools.FrozenInput.GetThreadId() != target.ThreadID || tools.FrozenInput.GetExecutionGeneration() != target.ExecutionGeneration {
		return ErrUnsupportedCurrentAgentStart
	}
	app, kind, err := decodeStaticInventoryApplication(tools.FrozenInput.Application)
	if err != nil || kind != tools.RootKind || app.ID != tools.ApplicationID || app.VersionID != tools.ApplicationVersionID {
		return ErrUnsupportedCurrentAgentStart
	}
	raw, err := json.Marshal(tools.Inventory)
	if err != nil {
		return ErrUnsupportedCurrentAgentStart
	}
	_, err = ParseCurrentStaticToolInventory(raw)
	return err
}
func (service *CurrentApplicationStartService) currentStaticToolContinuationInput(ctx context.Context, request CurrentContinuationRequest, target CurrentContinuationTarget) (*runtimev1.AgentExecutionInputV1, *CurrentContinueTurn, string, error) {
	tools := target.PipelineStaticTools
	if target.validateStaticTools() != nil || !staticLeafDecisionsMatch(request.StaticDecisions, tools.Inventory) {
		return nil, nil, "", ErrUnsupportedCurrentAgentStart
	}
	input, err := service.authorizeStaticFrozenInput(ctx, request, target, tools.ApplicationID, tools.ApplicationVersionID, tools.FrozenInput)
	if err != nil {
		return nil, nil, "", err
	}
	app, kind, err := decodeStaticInventoryApplication(input.Application)
	if err != nil || kind != tools.RootKind || app.ID != tools.ApplicationID || app.VersionID != tools.ApplicationVersionID {
		return nil, nil, "", ErrUnsupportedCurrentAgentStart
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(input.Meta, &meta) != nil {
		return nil, nil, "", ErrUnsupportedCurrentAgentStart
	}
	delete(meta, "pipeline_static_resume_v1")
	meta["pipeline_static_tool_resume_v1"], err = json.Marshal(map[string]any{"revision": 1, "decisions": request.StaticDecisions})
	if err != nil {
		return nil, nil, "", ErrInvalidCurrentAgentStart
	}
	input.Meta, err = json.Marshal(meta)
	if err != nil {
		return nil, nil, "", ErrInvalidCurrentAgentStart
	}
	input.ShouldContinue = true
	input.HitlResume = false
	input.HitlAction = nil
	input.HitlValue = nil
	input.HitlDecisions = []byte(`[]`)
	input.CheckpointId = nil
	input.IsRegenerate = false
	input.TruncatedContent = nil
	selected := tools.clone()
	selected.Selected = append([]CurrentStaticLeafDecision{}, request.StaticDecisions...)
	turn := &CurrentContinueTurn{ProjectID: request.ProjectID, ActorUserID: request.ActorUserID, ConversationUUID: request.ConversationUUID, TargetParticipantID: target.TargetParticipantID, Kind: CurrentRegenerationApplication, ApplicationID: tools.ApplicationID, ApplicationVersionID: tools.ApplicationVersionID, QuestionID: target.QuestionID, ResponseMessageID: request.ResponseMessageID, ExecutionGeneration: target.ExecutionGeneration, ThreadID: target.ThreadID, ContinuationKind: CurrentContinuationStatic, PipelineStaticTools: selected}
	return input, turn, executiondomain.AgentApplicationCapability, nil
}
