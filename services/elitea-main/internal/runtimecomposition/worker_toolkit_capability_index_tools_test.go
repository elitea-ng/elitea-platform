package runtimecomposition_test

import (
	"testing"

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
	if ok, _ := widened.SupportsTool("github", "create_pull_request"); ok {
		t.Error("github/create_pull_request must stay refused")
	}
	if ok, _ := pinned.SupportsTool("github", "search_index"); ok {
		t.Error("WithRustIndexTools must not mutate the pinned capability")
	}
	// Confluence (I2) stays as pinned: its index tools wait for their stage.
	before, _ := pinned.SupportsTool("confluence", "search_index")
	after, _ := widened.SupportsTool("confluence", "search_index")
	if before != after {
		t.Errorf("confluence/search_index changed from %v to %v", before, after)
	}
	python, err := runtimecomposition.LoadPinnedWorkerToolkitCapability("python")
	if err != nil {
		t.Fatal(err)
	}
	if python.WithRustIndexTools() != python {
		t.Error("a Python capability is returned unchanged")
	}
	var none *runtimecomposition.WorkerToolkitCapability
	if none.WithRustIndexTools() != nil {
		t.Error("a nil capability stays nil")
	}
}
