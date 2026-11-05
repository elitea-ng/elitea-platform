package predict

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

func boolRef(value bool) *bool       { return &value }
func stringRef(value string) *string { return &value }

func encodedBody(t *testing.T, req CompletionRequest) map[string]any {
	t.Helper()
	raw, err := encodeChatCompletions(req, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestEncodeChatCompletionsTranslatesTheSettingWithTheModelsForm(t *testing.T) {
	temp := 0.4
	base := CompletionRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, Temperature: &temp}

	toggle := base
	toggle.ReasoningEffort, toggle.ReasoningControl = "auto", configurationapp.ReasoningControlToggle
	body := encodedBody(t, toggle)
	if _, present := body["reasoning_effort"]; present {
		t.Errorf("toggle sent reasoning_effort: %v", body)
	}
	if kwargs, _ := body["chat_template_kwargs"].(map[string]any); kwargs["enable_thinking"] != true {
		t.Errorf("toggle auto: chat_template_kwargs = %v, want enable_thinking true", body["chat_template_kwargs"])
	}
	if body["temperature"] != 0.4 {
		t.Errorf("toggle keeps temperature: %v", body["temperature"])
	}

	effort := base
	effort.ReasoningEffort, effort.ReasoningControl = "auto", configurationapp.ReasoningControlEffort
	body = encodedBody(t, effort)
	if body["reasoning_effort"] != "medium" {
		t.Errorf("effort auto: reasoning_effort = %v, want medium", body["reasoning_effort"])
	}
	if _, present := body["temperature"]; present {
		t.Errorf("effort while thinking must not carry temperature: %v", body)
	}

	legacyOff := base
	legacyOff.ReasoningEffort = "none"
	body = encodedBody(t, legacyOff)
	if body["reasoning_effort"] != "none" || body["temperature"] != 0.4 {
		t.Errorf("undeclared off: %v", body)
	}

	fixed := base
	fixed.ReasoningEffort, fixed.ReasoningControl = "high", configurationapp.ReasoningControlFixedOn
	body = encodedBody(t, fixed)
	if _, present := body["reasoning_effort"]; present {
		t.Errorf("fixed_on sent a form: %v", body)
	}

	conflict := base
	conflict.ReasoningEffort, conflict.ReasoningControl = "off", configurationapp.ReasoningControlFixedOn
	if _, err := encodeChatCompletions(conflict, false); err == nil {
		t.Error("off on fixed_on encoded, want a refusal")
	}
}

func TestPrepareReasoningReadsTheControlAndRefusesAConflict(t *testing.T) {
	items := []configurationapp.CurrentModelCatalogItem{
		{Name: "qwen3", ProjectID: 1, SupportsReasoning: boolRef(true), ReasoningControl: stringRef("toggle")},
		{Name: "qwen3", ProjectID: 7, SupportsReasoning: boolRef(true), ReasoningControl: stringRef("fixed_on")},
		{Name: "gpt-4o", ProjectID: 7, SupportsReasoning: boolRef(false)},
		{Name: "mystery", ProjectID: 7},
	}
	catalog := &defaultCatalogStub{result: configurationapp.CurrentModelCatalogResponse{Items: items}}
	completer, err := WithDefaultModel(&recordingCompleter{content: "ok"}, catalog, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model, setting, wantControl, wantSetting, refusal string
	}{
		// The project's own row wins over the shared one of that name.
		{model: "qwen3", setting: "auto", wantControl: "fixed_on", wantSetting: "auto"},
		{model: "qwen3", setting: "off", refusal: "this model always thinks"},
		{model: "gpt-4o", setting: "low", refusal: "this model cannot think"},
		{model: "gpt-4o", setting: "none", wantControl: "fixed_off", wantSetting: "off"},
		{model: "mystery", setting: "high", wantControl: "", wantSetting: "high"},
		{model: "not-listed", setting: "medium", wantControl: "", wantSetting: "medium"},
		{model: "qwen3", setting: "max", refusal: "reasoning_effort must be one of"},
	} {
		req := CompletionRequest{ProjectID: "7", Model: tc.model, ReasoningEffort: tc.setting}
		got, err := PrepareReasoning(context.Background(), completer, req)
		if tc.refusal != "" {
			message, refused := IsReasoningError(err)
			if !refused || !strings.Contains(message, tc.refusal) {
				t.Errorf("%s/%s: err = %v, want a refusal containing %q", tc.model, tc.setting, err, tc.refusal)
			}
			continue
		}
		if err != nil || got.ReasoningControl != tc.wantControl || got.ReasoningEffort != tc.wantSetting {
			t.Errorf("%s/%s: got (%q, %q, %v), want (%q, %q)", tc.model, tc.setting,
				got.ReasoningControl, got.ReasoningEffort, err, tc.wantControl, tc.wantSetting)
		}
	}
	// A prepared request is not resolved again on dispatch.
	calls := catalog.calls
	prepared, err := PrepareReasoning(context.Background(), completer, CompletionRequest{ProjectID: "7", Model: "qwen3", ReasoningEffort: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completer.Complete(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if catalog.calls != calls+1 {
		t.Errorf("catalogue reads = %d, want one for prepare and none for dispatch", catalog.calls-calls)
	}
}

func TestPredictLLMRefusesASettingTheModelCannotHonour(t *testing.T) {
	t.Parallel()
	catalog := &defaultCatalogStub{result: configurationapp.CurrentModelCatalogResponse{Items: []configurationapp.CurrentModelCatalogItem{
		{Name: "qwq", ProjectID: 7, SupportsReasoning: boolRef(true), ReasoningControl: stringRef("fixed_on")},
	}}}
	next := &recordingCompleter{content: "never"}
	completer, err := WithDefaultModel(next, catalog, 1)
	if err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]string{
		`{"user_input":"x","llm_settings":{"model_name":"qwq","reasoning_effort":"off"}}`:     "this model always thinks",
		`{"user_input":"x","llm_settings":{"model_name":"qwq","reasoning_effort":"minimal"}}`: "reasoning_effort must be one of",
	} {
		recorder := serve(t, NewHandler(completer), body)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("%s: status %d body %s, want 400 with %q", body, recorder.Code, recorder.Body.String(), want)
		}
	}
	if next.calls != 0 {
		t.Errorf("a refused setting reached the gateway %d times", next.calls)
	}

	recorder := serve(t, NewHandler(completer), `{"user_input":"x","llm_settings":{"model_name":"qwq","reasoning_effort":"auto"}}`)
	if recorder.Code != http.StatusOK || next.got.ReasoningEffort != "auto" || next.got.ReasoningControl != "fixed_on" {
		t.Errorf("auto on fixed_on: status %d, request %+v", recorder.Code, next.got)
	}
}
