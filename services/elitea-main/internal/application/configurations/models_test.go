package configurations

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func TestBuildCurrentModelCatalogDeduplicatesIncludesSharedAndResolvesDefaults(t *testing.T) {
	zulu := "Zulu"
	replaced := "Beta"
	alpha := "Alpha"
	ignored := "Ignored"
	response := BuildCurrentModelCatalog(CurrentModelCatalogRequest{
		Section:         CurrentModelSectionEmbedding,
		ProjectID:       7,
		PublicProjectID: 1,
		IncludeShared:   true,
		ProjectItems: []CurrentModelCatalogItem{
			{Name: "project-model", DisplayName: &zulu, ProjectID: 7},
			{Name: "project-model", DisplayName: &replaced, ProjectID: 7},
		},
		PublicSharedItems: []CurrentModelCatalogItem{
			{Name: "public-model", DisplayName: &alpha, ProjectID: 1, Shared: true},
			{Name: "not-shared", DisplayName: &ignored, ProjectID: 1, Shared: false},
		},
		Defaults: CurrentModelCatalogDefaults{Model: CurrentModelDefaultSources{
			Project: CurrentModelDefault{Name: "public-model"},
			Public:  CurrentModelDefault{Name: "unused-public-name", ProjectID: "1"},
		}},
	})

	if response.Total != 2 || len(response.Items) != 2 {
		t.Fatalf("deduplicated response=%#v", response)
	}
	if response.DefaultModelName == nil || *response.DefaultModelName != "public-model" ||
		response.DefaultModelProjectID == nil || *response.DefaultModelProjectID != 1 {
		t.Fatalf("component-wise project/public default=%#v", response)
	}
	if !response.DefaultModelConfigured {
		t.Fatal("a configured default must read as configured")
	}
	if response.Items[0].Name != "public-model" || !response.Items[0].Shared || !response.Items[0].Default {
		t.Fatalf("shared default was not sorted first: %#v", response.Items)
	}
	if response.Items[1].Name != "project-model" || response.Items[1].DisplayName == nil ||
		*response.Items[1].DisplayName != "Beta" || response.Items[1].Default {
		t.Fatalf("last duplicate value was not retained: %#v", response.Items)
	}
}

func TestBuildCurrentModelCatalogFallbackUsesFirstAvailableBeforeResponseSort(t *testing.T) {
	zulu := "Zulu"
	alpha := "alpha"
	response := BuildCurrentModelCatalog(CurrentModelCatalogRequest{
		Section:         CurrentModelSectionEmbedding,
		ProjectID:       7,
		PublicProjectID: 1,
		IncludeShared:   true,
		ProjectItems: []CurrentModelCatalogItem{
			{Name: "first-project-model", DisplayName: &zulu, ProjectID: 7},
		},
		PublicSharedItems: []CurrentModelCatalogItem{
			{Name: "sorted-first-shared", DisplayName: &alpha, ProjectID: 1, Shared: true},
		},
		Defaults: CurrentModelCatalogDefaults{Model: CurrentModelDefaultSources{
			Project: CurrentModelDefault{Name: "missing", ProjectID: "99"},
		}},
	})

	if response.DefaultModelName == nil || *response.DefaultModelName != "first-project-model" ||
		response.DefaultModelProjectID == nil || *response.DefaultModelProjectID != 7 {
		t.Fatalf("fallback default=%#v", response)
	}
	// The configured default is not in the catalogue, so the answer is the
	// first-item fallback, and it says so.
	if response.DefaultModelConfigured {
		t.Fatal("a first-item fallback must not read as configured")
	}
	if response.Items[0].Name != "sorted-first-shared" || response.Items[0].Default ||
		response.Items[1].Name != "first-project-model" || !response.Items[1].Default {
		t.Fatalf("sort changed fallback identity: %#v", response.Items)
	}
}

