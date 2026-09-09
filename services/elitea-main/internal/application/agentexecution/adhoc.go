package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

// CurrentAdhocTurn is the immutable current-chat side of one durable ad-hoc
// execution admission. The repository rechecks the conversation, actor and
// dummy responder while inserting the current chat rows and runtime outbox in
// one transaction.
type CurrentAdhocTurn struct {
	ProjectID           int64
	ActorUserID         int64
	ConversationUUID    string
	TargetParticipantID int64
	QuestionID          string
	QuestionItemID      string
	ResponseMessageID   string
	QuestionMeta        json.RawMessage
	UserInput           string
	// Attachments are the `attachment_message` items written onto the QUESTION
	// group in the same transaction (#606). Empty for a turn with no files.
	Attachments []CurrentTurnAttachment
}

func (turn CurrentAdhocTurn) Validate() error {
	if turn.ProjectID <= 0 || turn.ActorUserID <= 0 || turn.TargetParticipantID <= 0 ||
		!validUUID(turn.ConversationUUID) || !validUUID(turn.QuestionID) ||
		!validUUID(turn.QuestionItemID) || !validUUID(turn.ResponseMessageID) ||
		!validCurrentAgentText(turn.UserInput, maxCurrentAgentUserInputBytes) ||
		!validJSONObject(turn.QuestionMeta) ||
		!validCurrentTurnAttachments(turn.Attachments) {
		return ErrInvalidCurrentAgentStart
	}
	return nil
}

func (turn *CurrentAdhocTurn) Clone() *CurrentAdhocTurn {
	if turn == nil {
		return nil
	}
	clone := *turn
	clone.QuestionMeta = bytes.Clone(turn.QuestionMeta)
	clone.Attachments = cloneCurrentTurnAttachments(turn.Attachments)
	return &clone
}

type CurrentAdhocTarget struct {
	TargetParticipantID int64
	LLMSettings         json.RawMessage
	Instructions        string
	Tools               json.RawMessage
	ChatHistory         json.RawMessage
	ConversationMeta    json.RawMessage
}

type CurrentAdhocResolver interface {
	ResolveCurrentAdhoc(
		context.Context,
		CurrentAdhocStartRequest,
	) (CurrentAdhocTarget, error)
}

type CurrentAdhocStartRequest struct {
	ProjectID           int64
	ActorUserID         int64
	ConversationUUID    string
	TargetParticipantID int64
	QuestionID          string
	UserInput           string
	InteractionUUID     string
	LLMSettings         json.RawMessage
	// Attachments carries `payload.attachments` from the start body: the
	// files the composer uploaded before sending, already split into
	// (bucket, name) by the route. #606.
	Attachments []CurrentTurnAttachmentRef
}

func (request CurrentAdhocStartRequest) Validate() error {
	if request.ProjectID <= 0 || request.ActorUserID <= 0 || request.TargetParticipantID < 0 ||
		!validUUID(request.ConversationUUID) || !validUUID(request.QuestionID) ||
		!validCurrentAgentText(request.UserInput, maxCurrentAgentUserInputBytes) ||
		(request.InteractionUUID != "" && !validUUID(request.InteractionUUID)) ||
		!validJSONObject(request.LLMSettings) {
		return ErrInvalidCurrentAgentStart
	}
	return nil
}

