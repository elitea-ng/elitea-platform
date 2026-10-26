package agentexecution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/failurelimit"
	"github.com/go-chi/chi/v5"
)

// Steering during execution (client contract 1.27): queue, list, edit and
// remove the user messages sent while a cloud turn runs. The permission is
// the send's; the store checks that the caller is a user participant of the
// answer's conversation.
const (
	CurrentAgentSteeringPath        = "/api/v2/elitea_core/task/prompt_lib/{projectID}/{responseMessageID}/steering"
	CurrentAgentSteeringItemPath    = CurrentAgentSteeringPath + "/{steeringID}"
	CurrentAgentSteeringHandOffPath = CurrentAgentSteeringPath + "/handoff"
	CurrentAgentSteeringMode        = auth.PermissionModeDefault
	CurrentAgentSteeringPermission  = "models.chat.messages.create"

	// steeringRatePerMinute bounds queued messages per caller on one replica.
	steeringRatePerMinute = 30
	// maxSteeringBodyBytes leaves room for JSON escaping of a 16 KiB text.
	maxSteeringBodyBytes = 128 << 10
	steeringAuditEntity  = "chat_steering"
)

var ErrInvalidSteeringRoute = errors.New("invalid steering route dependencies")

// SteeringUseCase is agentexecution.SteeringService.
type SteeringUseCase interface {
	Queue(context.Context, agentexecutionapp.SteeringQueueRequest) (agentexecutionapp.SteeringMessage, error)
	List(context.Context, agentexecutionapp.SteeringTarget) ([]agentexecutionapp.SteeringMessage, error)
	Edit(context.Context, agentexecutionapp.SteeringEditRequest) (agentexecutionapp.SteeringMessage, error)
	Remove(context.Context, agentexecutionapp.SteeringRemoveRequest) error
	HandOff(context.Context, agentexecutionapp.SteeringHandOffRequest) ([]agentexecutionapp.SteeringMessage, error)
}

var _ SteeringUseCase = (*agentexecutionapp.SteeringService)(nil)

type steeringHandler struct {
	steering SteeringUseCase
	limiter  *failurelimit.Limiter
	audit    audit.Recorder
	now      func() time.Time
}

// NewCurrentAgentSteeringRoute mounts the four steering operations. recorder
// may be nil (no audit store composed); every other dependency is required.
func NewCurrentAgentSteeringRoute(
	steering SteeringUseCase,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
	recorder audit.Recorder,
) (http.Handler, error) {
	if steering == nil || !authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidSteeringRoute
	}
	handler := &steeringHandler{
		steering: steering,
		limiter:  failurelimit.New(steeringRatePerMinute, time.Minute),
		audit:    recorder,
		now:      time.Now,
	}
	guard := func(endpoint http.HandlerFunc) http.Handler {
		guarded := apimw.RequireResolvedPermissionsForProject(
			permissions,
			CurrentAgentSteeringMode,
			func(request *http.Request) (string, bool) {
				projectID := chi.URLParam(request, "projectID")
				_, valid := positiveCanonicalID(projectID)
				return projectID, valid
			},
			CurrentAgentSteeringPermission,
		)(endpoint)
		return apimw.Auth(authConfig)(guarded)
	}
	router := chi.NewRouter()
	router.Method(http.MethodPost, CurrentAgentSteeringPath, guard(handler.queue))
	router.Method(http.MethodGet, CurrentAgentSteeringPath, guard(handler.list))
	router.Method(http.MethodPatch, CurrentAgentSteeringItemPath, guard(handler.edit))
	router.Method(http.MethodDelete, CurrentAgentSteeringItemPath, guard(handler.remove))
	router.Method(http.MethodPost, CurrentAgentSteeringHandOffPath, guard(handler.handOff))
	return router, nil
}

func (handler *steeringHandler) target(writer http.ResponseWriter, request *http.Request) (agentexecutionapp.SteeringTarget, bool) {
	user, authenticated := auth.UserFromContext(request.Context())
	if !authenticated {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return agentexecutionapp.SteeringTarget{}, false
	}
	actorUserID, owningUser := user.OwningUserID()
	if !owningUser {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return agentexecutionapp.SteeringTarget{}, false
	}
	projectID, validProject := positiveCanonicalID(chi.URLParam(request, "projectID"))
	target := agentexecutionapp.SteeringTarget{
		ProjectID:   projectID,
		ActorUserID: actorUserID,
		ResponseMessageID: agentexecutionapp.CanonicalCurrentAgentResponseMessageID(
			chi.URLParam(request, "responseMessageID"),
		),
	}
	if !validProject || target.Validate() != nil {
		writeSteeringCode(writer, http.StatusBadRequest, "steering_invalid", "The steering request is not valid.")
		return agentexecutionapp.SteeringTarget{}, false
	}
	return target, true
}

