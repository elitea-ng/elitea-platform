package drafts

// stream.go — POST /generate_application_draft_stream/prompt_lib/{projectID}
// (ADR-0037 decision 4): the conversational, streamed twin of
// GenerateApplicationDraft.
//
// One call is one gateway completion (v2predict.Stream over the same
// gRPC-over-mTLS hop as the one-shot route), so the budget cost per call is
// the one-shot route's. The model text is relayed as draft.token frames for
// display; the draft itself is decided only once the answer is whole, by
// finishApplicationDraft — the same function the one-shot route uses — so
// the two routes cannot drift. The route is a synchronous browser call: it
// saves nothing and does not enter the command bus.
//
// STATUS SPLIT. A body that cannot be used is a 400 and a deployment with no
// LLM plane is a 503, both as plain JSON before the stream opens. From then
// on the status is 200 and a failure is the last frame, draft.error, whose
// `status` field is what the one-shot route answers for the same failure.
// The stream opens before the gateway answers on purpose: a reasoning model
// can think for longer than a load balancer's idle timeout, and only an open
// stream can carry the keepalive comments that hold the connection.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	v2predict "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/predict"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/ssewriter"
)

// The draft stream's event names.
const (
	EventDraftToken    = "draft.token"
	EventDraftComplete = "draft.complete"
	EventDraftError    = "draft.error"
)

// GatewayFailedCode is the draft.error code of a gateway hop that failed.
const GatewayFailedCode = "gateway_failed"

const (
	// maxDraftHistoryTurns keeps the newest turns of a long builder
	// conversation; older ones are dropped rather than refused.
	maxDraftHistoryTurns = 20
	// maxDraftTurnLength bounds one history turn, in runes.
	maxDraftTurnLength = 16384
	// draftStreamTimeout bounds one generation: predict_llm's ceiling.
	draftStreamTimeout = 180 * time.Second
)

// draftKeepalive is how long the stream may stay silent; a variable so a test
// can shorten it.
var draftKeepalive = ssewriter.KeepaliveInterval

type applicationDraftStreamRequest struct {
	UserDescription     string              `json:"user_description"`
	ConversationHistory []v2predict.Message `json:"conversation_history"`
	CurrentDraft        *currentAgentDraft  `json:"current_draft"`
	LLMSettings         *llmSettings        `json:"llm_settings"`
}

// currentAgentDraft is the part of an AgentDefinition the model is shown when
// it revises a draft. Only the fields the model writes are read back.
type currentAgentDraft struct {
	Name                 string   `json:"name"`
	Description          string   `json:"description"`
	Instructions         string   `json:"instructions"`
	WelcomeMessage       string   `json:"welcome_message,omitempty"`
	ConversationStarters []string `json:"conversation_starters,omitempty"`
}

// capped bounds every field to the AgentDefinition limit, as
// finishApplicationDraft does for the model's own answer.
func (d currentAgentDraft) capped() currentAgentDraft {
	out := currentAgentDraft{
		Name:           strings.TrimSpace(truncate(d.Name, applicationNameMaxLength)),
		Description:    strings.TrimSpace(truncate(d.Description, applicationDescriptionMaxLength)),
		Instructions:   strings.TrimSpace(truncate(d.Instructions, applicationInstructionsSanityLength)),
		WelcomeMessage: strings.TrimSpace(truncate(d.WelcomeMessage, applicationWelcomeMessageMaxLength)),
	}
	for _, starter := range d.ConversationStarters {
		starter = strings.TrimSpace(starter)
		if starter == "" {
			continue
		}
		out.ConversationStarters = append(out.ConversationStarters, truncate(starter, applicationStarterMaxLength))
		if len(out.ConversationStarters) == applicationMaxConversationStarters {
			break
		}
	}
	return out
}

// DraftStreamError is the data of a draft.error frame.
type DraftStreamError struct {
	Code    string `json:"code"`
	Status  int    `json:"status"`
	Message string `json:"message"`
}

