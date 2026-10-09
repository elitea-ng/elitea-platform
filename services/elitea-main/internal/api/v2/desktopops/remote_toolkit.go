package desktopops

import (
	"bytes"
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

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	toolkitrun "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkitrun"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/localturn"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/failurelimit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	RemoteToolkitPath = "/api/v2/elitea_core/remote_toolkit_call/prompt_lib/{projectID}/{toolkitID}"
	// RemoteToolkitPermission is the execute permission ADR-0029 decision 5b
	// asks for. It is NOT `models.applications.tool.patch` (what test_tool
	// takes): running a tool an agent may run is a chat-time action, granted
	// to every role that may chat (shared/0156), while patch is an edit of the
	// toolkit. It is a platform string with no pylon `check_api` origin.
	RemoteToolkitPermission = "models.applications.tool.execute"

	// workerRust is runtimecomposition.RustWorkerImplementation, restated so
	// this package does not import the composition root.
	workerRust = "rust"

	remoteToolkitRequiresToken = "remote_toolkit_requires_token"

	// RemoteToolkitCallsPerMinute bounds one caller's remote toolkit calls
	// (per replica, a fixed one-minute window keyed by the owning user). An
	// agent loop calls a tool per step; sixty a minute is far above an
	// interactive loop and far below a scripted flood of provider writes.
	RemoteToolkitCallsPerMinute = 60

	// confirmationMaxAge and confirmationMaxSkew bound approved_at: the
	// approval belongs to the turn (whose deadline is localturn.DeadlineTTL),
	// and a timestamp from the future is a clock that cannot be trusted.
	confirmationMaxAge  = localturn.DeadlineTTL
	confirmationMaxSkew = 5 * time.Minute

	remoteToolkitAuditEntity = "remote_toolkit_call"
)

// RemoteToolAuthorizer is *storage.ClientApplicationVersionService: the 5a
// freeze, reused to decide what one remote call may run.
type RemoteToolAuthorizer interface {
	AuthorizeRemoteTool(context.Context, storage.RemoteToolAuthorization) (storage.RemoteToolGrant, error)
}

var _ RemoteToolAuthorizer = (*storage.ClientApplicationVersionService)(nil)

// LiveTurnReader is *localturn.LiveTurns: the start/commit checks for a turn
// that is still running.
type LiveTurnReader interface {
	Live(ctx context.Context, projectID, actorUserID int64, executionID string) (localturn.LiveTurn, error)
}

var _ LiveTurnReader = (*localturn.LiveTurns)(nil)

// RemoteToolkitDependencies is what executeRemoteToolkitTool runs on. Runs,
// Authorizer and Turns may each be nil, and the route then answers 501: a
// deployment without a toolkit worker or without the agent plane still mounts
// it, so a client does not read a 404 as "server too old".
type RemoteToolkitDependencies struct {
	Runs RemoteToolkitUseCase
	// Worker is the deployment's stated worker ("python" or "rust"); it only
	// words the refusal of a tool the worker cannot run remotely.
	Worker     string
	Authorizer RemoteToolAuthorizer
	Turns      LiveTurnReader
	// Audit receives one event per call that names a tool. Required.
	Audit audit.Recorder
	// Limiter counts calls per caller; nil uses RemoteToolkitCallsPerMinute.
	Limiter *failurelimit.Limiter
}

// RemoteToolkitUseCase is the toolkit.call_tool.v1 producer — the SAME
// *toolkitcalltool.RunService test_tool and the index-start route hold.
type RemoteToolkitUseCase = toolkitrun.UseCase

