package agentexecution

import (
	"encoding/json"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

func currentBuilderFlag(name string) bool {
	return name == "internal_mcp" || name == "skill_builder" || name == "project_context_builder"
}

// freezeCurrentInternalMCP converts product selections into credential-free
// fixed category references. Main redeems the executing actor only after claim.
func freezeCurrentInternalMCP(version map[string]any, extra json.RawMessage, tools []any, policy guardrails.Policy) ([]any, error) {
	selected := map[string]bool{}
	var conversation []string
	if len(extra) > 0 && json.Unmarshal(extra, &conversation) != nil {
		return nil, unsupportedStart("internal MCP selection is invalid")
	}
	for _, name := range conversation {
		if currentBuilderFlag(name) {
			selected[name] = true
		}
	}
	if meta, ok := version["meta"].(map[string]any); ok {
		if configured, ok := meta["internal_tools"].([]any); ok {
			retained := make([]any, 0, len(configured))
			for _, value := range configured {
				name, _ := value.(string)
				if currentBuilderFlag(name) {
					selected[name] = true
				} else {
					retained = append(retained, value)
				}
			}
			meta["internal_tools"] = retained
		}
	}
	if len(selected) == 0 {
		return tools, nil
	}
	seen := map[string]bool{}
	for _, raw := range tools {
		if tool, ok := raw.(map[string]any); ok {
			if kind, ok := tool["type"].(string); ok {
				seen[kind] = true
			}
		}
	}
	for _, entry := range mcpregistry.InternalBuilders() {
		if !selected["internal_mcp"] && !(entry.Key == "skills" && selected["skill_builder"]) && !(entry.Key == "project_context" && selected["project_context_builder"]) {
			continue
		}
		kind := mcpregistry.InternalBuilderTypePrefix + entry.Key
		if seen[kind] || policy.ToolkitBlocked(kind) || policy.ToolkitBlocked("mcp") {
			continue
		}
		tools = append(tools, map[string]any{
			"id": nil, "type": kind, "name": entry.Name, "toolkit_name": entry.Name,
			"description": "Read and change Elitea project resources through authorized platform tools.",
			"settings":    map[string]any{"server_name": kind}, "meta": map[string]any{"mcp": true, "internal_builder": true},
		})
		seen[kind] = true
	}
	return tools, nil
}
