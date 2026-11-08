package configurations

import (
	"encoding/json"
	"slices"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// BrowserConfigurationType is a project's cloud browser setting (ADR-0049
// decisions 3 and 5, #1596): one tenant configuration row with type
// `browser` in section `project_settings`. `enabled` (default false) lets the
// project's runs use the cloud browser. `engine` is the browser engine:
// one of the engines the ADR-0049 bake-off admitted
// (platformconfig.BrowserEngines): `full` or `auto` today; `lite` is refused
// with CurrentConfigurationMutationEngineNotAdmitted until a re-run admits
// Moli alone (platformconfig.BrowserLiteAdmitted). `allow_domains` is a list in the egresslib grammar that
// the browser may reach when its egress token has an allowlist. It takes the
// `models.project_settings.edit` gate of the other project settings.
const (
	BrowserConfigurationType = "browser"
	BrowserSection           = "project_settings"
	// BrowserEngineFull is the default engine (platformconfig.BrowserEngineFull).
	BrowserEngineFull = platformconfig.BrowserEngineFull
	// BrowserEngineNotAdmittedMessage is the API message of
	// CurrentConfigurationMutationEngineNotAdmitted.
	BrowserEngineNotAdmittedMessage = platformconfig.BrowserEngineNotAdmittedMessage
	// MaxBrowserAllowDomains is the most entries of `allow_domains`.
	MaxBrowserAllowDomains = 128
)

// addBrowserProjectSetting registers the type beside the pinned legacy
// entries, on the envelope of another project-settings entry, as
// addWorkerEgressProjectSetting does.
func (catalog *CurrentAvailableCatalog) addBrowserProjectSetting() error {
	if _, exists := catalog.entryIndexes[BrowserConfigurationType]; exists {
		return nil
	}
	template, ok := catalog.entryIndexes["project_icon"]
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	admitted := platformconfig.AdmittedBrowserEngines
	var schema map[string]any
	if err := json.Unmarshal(catalog.entries[template].ConfigSchema, &schema); err != nil {
		return ErrInvalidCurrentAvailableSnapshot
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	properties["data"] = map[string]any{
		"title": "Browser",
		"type":  "object",
		"metadata": map[string]any{
			"label": "Cloud browser", "section": BrowserSection, "type": BrowserConfigurationType,
		},
		"properties": map[string]any{
			"enabled": map[string]any{
				"title":       "Enabled",
				"description": "Let this project's agents use the cloud browser toolkit.",
				"type":        "boolean",
				"default":     false,
			},
			"engine": map[string]any{
				"title":       "Engine",
				"description": browserEngineDescription(admitted),
				"type":        "string",
				"enum":        platformconfig.BrowserEngines(admitted),
				"default":     BrowserEngineFull,
			},
			"allow_domains": map[string]any{
				"title": "Allowed domains",
				"description": "Hosts the browser may reach when the platform limits browser egress: " +
					"example.com, example.com:8443, *.example.com or a CIDR block. Added to the worker egress " +
					"entries of the platform and of this project.",
				"default": nil,
				"anyOf": []any{
					map[string]any{"type": "array", "items": map[string]any{"type": "string"},
						"maxItems": MaxBrowserAllowDomains},
					map[string]any{"type": "null"},
				},
			},
		},
	}
	schema["title"] = "Cloud browser"
	encoded, err := json.Marshal(schema)
	if err != nil {
		return ErrInvalidCurrentAvailableSnapshot
	}
	catalog.entryIndexes[BrowserConfigurationType] = len(catalog.entries)
	catalog.entries = append(catalog.entries, CurrentAvailableConfigurationType{
		Type: BrowserConfigurationType, Section: BrowserSection, ConfigSchema: encoded,
	})
	return nil
}

func browserEngineDescription(admitted platformconfig.BrowserAdmission) string {
	switch {
	case admitted.Lite && admitted.Auto:
		return "The browser engine: full (Chrome), lite (Moli, smaller and faster on read pages) " +
			"or auto (Moli first, Chrome when a page needs it)."
	case admitted.Auto:
		return "The browser engine: full (Chrome) or auto (Moli first, smaller and faster on read pages; " +
			"Chrome when a page needs it)."
	case admitted.Lite:
		return "The browser engine: full (Chrome) or lite (Moli, smaller and faster on read pages)."
	default:
		return "The browser engine. Only full is available."
	}
}

// normalizeBrowserSetting keeps `enabled` (default false), `engine` (one of
// the admitted engines, default `full`) and `allow_domains` (every entry
// valid in the egresslib grammar, at most MaxBrowserAllowDomains, duplicates
// removed). Other keys are dropped. A known engine the bake-off did not
// admit (`lite` today) is refused with
// CurrentConfigurationMutationEngineNotAdmitted, which names the ADR-0049
// appendix; any other value is invalid.
func normalizeBrowserSetting(data map[string]any) (map[string]any, error) {
	return normalizeBrowserSettingFor(data, platformconfig.BrowserEngines(platformconfig.AdmittedBrowserEngines))
}

// normalizeBrowserSettingFor is normalizeBrowserSetting over a given set of
// admitted engines.
func normalizeBrowserSettingFor(data map[string]any, engines []string) (map[string]any, error) {
	if data == nil {
		return nil, currentLocalConfigurationFieldError("data")
	}
	normalized := map[string]any{"enabled": false, "engine": BrowserEngineFull, "allow_domains": []any{}}
	if raw, present := data["enabled"]; present && raw != nil {
		enabled, ok := raw.(bool)
		if !ok {
			return nil, currentLocalConfigurationFieldError("data.enabled")
		}
		normalized["enabled"] = enabled
	}
	if raw, present := data["engine"]; present && raw != nil {
		engine, ok := raw.(string)
		if !ok || !platformconfig.IsBrowserEngine(engine) {
			return nil, currentLocalConfigurationFieldError("data.engine")
		}
		if !slices.Contains(engines, engine) {
			return nil, currentMutationFieldError(CurrentConfigurationMutationEngineNotAdmitted, "data.engine")
		}
		normalized["engine"] = engine
	}
	raw, present := data["allow_domains"]
	if !present || raw == nil {
		return normalized, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, currentLocalConfigurationFieldError("data.allow_domains")
	}
	entries := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		entry, ok := item.(string)
		if !ok {
			return nil, currentLocalConfigurationFieldError("data.allow_domains")
		}
		if !seen[entry] {
			seen[entry] = true
			entries = append(entries, entry)
		}
	}
	if platformconfig.ValidateWorkerEgressEntries(entries, MaxBrowserAllowDomains) != nil {
		return nil, currentLocalConfigurationFieldError("data.allow_domains")
	}
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry)
	}
	normalized["allow_domains"] = out
	return normalized, nil
}
