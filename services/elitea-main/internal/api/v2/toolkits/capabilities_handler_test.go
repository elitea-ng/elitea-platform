package toolkits_test

// RuntimeCapabilities (#865, #866): which worker this deployment runs, which
// of the toggleable internal chat tools it actually runs, and which
// catalogued toolkit types it hides for capability reasons.
//
// getCapabilities below reuses catalogueOptions (type_catalogue_test.go),
// the exact wiring the composition root uses for ListTypeSchemas, so a test
// here and TestRustWorkerCapabilityWithholdsTheFamiliesItCannotMaterialize
// read the
// SAME pinned snapshots and cannot silently drift apart.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
)

func getCapabilities(t *testing.T, opts ...toolkits.Option) toolkits.RuntimeCapabilitiesResponse {
	t.Helper()
	handler := toolkits.NewHandlerWithRepo(&mockRepo{}, opts...)
	response := httptest.NewRecorder()
	handler.RuntimeCapabilities(response, httptest.NewRequest(http.MethodGet, "/runtime_capabilities", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", response.Code, response.Body.String())
	}
	var body toolkits.RuntimeCapabilitiesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (%s)", err, response.Body.String())
	}
	return body
}

// The internal-tool toggles (apps/elitea-web's internalTools.ts) #866 named,
// split by what the Rust worker does with them today: skipped, or served.
var (
	// Still skipped by internal_tools.rs's PLATFORM_INTERNAL_TOOLS.
	rustSkippedInternalTools = []string{
		"image_generation", "internal_mcp", "swarm",
	}
	// Served natively since the planner / Smart Tools Selection / attachments
	// port (libs/rust/agent-runtime planner.rs and lazy_tools.rs, the
	// worker's attachment_tools.rs).
	rustServedInternalTools = []string{
		"planner", "lazy_tools_mode", "attachments", "data_analysis",
		"skills_builder", "project_context_builder",
	}
)

func TestRuntimeCapabilitiesReportsTheConfiguredWorker(t *testing.T) {
	t.Parallel()

	for _, implementation := range []string{"rust"} {
		implementation := implementation
		t.Run(implementation, func(t *testing.T) {
			t.Parallel()
			body := getCapabilities(t, append(
				catalogueOptions(t, implementation),
				toolkits.WithWorkerImplementation(implementation),
			)...)
			if body.Worker != implementation {
				t.Errorf("worker=%q, want %q", body.Worker, implementation)
			}
		})
	}
}

// #866's core claim: a toggle the Rust worker skips is a no-op there, and this
// endpoint is the one place that says so. The four it still skips are
// reported false; the three it now serves (planner, Smart Tools Selection,
// attachments) are reported true, so the form stops marking them unavailable.
func TestRuntimeCapabilitiesInternalToolsOnRust(t *testing.T) {
	t.Parallel()

	body := getCapabilities(t, append(
		catalogueOptions(t, "rust"),
		toolkits.WithWorkerImplementation("rust"),
	)...)

	for _, name := range rustSkippedInternalTools {
		available, found := body.InternalTools[name]
		if !found {
			t.Errorf("internal_tools has no entry for %q", name)
			continue
		}
		if available {
			t.Errorf("internal_tools[%q]=true on rust, want false (#866)", name)
		}
	}
	for _, name := range rustServedInternalTools {
		available, found := body.InternalTools[name]
		if !found {
			t.Errorf("internal_tools has no entry for %q", name)
			continue
		}
		if !available {
			t.Errorf("internal_tools[%q]=false on rust, want true: the native runtime serves it", name)
		}
	}
}

// ask_user (the Rust worker's clarification tool, not a Modules toggle) and
// pyodide (the retired Python sandbox) are deliberately not reported on.
func TestRuntimeCapabilitiesInternalToolsExcludesOutOfScopeNames(t *testing.T) {
	t.Parallel()

	body := getCapabilities(t, append(
		catalogueOptions(t, "rust"),
		toolkits.WithWorkerImplementation("rust"),
	)...)

	for _, name := range []string{"ask_user", "pyodide"} {
		if _, found := body.InternalTools[name]; found {
			t.Errorf("internal_tools unexpectedly reports on %q", name)
		}
	}
}

// #865's 17: exactly the types materialize.rs has no arm for, restated flat.
func TestRuntimeCapabilitiesHiddenToolkitTypesOnRust(t *testing.T) {
	t.Parallel()

	body := getCapabilities(t, append(
		catalogueOptions(t, "rust"),
		toolkits.WithWorkerImplementation("rust"),
	)...)

	// The catalogue's own type keys, which is not always the issue's prose
	// name: powerpoint is catalogued as "pptx", xray as "xray_cloud", testIO
	// as "testio" (verified directly against
	// current_toolkit_catalogue_snapshot.json, not assumed from #865's text).
	// A family leaves this list when the Rust worker starts materializing
	// it (even partially); the served check below then covers it. Of #865's
	// named types none is left: pptx, the last, joined with ADR-0032 P3 (the
	// host lends it a model). These two have no native family.
	want := []string{
		"localgit", "delta_lake",
	}
	sort.Strings(want)
	got := append([]string(nil), body.HiddenToolkitTypes...)
	sort.Strings(got)

	gotSet := make(map[string]bool, len(got))
	for _, t := range got {
		gotSet[t] = true
	}
	for _, w := range want {
		if !gotSet[w] {
			t.Errorf("hidden_toolkit_types is missing %q", w)
		}
	}
	// Families the Rust worker DOES materialize must not appear hidden. A
	// partial family (no indexing tools) is supported per tool, not hidden.
	for _, served := range []string{
		"sql", "aha", "ado_boards", "ado_plans", "ado_repos", "ado_wiki",
		"bigquery", "bitbucket", "carrier", "confluence", "figma", "gitlab",
		"jira", "qtest", "testio", "testrail", "xray_cloud",
		"zephyr_enterprise", "zephyr_essential", "zephyr_scale",
	} {
		if gotSet[served] {
			t.Errorf("hidden_toolkit_types contains %q, which materialize.rs supports", served)
		}
	}
}

// Unset entirely (no WithWorkerImplementation), the endpoint reports an
// honest "" rather than guessing which worker is running.
func TestRuntimeCapabilitiesWorkerUnsetReportsEmpty(t *testing.T) {
	t.Parallel()

	body := getCapabilities(t, catalogueOptions(t, "")...)
	if body.Worker != "" {
		t.Errorf("worker=%q, want empty when never configured", body.Worker)
	}
}
