// Package localturns serves the desktop local turn operations (ADR-0029
// decision 5c, client contract 1.5): startLocalTurn and commitLocalTurn. See
// internal/application/localturn for the rules.
package localturns

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const (
	StartPath  = "/api/v2/elitea_core/local_turn/prompt_lib/{projectID}/{conversationID}"
	CommitPath = "/api/v2/elitea_core/local_turn_commit/prompt_lib/{projectID}/{executionID}"
	// Permission is the chat send's: a local turn writes the same messages.
	// 0068 already grants it, so no migration seeds anything.
	Permission = "models.chat.messages.create"
	Mode       = auth.PermissionModeDefault

	maxStartBody  = int64(512 * 1024)
	maxCommitBody = int64(8 * 1024 * 1024)
)

var ErrInvalidRoute = errors.New("invalid local turn route dependencies")

// UseCase is *localturn.Service.
type UseCase interface {
	Start(context.Context, localturn.StartRequest) (localturn.StartOutcome, error)
	Commit(context.Context, localturn.CommitRequest) (localturn.CommittedTurn, error)
}

var _ UseCase = (*localturn.Service)(nil)

// Route is both operations behind their own Auth and project permission, the
// way the chat execution routes are composed.
type Route struct {
	handler http.Handler
}

func NewRoute(useCase UseCase, authConfig apimw.AuthConfig, permissions auth.PermissionResolver) (*Route, error) {
	// No ForwardedIdentityVerifier requirement: only a token caller is ever
	// served (see caller), and an OIDC-only deployment's group has none.
	if useCase == nil || authConfig.PrincipalValidator == nil || permissions == nil {
		return nil, ErrInvalidRoute
	}
	h := &handler{useCase: useCase}
	gate := func(endpoint http.HandlerFunc) http.Handler {
		wrapped := apimw.RequireResolvedPermissionsForProject(
			permissions, Mode,
			func(request *http.Request) (string, bool) {
				projectID := chi.URLParam(request, "projectID")
				_, valid := positiveID(projectID)
				return projectID, valid
			},
			Permission,
		)(endpoint)
		return apimw.Auth(authConfig)(wrapped)
	}
	router := chi.NewRouter()
	router.Method(http.MethodPost, StartPath, gate(h.start))
	router.Method(http.MethodPost, CommitPath, gate(h.commit))
	return &Route{handler: router}, nil
}

func (route *Route) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type handler struct {
	useCase UseCase
}

type startBody struct {
	QuestionID    string `json:"question_id"`
	UserInput     string `json:"user_input"`
	ParticipantID int64  `json:"participant_id"`
}

type commitBody struct {
	UserMessage struct {
		Content string `json:"content"`
	} `json:"user_message"`
	AssistantMessage struct {
		Content string `json:"content"`
		IsError bool   `json:"is_error"`
		Error   string `json:"error"`
	} `json:"assistant_message"`
	ToolCalls     json.RawMessage            `json:"tool_calls"`
	ThinkingSteps []json.RawMessage          `json:"thinking_steps"`
	HITLExchanges []localturn.HITLExchange   `json:"hitl_exchanges"`
	LocalWork     *localturn.LocalWorkReport `json:"local_work"`
}

// caller is the token principal both operations require. A local turn is a
// desktop (native token) or programmatic (PAT) operation; a browser session
// has no local machine to run it on.
func caller(writer http.ResponseWriter, request *http.Request) (auth.User, int64, bool) {
	user, ok := auth.UserFromContext(request.Context())
	if !ok {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "authentication required")
		return auth.User{}, 0, false
	}
	actor, ok := user.OwningUserID()
	if !ok {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "authentication required")
		return auth.User{}, 0, false
	}
	if user.TokenID == "" || !strings.EqualFold(user.AuthType, "token") {
		writeError(writer, http.StatusForbidden, "local_turn_requires_token",
			"Local turns need a native app token or a personal access token.")
		return auth.User{}, 0, false
	}
	return user, actor, true
}

func decodeBody(writer http.ResponseWriter, request *http.Request, limit int64, into any) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, limit)
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(into); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large")
			return false
		}
		writeError(writer, http.StatusBadRequest, "invalid_local_turn", "Invalid local turn request")
		return false
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "invalid_local_turn", "Invalid local turn request")
		return false
	}
	return true
}

