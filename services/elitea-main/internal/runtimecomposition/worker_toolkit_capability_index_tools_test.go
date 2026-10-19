package runtimecomposition_test

import (
	"testing"

	indexingapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexregistry"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/indexcatalog"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

// Under the Rust indexing runtime the Rust worker serves the five index tools
// inside the I1 indexing families (ADR-0030 I1); the pinned capability is
// widened by exactly those names, for exactly those families.
func TestWorkerToolkitCapabilityWithRustIndexTools(t *testing.T) {
	t.Parallel()
	pinned, err := runtimecomposition.LoadPinnedWorkerToolkitCapability("rust")
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range indexcatalog.Families {
		if ok, _ := pinned.SupportsTool(family, "search_index"); ok {
			t.Fatalf("the pinned snapshot must not claim search_index for %s", family)
		}
	}
	widened := pinned.WithRustIndexTools()
	for _, family := range indexcatalog.Families {
		for _, tool := range indexcatalog.ToolNames {
			if ok, reason := widened.SupportsTool(family, tool); !ok {
				t.Errorf("%s/%s must be served under rust indexing: %s", family, tool, reason)
			}
		}
		if ok, _ := widened.SupportsTool(family, "index_data"); ok {
			t.Errorf("%s/index_data is Main's, never a worker tool", family)
		}
	}
	// What the pinned snapshot served stays served, and the pinned value is
	// not mutated.
	if ok, _ := widened.SupportsTool("github", "read_file"); !ok {
		t.Error("github/read_file must stay served")
	}
	// A tool the family does not serve (PyGithub's method dispatcher) stays
	// refused; the widening is the five index tools and nothing else.
	if ok, _ := widened.SupportsTool("github", "generic_github_api_call"); ok {
		t.Error("github/generic_github_api_call must stay refused")
	}
	if ok, _ := pinned.SupportsTool("github", "search_index"); ok {
		t.Error("WithRustIndexTools must not mutate the pinned capability")
	}
	// Aha has no indexing toolkit: it stays as pinned.
	before, _ := pinned.SupportsTool("aha", "search_index")
	after, _ := widened.SupportsTool("aha", "search_index")
	if before != after {
		t.Errorf("aha/search_index changed from %v to %v", before, after)
	}
	var none *runtimecomposition.WorkerToolkitCapability
	if none.WithRustIndexTools() != nil {
		t.Error("a nil capability stays nil")
	}
}

// The catalogue's widening follows the composition root's one indexing
// decision (Config.RustIndexing): without index dispatch the capability stays
// as pinned.
func TestWorkerToolkitCapabilityForIndexingRuntimeFollowsTheCompositionDecision(t *testing.T) {
	t.Parallel()
	pinned, err := runtimecomposition.LoadPinnedWorkerToolkitCapability("rust")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		config runtimecomposition.Config
		served bool
	}{
		"index dispatch on": {
			config: runtimecomposition.Config{Enabled: true, IndexIngestDispatchEnabled: true},
			served: true,
		},
		"index dispatch off": {
			config: runtimecomposition.Config{Enabled: true},
			served: false,
		},
		"runtime off": {config: runtimecomposition.Config{}, served: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			capability := pinned.ForIndexingRuntime(tc.config)
			if capability == nil {
				t.Fatal("nil capability")
			}
			ok, _ := capability.SupportsTool("github", "search_index")
			if ok != tc.served {
				t.Fatalf("github/search_index served = %v; expected %v (RustIndexing=%v)", ok, tc.served, tc.config.RustIndexing())
			}
			if ok != tc.config.RustIndexing() {
				t.Fatal("the capability and the composition decision disagree")
			}
		})
	}
	var none *runtimecomposition.WorkerToolkitCapability
	if none.ForIndexingRuntime(runtimecomposition.Config{}) != nil {
		t.Error("a nil capability stays nil")
	}
}

// The permission the worker's remove_index is checked against is the one the
// REST delete route requires.
func TestIndexRemovalChecksTheRestRoutesDeletePermission(t *testing.T) {
	t.Parallel()
	if indexregistry.IndexDeletePermission != indexingapi.SourceOnlyIndexDeletePermission {
		t.Fatalf("%q != %q", indexregistry.IndexDeletePermission, indexingapi.SourceOnlyIndexDeletePermission)
	}
}
