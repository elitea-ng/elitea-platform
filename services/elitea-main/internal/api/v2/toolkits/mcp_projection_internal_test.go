package toolkits

// The MCP projection, pinned against the two consumers that decide whether the
// browser can create an MCP toolkit at all:
//
//   - `useGetCurrentMCPSchemas.hooks.ts`'s `isMcpFlavouredKey` — a catalogue
//     entry reaches the MCP page only when its KEY is `mcp` or ends in `mcp`;
//   - `ToolBase.render.tsx` — the "Load Tools" action and the MCP authorisation
//     pane appear only when `schema.title === 'mcp'`.
//
// Both are string comparisons in another repository half. A test that only
// asserted "some MCP entry is served" would pass while the page stayed empty.

import (
	"context"
	"errors"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

type stubCatalogue struct {
	entries []mcpregistry.PrebuiltServer
	err     error
	calls   int
}

func (s *stubCatalogue) List(context.Context) ([]mcpregistry.PrebuiltServer, error) {
	s.calls++
	return s.entries, s.err
}

func projectedByType(projected []ProjectedToolkitType, toolkitType string) (ProjectedToolkitType, bool) {
	for _, entry := range projected {
		if entry.Type == toolkitType {
			return entry, true
		}
	}
	return ProjectedToolkitType{}, false
}

func TestRemoteMCPTypeCarriesTheThreeStringsTheWebClientMatchesOn(t *testing.T) {
	projected := remoteMCPType()

	if projected.Type != "mcp" {
		t.Fatalf("the catalogue key must be `mcp`: got %q", projected.Type)
	}
	if got := projected.Schema["title"]; got != "mcp" {
		t.Fatalf("ToolBase keys the whole Remote MCP form off title === 'mcp': got %v", got)
	}
	metadata, _ := projected.Schema["metadata"].(map[string]any)
	if metadata["label"] != "Remote MCP" {
		t.Fatalf("the chooser tile's name: got %v", metadata["label"])
	}
}

func TestRemoteMCPTypeRestatesTheSDKSettingsSchema(t *testing.T) {
	properties, _ := remoteMCPType().Schema["properties"].(map[string]any)

	// The field set of elitea_sdk McpToolkit.toolkit_config_schema at the
	// pinned revision. A field this port dropped is a control the user cannot
	// reach; a field it invented is a control nothing reads.
	want := []string{
		"url", "headers", "client_id", "client_secret", "scopes",
		"timeout", "selected_tools", "enable_caching", "cache_ttl",
	}
	for _, field := range want {
		if _, found := properties[field]; !found {
			t.Errorf("settings field %q is missing; served fields are %v", field, keysOfAny(properties))
		}
	}
	if len(properties) != len(want) {
		t.Errorf("served %d settings fields, the SDK declares %d: %v",
			len(properties), len(want), keysOfAny(properties))
	}
}

// `transport` and `ssl_verify` are deliberately absent. Both would be controls
// nothing honours: the discoverer and the native worker each build streamable
// HTTP unconditionally and verify TLS unconditionally.
func TestRemoteMCPTypeOffersNoControlThatNothingHonours(t *testing.T) {
	properties, _ := remoteMCPType().Schema["properties"].(map[string]any)
	for _, field := range []string{"transport", "ssl_verify"} {
		if _, found := properties[field]; found {
			t.Errorf("settings field %q is served but no runtime reads it", field)
		}
	}
}

func TestRemoteMCPTypeRequiresAURL(t *testing.T) {
	required, _ := remoteMCPType().Schema["required"].([]any)
	if len(required) != 1 || required[0] != "url" {
		t.Fatalf("a remote server with no URL cannot be dialled: required = %v", required)
	}
}

func TestRemoteMCPTypeDeclaresAnEmptyArgsSchemasBlock(t *testing.T) {
	properties, _ := remoteMCPType().Schema["properties"].(map[string]any)
	selected, _ := properties["selected_tools"].(map[string]any)
	argsSchemas, found := selected["args_schemas"]
	if !found {
		t.Fatal("the web form indexes properties.selected_tools.args_schemas directly")
	}
	if typed, ok := argsSchemas.(map[string]any); !ok || len(typed) != 0 {
		t.Fatalf("a remote server's tools are discovered, never declared: %v", argsSchemas)
	}
}

// MEASURED IN A BROWSER, and the reason this test exists.
//
// `ToolBase.render.tsx`'s `resolveAvailableTools` reads
// `selected_tools.items.enum` BEFORE it falls back to
// `settings.available_mcp_tools`, and it uses `??`, so an enum that is present
// and empty wins. A discovery against a live MCP server then succeeds, writes
// the published tool names into `available_mcp_tools`, and the chip picker
// stays empty. Declaring the enum is worse than omitting it.
func TestNoMCPTypeDeclaresAToolEnumThatWouldBeatTheDiscoveredList(t *testing.T) {
	cases := map[string]ProjectedToolkitType{
		"remote": remoteMCPType(),
		"prebuilt": prebuiltMCPType(mcpregistry.PrebuiltServer{
			Key: "context7", DisplayName: "Context7", Enabled: true,
		}),
	}
	for name, projected := range cases {
		t.Run(name, func(t *testing.T) {
			properties, _ := projected.Schema["properties"].(map[string]any)
			selected, _ := properties["selected_tools"].(map[string]any)
			items, _ := selected["items"].(map[string]any)
			if items["type"] != "string" {
				t.Fatalf("selected_tools must stay a list of names: %v", items)
			}
			if _, declared := items["enum"]; declared {
				t.Fatalf("an items.enum beats the discovered tool list: %v", items)
			}
		})
	}
}

func TestTheRemoteMCPTypeIsServedWithoutADatabase(t *testing.T) {
	projected, err := remoteMCPProjection{}.ProjectToolkitTypes(context.Background())
	if err != nil {
		t.Fatalf("a deployment with no pool must still offer Remote MCP: %v", err)
	}
	if len(projected) != 1 || projected[0].Type != "mcp" {
		t.Fatalf("want the Remote MCP type alone, got %v", projected)
	}
}

// THE REASON THE TWO SOURCES ARE SEPARATE. A source that fails contributes
// nothing, so a Remote MCP type folded into the catalogue source would be
// withdrawn whenever `elitea_mcp.prebuilt_servers` could not be read — and
// closing the browser's only MCP create surface over a platform-wide table that
// most deployments leave empty is a much worse answer than one fewer tile.
func TestAnUnreadableCatalogueDoesNotWithdrawTheRemoteMCPType(t *testing.T) {
	failing := &prebuiltMCPProjection{catalogue: &stubCatalogue{err: errors.New("relation does not exist")}}
	handler := NewHandlerWithRepo(nil, WithToolkitTypeProjections(remoteMCPProjection{}, failing))

	merged := mergeProjectedTypes(nil, handler.projectedToolkitTypes(context.Background())...)

	if _, found := merged["mcp"]; !found {
		t.Fatalf("an unreadable catalogue withdrew the Remote MCP type: %v", keysOf(merged))
	}
}

func TestPrebuiltProjectionWithNoPoolContributesNothingAndDoesNotFail(t *testing.T) {
	projected, err := newPrebuiltMCPProjection(nil).ProjectToolkitTypes(context.Background())
	if err != nil {
		t.Fatalf("no pool is not a failed read: %v", err)
	}
	if len(projected) != 0 {
		t.Fatalf("want nothing, got %v", projected)
	}
}

func TestMCPProjectionTable(t *testing.T) {
	cases := []struct {
		name      string
		entries   []mcpregistry.PrebuiltServer
		wantTypes []string
	}{
		{
			name:      "an empty catalogue contributes nothing",
			entries:   nil,
			wantTypes: nil,
		},
		{
			name: "an enabled row becomes an mcp_ type",
			entries: []mcpregistry.PrebuiltServer{
				{Key: "context7", DisplayName: "Context7", ServerURL: "https://ctx7.example/mcp", Enabled: true},
			},
			wantTypes: []string{"mcp_context7"},
		},
		{
			name: "a disabled row is not offered",
			entries: []mcpregistry.PrebuiltServer{
				{Key: "context7", DisplayName: "Context7", Enabled: false},
				{Key: "tavily", DisplayName: "Tavily", Enabled: true},
			},
			wantTypes: []string{"mcp_tavily"},
		},
		{
			name: "a keyless row is not offered",
			entries: []mcpregistry.PrebuiltServer{
				{Key: "", DisplayName: "Nameless", Enabled: true},
			},
			wantTypes: nil,
		},
		{
			name: "several rows are all offered",
			entries: []mcpregistry.PrebuiltServer{
				{Key: "epam_query", DisplayName: "Epam Query", Enabled: true},
				{Key: "miro", DisplayName: "Miro", Enabled: true},
			},
			wantTypes: []string{"mcp_epam_query", "mcp_miro"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			catalogue := &stubCatalogue{entries: testCase.entries}
			projection := &prebuiltMCPProjection{catalogue: catalogue}

			projected, err := projection.ProjectToolkitTypes(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := make([]string, 0, len(projected))
			for _, entry := range projected {
				got = append(got, entry.Type)
			}
			sortStrings(got)
			want := append([]string(nil), testCase.wantTypes...)
			sortStrings(want)
			if len(got) != len(want) {
				t.Fatalf("types = %v, want %v", got, want)
			}
			for index := range want {
				if got[index] != want[index] {
					t.Fatalf("types = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestPrebuiltTypeNameMatchesTheResolverThatFillsItsSettings(t *testing.T) {
	entry := mcpregistry.PrebuiltServer{Key: "epam_query", DisplayName: "Epam Query", Enabled: true}
	projected := prebuiltMCPType(entry)

	if !mcpregistry.IsPrebuiltToolkitType(projected.Type) {
		t.Fatalf("%q does not carry the prefix Resolve gates on", projected.Type)
	}
	if got := mcpregistry.NormalizeCatalogueKey(projected.Type); got != entry.Key {
		t.Fatalf("the served type normalises to %q, the catalogue row is keyed %q", got, entry.Key)
	}
}

func TestPrebuiltTypeMetadataAndDefaults(t *testing.T) {
	entry := mcpregistry.PrebuiltServer{
		Key: "context7", DisplayName: "Context7",
		ServerURL: "https://ctx7.example/mcp", Enabled: true,
	}
	projected := prebuiltMCPType(entry)

	metadata, _ := projected.Schema["metadata"].(map[string]any)
	if metadata["label"] != "Context7" {
		t.Errorf("label = %v, want the operator's display name", metadata["label"])
	}
	categories, _ := metadata["categories"].([]any)
	if len(categories) != 1 || categories[0] != "mcp" {
		t.Errorf("a pre-built server belongs under the MCP heading: %v", categories)
	}
	if metadata["mcp_server_name"] != "context7" {
		t.Errorf("mcp_server_name = %v", metadata["mcp_server_name"])
	}

	properties, _ := projected.Schema["properties"].(map[string]any)
	url, _ := properties["url"].(map[string]any)
	if url["default"] != "https://ctx7.example/mcp" {
		t.Errorf("the catalogue's URL must be visible as the default: %v", url["default"])
	}

	required, _ := projected.Schema["required"].([]any)
	if len(required) != 0 {
		t.Errorf("the catalogue supplies what a pre-built server needs: required = %v", required)
	}
}

func TestPrebuiltTypeFallsBackToTheKeyWhenTheRowHasNoDisplayName(t *testing.T) {
	projected := prebuiltMCPType(mcpregistry.PrebuiltServer{Key: "miro", Enabled: true})
	metadata, _ := projected.Schema["metadata"].(map[string]any)
	if metadata["label"] != "miro" {
		t.Fatalf("a tile must never have an empty accessible name: label = %v", metadata["label"])
	}
}

func TestPrebuiltTypesDoNotShareTheGenericTypesPropertyMap(t *testing.T) {
	first := prebuiltMCPType(mcpregistry.PrebuiltServer{Key: "a", ServerURL: "https://a.example/mcp", Enabled: true})
	second := prebuiltMCPType(mcpregistry.PrebuiltServer{Key: "b", ServerURL: "https://b.example/mcp", Enabled: true})

	firstURL, _ := first.Schema["properties"].(map[string]any)["url"].(map[string]any)
	secondURL, _ := second.Schema["properties"].(map[string]any)["url"].(map[string]any)
	if firstURL["default"] == secondURL["default"] {
		t.Fatal("two catalogue rows share one property map; one row's URL overwrote the other")
	}
	genericURL, _ := remoteMCPType().Schema["properties"].(map[string]any)["url"].(map[string]any)
	if _, leaked := genericURL["default"]; leaked {
		t.Fatal("a catalogue row's URL leaked into the generic Remote MCP type")
	}
}

func TestMCPProjectionReportsACatalogueReadFailure(t *testing.T) {
	catalogue := &stubCatalogue{err: errors.New("relation does not exist")}
	projection := &prebuiltMCPProjection{catalogue: catalogue}

	projected, err := projection.ProjectToolkitTypes(context.Background())
	if err == nil {
		t.Fatal("an unreadable catalogue must be reported, not read as an empty one")
	}
	if projected != nil {
		t.Fatalf("a failed read must contribute nothing: %v", projected)
	}
}

func TestMCPProjectionIsNamedInItsLogLine(t *testing.T) {
	if got := newPrebuiltMCPProjection(nil).Name(); got == "" {
		t.Fatal("a source that fails must be identifiable in the log")
	}
	if got := (remoteMCPProjection{}).Name(); got == "" {
		t.Fatal("a source that fails must be identifiable in the log")
	}
}