func TestBuildCurrentModelCatalogSortsCaseInsensitiveDisplayNameThenNameStably(t *testing.T) {
	beta := "bETA"
	alpha := "alpha"
	response := BuildCurrentModelCatalog(CurrentModelCatalogRequest{
		Section:   CurrentModelSectionEmbedding,
		ProjectID: 7,
		ProjectItems: []CurrentModelCatalogItem{
			{Name: "first-alpha", DisplayName: &alpha, ProjectID: 7},
			{Name: "Alpha", ProjectID: 7},
			{Name: "last-beta", DisplayName: &beta, ProjectID: 7},
		},
	})

	want := []string{"first-alpha", "Alpha", "last-beta"}
	got := make([]string, 0, len(response.Items))
	for _, item := range response.Items {
		got = append(got, item.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("case-insensitive stable order=%v, want %v", got, want)
	}
}

func TestBuildCurrentModelCatalogLLMTiersAreExplicitAndCapabilityGated(t *testing.T) {
	lowName := "Low"
	highName := "High"
	lowTier := true
	highTier := false
	response := BuildCurrentModelCatalog(CurrentModelCatalogRequest{
		Section:         CurrentModelSectionLLM,
		ProjectID:       7,
		PublicProjectID: 1,
		IncludeShared:   true,
		ProjectItems: []CurrentModelCatalogItem{
			{Name: "low", DisplayName: &lowName, ProjectID: 7, LowTier: &lowTier},
			{Name: "high-disabled", DisplayName: &highName, ProjectID: 7, HighTier: &highTier},
		},
		Defaults: CurrentModelCatalogDefaults{
			LowTier: CurrentModelDefaultSources{
				Project: CurrentModelDefault{Name: "low"},
				Public:  CurrentModelDefault{ProjectID: "7"},
			},
			HighTier: CurrentModelDefaultSources{
				Project: CurrentModelDefault{Name: "high-disabled", ProjectID: "7"},
			},
		},
	})

	if response.DefaultModelName == nil || *response.DefaultModelName != "low" ||
		response.DefaultModelProjectID == nil || *response.DefaultModelProjectID != 7 {
		t.Fatalf("ordinary LLM fallback=%#v", response)
	}
	if response.LowTierDefaultModelName == nil || *response.LowTierDefaultModelName != "low" ||
		response.LowTierDefaultModelProjectID != int32(7) {
		t.Fatalf("low-tier default=%#v", response)
	}
	if response.HighTierDefaultModelName == nil || *response.HighTierDefaultModelName != "" ||
		response.HighTierDefaultModelProjectID != "" {
		t.Fatalf("capability-disabled high-tier default=%#v", response)
	}
	for _, item := range response.Items {
		if item.ContextWindow == nil || *item.ContextWindow != 128000 ||
			item.MaxOutputTokens == nil || *item.MaxOutputTokens != 16000 ||
			item.SupportsReasoning == nil || *item.SupportsReasoning ||
			item.SupportsVision == nil || !*item.SupportsVision ||
			item.LowTier == nil || item.HighTier == nil || item.OpenAICompatible == nil {
			t.Fatalf("LLM defaults were not materialized: %#v", item)
		}
	}
}

func TestBuildCurrentModelCatalogPreservesSectionSpecificResponseFields(t *testing.T) {
	displayName := "Model"
	tests := []struct {
		name         string
		section      CurrentModelSection
		item         CurrentModelCatalogItem
		wantItemKeys []string
		wantTiers    bool
	}{
		{
			name: "llm", section: CurrentModelSectionLLM,
			item: CurrentModelCatalogItem{Name: "model", DisplayName: &displayName, ProjectID: 7},
			wantItemKeys: []string{
				"name", "display_name", "project_id", "shared", "context_window", "max_output_tokens",
				"supports_reasoning", "supports_vision", "low_tier", "high_tier", "openai_compatible", "default",
			},
			wantTiers: true,
		},
		{
			name: "embedding", section: CurrentModelSectionEmbedding,
			item:         CurrentModelCatalogItem{Name: "model", DisplayName: &displayName, ProjectID: 7},
			wantItemKeys: []string{"name", "display_name", "project_id", "shared", "default"},
		},
		{
			name: "vector storage", section: CurrentModelSectionVectorStorage,
			item:         CurrentModelCatalogItem{Name: "pgvector", DisplayName: &displayName, ProjectID: 7},
			wantItemKeys: []string{"name", "project_id", "shared", "default"},
		},
		{name: "image generation", section: CurrentModelSectionImageGeneration, item: CurrentModelCatalogItem{Name: "model", DisplayName: &displayName, ProjectID: 7}, wantItemKeys: []string{"name", "display_name", "project_id", "shared", "default"}},
		{name: "asr", section: CurrentModelSectionASR, item: CurrentModelCatalogItem{Name: "model", DisplayName: &displayName, ProjectID: 7}, wantItemKeys: []string{"name", "display_name", "project_id", "shared", "default"}},
		{name: "tts", section: CurrentModelSectionTTS, item: CurrentModelCatalogItem{Name: "model", DisplayName: &displayName, ProjectID: 7}, wantItemKeys: []string{"name", "display_name", "project_id", "shared", "default"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := BuildCurrentModelCatalog(CurrentModelCatalogRequest{
				Section: test.section, ProjectID: 7, PublicProjectID: 1,
				ProjectItems: []CurrentModelCatalogItem{test.item},
			})
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := json.Unmarshal(encoded, &document); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sortedCurrentModelKeys(document["items"].([]any)[0].(map[string]any)), sortedCurrentModelStrings(test.wantItemKeys)) {
				t.Fatalf("item fields=%s", encoded)
			}
			_, hasLowTier := document["low_tier_default_model_name"]
			_, hasHighTier := document["high_tier_default_model_name"]
			if hasLowTier != test.wantTiers || hasHighTier != test.wantTiers {
				t.Fatalf("tier fields=%s", encoded)
			}
			if test.wantTiers && (document["low_tier_default_model_project_id"] != "" || document["high_tier_default_model_project_id"] != "") {
				t.Fatalf("empty tier IDs changed=%s", encoded)
			}
		})
	}
}

