package configurations

import "encoding/json"

// CloudExecutorConfigurationType is the project setting of the desktop ⇄
// cloud handoff (ADR-0034 decision 11): one tenant configuration row with
// type `cloud_executor` in section `project_settings`. It holds `enabled`,
// `shell`, `network`, `network_allowlist`, `history` and optional lower caps,
// and it can only NARROW the platform ceiling (`cloud_executor` platform
// section): the handoff's effective policy takes the minimum of the two. The
// exception is `enabled`: an explicit value wins under a platform that has the
// executor on, and an unset one follows the platform's
// `project_default_enabled`. Its
// writes take `models.project_settings.edit` (the configurations routes add
// that gate for this type).
const (
	CloudExecutorConfigurationType = "cloud_executor"
	CloudExecutorSection           = "project_settings"
)

// addCloudExecutorProjectSetting registers the type beside the pinned legacy
// entries. It reuses the generic configuration envelope of another
// project-settings entry, so the create form and the required-field rules
// read it the way they read every other type.
func (catalog *CurrentAvailableCatalog) addCloudExecutorProjectSetting() error {
	if _, exists := catalog.entryIndexes[CloudExecutorConfigurationType]; exists {
		return nil
	}
	template, ok := catalog.entryIndexes["project_icon"]
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	var schema map[string]any
	if err := json.Unmarshal(catalog.entries[template].ConfigSchema, &schema); err != nil {
		return ErrInvalidCurrentAvailableSnapshot
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return ErrInvalidCurrentAvailableSnapshot
	}
	optionalInteger := func(title, description string) map[string]any {
		return map[string]any{
			"title": title, "description": description, "default": nil,
			"anyOf": []any{map[string]any{"type": "integer", "minimum": 1}, map[string]any{"type": "null"}},
		}
	}
	optionalList := func(title, description string) map[string]any {
		return map[string]any{
			"title": title, "description": description, "default": nil,
			"anyOf": []any{
				map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 500},
				map[string]any{"type": "null"},
			},
		}
	}
	optionalBool := func(title, description string) map[string]any {
		return map[string]any{
			"title": title, "description": description, "default": nil,
			"anyOf": []any{map[string]any{"type": "boolean"}, map[string]any{"type": "null"}},
		}
	}
	properties["data"] = map[string]any{
		"title": "CloudExecutor",
		"type":  "object",
		"metadata": map[string]any{
			"label": "Cloud executor", "section": CloudExecutorSection, "type": CloudExecutorConfigurationType,
		},
		"properties": map[string]any{
			"enabled": optionalBool("Enabled", "Allow Send to cloud in this project. Unset follows the "+
				"platform's default for projects. On has no effect while the platform has the cloud executor off."),
			"shell":   optionalBool("Shell", "Allow shell commands in the cloud sandbox."),
			"network": optionalBool("Network", "Allow the cloud sandbox to reach the public internet."),
			"network_allowlist": map[string]any{
				"title": "Network Allowlist", "description": "Host names the sandbox may reach. The project " +
					"list is intersected with the platform list.",
				"default": nil,
				"anyOf": []any{
					map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 200},
					map[string]any{"type": "null"},
				},
			},
			"history": map[string]any{
				"title": "History", "description": "`none` (the default) or `full` when the platform allows it.",
				"default": nil,
				"anyOf":   []any{map[string]any{"type": "string", "enum": []any{"none", "full"}}, map[string]any{"type": "null"}},
			},
			"exclusions_add": optionalList("Exclusions to Add", "Path patterns a send never uploads, on top of the "+
				"platform list (which a project cannot remove)."),
			"allow_user_include": optionalBool("Allow Explicit Include", "Allow users to send a file the "+
				"exclusions match. Cannot be turned on when the platform has it off."),
			"env_harvest": optionalBool("Allow Environment Variables", "Allow users to send selected environment "+
				"variables. Cannot be turned on when the platform has it off."),
			"env_deny_add": optionalList("Denied Variable Names to Add", "Name patterns never offered, on top of "+
				"the platform list."),
			"env_allow": optionalList("Allowed Variable Names", "When set, the only names that can be sent; "+
				"intersected with the platform list."),
			"max_segment_mb":           optionalInteger("Maximum Upload (MB)", "Lower than the platform value only."),
			"max_file_mb":              optionalInteger("Maximum File Size (MB)", "Lower than the platform value only."),
			"max_files":                optionalInteger("Maximum Files", "Lower than the platform value only."),
			"max_unpacked_mb":          optionalInteger("Maximum Unpacked Size (MB)", "Lower than the platform value only."),
			"max_project_workspace_gb": optionalInteger("Cloud Copies (GB)", "Lower than the platform value only."),
			"idle_retention_days":      optionalInteger("Idle Retention (days)", "Lower than the platform value only."),
			"return_grace_days":        optionalInteger("Return Grace (days)", "Lower than the platform value only."),
		},
	}
	schema["title"] = "Cloud executor"
	encoded, err := json.Marshal(schema)
	if err != nil {
		return ErrInvalidCurrentAvailableSnapshot
	}
	catalog.entryIndexes[CloudExecutorConfigurationType] = len(catalog.entries)
	catalog.entries = append(catalog.entries, CurrentAvailableConfigurationType{
		Type: CloudExecutorConfigurationType, Section: CloudExecutorSection, ConfigSchema: encoded,
	})
	return nil
}

