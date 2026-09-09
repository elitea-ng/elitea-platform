package toolkits_test

// RuntimeCapabilities (#865, #866): which worker this deployment runs, which
// of the six toggleable internal chat tools it actually runs, and which
// catalogued toolkit types it hides for capability reasons.
//
// getCapabilities below reuses catalogueOptions (type_catalogue_test.go),
// the exact wiring the composition root uses for ListTypeSchemas, so a test
// here and TestPythonWorkerCapabilityWithholdsTheTypesItCannotImport /
// TestRustWorkerCapabilityWithholdsTheFamiliesItCannotMaterialize read the
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

// The six internal-tool toggles (apps/elitea-web's internalTools.ts) named by
// #866.
var sixInternalTools = []string{
	"image_generation", "data_analysis", "internal_mcp",
	"planner", "swarm", "lazy_tools_mode",
}

func TestRuntimeCapabilitiesReportsTheConfiguredWorker(t *testing.T) {
	t.Parallel()

	for _, implementation := range []string{"python", "rust"} {
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

// #866's core claim: on Rust, every one of the six toggles the agent form
// offers is a no-op, and this endpoint is the one place that says so.
func TestRuntimeCapabilitiesInternalToolsOnRust(t *testing.T) {
	t.Parallel()

	body := getCapabilities(t, append(
		catalogueOptions(t, "rust"),
		toolkits.WithWorkerImplementation("rust"),
	)...)

	for _, name := range sixInternalTools {
		available, found := body.InternalTools[name]
		if !found {
			t.Errorf("internal_tools has no entry for %q", name)
			continue
		}
		if available {
			t.Errorf("internal_tools[%q]=true on rust, want false (#866)", name)
		}
	}
}

// On Python every one of the six runs for real.
func TestRuntimeCapabilitiesInternalToolsOnPython(t *testing.T) {
	t.Parallel()

	body := getCapabilities(t, append(
		catalogueOptions(t, "python"),
		toolkits.WithWorkerImplementation("python"),
	)...)

	for _, name := range sixInternalTools {
		available, found := body.InternalTools[name]
		if !found {
			t.Errorf("internal_tools has no entry for %q", name)
			continue
		}
		if !available {
			t.Errorf("internal_tools[%q]=false on python, want true", name)
		}
	}
}

// ask_user (the Rust worker's one real internal tool), attachments and
// pyodide are deliberately not among the six #866 reports on.
func TestRuntimeCapabilitiesInternalToolsExcludesOutOfScopeNames(t *testing.T) {
	t.Parallel()

	body := getCapabilities(t, append(
		catalogueOptions(t, "rust"),
		toolkits.WithWorkerImplementation("rust"),
	)...)

	for _, name := range []string{"ask_user", "attachments", "pyodide"} {
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
	// zephyr_essential is a genuine 18th type in the same boat as #865's
	// named 17 — present in the catalogue, absent from the Rust worker's
	// supported_tool_types — that #865 did not name; included here because
	// this test asserts what the endpoint actually reports, not what the
	// issue happened to list.
	want := []string{
		"ado_boards", "ado_plans", "ado_repos", "ado_wiki", "aha", "bitbucket",
		"confluence", "figma", "gitlab", "jira", "pptx", "qtest",
		"testio", "testrail", "xray_cloud", "zephyr_enterprise",
		"zephyr_essential", "zephyr_scale",
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
	// One family the Rust worker DOES materialize: it must not appear hidden.
	if gotSet["sql"] {
		t.Error(`hidden_toolkit_types contains "sql", which materialize.rs supports`)
	}
}

// The Python worker's own gap (9 types, #869's survivors) is a different,
// smaller set — and the 17 Rust-only-missing types above must NOT appear
// here, since the Python worker imports every one of them.
func TestRuntimeCapabilitiesHiddenToolkitTypesOnPython(t *testing.T) {
	t.Parallel()

	body := getCapabilities(t, append(
		catalogueOptions(t, "python"),
		toolkits.WithWorkerImplementation("python"),
	)...)

	gotSet := make(map[string]bool, len(body.HiddenToolkitTypes))
	for _, name := range body.HiddenToolkitTypes {
		gotSet[name] = true
	}
	for _, want := range pythonUnsupportedTypes {
		if !gotSet[want] {
			t.Errorf("hidden_toolkit_types is missing %q", want)
		}
	}
	if gotSet["jira"] {
		t.Error(`hidden_toolkit_types contains "jira" on python, which the SDK imports`)
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
