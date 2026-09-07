// Package toolkitrun is the ONE HTTP mapping of a synchronous tool run.
//
// Two handlers own the same product action in different deployments — see the
// route decision in internal/application/toolkitcalltool/doc.go — and both must
// answer identically. A second copy of this mapping is how the two would come
// to disagree about what a tool error looks like, so there is one copy and both
// import it.
//
// # THE RESPONSE, AND WHERE IT DIFFERS FROM PYLON
//
// pylon's `test_toolkit_tool` answers `serialize(result)` at 200 for every
// settled run, `{"error": "Timeout"}` at 400 on expiry, and `{"error": ...}` at
// 500 for anything else. It cannot tell the four conditions apart, because the
// SDK hands it one dict.
//
// This mapping keeps the 200 for a run that reached the tool — including a tool
// that RAISED, which is a completed run and the answer the caller asked for —
// and separates the two refusals that happen before any provider work:
//
//	OK                  200  ok:true  with the result
//	TOOL_ERROR          200  ok:false with the tool's own sentence
//	UNSUPPORTED_TOOLKIT 422  ok:false naming the type this image cannot build
//	UNKNOWN_TOOL        422  ok:false naming the tool
//	runtime failure     500  ok:false with the runtime's safe message
//	bounded wait passed 504  ok:false with the task id, so the caller can poll
//
// 504 rather than pylon's 400: the run is STILL GOING and durable. 400 would
// tell the caller their request was wrong, and it was not.
package toolkitrun

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
)

// UseCase is the seam both handlers hold. A nil one means this deployment
// composes no runtime, and the handler answers the refusal it always did.
type UseCase interface {
	RunTool(context.Context, toolkitcalltoolapp.RunRequest) (toolkitcalltoolapp.RunOutcome, error)
}

// MaxRequestBodyBytes bounds one tool-run body. Arguments are caller content of
// unbounded size, and the entry bound that ultimately refuses them is inside
// the bundle factory — too late for the caller to see why.
const MaxRequestBodyBytes = int64(1 << 20)

// Body is the request shape both handlers decode. It is pylon's
// `TestToolkitToolInputModel` minus the fields this platform refuses to take
// from a caller.
//
// pylon accepts `toolkit_config.settings` from the request and runs the tool
// against them, which is how its UI tests unsaved form values. THIS SERVICE
// TAKES ONLY THE ID. The settings come from the saved row, resolved by the
// project-scoped resolver, because a caller that can supply settings can supply
// credentials — and the same choice was already made for index_data by
// StartUseCase ("The HTTP boundary never forwards client-supplied toolkit
// settings or credentials", indexing/start_handler.go). A tool run is not the
// place to reverse it.
//
// The cost, stated: testing UNSAVED settings is not possible through this
// route. A caller must save the toolkit first.
type Body struct {
	ToolkitConfig json.RawMessage `json:"toolkit_config"`
	ToolName      string          `json:"tool_name"`
	ToolParams    json.RawMessage `json:"tool_params"`
}

var errInvalidBody = errors.New("invalid tool-run request body")

// DecodeRequest turns one HTTP request into a RunRequest. toolkitID is the
// route's own {toolID} where the route carries one and 0 where it does not; a
// route that names the toolkit wins over the body, because the route is what
// the permission gate resolved.
func DecodeRequest(
	r *http.Request,
	projectID, actorUserID, routeToolkitID int64,
) (toolkitcalltoolapp.RunRequest, error) {
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	var body Body
	if err := decoder.Decode(&body); err != nil {
		return toolkitcalltoolapp.RunRequest{}, errInvalidBody
	}
	toolkitID := routeToolkitID
	if toolkitID <= 0 {
		var err error
		if toolkitID, err = toolkitReferenceID(body.ToolkitConfig); err != nil {
			return toolkitcalltoolapp.RunRequest{}, errInvalidBody
		}
	}
	arguments := body.ToolParams
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	request := toolkitcalltoolapp.RunRequest{
		ProjectID:   projectID,
		ActorUserID: actorUserID,
		ToolkitID:   toolkitID,
		ToolName:    strings.TrimSpace(body.ToolName),
		Arguments:   arguments,
	}
	if err := request.Validate(); err != nil {
		return toolkitcalltoolapp.RunRequest{}, errInvalidBody
	}
	return request, nil
}

// WriteInvalidRequest is the answer for a body this boundary could not read.
func WriteInvalidRequest(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"ok": false, "error": "invalid request body",
	})
}

