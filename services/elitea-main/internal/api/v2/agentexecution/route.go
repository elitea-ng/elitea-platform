package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"strconv"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentApplicationStartPath              = "/api/v2/elitea_core/messages/prompt_lib/{projectID}/{conversationID}"
	CurrentApplicationStartContract          = "agent.execute.application.v1"
	CurrentAdhocStartContract                = "agent.execute.adhoc.v1"
	CurrentRegenerationPath                  = "/api/v2/elitea_core/regenerate/prompt_lib/{projectID}/{responseMessageID}"
	CurrentRegenerationContract              = "agent.regenerate.v1"
	CurrentContinuationPath                  = "/api/v2/elitea_core/continue_predict/prompt_lib/{projectID}/{conversationID}"
	CurrentContinuationContract              = "agent.continue.hitl.v1"
	CurrentAuthorizationContinuationContract = "agent.continue.authorization.v1"
	CurrentOutputLimitContinuationContract   = "agent.continue.output-limit.v1"
	CurrentApplicationStartMode              = auth.PermissionModeDefault
	CurrentApplicationStartPermission        = "models.chat.messages.create"
	CurrentRegenerationPermission            = "models.chat.conversations.regenerate"
	maxCurrentApplicationStartBody           = int64(512 * 1024)
)

var ErrInvalidCurrentApplicationStartRoute = errors.New("invalid current application-start route dependencies")

type StartUseCase interface {
	StartCurrentApplication(
		context.Context,
		agentexecutionapp.CurrentApplicationStartRequest,
	) (agentexecutionapp.CurrentApplicationStartOutcome, error)
	StartCurrentAdhoc(
		context.Context,
		agentexecutionapp.CurrentAdhocStartRequest,
	) (agentexecutionapp.CurrentApplicationStartOutcome, error)
	RegenerateCurrentAgent(
		context.Context,
		agentexecutionapp.CurrentRegenerationRequest,
	) (agentexecutionapp.CurrentApplicationStartOutcome, error)
	ContinueCurrentAgent(
		context.Context,
		agentexecutionapp.CurrentContinuationRequest,
	) (agentexecutionapp.CurrentApplicationStartOutcome, error)
}

type CurrentApplicationStartRoute struct {
	handler http.Handler
}

func NewCurrentApplicationStartRoute(
	useCase StartUseCase,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentApplicationStartRoute, error) {
	if useCase == nil || authConfig.PrincipalValidator == nil ||
		authConfig.ForwardedIdentityVerifier == nil || permissions == nil {
		return nil, ErrInvalidCurrentApplicationStartRoute
	}
	handler := &currentApplicationStartHandler{useCase: useCase}
	startEndpoint := http.Handler(http.HandlerFunc(handler.Start))
	startEndpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentApplicationStartMode,
		func(request *http.Request) (string, bool) {
			projectID := chi.URLParam(request, "projectID")
			_, valid := positiveCanonicalID(projectID)
			return projectID, valid
		},
		CurrentApplicationStartPermission,
	)(startEndpoint)
	startEndpoint = apimw.Auth(authConfig)(startEndpoint)
	regenerateEndpoint := http.Handler(http.HandlerFunc(handler.Regenerate))
	regenerateEndpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentApplicationStartMode,
		func(request *http.Request) (string, bool) {
			projectID := chi.URLParam(request, "projectID")
			_, valid := positiveCanonicalID(projectID)
			return projectID, valid
		},
		CurrentRegenerationPermission,
	)(regenerateEndpoint)
	regenerateEndpoint = apimw.Auth(authConfig)(regenerateEndpoint)
	continueEndpoint := http.Handler(http.HandlerFunc(handler.Continue))
	continueEndpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentApplicationStartMode,
		func(request *http.Request) (string, bool) {
			projectID := chi.URLParam(request, "projectID")
			_, valid := positiveCanonicalID(projectID)
			return projectID, valid
		},
		CurrentApplicationStartPermission,
	)(continueEndpoint)
	continueEndpoint = apimw.Auth(authConfig)(continueEndpoint)
	router := chi.NewRouter()
	router.Method(http.MethodPost, CurrentApplicationStartPath, startEndpoint)
	router.Method(http.MethodPost, CurrentRegenerationPath, regenerateEndpoint)
	router.Method(http.MethodPost, CurrentContinuationPath, continueEndpoint)
	return &CurrentApplicationStartRoute{handler: router}, nil
}