func (service *CurrentApplicationStartService) StartCurrentAdhoc(
	ctx context.Context,
	request CurrentAdhocStartRequest,
) (CurrentApplicationStartOutcome, error) {
	if err := request.Validate(); err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	if service.adhocResolver == nil {
		return CurrentApplicationStartOutcome{}, ErrUnsupportedCurrentAgentStart
	}
	target, err := service.adhocResolver.ResolveCurrentAdhoc(ctx, request)
	if err != nil {
		return CurrentApplicationStartOutcome{}, fmt.Errorf("resolve current ad-hoc target: %w", err)
	}
	if request.ProjectID > math.MaxInt32 || request.ActorUserID > math.MaxInt32 ||
		target.TargetParticipantID <= 0 || target.TargetParticipantID > math.MaxInt32 ||
		!validJSONObject(target.LLMSettings) || !validJSONArray(target.Tools) ||
		!validJSONArray(target.ChatHistory) || !validJSONObject(target.ConversationMeta) {
		return CurrentApplicationStartOutcome{}, ErrUnsupportedCurrentAgentStart
	}
	snapshot, err := currentAdhocSnapshot(request.LLMSettings, target)
	if err != nil {
		return CurrentApplicationStartOutcome{}, fmt.Errorf("build current ad-hoc snapshot: %w", err)
	}
	frozen, err := service.freezer.FreezeCurrentApplicationVersion(
		ctx,
		CurrentApplicationVersionFreezeRequest{
			ProjectID: int32(request.ProjectID), ActorUserID: int32(request.ActorUserID),
			VersionDetails: snapshot,
		},
	)
	if err != nil {
		return CurrentApplicationStartOutcome{}, fmt.Errorf("freeze current ad-hoc snapshot: %w", err)
	}
	suggestionPolicy := service.resolveNextInputSuggestionPolicy(
		ctx,
		request.ProjectID,
		request.ActorUserID,
	)
	toolkitGuardrails, err := service.resolveToolkitGuardrails(ctx)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	attachments, err := currentTurnAttachments(request.QuestionID, request.ConversationUUID, request.Attachments)
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	// #870: same recall the application start path performs — see
	// StartCurrentApplication's own comment.
	memoryRecall := service.resolveCurrentMemoryRecall(
		ctx, request.ProjectID, request.ActorUserID,
		currentMemoryRecallUserInputText(request.UserInput),
	)
	input, err := currentAdhocInput(
		request, target, frozen, suggestionPolicy, toolkitGuardrails, attachments, memoryRecall.Text,
	)
	if err != nil {
		return CurrentApplicationStartOutcome{}, fmt.Errorf("build current ad-hoc execution input: %w", err)
	}
	questionItemID := currentTurnUUID(request.QuestionID, "question-item")
	responseMessageID := currentTurnUUID(request.QuestionID, "response-message")
	questionMeta := json.RawMessage(`{}`)
	if request.InteractionUUID != "" {
		questionMeta, _ = json.Marshal(map[string]string{"interaction_uuid": request.InteractionUUID})
	}
	projectID := strconv.FormatInt(request.ProjectID, 10)
	actorID := strconv.FormatInt(request.ActorUserID, 10)
	outcome, err := service.admissions.Submit(ctx, SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: projectID, ResourceProjectID: projectID,
			ProjectionProjectID: projectID, ActorID: actorID,
		},
		IdempotencyKey: request.QuestionID, CapabilityID: executiondomain.AgentAdhocCapability,
		ClientStreamID: request.ConversationUUID, ClientMessageID: responseMessageID,
		SIOEvent: "chat_predict", Input: input,
		CurrentAdhocTurn: &CurrentAdhocTurn{
			ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
			ConversationUUID: request.ConversationUUID, TargetParticipantID: target.TargetParticipantID,
			QuestionID: request.QuestionID, QuestionItemID: questionItemID,
			ResponseMessageID: responseMessageID, QuestionMeta: questionMeta,
			UserInput: request.UserInput, Attachments: attachments,
		},
	})
	if err != nil {
		return CurrentApplicationStartOutcome{}, err
	}
	// Best-effort, AFTER admission — see recordCurrentMemoryUsage's own
	// comment for why this never affects the turn's outcome.
	service.recordCurrentMemoryUsage(ctx, request.ProjectID, responseMessageID, memoryRecall)
	return CurrentApplicationStartOutcome{
		ExecutionID: outcome.ExecutionID, CommandID: outcome.CommandID,
		ResponseMessageID: responseMessageID, Created: outcome.Created,
	}, nil
}

