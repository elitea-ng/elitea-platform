package repos

import (
	"context"
	"strings"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

// The description is display text. A row that carries none, or carries a
// malformed one, still reaches the picker.
func TestCurrentModelsRepositoryReadsTheDescriptionTolerantly(t *testing.T) {
	queries := &currentModelQueriesStub{rows: []sqlcgen.ListCurrentModelConfigurationsRow{
		currentModelRow(1, 7, configurationapp.CurrentModelSectionLLM, "Described", false,
			`{"name":"described","description":"  Best for coding and agents "}`),
		currentModelRow(2, 7, configurationapp.CurrentModelSectionLLM, "Plain", false,
			`{"name":"plain"}`),
		currentModelRow(3, 7, configurationapp.CurrentModelSectionLLM, "Malformed", false,
			`{"name":"malformed","description":{"text":"x"}}`),
		currentModelRow(4, 7, configurationapp.CurrentModelSectionLLM, "Long", false,
			`{"name":"long","description":"`+strings.Repeat("y", 70)+`"}`),
	}}
	repository := newCurrentModelsRepositoryForTest(t, &currentModelProjectStore{}, queries)
	items, err := repository.List(context.Background(), 7, configurationapp.CurrentModelSectionLLM, false)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]configurationapp.CurrentModelCatalogItem{}
	for _, item := range items {
		byName[item.Name] = item
	}
	if len(byName) != 4 {
		t.Fatalf("items = %#v, want all four rows", items)
	}
	if got := byName["described"].Description; got == nil || *got != "Best for coding and agents" {
		t.Fatalf("described = %v", got)
	}
	if byName["plain"].Description != nil || byName["malformed"].Description != nil {
		t.Fatal("a missing or malformed description must read as none")
	}
	if got := byName["long"].Description; got == nil || len(*got) != configurationapp.MaxLLMModelDescriptionRunes {
		t.Fatalf("long = %v, want it cut to the limit", got)
	}
}

func TestCurrentModelsRepositoryIgnoresTheDescriptionOutsideLLM(t *testing.T) {
	queries := &currentModelQueriesStub{rows: []sqlcgen.ListCurrentModelConfigurationsRow{
		currentModelRow(1, 7, configurationapp.CurrentModelSectionEmbedding, "E", false,
			`{"name":"e","description":"text"}`),
	}}
	repository := newCurrentModelsRepositoryForTest(t, &currentModelProjectStore{}, queries)
	items, err := repository.List(context.Background(), 7, configurationapp.CurrentModelSectionEmbedding, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Description != nil {
		t.Fatalf("items = %#v, want no description on an embedding model", items)
	}
}
