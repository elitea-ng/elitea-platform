package mcpregistry

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// The normalisation is the join between two independently-written strings: an
// operator's display name in the catalogue and a toolkit's `type` in the
// database. If the two sides ever derive different keys the lookup silently
// finds nothing, and a pre-built toolkit quietly loses its credentials. These
// cases are transcribed from `normalize_mcp_toolkit_name`'s own docstring.
func TestNormalizeCatalogueKeyMatchesTheProducer(t *testing.T) {
	for _, testCase := range []struct{ input, want string }{
		{"mcp_epam_presales", "epam_presales"},
		{"Epam Presales", "epam_presales"},
		{"EPAM PRESALES", "epam_presales"},
		{"GitHub Copilot", "github_copilot"},
		// SURROUNDING SPACES SURVIVE AS UNDERSCORES, and that is the legacy runtime's
		// behaviour, not a defect here. Python runs
		// `.lower().replace(" ", "_").strip()` in that order, so by the time
		// `strip()` runs there is no whitespace left to strip — the spaces are
		// already underscores. Both stacks therefore derive the same odd key
		// from the same padded name, which is the only property that matters:
		// the producer (`indexer_mcp_prebuilt_config.py`) normalises the same
		// way, so the two sides agree. "Correcting" this would make the Go key
		// differ from every key the legacy runtime ever wrote.
		{"  Epam Presales  ", "__epam_presales__"},
		// The prefix is stripped AFTER lower-casing and space substitution, so
		// a display name that spells the prefix with a space reduces the same
		// way a toolkit type does.
		{"MCP Epam Presales", "epam_presales"},
		{"", ""},
		// Only ONE leading prefix is removed, which is Python's
		// `str[4:]` on a single startswith check.
		{"mcp_mcp_thing", "mcp_thing"},
	} {
		require.Equal(t, testCase.want, NormalizeCatalogueKey(testCase.input),
			"input %q", testCase.input)
	}
}

// The gate is on the RAW toolkit type, which is the legacy
// `toolkit_type.startswith('mcp_')`. A normalised key carries no prefix and
// must NOT be treated as a pre-built toolkit, or every remote MCP toolkit would
// start acquiring catalogue credentials it was never configured with.
func TestIsPrebuiltToolkitTypeTestsTheRawType(t *testing.T) {
	require.True(t, IsPrebuiltToolkitType("mcp_github_copilot"))
	require.False(t, IsPrebuiltToolkitType("github_copilot"))
	require.False(t, IsPrebuiltToolkitType("sharepoint"))
	require.False(t, IsPrebuiltToolkitType(""))
}

func catalogueEntry() *PrebuiltServer {
	return &PrebuiltServer{
		Key:             "github_copilot",
		DisplayName:     "GitHub Copilot",
		ServerURL:       "https://api.githubcopilot.com/mcp/",
		BaseURL:         "https://api.githubcopilot.com",
		ClientID:        "catalogue-client",
		ClientSecretRef: "mcp_prebuilt__github_copilot__client_secret",
		TimeoutSeconds:  30,
		Headers:         map[string]string{"X-Catalogue": "yes"},
		Enabled:         true,
	}
}

func constantSecret(value string) ResolvedSecretLookup {
	return func(string) (string, error) { return value, nil }
}

// The whole point of the priority order: a value the caller supplied survives.
// An operator's platform-wide default must never overwrite what a project
// configured for itself.
func TestResolveNeverOverwritesACallerSuppliedValue(t *testing.T) {
	settings := map[string]any{
		"url":           "https://project-own.example.com/mcp",
		"client_id":     "project-client",
		"client_secret": "project-secret",
		"timeout":       5,
		"headers":       map[string]any{"X-Project": "yes"},
		"base_url":      "https://project-own.example.com",
	}

	resolved := Resolve(settings, "mcp_github_copilot", catalogueEntry(), constantSecret("catalogue-secret"))

	require.Equal(t, "https://project-own.example.com/mcp", resolved["url"])
	require.Equal(t, "project-client", resolved["client_id"])
	require.Equal(t, "project-secret", resolved["client_secret"])
	require.Equal(t, 5, resolved["timeout"])
	require.Equal(t, "https://project-own.example.com", resolved["base_url"])
	require.Equal(t, map[string]any{"X-Project": "yes"}, resolved["headers"])
}