// NewRemoteToolkitRoute composes POST executeRemoteToolkitTool.
//
// THE AUTHORITY OF A CHAT TURN, AND NO MORE. A remote call runs one tool of one
// saved toolkit on a cloud worker with the toolkit's server-side credentials.
// It is bound to a LIVE local turn the caller started (execution_id: owned by
// the caller, in this project, not committed, not expired, local work
// allowed) and to the agent version that turn runs (or a nested agent
// reachable from it). The server resolves that version with the same freeze a
// cloud turn uses and refuses, before anything runs:
//
//   - a toolkit the version does not hold, or a tool outside its
//     selected_tools: 403 tool_not_in_agent;
//   - a toolkit or tool the guardrails policy blocks: 403 tool_blocked;
//   - a toolkit_ref that is not the one resolveApplicationVersion handed out
//     for this toolkit of this version: 403 toolkit_ref_mismatch;
//   - a sensitive tool without `confirmation`: 409 confirmation_required,
//     carrying the chat HITL interrupt shape, so the desktop prompts the user
//     exactly as a cloud turn would pause.
//
// The worker is told the approval (or its absence) in the runtime context and
// enforces toolkit_security itself as well.
//
// TOKEN CALLERS ONLY. A browser session answers 403
// `remote_toolkit_requires_token`, as a local turn does. The operation exists
// for the desktop's local agent loop; the browser has test_tool under
// `tool.patch`. Serving a cookie here would hand every viewer (the role set
// this permission is granted to) a cookie-authenticated, write-capable POST,
// whose only CSRF defence would be the JSON content type — a weaker footing
// than "no browser credential is accepted at all".
func NewRemoteToolkitRoute(
	deps RemoteToolkitDependencies,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (http.Handler, error) {
	if authConfig.PrincipalValidator == nil || permissions == nil || deps.Audit == nil {
		return nil, ErrInvalidRoute
	}
	h := newRemoteToolkitHandler(deps)
	router := chi.NewRouter()
	router.Method(http.MethodPost, RemoteToolkitPath, gate(authConfig, permissions, RemoteToolkitPermission, h.serve))
	return router, nil
}

func newRemoteToolkitHandler(deps RemoteToolkitDependencies) *remoteToolkitHandler {
	limiter := deps.Limiter
	if limiter == nil {
		limiter = failurelimit.New(RemoteToolkitCallsPerMinute, time.Minute)
	}
	recorder := deps.Audit
	if recorder == nil {
		recorder = discardAudit{}
	}
	return &remoteToolkitHandler{
		useCase: deps.Runs, worker: deps.Worker, authorizer: deps.Authorizer, turns: deps.Turns,
		audit: recorder, limiter: limiter, now: time.Now,
	}
}

type discardAudit struct{}

func (discardAudit) Record(context.Context, audit.Event) {}

type remoteToolkitHandler struct {
	useCase    RemoteToolkitUseCase
	worker     string
	authorizer RemoteToolAuthorizer
	turns      LiveTurnReader
	audit      audit.Recorder
	limiter    *failurelimit.Limiter
	now        func() time.Time
}

type remoteToolkitConfirmation struct {
	Approved   bool   `json:"approved"`
	ApprovedAt string `json:"approved_at"`
}

type remoteToolkitBody struct {
	ExecutionID               string                     `json:"execution_id"`
	ApplicationID             int64                      `json:"application_id"`
	VersionID                 int64                      `json:"version_id"`
	ToolkitRef                string                     `json:"toolkit_ref"`
	ToolName                  string                     `json:"tool_name"`
	Arguments                 json.RawMessage            `json:"arguments"`
	RequestID                 string                     `json:"request_id"`
	MCPAuthorizationReference string                     `json:"mcp_authorization_reference"`
	LLMModel                  string                     `json:"llm_model"`
	LLMSettings               json.RawMessage            `json:"llm_settings"`
	Confirmation              *remoteToolkitConfirmation `json:"confirmation"`
}

// remoteCall is what one call's audit event records.
type remoteCall struct {
	actor, project, toolkit int64
	executionID, toolName   string
	toolkitType             string
	confirmedAt             string
	argumentsSHA256         string
	outcome                 string
}

func (h *remoteToolkitHandler) serve(writer http.ResponseWriter, request *http.Request) {
	started := h.now()
	projectID, okProject := positiveID(chi.URLParam(request, "projectID"))
	toolkitID, okToolkit := positiveID(chi.URLParam(request, "toolkitID"))
	if !okProject || !okToolkit {
		writeError(writer, http.StatusBadRequest, "invalid_remote_toolkit_call", "Invalid project or toolkit id.")
		return
	}
	user, ok := auth.UserFromContext(request.Context())
	if !ok {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	actor, ok := user.OwningUserID()
	if !ok {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if user.TokenID == "" || !strings.EqualFold(user.AuthType, "token") {
		writeError(writer, http.StatusForbidden, remoteToolkitRequiresToken,
			"Remote toolkit calls need a native app token or a personal access token.")
		return
	}
	if h.useCase == nil || h.authorizer == nil || h.turns == nil {
		writeError(writer, http.StatusNotImplemented, "remote_toolkit_unavailable",
			"This deployment runs no cloud worker that can execute toolkit tools.")
		return
	}
	limitKey := "actor:" + strconv.FormatInt(actor, 10)
	if blocked, retry := h.limiter.Blocked(limitKey); blocked {
		writer.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
		writeError(writer, http.StatusTooManyRequests, "rate_limited",
			"Too many remote toolkit calls from this caller. Retry after the interval in Retry-After.")
		return
	}
	h.limiter.Fail(limitKey)

	body, ok := decodeRemoteToolkitBody(writer, request)
	if !ok {
		return
	}
	arguments := body.Arguments
	if len(arguments) == 0 || bytes.Equal(bytes.TrimSpace(arguments), []byte("null")) {
		arguments = json.RawMessage(`{}`)
	}
	call := &remoteCall{
		actor: actor, project: projectID, toolkit: toolkitID,
		executionID: body.ExecutionID, toolName: strings.TrimSpace(body.ToolName),
		argumentsSHA256: argumentsDigest(arguments),
	}
	recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
	writer = recorder
	defer func() { h.record(request.Context(), call, recorder.status, started) }()

	runRequest := toolkitcalltoolapp.RunRequest{
		RequestID:                 body.RequestID,
		IdempotencyKey:            request.Header.Get("Idempotency-Key"),
		ProjectID:                 projectID,
		ActorUserID:               actor,
		ToolkitID:                 toolkitID,
		ToolName:                  call.toolName,
		Arguments:                 arguments,
		LLMModel:                  body.LLMModel,
		LLMSettings:               body.LLMSettings,
		MCPAuthorizationReference: body.MCPAuthorizationReference,
	}
	confirmedAt, confirmationOK := h.validConfirmation(body.Confirmation)
	if err := runRequest.Validate(); err != nil || !localturn.ValidExecutionID(body.ExecutionID) ||
		!positiveInt32(body.ApplicationID) || !positiveInt32(body.VersionID) ||
		!validToolkitRef(body.ToolkitRef) || !confirmationOK {
		call.outcome = "invalid"
		writeError(writer, http.StatusBadRequest, "invalid_remote_toolkit_call",
			"Invalid remote toolkit call: execution_id, application_id, version_id, toolkit_ref and tool_name are "+
				"required, arguments must be one JSON object within the size bound, and a confirmation must be "+
				"approved with an RFC 3339 approved_at.")
		return
	}

	turn, err := h.turns.Live(request.Context(), projectID, actor, body.ExecutionID)
	if err != nil {
		call.outcome = "turn_refused"
		writeLiveTurnError(request.Context(), writer, err)
		return
	}
	grant, err := h.authorizer.AuthorizeRemoteTool(request.Context(), storage.RemoteToolAuthorization{
		ProjectID: projectID, ActorID: actor,
		TurnApplicationID: turn.ApplicationID, TurnVersionID: turn.VersionID,
		ApplicationID: body.ApplicationID, VersionID: body.VersionID,
		ToolkitID: toolkitID, ToolkitRef: body.ToolkitRef, ToolName: call.toolName,
	})
	if err != nil {
		call.outcome = "refused"
		writeAuthorizationError(request.Context(), writer, err)
		return
	}
	call.toolkitType = grant.ToolkitType
	if grant.Sensitive != nil {
		if body.Confirmation == nil {
			call.outcome = "confirmation_required"
			writeConfirmationRequired(writer, toolkitID, call.toolName, body.ExecutionID, call.argumentsSHA256, grant)
			return
		}
		call.confirmedAt = confirmedAt
		runRequest.SensitiveApproval = &toolkitcalltoolapp.SensitiveActionApproval{
			Source: toolkitcalltoolapp.ApprovalSourceUserConfirmation, ApprovedAt: confirmedAt,
		}
	}

	outcome, err := h.useCase.RunTool(request.Context(), runRequest)
	if err != nil {
		call.outcome = "run_error"
		h.writeRunError(request.Context(), writer, toolkitID, err)
		return
	}
	call.outcome = string(outcome.Status)
	h.writeOutcome(writer, toolkitID, outcome)
}

// validConfirmation answers the normalized approved_at of a confirmation, and
// false for a confirmation that is present but not an approval: a desktop that
// asked the user and was refused sends no call at all.
func (h *remoteToolkitHandler) validConfirmation(confirmation *remoteToolkitConfirmation) (string, bool) {
	if confirmation == nil {
		return "", true
	}
	if !confirmation.Approved {
		return "", false
	}
	approvedAt, err := time.Parse(time.RFC3339Nano, confirmation.ApprovedAt)
	if err != nil {
		return "", false
	}
	now := h.now()
	if approvedAt.After(now.Add(confirmationMaxSkew)) || approvedAt.Before(now.Add(-confirmationMaxAge)) {
		return "", false
	}
	return approvedAt.UTC().Format(time.RFC3339Nano), true
}

func positiveInt32(id int64) bool { return id > 0 && id <= math.MaxInt32 }

func validToolkitRef(ref string) bool {
	if len(ref) != len(storage.ClientToolkitRefPrefix)+32 || !strings.HasPrefix(ref, storage.ClientToolkitRefPrefix) {
		return false
	}
	_, err := hex.DecodeString(ref[len(storage.ClientToolkitRefPrefix):])
	return err == nil && strings.ToLower(ref) == ref
}

func argumentsDigest(arguments json.RawMessage) string {
	var compacted bytes.Buffer
	if json.Compact(&compacted, arguments) != nil {
		compacted.Reset()
		compacted.Write(arguments)
	}
	digest := sha256.Sum256(compacted.Bytes())
	return hex.EncodeToString(digest[:])
}

// writeConfirmationRequired answers a sensitive call that carries no
// confirmation, in the chat HITL interrupt shape (ClientFrameHitlInterruptDetail,
// guardrail_type `sensitive_tool`, as the SDK's sensitive tool guard raises it)
// so the desktop can put the same question to the user through its
// ApprovalChannel and repeat the call with `confirmation`. The arguments are
// not echoed: the desktop holds them, and they are caller content.
func writeConfirmationRequired(writer http.ResponseWriter, toolkitID int64, toolName, executionID, argumentsSHA256 string, grant storage.RemoteToolGrant) {
	identity := sha256.Sum256([]byte(executionID + "\x00" + strconv.FormatInt(toolkitID, 10) + "\x00" +
		grant.Sensitive.ActionLabel + "\x00" + argumentsSHA256))
	interrupt := map[string]any{
		"interrupt_id":      "hitl_" + hex.EncodeToString(identity[:16]),
		"tool_call_id":      nil,
		"guardrail_type":    "sensitive_tool",
		"message":           grant.Sensitive.PolicyMessage,
		"policy_message":    grant.Sensitive.PolicyMessage,
		"action_label":      grant.Sensitive.ActionLabel,
		"available_actions": []string{"approve", "reject"},
		"tool_name":         toolName,
		"toolkit_name":      grant.ToolkitName,
		"toolkit_type":      grant.ToolkitType,
		"tool_args":         nil,
	}
	writeJSON(writer, http.StatusConflict, map[string]any{
		"ok": false, "error": "confirmation_required", "toolkit_id": toolkitID,
		"message": grant.Sensitive.PolicyMessage, "hitl_interrupt": interrupt,
	})
}

func writeLiveTurnError(ctx context.Context, writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, localturn.ErrLocalWorkDisabled):
		writeError(writer, http.StatusForbidden, "local_work_disabled",
			"Local work is turned off for this deployment (native client policy local_work.allowed).")
	case errors.Is(err, localturn.ErrNotFound), errors.Is(err, localturn.ErrInvalid):
		writeError(writer, http.StatusNotFound, "local_turn_not_found",
			"The local turn was not found for this caller in this project.")
	case errors.Is(err, localturn.ErrAlreadyCommitted):
		writeError(writer, http.StatusConflict, "local_turn_already_committed",
			"This local turn is already committed; a remote toolkit call needs a running turn.")
	case errors.Is(err, localturn.ErrExpired):
		writeError(writer, http.StatusGone, "local_turn_expired", "This local turn passed its deadline.")
	default:
		if !errors.Is(err, localturn.ErrUnavailable) {
			slog.ErrorContext(ctx, "remote toolkit call: local turn check failed", "err", err)
		}
		writer.Header().Set("Retry-After", "1")
		writeError(writer, http.StatusServiceUnavailable, "remote_toolkit_unavailable_now",
			"The local turn cannot be checked right now. Retry.")
	}
}

func writeAuthorizationError(ctx context.Context, writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrRemoteToolNotInAgent):
		writeError(writer, http.StatusForbidden, "tool_not_in_agent",
			"This tool is not one the running agent may call: its toolkit is not attached to the version, "+
				"the tool is not selected, or the version is not the turn's agent or one it nests.")
	case errors.Is(err, storage.ErrRemoteToolBlocked):
		writeError(writer, http.StatusForbidden, "tool_blocked", "This toolkit or tool is blocked by the platform guardrails.")
	case errors.Is(err, storage.ErrRemoteToolkitRefMismatch):
		writeError(writer, http.StatusForbidden, "toolkit_ref_mismatch",
			"toolkit_ref is not the reference resolveApplicationVersion gives this toolkit of this version.")
	case errors.Is(err, storage.ErrClientApplicationVersionUnresolvable):
		writeError(writer, http.StatusUnprocessableEntity, "application_version_unresolvable",
			"This version cannot be resolved for execution: a cloud run of it would be refused too.")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(writer, http.StatusServiceUnavailable, "remote_toolkit_unavailable_now", "The call was cancelled.")
	default:
		if !errors.Is(err, storage.ErrContentUnavailable) {
			slog.ErrorContext(ctx, "remote toolkit call: authorization failed", "err", err)
		}
		writer.Header().Set("Retry-After", "1")
		writeError(writer, http.StatusServiceUnavailable, "remote_toolkit_unavailable_now",
			"The agent version cannot be resolved right now. Retry.")
	}
}

