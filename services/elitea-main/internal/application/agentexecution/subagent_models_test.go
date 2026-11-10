package agentexecution

import (
	"context"
	"encoding/json"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// subagentFreezer has the agent's model, a second project model, the same
// name in the public project, and a public model.
func subagentFreezer(t *testing.T) *CurrentApplicationToolSnapshotService {
	t.Helper()
	models := currentAgentModelCatalogForTest(true)
	compatible := true
	models.response.Items = append(models.response.Items,
		configurationapp.CurrentModelCatalogItem{
			Name: "small", ProjectID: 7, OpenAICompatible: &compatible,
			ContextWindow: ptrModelLimit(32000), MaxOutputTokens: ptrModelLimit(4000),
		},
		configurationapp.CurrentModelCatalogItem{
			Name: "small", ProjectID: 1, Shared: true, OpenAICompatible: &compatible,
			ContextWindow: ptrModelLimit(64000), MaxOutputTokens: ptrModelLimit(16000),
		},
		configurationapp.CurrentModelCatalogItem{
			Name: "public-large", ProjectID: 1, Shared: true, OpenAICompatible: &compatible,
			ContextWindow: ptrModelLimit(200000), MaxOutputTokens: ptrModelLimit(32000),
		},
	)
	service, err := NewCurrentApplicationToolSnapshotService(
		&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{}, models,
		&currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

const subagentVersion = `{"agent_type":"agent","llm_settings":{"model_name":"model"},"tools":[],` +
	`"subagent_models":[{"llm_settings":"forged"}]}`

// #1825: with sub-agents enabled the freeze writes the catalogue's other
// models (the project's own before the public one of the same name, never
// the agent's own model), and both input builders carry them in field 77.
// A list the saved version carried is never kept.
func TestSubagentModelsAreFrozenFromTheCatalogue(t *testing.T) {
	service := subagentFreezer(t)
	raw, err := service.FreezeCurrentApplicationVersion(context.Background(), CurrentApplicationVersionFreezeRequest{
		ProjectID: 7, ActorUserID: 11, VersionDetails: json.RawMessage(subagentVersion),
		InternalTools: json.RawMessage(`["subagents"]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(raw)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := currentFrozenSubagentPolicy(version)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]map[string]any{}
	for _, model := range policy.GetModels() {
		var settings map[string]any
		if err := json.Unmarshal(model.GetLlmSettings(), &settings); err != nil {
			t.Fatal(err)
		}
		names[settings["model_name"].(string)] = settings
	}
	if len(names) != 2 || names["small"] == nil || names["public-large"] == nil {
		t.Fatalf("models = %v", names)
	}
	if names["small"]["model_project_id"] != float64(7) || names["small"]["max_tokens"] != float64(4000) {
		t.Fatalf("small = %v, want the project's own with its output cap", names["small"])
	}
	if names["public-large"]["max_tokens"] != float64(subagentModelOutputCap) {
		t.Fatalf("public-large = %v", names["public-large"])
	}
	if policy.GetMaxDepth() != 0 {
		t.Fatal("the freeze sets no depth; admission stamps it")
	}

	application, err := currentApplicationInput(validCurrentApplicationStartRequest(), CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41, VersionDetails: raw, Variables: json.RawMessage(`[]`),
		ChatHistory: json.RawMessage(`[]`), InternalTools: json.RawMessage(`["subagents"]`),
	}, json.RawMessage(`{}`), nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	adhoc, err := currentAdhocInput(validCurrentAdhocStartRequest(), CurrentAdhocTarget{
		ConversationMeta: json.RawMessage(`{}`), ChatHistory: json.RawMessage(`[]`),
	}, raw, json.RawMessage(`{}`), nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []*runtimev1.AgentExecutionInputV1{application, adhoc} {
		if !proto.Equal(input.GetSubagentPolicy(), policy) {
			t.Fatalf("policy = %v", input.GetSubagentPolicy())
		}
	}
}

// Without the sub-agents toggle nothing is frozen, and a forged list goes.
func TestSubagentModelsNeedTheToggle(t *testing.T) {
	service := subagentFreezer(t)
	for _, request := range []CurrentApplicationVersionFreezeRequest{
		{ProjectID: 7, ActorUserID: 11, VersionDetails: json.RawMessage(subagentVersion)},
		{ProjectID: 7, ActorUserID: 11, VersionDetails: json.RawMessage(subagentVersion), InternalTools: json.RawMessage(`["planner"]`)},
	} {
		raw, err := service.FreezeCurrentApplicationVersion(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		version, err := decodeCurrentApplicationVersion(raw)
		if err != nil {
			t.Fatal(err)
		}
		if policy, err := currentFrozenSubagentPolicy(version); err != nil || policy != nil {
			t.Fatalf("policy = %v, err = %v", policy, err)
		}
	}
	// An ad-hoc chat enables sub-agents in its meta.
	raw, err := service.FreezeCurrentApplicationVersion(context.Background(), CurrentApplicationVersionFreezeRequest{
		ProjectID: 7, ActorUserID: 11,
		VersionDetails: json.RawMessage(`{"llm_settings":{"model_name":"model"},"tools":[],"meta":{"internal_tools":["subagents"]}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(raw)
	if err != nil {
		t.Fatal(err)
	}
	if policy, err := currentFrozenSubagentPolicy(version); err != nil || len(policy.GetModels()) != 2 {
		t.Fatalf("ad-hoc policy = %v, err = %v", policy, err)
	}
}

// #1824: admission stamps the platform's depth. The caller's depth is never
// trusted, the frozen models stay, and a run at depth 1 without models is
// unchanged.
func TestAdmissionStampsTheSubagentDepth(t *testing.T) {
	t.Parallel()
	models := []*runtimev1.SubAgentModelV1{{LlmSettings: []byte(`{"model_name":"small"}`)}}
	depth2 := (&AdmissionService{}).WithToolEnvelope(agentToolSettingsStub{settings: platformconfig.AgentToolSettings{SubagentsMaxDepth: 2}})
	input := envelopeInput(`{}`)
	input.SubagentPolicy = &runtimev1.SubAgentPolicyV1{Models: models}
	stamped, err := depth2.stampToolEnvelope(context.Background(), executiondomain.AgentApplicationCapability, input)
	if err != nil {
		t.Fatal(err)
	}
	if stamped.GetSubagentPolicy().GetMaxDepth() != 2 || len(stamped.GetSubagentPolicy().GetModels()) != 1 {
		t.Fatalf("policy = %v", stamped.GetSubagentPolicy())
	}
	if input.GetSubagentPolicy().GetMaxDepth() != 0 {
		t.Fatal("the caller's input changed")
	}

	plain := envelopeInput(`{}`)
	for _, service := range []*AdmissionService{(&AdmissionService{}).WithToolEnvelope(agentToolSettingsStub{}), {}} {
		if same, err := service.stampToolEnvelope(context.Background(), executiondomain.AgentApplicationCapability, plain); err != nil || same != plain {
			t.Fatalf("same = %v, err = %v", same, err)
		}
		forged := envelopeInput(`{}`)
		forged.SubagentPolicy = &runtimev1.SubAgentPolicyV1{MaxDepth: 2}
		stamped, err := service.stampToolEnvelope(context.Background(), executiondomain.AgentApplicationCapability, forged)
		if err != nil || stamped.GetSubagentPolicy() != nil {
			t.Fatalf("forged depth kept: %v, err = %v", stamped.GetSubagentPolicy(), err)
		}
		withModels := envelopeInput(`{}`)
		withModels.SubagentPolicy = &runtimev1.SubAgentPolicyV1{MaxDepth: 2, Models: models}
		stamped, err = service.stampToolEnvelope(context.Background(), executiondomain.AgentApplicationCapability, withModels)
		if err != nil || stamped.GetSubagentPolicy().GetMaxDepth() != 1 || len(stamped.GetSubagentPolicy().GetModels()) != 1 {
			t.Fatalf("policy = %v, err = %v", stamped.GetSubagentPolicy(), err)
		}
	}
}