func TestResolveFillsOnlyTheFieldsLeftEmpty(t *testing.T) {
	resolved := Resolve(
		map[string]any{"client_id": "project-client"},
		"mcp_github_copilot", catalogueEntry(), constantSecret("catalogue-secret"))

	require.Equal(t, "project-client", resolved["client_id"], "supplied value must survive")
	require.Equal(t, "https://api.githubcopilot.com/mcp/", resolved["url"])
	require.Equal(t, "https://api.githubcopilot.com", resolved["base_url"])
	require.Equal(t, 30, resolved["timeout"])
	require.Equal(t, "catalogue-secret", resolved["client_secret"])
	require.Equal(t, map[string]any{"X-Catalogue": "yes"}, resolved["headers"])
}

// The legacy runtime's test is Python truthiness, so an EMPTY string counts as
// "not supplied". The case that actually occurs is a form posting `"url": ""`
// for an untouched field; treating that as supplied would leave the URL empty
// with the catalogue entry sitting right there unused.
func TestResolveTreatsEmptyValuesAsUnsupplied(t *testing.T) {
	resolved := Resolve(
		map[string]any{"url": "", "client_id": "   ", "timeout": 0, "headers": map[string]any{}},
		"mcp_github_copilot", catalogueEntry(), constantSecret("catalogue-secret"))

	require.Equal(t, "https://api.githubcopilot.com/mcp/", resolved["url"])
	require.Equal(t, "catalogue-client", resolved["client_id"])
	require.Equal(t, 30, resolved["timeout"])
	require.Equal(t, map[string]any{"X-Catalogue": "yes"}, resolved["headers"])
}

// `ssl_verify` is the legacy runtime's seventh fillable field and is
// deliberately NOT one here: this service verifies TLS unconditionally, so a
// catalogue value for it would be a setting nothing honours — a control that
// reads as working and is not one.
func TestResolveNeverFillsSSLVerify(t *testing.T) {
	entry := catalogueEntry()
	resolved := Resolve(map[string]any{}, "mcp_github_copilot", entry, constantSecret("s"))
	_, present := resolved["ssl_verify"]
	require.False(t, present, "ssl_verify must not be injected from the catalogue")
}

func TestResolveDeclinesWhenTheToolkitIsNotPrebuilt(t *testing.T) {
	settings := map[string]any{"url": ""}
	resolved := Resolve(settings, "github_copilot", catalogueEntry(), constantSecret("s"))
	require.Equal(t, "", resolved["url"], "a non-prebuilt toolkit must be untouched")
}

// A disabled entry is stored so an operator can withdraw it without losing the
// sealed secret. Withdrawn means withdrawn: it must not resolve.
func TestResolveDeclinesADisabledEntry(t *testing.T) {
	entry := catalogueEntry()
	entry.Enabled = false
	resolved := Resolve(map[string]any{"url": ""}, "mcp_github_copilot", entry, constantSecret("s"))
	require.Equal(t, "", resolved["url"])
}

func TestResolveDeclinesANilEntry(t *testing.T) {
	settings := map[string]any{"url": ""}
	require.Equal(t, settings, Resolve(settings, "mcp_github_copilot", nil, constantSecret("s")))
}

// A vault miss must leave the field ABSENT rather than write an empty string.
// An empty client_secret is a value a downstream OAuth exchange would send and
// be rejected for; an absent one lets the caller's own error path say the
// credential is missing.
func TestResolveLeavesClientSecretAbsentWhenTheVaultFails(t *testing.T) {
	failing := func(string) (string, error) { return "", errors.New("vault unavailable") }
	resolved := Resolve(map[string]any{}, "mcp_github_copilot", catalogueEntry(), failing)
	_, present := resolved["client_secret"]
	require.False(t, present, "a vault failure must not write an empty client_secret")
}

func TestResolveLeavesClientSecretAbsentWithNoLookup(t *testing.T) {
	resolved := Resolve(map[string]any{}, "mcp_github_copilot", catalogueEntry(), nil)
	_, present := resolved["client_secret"]
	require.False(t, present)
}

// The legacy runtime copies the dict (`result = dict(raw_data)`) and so does
// this. A stored toolkit configuration must never be rewritten as a side effect
// of a discovery.
func TestResolveDoesNotMutateTheCallersSettings(t *testing.T) {
	settings := map[string]any{"url": ""}
	resolved := Resolve(settings, "mcp_github_copilot", catalogueEntry(), constantSecret("s"))

	require.Equal(t, "", settings["url"], "the caller's map must be untouched")
	require.NotEqual(t, "", resolved["url"])
	_, injected := settings["client_secret"]
	require.False(t, injected, "the caller's map must not gain a secret")
}