// GenerateApplicationDraftStream serves the streaming draft route.
func (h *Handler) GenerateApplicationDraftStream(w http.ResponseWriter, r *http.Request) {
	var body applicationDraftStreamRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if !requireUserDescription(w, body.UserDescription) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	if h.completer == nil {
		slog.ErrorContext(r.Context(), "draft stream: no LLM plane composed", "project_id", projectID)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the LLM gateway is not configured: LLM_GATEWAY_URL is empty, so no completion backend is composed",
			"code":  NotConfiguredCode,
		})
		return
	}
	request, err := streamCompletionRequest(projectID, callerUserID(r.Context()), body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// New sets the event-stream headers and refuses a writer that cannot
	// flush; the frames themselves go through the keepalive, one write each,
	// so a keepalive comment never lands inside a frame.
	if _, err := ssewriter.New(w); err != nil {
		writeError(w, http.StatusInternalServerError, "streaming is not supported on this connection")
		return
	}
	w.WriteHeader(http.StatusOK)
	// Flush the headers now: a reasoning model can think for a long time
	// before its first token, and a client (or proxy) that has seen no
	// response head yet cannot tell an open stream from a dead connection.
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	keepalive := ssewriter.StartKeepalive(w, draftKeepalive)
	defer keepalive.Stop()
	frame := func(event string, data any) error {
		encoded, err := json.Marshal(data)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(keepalive, "event: %s\ndata: %s\n\n", event, encoded)
		return err
	}

	ctx, cancel := context.WithTimeout(r.Context(), draftStreamTimeout)
	defer cancel()
	content, err := v2predict.Stream(ctx, h.completer, request, func(delta string) error {
		return frame(EventDraftToken, map[string]string{"text": delta})
	})
	if err != nil {
		if r.Context().Err() != nil {
			return // The client left; there is no one to tell.
		}
		slog.ErrorContext(r.Context(), "draft stream: completion failed",
			"project_id", projectID, "model", request.Model, "err", err)
		message := "the LLM gateway could not complete this request"
		if errors.Is(err, context.DeadlineExceeded) {
			message = "the LLM gateway did not finish the draft in time"
		}
		_ = frame(EventDraftError, DraftStreamError{Code: GatewayFailedCode, Status: http.StatusBadGateway, Message: message})
		return
	}

	draft, failure := h.finishApplicationDraft(r.Context(), projectID, content)
	if failure != nil {
		failure.log(r.Context())
		_ = frame(EventDraftError, DraftStreamError{Code: failure.code, Status: failure.status, Message: failure.message})
		return
	}
	_ = frame(EventDraftComplete, draft)
}

// streamCompletionRequest builds the one completion: the system prompt (with
// the current draft when one is being revised), the bounded history, then
// this turn's request.
func streamCompletionRequest(projectID, userID string, body applicationDraftStreamRequest) (v2predict.CompletionRequest, error) {
	prompt := applicationDraftSystemPrompt + "\n\n" + conversationalDraftPrompt
	messages := []v2predict.Message{{Role: "system", Content: prompt}}
	history := body.ConversationHistory
	if len(history) > maxDraftHistoryTurns {
		history = history[len(history)-maxDraftHistoryTurns:]
	}
	for _, turn := range history {
		role := strings.TrimSpace(turn.Role)
		if (role != "user" && role != "assistant") || strings.TrimSpace(turn.Content) == "" {
			continue
		}
		messages = append(messages, v2predict.Message{Role: role, Content: truncate(turn.Content, maxDraftTurnLength)})
	}
	// The caller's draft is untrusted text: it rides in a user message (never
	// the system prompt) and is capped to the AgentDefinition limits first.
	request := body.UserDescription
	if body.CurrentDraft != nil {
		encoded, err := json.Marshal(body.CurrentDraft.capped())
		if err != nil {
			return v2predict.CompletionRequest{}, errors.New("current_draft could not be read")
		}
		request = "This is the current draft. Revise it to satisfy the request below. Keep what still applies. Return the complete draft.\n" +
			string(encoded) + "\n\nRequest:\n" + body.UserDescription
	}
	messages = append(messages, v2predict.Message{Role: "user", Content: request})

	completion := v2predict.CompletionRequest{ProjectID: projectID, UserID: userID, Messages: messages}
	if body.LLMSettings != nil {
		completion.Model = body.LLMSettings.ModelName
		completion.Temperature = body.LLMSettings.Temperature
		completion.MaxTokens = body.LLMSettings.MaxTokens
		completion.ReasoningEffort = body.LLMSettings.ReasoningEffort
	}
	return completion, nil
}
