package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

const platformApplications = "mcp_elitea_internal_applications"

type platformCatalogStub struct {
	tools map[string][]ClientPlatformTool
	err   error
}

func (stub platformCatalogStub) PlatformTools(_ context.Context, _ int64, toolkitType string) ([]ClientPlatformTool, error) {
	return stub.tools[toolkitType], stub.err
}

func applicationsCatalog() platformCatalogStub {
	return platformCatalogStub{tools: map[string][]ClientPlatformTool{platformApplications: {
		{Name: "get_elitea_core_applications", Description: "List the project's agents."},
		{Name: "post_elitea_core_applications", Description: strings.Repeat("é", 400)},
	}}}
}

// resolvedPlatformTools resolves version 1/2 and answers its platform MCP
// entries by type.
func resolvedPlatformTools(t *testing.T, service *ClientApplicationVersionService) map[string]map[string]any {
	t.Helper()
	resolved, err := service.Resolve(t.Context(), 90106, 11, 1, 2)
	require.NoError(t, err)
	var details map[string]any
	require.NoError(t, json.Unmarshal(resolved.VersionDetails, &details))
	byType := map[string]map[string]any{}
	for _, raw := range details["tools"].([]any) {
		if tool := raw.(map[string]any); tool["kind"] == ClientToolKindPlatformMCP {
			byType[tool["type"].(string)] = tool
		}
	}
	require.Contains(t, byType, platformApplications)
	return byType
}

// The definition names a platform MCP server's tools (issue 1231, client
// contract 1.16), and offers exactly what the call path admits: a tool the
// guardrails block is never offered, and once one is, the rest are listed
// explicitly with all_tools false instead of "every tool".
func TestClientApplicationVersionPlatformMCPToolsFollowTheGuardrails(t *testing.T) {
	t.Parallel()

	open := resolvedPlatformTools(t, clientRemoteToolService(t, guardrails.Policy{}).WithPlatformTools(applicationsCatalog()))
	applications := open[platformApplications]
	require.Equal(t, true, applications["all_tools"])
	require.Equal(t, []any{}, applications["selected_tools"])
	tools := applications["tools"].([]any)
	require.Len(t, tools, 2)
	require.Equal(t, map[string]any{"name": "get_elitea_core_applications", "description": "List the project's agents."}, tools[0])
	description := tools[1].(map[string]any)["description"].(string)
	require.LessOrEqual(t, len(description), clientPlatformToolDescriptionMax)
	require.True(t, strings.HasPrefix(strings.Repeat("é", 400), description), "cut on a rune boundary")
	for toolType, entry := range open {
		if toolType != platformApplications {
			require.NotContains(t, entry, "tools", "a server the catalogue does not know has no tools")
			require.Equal(t, true, entry["all_tools"])
		}
	}

	blockedTool := guardrails.NewPolicy(guardrails.PolicyInput{BlockedTools: map[string][]string{
		platformApplications: {"get_elitea_core_applications"},
	}})
	blocked := resolvedPlatformTools(t, clientRemoteToolService(t, blockedTool).WithPlatformTools(applicationsCatalog()))[platformApplications]
	require.Equal(t, false, blocked["all_tools"])
	require.Equal(t, []any{"post_elitea_core_applications"}, blocked["selected_tools"])
	require.Len(t, blocked["tools"], 1)
	require.Equal(t, "post_elitea_core_applications", blocked["tools"].([]any)[0].(map[string]any)["name"])
	// The call path agrees: the listed tool is admitted, the blocked one is not.
	_, err := clientRemoteToolService(t, blockedTool).AuthorizeRemoteTool(t.Context(), platformToolRequest(platformApplications, func(r *RemoteToolAuthorization) {
		r.ToolName = "post_elitea_core_applications"
	}))
	require.NoError(t, err)
	_, err = clientRemoteToolService(t, blockedTool).AuthorizeRemoteTool(t.Context(), platformToolRequest(platformApplications, func(r *RemoteToolAuthorization) {
		r.ToolName = "get_elitea_core_applications"
	}))
	require.ErrorIs(t, err, ErrRemoteToolBlocked)

}

func TestClientApplicationVersionPlatformCatalogFaultIsUnavailable(t *testing.T) {
	t.Parallel()
	service := clientRemoteToolService(t, guardrails.Policy{}).WithPlatformTools(platformCatalogStub{err: errors.New("down")})
	_, err := service.Resolve(t.Context(), 90106, 11, 1, 2)
	require.ErrorIs(t, err, ErrContentUnavailable)
}

func TestClientPlatformProjectionSelection(t *testing.T) {
	t.Parallel()
	catalog := map[string][]ClientPlatformTool{platformApplications: {{Name: "a"}, {Name: "b"}}}
	blockA := guardrails.NewPolicy(guardrails.PolicyInput{BlockedTools: map[string][]string{platformApplications: {"a"}}})
	// The freeze drops a server these block; the projection offers it
	// nothing either way.
	everyMCP := guardrails.NewPolicy(guardrails.PolicyInput{BlockedToolkits: []string{"mcp"}})
	for name, tc := range map[string]struct {
		projection clientPlatformProjection
		frozen     []string
		selected   []string
		allTools   bool
		listed     []string
	}{
		"no policy, unknown tools":     {projection: clientPlatformProjection{}, selected: []string{}, allTools: true},
		"blocked tool, unknown tools":  {projection: clientPlatformProjection{policy: &blockA}, selected: []string{}, allTools: true},
		"blocked tool, known tools":    {projection: clientPlatformProjection{policy: &blockA, tools: catalog}, selected: []string{"b"}, listed: []string{"b"}},
		"frozen selection, blocked":    {projection: clientPlatformProjection{policy: &blockA, tools: catalog}, frozen: []string{"a", "b"}, selected: []string{"b"}, listed: []string{"b"}},
		"frozen selection, no catalog": {projection: clientPlatformProjection{policy: &blockA}, frozen: []string{"a"}, selected: []string{}},
		"every MCP server blocked":     {projection: clientPlatformProjection{policy: &everyMCP, tools: catalog}, selected: []string{}},
	} {
		selected, allTools, listed := tc.projection.selection(platformApplications, tc.frozen)
		require.Equal(t, tc.selected, selected, name)
		require.Equal(t, tc.allTools, allTools, name)
		var names []string
		for _, tool := range listed {
			names = append(names, tool.Name)
		}
		require.Equal(t, tc.listed, names, name)
	}
}
