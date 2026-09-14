package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	configsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	conversationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	agentapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/google/uuid"
)

const internalChatSend internalChatOperation = "send"

type internalChatStartUseCase interface {
	AgentStartUseCase
	StartCurrentAdhoc(context.Context, agentapp.CurrentAdhocStartRequest) (agentapp.CurrentApplicationStartOutcome, error)
}
type internalChatSendRuntime struct {
	start           internalChatStartUseCase
	conversationGet http.HandlerFunc
	modelsGet       http.HandlerFunc
	read            func(context.Context, string, string) (turnState, error)
}

// WithInternalChatRuntime binds message sending to the browser's shared services.
func WithInternalChatRuntime(conversations *conversationsapi.Handler, models *configsapi.CurrentConfigurationToolHandler) Option {
	return func(h *Handler) {
		start, ok := h.start.(internalChatStartUseCase)
		if !ok || conversations == nil || models == nil {
			return
		}
		h.internalChatSend = &internalChatSendRuntime{start: start, conversationGet: conversations.Get, modelsGet: models.Models, read: h.readTurnState}
	}
}

type internalChatSendArguments struct {
	ProjectID        int64           `json:"project_id"`
	ConversationUUID string          `json:"conversation_uuid"`
	ParticipantID    int64           `json:"participant_id"`
	QuestionID       string          `json:"question_id"`
	UserInput        string          `json:"user_input"`
	LLMSettings      json.RawMessage `json:"llm_settings"`
	AwaitTaskTimeout *int            `json:"await_task_timeout"`
}

func (e *internalChatSendRuntime) execute(ctx context.Context, projectID, actorID int64, args map[string]any) (internalApplicationExecution, error) {
	if e == nil || e.start == nil || e.read == nil || ctx == nil || actorID <= 0 {
		return internalApplicationExecution{}, errors.New("chat execution unavailable")
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return internalNotificationBadRequest("invalid chat send arguments")
	}
	var input internalChatSendArguments
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.ProjectID != projectID || input.ParticipantID < 0 || input.ParticipantID > 2147483647 || len(input.UserInput) == 0 || len(input.UserInput) > 32768 {
		return internalNotificationBadRequest("invalid chat send arguments")
	}
	parsed, err := uuid.Parse(input.ConversationUUID)
	if err != nil || parsed.String() != input.ConversationUUID {
		return internalNotificationBadRequest("conversation_uuid must be a canonical UUID")
	}
	if input.QuestionID == "" {
		input.QuestionID = uuid.NewString()
	}
	question, err := uuid.Parse(input.QuestionID)
	if err != nil || question.String() != input.QuestionID {
		return internalNotificationBadRequest("question_id must be a canonical UUID")
	}
	wait := 30
	if input.AwaitTaskTimeout != nil {
		wait = *input.AwaitTaskTimeout
	}
	if wait < -1 || wait > 300 {
		return internalNotificationBadRequest("await_task_timeout must be between -1 and 300")
	}
	principal, ok := auth.UserFromContext(ctx)
	if !ok {
		return internalApplicationExecution{}, errors.New("chat principal unavailable")
	}
	principal.UserID = strconv.FormatInt(actorID, 10)
	ctx = auth.ContextWithUser(ctx, principal)
	target, err := resolveInternalChatSendTarget(ctx, projectID, input.ConversationUUID, input.ParticipantID, input.LLMSettings, e.conversationGet, e.modelsGet)
	if err != nil {
		return internalNotificationBadRequest("conversation, participant, or model is unavailable; nothing was admitted")
	}
	var outcome agentapp.CurrentApplicationStartOutcome
	if target.application {
		outcome, err = e.start.StartCurrentApplication(ctx, agentapp.CurrentApplicationStartRequest{ProjectID: projectID, ActorUserID: actorID, ConversationUUID: input.ConversationUUID, TargetParticipantID: target.participantID, QuestionID: input.QuestionID, UserInput: input.UserInput})
	} else {
		outcome, err = e.start.StartCurrentAdhoc(ctx, agentapp.CurrentAdhocStartRequest{ProjectID: projectID, ActorUserID: actorID, ConversationUUID: input.ConversationUUID, TargetParticipantID: target.participantID, QuestionID: input.QuestionID, UserInput: input.UserInput, LLMSettings: target.llmSettings})
	}
	if err != nil {
		return internalNotificationBadRequest("chat admission was not confirmed; check the conversation before retrying with the same question_id")
	}
	reply := map[string]any{"task_id": outcome.ExecutionID, "execution_id": outcome.ExecutionID, "response_message_id": outcome.ResponseMessageID, "question_id": input.QuestionID, "conversation_uuid": input.ConversationUUID, "status": "pending"}
	finish := func(status int) (internalApplicationExecution, error) {
		body, err := json.Marshal(reply)
		return internalApplicationExecution{status: status, body: body}, err
	}
	if wait <= 0 {
		return finish(http.StatusAccepted)
	}
	observing, cancel := context.WithTimeout(ctx, time.Duration(wait)*time.Second)
	defer cancel()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-observing.Done():
			return finish(http.StatusAccepted)
		case <-timer.C:
		}
		state, err := e.read(observing, fmt.Sprintf("p_%d", projectID), outcome.ResponseMessageID)
		if err != nil {
			reply["observation_unavailable"] = true
			return finish(http.StatusAccepted)
		}
		if state.settled {
			result := state.result(Tool{Name: "post_elitea_core_messages"}, outcome.ExecutionID)
			reply["result"] = result
			if failed, _ := result["isError"].(bool); failed {
				reply["status"] = "incomplete"
				return finish(http.StatusConflict)
			}
			reply["status"] = "completed"
			return finish(http.StatusOK)
		}
		timer.Reset(time.Second)
	}
}
