package storage

// The tools of a platform MCP server in the desktop's resolved definition
// (ADR-0029 decision 5a, issue 1231, client contract 1.16).

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpregistry"
)

// clientPlatformToolDescriptionMax bounds one tool's description in the
// definition: the model reads it in the generic tool's description, and the
// operations' own descriptions can run to paragraphs.
const clientPlatformToolDescriptionMax = 512

// ClientPlatformTool is one tool of a platform MCP server, as its internal
// MCP endpoint lists it to a cloud turn.
type ClientPlatformTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ClientPlatformToolCatalog lists one platform MCP server's tools (the
// internal builder toolkitType names) in projectID. It answers nil, nil when
// it cannot tell (no MCP server is composed); an error is a fault.
type ClientPlatformToolCatalog interface {
	PlatformTools(ctx context.Context, projectID int64, toolkitType string) ([]ClientPlatformTool, error)
}

// WithPlatformTools gives the service the catalogue that names each platform
// MCP server's tools in the resolved definition. Without it a server is
// projected without `tools`, and per-tool guardrail blocks cannot be listed
// out of an every-tool selection (the call path still refuses them).
func (service *ClientApplicationVersionService) WithPlatformTools(catalog ClientPlatformToolCatalog) *ClientApplicationVersionService {
	if service != nil && catalog != nil {
		service.platformTools = catalog
	}
	return service
}

// clientPlatformProjection is what projecting a platform MCP server needs
// beyond the frozen entry: the guardrails policy the call path applies, and
// each server's tools (by type; a type absent from the map is unknown).
type clientPlatformProjection struct {
	policy *guardrails.Policy
	tools  map[string][]ClientPlatformTool
}

// clientPlatformProjectionFor resolves the policy and the catalogue for the
// platform servers of one frozen version; it reads nothing when the version
// has none.
func (service *ClientApplicationVersionService) clientPlatformProjectionFor(
	ctx context.Context, projectID int64, frozen json.RawMessage,
) (clientPlatformProjection, error) {
	tools, err := decodeClientTools(frozen)
	if err != nil {
		return clientPlatformProjection{}, err
	}
	var types []string
	for _, tool := range tools {
		toolType, _ := tool["type"].(string)
		if _, internal := mcpregistry.InternalBuilderForType(toolType); internal {
			types = append(types, toolType)
		}
	}
	projection := clientPlatformProjection{}
	if len(types) == 0 {
		return projection, nil
	}
	if service.guardrails != nil {
		policy, err := service.guardrails.ResolveCurrentAgentGuardrails(ctx)
		if err != nil {
			return clientPlatformProjection{}, err
		}
		projection.policy = &policy
	}
	if service.platformTools != nil {
		projection.tools = make(map[string][]ClientPlatformTool, len(types))
		for _, toolType := range types {
			listed, err := service.platformTools.PlatformTools(ctx, projectID, toolType)
			if err != nil {
				return clientPlatformProjection{}, err
			}
			if listed != nil {
				projection.tools[toolType] = listed
			}
		}
	}
	return projection, nil
}

// selection is a platform server's selected_tools, all_tools and tools,
// under the predicates authorizePlatformTool admits a call with: a server
// the guardrails block (or block every MCP server) offers no tool; a blocked
// tool is never offered. An every-tool selection stays all_tools true while
// nothing of it is blocked; once a tool is, the remaining tools are listed
// explicitly with all_tools false. tools is nil when the catalogue does not
// know the server.
func (projection clientPlatformProjection) selection(toolType string, frozen []string) ([]string, bool, []ClientPlatformTool) {
	policy := projection.policy
	if policy != nil && (policy.ToolkitBlocked(toolType) || policy.ToolkitBlocked("mcp")) {
		return []string{}, false, emptyIfKnown(projection.tools, toolType)
	}
	blocked := func(name string) bool { return policy != nil && policy.ToolBlocked(toolType, name) }
	catalog, known := projection.tools[toolType]
	if len(frozen) > 0 {
		selected := []string{}
		for _, name := range frozen {
			if !blocked(name) {
				selected = append(selected, name)
			}
		}
		var listed []ClientPlatformTool
		if known {
			listed = []ClientPlatformTool{}
			for _, tool := range catalog {
				if containsExact(selected, tool.Name) {
					listed = append(listed, clientPlatformTool(tool))
				}
			}
		}
		return selected, false, listed
	}
	if !known {
		return []string{}, true, nil
	}
	listed := []ClientPlatformTool{}
	names := []string{}
	for _, tool := range catalog {
		if !blocked(tool.Name) {
			listed = append(listed, clientPlatformTool(tool))
			names = append(names, tool.Name)
		}
	}
	if len(listed) == len(catalog) {
		return []string{}, true, listed
	}
	return names, false, listed
}

func emptyIfKnown(tools map[string][]ClientPlatformTool, toolType string) []ClientPlatformTool {
	if _, known := tools[toolType]; known {
		return []ClientPlatformTool{}
	}
	return nil
}

func clientPlatformTool(tool ClientPlatformTool) ClientPlatformTool {
	description := tool.Description
	if len(description) > clientPlatformToolDescriptionMax {
		end := clientPlatformToolDescriptionMax
		for end > 0 && !utf8.RuneStart(description[end]) {
			end--
		}
		description = description[:end]
	}
	return ClientPlatformTool{Name: tool.Name, Description: description}
}

// clientPlatformToolValues is tools as decoded JSON, so the projection's
// scrub walks it like every other value of the document.
func clientPlatformToolValues(tools []ClientPlatformTool) []any {
	values := make([]any, 0, len(tools))
	for _, tool := range tools {
		values = append(values, map[string]any{"name": tool.Name, "description": tool.Description})
	}
	return values
}
