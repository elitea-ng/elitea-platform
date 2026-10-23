package runtimecomposition_test

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

func TestPinnedWorkerToolkitCapabilityLoads(t *testing.T) {
	t.Parallel()

	rust, err := runtimecomposition.LoadPinnedWorkerToolkitCapability("rust")
	if err != nil {
		t.Fatalf("load rust capability: %v", err)
	}
	if rust.Implementation() != "rust" || len(rust.SupportedNames()) != 49 {
		t.Errorf("rust implementation=%q supported=%d, want rust and 49",
			rust.Implementation(), len(rust.SupportedNames()))
	}
	// The Python worker was removed: its name, like any other, is refused.
	for _, implementation := range []string{"node", "python"} {
		if _, err := runtimecomposition.LoadPinnedWorkerToolkitCapability(implementation); err == nil {
			t.Errorf("implementation %q was accepted", implementation)
		}
	}
}

func TestWorkerToolkitCapabilityVerdicts(t *testing.T) {
	t.Parallel()

	rust, err := runtimecomposition.LoadPinnedWorkerToolkitCapability("rust")
	if err != nil {
		t.Fatalf("load rust capability: %v", err)
	}
	// The rust answer keys on the TYPE, and its arm is the string "k8s", which
	// no platform type equals.
	if supported, _ := rust.SupportsToolkitType("kubernetes", "k8s"); supported {
		t.Error("the native worker claims kubernetes, whose match arm is \"k8s\"")
	}
	if supported, _ := rust.SupportsToolkitType("github", "github"); !supported {
		t.Error("github has a native family and was refused")
	}
	// pptx joined with ADR-0032 P3: the Office family runs in the Rust worker.
	if supported, reason := rust.SupportsToolkitType("pptx", "pptx"); !supported {
		t.Errorf("pptx has a native family and was refused: %s", reason)
	}
	// docx and xlsx joined with ADR-0032 P4 and have no SDK counterpart: the
	// Rust worker serves them whatever import key (none) the catalogue gives.
	for _, toolkitType := range []string{"docx", "xlsx"} {
		if supported, reason := rust.SupportsToolkitType(toolkitType, ""); !supported {
			t.Errorf("%s has a native family and was refused: %s", toolkitType, reason)
		}
	}
	if supported, reason := rust.SupportsToolkitType("mcp", ""); !supported {
		t.Errorf("direct MCP runtime was refused: %s", reason)
	}
	if supported, reason := rust.SupportsToolkitType("localgit", "localgit"); supported ||
		!strings.Contains(reason, "localgit") {
		t.Errorf("localgit supported=%v reason=%q", supported, reason)
	}
	// UI-DC-1: the reason is a sentence for a person, with no internal terms.
	if _, reason := rust.SupportsToolkitType("example_unported", "example_unported"); reason !=
		"This deployment's agent worker does not support the example_unported toolkit." {
		t.Errorf("example_unported reason = %q", reason)
	}

	// A nil projection supports everything: a deployment that has not stated
	// its worker keeps the catalogue it had before this projection existed.
	var absent *runtimecomposition.WorkerToolkitCapability
	if supported, reason := absent.SupportsToolkitType("slack", "slack"); !supported || reason != "" {
		t.Errorf("a nil projection answered supported=%v reason=%q", supported, reason)
	}
	if absent.Implementation() != "" || absent.SupportedNames() != nil {
		t.Error("a nil projection answered with content")
	}
}