func decodeSteeringBody(writer http.ResponseWriter, request *http.Request) (string, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxSteeringBodyBytes))
	var body struct {
		Content *string `json:"content"`
	}
	if err != nil || json.Unmarshal(raw, &body) != nil || body.Content == nil ||
		!agentexecutionapp.ValidSteeringContent(*body.Content) {
		writeSteeringCode(writer, http.StatusBadRequest, "steering_invalid",
			"The steering message must be text of at most 16 KiB.")
		return "", false
	}
	return *body.Content, true
}

func (handler *steeringHandler) queue(writer http.ResponseWriter, request *http.Request) {
	target, ok := handler.target(writer, request)
	if !ok {
		return
	}
	content, ok := decodeSteeringBody(writer, request)
	if !ok {
		return
	}
	limitKey := "actor:" + strconv.FormatInt(target.ActorUserID, 10)
	if admitted, retry := handler.limiter.Take(limitKey); !admitted {
		writer.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
		writeJSON(writer, http.StatusTooManyRequests, map[string]any{
			"error":          "rate_limited",
			"message":        "Too many steering messages. Retry after the interval in Retry-After.",
			"retry_after_ms": max(retry.Milliseconds(), 1),
		})
		return
	}
	message, err := handler.steering.Queue(request.Context(), agentexecutionapp.SteeringQueueRequest{
		SteeringTarget: target,
		IdempotencyKey: request.Header.Get("Idempotency-Key"),
		Content:        content,
	})
	if err != nil {
		handler.limiter.Refund(limitKey)
		handler.record(request, target, "queue", content, err)
		writeSteeringError(request.Context(), writer, err)
		return
	}
	handler.record(request, target, "queue", content, nil)
	writeJSON(writer, http.StatusAccepted, steeringView(message))
}

func (handler *steeringHandler) list(writer http.ResponseWriter, request *http.Request) {
	target, ok := handler.target(writer, request)
	if !ok {
		return
	}
	messages, err := handler.steering.List(request.Context(), target)
	if err != nil {
		writeSteeringError(request.Context(), writer, err)
		return
	}
	writeSteeringList(writer, messages)
}

func writeSteeringList(writer http.ResponseWriter, messages []agentexecutionapp.SteeringMessage) {
	items := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		items = append(items, steeringView(message))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items})
}