func TestBuildCurrentModelCatalogSharedInputAndUnsupportedSectionBoundaries(t *testing.T) {
	publicName := "Public"
	request := CurrentModelCatalogRequest{
		Section:         CurrentModelSectionEmbedding,
		ProjectID:       7,
		PublicProjectID: 1,
		PublicSharedItems: []CurrentModelCatalogItem{
			{Name: "public", DisplayName: &publicName, ProjectID: 1, Shared: true},
		},
	}
	if response := BuildCurrentModelCatalog(request); response.Total != 0 || response.Items == nil {
		t.Fatalf("shared items included without request: %#v", response)
	}
	request.IncludeShared = true
	if response := BuildCurrentModelCatalog(request); response.Total != 1 || response.Items[0].Name != "public" {
		t.Fatalf("requested public shared item missing: %#v", response)
	}
	request.ProjectID = 1
	if response := BuildCurrentModelCatalog(request); response.Total != 0 {
		t.Fatalf("public project fetched its shared rows twice: %#v", response)
	}

	unsupported := BuildCurrentModelCatalog(CurrentModelCatalogRequest{
		Section:      CurrentModelSection("unknown"),
		ProjectItems: []CurrentModelCatalogItem{{Name: "must-not-leak", ProjectID: 7}},
	})
	if unsupported.Total != 0 || len(unsupported.Items) != 0 || unsupported.DefaultModelName != nil || unsupported.DefaultModelProjectID != nil {
		t.Fatalf("unsupported section response=%#v", unsupported)
	}
}

func sortedCurrentModelKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return sortedCurrentModelStrings(keys)
}

func sortedCurrentModelStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