// record writes the call's audit event: who, where, which tool of which
// toolkit, under which turn, with which confirmation, and how it ended. The
// arguments are recorded as a SHA-256 of their compact JSON, never as content.
func (h *remoteToolkitHandler) record(ctx context.Context, call *remoteCall, status int, started time.Time) {
	if call == nil {
		return
	}
	statusCode := int32(status)
	duration := float64(h.now().Sub(started).Microseconds()) / 1000
	confirmation := "none"
	if call.confirmedAt != "" {
		confirmation = "approved_at " + call.confirmedAt
	}
	outcome := call.outcome
	if outcome == "" {
		outcome = "unknown"
	}
	tool := call.toolName
	if call.toolkitType != "" {
		tool = call.toolkitType + "." + call.toolName
	}
	action := "remote toolkit call " + strconv.Quote(tool) + ": " + outcome +
		"; execution " + call.executionID + "; confirmation " + confirmation +
		"; arguments sha256 " + call.argumentsSHA256
	if runes := []rune(action); len(runes) > 512 {
		action = string(runes[:511]) + "…"
	}
	if !utf8.ValidString(action) {
		action = strings.ToValidUTF8(action, "")
	}
	toolkitID := call.toolkit
	h.audit.Record(context.WithoutCancel(ctx), audit.Event{
		Timestamp:  h.now().UTC(),
		UserID:     audit.ID(call.actor),
		ProjectID:  audit.ID(call.project),
		EventType:  "agent",
		Action:     action,
		HTTPMethod: http.MethodPost,
		HTTPRoute:  RemoteToolkitPath,
		StatusCode: &statusCode,
		DurationMS: &duration,
		IsError:    status >= http.StatusBadRequest,
		EntityType: remoteToolkitAuditEntity,
		EntityID:   &toolkitID,
		EntityName: call.toolName,
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.written {
		r.status, r.written = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(body []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(body)
}

func decodeRemoteToolkitBody(writer http.ResponseWriter, request *http.Request) (remoteToolkitBody, bool) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return remoteToolkitBody{}, false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, toolkitrun.MaxRequestBodyBytes)
	decoder := json.NewDecoder(request.Body)
	var body remoteToolkitBody
	if err := decoder.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large")
			return remoteToolkitBody{}, false
		}
		writeError(writer, http.StatusBadRequest, "invalid_remote_toolkit_call", "Invalid remote toolkit call.")
		return remoteToolkitBody{}, false
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "invalid_remote_toolkit_call", "Invalid remote toolkit call.")
		return remoteToolkitBody{}, false
	}
	return body, true
}

