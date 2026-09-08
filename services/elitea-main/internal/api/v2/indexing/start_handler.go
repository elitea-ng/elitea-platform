package indexing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	toolkitrun "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkitrun"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	MaxCurrentIndexStartBodyBytes = int64(1 << 20)
)

var defaultCurrentLLMSettings = json.RawMessage(`{"max_tokens":1024,"temperature":0.1}`)

// StartUseCase owns toolkit visibility, authoritative toolkit/model settings,
// secret resolution, immutable input construction and durable admission. The
// HTTP boundary never forwards client-supplied toolkit settings or credentials.
type StartUseCase interface {
	StartIndexData(context.Context, indexingapp.StartRequest) (indexingapp.StartOutcome, error)
}

type StartHandler struct {
	useCase StartUseCase
	// toolRuns owns the SYNCHRONOUS branch of this path (#340).
	//
	// This handler is registered at indexingapi.CurrentIndexStartPath, which is
	// the SAME string as the toolkits router's `test_toolkit_tool` route, and
	// chi resolves this explicit registration before that subrouter's wildcard.
	// So wherever the runtime is composed, THIS is the handler a tool run
	// reaches — which is why the synchronous branch belongs here and not only
	// on the shadowed sibling. The full route decision is recorded in
	// internal/application/toolkitcalltool/doc.go.
	//
	// Nil keeps the two refusals below exactly as they were.
	toolRuns toolkitrun.UseCase
}

func NewStartHandler(useCase StartUseCase) (*StartHandler, error) {
	if useCase == nil {
		return nil, errors.New("index start use case is required")
	}
	return &StartHandler{useCase: useCase}, nil
}

// NewStartHandlerWithToolRuns adds the synchronous tool-run branch. The
// asynchronous index_data half is unchanged.
func NewStartHandlerWithToolRuns(useCase StartUseCase, toolRuns toolkitrun.UseCase) (*StartHandler, error) {
	handler, err := NewStartHandler(useCase)
	if err != nil {
		return nil, err
	}
	if toolRuns == nil {
		return nil, errors.New("tool-run use case is required")
	}
	handler.toolRuns = toolRuns
	return handler, nil
}

// Start serves two branches of one path, selected by `await_response` exactly
// as pylon selects them:
//
//	await_response=false  the asynchronous index_data admission (task id only)
//	anything else         one synchronous tool run, answered with its result
//
// The synchronous branch used to be a validation refusal — "Only asynchronous
// index_data admission is supported" — because nothing in this service could
// run a tool. It is now the tool run, when one is composed; without a use case
// the refusal is unchanged.
func (h *StartHandler) Start(w http.ResponseWriter, r *http.Request) {
	projectID, ok := positiveCanonicalID(chi.URLParam(r, "projectID"))
	if !ok {
		writeValidationError(w, "project_id", "Input should be a valid integer")
		return
	}
	if !strings.EqualFold(r.URL.Query().Get("await_response"), "false") {
		if h.toolRuns != nil {
			h.runTool(w, r, projectID)
			return
		}
		writeValidationError(w, "await_response", "Only asynchronous index_data admission is supported")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}

	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	actorUserID, ok := user.OwningUserID()
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxCurrentIndexStartBodyBytes)
	body, err := decodeCurrentStartBody(r.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeValidationError(w, "body", "Invalid request body")
		return
	}
	if body.ToolName != indexingapp.IndexDataToolName {
		// The asynchronous branch is index_data's alone. A caller that asked
		// for another tool asynchronously is asking for a durable run whose
		// result nothing on this path would ever hand back, which is why this
		// stays a refusal while the synchronous branch above became a run.
		writeValidationError(w, "tool_name", "Input should be 'index_data'")
		return
	}

	toolkitID, err := toolkitReferenceID(body.ToolkitConfig)
	if err != nil {
		writeValidationError(w, "toolkit_config", "A valid toolkit_id is required")
		return
	}
	toolParameters := body.ToolParameters
	if len(toolParameters) == 0 {
		toolParameters = json.RawMessage(`{}`)
	}
	requestedModel, err := requestedLLMModel(body.LLMModel)
	if err != nil {
		writeValidationError(w, "llm_model", "Input should be a valid string")
		return
	}
	requestedSettings, err := requestedLLMSettings(body.LLMSettings)
	if err != nil {
		writeValidationError(w, "llm_settings", "Input should be a valid object")
		return
	}

	request := indexingapp.StartRequest{
		ProjectID:            projectID,
		ActorUserID:          actorUserID,
		ToolkitID:            toolkitID,
		ToolParameters:       toolParameters,
		RequestedLLMModel:    requestedModel,
		RequestedLLMSettings: requestedSettings,
		StreamID:             body.StreamID,
		MessageID:            body.MessageID,
		SIOEvent:             indexingapp.CurrentIndexSIOEvent,
	}
	if err := request.Validate(); err != nil {
		writeValidationError(w, "body", "Invalid index_data request")
		return
	}

	outcome, err := h.useCase.StartIndexData(r.Context(), request.Clone())
	if err != nil {
		h.writeStartError(w, err)
		return
	}
	if !validTaskID(outcome.TaskID) {
		writeError(w, http.StatusInternalServerError, "No response from toolkit tool test")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		TaskID string `json:"task_id"`
	}{TaskID: outcome.TaskID})
}

