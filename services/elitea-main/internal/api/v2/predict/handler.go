package predict

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// maxPredictRequestBytes bounds the body. user_input and chat_history are free
// text, so the bound is generous, but it is not absent: the whole body is read
// into memory before the gateway hop starts.
const maxPredictRequestBytes = 4 << 20 // 4 MiB

// NotConfiguredCode is the machine-readable code this route answers with while
// LLM_GATEWAY_URL is empty. It mirrors api.LLMNotConfiguredCode's contract on
// /llm: 503 naming the missing configuration, never an invisible 404. #126 is
// the record of what a 404 costs — it is indistinguishable from a typo'd path,
// so the gap survived an entire replatform.
const NotConfiguredCode = "llm_gateway_not_configured"

// llmSettings is the nested block the SPA and legacy both send.
type llmSettings struct {
	ModelName string   `json:"model_name"`
	MaxTokens *int     `json:"max_tokens"`
	Temp      *float64 `json:"temperature"`
	Reasoning string   `json:"reasoning_effort"`
	// model_project_id and integration_uid are accepted and IGNORED. The
	// gateway resolves the model against the project signed into the identity
	// headers; honouring a caller-supplied project id here would let a caller
	// spend another project's credentials from a path whose only membership
	// check is on {projectID}.
	ModelProjectID *int64 `json:"model_project_id"`
	IntegrationUID string `json:"integration_uid"`
}

// predictRequest is the accepted subset of legacy's LLMPredictRequest. That
// model carries `extra: allow`, so unknown fields are tolerated here too
// (the decoder is not strict) — but only the fields below have any effect.
type predictRequest struct {
	UserInput         string       `json:"user_input"`
	Instructions      string       `json:"instructions"`
	ChatHistory       []Message    `json:"chat_history"`
	LLMSettings       *llmSettings `json:"llm_settings"`
	AwaitTaskTimeout  *int         `json:"await_task_timeout"`
	ReturnChatHistory bool         `json:"return_chat_history"`
	// sid, stream_id, message_id, thread_id and checkpoint_id are legacy's
	// socket.io/async plumbing. They are accepted (a client that sends them
	// must not get a 400) and have no effect: there is no async half here.
}

// Handler serves the predict_llm route.
type Handler struct {
	completer Completer
}

// NewHandler builds the handler. completer may be nil — that is the
// "LLM_GATEWAY_URL is unset" deployment, and PredictLLM then answers 503
// naming the variable. It is NOT a reason to leave the route unregistered.
func NewHandler(completer Completer) *Handler {
	return &Handler{completer: completer}
}

// PredictLLM runs one stateless LLM turn and returns its text.
//
// STATUS CONTRACT. 200 with the content; 400 for an unusable body; 503 when no
// LLM plane is composed; 502 when the gateway hop itself failed. Legacy
// answered 500 for that last case, from inside a process that WAS the LLM
// caller. Here the failure is one identifiable hop away, and an operator who
// cannot tell "elitea-main is broken" from "the gateway did not answer" cannot
// tell which component to look at. Callers key on ok/not-ok, so the widened
// vocabulary costs them nothing.
func (h *Handler) PredictLLM(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	r.Body = http.MaxBytesReader(w, r.Body, maxPredictRequestBytes)
	var body predictRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body too large"})
			return
		}
		if errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "request body is empty"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	if strings.TrimSpace(body.UserInput) == "" {
		// Legacy's pydantic model makes user_input required and answers 400
		// with the validation errors. The shape differs; the status and the
		// meaning do not.
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "user_input is required"})
		return
	}

	if h.completer == nil {
		slog.ErrorContext(r.Context(), "predict_llm: no LLM plane composed", "project_id", projectID)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the LLM gateway is not configured: LLM_GATEWAY_URL is empty, so no completion backend is composed",
			"code":  NotConfiguredCode,
		})
		return
	}

	completion := CompletionRequest{
		ProjectID: projectID,
		UserID:    callerUserID(r.Context()),
		Messages:  buildMessages(body),
	}
	// The shared completer resolves an omitted model through the project catalog.
	if body.LLMSettings != nil {
		completion.Model = body.LLMSettings.ModelName
		completion.Temperature = body.LLMSettings.Temp
		completion.MaxTokens = body.LLMSettings.MaxTokens
		completion.ReasoningEffort = body.LLMSettings.Reasoning
	}

	ctx, cancel := context.WithTimeout(r.Context(), resolveRequestTimeout(body.AwaitTaskTimeout))
	defer cancel()

	content, err := h.completer.Complete(ctx, completion)
	if err != nil {
		// The real cause is logged server-side only; the browser gets a fixed,
		// safe message. Never a 200 with empty content: the callers render
		// whatever comes back straight into a document, so a silent failure
		// would look like the model answering with nothing.
		slog.ErrorContext(r.Context(), "predict_llm: completion failed",
			"project_id", projectID, "model", completion.Model, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "the LLM gateway could not complete this request",
		})
		return
	}

	// `content` is the field every current caller reads first
	// (apps/elitea-web features/agents/api/aiEdit.ts readGeneratedContent, and
	// the canvas quick fix). `messages` repeats it in the OpenAI-ish shape the
	// same reader falls back to, so a caller written against either shape
	// works without the two ever disagreeing.
	response := map[string]any{
		"content":  content,
		"messages": []Message{{Role: "assistant", Content: content}},
	}
	if body.ReturnChatHistory {
		response["chat_history"] = append(buildMessages(body), Message{Role: "assistant", Content: content})
	}
	writeJSON(w, http.StatusOK, response)
}

// buildMessages assembles the OpenAI message list: the optional system
// instructions, then the prior turns, then this turn's user input last.
func buildMessages(body predictRequest) []Message {
	messages := make([]Message, 0, len(body.ChatHistory)+2)
	if strings.TrimSpace(body.Instructions) != "" {
		messages = append(messages, Message{Role: "system", Content: body.Instructions})
	}
	for _, message := range body.ChatHistory {
		// A history entry with no role or no content is dropped rather than
		// forwarded: the gateway rejects the whole request for one malformed
		// element, which would turn a stray client entry into a hard failure
		// of a turn that is otherwise complete.
		if message.Role == "" || message.Content == "" {
			continue
		}
		messages = append(messages, message)
	}
	return append(messages, Message{Role: "user", Content: body.UserInput})
}

// callerUserID reads the authenticated principal the router's Auth middleware
// put on the context. It is never taken from a header or from the body.
func callerUserID(ctx context.Context) string {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return ""
	}
	if user.UserID != "" {
		return user.UserID
	}
	return user.ID
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode predict_llm response", "err", err)
	}
}
