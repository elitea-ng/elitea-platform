package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
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
		"id":     section,
		"access": agentGovernanceAccess(),
		"title":  "Worker Egress",
		"description": "Which public hosts agents, toolkits and MCP servers may reach from the worker. Open leaves " +
			"public hosts unrestricted (private addresses are refused unless the Private Permit names them; " +
			"link-local and metadata addresses always are). Allowlist " +
			"limits each run to the entries below, its project's entries, and the hosts of the toolkits and MCP " +
			"servers it uses; a refused host is a tool error. Runs under the governed credential profile always " +
			"use the allowlist. Seed the list from Worker egress hosts before switching. In both modes a toolkit " +
			"or MCP server on a private address (an on-premises Jira or GitLab) is refused unless the Private " +
			"Permit names it; link-local and cloud metadata addresses are refused even then.",
		"order":               6,
		"icon":                "public",
		"always_visible":      true,
		"required_permission": adminaccess.ConfigurationGovernance,
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
			{"key": platformconfig.KeyWorkerEgressPrivatePermit, "type": "array", "items": map[string]any{"type": "string"},
				"title": "Private Permit",
				"description": fmt.Sprintf("Private destinations toolkits and MCP servers may reach in either mode: an "+
					"on-premises host such as jira.corp.example, jira.corp.example:8443, *.corp.example, or a CIDR "+
					"block such as 10.20.0.0/16. A host name admits the private addresses it resolves to. Link-local "+
					"and cloud metadata addresses are never admitted. No scheme or path. At most %d.",
					platformconfig.MaxWorkerEgressPrivateEntries),
				"section": section, "default": []any{}, "maxItems": platformconfig.MaxWorkerEgressPrivateEntries},
		},
	}
}

// validateWorkerEgressValues refuses an allowlist or private permit entry the
// command assembly would refuse later: a saved list that silently failed
// every run is worse than a form that says which entry is wrong.
func validateWorkerEgressValues(values map[string]any) string {
	if raw, present := values[platformconfig.KeyWorkerEgressMode]; present {
		mode, _ := raw.(string)
		switch platformconfig.WorkerEgressMode(mode) {
		case platformconfig.WorkerEgressOpen, platformconfig.WorkerEgressAllowlist:
		default:
			return fmt.Sprintf("%q must be open or allowlist", platformconfig.KeyWorkerEgressMode)
		}
	}
	if reason := validateWorkerEgressList(values, platformconfig.KeyWorkerEgressAllowlist,
		platformconfig.MaxWorkerEgressPlatformEntries); reason != "" {
		return reason
	}
	return validateWorkerEgressList(values, platformconfig.KeyWorkerEgressPrivatePermit,
		platformconfig.MaxWorkerEgressPrivateEntries)
}

// validateWorkerEgressList checks one list field, when the save carries it.
func validateWorkerEgressList(values map[string]any, key string, limit int) string {
	raw, present := values[key]
	if !present {
		return ""
	}
	items, ok := raw.([]any)
	if !ok {
		return fmt.Sprintf("%q must be an array", key)
	}
	entries := make([]string, 0, len(items))
	for index, item := range items {
		entry, ok := item.(string)
		if !ok {
			return fmt.Sprintf("%q[%d] must be a string", key, index)
		}
		entries = append(entries, entry)
	}
	if err := platformconfig.ValidateWorkerEgressEntries(entries, limit); err != nil {
		return fmt.Sprintf("%q: %v", key, err)
	}
	return ""
}
