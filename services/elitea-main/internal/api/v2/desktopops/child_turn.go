package desktopops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/desktopwire"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	toolkitcalltoolapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/failurelimit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	ChildTurnStartPath = "/api/v2/elitea_core/local_child_turn/prompt_lib/{projectID}/{executionID}"
	ChildTurnReadPath  = ChildTurnStartPath + "/{childExecutionID}"
	// ChildTurnPermission is the chat composer's own permission: a child
	// turn is a chat turn of the nested agent, under the caller's grants.
	ChildTurnPermission = "models.chat.messages.create"

	// ChildTurnStartsPerMinute bounds one caller's child starts (per
	// replica). A supervisor delegates a few times a turn; thirty a minute
	// is far above that and far below a loop starting cloud turns.
	ChildTurnStartsPerMinute = 30

	// maxChildInput bounds the task a child is given (the start use case's
	// own ceiling is 256 KiB).
	maxChildInput = 128 * 1024
	// maxChildBody bounds the start body: the input plus the ids.
	maxChildBody = maxChildInput + 4*1024
	// maxChildOutput bounds the answer a read relays.
	maxChildOutput = 256 * 1024

	childTurnAuditEntity = "local_child_turn"
	childTurnRequiresTok = "local_child_turn_requires_token"
)

// ChildAgentStarter is the chat composer's start use case
// (*agentexecution.CurrentApplicationStartService): a child turn is admitted
// exactly like a typed one.
type ChildAgentStarter interface {
	StartCurrentApplication(context.Context, agentexecutionapp.CurrentApplicationStartRequest) (agentexecutionapp.CurrentApplicationStartOutcome, error)
}

// ChildAgentAuthorizer is *storage.ClientApplicationVersionService.
type ChildAgentAuthorizer interface {
	AuthorizeChildAgent(context.Context, storage.ChildAgentAuthorization) (storage.ChildAgentGrant, error)
}

var _ ChildAgentAuthorizer = (*storage.ClientApplicationVersionService)(nil)

// ChildTurnDependencies is what the child turn routes run on. Start,
// Authorizer, Turns and Store may each be nil, and the routes then answer
// 501, so a desktop does not read a 404 as "server too old".
type ChildTurnDependencies struct {
	Start      ChildAgentStarter
	Authorizer ChildAgentAuthorizer
	Turns      LiveTurnReader
	Store      localturn.ChildTurnStore
	// Audit receives one event per start attempt that names an agent. Required.
	Audit audit.Recorder
	// Limiter counts starts per caller; nil uses ChildTurnStartsPerMinute.
	Limiter *failurelimit.Limiter
}

