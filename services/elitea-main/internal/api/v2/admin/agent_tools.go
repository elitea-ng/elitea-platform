package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The agents section: the platform's ceilings on the per-agent tool envelope
// (ADR-0050 decision 3, #1613). It is LIVE: Main reads it through a
// per-replica cache at every agent admission and freezes it into the run
// input; a save on this replica drops the cache at once, other replicas see
// it within platformconfig.DefaultSectionTTL.

// WithAgentToolsCache supplies the cache an agents save drops.
func WithAgentToolsCache(cache SectionInvalidator) Option {
	return func(h *Handler) {
		if cache != nil {
			h.agentTools = cache
		}
	}
}

func (h *Handler) invalidateAgentTools(section string) {
	if section == platformconfig.SectionAgents && h.agentTools != nil {
		h.agentTools.Invalidate()
	}
}

// agentToolsSection uses the permission of the other runtime governance
// sections, so no migration seeds a new one.
func agentToolsSection() map[string]any {
	const section = platformconfig.SectionAgents
	return map[string]any{
		"id":    section,
		"title": "Agent tools",
		"description": "Ceilings on each agent's tool envelope. An agent's author can limit how often a tool " +
			"may be called in a run, which tools the agent may use when nobody is present (a turn another " +
			"agent handed it), and which tools its sub-agents may get. These settings can only narrow an " +
			"agent's tools; they never lift an approval, a guardrail or a blocked tool.",
		"order":               7,
		"icon":                "build",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			{
				"key":   platformconfig.KeyAgentsMaxToolLimit,
				"type":  "integer",
				"title": "Maximum Calls of One Tool",
				"description": fmt.Sprintf("The most calls of any one toolkit tool in a turn, whatever the agent's own "+
					"limits say. 0 (the default) sets no limit beyond the run caps. From 0 to %d.",
					platformconfig.MaxAgentsMaxToolLimit),
				"section": section,
				"default": 0,
				"minimum": 0,
				"maximum": platformconfig.MaxAgentsMaxToolLimit,
			},
			{
				"key":   platformconfig.KeyAgentsUnattendedToolsAllowlist,
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"title": "Tools That Write, Allowed Unattended",
				"description": "Tools that change something and that an agent may still use with nobody present, " +
					"as \"toolkit type/tool\", for example \"github/create_comment\" (\"github/*\" for every tool " +
					"of the type). The toolkit is matched by its type, never by its name. Read-only tools need no " +
					"entry. A tool that writes and is not listed is never used unattended, even when the " +
					"agent's own unattended tools name it. Empty (the default) allows none.",
				"section":  section,
				"default":  []any{},
				"maxItems": platformconfig.MaxAgentsUnattendedAllowlistLength,
			},
		},
	}
}

// validateAgentToolsValues applies the range and the key shape the schema
// cannot enforce.
func validateAgentToolsValues(section string, values map[string]any) string {
	if section != platformconfig.SectionAgents {
		return ""
	}
	if raw, present := values[platformconfig.KeyAgentsMaxToolLimit]; present && raw != nil &&
		!platformconfig.ValidAgentsMaxToolLimit(raw) {
		return fmt.Sprintf("%q must be a whole number from 0 to %d", platformconfig.KeyAgentsMaxToolLimit,
			platformconfig.MaxAgentsMaxToolLimit)
	}
	if raw, present := values[platformconfig.KeyAgentsUnattendedToolsAllowlist]; present && raw != nil {
		items, ok := raw.([]any)
		if !ok || len(items) > platformconfig.MaxAgentsUnattendedAllowlistLength {
			return fmt.Sprintf("%q must be a list of at most %d tool keys",
				platformconfig.KeyAgentsUnattendedToolsAllowlist, platformconfig.MaxAgentsUnattendedAllowlistLength)
		}
		for _, item := range items {
			if key, ok := item.(string); !ok || !platformconfig.ValidAgentToolKey(key) {
				return fmt.Sprintf("%q entry %v is not \"toolkit/tool\"",
					platformconfig.KeyAgentsUnattendedToolsAllowlist, item)
			}
		}
	}
	return ""
}
