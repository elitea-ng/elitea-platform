package toolkitcatalogue

// The capability verdicts, and the DRIFT CHECK that keeps the two transcribed
// lists honest.
//
// capability.go carries two lists copied out of other trees: the Python
// worker's pinned SDK lock and the Rust worker's materialiser. A copy with no
// check is a copy that goes stale silently, and a stale capability verdict is
// worse than none — it tells an operator a type is carried when it is not.
//
// Both checks fail LOUDLY when they cannot find their subject. A check that
// reads no file and reports success is the failure mode this repository has
// shipped before.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// repoRelative resolves a path against the repository root, four levels above
// this package.
func repoRelative(t *testing.T, parts ...string) string {
	t.Helper()
	root := filepath.Join("..", "..", "..", "..", "..")
	return filepath.Join(append([]string{root}, parts...)...)
}

// The Python list must equal the lock file's own
// `required_sdk_tool_import_keys`. Change the image's extras and this fails,
// naming the file.
func TestPythonVerifiedImportKeysMatchTheWorkerLock(t *testing.T) {
	t.Parallel()

	path := repoRelative(t, "services", "elitea-worker-python", "elitea-sdk.lock.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err,
		"the pinned worker lock must be readable: capability.go transcribes it, and a "+
			"transcription nothing checks goes stale without a symptom")

	var lock struct {
		IndexingCapabilityProfile struct {
			RequiredSDKToolImportKeys []string `json:"required_sdk_tool_import_keys"`
		} `json:"indexing_capability_profile"`
	}
	require.NoError(t, json.Unmarshal(raw, &lock))

	keys := lock.IndexingCapabilityProfile.RequiredSDKToolImportKeys
	require.NotEmpty(t, keys, "the lock declared no SDK tool import keys; the premise of this check is gone")

	want := append([]string(nil), keys...)
	got := append([]string(nil), pythonVerifiedImportKeys...)
	sort.Strings(want)
	sort.Strings(got)
	require.Equal(t, want, got,
		"pythonVerifiedImportKeys has drifted from %s. Update the list in capability.go.", path)
}

// The Rust list must equal the tool types `materialize.rs` actually dispatches.
//
// It reads the DISPATCH, not the directory listing. A family that is complete
// on disk but reached by no arm (`aha`, today) is skipped at run time like any
// unsupported type, and a list built from the directory would claim support the
// runtime does not give.
func TestRustNativeToolTypesMatchTheMaterialiser(t *testing.T) {
	t.Parallel()

	path := repoRelative(t, "services", "elitea-worker-rust", "src", "toolkits", "materialize.rs")
	raw, err := os.ReadFile(path)
	require.NoError(t, err,
		"the Rust materialiser must be readable: capability.go transcribes its dispatch")

	source := string(raw)
	dispatched := map[string]bool{}
	for _, match := range regexp.MustCompile(`"([a-z0-9_]+)" =>`).FindAllStringSubmatch(source, -1) {
		dispatched[match[1]] = true
	}
	for _, match := range regexp.MustCompile(
		`tool_type\(\) == "([a-z0-9_]+)"`).FindAllStringSubmatch(source, -1) {
		dispatched[match[1]] = true
	}
	require.GreaterOrEqual(t, len(dispatched), 15,
		"found only %d dispatched tool types in %s. The match arms were probably restructured; "+
			"read the file and fix this check rather than lowering the floor.", len(dispatched), path)

	want := make([]string, 0, len(dispatched))
	for toolType := range dispatched {
		want = append(want, toolType)
	}
	got := append([]string(nil), rustNativeToolTypes...)
	sort.Strings(want)
	sort.Strings(got)
	require.Equal(t, want, got,
		"rustNativeToolTypes has drifted from %s. Update the list in capability.go.", path)
}

// The naming trap: the schema title is `kubernetes` and both worker spellings
// are `k8s`. A capability list keyed on the import key silently never matches
// the catalogue, so the page would report every type unverified.
func TestKubernetesIsKeyedOnTheSchemaTitle(t *testing.T) {
	t.Parallel()

	capability := PinnedWorkerCapabilities{}.ToolkitCapability("kubernetes")
	require.True(t, capability.Rust)
	require.Equal(t, VerdictSupported, capability.Verdict)

	// The import key itself is NOT a catalogue type, so it resolves unverified.
	require.Equal(t, VerdictUnverified, PinnedWorkerCapabilities{}.ToolkitCapability("k8s").Verdict)

	// `ado` is an SDK base family with no schema of its own. It must not appear
	// as a catalogue type.
	require.NotContains(t, CapabilityTypes(), "ado")
}

func TestCapabilityVerdicts(t *testing.T) {
	t.Parallel()

	source := PinnedWorkerCapabilities{}

	both := source.ToolkitCapability("github")
	require.True(t, both.Python)
	require.True(t, both.Rust)
	require.Equal(t, VerdictSupported, both.Verdict)

	pythonOnly := source.ToolkitCapability("jira")
	require.True(t, pythonOnly.Python)
	require.False(t, pythonOnly.Rust)
	require.Equal(t, VerdictSupported, pythonOnly.Verdict)

	rustOnly := source.ToolkitCapability("slack")
	require.False(t, rustOnly.Python)
	require.True(t, rustOnly.Rust)
	require.Equal(t, VerdictSupported, rustOnly.Verdict)

	neither := source.ToolkitCapability("pptx")
	require.False(t, neither.Python)
	require.False(t, neither.Rust)
	require.Equal(t, VerdictUnverified, neither.Verdict)

	// Every verdict carries a sentence naming the runtime fact behind it. A
	// bare boolean would send the operator to read Go source.
	for _, capability := range []Capability{both, pythonOnly, rustOnly, neither} {
		require.NotEmpty(t, capability.Reason)
	}
	require.Contains(t, neither.Reason, "not verified",
		"an unverified type must not read as a refusal: it may still work")

	// `aha` is built in the Rust tree and reached by no dispatch arm.
	require.False(t, source.ToolkitCapability("aha").Rust)
}

func TestCapabilityTypesIsSortedAndDeduplicated(t *testing.T) {
	t.Parallel()

	types := CapabilityTypes()
	require.NotEmpty(t, types)
	require.True(t, sort.StringsAreSorted(types))
	seen := map[string]bool{}
	for _, toolkitType := range types {
		require.False(t, seen[toolkitType], "duplicate %q", toolkitType)
		seen[toolkitType] = true
	}
	require.Contains(t, types, "kubernetes")
	require.Contains(t, types, "github")
}