func currentAdhocSnapshot(
	override json.RawMessage,
	target CurrentAdhocTarget,
) (json.RawMessage, error) {
	base, err := currentAdhocLLMSettings(target.LLMSettings)
	if err != nil {
		return nil, err
	}
	requested, err := currentAdhocLLMSettings(override)
	if err != nil {
		return nil, err
	}
	for key, value := range requested {
		base[key] = value
	}
	if model, _ := base["model_name"].(string); model == "" {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	var tools []any
	if err := decodeCurrentJSON(target.Tools, &tools); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(map[string]any{
		"llm_settings": base,
		"tools":        tools,
	})
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return encoded, nil
}

func currentAdhocLLMSettings(source json.RawMessage) (map[string]any, error) {
	var decoded map[string]any
	if err := decodeCurrentJSON(source, &decoded); err != nil || decoded == nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	result := make(map[string]any, len(decoded))
	for key, value := range decoded {
		if value == nil {
			continue
		}
		switch key {
		case "model_name":
			model, ok := value.(string)
			if !ok || model == "" || len(model) > 256 || strings.ContainsAny(model, "\x00\r\n") {
				return nil, ErrUnsupportedCurrentAgentStart
			}
			result[key] = model
		case "model_project_id":
			integer, ok := positiveCurrentAgentJSONInteger(value)
			if !ok {
				return nil, ErrUnsupportedCurrentAgentStart
			}
			result[key] = integer
		case "max_tokens":
			integer, ok := currentAgentJSONInteger(value)
			if !ok || integer == 0 || integer < -1 || integer > math.MaxInt32 {
				return nil, ErrUnsupportedCurrentAgentStart
			}
			result[key] = integer
		case "temperature":
			number, ok := value.(json.Number)
			if !ok {
				return nil, ErrUnsupportedCurrentAgentStart
			}
			parsed, err := number.Float64()
			if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) || parsed <= 0 || parsed > 1 {
				return nil, ErrUnsupportedCurrentAgentStart
			}
			result[key] = number
		case "reasoning_effort":
			effort, ok := value.(string)
			if !ok || (effort != "low" && effort != "medium" && effort != "high" && effort != "none") {
				return nil, ErrUnsupportedCurrentAgentStart
			}
			result[key] = effort
		}
	}
	return result, nil
}

