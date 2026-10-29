package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// workerEgressSection is the worker public egress policy (ADR-0044 decision
// 2, #1490). It is LIVE: Main reads it whenever it prepares a worker command
// and signs the run's assembled allowlist into the command. It shares the
// gateway governance permission (`configuration.governance`, which already
// owns the gateway's egress allowlist), so no migration seeds anything.
func workerEgressSection() map[string]any {
	const section = platformconfig.SectionWorkerEgress
	defaults := platformconfig.DefaultWorkerEgressSettings()
	return map[string]any{
		"id":    section,
		"title": "Worker Egress",
		"description": "Which public hosts agents, toolkits and MCP servers may reach from the worker. Open leaves " +
			"public hosts unrestricted (private, link-local and metadata addresses are always refused). Allowlist " +
			"limits each run to the entries below, its project's entries, and the hosts of the toolkits and MCP " +
			"servers it uses; a refused host is a tool error. Runs under the governed credential profile always " +
			"use the allowlist. Seed the list from Worker egress hosts before switching.",
		"order":               6,
		"icon":                "public",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			{"key": platformconfig.KeyWorkerEgressMode, "type": "string", "title": "Mode",
				"description": "open (the default) or allowlist.", "section": section,
				"default": string(defaults.Mode),
				"enum":    []string{string(platformconfig.WorkerEgressOpen), string(platformconfig.WorkerEgressAllowlist)}},
			{"key": platformconfig.KeyWorkerEgressAllowlist, "type": "array", "items": map[string]any{"type": "string"},
				"title": "Platform Allowlist",
				"description": fmt.Sprintf("Hosts every run may reach in allowlist mode: example.com, example.com:8443, "+
					"*.example.com (subdomains only) or a CIDR block (IP literals only). No scheme or path. At most %d.",
					platformconfig.MaxWorkerEgressPlatformEntries),
				"section": section, "default": []any{}, "maxItems": platformconfig.MaxWorkerEgressPlatformEntries},
		},
	}
}

// validateWorkerEgressValues refuses an allowlist entry the command assembly
// would refuse later: a saved list that silently failed every run is worse
// than a form that says which entry is wrong.
func validateWorkerEgressValues(values map[string]any) string {
	if raw, present := values[platformconfig.KeyWorkerEgressMode]; present {
		mode, _ := raw.(string)
		switch platformconfig.WorkerEgressMode(mode) {
		case platformconfig.WorkerEgressOpen, platformconfig.WorkerEgressAllowlist:
		default:
			return fmt.Sprintf("%q must be open or allowlist", platformconfig.KeyWorkerEgressMode)
		}
	}
	raw, present := values[platformconfig.KeyWorkerEgressAllowlist]
	if !present {
		return ""
	}
	items, ok := raw.([]any)
	if !ok {
		return fmt.Sprintf("%q must be an array", platformconfig.KeyWorkerEgressAllowlist)
	}
	entries := make([]string, 0, len(items))
	for index, item := range items {
		entry, ok := item.(string)
		if !ok {
			return fmt.Sprintf("%q[%d] must be a string", platformconfig.KeyWorkerEgressAllowlist, index)
		}
		entries = append(entries, entry)
	}
	if err := platformconfig.ValidateWorkerEgressEntries(entries, platformconfig.MaxWorkerEgressPlatformEntries); err != nil {
		return fmt.Sprintf("%q: %v", platformconfig.KeyWorkerEgressAllowlist, err)
	}
	return ""
}
