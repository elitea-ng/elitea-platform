package runtimecomposition_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

func TestWorkerImplementationFromEnv(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		value string
		want  string
		fails bool
	}{
		"unset defaults to python": {value: "", want: "python"},
		"python":                   {value: "python", want: "python"},
		"rust":                     {value: "rust", want: "rust"},
		"a typo is refused":        {value: "Rust", fails: true},
		"an unknown worker":        {value: "node", fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			lookup := func(string) (string, bool) { return testCase.value, testCase.value != "" }
			got, err := runtimecomposition.WorkerImplementationFromEnv(lookup)
			if testCase.fails {
				if err == nil {
					t.Errorf("value %q was accepted as %q", testCase.value, got)
				}
				return
			}
			if err != nil || got != testCase.want {
				t.Errorf("got %q err=%v, want %q", got, err, testCase.want)
			}
		})
	}
	if _, err := runtimecomposition.WorkerImplementationFromEnv(nil); err == nil {
		t.Error("a nil lookup was accepted")
	}
}

func TestPinnedWorkerToolkitCapabilityLoads(t *testing.T) {
	t.Parallel()

	python, err := runtimecomposition.LoadPinnedWorkerToolkitCapability("python")
	if err != nil {
		t.Fatalf("load python capability: %v", err)
	}
	if python.Implementation() != "python" {
		t.Errorf("implementation=%q", python.Implementation())
	}
	if len(python.UnsupportedNames()) == 0 {
		t.Error("the python image reports no unsupported toolkit; the measured set is 13")
	}
	if python.SupportedNames() != nil {
		t.Error("the python projection is a deny list and reported an allow list")
	}

	rust, err := runtimecomposition.LoadPinnedWorkerToolkitCapability("rust")
	if err != nil {
		t.Fatalf("load rust capability: %v", err)
	}
	if rust.Implementation() != "rust" || len(rust.SupportedNames()) != 22 {
		t.Errorf("rust implementation=%q supported=%d, want rust and 22",
			rust.Implementation(), len(rust.SupportedNames()))
	}
	if rust.UnsupportedNames() != nil {
		t.Error("the rust projection is an allow list and reported a deny list")
	}

	if _, err := runtimecomposition.LoadPinnedWorkerToolkitCapability("node"); err == nil {
		t.Error("an unknown implementation was accepted")
	}
}

func TestWorkerToolkitCapabilityVerdicts(t *testing.T) {
	t.Parallel()

	python, err := runtimecomposition.LoadPinnedWorkerToolkitCapability("python")
	if err != nil {
		t.Fatalf("load python capability: %v", err)
	}
	// The join runs on the import key, so the type name is irrelevant to the
	// python answer. kubernetes/k8s is the pair that proves it.
	if supported, reason := python.SupportsToolkitType("kubernetes", "k8s"); supported ||
		!strings.Contains(reason, "k8s") {
		t.Errorf("kubernetes supported=%v reason=%q", supported, reason)
	}
	if supported, _ := python.SupportsToolkitType("github", "github"); !supported {
		t.Error("github is not supported by the python image")
	}
	// A runtime toolkit declares no import key and cannot fail to import.
	if supported, _ := python.SupportsToolkitType("artifact", ""); !supported {
		t.Error("a toolkit with no import key was refused")
	}

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
	if supported, reason := rust.SupportsToolkitType("jira", "jira"); supported ||
		!strings.Contains(reason, "jira") {
		t.Errorf("jira supported=%v reason=%q", supported, reason)
	}

	// A nil projection supports everything: a deployment that has not stated
	// its worker keeps the catalogue it had before this projection existed.
	var absent *runtimecomposition.WorkerToolkitCapability
	if supported, reason := absent.SupportsToolkitType("slack", "slack"); !supported || reason != "" {
		t.Errorf("a nil projection answered supported=%v reason=%q", supported, reason)
	}
	if absent.Implementation() != "" || absent.UnsupportedNames() != nil ||
		absent.SupportedNames() != nil {
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
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runtimecomposition.LoadWorkerToolkitCapability("rust", []byte(document))
			if !errors.Is(err, runtimecomposition.ErrWorkerToolkitCapabilityInvalid) {
				t.Errorf("err=%v, want ErrWorkerToolkitCapabilityInvalid", err)
			}
		})
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
		root, "services", "elitea-worker-rust", "src", "toolkits", "materialize.rs",
	))
	if err != nil {
		t.Fatalf("read materialize.rs: %v", err)
	}

	found := map[string]struct{}{}
	// The match arms of materialize_a_to_k and materialize_p_to_z. They are
	// indented eight spaces inside the `match tool_type {` of each function.
	for _, match := range regexp.MustCompile(`(?m)^ {8}"([a-z0-9_]+)" =>`).
		FindAllStringSubmatch(string(source), -1) {
		found[match[1]] = struct{}{}
	}
	// The two families materialize() handles itself, because they return a
	// delegated authorization catalogue rather than a bare toolset.
	for _, match := range regexp.MustCompile(`reference\.tool_type\(\) == "([a-z0-9_]+)"`).
		FindAllStringSubmatch(string(source), -1) {
		found[match[1]] = struct{}{}
	}
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

// The Python snapshot names the SDK revision the toolkit catalogue was
// projected from. Two different revisions would mean the capability answer
// describes a registry the catalogue does not hold.
func TestPythonCapabilitySnapshotNamesTheCataloguedSDKRevision(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)
	raw, err := os.ReadFile(filepath.Join(
		root, "services", "elitea-main", "internal", "runtimecomposition",
		"current_python_worker_toolkit_capability_snapshot.json",
	))
	if err != nil {
		t.Fatalf("read the python capability snapshot: %v", err)
	}
	var document struct {
		SDKRevision string `json:"sdk_revision"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode the python capability snapshot: %v", err)
	}
	catalogue, err := runtimecomposition.LoadPinnedCurrentToolkitCatalogueSnapshot()
	if err != nil {
		t.Fatalf("load the toolkit catalogue: %v", err)
	}
	if document.SDKRevision != catalogue.SDKRevision() {
		t.Errorf("capability revision=%q, catalogue revision=%q",
			document.SDKRevision, catalogue.SDKRevision())
	}
}

// Every import key the python snapshot names must be a key the catalogue knows.
// A key that names nothing is a capability answer about a toolkit that does not
// exist, and it would silently withhold nothing.
func TestPythonCapabilityKeysAllNameACataloguedType(t *testing.T) {
	t.Parallel()

	capability, err := runtimecomposition.LoadPinnedWorkerToolkitCapability("python")
	if err != nil {
		t.Fatalf("load python capability: %v", err)
	}
	catalogue, err := runtimecomposition.LoadPinnedCurrentToolkitCatalogueSnapshot()
	if err != nil {
		t.Fatalf("load the toolkit catalogue: %v", err)
	}
	known := map[string]string{}
	for _, toolkitType := range catalogue.ToolkitTypes() {
		if key, found := catalogue.ToolkitImportKey(toolkitType); found && key != "" {
			known[key] = toolkitType
		}
	}
	for _, key := range capability.UnsupportedNames() {
		if _, found := known[key]; !found {
			t.Errorf("the python image reports import key %q, which no catalogued type declares", key)
		}
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
