package mcpregistry

import "strings"

// InternalBuilder is a fixed Main MCP category. It has no user-authored URL or
// credential. General chat enables every implemented builder category.
type InternalBuilder struct{ Key, Category, Name string }

var internalBuilders = []InternalBuilder{
	{"applications", "elitea_core/applications", "Elitea Applications"},
	{"chat", "elitea_core/chat", "Elitea Chat"},
	{"toolkits", "elitea_core/toolkits", "Elitea Toolkits"},
	{"configurations", "configurations", "Elitea Configurations"},
	{"secrets", "secrets", "Elitea Secrets"},
	{"discovery", "elitea_core/discovery", "Elitea Discovery"},
	{"notifications", "notifications", "Elitea Notifications"},
	{"skills", "elitea_core/skills", "Elitea Skills"},
	{"project_context", "elitea_core/project_context", "Elitea Project Context"},
}

const InternalBuilderTypePrefix = "mcp_elitea_internal_"

func InternalBuilders() []InternalBuilder { return append([]InternalBuilder(nil), internalBuilders...) }
func InternalBuilderForType(toolkitType string) (InternalBuilder, bool) {
	if !strings.HasPrefix(toolkitType, InternalBuilderTypePrefix) {
		return InternalBuilder{}, false
	}
	key := strings.TrimPrefix(toolkitType, InternalBuilderTypePrefix)
	for _, entry := range internalBuilders {
		if entry.Key == key {
			return entry, true
		}
	}
	return InternalBuilder{}, false
}