func (h *remoteToolkitHandler) writeOutcome(writer http.ResponseWriter, toolkitID int64, outcome toolkitcalltoolapp.RunOutcome) {
	body := map[string]any{
		"ok":         outcome.Status == toolkitcalltoolapp.RunStatusOK,
		"task_id":    outcome.ExecutionID,
		"toolkit_id": toolkitID,
		"tool_name":  outcome.ToolName,
	}
	if outcome.ToolkitType != "" {
		body["toolkit_type"] = outcome.ToolkitType
	}
	switch outcome.Status {
	case toolkitcalltoolapp.RunStatusOK:
		body["status"] = "ok"
		if outcome.Truncated {
			body["truncated"] = true
		} else if raw := json.RawMessage(outcome.ResultJSON); len(raw) > 0 && json.Valid(raw) {
			body["result"] = raw
		} else {
			body["result"] = outcome.ResultJSON
		}
		writeJSON(writer, http.StatusOK, body)
	case toolkitcalltoolapp.RunStatusToolError:
		// The run reached the tool and the tool raised: the answer the agent
		// loop needs, not a transport failure.
		body["status"] = "tool_error"
		body["error"] = outcome.ErrorMessage
		writeJSON(writer, http.StatusOK, body)
	case toolkitcalltoolapp.RunStatusAuthorizationRequired:
		body["error"] = "mcp_authorization_required"
		body["message"] = outcome.ErrorMessage
		body["authorization_required"] = clientAuthorizationRequest(outcome)
		if outcome.AuthorizationRetry != nil {
			body["authorization_retry"] = outcome.AuthorizationRetry
		}
		writeJSON(writer, http.StatusConflict, body)
	case toolkitcalltoolapp.RunStatusUnsupportedToolkit, toolkitcalltoolapp.RunStatusUnknownTool:
		body["error"] = "remote_tool_unsupported"
		body["reason"] = string(outcome.Status)
		body["message"] = outcome.ErrorMessage
		writeJSON(writer, http.StatusUnprocessableEntity, body)
	default:
		switch outcome.FailureCode {
		case "RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY", "RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED":
			body["error"] = "remote_tool_unsupported"
			body["reason"] = "worker_refused"
			body["message"] = h.workerRefusalSentence(outcome.ToolName)
			writeJSON(writer, http.StatusUnprocessableEntity, body)
		default:
			body["error"] = "remote_toolkit_failed"
			body["message"] = outcome.ErrorMessage
			writeJSON(writer, http.StatusBadGateway, body)
		}
	}
}

