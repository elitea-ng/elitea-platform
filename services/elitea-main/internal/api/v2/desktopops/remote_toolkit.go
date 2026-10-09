package desktopops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	toolkitrun "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkitrun"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
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
)

// RemoteToolkitUseCase is the toolkit.call_tool.v1 producer — the SAME
// *toolkitcalltool.RunService test_tool and the index-start route hold.
type RemoteToolkitUseCase = toolkitrun.UseCase

// NewRemoteToolkitRoute composes POST executeRemoteToolkitTool.
//
// useCase may be nil: a deployment that composes no toolkit worker still
// mounts the route, which then answers 501 `remote_toolkit_unavailable`
// rather than a 404 a client would read as "server too old".
//
// workerImplementation is the deployment's stated worker ("python" or "rust").
// It only words the refusal of a tool the worker cannot run remotely: on the
// Rust worker toolkit.call_tool.v1 admits read-only, non-sensitive tools only
// (services/elitea-worker-rust/src/toolkits/direct_execution.rs,
// execute_toolsets: EffectfulToolUnavailable), and the worker answers that with
// a RUNTIME_FAILURE whose code is UNSUPPORTED_CAPABILITY.
//
// TOKEN CALLERS ONLY. A browser session answers 403
// `remote_toolkit_requires_token`, as a local turn does. The operation exists
// for the desktop's local agent loop; the browser has test_tool under
// `tool.patch`. Serving a cookie here would hand every viewer (the role set
// this permission is granted to) a cookie-authenticated, write-capable POST,
// whose only CSRF defence would be the JSON content type — a weaker footing
// than "no browser credential is accepted at all".
func NewRemoteToolkitRoute(
	useCase RemoteToolkitUseCase,
	workerImplementation string,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (http.Handler, error) {
	if authConfig.PrincipalValidator == nil || permissions == nil {
		return nil, ErrInvalidRoute
	}
	h := &remoteToolkitHandler{useCase: useCase, worker: workerImplementation}
	router := chi.NewRouter()
	router.Method(http.MethodPost, RemoteToolkitPath, gate(authConfig, permissions, RemoteToolkitPermission, h.serve))
	return router, nil
}

type remoteToolkitHandler struct {
	useCase RemoteToolkitUseCase
	worker  string
}

type remoteToolkitBody struct {
	ToolName                  string          `json:"tool_name"`
	Arguments                 json.RawMessage `json:"arguments"`
	RequestID                 string          `json:"request_id"`
	MCPAuthorizationReference string          `json:"mcp_authorization_reference"`
	LLMModel                  string          `json:"llm_model"`
	LLMSettings               json.RawMessage `json:"llm_settings"`
}

func (h *remoteToolkitHandler) serve(writer http.ResponseWriter, request *http.Request) {
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
	if h.useCase == nil {
		writeError(writer, http.StatusNotImplemented, "remote_toolkit_unavailable",
			"This deployment runs no cloud worker that can execute toolkit tools.")
		return
	}
	body, ok := decodeRemoteToolkitBody(writer, request)
	if !ok {
		return
	}
	arguments := body.Arguments
	if len(arguments) == 0 || bytes.Equal(bytes.TrimSpace(arguments), []byte("null")) {
		arguments = json.RawMessage(`{}`)
	}
	runRequest := toolkitcalltoolapp.RunRequest{
		RequestID:                 body.RequestID,
		IdempotencyKey:            request.Header.Get("Idempotency-Key"),
		ProjectID:                 projectID,
		ActorUserID:               actor,
		ToolkitID:                 toolkitID,
		ToolName:                  strings.TrimSpace(body.ToolName),
		Arguments:                 arguments,
		LLMModel:                  body.LLMModel,
		LLMSettings:               body.LLMSettings,
		MCPAuthorizationReference: body.MCPAuthorizationReference,
	}
	if err := runRequest.Validate(); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_remote_toolkit_call",
			"Invalid remote toolkit call: tool_name is required and arguments must be one JSON object within the size bound.")
		return
	}
	outcome, err := h.useCase.RunTool(request.Context(), runRequest)
	if err != nil {
		h.writeRunError(request.Context(), writer, toolkitID, err)
		return
	}
	h.writeOutcome(writer, toolkitID, outcome)
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