type currentStartBody struct {
	ToolkitConfig  json.RawMessage `json:"toolkit_config"`
	ToolName       string          `json:"tool_name"`
	ToolParameters json.RawMessage `json:"tool_params"`
	LLMModel       json.RawMessage `json:"llm_model"`
	LLMSettings    json.RawMessage `json:"llm_settings"`
	StreamID       string          `json:"stream_id"`
	MessageID      string          `json:"message_id"`
}

func decodeCurrentStartBody(reader io.Reader) (currentStartBody, error) {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	var body currentStartBody
	if err := decoder.Decode(&body); err != nil {
		return currentStartBody{}, err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return currentStartBody{}, errors.New("multiple JSON values")
		}
		return currentStartBody{}, err
	}
	return body, nil
}

func toolkitReferenceID(raw json.RawMessage) (int64, error) {
	if !jsonObject(raw) {
		return 0, indexingapp.ErrInvalidIndexStart
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return 0, err
	}
	value := fields["toolkit_id"]
	if len(value) == 0 {
		// Some internal callers already use the post-resolution field name.
		value = fields["id"]
	}
	return flexiblePositiveID(value)
}

func flexiblePositiveID(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, indexingapp.ErrInvalidIndexStart
	}
	value := string(bytes.TrimSpace(raw))
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		if err := json.Unmarshal(raw, &value); err != nil {
			return 0, err
		}
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, indexingapp.ErrInvalidIndexStart
	}
	return id, nil
}

func requestedLLMModel(raw json.RawMessage) (*string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		return nil, err
	}
	return &model, nil
}

func requestedLLMSettings(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return defaultCurrentLLMSettings, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		// The current RPC already treats a falsey settings value as an empty
		// mapping while resolving model metadata. Normalize it once here.
		return json.RawMessage(`{}`), nil
	}
	if !jsonObject(raw) {
		return nil, indexingapp.ErrInvalidIndexStart
	}
	return raw, nil
}

func jsonObject(raw []byte) bool {
	if !json.Valid(raw) {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func positiveCanonicalID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0 && strconv.FormatInt(id, 10) == raw
}

func validTaskID(value string) bool {
	return value != "" && len(value) <= indexingapp.MaxClientCorrelationBytes &&
		utf8.ValidString(value) && value == strings.TrimSpace(value) &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func (h *StartHandler) writeStartError(w http.ResponseWriter, err error) {
	var capacity *executionapp.AdmissionCapacityError
	var active *indexingapp.ActiveIndexConflictError
	switch {
	case errors.Is(err, indexingapp.ErrToolkitNotVisible):
		writeError(w, http.StatusNotFound, "Toolkit not found")
	case errors.Is(err, indexingapp.ErrInvalidIndexStart):
		writeError(w, http.StatusBadRequest, "Invalid index_data request")
	case errors.As(err, &active):
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		writeJSON(w, http.StatusConflict, struct {
			Error  string `json:"error"`
			TaskID string `json:"task_id"`
		}{
			Error:  "Indexing is already in progress for this index",
			TaskID: active.TaskID,
		})
	case errors.Is(err, indexingapp.ErrCurrentEmbeddingBindingUnavailable),
		errors.Is(err, indexingapp.ErrCurrentEmbeddingBindingAmbiguous),
		errors.Is(err, indexingapp.ErrInvalidCurrentEmbeddingBinding):
		writeError(w, http.StatusServiceUnavailable, "Embedding model is unavailable")
	case errors.As(err, &capacity):
		retryAfter := boundedRetrySeconds(capacity.RetryAfter())
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
		writeJSON(w, http.StatusServiceUnavailable, struct {
			Error      string `json:"error"`
			Message    string `json:"message"`
			RetryAfter int64  `json:"retry_after"`
		}{
			Error:      "temporarily_unavailable",
			Message:    "The service is busy processing other requests. Please try again in a few seconds.",
			RetryAfter: retryAfter,
		})
	default:
		writeError(w, http.StatusInternalServerError, "Failed to start index_data")
	}
}

func boundedRetrySeconds(delay time.Duration) int64 {
	seconds := int64(delay / time.Second)
	if delay%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		return 1
	}
	if seconds > 300 {
		return 300
	}
	return seconds
}

type validationIssue struct {
	Type string   `json:"type"`
	Loc  []string `json:"loc"`
	Msg  string   `json:"msg"`
}

func writeValidationError(w http.ResponseWriter, field, message string) {
	writeJSON(w, http.StatusBadRequest, struct {
		Error []validationIssue `json:"error"`
	}{Error: []validationIssue{{Type: "value_error", Loc: []string{field}, Msg: message}}})
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// runTool is the synchronous branch. It takes the caller's identity from the
// authenticated context and the toolkit from the body, never the settings — see
// toolkitrun.Body for that rule and what it costs.
func (h *StartHandler) runTool(w http.ResponseWriter, r *http.Request, projectID int64) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	actorUserID, ok := user.OwningUserID()
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, toolkitrun.MaxRequestBodyBytes)
	request, err := toolkitrun.DecodeRequest(r, projectID, actorUserID, 0)
	if err != nil {
		toolkitrun.WriteInvalidRequest(w)
		return
	}
	outcome, err := h.toolRuns.RunTool(r.Context(), request)
	if err != nil {
		toolkitrun.WriteError(w, err)
		return
	}
	toolkitrun.WriteOutcome(w, outcome)
}
