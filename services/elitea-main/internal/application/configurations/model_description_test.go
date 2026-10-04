package configurations

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeLLMModelDescription(t *testing.T) {
	forty := strings.Repeat("é", MaxLLMModelDescriptionRunes) // 40 characters, 80 bytes
	cases := []struct {
		name    string
		data    map[string]any
		wantErr bool
		want    any
		present bool
	}{
		{"absent", map[string]any{}, false, nil, false},
		{"null removed", map[string]any{"description": nil}, false, nil, false},
		{"blank removed", map[string]any{"description": " \t "}, false, nil, false},
		{"trimmed", map[string]any{"description": "  Best for coding and agents "}, false, "Best for coding and agents", true},
		{"40 characters counted as characters", map[string]any{"description": forty}, false, forty, true},
		{"41 characters refused", map[string]any{"description": strings.Repeat("a", 41)}, true, nil, true},
		{"40 after trim accepted", map[string]any{"description": "  " + strings.Repeat("a", 40) + "  "}, false, strings.Repeat("a", 40), true},
		{"not text refused", map[string]any{"description": true}, true, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NormalizeLLMModelDescription(tc.data)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if !errors.Is(err, ErrLLMModelDescriptionInvalid) {
					t.Fatalf("err = %v, want ErrLLMModelDescriptionInvalid", err)
				}
				return
			}
			value, present := tc.data["description"]
			if present != tc.present || (present && value != tc.want) {
				t.Fatalf("description = %q (present %v), want %q (present %v)", value, present, tc.want, tc.present)
			}
		})
	}
	if err := NormalizeLLMModelDescription(nil); err != nil {
		t.Fatalf("a nil map: %v", err)
	}
}

func TestReadLLMModelDescriptionIsTolerant(t *testing.T) {
	if ReadLLMModelDescription(42) != nil || ReadLLMModelDescription("  ") != nil || ReadLLMModelDescription(nil) != nil {
		t.Fatal("a non-text or blank value must read as no description")
	}
	long := ReadLLMModelDescription(strings.Repeat("b", 60))
	if long == nil || len([]rune(*long)) != MaxLLMModelDescriptionRunes {
		t.Fatalf("a stored long value = %v, want it cut to the limit", long)
	}
}

// A create keeps only declared fields. The description must survive it, or a
// description saved on create disappears while one saved on edit stays.
func TestLocalCreateKeepsTheModelDescription(t *testing.T) {
	data := map[string]any{
		"name":           "gpt-5",
		"ai_credentials": map[string]any{"elitea_title": "c", "private": false},
		"description":    "  Fast for everyday tasks ",
	}
	result, err := CurrentLocalConfigurationCreateNormalizer{}.NormalizeCreate("llm_model", data)
	if err != nil || !result.Complete {
		t.Fatalf("normalize: %v (complete %v)", err, result.Complete)
	}
	if result.Data["description"] != "Fast for everyday tasks" {
		t.Fatalf("description = %v, want the trimmed value", result.Data["description"])
	}

	data["description"] = strings.Repeat("x", 41)
	if _, err := (CurrentLocalConfigurationCreateNormalizer{}).NormalizeCreate("llm_model", data); err == nil {
		t.Fatal("a 41-character description must be refused on create")
	}

	data["description"] = "   "
	result, err = CurrentLocalConfigurationCreateNormalizer{}.NormalizeCreate("llm_model", data)
	if err != nil {
		t.Fatalf("normalize blank: %v", err)
	}
	if _, present := result.Data["description"]; present {
		t.Fatal("a blank description must not be stored")
	}

	embedding := map[string]any{
		"name":           "e",
		"ai_credentials": map[string]any{"elitea_title": "c", "private": false},
		"description":    "ignored",
	}
	result, err = CurrentLocalConfigurationCreateNormalizer{}.NormalizeCreate("embedding_model", embedding)
	if err != nil {
		t.Fatalf("normalize embedding: %v", err)
	}
	if _, present := result.Data["description"]; present {
		t.Fatal("only llm_model declares a description")
	}
}

// An update stores the submitted object, so the rule must run there too.
func TestMutationUpdateAppliesTheDescriptionRule(t *testing.T) {
	service := &CurrentConfigurationMutationService{normalizer: CurrentPoVDataNormalizer{}}
	schema := map[string]any{"properties": map[string]any{}}

	normalized, err := service.normalizeData(context.Background(), CurrentConfigurationNormalizationUpdate, 7, 1,
		"llm_model", schema, map[string]any{"name": "m", "description": " Short "})
	if err != nil || normalized["description"] != "Short" {
		t.Fatalf("normalized = %v, err = %v", normalized, err)
	}

	_, err = service.normalizeData(context.Background(), CurrentConfigurationNormalizationUpdate, 7, 1,
		"llm_model", schema, map[string]any{"name": "m", "description": strings.Repeat("x", 41)})
	var mutationErr *CurrentConfigurationMutationError
	if !errors.As(err, &mutationErr) || mutationErr.Field != "data.description" {
		t.Fatalf("err = %v, want a field error on data.description", err)
	}
}

func TestPinnedModelSchemaDeclaresTheDescription(t *testing.T) {
	catalog, err := LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := catalog.DataSchemaByType("llm_model")
	field, ok := data["properties"].(map[string]any)["description"].(map[string]any)
	if !ok || field["default"] != nil {
		t.Fatalf("description field = %v, want an optional field", field)
	}
	for _, key := range data["required"].([]any) {
		if key == "description" {
			t.Fatal("the description must not be required")
		}
	}
	plain, err := LoadCurrentAvailableCatalog([]byte(pinnedCurrentAvailableSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := plain.DataSchemaByType("llm_model")
	if _, exists := legacy["properties"].(map[string]any)["description"]; exists {
		t.Fatal("the legacy snapshot changed")
	}
}

func TestModelCatalogueCarriesTheDescriptionForLLMOnly(t *testing.T) {
	description := "Fast"
	item := CurrentModelCatalogItem{Name: "m", ProjectID: 7, Description: &description}
	llm := BuildCurrentModelCatalog(CurrentModelCatalogRequest{
		Section: CurrentModelSectionLLM, ProjectID: 7, PublicProjectID: 1, ProjectItems: []CurrentModelCatalogItem{item},
	})
	if len(llm.Items) != 1 || llm.Items[0].Description == nil || *llm.Items[0].Description != "Fast" {
		t.Fatalf("llm items = %+v, want the description", llm.Items)
	}
	embedding := BuildCurrentModelCatalog(CurrentModelCatalogRequest{
		Section: CurrentModelSectionEmbedding, ProjectID: 7, PublicProjectID: 1, ProjectItems: []CurrentModelCatalogItem{item},
	})
	if len(embedding.Items) != 1 || embedding.Items[0].Description != nil {
		t.Fatalf("embedding items = %+v, want no description", embedding.Items)
	}
}