func (route *CurrentApplicationStartRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentApplicationStartHandler struct {
	useCase StartUseCase
}

// currentStartAttachment is one entry of `payload.attachments`, the shape the
// composer sends after uploading files (apps/elitea-web
// widgets/chat-box/ui/hooks/useChatBoxHandlers.helpers.ts:286-295, put inside
// `payload` by useChatBoxSend.ts:135). `filepath` is exactly what the upload
// endpoint returned. `name` is also sent and is deliberately IGNORED: it is
// the display basename, while the column's `name` must be the object key —
// the filepath's post-bucket remainder, conversation-uuid prefix included —
// or the stored bytes become unaddressable. See
// agentexecutionapp.ParseAttachmentFilepath. #606.
type currentStartAttachment struct {
	Filepath string `json:"filepath"`
	Name     string `json:"name"`
}

type currentApplicationStartBody struct {
	Payload struct {
		UserInput string `json:"user_input"`
		// #606: previously undeclared, so every attachment the client sent
		// was silently dropped by the decoder and no chat_messages_attachment
		// row was ever written. Distinct from AttachmentsInfo below, which is
		// a different, still-unported field kept only as a rejection gate.
		Attachments []currentStartAttachment `json:"attachments"`
	} `json:"payload"`
	ProjectID        int64           `json:"project_id"`
	ParticipantID    int64           `json:"participant_id"`
	ConversationUUID string          `json:"conversation_uuid"`
	QuestionID       string          `json:"question_id"`
	InteractionUUID  string          `json:"interaction_uuid"`
	AttachmentsInfo  json.RawMessage `json:"attachments_info"`
	LLMSettings      json.RawMessage `json:"llm_settings"`
	MCPTokens        json.RawMessage `json:"mcp_tokens"`
	UserIDs          json.RawMessage `json:"user_ids"`
}

type currentRegenerationBody struct {
	Payload struct {
		UserInput       string          `json:"user_input"`
		LLMSettings     json.RawMessage `json:"llm_settings"`
		AttachmentsInfo json.RawMessage `json:"attachments_info"`
		MCPTokens       json.RawMessage `json:"mcp_tokens"`
		UserIDs         json.RawMessage `json:"user_ids"`
	} `json:"payload"`
	ProjectID        int64           `json:"project_id"`
	ParticipantID    int64           `json:"participant_id"`
	ConversationUUID string          `json:"conversation_uuid"`
	QuestionID       string          `json:"question_id"`
	MessageID        string          `json:"message_id"`
	StreamID         string          `json:"stream_id"`
	RegenerationID   string          `json:"regeneration_id"`
	UpdatedItems     json.RawMessage `json:"updated_items"`
}

type currentContinuationBody struct {
	ProjectID              int64           `json:"project_id"`
	ConversationUUID       string          `json:"conversation_uuid"`
	MessageID              string          `json:"message_id"`
	ThreadID               string          `json:"thread_id"`
	HITLResume             bool            `json:"hitl_resume"`
	HITLAction             string          `json:"hitl_action"`
	HITLValue              json.RawMessage `json:"hitl_value"`
	HITLDecisions          json.RawMessage `json:"hitl_decisions"`
	MCPTokens              json.RawMessage `json:"mcp_tokens"`
	IgnoredMCPServers      json.RawMessage `json:"ignored_mcp_servers"`
	UserDeclinedMCPServers json.RawMessage `json:"user_declined_mcp_servers"`
	UserInput              string          `json:"user_input"`
	AuthorizationRequestID string          `json:"authorization_request_id"`
	AuthorizationAction    string          `json:"authorization_action"`
}

func (handler *currentApplicationStartHandler) Start(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := positiveCanonicalID(chi.URLParam(request, "projectID"))
	contract := request.URL.Query().Get("execution_contract")
	if !ok || (contract != CurrentApplicationStartContract && contract != CurrentAdhocStartContract) {
		writeError(writer, http.StatusBadRequest, "Invalid agent execution request")
		return
	}
	conversationID := chi.URLParam(request, "conversationID")
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	user, ok := auth.UserFromContext(request.Context())
	if !ok {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	actorUserID, ok := user.OwningUserID()
	if !ok {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxCurrentApplicationStartBody)
	var body currentApplicationStartBody
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&body); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeError(writer, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(writer, http.StatusBadRequest, "Invalid agent execution request")
		return
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "Invalid agent execution request")
		return
	}
	if body.ProjectID != projectID || body.ConversationUUID != conversationID ||
		body.Payload.UserInput == "" || !emptyJSONArray(body.AttachmentsInfo) ||
		(!absentJSON(body.MCPTokens) && !currentJSONObject(body.MCPTokens)) || !absentJSON(body.UserIDs) {
		writeUnsupported(writer)
		return
	}
	// A filepath that does not split into a non-empty bucket and a non-empty
	// name is malformed input, not an unsupported feature, so it is a 400 and
	// not the 422 the parity gates above return. Refusing the whole start
	// rather than dropping the offending entry is deliberate: a silently
	// dropped attachment produces an admitted turn the user believes carried
	// their file.
	attachments, ok := parseStartAttachments(body.Payload.Attachments)
	if !ok {
		writeError(writer, http.StatusBadRequest, "Invalid agent execution request")
		return
	}

	var outcome agentexecutionapp.CurrentApplicationStartOutcome
	switch contract {
	case CurrentApplicationStartContract:
		if body.ParticipantID <= 0 || !absentJSON(body.LLMSettings) {
			writeUnsupported(writer)
			return
		}
		outcome, err = handler.useCase.StartCurrentApplication(
			request.Context(),
			agentexecutionapp.CurrentApplicationStartRequest{
				MCPTokens: bytes.Clone(body.MCPTokens),
				ProjectID: projectID, ActorUserID: actorUserID,
				ConversationUUID: conversationID, TargetParticipantID: body.ParticipantID,
				QuestionID: body.QuestionID, UserInput: body.Payload.UserInput,
				InteractionUUID: body.InteractionUUID,
				Attachments:     attachments,
			},
		)
	case CurrentAdhocStartContract:
		if body.ParticipantID < 0 || !currentJSONObject(body.LLMSettings) {
			writeUnsupported(writer)
			return
		}
		outcome, err = handler.useCase.StartCurrentAdhoc(
			request.Context(),
			agentexecutionapp.CurrentAdhocStartRequest{
				MCPTokens: bytes.Clone(body.MCPTokens),
				ProjectID: projectID, ActorUserID: actorUserID,
				ConversationUUID: conversationID, TargetParticipantID: body.ParticipantID,
				QuestionID: body.QuestionID, UserInput: body.Payload.UserInput,
				InteractionUUID: body.InteractionUUID,
				LLMSettings:     bytes.Clone(body.LLMSettings),
				Attachments:     attachments,
			},
		)
	}
	if err != nil {
		slog.Error("current agent execution start failed", "contract", contract, "err", err)
		writeStartError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"task_id":             outcome.ExecutionID,
		"execution_id":        outcome.ExecutionID,
		"command_id":          outcome.CommandID,
		"response_message_id": outcome.ResponseMessageID,
		"events_url":          "/api/v2/executions/" + strconv.FormatInt(projectID, 10) + "/" + outcome.ExecutionID + "/events",
		"created":             outcome.Created,
	})
}

