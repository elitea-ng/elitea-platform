package agentexecution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

const maxCurrentStaticInputBytes = 8 * 1024
const maxCurrentStaticProofBytes = 16 * 1024

// CurrentPipelineStaticProof contains public identities, never checkpoint state.
// Main reads it from the original durable response. The browser echoes only PauseID.
type CurrentPipelineStaticProof struct {
	Revision         int                       `json:"revision"`
	PauseID          string                    `json:"pause_id"`
	CheckpointID     string                    `json:"checkpoint_id"`
	Kind             string                    `json:"kind"`
	NodeName         string                    `json:"node_name"`
	DefinitionDigest string                    `json:"definition_digest"`
	NodeDigest       string                    `json:"node_digest"`
	PendingNodes     []string                  `json:"pending_nodes"`
	Step             uint64                    `json:"step"`
	DescendantPath   []CurrentStaticDescendant `json:"descendant_path"`
}

type CurrentStaticDescendant struct {
	NodeName     string `json:"node_name"`
	ThreadID     string `json:"thread_id"`
	CheckpointID string `json:"checkpoint_id"`
}

func ParseCurrentPipelineStaticProof(raw json.RawMessage, rootThread string) (*CurrentPipelineStaticProof, error) {
	if len(raw) == 0 || len(raw) > maxCurrentStaticProofBytes {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	var proof CurrentPipelineStaticProof
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&proof) != nil || !proof.valid(rootThread) {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return &proof, nil
}

func (proof CurrentPipelineStaticProof) valid(rootThread string) bool {
	if proof.Revision != 1 || !validStaticPauseID(proof.PauseID) ||
		!validStaticIdentity(proof.CheckpointID, 256) || !validStaticNode(proof.NodeName) ||
		(proof.Kind != "before" && proof.Kind != "after") || !validStaticDigest(proof.DefinitionDigest) ||
		!validStaticDigest(proof.NodeDigest) || proof.Step > 1<<31-1 || len(proof.PendingNodes) > 128 ||
		len(proof.DescendantPath) > 8 || !validStaticIdentity(rootThread, 256) {
		return false
	}
	if proof.Kind == "before" && (len(proof.PendingNodes) != 1 || proof.PendingNodes[0] != proof.NodeName) {
		return false
	}
	seen := make(map[string]struct{}, len(proof.PendingNodes))
	for _, node := range proof.PendingNodes {
		if !validStaticNode(node) {
			return false
		}
		if _, exists := seen[node]; exists {
			return false
		}
		seen[node] = struct{}{}
	}
	thread := rootThread
	for _, child := range proof.DescendantPath {
		if !validStaticNode(child.NodeName) || !validStaticIdentity(child.CheckpointID, 256) ||
			child.ThreadID != thread+"/"+child.NodeName || !validStaticIdentity(child.ThreadID, 1024) {
			return false
		}
		thread = child.ThreadID
	}
	return true
}

func validStaticPauseID(value string) bool {
	return strings.HasPrefix(value, "pipeline-static:") && validStaticDigest(strings.TrimPrefix(value, "pipeline-static:"))
}
func validStaticDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, ch := range value[len("sha256:"):] {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
func validStaticIdentity(value string, limit int) bool {
	return value != "" && len(value) <= limit && !strings.ContainsRune(value, '\x00')
}
func validStaticNode(value string) bool {
	if !validStaticIdentity(value, 128) {
		return false
	}
	for _, ch := range value {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '_' && ch != '-' && ch != '.' && ch != ':' {
			return false
		}
	}
	return true
}

// CurrentPipelineStaticPause is server-owned. FrozenInput comes from the exact
// original command entry and is digest checked before this value is constructed.
type CurrentPipelineStaticPause struct {
	Proof                CurrentPipelineStaticProof
	ApplicationID        int64
	ApplicationVersionID int64
	FrozenInput          *runtimev1.AgentExecutionInputV1
	InputDigest          []byte
}

func DecodeCurrentStaticInput(encoded, storedDigest []byte, proof CurrentPipelineStaticProof, conversation, generation, thread string) (*CurrentPipelineStaticPause, error) {
	if len(encoded) == 0 || len(encoded) > executiondomain.MaxAgentExecutionInputBytes {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	digest := sha256.Sum256(encoded)
	if !bytes.Equal(digest[:], storedDigest) || !proof.valid(thread) {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	var input runtimev1.AgentExecutionInputV1
	if proto.Unmarshal(encoded, &input) != nil || input.GetSchemaRevision() != "elitea.runtime.agent-execution-input.v1" ||
		input.GetConversationId() != conversation || input.GetExecutionGeneration() != generation || input.GetThreadId() != thread {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	var app struct {
		ID             int64 `json:"id"`
		VersionID      int64 `json:"version_id"`
		VersionDetails struct {
			ID            int64  `json:"id"`
			ApplicationID int64  `json:"application_id"`
			AgentType     string `json:"agent_type"`
		} `json:"version_details"`
	}
	if json.Unmarshal(input.Application, &app) != nil || app.ID <= 0 || app.VersionID <= 0 ||
		app.VersionDetails.ID != app.VersionID || app.VersionDetails.ApplicationID != app.ID || app.VersionDetails.AgentType != "pipeline" {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return &CurrentPipelineStaticPause{Proof: proof, ApplicationID: app.ID, ApplicationVersionID: app.VersionID, FrozenInput: &input, InputDigest: bytes.Clone(storedDigest)}, nil
}

func (pause *CurrentPipelineStaticPause) clone() *CurrentPipelineStaticPause {
	if pause == nil {
		return nil
	}
	clone := *pause
	clone.Proof.PendingNodes = append([]string{}, pause.Proof.PendingNodes...)
	clone.Proof.DescendantPath = append([]CurrentStaticDescendant{}, pause.Proof.DescendantPath...)
	clone.InputDigest = bytes.Clone(pause.InputDigest)
	if pause.FrozenInput != nil {
		clone.FrozenInput = proto.Clone(pause.FrozenInput).(*runtimev1.AgentExecutionInputV1)
	}
	return &clone
}

func (target CurrentContinuationTarget) validateStaticContinuation() error {
	if target.PipelineStaticTools != nil {
		return target.validateStaticTools()
	}
	pause := target.PipelineStaticPause
	if target.Kind != CurrentRegenerationApplication || pause == nil || pause.FrozenInput == nil ||
		!pause.Proof.valid(target.ThreadID) || len(pause.InputDigest) != sha256.Size ||
		pause.FrozenInput.GetThreadId() != target.ThreadID || pause.FrozenInput.GetExecutionGeneration() != target.ExecutionGeneration ||
		target.PipelineHITLReview != nil || len(target.HITLInterrupts) != 0 || len(target.AuthorizationRequests) != 0 ||
		target.InterruptID != "" || target.ToolCallID != "" || len(target.AvailableActions) != 0 || target.OutputLimitSequence != 0 || target.TruncatedContent != "" {
		return ErrUnsupportedCurrentAgentStart
	}
	return nil
}
func (request CurrentContinuationRequest) validateStaticContinuation() error {
	if len(request.StaticDecisions) != 0 {
		if request.StaticPauseID != "" || request.StaticInputText != "" || request.Action != "" || request.Value != "" || request.AuthorizationID != "" || len(request.HITLDecisions) != 0 || len(request.MCPTokens) != 0 || len(request.IgnoredMCPServers) != 0 || len(request.DeclinedMCPServers) != 0 || !validStaticLeafDecisions(request.StaticDecisions) {
			return ErrInvalidCurrentAgentStart
		}
		return nil
	}
	if !validStaticPauseID(request.StaticPauseID) || strings.TrimSpace(request.StaticInputText) == "" ||
		len(request.StaticInputText) > maxCurrentStaticInputBytes || strings.ContainsRune(request.StaticInputText, '\x00') ||
		request.Action != "" || request.Value != "" || request.AuthorizationID != "" || len(request.HITLDecisions) != 0 ||
		len(request.MCPTokens) != 0 || len(request.IgnoredMCPServers) != 0 || len(request.DeclinedMCPServers) != 0 {
		return ErrInvalidCurrentAgentStart
	}
	return nil
}
func (turn CurrentContinueTurn) validateStaticContinuation() error {
	if tools := turn.PipelineStaticTools; tools != nil {
		if turn.PipelineStaticPause != nil || turn.PipelineHITLReview != nil || turn.Kind != CurrentRegenerationApplication || turn.ApplicationID != tools.ApplicationID || turn.ApplicationVersionID != tools.ApplicationVersionID || turn.ApplicationID <= 0 || turn.ApplicationVersionID <= 0 || len(turn.HITLDecisions) != 0 || turn.InterruptID != "" || turn.Action != "" || turn.OutputLimitSequence != 0 || len(tools.InputDigest) != sha256.Size || !staticLeafDecisionsMatch(tools.Selected, tools.Inventory) {
			return ErrInvalidCurrentAgentStart
		}
		return nil
	}
	pause := turn.PipelineStaticPause
	if turn.Kind != CurrentRegenerationApplication || pause == nil || !pause.Proof.valid(turn.ThreadID) ||
		turn.ApplicationID != pause.ApplicationID || turn.ApplicationVersionID != pause.ApplicationVersionID ||
		turn.ApplicationID <= 0 || turn.ApplicationVersionID <= 0 || len(pause.InputDigest) != sha256.Size ||
		len(turn.HITLDecisions) != 0 || turn.InterruptID != "" || turn.Action != "" || turn.PipelineHITLReview != nil || turn.OutputLimitSequence != 0 {
		return ErrInvalidCurrentAgentStart
	}
	return nil
}
func currentStaticContinuationIdempotencyKey(response, pauseID string) string {
	digest := sha256.Sum256([]byte(pauseID))
	return "continue-static/" + response + "/" + hex.EncodeToString(digest[:])
}

func (service *CurrentApplicationStartService) currentStaticContinuationInput(ctx context.Context, request CurrentContinuationRequest, target CurrentContinuationTarget) (*runtimev1.AgentExecutionInputV1, *CurrentContinueTurn, string, error) {
	if target.validateStaticContinuation() != nil || target.PipelineStaticPause.Proof.PauseID != request.StaticPauseID {
		return nil, nil, "", ErrUnsupportedCurrentAgentStart
	}
	pause := target.PipelineStaticPause
	input, err := service.authorizeStaticFrozenInput(ctx, request, target, pause.ApplicationID, pause.ApplicationVersionID, pause.FrozenInput)
	if err != nil {
		return nil, nil, "", err
	}
	input.UserInput, err = json.Marshal(request.StaticInputText)
	if err != nil {
		return nil, nil, "", ErrInvalidCurrentAgentStart
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(input.Meta, &meta) != nil {
		return nil, nil, "", ErrUnsupportedCurrentAgentStart
	}
	meta["pipeline_static_resume_v1"], err = json.Marshal(map[string]any{"revision": 1, "pause_id": request.StaticPauseID})
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
	input.TruncatedContent = nil
	input.IsRegenerate = false
	turn := &CurrentContinueTurn{ProjectID: request.ProjectID, ActorUserID: request.ActorUserID, ConversationUUID: request.ConversationUUID, TargetParticipantID: target.TargetParticipantID, Kind: CurrentRegenerationApplication, ApplicationID: pause.ApplicationID, ApplicationVersionID: pause.ApplicationVersionID, QuestionID: target.QuestionID, ResponseMessageID: request.ResponseMessageID, ExecutionGeneration: target.ExecutionGeneration, ThreadID: target.ThreadID, ContinuationKind: CurrentContinuationStatic, PipelineStaticPause: pause.clone()}
	return input, turn, executiondomain.AgentApplicationCapability, nil
}

func (service *CurrentApplicationStartService) authorizeStaticFrozenInput(ctx context.Context, request CurrentContinuationRequest, target CurrentContinuationTarget, applicationID, applicationVersionID int64, original *runtimev1.AgentExecutionInputV1) (*runtimev1.AgentExecutionInputV1, error) {
	start := CurrentApplicationStartRequest{ProjectID: request.ProjectID, ActorUserID: request.ActorUserID, ConversationUUID: request.ConversationUUID, TargetParticipantID: target.TargetParticipantID, QuestionID: target.QuestionID, UserInput: target.UserInput}
	authorized, err := service.resolver.ResolveCurrentApplication(ctx, start)
	if err != nil {
		return nil, err
	}
	if authorized.ApplicationID != applicationID || authorized.ApplicationVersionID != applicationVersionID {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	project, projectOK := currentContinuationDatabaseID(request.ProjectID)
	actor, actorOK := currentContinuationDatabaseID(request.ActorUserID)
	if !projectOK || !actorOK {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	// Reauthorize the original frozen version. Mutable current version bytes are
	// not a substitute for the definition that owns the checkpoint.
	var app map[string]json.RawMessage
	if json.Unmarshal(original.Application, &app) != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	frozen, err := service.freezer.FreezeCurrentApplicationVersion(ctx, CurrentApplicationVersionFreezeRequest{ProjectID: project, ActorUserID: actor, VersionDetails: app["version_details"], InternalTools: original.InternalTools})
	if err != nil {
		return nil, err
	}
	app["version_details"] = frozen
	application, err := json.Marshal(app)
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	version, err := decodeCurrentApplicationVersion(frozen)
	if err != nil {
		return nil, err
	}
	llm, err := currentApplicationRuntimeLLM(version)
	if err != nil {
		return nil, err
	}
	guards, err := service.resolveToolkitGuardrails(ctx)
	if err != nil {
		return nil, err
	}
	input := proto.Clone(original).(*runtimev1.AgentExecutionInputV1)
	input.Application = application
	input.Llm = llm
	input.ToolkitGuardrails = bytes.Clone(guards)
	return input, nil
}

func strictStaticJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return ErrUnsupportedCurrentAgentStart
	}
	return nil
}
