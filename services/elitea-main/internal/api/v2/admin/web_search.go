package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// webSearchSection is the platform policy of the `web` and `host_intel`
// toolkits (#1238). It is LIVE: Main reads it whenever it freezes a web
// toolkit's settings for a run (application/configurations, the web provider
// resolution) and hands the result to the worker with the providers. It
// carries no spend limit: web and host calls are billed through the LLM
// budget engine, so the project and member budgets bound them.
func webSearchSection() map[string]any {
	const section = platformconfig.SectionWebSearch
	defaults := platformconfig.DefaultWebSearchSettings()
	domains := func(key, title, description string) map[string]any {
		return map[string]any{
			"key": key, "type": "array", "items": map[string]any{"type": "string"}, "title": title,
			"description": description, "section": section, "default": []any{},
			"maxItems": platformconfig.MaxWebSearchDomains,
		}
	}
	return map[string]any{
		"id":    section,
		"title": "Web Search",
		"description": "Which web search and host-intelligence providers agents may use, and the rules of direct " +
			"page fetches. A provider also needs a credential: the project's own, or a shared one in the public " +
			"project (Shodan accepts only a project's own key). Spending is limited by the project and member budgets.",
		"order":          9,
		"icon":           "travel_explore",
		"always_visible": true,
		"fields": []map[string]any{
			{"key": platformconfig.KeyWebSearchEnabledProviders, "type": "array",
				"items": map[string]any{"type": "string", "enum": toAny(platformconfig.WebSearchProviderIDs())},
				"title": "Enabled Providers",
				"description": "Providers agents may call: tavily, firecrawl, brave, ddg_instant (DuckDuckGo instant " +
					"answers only) and shodan (host intelligence; each project must also be granted the host_intel " +
					"toolkit type). None is enabled by default.",
				"section": section, "default": []any{}},
			{"key": platformconfig.KeyWebSearchDefaultSearchProvider, "type": "string", "title": "Default Search Provider",
				"description": "The provider a web search uses when neither the call nor the toolkit names one. " +
					"DuckDuckGo instant answers cannot be the default.",
				"section": section, "default": "",
				"enum": append([]string{""}, platformconfig.WebSearchDefaultableProviders()...)},
			{"key": platformconfig.KeyWebSearchDirectFetchEnabled, "type": "boolean", "title": "Allow Direct Fetch",
				"description": "Whether web_fetch may download a page itself, through the platform's egress guard, " +
					"instead of through a provider.",
				"section": section, "default": defaults.DirectFetchEnabled},
			domains(platformconfig.KeyWebSearchFetchAllowDomains, "Fetch Allow Domains",
				fmt.Sprintf("When set, the only hosts a direct fetch may reach (for example docs.rs or *.github.com). "+
					"At most %d.", platformconfig.MaxWebSearchDomains)),
			domains(platformconfig.KeyWebSearchFetchDenyDomains, "Fetch Deny Domains",
				"Hosts a direct fetch may never reach."),
			{"key": platformconfig.KeyWebSearchAllowHTTP, "type": "boolean", "title": "Allow Plain HTTP",
				"description": "Whether a direct fetch may use http:// on port 80. Off, only https:// on port 443.",
				"section":     section, "default": defaults.AllowHTTP},
			{"key": platformconfig.KeyWebSearchMaxResultsCap, "type": "integer", "title": "Web Search Result Cap",
				"description": "The most results one web search returns.", "section": section,
				"default": defaults.MaxResultsCap, "minimum": 1, "maximum": platformconfig.MaxWebMaxResultsCap},
			{"key": platformconfig.KeyWebSearchHostMaxResultsCap, "type": "integer", "title": "Host Search Result Cap",
				"description": "The most hosts one host-intelligence search returns.", "section": section,
				"default": defaults.HostMaxResultsCap, "minimum": 1, "maximum": platformconfig.MaxHostMaxResultsCap},
		},
	}
}
