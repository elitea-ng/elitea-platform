package toolkitrun

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
)

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return body
}

func TestWriteOutcomeMapsEveryStatus(t *testing.T) {
	cases := []struct {
		name     string
		outcome  toolkitcalltoolapp.RunOutcome
		wantCode int
		wantOK   bool
		wantKeys map[string]any
	}{
		{
			name: "ok carries the result as JSON",
			outcome: toolkitcalltoolapp.RunOutcome{
				ExecutionID: "exec-1", Status: toolkitcalltoolapp.RunStatusOK,
				ResultJSON: `{"issues":2}`, ToolName: "list_issues", ToolkitType: "github",
			},
			wantCode: http.StatusOK, wantOK: true,
			wantKeys: map[string]any{"task_id": "exec-1", "tool_name": "list_issues", "toolkit_type": "github"},
		},
		{
			// 200, deliberately. The run reached the tool and the tool answered.
			name: "a tool that raised is a completed run",
			outcome: toolkitcalltoolapp.RunOutcome{
				ExecutionID: "exec-2", Status: toolkitcalltoolapp.RunStatusToolError,
				ErrorMessage: "401 from the provider", ToolName: "list_issues",
			},
			wantCode: http.StatusOK, wantOK: false,
			wantKeys: map[string]any{"error": "401 from the provider"},
		},
		{
			name: "an unsupported toolkit is a 422 naming the reason",
			outcome: toolkitcalltoolapp.RunOutcome{
				ExecutionID: "exec-3", Status: toolkitcalltoolapp.RunStatusUnsupportedToolkit,
				ErrorMessage: "the image does not carry slack", ToolName: "post",
			},
			wantCode: http.StatusUnprocessableEntity, wantOK: false,
			wantKeys: map[string]any{"reason": "unsupported_toolkit", "error": "the image does not carry slack"},
		},
		{
			name: "an unknown tool is a 422 naming the reason",
			outcome: toolkitcalltoolapp.RunOutcome{
				ExecutionID: "exec-4", Status: toolkitcalltoolapp.RunStatusUnknownTool,
				ErrorMessage: "no such tool", ToolName: "nope",
			},
			wantCode: http.StatusUnprocessableEntity, wantOK: false,
			wantKeys: map[string]any{"reason": "unknown_tool"},
		},
		{
			name: "a runtime failure is a 500, not a tool answer",
			outcome: toolkitcalltoolapp.RunOutcome{
				ExecutionID: "exec-5", Status: toolkitcalltoolapp.RunStatusRuntimeFailure,
				ErrorMessage: "The runtime operation failed.", ToolName: "list_issues",
			},
			wantCode: http.StatusInternalServerError, wantOK: false,
			wantKeys: map[string]any{"reason": "runtime_failure"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			WriteOutcome(recorder, testCase.outcome)
			if recorder.Code != testCase.wantCode {
				t.Fatalf("status %d, want %d (%s)", recorder.Code, testCase.wantCode, recorder.Body.String())
			}
			body := decodeBody(t, recorder)
			if body["ok"] != testCase.wantOK {
				t.Fatalf("ok is %v, want %v", body["ok"], testCase.wantOK)
			}
			for key, want := range testCase.wantKeys {
				if body[key] != want {
					t.Fatalf("%s is %v, want %v", key, body[key], want)
				}
			}
		})
	}
}

// A truncated result carries NO body. The worker dropped an oversized encoding
// rather than cutting it, and a `result` key here would make that absence read
// as an empty answer.
func TestWriteOutcomeOmitsATruncatedResultBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteOutcome(recorder, toolkitcalltoolapp.RunOutcome{
		ExecutionID: "exec-1", Status: toolkitcalltoolapp.RunStatusOK, Truncated: true,
	})
	body := decodeBody(t, recorder)
	if _, present := body["result"]; present {
		t.Fatalf("a truncated result carried a body: %v", body["result"])
	}
	if body["truncated"] != true {
		t.Fatal("a truncated result did not say so")
	}
}

// The result is relayed as JSON, not as a string holding JSON: a client that
// re-parses a string would be doing this boundary's work.
func TestWriteOutcomeRelaysTheResultAsJSON(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteOutcome(recorder, toolkitcalltoolapp.RunOutcome{
		ExecutionID: "exec-1", Status: toolkitcalltoolapp.RunStatusOK,
		ResultJSON: `{"issues":[{"id":1}]}`,
	})
	body := decodeBody(t, recorder)
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is %T, not an object", body["result"])
	}
	if _, present := result["issues"]; !present {
		t.Fatalf("result lost its content: %v", result)
	}
}

