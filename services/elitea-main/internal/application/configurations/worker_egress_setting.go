package configurations

import (
	"encoding/json"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// WorkerEgressConfigurationType is a project's own worker egress entries
// (ADR-0044 decision 2, #1490): one tenant configuration row with type
// `worker_egress` in section `project_settings`, holding `allowlist`, a list
// in the egresslib grammar. While the platform's `worker_egress` mode is
// `allowlist` (or the run is governed), a run of the project may reach these
// hosts beside the platform's entries and its toolkits' hosts. It only ever
// ADDS hosts to the project's runs; the mode stays the platform's decision.
// It is the cloud executor setting's mechanism (the same row, the same
// routes, the same `models.project_settings.edit` gate), not a new store.
const (
	WorkerEgressConfigurationType = "worker_egress"
	WorkerEgressSection           = "project_settings"
)

// addWorkerEgressProjectSetting registers the type beside the pinned legacy
// entries, on the envelope of another project-settings entry, as
// addCloudExecutorProjectSetting does.
func (catalog *CurrentAvailableCatalog) addWorkerEgressProjectSetting() error {
	if _, exists := catalog.entryIndexes[WorkerEgressConfigurationType]; exists {
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
	properties["data"] = map[string]any{
		"title": "WorkerEgress",
		"type":  "object",
		"metadata": map[string]any{
			"label": "Worker egress", "section": WorkerEgressSection, "type": WorkerEgressConfigurationType,
		},
		"properties": map[string]any{
			"allowlist": map[string]any{
				"title": "Allowlist",
				"description": "Public hosts this project's runs may reach when the platform limits worker " +
					"egress: example.com, example.com:8443, *.example.com or a CIDR block. Added to the " +
					"platform's entries and the hosts of the toolkits a run uses.",
				"default": nil,
				"anyOf": []any{
					map[string]any{"type": "array", "items": map[string]any{"type": "string"},
						"maxItems": platformconfig.MaxWorkerEgressProjectEntries},
					map[string]any{"type": "null"},
				},
			},
		},
	}
	schema["title"] = "Worker egress"
	encoded, err := json.Marshal(schema)
	if err != nil {
		return ErrInvalidCurrentAvailableSnapshot
	}
	catalog.entryIndexes[WorkerEgressConfigurationType] = len(catalog.entries)
	catalog.entries = append(catalog.entries, CurrentAvailableConfigurationType{
		Type: WorkerEgressConfigurationType, Section: WorkerEgressSection, ConfigSchema: encoded,
	})
	return nil
}

// normalizeWorkerEgressSetting keeps `allowlist`, every entry valid in the
// egresslib grammar: a project entry the command assembly would refuse is
// refused here, where the project admin can fix it.
func normalizeWorkerEgressSetting(data map[string]any) (map[string]any, error) {
	if data == nil {
		return nil, currentLocalConfigurationFieldError("data")
	}
	normalized := map[string]any{}
	raw, present := data["allowlist"]
	if !present || raw == nil {
		return normalized, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, currentLocalConfigurationFieldError("data.allowlist")
	}
	entries := make([]string, 0, len(items))
	for _, item := range items {
		entry, ok := item.(string)
		if !ok {
			return nil, currentLocalConfigurationFieldError("data.allowlist")
		}
		entries = append(entries, entry)
	}
	if platformconfig.ValidateWorkerEgressEntries(entries, platformconfig.MaxWorkerEgressProjectEntries) != nil {
		return nil, currentLocalConfigurationFieldError("data.allowlist")
	}
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry)
	}
	normalized["allowlist"] = out
	return normalized, nil
}