// NewChildTurnRoute composes startLocalChildTurn and readLocalChildTurn
// (issue 1239).
//
// A nested agent of the agent a desktop runs locally runs as a CLOUD CHILD:
// an ordinary cloud turn on a worker (ADR-0029 decision 1: the laptop never
// joins the worker protocol), in a hidden child conversation linked to the
// parent turn by `meta.parent_execution_id`. The worker runs it with the
// credentials it materialises for any cloud turn; nothing secret returns.
//
// THE AUTHORITY OF THE PARENT TURN, AND NO MORE. A start names a LIVE local
// turn the caller started (owned by the caller, in this project, with the same
// credential family, not committed, not expired, local work allowed) and an
// agent that turn's own version holds as an `application` tool; any other
// agent is refused (403 `child_agent_not_in_agent`). A thread handed off to
// the cloud has no live local turn (a handoff waits for the local turn to end,
// and a cloud thread starts none), so it can start no child either.
//
// TOKEN CALLERS ONLY, like the local turn routes: a browser session answers
// 403 `local_child_turn_requires_token`.
//
// IDEMPOTENT BY KEY. One `idempotency_key` per intended call, repeated on every
// retry of it: a retry finds the conversation the first attempt made and the
// turn it admitted (the start's question id is derived from the key), and a
// different input under the same key is 409 `idempotency_conflict`.
//
// The child is read back (GET) by its execution id: its status, its answer,
// a summary of its tool calls and its model usage. A paused child answers
// `interrupted` with the pause; the desktop answers it through the chat's own
// continue route on the child conversation, and stops it through the chat's
// stop route.
func NewChildTurnRoute(
	deps ChildTurnDependencies,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (http.Handler, error) {
	if authConfig.PrincipalValidator == nil || permissions == nil || deps.Audit == nil {
		return nil, ErrInvalidRoute
	}
	h := newChildTurnHandler(deps)
	router := chi.NewRouter()
	router.Method(http.MethodPost, ChildTurnStartPath, gate(authConfig, permissions, ChildTurnPermission, h.start))
	router.Method(http.MethodGet, ChildTurnReadPath, gate(authConfig, permissions, ChildTurnPermission, h.read))
	return router, nil
}

func newChildTurnHandler(deps ChildTurnDependencies) *childTurnHandler {
	limiter := deps.Limiter
	if limiter == nil {
		limiter = failurelimit.New(ChildTurnStartsPerMinute, time.Minute)
	}
	recorder := deps.Audit
	if recorder == nil {
		recorder = discardAudit{}
	}
	return &childTurnHandler{
		starter: deps.Start, authorizer: deps.Authorizer, turns: deps.Turns, store: deps.Store,
		audit: recorder, limiter: limiter, now: time.Now,
	}
}

type childTurnHandler struct {
	starter    ChildAgentStarter
	authorizer ChildAgentAuthorizer
	turns      LiveTurnReader
	store      localturn.ChildTurnStore
	audit      audit.Recorder
	limiter    *failurelimit.Limiter
	now        func() time.Time
}

type childTurnBody struct {
	ApplicationID  int64  `json:"application_id"`
	VersionID      int64  `json:"version_id"`
	Input          string `json:"input"`
	IdempotencyKey string `json:"idempotency_key"`
}

// childStart is what one start's audit event records.
type childStart struct {
	actor, project       int64
	application, version int64
	parentExecutionID    string
	childExecutionID     string
	conversationUUID     string
	outcome              string
}

// childCaller answers the caller of a child route: a token caller's owning
// user, or false after writing the refusal.
func childCaller(writer http.ResponseWriter, request *http.Request) (auth.User, int64, bool) {
	user, ok := auth.UserFromContext(request.Context())
	if !ok {
		desktopwire.WriteError(writer, http.StatusUnauthorized, "unauthorized", "authentication required")
		return auth.User{}, 0, false
	}
	actor, ok := user.OwningUserID()
	if !ok {
		desktopwire.WriteError(writer, http.StatusUnauthorized, "unauthorized", "authentication required")
		return auth.User{}, 0, false
	}
	if user.TokenID == "" || !strings.EqualFold(user.AuthType, "token") {
		desktopwire.WriteError(writer, http.StatusForbidden, childTurnRequiresTok,
			"Cloud child turns need a native app token or a personal access token.")
		return auth.User{}, 0, false
	}
	return user, actor, true
}

func (h *childTurnHandler) start(writer http.ResponseWriter, request *http.Request) {
	started := h.now()
	projectID, okProject := desktopwire.PositiveID(chi.URLParam(request, "projectID"))
	parentExecutionID := chi.URLParam(request, "executionID")
	if !okProject || !localturn.ValidExecutionID(parentExecutionID) {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_local_child_turn", "Invalid project or execution id.")
		return
	}
	user, actor, ok := childCaller(writer, request)
	if !ok {
		return
	}
	if h.starter == nil || h.authorizer == nil || h.turns == nil || h.store == nil {
		desktopwire.WriteError(writer, http.StatusNotImplemented, "local_child_turn_unavailable",
			"This deployment runs no cloud agents, so a nested agent cannot run as a cloud child.")
		return
	}
	body, ok := decodeChildTurnBody(writer, request)
	if !ok {
		return
	}
	if !positiveInt32(body.ApplicationID) || !positiveInt32(body.VersionID) ||
		!toolkitcalltoolapp.ValidRequestKey(body.IdempotencyKey) ||
		strings.TrimSpace(body.Input) == "" || len(body.Input) > maxChildInput ||
		!utf8.ValidString(body.Input) || strings.ContainsRune(body.Input, '\x00') {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_local_child_turn",
			"Invalid child turn: application_id and version_id are required, input must be non-empty UTF-8 text "+
				"of at most 128 KiB, and idempotency_key 1 to 128 letters, digits, '-' or '_'.")
		return
	}
	call := &childStart{
		actor: actor, project: projectID, application: body.ApplicationID, version: body.VersionID,
		parentExecutionID: parentExecutionID,
	}
	recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
	writer = recorder
	defer func() { h.record(request.Context(), call, recorder.status, started) }()

	turn, err := h.turns.Live(request.Context(), projectID, actor,
		localturn.Credential{TokenID: user.TokenID, NativeClientID: user.NativeClientID}, parentExecutionID)
	if err != nil {
		call.outcome = "turn_refused"
		writeLiveTurnError(request.Context(), writer, err)
		return
	}
	grant, err := h.authorizer.AuthorizeChildAgent(request.Context(), storage.ChildAgentAuthorization{
		ProjectID: projectID, ActorID: actor,
		TurnApplicationID: turn.ApplicationID, TurnVersionID: turn.VersionID,
		ApplicationID: body.ApplicationID, VersionID: body.VersionID,
	})
	if err != nil {
		call.outcome = "refused"
		writeChildAuthorizationError(request.Context(), writer, err)
		return
	}
	digest := childInputDigest(body)
	conversation, err := h.store.PrepareChildConversation(request.Context(), localturn.ChildConversationRequest{
		ProjectID: projectID, ActorUserID: actor, ParentExecutionID: parentExecutionID,
		IdempotencyKey: body.IdempotencyKey, InputSHA256: digest,
		ApplicationID: body.ApplicationID, VersionID: body.VersionID, Name: grant.Name,
	})
	if err != nil {
		call.outcome = "store_error"
		slog.ErrorContext(request.Context(), "local child turn: prepare conversation", "err", err)
		writer.Header().Set("Retry-After", "1")
		desktopwire.WriteError(writer, http.StatusServiceUnavailable, "local_child_turn_unavailable_now",
			"The child conversation could not be prepared. Retry with the same idempotency_key.")
		return
	}
	call.conversationUUID = conversation.ConversationUUID
	if conversation.InputSHA256 != digest {
		call.outcome = "idempotency_conflict"
		desktopwire.WriteError(writer, http.StatusConflict, "idempotency_conflict",
			"This idempotency_key already started a child turn with another input.")
		return
	}
	if conversation.ExecutionID != "" {
		call.outcome, call.childExecutionID = "replayed", conversation.ExecutionID
		writeChildStarted(writer, conversation.ExecutionID, conversation.ResponseMessageID, conversation.ConversationUUID, false)
		return
	}
	// RATE. Charged up front in one step (Take), so concurrent starts cannot
	// all pass a check made before any of them is charged; refunded below
	// when this request admitted nothing (a failed start, or a replay of a
	// turn already admitted).
	limitKey := "actor:" + strconv.FormatInt(actor, 10)
	if admitted, retry := h.limiter.Take(limitKey); !admitted {
		call.outcome = "rate_limited"
		if conversation.Created {
			h.store.DiscardChildConversation(request.Context(), projectID, conversation.ConversationUUID)
		}
		writer.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
		desktopwire.WriteError(writer, http.StatusTooManyRequests, "rate_limited",
			"Too many child turns from this caller. Retry after the interval in Retry-After.")
		return
	}
	outcome, err := h.starter.StartCurrentApplication(request.Context(), agentexecutionapp.CurrentApplicationStartRequest{
		ProjectID:           projectID,
		ActorUserID:         actor,
		ConversationUUID:    conversation.ConversationUUID,
		TargetParticipantID: conversation.ParticipantID,
		QuestionID:          childQuestionID(projectID, parentExecutionID, body.IdempotencyKey),
		UserInput:           body.Input,
		// The person is at the desktop running the parent turn: their own
		// action, counted as such (shared 0140).
		TriggerOrigin: executiondomain.TriggerOriginManual,
	})
	if err != nil {
		h.limiter.Refund(limitKey)
		call.outcome = "start_error"
		if conversation.Created {
			h.store.DiscardChildConversation(request.Context(), projectID, conversation.ConversationUUID)
		}
		writeChildStartError(request.Context(), writer, err)
		return
	}
	if !outcome.Created {
		h.limiter.Refund(limitKey)
	}
	if err := h.store.RecordChildStart(request.Context(), projectID, conversation.ConversationUUID,
		outcome.ExecutionID, outcome.ResponseMessageID); err != nil {
		// The turn runs; only its stamp is missing. A retry with the key
		// replays the start (same question id) and stamps it then.
		call.outcome, call.childExecutionID = "unstamped", outcome.ExecutionID
		slog.ErrorContext(request.Context(), "local child turn: record start", "err", err)
		writer.Header().Set("Retry-After", "1")
		desktopwire.WriteError(writer, http.StatusServiceUnavailable, "local_child_turn_unavailable_now",
			"The child turn started but could not be recorded. Retry with the same idempotency_key.")
		return
	}
	call.outcome, call.childExecutionID = "started", outcome.ExecutionID
	writeChildStarted(writer, outcome.ExecutionID, outcome.ResponseMessageID, conversation.ConversationUUID, outcome.Created)
}