func (handler *currentApplicationStartHandler) Regenerate(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := positiveCanonicalID(chi.URLParam(request, "projectID"))
	responseMessageID := chi.URLParam(request, "responseMessageID")
	if !ok || request.URL.Query().Get("execution_contract") != CurrentRegenerationContract {
		writeError(writer, http.StatusBadRequest, "Invalid agent regeneration request")
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	user, ok := auth.UserFromContext(request.Context())
	if !ok {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	actorUserID, ok := user.OwningUserID()
	if !ok {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxCurrentApplicationStartBody)
	var body currentRegenerationBody
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&body); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeError(writer, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(writer, http.StatusBadRequest, "Invalid agent regeneration request")
		return
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "Invalid agent regeneration request")
		return
	}
	if body.ProjectID != projectID || body.ParticipantID < 0 ||
		body.MessageID != responseMessageID || body.StreamID != responseMessageID ||
		body.Payload.UserInput == "" || !emptyJSONArray(body.UpdatedItems) ||
		!emptyJSONArray(body.Payload.AttachmentsInfo) ||
		(!absentJSON(body.Payload.MCPTokens) && !currentJSONObject(body.Payload.MCPTokens)) || !absentJSON(body.Payload.UserIDs) ||
		(!absentJSON(body.Payload.LLMSettings) && !currentJSONObject(body.Payload.LLMSettings)) {
		writeUnsupported(writer)
		return
	}
	llmSettings := body.Payload.LLMSettings
	if absentJSON(llmSettings) {
		llmSettings = json.RawMessage(`{}`)
	}
	outcome, err := handler.useCase.RegenerateCurrentAgent(
		request.Context(),
		agentexecutionapp.CurrentRegenerationRequest{
			MCPTokens: bytes.Clone(body.Payload.MCPTokens),
			ProjectID: projectID, ActorUserID: actorUserID,
			ConversationUUID: body.ConversationUUID, QuestionID: body.QuestionID,
			ResponseMessageID: responseMessageID, RegenerationID: body.RegenerationID,
			RequestedParticipantID: body.ParticipantID,
			LLMSettings:            bytes.Clone(llmSettings),
		},
	)
	if err != nil {
		slog.Error(
			"current agent regeneration failed",
			"response_message_id", responseMessageID,
			"err", err,
		)
		writeStartError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"task_id": outcome.ExecutionID, "execution_id": outcome.ExecutionID,
		"command_id": outcome.CommandID, "response_message_id": outcome.ResponseMessageID,
		"events_url": "/api/v2/executions/" + strconv.FormatInt(projectID, 10) + "/" + outcome.ExecutionID + "/events",
		"created":    outcome.Created,
	})
}