// normalizeCloudExecutorSetting keeps the declared fields of a cloud executor
// project setting, each one typed or null. Narrowing against the platform
// ceiling happens when the policy is computed, not here: a project value above
// the ceiling is stored and simply has no effect.
func normalizeCloudExecutorSetting(data map[string]any) (map[string]any, error) {
	if data == nil {
		return nil, currentLocalConfigurationFieldError("data")
	}
	normalized := map[string]any{}
	for _, key := range []string{"exclusions_add", "env_deny_add", "env_allow"} {
		raw, present := data[key]
		if !present || raw == nil {
			continue
		}
		items, ok := raw.([]any)
		if !ok || len(items) > 500 {
			return nil, currentLocalConfigurationFieldError("data." + key)
		}
		patterns := make([]any, 0, len(items))
		for _, item := range items {
			pattern, ok := item.(string)
			if !ok || pattern == "" || len(pattern) > 512 {
				return nil, currentLocalConfigurationFieldError("data." + key)
			}
			patterns = append(patterns, pattern)
		}
		normalized[key] = patterns
	}
	for _, key := range []string{"enabled", "shell", "network", "allow_user_include", "env_harvest"} {
		raw, present := data[key]
		if !present || raw == nil {
			continue
		}
		value, ok := raw.(bool)
		if !ok {
			return nil, currentLocalConfigurationFieldError("data." + key)
		}
		normalized[key] = value
	}
	if raw, present := data["network_allowlist"]; present && raw != nil {
		items, ok := raw.([]any)
		if !ok || len(items) > 200 {
			return nil, currentLocalConfigurationFieldError("data.network_allowlist")
		}
		hosts := make([]any, 0, len(items))
		for _, item := range items {
			host, ok := item.(string)
			if !ok || host == "" || len(host) > 253 {
				return nil, currentLocalConfigurationFieldError("data.network_allowlist")
			}
			hosts = append(hosts, host)
		}
		normalized["network_allowlist"] = hosts
	}
	if raw, present := data["history"]; present && raw != nil {
		history, ok := raw.(string)
		if !ok || (history != "none" && history != "full") {
			return nil, currentLocalConfigurationFieldError("data.history")
		}
		normalized["history"] = history
	}
	for _, key := range []string{"max_segment_mb", "max_file_mb", "max_files", "max_unpacked_mb",
		"max_project_workspace_gb", "idle_retention_days", "return_grace_days"} {
		raw, present := data[key]
		if !present || raw == nil {
			continue
		}
		if _, flag := raw.(bool); flag {
			return nil, currentLocalConfigurationFieldError("data." + key)
		}
		value, ok := currentLocalPydanticInteger(raw)
		number, integer := value.(int64)
		if !ok || !integer || number < 0 || number > 1<<31 {
			return nil, currentLocalConfigurationFieldError("data." + key)
		}
		normalized[key] = number
	}
	return normalized, nil
}
