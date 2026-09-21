package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"google.golang.org/protobuf/proto"
)

func summaryFreezer(t *testing.T) (*CurrentApplicationToolSnapshotService, *currentAgentModelCatalogStub) {
	t.Helper()
	models := currentAgentModelCatalogForTest(true)
	compatible, reasoning := false, false
	models.response.Items = append(models.response.Items, configurationapp.CurrentModelCatalogItem{
		Name: "claude-summary", ProjectID: 1, OpenAICompatible: &compatible, SupportsReasoning: &reasoning,
		ContextWindow: ptrModelLimit(32000), MaxOutputTokens: ptrModelLimit(4000), MaxInputTokens: ptrModelLimit(28000),
	})
	service, err := NewCurrentApplicationToolSnapshotService(
		&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{}, models,
		&currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	return service, models
}

func summaryFreezeRequest(settings map[string]any) CurrentApplicationVersionFreezeRequest {
	return CurrentApplicationVersionFreezeRequest{
		ProjectID: 7, ActorUserID: 11, SummaryLLMSettings: settings,
		VersionDetails: json.RawMessage(`{"agent_type":"agent","llm_settings":{"model_name":"model"},"tools":[],"summary_model":{"llm_settings":"forged","model_context_limits":{"context_window_tokens":999999999}}}`),
	}
}

func TestSummaryModelFreezesAuthorizedCatalogueAndReachesBothInputBuilders(t *testing.T) {
	service, models := summaryFreezer(t)
	raw, err := service.FreezeCurrentApplicationVersion(context.Background(), summaryFreezeRequest(map[string]any{
		"model_name": "claude-summary", "model_project_id": 1, "max_tokens": 512, "temperature": 0.25,
	}))
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(raw)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := currentFrozenSummaryModel(version)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(summary.LlmSettings, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["model_name"] != "claude-summary" || settings["model_project_id"] != float64(1) ||
		settings["max_tokens"] != float64(512) || settings["openai_compatible"] != false || settings["temperature"] != 0.25 {
		t.Fatalf("unexpected frozen summary settings: %v", settings)
	}
	if summary.ModelContextLimits.ContextWindowTokens != 32000 || summary.ModelContextLimits.MaxOutputTokens != 4000 || summary.ModelContextLimits.ContextWindowFallback {
		t.Fatalf("summary limits lost catalogue authority: %v", summary.ModelContextLimits)
	}
	if summary.ModelContextLimits.GetMaxInputTokens() != 28000 {
		t.Fatal("summary model lost its separate input ceiling")
	}
	for _, query := range models.queries {
		if query.ProjectID != 7 || query.PublicProjectID != 1 || !query.IncludeShared {
			t.Fatalf("wrong catalogue scope: %+v", query)
		}
	}
	application, err := currentApplicationInput(validCurrentApplicationStartRequest(), CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41, VersionDetails: raw, Variables: json.RawMessage(`[]`),
		ChatHistory: json.RawMessage(`[]`), InternalTools: json.RawMessage(`[]`),
	}, json.RawMessage(`null`), json.RawMessage(`{}`), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	adhoc, err := currentAdhocInput(validCurrentAdhocStartRequest(), CurrentAdhocTarget{
		ConversationMeta: json.RawMessage(`{}`), ChatHistory: json.RawMessage(`[]`),
	}, raw, json.RawMessage(`null`), json.RawMessage(`{}`), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []*runtimev1.AgentExecutionInputV1{application, adhoc} {
		wire, err := proto.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var restored runtimev1.AgentExecutionInputV1
		if err := proto.Unmarshal(wire, &restored); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(summary, restored.SummaryModel) {
			t.Fatal("summary snapshot changed at dispatch")
		}
		if proto.Equal(restored.ModelContextLimits, restored.SummaryModel.ModelContextLimits) {
			t.Fatal("task and summary limits were conflated")
		}
	}
}

func TestSummaryModelRejectsUnknownSelectionsWithoutDefaultFallback(t *testing.T) {
	for _, selection := range []map[string]any{
		{"model_name": "missing"}, {"model_name": "claude-summary", "model_project_id": 99},
		{"model_name": "claude-summary", "model_project_id": 0},
		{"model_name": 42}, {"model_project_id": 1},
		{"model_name": "claude-summary", "max_tokens": 4001},
		{"model_name": "claude-summary", "max_tokens": -1},
		{"model_name": "claude-summary", "max_tokens": 0},
		{"model_name": "claude-summary", "max_tokens": 1.5},
		{"model_name": "claude-summary", "temperature": "0.5"},
		{"model_name": "claude-summary", "temperature": 2},
		{"model_name": "claude-summary", "openai_compatible": true},
		{"model_name": "claude-summary", "api_key": "must-not-be-accepted"},
	} {
		service, models := summaryFreezer(t)
		name, project := "model", int32(7)
		models.response.DefaultModelName, models.response.DefaultModelProjectID = &name, &project
		if _, err := service.FreezeCurrentApplicationVersion(context.Background(), summaryFreezeRequest(selection)); err == nil {
			t.Fatalf("accepted invalid summary selection: %v", selection)
		}
	}
}

func TestSummaryModelDropsForgedSnapshotsAndSupportsOutputOnlySelection(t *testing.T) {
	for _, settings := range []map[string]any{nil, {}, {"max_tokens": 1000}} {
		service, _ := summaryFreezer(t)
		raw, err := service.FreezeCurrentApplicationVersion(context.Background(), summaryFreezeRequest(settings))
		if err != nil {
			t.Fatal(err)
		}
		version, err := decodeCurrentApplicationVersion(raw)
		if err != nil {
			t.Fatal(err)
		}
		summary, err := currentFrozenSummaryModel(version)
		if err != nil {
			t.Fatal(err)
		}
		if len(settings) == 0 {
			if summary != nil {
				t.Fatal("an authored snapshot survived without an authorized selection")
			}
		} else {
			var selected map[string]any
			if err := json.Unmarshal(summary.LlmSettings, &selected); err != nil {
				t.Fatal(err)
			}
			if selected["model_name"] != "model" || selected["max_tokens"] != float64(1000) {
				t.Fatal("output-only selection did not retain the task model")
			}
		}
	}
	service, _ := summaryFreezer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.FreezeCurrentApplicationVersion(ctx, summaryFreezeRequest(map[string]any{"model_name": "claude-summary"})); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestSummarySamplingDoesNotEnableModelReasoning(t *testing.T) {
	service, models := summaryFreezer(t)
	supported := true
	models.response.Items[1].SupportsReasoning = &supported
	raw, err := service.FreezeCurrentApplicationVersion(context.Background(), summaryFreezeRequest(map[string]any{
		"model_name": "claude-summary", "temperature": 0.25,
	}))
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(raw)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := currentFrozenSummaryModel(version)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(summary.LlmSettings, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["temperature"] != 0.25 || settings["max_tokens"] != float64(4000) {
		t.Fatal("summary controls lost the selected model's bounds")
	}
	if _, present := settings["reasoning_effort"]; present {
		t.Fatal("model reasoning support enabled reasoning implicitly")
	}
}