func (handler *currentApplicationStartHandler) Continue(writer http.ResponseWriter, request *http.Request) {
	projectID, ok := positiveCanonicalID(chi.URLParam(request, "projectID"))
	conversationID := chi.URLParam(request, "conversationID")
	contract := request.URL.Query().Get("execution_contract")
	if !ok || (contract != CurrentContinuationContract &&
		contract != CurrentAuthorizationContinuationContract &&
		contract != CurrentOutputLimitContinuationContract) {
		writeError(writer, http.StatusBadRequest, "Invalid agent continuation request")
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	user, ok := auth.UserFromContext(request.Context())
	if !ok {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	actorUserID, ok := user.OwningUserID()
	if !ok {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxCurrentApplicationStartBody)
	var body currentContinuationBody
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&body); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeError(writer, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(writer, http.StatusBadRequest, "Invalid agent continuation request")
		return
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "Invalid agent continuation request")
		return
	}
	if body.ProjectID != projectID || body.ConversationUUID != conversationID || body.MessageID == "" {
		writeUnsupported(writer)
		return
	}
	continuation := agentexecutionapp.CurrentContinuationRequest{
		ProjectID: projectID, ActorUserID: actorUserID,
		ConversationUUID: conversationID, ResponseMessageID: body.MessageID,
		ThreadID: body.ThreadID,
	}
	switch contract {
	case CurrentOutputLimitContinuationContract:
		if body.ThreadID != "" || body.HITLResume || body.HITLAction != "" ||
			!absentJSON(body.HITLValue) || !absentJSON(body.HITLDecisions) ||
			!absentJSON(body.MCPTokens) || !absentJSON(body.IgnoredMCPServers) ||
			!absentJSON(body.UserDeclinedMCPServers) || body.UserInput != "" ||
			body.AuthorizationRequestID != "" || body.AuthorizationAction != "" {
			writeUnsupported(writer)
			return
		}
		continuation.Kind = agentexecutionapp.CurrentContinuationOutputLimit
	case CurrentContinuationContract:
		decisions, decisionsValid := currentHITLDecisions(body.HITLDecisions)
		if !body.HITLResume || !decisionsValid || !emptyJSONObject(body.MCPTokens) ||
			!emptyJSONArray(body.IgnoredMCPServers) || !emptyJSONArray(body.UserDeclinedMCPServers) ||
			body.AuthorizationRequestID != "" || body.AuthorizationAction != "" {
			writeUnsupported(writer)
			return
		}
		if len(decisions) == 0 {
			value, valid := currentHITLValue(body.HITLAction, body.HITLValue)
			if !currentRootHITLAction(body.HITLAction) || !valid {
				writeUnsupported(writer)
				return
			}
			continuation.Action = body.HITLAction
			continuation.Value = value
		} else {
			if body.HITLAction != "" || !absentJSON(body.HITLValue) {
				writeUnsupported(writer)
				return
			}
			continuation.HITLDecisions = decisions
		}
		continuation.Kind = agentexecutionapp.CurrentContinuationHITL
	case CurrentAuthorizationContinuationContract:
		decisions, decisionsValid := currentAuthorizationDecisions(body.HITLDecisions)
		if body.HITLAction != "" || !absentJSON(body.HITLValue) || !decisionsValid ||
			!currentJSONObject(body.MCPTokens) ||
			!currentJSONArray(body.IgnoredMCPServers) || !currentJSONArray(body.UserDeclinedMCPServers) ||
			(len(decisions) == 0 && (body.HITLResume || body.AuthorizationRequestID == "" ||
				(body.AuthorizationAction != "authorize" && body.AuthorizationAction != "skip"))) ||
			(len(decisions) != 0 && (!body.HITLResume || body.AuthorizationRequestID != "" ||
				body.AuthorizationAction != "")) {
			writeUnsupported(writer)
			return
		}
		continuation.Kind = agentexecutionapp.CurrentContinuationAuthorization
		if len(decisions) == 0 {
			continuation.AuthorizationID = body.AuthorizationRequestID
			continuation.Action = body.AuthorizationAction
		} else {
			continuation.HITLDecisions = decisions
		}
		continuation.MCPTokens = bytes.Clone(body.MCPTokens)
		continuation.IgnoredMCPServers = bytes.Clone(body.IgnoredMCPServers)
		continuation.DeclinedMCPServers = bytes.Clone(body.UserDeclinedMCPServers)
	}
	outcome, err := handler.useCase.ContinueCurrentAgent(
		request.Context(),
		continuation,
	)
	if err != nil {
		slog.Error("current agent continuation failed", "response_message_id", body.MessageID, "err", err)
		writeStartError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"task_id": outcome.ExecutionID, "execution_id": outcome.ExecutionID,
		"command_id": outcome.CommandID, "response_message_id": outcome.ResponseMessageID,
		"events_url": "/api/v2/executions/" + strconv.FormatInt(projectID, 10) + "/" + outcome.ExecutionID + "/events",
		"created":    outcome.Created,
	})
}