func (h *remoteToolkitHandler) workerRefusalSentence(toolName string) string {
	if h.worker == workerRust {
		return "This deployment's agent worker runs only read-only, non-sensitive tools in a remote toolkit call, " +
			"and refused " + strconv.Quote(toolName) + "."
	}
	return "This deployment's agent worker refused to run " + strconv.Quote(toolName) + " in a remote toolkit call."
}

// clientAuthorizationRequest renders the challenge in the fields of
// ClientFrameMcpAuthorizationRequest, the shape a chat stream's
// `mcp_authorization_required` frame already carries.
func clientAuthorizationRequest(outcome toolkitcalltoolapp.RunOutcome) map[string]any {
	request := map[string]any{"tool_name": outcome.ToolName}
	if challenge := outcome.AuthorizationRequired; challenge != nil {
		request["toolkit_name"] = challenge.ToolkitName
		request["toolkit_type"] = challenge.ToolkitType
		request["toolkit_id"] = challenge.ToolkitID
		request["server_url"] = challenge.ServerURL
		if challenge.ResourceMetadataURL != "" {
			request["resource_metadata_url"] = challenge.ResourceMetadataURL
		}
		if len(challenge.ResourceMetadata) > 0 && json.Valid(challenge.ResourceMetadata) {
			request["resource_metadata"] = challenge.ResourceMetadata
		}
	}
	return request
}