func toolkitReferenceID(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, errInvalidBody
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return 0, errInvalidBody
	}
	value := fields["toolkit_id"]
	if len(value) == 0 {
		// Some internal callers already use the post-resolution field name;
		// indexing/start_handler.go accepts the same pair.
		value = fields["id"]
	}
	if len(value) == 0 {
		return 0, errInvalidBody
	}
	text := strings.TrimSpace(string(value))
	if len(text) >= 2 && text[0] == '"' && text[len(text)-1] == '"' {
		if err := json.Unmarshal(value, &text); err != nil {
			return 0, errInvalidBody
		}
	}
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id <= 0 {
		return 0, errInvalidBody
	}
	return id, nil
}

// WriteOutcome maps one settled run onto the HTTP answer.
func WriteOutcome(w http.ResponseWriter, outcome toolkitcalltoolapp.RunOutcome) {
	body := map[string]any{
		"ok":        outcome.Status == toolkitcalltoolapp.RunStatusOK,
		"task_id":   outcome.ExecutionID,
		"tool_name": outcome.ToolName,
	}
	if outcome.ToolkitType != "" {
		body["toolkit_type"] = outcome.ToolkitType
	}
	switch outcome.Status {
	case toolkitcalltoolapp.RunStatusOK:
		if outcome.Truncated {
			// No body at all, deliberately. The worker dropped an oversized
			// encoding rather than cutting it, because half a JSON document
			// reads as a corrupt result; passing on a `result` key here would
			// undo that by making the absence look like an empty answer.
			body["truncated"] = true
		} else if raw := json.RawMessage(outcome.ResultJSON); json.Valid(raw) && len(raw) > 0 {
			body["result"] = raw
		} else {
			// The worker guarantees canonical JSON, so this is a shape the
			// worker did not promise. It is relayed as a string rather than
			// dropped: the caller asked what the tool returned.
			body["result"] = outcome.ResultJSON
		}
		writeJSON(w, http.StatusOK, body)
	case toolkitcalltoolapp.RunStatusToolError:
		// 200, not 4xx or 5xx. The run REACHED the tool and the tool answered;
		// "it raised, and here is why" is the answer to "does this tool work
		// against my saved settings".
		body["error"] = outcome.ErrorMessage
		writeJSON(w, http.StatusOK, body)
	case toolkitcalltoolapp.RunStatusUnsupportedToolkit:
		body["reason"] = "unsupported_toolkit"
		body["error"] = outcome.ErrorMessage
		writeJSON(w, http.StatusUnprocessableEntity, body)
	case toolkitcalltoolapp.RunStatusUnknownTool:
		body["reason"] = "unknown_tool"
		body["error"] = outcome.ErrorMessage
		writeJSON(w, http.StatusUnprocessableEntity, body)
	default:
		body["reason"] = "runtime_failure"
		body["error"] = outcome.ErrorMessage
		writeJSON(w, http.StatusInternalServerError, body)
	}
}

// WriteError maps a run that never settled. It is the only place a tool-run
// failure becomes a status code, so a caller sees the same code from both
// handlers.
func WriteError(w http.ResponseWriter, err error) {
	var pending *toolkitcalltoolapp.PendingRun
	switch {
	case errors.As(err, &pending):
		// The execution is durable and STILL RUNNING; the request gave up, not
		// the run. Naming the id is what lets a person find or cancel it.
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{
			"ok":      false,
			"task_id": pending.ExecutionID,
			"reason":  "timeout",
			"error": "the tool did not finish within the bounded wait. It is still running; " +
				"poll the execution named by task_id for its result.",
		})
	case errors.Is(err, toolkitcalltoolapp.ErrToolkitNotVisible):
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "error": "toolkit not found",
		})
	case errors.Is(err, toolkitcalltoolapp.ErrUnsupportedToolkitType):
		// Refused BEFORE admission, so there is no task id to report: nothing
		// was written and nothing ran.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"ok": false, "reason": "unsupported_toolkit", "error": err.Error(),
		})
	case errors.Is(err, toolkitcalltoolapp.ErrInvalidToolRun),
		errors.Is(err, toolkitcalltoolapp.ErrInvalidAuthoritativeToolRunInput):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "invalid tool-run request",
		})
	case errors.Is(err, toolkitcalltoolapp.ErrToolkitSettingsResolutionUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "toolkit settings could not be resolved",
		})
	default:
		// The cause is not put on the wire. It is a server fault, not the
		// caller's, and this boundary never relays an internal message.
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "error": "failed to run the toolkit tool",
		})
	}
}

// WriteUnavailable is the answer where no runtime is composed. The body is
// UNCHANGED from what every deployment returned before this capability existed
// (#126, #340): a deployment with no worker genuinely cannot run a tool, and a
// new sentence there would read as a new failure rather than the same absence.
func WriteUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"ok": false, "error": "indexer service not available",
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
