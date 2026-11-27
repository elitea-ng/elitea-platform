package agentexecution

import (
	"context"
	"encoding/json"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"google.golang.org/protobuf/proto"
)

func TestCurrentModelContextLimitsUseAuthorizedCatalogue(t *testing.T) {
	window, output := 1_000_000, 128_000
	models := currentAgentModelCatalogForTest(true)
	models.response.Items[0].ContextWindow = &window
	models.response.Items[0].MaxOutputTokens = &output
	service, err := NewCurrentApplicationToolSnapshotService(
		&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{}, models,
		&currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := service.FreezeCurrentApplicationVersion(context.Background(), CurrentApplicationVersionFreezeRequest{
		ProjectID: 7, ActorUserID: 11,
		VersionDetails: json.RawMessage(`{"agent_type":"agent","llm_settings":{"model_name":"model"},"tools":[],"model_context_limits":{"context_window_tokens":999999999,"max_output_tokens":1,"max_input_tokens":999999999}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(raw)
	if err != nil {
		t.Fatal(err)
	}
	limits, err := currentFrozenModelContextLimits(version)
	if err != nil {
		t.Fatal(err)
	}
	if limits.ContextWindowTokens != 1_000_000 || limits.MaxOutputTokens != 128_000 ||
		limits.ContextWindowFallback || limits.MaxOutputFallback || limits.MaxInputTokens != nil {
		t.Fatalf("unexpected authorized limits: %v", limits)
	}
	input := &runtimev1.AgentExecutionInputV1{ModelContextLimits: limits}
	wire, err := proto.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var restored runtimev1.AgentExecutionInputV1
	if err := proto.Unmarshal(wire, &restored); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(limits, restored.ModelContextLimits) {
		t.Fatal("limits changed in the input contract")
	}
	applicationInput, err := currentApplicationInput(validCurrentApplicationStartRequest(), CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41, VersionDetails: raw,
		Variables: json.RawMessage(`[]`), ChatHistory: json.RawMessage(`[]`), InternalTools: json.RawMessage(`[]`),
	}, json.RawMessage(`{}`), nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	adhocInput, err := currentAdhocInput(validCurrentAdhocStartRequest(), CurrentAdhocTarget{
		ConversationMeta: json.RawMessage(`{}`), ChatHistory: json.RawMessage(`[]`),
	}, raw, json.RawMessage(`{}`), nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, dispatched := range []*runtimev1.AgentExecutionInputV1{applicationInput, adhocInput} {
		if !proto.Equal(limits, dispatched.ModelContextLimits) {
			t.Fatal("a dispatch path lost model context limits")
		}
	}
}

func TestCurrentModelContextLimitsRetainFallbackAndRejectInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name           string
		window, output *int
		invalid        bool
	}{
		{name: "missing catalogue fields"},
		{name: "zero window", window: new(int), invalid: true},
		{name: "negative output", output: ptrModelLimit(-1), invalid: true},
		{name: "output consumes window", window: ptrModelLimit(16000), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version := map[string]any{}
			err := freezeCurrentModelContextLimits(version, configurationapp.CurrentModelCatalogItem{
				ContextWindow: tc.window, MaxOutputTokens: tc.output,
			})
			if (err != nil) != tc.invalid {
				t.Fatalf("unexpected result: %v", err)
			}
			if tc.invalid {
				return
			}
			limits, err := currentFrozenModelContextLimits(version)
			if err != nil || !limits.ContextWindowFallback || !limits.MaxOutputFallback {
				t.Fatalf("fallback provenance lost: %v, %v", limits, err)
			}
		})
	}
	if limits, err := currentFrozenModelContextLimits(map[string]any{}); limits != nil || err != nil {
		t.Fatal("old frozen inputs must remain readable")
	}
}

func ptrModelLimit(value int) *int { return &value }

func TestCurrentModelContextFallbackSurvivesCatalogueNormalization(t *testing.T) {
	catalog := configurationapp.BuildCurrentModelCatalog(configurationapp.CurrentModelCatalogRequest{
		Section: configurationapp.CurrentModelSectionLLM, ProjectID: 7,
		ProjectItems: []configurationapp.CurrentModelCatalogItem{{Name: "model", ProjectID: 7}},
	})
	if len(catalog.Items) != 1 {
		t.Fatal("catalogue fixture lost its model")
	}
	item := catalog.Items[0]
	if item.ContextWindow == nil || item.MaxOutputTokens == nil {
		t.Fatal("expected normalized numeric defaults")
	}
	version := map[string]any{}
	if err := freezeCurrentModelContextLimits(version, item); err != nil {
		t.Fatal(err)
	}
	limits, err := currentFrozenModelContextLimits(version)
	if err != nil || !limits.ContextWindowFallback || !limits.MaxOutputFallback {
		t.Fatalf("normalization lost fallback provenance: %v, %v", limits, err)
	}
}

func TestCurrentModelContextLimitsPreserveExplicitInputCeiling(t *testing.T) {
	for _, input := range []int{272000, 0, -1} {
		version := map[string]any{}
		err := freezeCurrentModelContextLimits(version, configurationapp.CurrentModelCatalogItem{
			ContextWindow: ptrModelLimit(400000), MaxOutputTokens: ptrModelLimit(128000), MaxInputTokens: &input,
		})
		if input <= 0 {
			if err == nil {
				t.Fatal("invalid input ceiling accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		limits, err := currentFrozenModelContextLimits(version)
		if err != nil || limits.MaxInputTokens == nil || *limits.MaxInputTokens != 272000 {
			t.Fatalf("input ceiling lost: %v, %v", limits, err)
		}
	}
}

// Issue 3143: a run's context window is its model's, and a version that
// names no model this catalogue holds runs on the project default. A
// blueprint names none, so before the import pinned a model every imported
// agent ran on a 256k default although a 1M model was configured. The
// settings the import now writes (the model's name and project) select the
// 1M model and its window.
func TestImportedAgentContextWindowFollowsThePinnedModel(t *testing.T) {
	small, large, output := 262_144, 1_000_000, 32_000
	reasoning := false
	defaultName, defaultProject := "local-qwen", int32(7)
	models := &currentAgentModelCatalogStub{response: configurationapp.CurrentModelCatalogResponse{
		Items: []configurationapp.CurrentModelCatalogItem{
			{Name: "local-qwen", ProjectID: 7, ContextWindow: &small, MaxOutputTokens: &output, SupportsReasoning: &reasoning},
			{Name: "frontier-1m", ProjectID: 1, Shared: true, ContextWindow: &large, MaxOutputTokens: &output, SupportsReasoning: &reasoning},
		},
		DefaultModelName: &defaultName, DefaultModelProjectID: &defaultProject,
	}}
	service, err := NewCurrentApplicationToolSnapshotService(
		&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{}, models,
		&currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		llm    string
		window uint32
	}{
		{"a blueprint's settings, stored as the file had them", `{"reasoning_effort":"high"}`, 262_144},
		{"the settings the import pins", `{"reasoning_effort":"high","model_name":"frontier-1m","model_project_id":1}`, 1_000_000},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := service.FreezeCurrentApplicationVersion(context.Background(), CurrentApplicationVersionFreezeRequest{
				ProjectID: 7, ActorUserID: 11,
				VersionDetails: json.RawMessage(`{"agent_type":"agent","llm_settings":` + test.llm + `,"tools":[]}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			version, err := decodeCurrentApplicationVersion(raw)
			if err != nil {
				t.Fatal(err)
			}
			limits, err := currentFrozenModelContextLimits(version)
			if err != nil {
				t.Fatal(err)
			}
			if limits.ContextWindowTokens != test.window {
				t.Fatalf("context window %d, want %d", limits.ContextWindowTokens, test.window)
			}
		})
	}
}