func writeStartError(writer http.ResponseWriter, err error) {
	var capacity *executionapp.AdmissionCapacityError
	switch {
	case errors.Is(err, agentexecutionapp.ErrInvalidCurrentAgentStart),
		errors.Is(err, agentexecutionapp.ErrInvalidAgentAdmission):
		writeError(writer, http.StatusBadRequest, "Invalid agent execution request")
	case errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart):
		writeUnsupported(writer)
	case errors.Is(err, agentexecutionapp.ErrCurrentAgentRegenerationStillFinalizing):
		writer.Header().Set("Retry-After", "1")
		writeJSON(writer, http.StatusConflict, map[string]any{
			"error":     "agent_regeneration_pending",
			"message":   "The previous agent response is still being finalized. Please retry shortly.",
			"retryable": true,
		})
	case errors.Is(err, agentexecutionapp.ErrCurrentAgentHITLAlreadyResolved):
		writeJSON(writer, http.StatusConflict, map[string]any{
			"error":     "agent_hitl_already_resolved",
			"message":   "This agent approval was already resolved. Refresh the conversation before retrying.",
			"retryable": false,
		})
	case errors.Is(err, agentexecutionapp.ErrCurrentAgentAuthorizationAlreadyResolved):
		writeJSON(writer, http.StatusConflict, map[string]any{
			"error":     "agent_authorization_already_resolved",
			"message":   "This toolkit authorization request was already resolved. Refresh the conversation before retrying.",
			"retryable": false,
		})
	case errors.Is(err, agentexecutionapp.ErrCurrentAgentOutputLimitAlreadyResolved):
		writeJSON(writer, http.StatusConflict, map[string]any{
			"error":     "agent_output_limit_already_resolved",
			"message":   "This output continuation was already started. Refresh the conversation before retrying.",
			"retryable": false,
		})
	case errors.Is(err, executionapp.ErrIdempotencyConflict):
		writeError(writer, http.StatusConflict, "Agent execution request conflicts with an existing turn")
	case errors.As(err, &capacity):
		writer.Header().Set("Retry-After", "1")
		writeError(writer, http.StatusServiceUnavailable, "The service is busy processing other requests. Please try again in a few seconds.")
	default:
		writeError(writer, http.StatusInternalServerError, "Failed to start agent execution")
	}
}