// attachments: see currentApplicationInput's note on why the regenerate and
// resume paths pass nil.
func currentAdhocInput(
	request CurrentAdhocStartRequest,
	target CurrentAdhocTarget,
	frozen json.RawMessage,
	nextInputSuggestion json.RawMessage,
	toolkitGuardrails json.RawMessage,
	attachments []CurrentTurnAttachment,
	memoryText string,
) (*runtimev1.AgentExecutionInputV1, error) {
	var snapshot map[string]any
	if err := decodeCurrentJSON(frozen, &snapshot); err != nil {
		return nil, err
	}
	settings, ok := snapshot["llm_settings"].(map[string]any)
	if !ok || settings == nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	tools, ok := snapshot["tools"].([]any)
	if !ok {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	llm, err := currentAdhocRuntimeLLM(settings)
	if err != nil {
		return nil, err
	}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	// #870: appended in the SAME position ResolveCurrentAdhocTurn's own SQL
	// already uses for `default_instructions` (agent_chat.sql) — see
	// appendCurrentInstructionsMemories's own comment.
	instructions := appendCurrentInstructionsMemories(target.Instructions, memoryText)
	application, err := json.Marshal(map[string]string{"instructions": instructions})
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	userInput, err := json.Marshal(request.UserInput)
	if err != nil {
		return nil, ErrInvalidCurrentAgentStart
	}
	persona, stepsLimit, err := currentAdhocConversationOptions(target.ConversationMeta)
	if err != nil {
		return nil, err
	}
	internalTools, err := currentAdhocRuntimeInternalTools(target.ConversationMeta)
	if err != nil {
		return nil, err
	}
	threadID := request.ConversationUUID
	conversationID := request.ConversationUUID
	executionGeneration := request.QuestionID
	input := &runtimev1.AgentExecutionInputV1{
		SchemaRevision: "elitea.runtime.agent-execution-input.v1",
		Llm:            llm, ChatHistory: bytes.Clone(target.ChatHistory), UserInput: userInput,
		ThreadId: &threadID, Tools: toolsJSON, Application: application,
		InternalTools: internalTools, McpTokens: []byte(`{}`),
		IgnoredMcpServers: []byte(`[]`), UserDeclinedMcpServers: []byte(`[]`),
		HitlDecisions: []byte(`[]`), ExecutionGeneration: &executionGeneration,
		Meta: []byte(`{}`), ConversationId: &conversationID, Persona: persona,
		ContextSettings: []byte(`{}`), InvokedSkills: []byte(`[]`),
		AppliedSkills: []byte(`[]`), AttachedSkills: []byte(`[]`),
		InputAttachments:       currentTurnInputAttachments(attachments),
		ParallelReconcile:      []byte(`null`),
		ParallelTerminalErrors: []byte(`[]`),
		NextInputSuggestion:    bytes.Clone(nextInputSuggestion),
		ToolkitGuardrails:      bytes.Clone(toolkitGuardrails),
	}
	if stepsLimit != nil {
		input.StepsLimit = stepsLimit
	}
	return input, nil
}

func currentAdhocRuntimeInternalTools(meta json.RawMessage) ([]byte, error) {
	var decoded struct {
		InternalTools json.RawMessage `json:"internal_tools"`
	}
	if !validJSONObject(meta) || json.Unmarshal(meta, &decoded) != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	if len(decoded.InternalTools) == 0 {
		return []byte(`[]`), nil
	}
	return currentRuntimeInternalTools(decoded.InternalTools)
}

func currentAdhocRuntimeLLM(settings map[string]any) ([]byte, error) {
	model, ok := settings["model_name"].(string)
	compatible, compatibleOK := settings["openai_compatible"].(bool)
	if !ok || model == "" || !compatibleOK {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	kwargs := map[string]any{
		"model": model, "stream": true, "openai_compatible": compatible,
	}
	for _, key := range []string{"model_project_id", "max_tokens", "reasoning_effort", "temperature"} {
		if value, exists := settings[key]; exists {
			kwargs[key] = value
		}
	}
	encoded, err := json.Marshal(map[string]any{"kwargs": kwargs})
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return encoded, nil
}

func currentAdhocConversationOptions(source json.RawMessage) (string, *int32, error) {
	var meta map[string]any
	if err := decodeCurrentJSON(source, &meta); err != nil {
		return "", nil, err
	}
	persona := "generic"
	if value, exists := meta["persona"]; exists && value != nil {
		candidate, ok := value.(string)
		if !ok || !currentAdhocPersona(candidate) {
			return "", nil, ErrUnsupportedCurrentAgentStart
		}
		persona = candidate
	}
	var stepsLimit *int32
	if value, exists := meta["steps_limit"]; exists && value != nil {
		parsed, ok := positiveCurrentAgentJSONInteger(value)
		// The same ceiling the application path enforces
		// (maxCurrentAgentStepLimit, start.go): forwarding a larger number
		// admits a turn the native runtime then refuses as invalid_profile —
		// a start that fails with no stated reason. The web UI clamps below
		// this, so only direct API writers can hit it.
		if !ok || parsed > maxCurrentAgentStepLimit {
			return "", nil, ErrUnsupportedCurrentAgentStart
		}
		bounded := int32(parsed)
		stepsLimit = &bounded
	}
	return persona, stepsLimit, nil
}

func currentAdhocPersona(value string) bool {
	switch value {
	case "generic", "qa", "nerdy", "quirky", "cynical", "none", "bare":
		return true
	default:
		return false
	}
}

func decodeCurrentJSON(source []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return ErrUnsupportedCurrentAgentStart
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return ErrUnsupportedCurrentAgentStart
	} else if !errors.Is(err, io.EOF) {
		return ErrUnsupportedCurrentAgentStart
	}
	return nil
}