func writeChildStarted(writer http.ResponseWriter, executionID, responseMessageID, conversationUUID string, created bool) {
	desktopwire.WriteJSON(writer, http.StatusAccepted, map[string]any{
		"child_execution_id":  executionID,
		"response_message_id": responseMessageID,
		"conversation_uuid":   conversationUUID,
		"created":             created,
	})
}

// childQuestionID is the start's question id: a name-based UUID of the
// project, the parent turn and the key, so every attempt of one call admits
// (or replays) the same turn.
func childQuestionID(projectID int64, parentExecutionID, key string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL,
		[]byte("elitea:local_child_turn:"+strconv.FormatInt(projectID, 10)+":"+parentExecutionID+":"+key)).String()
}

func childInputDigest(body childTurnBody) string {
	digest := sha256.Sum256([]byte(strconv.FormatInt(body.ApplicationID, 10) + "\x00" +
		strconv.FormatInt(body.VersionID, 10) + "\x00" + body.Input))
	return hex.EncodeToString(digest[:])
}

func decodeChildTurnBody(writer http.ResponseWriter, request *http.Request) (childTurnBody, bool) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		desktopwire.WriteError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return childTurnBody{}, false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxChildBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var body childTurnBody
	if err := decoder.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			desktopwire.WriteError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large")
			return childTurnBody{}, false
		}
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_local_child_turn", "Invalid child turn.")
		return childTurnBody{}, false
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_local_child_turn", "Invalid child turn.")
		return childTurnBody{}, false
	}
	return body, true
}