func writeUnsupported(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusUnprocessableEntity, map[string]string{
		"error":   "unsupported_agent_execution",
		"message": "This agent turn requires the current execution path.",
	})
}

func emptyJSONArray(raw json.RawMessage) bool {
	return absentJSON(raw) || bytes.Equal(bytes.TrimSpace(raw), []byte("[]"))
}

func emptyJSONObject(raw json.RawMessage) bool {
	return absentJSON(raw) || bytes.Equal(bytes.TrimSpace(raw), []byte("{}"))
}

func absentJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func currentJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return json.Valid(trimmed) && len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func currentJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return json.Valid(trimmed) && len(trimmed) >= 2 && trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']'
}

func currentRootHITLAction(action string) bool {
	switch action {
	case "approve", "reject", "edit", "block_with_comment", "answer":
		return true
	default:
		return false
	}
}

func currentHITLStringValue(raw json.RawMessage) (string, bool) {
	if absentJSON(raw) {
		return "", true
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func currentHITLValue(action string, raw json.RawMessage) (string, bool) {
	if action != "answer" {
		return currentHITLStringValue(raw)
	}
	trimmed := bytes.TrimSpace(raw)
	if !json.Valid(trimmed) || len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) ||
		(trimmed[0] != '{' && trimmed[0] != '"') {
		return "", false
	}
	var decoded any
	if json.Unmarshal(trimmed, &decoded) != nil {
		return "", false
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return "", false
	}
	return string(canonical), true
}

func currentHITLDecisions(raw json.RawMessage) ([]agentexecutionapp.CurrentHITLDecision, bool) {
	if emptyJSONArray(raw) {
		return nil, true
	}
	var objects []map[string]json.RawMessage
	if json.Unmarshal(raw, &objects) != nil || len(objects) == 0 || len(objects) > 16 {
		return nil, false
	}
	decisions := make([]agentexecutionapp.CurrentHITLDecision, 0, len(objects))
	for _, object := range objects {
		for key := range object {
			switch key {
			case "interrupt_id", "tool_call_id", "action", "value":
			default:
				return nil, false
			}
		}
		var decision agentexecutionapp.CurrentHITLDecision
		if json.Unmarshal(object["interrupt_id"], &decision.InterruptID) != nil ||
			json.Unmarshal(object["action"], &decision.Action) != nil {
			return nil, false
		}
		if value, exists := object["tool_call_id"]; exists && json.Unmarshal(value, &decision.ToolCallID) != nil {
			return nil, false
		}
		if value, exists := object["value"]; exists {
			var valid bool
			decision.Value, valid = currentHITLValue(decision.Action, value)
			if !valid {
				return nil, false
			}
		}
		decisions = append(decisions, decision)
	}
	return decisions, true
}

func currentAuthorizationDecisions(raw json.RawMessage) ([]agentexecutionapp.CurrentHITLDecision, bool) {
	if emptyJSONArray(raw) {
		return nil, true
	}
	var objects []map[string]json.RawMessage
	if json.Unmarshal(raw, &objects) != nil || len(objects) == 0 || len(objects) > 16 {
		return nil, false
	}
	decisions := make([]agentexecutionapp.CurrentHITLDecision, 0, len(objects))
	for _, object := range objects {
		for key := range object {
			switch key {
			case "interrupt_id", "tool_call_id", "guardrail_type", "action":
			default:
				return nil, false
			}
		}
		var decision agentexecutionapp.CurrentHITLDecision
		if json.Unmarshal(object["interrupt_id"], &decision.InterruptID) != nil ||
			json.Unmarshal(object["guardrail_type"], &decision.GuardrailType) != nil ||
			json.Unmarshal(object["action"], &decision.Action) != nil {
			return nil, false
		}
		if value, exists := object["tool_call_id"]; exists && json.Unmarshal(value, &decision.ToolCallID) != nil {
			return nil, false
		}
		decisions = append(decisions, decision)
	}
	return decisions, true
}

// positiveCanonicalID parses a path/body id and rejects anything that cannot
// address a real row.
//
// The upper bound is not cosmetic and not merely a CodeQL appeasement. Every id
// parsed here is eventually narrowed to int32 for sqlc (the underlying columns
// are Postgres `integer` — see sqlcgen's ProjectID/ActorUserID/ApplicationID
// int32 fields), and in Go that narrowing is a silent truncation. Without this
// bound, `4294967301` truncates to `5`, so a caller could address a DIFFERENT,
// VALID row by sending an out-of-range id — an aliasing bug, not just a lossy
// conversion. CodeQL flagged the five int32() sites downstream
// (go/incorrect-integer-conversion, alerts 74-78); fixing it here fixes the
// cause rather than each symptom, because this is the only ParseInt in the
// agentexecution request path.
//
// Rejecting is correct rather than clamping: an id above MaxInt32 cannot
// correspond to any row in an `integer` column, so the honest answer to the
// caller is "no such id", which is what the false return produces.
//
// The FormatInt round-trip additionally rejects non-canonical spellings
// ("007", "+5"), so two different strings can never denote the same entity.
func positiveCanonicalID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0 && id <= math.MaxInt32 && strconv.FormatInt(id, 10) == raw
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

// parseStartAttachments splits every `payload.attachments` filepath into the
// (bucket, name) pair the item rows are keyed by, rejecting the whole request
// if any one of them is malformed. The split itself is pylon's, and lives in
// the application package so the HTTP layer never decides what a column holds.
func parseStartAttachments(
	entries []currentStartAttachment,
) ([]agentexecutionapp.CurrentTurnAttachmentRef, bool) {
	if len(entries) == 0 {
		return nil, true
	}
	refs := make([]agentexecutionapp.CurrentTurnAttachmentRef, 0, len(entries))
	for _, entry := range entries {
		ref, ok := agentexecutionapp.ParseAttachmentFilepath(entry.Filepath)
		if !ok {
			return nil, false
		}
		refs = append(refs, ref)
	}
	return refs, true
}