func (h *remoteToolkitHandler) writeRunError(ctx context.Context, writer http.ResponseWriter, toolkitID int64, err error) {
	var pending *toolkitcalltoolapp.PendingRun
	switch {
	case errors.As(err, &pending):
		writeJSON(writer, http.StatusGatewayTimeout, map[string]any{
			"ok": false, "error": "remote_toolkit_timeout", "task_id": pending.ExecutionID, "toolkit_id": toolkitID,
			"message": "The tool did not finish within the bounded wait. It is still running; " +
				"poll the execution named by task_id for its result.",
		})
	case errors.Is(err, toolkitcalltoolapp.ErrToolkitNotVisible):
		writeError(writer, http.StatusNotFound, "toolkit_not_found", "The toolkit was not found in this project.")
	case errors.Is(err, executionapp.ErrIdempotencyConflict):
		writeError(writer, http.StatusConflict, "idempotency_conflict",
			"This request key already belongs to a different tool input.")
	case errors.Is(err, toolkitcalltoolapp.ErrUnsupportedToolkitType):
		// The pre-admission verdict (the worker capability snapshot): nothing
		// was written and nothing ran.
		writeJSON(writer, http.StatusUnprocessableEntity, map[string]any{
			"ok": false, "error": "remote_tool_unsupported", "reason": "unsupported_toolkit", "toolkit_id": toolkitID,
			"message": toolkitcalltoolapp.UnsupportedToolkitTypeSentence(err),
		})
	case errors.Is(err, toolkitcalltoolapp.ErrInvalidToolRun),
		errors.Is(err, toolkitcalltoolapp.ErrInvalidAuthoritativeToolRunInput):
		writeError(writer, http.StatusBadRequest, "invalid_remote_toolkit_call", "Invalid remote toolkit call.")
	case errors.Is(err, toolkitcalltoolapp.ErrToolkitSettingsResolutionUnavailable),
		errors.Is(err, storage.ErrContentUnavailable):
		writer.Header().Set("Retry-After", "1")
		writeError(writer, http.StatusServiceUnavailable, "remote_toolkit_unavailable_now",
			"The toolkit's settings could not be resolved right now. Retry.")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(writer, http.StatusServiceUnavailable, "remote_toolkit_unavailable_now", "The call was cancelled.")
	default:
		slog.ErrorContext(ctx, "remote toolkit call failed", "err", err)
		writeError(writer, http.StatusInternalServerError, "remote_toolkit_failed", "The remote toolkit call failed.")
	}
}