func TestWorkerToolkitCapabilityRefusesBadDocuments(t *testing.T) {
	t.Parallel()

	for name, document := range map[string]string{
		"empty":         "",
		"not json":      "{",
		"unknown field": `{"schema_version":"elitea.worker-toolkit-capability.v1","x":1}`,
		"wrong version": `{"schema_version":"other","implementation":"rust",` +
			`"source":"s","supported_tool_types":["a"]}`,
		"unsorted names": `{"schema_version":"elitea.worker-toolkit-capability.v1",` +
			`"implementation":"rust","source":"s","supported_tool_types":["b","a"]}`,
		"empty allow list": `{"schema_version":"elitea.worker-toolkit-capability.v1",` +
			`"implementation":"rust","source":"s","supported_tool_types":[]}`,
		"tools of a type it does not serve": `{"schema_version":"elitea.worker-toolkit-capability.v1",` +
			`"implementation":"rust","source":"s","supported_tool_types":["a"],"supported_tools":{"b":["x"]}}`,
		"an empty tool list": `{"schema_version":"elitea.worker-toolkit-capability.v1",` +
			`"implementation":"rust","source":"s","supported_tool_types":["a"],"supported_tools":{"a":[]}}`,
		"unsorted tools": `{"schema_version":"elitea.worker-toolkit-capability.v1",` +
			`"implementation":"rust","source":"s","supported_tool_types":["a"],"supported_tools":{"a":["y","x"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runtimecomposition.LoadWorkerToolkitCapability("rust", []byte(document))
			if !errors.Is(err, runtimecomposition.ErrWorkerToolkitCapabilityInvalid) {
				t.Errorf("err=%v, want ErrWorkerToolkitCapabilityInvalid", err)
			}
		})
	}
}

// A partial Rust family serves a subset of its SDK type's tools (ADR-0027);
// a full one serves them all.
func TestWorkerToolkitCapabilitySupportsTool(t *testing.T) {
	t.Parallel()
	rust, err := runtimecomposition.LoadWorkerToolkitCapability("rust", []byte(
		`{"schema_version":"elitea.worker-toolkit-capability.v1","implementation":"rust","source":"s",`+
			`"supported_tool_types":["github","slack"],"supported_tools":{"github":["get_issue","read_file"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		toolkitType, tool string
		want              bool
	}{
		{"github", "read_file", true},
		{"github", "create_pull_request", false},
		{"slack", "send_message", true},
	} {
		got, reason := rust.SupportsTool(tc.toolkitType, tc.tool)
		if got != tc.want || (got == (reason != "")) {
			t.Errorf("%s/%s = %v %q, want %v", tc.toolkitType, tc.tool, got, reason, tc.want)
		}
	}
	if _, reason := rust.SupportsTool("github", "create_pull_request"); reason !=
		"This deployment's agent worker does not support the create_pull_request tool of the github toolkit." {
		t.Errorf("reason = %q", reason)
	}
	var none *runtimecomposition.WorkerToolkitCapability
	if ok, _ := none.SupportsTool("github", "anything"); !ok {
		t.Error("a nil projection must serve everything")
	}
}

// The Rust supported set is DERIVED, not declared. materialize.rs states it
// three times — a prefilter, and one match arm list per half of the alphabet —
// and the committed snapshot is compared with what that file actually holds.
// A family added, renamed or removed in Rust fails here.
func TestRustCapabilitySnapshotMatchesTheRustSource(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)
	source, err := os.ReadFile(filepath.Join(
		root, "libs", "rust", "agent-runtime", "src", "toolkits", "materialize.rs",
	))
	if err != nil {
		t.Fatalf("read materialize.rs: %v", err)
	}

	found := map[string]struct{}{}
	dispatch := materializeDispatchBodies(string(source))
	// The match arms of materialize_a_to_k and materialize_p_to_z. They are
	// indented eight spaces inside the `match tool_type {` of each function.
	for _, match := range regexp.MustCompile(`(?m)^ {8}"([a-z0-9_]+)" =>`).
		FindAllStringSubmatch(dispatch, -1) {
		found[match[1]] = struct{}{}
	}
	// The two families materialize() handles itself, because they return a
	// delegated authorization catalogue rather than a bare toolset.
	for _, match := range regexp.MustCompile(`reference\.tool_type\(\) == "([a-z0-9_]+)"`).
		FindAllStringSubmatch(dispatch, -1) {
		found[match[1]] = struct{}{}
	}
	// MCP uses the direct runtime branch, outside configured families.
	direct, err := os.ReadFile(filepath.Join(root, "services", "elitea-worker-rust", "src", "toolkits", "direct_runtime.rs"))
	if err != nil {
		t.Fatalf("read direct_runtime.rs: %v", err)
	}
	if !strings.Contains(string(direct), "FrozenToolKind::Mcp => {") ||
		!strings.Contains(string(direct), "materialize_mcp_toolsets_with_tokens_and_authorization(") {
		t.Fatal("direct MCP materialization is absent; review the runtime capability")
	}
	found["mcp"] = struct{}{}
	// mcp_config (the saved pre-built MCP definition Main resolves to an HTTP
	// endpoint at claim time) is classified as MCP by the snapshot and served
	// by the same direct MCP branch.
	snapshot, err := os.ReadFile(filepath.Join(
		root, "libs", "rust", "agent-runtime", "src", "toolkits", "snapshot.rs",
	))
	if err != nil {
		t.Fatalf("read snapshot.rs: %v", err)
	}
	if !strings.Contains(string(snapshot), `matches!(tool_type, "mcp" | "mcp_config")`) {
		t.Fatal("snapshot.rs no longer classifies mcp_config as MCP; review the runtime capability")
	}
	found["mcp_config"] = struct{}{}
	if len(found) < 20 {
		t.Fatalf("extracted only %d families from materialize.rs; the extraction "+
			"stopped matching, so this test would prove nothing", len(found))
	}
	extracted := make([]string, 0, len(found))
	for name := range found {
		extracted = append(extracted, name)
	}
	sort.Strings(extracted)

	capability, err := runtimecomposition.LoadPinnedWorkerToolkitCapability("rust")
	if err != nil {
		t.Fatalf("load rust capability: %v", err)
	}
	committed := capability.SupportedNames()
	if strings.Join(committed, ",") != strings.Join(extracted, ",") {
		t.Errorf("the committed rust capability snapshot does not match materialize.rs:\n"+
			" committed=%v\n source   =%v", committed, extracted)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for depth := 0; depth < 8; depth++ {
		if _, err := os.Stat(filepath.Join(directory, "go.work")); err == nil {
			return directory
		}
		directory = filepath.Dir(directory)
	}
	t.Fatal("repository root not found from the test working directory")
	return ""
}

// Real pinned data for memory (served only under the Rust indexing runtime
// with an operator-chosen model) and imagegen (always served).
func TestPinnedCapabilityMemoryAndImagegen(t *testing.T) {
	t.Parallel()

	rust, err := runtimecomposition.LoadPinnedWorkerToolkitCapability("rust")
	if err != nil {
		t.Fatalf("load rust capability: %v", err)
	}
	if ok, reason := rust.SupportsToolkitType("memory", "memory"); !ok {
		t.Errorf("memory has a Rust family over elitea-vector (unit M9): %q", reason)
	}
	// ... but only where the deployment indexes with the Rust runtime, which
	// is what gives the worker a vector store and the claim a vector token.
	if ok, reason := rust.ForIndexingRuntime(runtimecomposition.Config{}).SupportsToolkitType("memory", "memory"); ok || reason == "" {
		t.Errorf("memory must be withheld from a Rust worker without the Rust indexing runtime, with a reason: ok=%v reason=%q", ok, reason)
	}
	rustIndexing := runtimecomposition.Config{Enabled: true, IndexIngestDispatchEnabled: true}
	if ok, reason := rust.ForIndexingRuntime(rustIndexing).SupportsToolkitType("memory", "memory"); ok ||
		!strings.Contains(reason, "Memory needs an embedding model configured by the operator") {
		t.Errorf("memory needs the operator's embedding model: ok=%v reason=%q", ok, reason)
	}
	rustIndexing.MemoryEmbeddingModel = "text-embedding-3-small"
	if ok, reason := rust.ForIndexingRuntime(rustIndexing).SupportsToolkitType("memory", "memory"); !ok {
		t.Errorf("memory is served under the Rust indexing runtime with a model: %q", reason)
	}
	if ok, reason := rust.SupportsToolkitType("imagegen", "imagegen"); !ok {
		t.Errorf("rust must build imagegen: %q", reason)
	}
}

// Absence of a capability projection must not advertise what only the Rust
// worker serves: docx and xlsx (and pptx's Rust-only tools) are offered only
// where the deployment states a Rust worker. The SDK's own types stay.
func TestWithoutACapabilityTheRustOnlyTypesAndToolsAreWithheld(t *testing.T) {
	t.Parallel()

	var absent *runtimecomposition.WorkerToolkitCapability
	rust, err := runtimecomposition.LoadPinnedWorkerToolkitCapability("rust")
	if err != nil {
		t.Fatal(err)
	}
	for _, toolkitType := range []string{"docx", "xlsx"} {
		supported, reason := absent.SupportsToolkitType(toolkitType, "")
		if supported || !strings.Contains(reason, "only the Rust worker serves") {
			t.Errorf("nil: %s supported=%v reason=%q", toolkitType, supported, reason)
		}
		if supported, reason := rust.SupportsToolkitType(toolkitType, ""); !supported {
			t.Errorf("rust withholds %s: %s", toolkitType, reason)
		}
	}
	// pptx's Rust-only tools: withheld on nil, offered on Rust;
	// its SDK tools are offered everywhere.
	for _, tool := range []string{"add_slide", "export_pdf"} {
		if supported, reason := absent.SupportsTool("pptx", tool); supported || reason == "" {
			t.Errorf("nil offers pptx/%s", tool)
		}
		if supported, reason := rust.SupportsTool("pptx", tool); !supported {
			t.Errorf("rust withholds pptx/%s: %s", tool, reason)
		}
	}
	for _, tool := range []string{"fill_template", "translate_presentation"} {
		for name, capability := range map[string]*runtimecomposition.WorkerToolkitCapability{"nil": absent, "rust": rust} {
			if supported, reason := capability.SupportsTool("pptx", tool); !supported {
				t.Errorf("%s withholds the SDK tool pptx/%s: %s", name, tool, reason)
			}
		}
	}
	// The SDK's types are unchanged on nil.
	for _, toolkitType := range []string{"github", "pptx", "artifact"} {
		if supported, _ := absent.SupportsToolkitType(toolkitType, toolkitType); !supported {
			t.Errorf("nil withholds the SDK type %s", toolkitType)
		}
	}
}

// materializeDispatchBodies is the text of the dispatch functions of
// materialize.rs only: every `fn materialize*` body, from its signature to
// the closing brace at column 0. Matching the whole file read the arms of
// `family_tool_catalog` (the per-family tool lists, review C1) and any test
// table as dispatch, so a family whose arm was removed still passed.
func materializeDispatchBodies(source string) string {
	var bodies strings.Builder
	for _, match := range regexp.MustCompile(`(?m)^(?:pub )?(?:async )?fn materialize\w*[\s\S]*?\n\}\n`).
		FindAllString(source, -1) {
		bodies.WriteString(match)
	}
	return bodies.String()
}
