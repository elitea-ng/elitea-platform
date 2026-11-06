package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// browserSection is the cloud browser policy (ADR-0049 decisions 5 and 8,
// #1596). It is LIVE: Main reads it whenever it serves the `browser`
// runtime-context field to a worker. It shares the governance permission of
// the worker egress section, so no migration seeds anything.
func browserSection() map[string]any {
	const section = platformconfig.SectionBrowser
	defaults := platformconfig.DefaultBrowserSettings()
	return map[string]any{
		"id":    section,
		"title": "Cloud Browser",
		"description": "The browser toolkit's limits. A project turns the cloud browser on in its own settings. " +
			"The browser reaches the internet only through the egress proxy. Open lets it reach any public host. " +
			"Allowlist limits it to the worker egress allowlist, the project's worker egress entries and the " +
			"project's browser domains. Runs under the governed credential profile always use the allowlist.",
		"order":               6,
		"icon":                "travel_explore",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			{"key": platformconfig.KeyBrowserEgress, "type": "string", "title": "Browser Egress",
				"description": "open (the default) or allowlist.", "section": section,
				"default": string(defaults.Egress),
				"enum":    []string{string(platformconfig.BrowserEgressOpen), string(platformconfig.BrowserEgressAllowlist)}},
			{"key": platformconfig.KeyBrowserMaxRunSeconds, "type": "integer", "title": "Maximum Browser Seconds per Run",
				"description": fmt.Sprintf("The most browser engine seconds one run may use. 0 is no cap. At most %d.",
					platformconfig.MaxBrowserMaxRunSeconds),
				"section": section, "default": defaults.MaxRunSeconds, "minimum": 0,
				"maximum": platformconfig.MaxBrowserMaxRunSeconds},
			{"key": platformconfig.KeyBrowserMonthlyQuotaSeconds, "type": "integer",
				"title": "Monthly Browser Seconds per Project",
				"description": fmt.Sprintf("The most browser engine seconds one project may use in one calendar "+
					"month (UTC). 0 is no quota. At most %d.", platformconfig.MaxBrowserMonthlyQuotaSeconds),
				"section": section, "default": defaults.MonthlyQuotaSeconds, "minimum": 0,
				"maximum": platformconfig.MaxBrowserMonthlyQuotaSeconds},
		},
	}
}

// validateBrowserValues applies the ranges the schema cannot enforce. Each
// refusal names the key.
func validateBrowserValues(values map[string]any) string {
	if raw, present := values[platformconfig.KeyBrowserEgress]; present {
		mode, _ := raw.(string)
		switch platformconfig.BrowserEgressMode(mode) {
		case platformconfig.BrowserEgressOpen, platformconfig.BrowserEgressAllowlist:
		default:
			return fmt.Sprintf("%q must be open or allowlist", platformconfig.KeyBrowserEgress)
		}
	}
	for _, bound := range []struct {
		key   string
		limit int64
	}{
		{platformconfig.KeyBrowserMaxRunSeconds, platformconfig.MaxBrowserMaxRunSeconds},
		{platformconfig.KeyBrowserMonthlyQuotaSeconds, platformconfig.MaxBrowserMonthlyQuotaSeconds},
	} {
		raw, present := values[bound.key]
		if !present {
			continue
		}
		if !platformconfig.ValidBrowserSeconds(raw, bound.limit) {
			return fmt.Sprintf("%q must be 0 or a whole number of seconds up to %d", bound.key, bound.limit)
		}
	}
	return ""
}
