package toolkitcatalogue

// The capability verdicts, and the DRIFT CHECK that keeps the transcribed list
// honest.
//
// capability.go carries a list copied out of another tree: the Rust worker's
// materialiser. A copy with no check is a copy that goes stale silently, and a
// stale capability verdict is worse than none — it tells an operator a type is
// carried when it is not.
//
// The check fails LOUDLY when it cannot find its subject. A check that
// reads no file and reports success is the failure mode this repository has
// shipped before.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

// The Rust list must equal the tool types `materialize.rs` actually dispatches.
//
// It reads the DISPATCH, not the directory listing. A family that is complete
// on disk but reached by no arm (as `aha` was before its arm landed) is skipped
// at run time like any unsupported type, and a list built from the directory
// would claim support the runtime does not give.
func TestRustNativeToolTypesMatchTheMaterialiser(t *testing.T) {
	t.Parallel()

	path := repoRelative(t, "libs", "rust", "agent-runtime", "src", "toolkits", "materialize.rs")
	raw, err := os.ReadFile(path)
	require.NoError(t, err,
		"the Rust materialiser must be readable: capability.go transcribes its dispatch")

	source := materializeDispatchBodies(string(raw))
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

	github := source.ToolkitCapability("github")
	require.True(t, github.Rust)
	require.Equal(t, VerdictSupported, github.Verdict)

	jira := source.ToolkitCapability("jira")
	require.True(t, jira.Rust)
	require.Equal(t, VerdictSupported, jira.Verdict)

	slack := source.ToolkitCapability("slack")
	require.True(t, slack.Rust)
	require.Equal(t, VerdictSupported, slack.Verdict)

	// imagegen (#864) is carried by the Rust worker's native family.
	imagegen := source.ToolkitCapability("imagegen")
	require.True(t, imagegen.Rust)
	require.Equal(t, VerdictSupported, imagegen.Verdict)

	neither := source.ToolkitCapability("unlisted_demo")
	require.False(t, neither.Rust)
	require.Equal(t, VerdictUnverified, neither.Verdict)

	// Every verdict carries a sentence naming the runtime fact behind it. A
	// bare boolean would send the operator to read Go source.
	for _, capability := range []Capability{github, jira, slack, neither} {
		require.NotEmpty(t, capability.Reason)
	}
	require.Contains(t, neither.Reason, "not verified",
		"an unverified type must not read as a refusal: it may still work")

	// `aha` was built in the Rust tree and reached by no dispatch arm; it is
	// dispatched now (a partial family without attach_file).
	require.True(t, source.ToolkitCapability("aha").Rust)
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