// 504, not pylon's 400. The run is durable and STILL GOING; 400 would say the
// caller's request was wrong, and it was not.
func TestWriteErrorReportsAPendingRunAsAGatewayTimeout(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteError(recorder, &toolkitcalltoolapp.PendingRun{ExecutionID: "exec-9"})
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status %d, want 504", recorder.Code)
	}
	body := decodeBody(t, recorder)
	if body["task_id"] != "exec-9" {
		t.Fatalf("the timeout does not name the execution: %v", body)
	}
	if body["reason"] != "timeout" {
		t.Fatalf("reason is %v", body["reason"])
	}
}

func TestWriteErrorMapsTheRefusals(t *testing.T) {
	cases := map[string]struct {
		err      error
		wantCode int
	}{
		"invisible toolkit":  {toolkitcalltoolapp.ErrToolkitNotVisible, http.StatusNotFound},
		"unrunnable type":    {toolkitcalltoolapp.ErrUnsupportedToolkitType, http.StatusUnprocessableEntity},
		"invalid request":    {toolkitcalltoolapp.ErrInvalidToolRun, http.StatusBadRequest},
		"settings not built": {toolkitcalltoolapp.ErrToolkitSettingsResolutionUnavailable, http.StatusServiceUnavailable},
		"anything else":      {errors.New("boom"), http.StatusInternalServerError},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			WriteError(recorder, testCase.err)
			if recorder.Code != testCase.wantCode {
				t.Fatalf("status %d, want %d", recorder.Code, testCase.wantCode)
			}
			if decodeBody(t, recorder)["ok"] != false {
				t.Fatal("a refusal did not answer ok:false")
			}
		})
	}
}

// An internal fault must never put its cause on the wire.
func TestWriteErrorDoesNotRelayAnInternalCause(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteError(recorder, errors.New("dial tcp 10.0.0.5:5432: connection refused"))
	if strings.Contains(recorder.Body.String(), "10.0.0.5") {
		t.Fatalf("an internal cause reached the wire: %s", recorder.Body.String())
	}
}

// The refusal a deployment with no runtime gives is UNCHANGED, byte for byte,
// from what every deployment answered before this capability existed.
func TestWriteUnavailableIsTheHistoricalRefusal(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteUnavailable(recorder)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", recorder.Code)
	}
	body := decodeBody(t, recorder)
	if body["ok"] != false || body["error"] != "indexer service not available" {
		t.Fatalf("the historical 503 body changed: %v", body)
	}
}

func TestDecodeRequestTakesTheToolkitFromTheRouteFirst(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(
		`{"toolkit_config":{"toolkit_id":19},"tool_name":"list_issues","tool_params":{"repo":"a"}}`))
	decoded, err := DecodeRequest(request, 1, 7, 42)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.ToolkitID != 42 {
		t.Fatalf("toolkit id %d, want the route's 42", decoded.ToolkitID)
	}
	if decoded.ToolName != "list_issues" || string(decoded.Arguments) != `{"repo":"a"}` {
		t.Fatalf("decoded %+v", decoded)
	}
}

func TestDecodeRequestFallsBackToTheBodyToolkitID(t *testing.T) {
	for name, body := range map[string]string{
		"toolkit_id":         `{"toolkit_config":{"toolkit_id":19},"tool_name":"t"}`,
		"id":                 `{"toolkit_config":{"id":19},"tool_name":"t"}`,
		"a quoted toolkit":   `{"toolkit_config":{"toolkit_id":"19"},"tool_name":"t"}`,
		"absent tool_params": `{"toolkit_config":{"toolkit_id":19},"tool_name":"t"}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
			decoded, err := DecodeRequest(request, 1, 7, 0)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if decoded.ToolkitID != 19 {
				t.Fatalf("toolkit id %d", decoded.ToolkitID)
			}
			if string(decoded.Arguments) != `{}` && string(decoded.Arguments) == "" {
				t.Fatalf("arguments %q", decoded.Arguments)
			}
		})
	}
}

// The settings NEVER come from the caller. A body that carries them is decoded
// without them, so the resolver's saved-row settings are the only ones a run
// can use.
func TestDecodeRequestIgnoresCallerSuppliedSettings(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(
		`{"toolkit_config":{"toolkit_id":19,"settings":{"token":"attacker-supplied"}},"tool_name":"t"}`))
	decoded, err := DecodeRequest(request, 1, 7, 0)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Contains(string(decoded.Arguments), "attacker-supplied") {
		t.Fatalf("caller-supplied settings survived decoding: %s", decoded.Arguments)
	}
}

func TestDecodeRequestRefusesABodyItCannotBind(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":               `{`,
		"no toolkit":             `{"tool_name":"t"}`,
		"no tool name":           `{"toolkit_config":{"toolkit_id":19}}`,
		"toolkit id is zero":     `{"toolkit_config":{"toolkit_id":0},"tool_name":"t"}`,
		"arguments are an array": `{"toolkit_config":{"toolkit_id":19},"tool_name":"t","tool_params":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
			if _, err := DecodeRequest(request, 1, 7, 0); err == nil {
				t.Fatal("expected a refusal")
			}
		})
	}
}
