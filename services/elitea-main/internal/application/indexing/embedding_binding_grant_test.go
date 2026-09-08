package indexing

// The platform GRANT on an embedding model.
//
// A platform embedding model can be granted to every project, to none, or to a
// chosen set (application/configurations/model_grant.go). The index plane
// resolves such a model on its own path — the caller's project first, then the
// public project's shared rows — so the rule has to hold here too. Without it a
// project could not SEE a model it could still build an index against.
//
// Two things are pinned, and the first is the one that bites first: the grant
// lives on the same `data` object this package decodes with unknown fields
// REFUSED, so a granted model failed the decode entirely and stopped resolving
// for every project, including the ones it had just been granted to.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// grantedEmbeddingConfiguration is one public-project embedding row carrying a
// grant.
func grantedEmbeddingConfiguration(scope string, sharedWith []int32) CurrentEmbeddingConfiguration {
	document := map[string]any{
		"name": "embed",
		"ai_credentials": map[string]any{
			"elitea_title": "credential-current",
			"private":      false,
		},
		"share_scope": scope,
	}
	if sharedWith != nil {
		document["shared_with"] = sharedWith
	}
	data, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return CurrentEmbeddingConfiguration{
		UUID:      "00000000-0000-0000-0000-000000000101",
		ProjectID: 1,
		Type:      "embedding_model",
		Section:   "embedding",
		Data:      data,
		Shared:    true,
	}
}

// TestAGrantedEmbeddingModelStillResolves is the decode half.
//
// The two grant fields are declared on the strict document precisely so this
// passes; before that, `share_scope` made the whole row an invalid binding.
func TestAGrantedEmbeddingModelStillResolves(t *testing.T) {
	for _, sharedWith := range [][]int32{{7}, {7, 9}} {
		configurations := &embeddingConfigurationReaderStub{
			configurations: map[int32]CurrentEmbeddingConfiguration{
				1: grantedEmbeddingConfiguration("projects", sharedWith),
			},
		}
		resolver, err := NewCurrentEmbeddingBindingResolver(configurations, 1)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := resolver.Resolve(context.Background(), 7, "embed", nil)
		if err != nil {
			t.Fatalf("a granted embedding model did not resolve: %v", err)
		}
		if binding.ConfigurationUUID != "00000000-0000-0000-0000-000000000101" {
			t.Errorf("bound %q, want the granted public row", binding.ConfigurationUUID)
		}
	}
}

// TestAnUngrantedEmbeddingModelIsNotFound is the enforcement half.
//
// NOT FOUND rather than an error: for this project the model does not exist,
// which is the answer every caller already handles.
func TestAnUngrantedEmbeddingModelIsNotFound(t *testing.T) {
	for name, configuration := range map[string]CurrentEmbeddingConfiguration{
		"granted to another project": grantedEmbeddingConfiguration("projects", []int32{9}),
		"granted to no project":      grantedEmbeddingConfiguration("none", nil),
	} {
		t.Run(name, func(t *testing.T) {
			configurations := &embeddingConfigurationReaderStub{
				configurations: map[int32]CurrentEmbeddingConfiguration{1: configuration},
			}
			resolver, err := NewCurrentEmbeddingBindingResolver(configurations, 1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resolver.Resolve(context.Background(), 7, "embed", nil); err == nil {
				t.Fatal("an ungranted embedding model was bound")
			} else if !strings.Contains(err.Error(), "embedding") {
				t.Errorf("refusal = %v, want the not-found refusal this package raises", err)
			}
		})
	}
}

// TestTheCatalogueProjectBindsItsOwnUngrantedEmbeddingModel: the public project
// reads its own row as its own, so a withdrawn model stays usable there — which
// is what keeps it recoverable.
func TestTheCatalogueProjectBindsItsOwnUngrantedEmbeddingModel(t *testing.T) {
	configurations := &embeddingConfigurationReaderStub{
		configurations: map[int32]CurrentEmbeddingConfiguration{
			1: grantedEmbeddingConfiguration("none", nil),
		},
	}
	resolver, err := NewCurrentEmbeddingBindingResolver(configurations, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), 1, "embed", nil); err != nil {
		t.Fatalf("the catalogue project cannot bind its own model: %v", err)
	}
}

// TestAPinnedPublicEmbeddingModelIsStillSubjectToTheGrant.
//
// The pin says which project the model was resolved in, not that this project
// may use it — so a stored index whose model was later withdrawn stops binding
// rather than going on resolving for ever.
func TestAPinnedPublicEmbeddingModelIsStillSubjectToTheGrant(t *testing.T) {
	configurations := &embeddingConfigurationReaderStub{
		configurations: map[int32]CurrentEmbeddingConfiguration{
			1: grantedEmbeddingConfiguration("projects", []int32{9}),
		},
	}
	resolver, err := NewCurrentEmbeddingBindingResolver(configurations, 1)
	if err != nil {
		t.Fatal(err)
	}
	public := int32(1)
	if _, err := resolver.Resolve(context.Background(), 7, "embed", &public); err == nil {
		t.Fatal("a pinned public model was bound for a project holding no grant")
	}
}
