package agentexecution

import (
	"context"
	"encoding/json"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// #1936: one freeze reads the model catalogue once, however many of the
// agent's model, the summary model and the sub-agent models it resolves; and
// the summary model and a sub-agent model of the same catalogue model have
// one shape (frozenCatalogModel), which the worker's admit reads for both.
func TestOneFreezeReadsTheCatalogueOnceAndSharesTheModelShape(t *testing.T) {
	service := subagentFreezer(t)
	stub := service.models.(*currentAgentModelCatalogStub)
	raw, err := service.FreezeCurrentApplicationVersion(context.Background(), CurrentApplicationVersionFreezeRequest{
		ProjectID: 7, ActorUserID: 11, VersionDetails: json.RawMessage(subagentVersion),
		InternalTools: json.RawMessage(`["subagents"]`), SummaryLLMSettings: map[string]any{"model_name": "small"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.queries) != 1 {
		t.Fatalf("catalogue reads = %d, want 1", len(stub.queries))
	}
	version, err := decodeCurrentApplicationVersion(raw)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := currentFrozenSummaryModel(version)
	if err != nil || summary == nil {
		t.Fatalf("summary = %v, err = %v", summary, err)
	}
	policy, err := currentFrozenSubagentPolicy(version)
	if err != nil {
		t.Fatal(err)
	}
	var small *runtimev1.SubAgentModelV1
	for _, model := range policy.GetModels() {
		var settings map[string]any
		if err := json.Unmarshal(model.GetLlmSettings(), &settings); err != nil {
			t.Fatal(err)
		}
		if settings["model_name"] == "small" {
			small = model
		}
	}
	if small == nil {
		t.Fatal("no sub-agent model small")
	}
	if string(summary.GetLlmSettings()) != string(small.GetLlmSettings()) ||
		!proto.Equal(summary.GetModelContextLimits(), small.GetModelContextLimits()) ||
		!proto.Equal(summary.GetModelReasoning(), small.GetModelReasoning()) {
		t.Fatalf("summary %v and sub-agent %v shapes differ", summary, small)
	}
}

// The one-pass map picks what selectCurrentAgentModel picks for a name with
// no project, for every name in the catalogue.
func TestSelectableCatalogModelsMatchesTheScan(t *testing.T) {
	compatible := true
	items := []configurationapp.CurrentModelCatalogItem{
		{Name: "a", ProjectID: 1, MaxOutputTokens: ptrModelLimit(1)},
		{Name: "a", ProjectID: 7, MaxOutputTokens: ptrModelLimit(2)},
		{Name: "a", ProjectID: 7, MaxOutputTokens: ptrModelLimit(3)},
		{Name: "b", ProjectID: 1, MaxOutputTokens: ptrModelLimit(4)},
		{Name: "b", ProjectID: 1, MaxOutputTokens: ptrModelLimit(5)},
		{Name: "c", ProjectID: 9, OpenAICompatible: &compatible},
		{Name: "", ProjectID: 7},
	}
	selectable := selectableCatalogModels(items, 7, 1)
	for _, name := range []string{"a", "b", "c", "", "missing"} {
		want, wantFound := selectCurrentAgentModel(items, name, 0, false, 7, 1)
		got, found := selectable[name]
		if found != wantFound || (found && *got.MaxOutputTokens != *want.MaxOutputTokens) {
			t.Fatalf("%q: got %+v %v, want %+v %v", name, got, found, want, wantFound)
		}
	}
}