func (h *handler) start(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := positiveID(chi.URLParam(request, "projectID"))
	if !ok {
		writeError(writer, http.StatusBadRequest, "invalid_local_turn", "Invalid local turn request")
		return
	}
	user, actor, ok := caller(writer, request)
	if !ok {
		return
	}
	var body startBody
	if !decodeBody(writer, request, maxStartBody, &body) {
		return
	}
	outcome, err := h.useCase.Start(request.Context(), localturn.StartRequest{
		ProjectID: projectID, ActorUserID: actor, TokenID: user.TokenID,
		NativeClientID: user.NativeClientID, ConversationUUID: chi.URLParam(request, "conversationID"),
		QuestionID: body.QuestionID, UserInput: body.UserInput, ParticipantID: body.ParticipantID,
		AuditRoute: StartPath,
	})
	if err != nil {
		writeUseCaseError(request.Context(), writer, "start", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"execution_id":        outcome.ExecutionID,
		"question_id":         outcome.QuestionID,
		"response_message_id": outcome.ResponseMessageID,
		"conversation_uuid":   outcome.ConversationUUID,
		"participant_id":      outcome.ParticipantID,
		"expires_at":          outcome.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"created":             outcome.Created,
		"memory_recall": map[string]any{
			"text":       outcome.Recall.Text,
			"count":      outcome.Recall.Count,
			"memory_ids": outcome.Recall.IDs,
		},
	})
}

func (h *handler) commit(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := positiveID(chi.URLParam(request, "projectID"))
	if !ok {
		writeError(writer, http.StatusBadRequest, "invalid_local_turn", "Invalid local turn request")
		return
	}
	_, actor, ok := caller(writer, request)
	if !ok {
		return
	}
	var body commitBody
	if !decodeBody(writer, request, maxCommitBody, &body) {
		return
	}
	report := localturn.LocalWorkReport{}
	if body.LocalWork != nil {
		report = *body.LocalWork
	}
	turn, err := h.useCase.Commit(request.Context(), localturn.CommitRequest{
		ProjectID: projectID, ActorUserID: actor, ExecutionID: chi.URLParam(request, "executionID"),
		UserMessage:      body.UserMessage.Content,
		AssistantMessage: body.AssistantMessage.Content,
		AssistantIsError: body.AssistantMessage.IsError,
		AssistantError:   body.AssistantMessage.Error,
		ToolCalls:        body.ToolCalls, ThinkingSteps: body.ThinkingSteps,
		HITLExchanges: body.HITLExchanges, Report: report, AuditRoute: CommitPath,
	})
	if err != nil {
		writeUseCaseError(request.Context(), writer, "commit", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"execution_id":        turn.ExecutionID,
		"conversation_uuid":   turn.ConversationUUID,
		"question_message_id": turn.QuestionMessageID,
		"response_message_id": turn.ResponseMessageID,
		"memories_used":       turn.MemoriesUsed,
		"committed_at":        turn.CommittedAt.UTC().Format(time.RFC3339Nano),
		"created":             turn.Created,
	})
}

func writeUseCaseError(ctx context.Context, writer http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, localturn.ErrInvalid):
		writeError(writer, http.StatusBadRequest, "invalid_local_turn", "Invalid local turn request")
	case errors.Is(err, localturn.ErrLocalWorkDisabled):
		writeError(writer, http.StatusForbidden, "local_work_disabled",
			"Local work is turned off for this deployment (native client policy local_work.allowed).")
	case errors.Is(err, localturn.ErrNotFound):
		writeError(writer, http.StatusNotFound, "local_turn_not_found",
			"The conversation or the local turn was not found for this caller.")
	case errors.Is(err, localturn.ErrParticipant):
		writeError(writer, http.StatusUnprocessableEntity, "local_turn_participant",
			"The answering participant is not an agent or model participant of this conversation.")
	case errors.Is(err, localturn.ErrConflict):
		writeError(writer, http.StatusConflict, "local_turn_conflict",
			"The question id is already used by another turn.")
	case errors.Is(err, localturn.ErrAlreadyCommitted):
		writeError(writer, http.StatusConflict, "local_turn_already_committed",
			"This local turn is already committed.")
	case errors.Is(err, localturn.ErrExpired):
		writeError(writer, http.StatusGone, "local_turn_expired",
			"This local turn passed its deadline. Start a new turn with a new question id.")
	case errors.Is(err, localturn.ErrUnavailable):
		writer.Header().Set("Retry-After", "1")
		writeError(writer, http.StatusServiceUnavailable, "local_turn_unavailable",
			"The local turn cannot be served right now. Retry with the same body.")
	default:
		slog.ErrorContext(ctx, "local turn failed", "operation", operation, "err", err)
		writeError(writer, http.StatusInternalServerError, "local_turn_failed", "The local turn could not be recorded.")
	}
}

func positiveID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0 && id <= math.MaxInt32 && strconv.FormatInt(id, 10) == raw
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]string{"error": code, "message": message})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