func writeChildAuthorizationError(ctx context.Context, writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrChildAgentNotInAgent):
		desktopwire.WriteError(writer, http.StatusForbidden, "child_agent_not_in_agent",
			"This agent is not one the running agent uses as a tool, so it cannot run as its child.")
	case errors.Is(err, storage.ErrChildAgentPipeline):
		desktopwire.WriteError(writer, http.StatusUnprocessableEntity, "child_agent_unsupported",
			"A pipeline does not run as a nested agent of a workspace turn; run this agent in the cloud.")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		desktopwire.WriteError(writer, http.StatusServiceUnavailable, "local_child_turn_unavailable_now", "The call was cancelled.")
	default:
		if !errors.Is(err, storage.ErrContentUnavailable) {
			slog.ErrorContext(ctx, "local child turn: authorization failed", "err", err)
		}
		writer.Header().Set("Retry-After", "1")
		desktopwire.WriteError(writer, http.StatusServiceUnavailable, "local_child_turn_unavailable_now",
			"The agent version cannot be read right now. Retry.")
	}
}

func writeChildStartError(ctx context.Context, writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agentexecutionapp.ErrInvalidCurrentAgentStart),
		errors.Is(err, agentexecutionapp.ErrInvalidAgentAdmission),
		errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart):
		desktopwire.WriteError(writer, http.StatusUnprocessableEntity, "child_agent_unsupported",
			"The nested agent cannot be started on this deployment (a cloud chat turn of it would be refused too).")
	case errors.Is(err, executionapp.ErrIdempotencyConflict):
		desktopwire.WriteError(writer, http.StatusConflict, "idempotency_conflict",
			"This idempotency_key already started a child turn with another input.")
	case errors.Is(err, executionapp.ErrAdmissionCapacityExhausted):
		writer.Header().Set("Retry-After", "2")
		desktopwire.WriteError(writer, http.StatusServiceUnavailable, "local_child_turn_unavailable_now",
			"The cloud workers are at capacity. Retry with the same idempotency_key.")
	default:
		slog.ErrorContext(ctx, "local child turn: start failed", "err", err)
		writer.Header().Set("Retry-After", "1")
		desktopwire.WriteError(writer, http.StatusServiceUnavailable, "local_child_turn_unavailable_now",
			"The child turn could not be started. Retry with the same idempotency_key.")
	}
}