// handOff takes the caller's turn_ended messages, so the client sends them as
// the next message. The server's hand-off routine takes the same rows the
// same way: the messages it answers are the client's to send, and no other
// party starts a turn with them.
func (handler *steeringHandler) handOff(writer http.ResponseWriter, request *http.Request) {
	target, ok := handler.target(writer, request)
	if !ok {
		return
	}
	messages, err := handler.steering.HandOff(request.Context(), agentexecutionapp.SteeringHandOffRequest{
		SteeringTarget: target,
		IdempotencyKey: request.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		handler.record(request, target, "hand-off", "", err)
		writeSteeringError(request.Context(), writer, err)
		return
	}
	if len(messages) > 0 {
		handler.record(request, target, "hand-off", "", nil)
	}
	writeSteeringList(writer, messages)
}

func (handler *steeringHandler) steeringID(writer http.ResponseWriter, request *http.Request) (string, bool) {
	id := agentexecutionapp.CanonicalCurrentAgentResponseMessageID(chi.URLParam(request, "steeringID"))
	if !validSteeringID(id) {
		writeSteeringCode(writer, http.StatusBadRequest, "steering_invalid", "The steering request is not valid.")
		return "", false
	}
	return id, true
}

func (handler *steeringHandler) edit(writer http.ResponseWriter, request *http.Request) {
	target, ok := handler.target(writer, request)
	if !ok {
		return
	}
	steeringID, ok := handler.steeringID(writer, request)
	if !ok {
		return
	}
	content, ok := decodeSteeringBody(writer, request)
	if !ok {
		return
	}
	message, err := handler.steering.Edit(request.Context(), agentexecutionapp.SteeringEditRequest{
		SteeringTarget: target, SteeringID: steeringID, Content: content,
	})
	handler.record(request, target, "edit", content, err)
	if err != nil {
		writeSteeringError(request.Context(), writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, steeringView(message))
}

func (handler *steeringHandler) remove(writer http.ResponseWriter, request *http.Request) {
	target, ok := handler.target(writer, request)
	if !ok {
		return
	}
	steeringID, ok := handler.steeringID(writer, request)
	if !ok {
		return
	}
	err := handler.steering.Remove(request.Context(), agentexecutionapp.SteeringRemoveRequest{
		SteeringTarget: target, SteeringID: steeringID,
	})
	handler.record(request, target, "remove", "", err)
	if err != nil {
		writeSteeringError(request.Context(), writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusNoContent)
}

// record audits one write. The text never reaches the audit row: only its
// SHA-256 and size, so a reviewer can match a message without reading it.
func (handler *steeringHandler) record(request *http.Request, target agentexecutionapp.SteeringTarget, verb, content string, err error) {
	if handler.audit == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = steeringCode(err)
	}
	action := "steering " + verb + " for response " + target.ResponseMessageID + ": " + outcome
	if content != "" {
		digest := sha256.Sum256([]byte(content))
		action += "; content sha256 " + hex.EncodeToString(digest[:]) + ", " + strconv.Itoa(len(content)) + " bytes"
	}
	handler.audit.Record(context.WithoutCancel(request.Context()), audit.Event{
		Timestamp:  handler.now().UTC(),
		UserID:     audit.ID(target.ActorUserID),
		ProjectID:  audit.ID(target.ProjectID),
		EventType:  "chat",
		Action:     action,
		HTTPMethod: request.Method,
		HTTPRoute:  chi.RouteContext(request.Context()).RoutePattern(),
		IsError:    err != nil,
		EntityType: steeringAuditEntity,
		EntityName: target.ResponseMessageID,
	})
}

func steeringView(message agentexecutionapp.SteeringMessage) map[string]any {
	view := map[string]any{
		"id":                  message.ID,
		"response_message_id": message.ResponseMessageID,
		"author_id":           message.AuthorUserID,
		"content":             message.Content,
		"state":               string(message.State),
		"revision":            message.Revision,
		"created_at":          message.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":          message.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"injected_at":         nil,
		"message_id":          nil,
	}
	if message.InjectedAt != nil {
		view["injected_at"] = message.InjectedAt.UTC().Format(time.RFC3339Nano)
	}
	if message.MessageID != "" {
		view["message_id"] = message.MessageID
	}
	return view
}

func steeringCode(err error) string {
	switch {
	case errors.Is(err, agentexecutionapp.ErrInvalidSteering):
		return "steering_invalid"
	case errors.Is(err, agentexecutionapp.ErrSteeringIdempotencyRequired):
		return "idempotency_key_required"
	case errors.Is(err, agentexecutionapp.ErrSteeringTurnNotRunning):
		return "turn_not_running"
	case errors.Is(err, agentexecutionapp.ErrSteeringNotSupported):
		return "steering_not_supported"
	case errors.Is(err, agentexecutionapp.ErrSteeringQueueFull):
		return "steering_queue_full"
	case errors.Is(err, agentexecutionapp.ErrSteeringIdempotencyConflict):
		return "idempotency_conflict"
	case errors.Is(err, agentexecutionapp.ErrSteeringNotFound):
		return "steering_not_found"
	case errors.Is(err, agentexecutionapp.ErrSteeringNotPending):
		return "steering_not_pending"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "failed"
	}
}

func writeSteeringError(ctx context.Context, writer http.ResponseWriter, err error) {
	code := steeringCode(err)
	switch code {
	case "steering_invalid", "idempotency_key_required":
		writeSteeringCode(writer, http.StatusBadRequest, code, "The steering request is not valid.")
	case "turn_not_running":
		writeSteeringCode(writer, http.StatusConflict, code, "The turn is not running. Send a new message instead.")
	case "steering_not_supported":
		writeSteeringCode(writer, http.StatusConflict, code, "This turn cannot take a message while it runs.")
	case "steering_queue_full":
		writeSteeringCode(writer, http.StatusConflict, code, "Too many messages wait for this turn.")
	case "idempotency_conflict":
		writeSteeringCode(writer, http.StatusConflict, code, "The Idempotency-Key names another message.")
	case "steering_not_found":
		writeSteeringCode(writer, http.StatusNotFound, code, "This steering message does not exist.")
	case "steering_not_pending":
		writeSteeringCode(writer, http.StatusConflict, code, "The steering message is no longer pending.")
	case "timeout":
		writeError(writer, http.StatusGatewayTimeout, "Steering request timed out")
	default:
		slog.WarnContext(ctx, "steering request failed", "error_class", "steering_store")
		writeError(writer, http.StatusBadGateway, "The steering message could not be recorded")
	}
}

func writeSteeringCode(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]string{"error": code, "message": message})
}

func validSteeringID(value string) bool {
	return agentexecutionapp.ValidSteeringID(value)
}