// read answers the child turn's state. Only the caller who started the child,
// under the parent turn the path names, reads it; the parent need not still
// be live (its commit may race the child's last read).
func (h *childTurnHandler) read(writer http.ResponseWriter, request *http.Request) {
	projectID, okProject := desktopwire.PositiveID(chi.URLParam(request, "projectID"))
	parentExecutionID := chi.URLParam(request, "executionID")
	childExecutionID := chi.URLParam(request, "childExecutionID")
	if !okProject || !localturn.ValidExecutionID(parentExecutionID) || !toolkitcalltoolapp.ValidRequestKey(childExecutionID) {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_local_child_turn", "Invalid project or execution id.")
		return
	}
	_, actor, ok := childCaller(writer, request)
	if !ok {
		return
	}
	if h.store == nil {
		desktopwire.WriteError(writer, http.StatusNotImplemented, "local_child_turn_unavailable",
			"This deployment runs no cloud agents, so a nested agent cannot run as a cloud child.")
		return
	}
	state, err := h.store.ReadChildTurn(request.Context(), projectID, actor, parentExecutionID, childExecutionID)
	switch {
	case errors.Is(err, localturn.ErrChildTurnNotFound):
		desktopwire.WriteError(writer, http.StatusNotFound, "local_child_turn_not_found",
			"No child turn with this execution id was started by this caller under this local turn.")
		return
	case err != nil:
		slog.ErrorContext(request.Context(), "local child turn: read", "err", err)
		writer.Header().Set("Retry-After", "1")
		desktopwire.WriteError(writer, http.StatusServiceUnavailable, "local_child_turn_unavailable_now",
			"The child turn cannot be read right now. Retry.")
		return
	}
	desktopwire.WriteJSON(writer, http.StatusOK, childTurnView(state))
}

// childInterruptKeys are the fields of a pending `hitl_interrupt` a read
// relays: what the person needs to decide, never the tool's arguments.
var childInterruptKeys = []string{
	"interrupt_id", "guardrail_type", "message", "policy_message", "action_label",
	"available_actions", "tool_name", "toolkit_name", "toolkit_type",
}

func childTurnView(state localturn.ChildTurnState) map[string]any {
	view := map[string]any{
		"child_execution_id":  state.ExecutionID,
		"response_message_id": state.ResponseMessageID,
		"conversation_uuid":   state.ConversationUUID,
	}
	switch {
	case state.Removed:
		view["status"] = "cancelled"
	case state.Streaming:
		view["status"] = "running"
	case state.IsError:
		view["status"] = "failed"
		view["error"] = state.Failure
	case state.Interrupt != nil:
		view["status"] = "interrupted"
		var interrupt map[string]any
		if json.Unmarshal(state.Interrupt, &interrupt) == nil {
			relayed := map[string]any{}
			for _, key := range childInterruptKeys {
				if value, ok := interrupt[key]; ok {
					relayed[key] = value
				}
			}
			view["interrupt"] = relayed
		}
	case state.AuthorizationPending:
		view["status"] = "authorization_required"
	case state.OutputLimit:
		view["status"] = "output_limit"
	default:
		view["status"] = "completed"
		output := state.Text
		if len(output) > maxChildOutput {
			cut := maxChildOutput
			for cut > 0 && !utf8.RuneStart(output[cut]) {
				cut--
			}
			output = output[:cut]
			view["output_truncated"] = true
		}
		view["output"] = output
	}
	calls := make([]map[string]any, 0, len(state.ToolCalls))
	for _, call := range state.ToolCalls {
		calls = append(calls, map[string]any{"tool_name": call.Name, "is_error": call.IsError})
	}
	view["tool_calls_summary"] = calls
	if state.Usage != nil {
		view["usage"] = map[string]any{
			"llm_calls":         state.Usage.LLMCalls,
			"prompt_tokens":     state.Usage.PromptTokens,
			"completion_tokens": state.Usage.CompletionTokens,
		}
	}
	return view
}

// record writes one audit event per start that named an agent: the child's
// usage is the child execution's own (the gateway attributes it by execution
// id), and this event is what links that execution to its parent turn.
func (h *childTurnHandler) record(ctx context.Context, call *childStart, status int, started time.Time) {
	if call == nil {
		return
	}
	statusCode := int32(status)
	duration := float64(h.now().Sub(started).Microseconds()) / 1000
	outcome := call.outcome
	if outcome == "" {
		outcome = "unknown"
	}
	child := call.childExecutionID
	if child == "" {
		child = "none"
	}
	action := "local child turn of agent " + strconv.FormatInt(call.application, 10) + " version " +
		strconv.FormatInt(call.version, 10) + ": " + outcome + "; parent execution " + call.parentExecutionID +
		"; child execution " + child
	if call.conversationUUID != "" {
		action += "; conversation " + call.conversationUUID
	}
	application := call.application
	h.audit.Record(context.WithoutCancel(ctx), audit.Event{
		Timestamp:  h.now().UTC(),
		UserID:     audit.ID(call.actor),
		ProjectID:  audit.ID(call.project),
		EventType:  "agent",
		Action:     action,
		HTTPMethod: http.MethodPost,
		HTTPRoute:  ChildTurnStartPath,
		StatusCode: &statusCode,
		DurationMS: &duration,
		IsError:    status >= http.StatusBadRequest,
		EntityType: childTurnAuditEntity,
		EntityID:   &application,
	})
}
