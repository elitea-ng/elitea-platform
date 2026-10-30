package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// remoteAgentsSection is the remote agent policy (ADR-0044 decision 4,
// #1501). It is LIVE: Main reads it when a worker asks for a delegation
// assertion and when the MCP endpoint verifies one. It shares the gateway
// governance permission (`configuration.governance`), so no migration seeds
// anything.
func remoteAgentsSection() map[string]any {
	const section = platformconfig.SectionRemoteAgents
	defaults := platformconfig.DefaultRemoteAgentSettings()
	return map[string]any{
		"id":    section,
		"title": "Remote Agents",
		"description": "How agents authenticate calls to agents outside the run. Outbound: each call a run makes to a " +
			"remote MCP server carries a short-lived assertion signed with the agent assertion key " +
			"(header Elitea-Agent-Assertion; keys at /.well-known/elitea-agent-jwks.json). Inbound: the MCP endpoint " +
			"accepts an assertion only from the issuers listed below. Runs under the governed credential profile " +
			"always send an assertion.",
		"order":               6,
		"icon":                "hub",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			{"key": platformconfig.KeyRemoteAgentsOutboundAssertions, "type": "string", "title": "Outbound Assertions",
				"description": "on (the default) or off. Off sends no assertion, except from governed runs.",
				"section":     section, "default": string(defaults.OutboundAssertions),
				"enum": []string{string(platformconfig.OutboundAssertionsOn), string(platformconfig.OutboundAssertionsOff)}},
			{"key": platformconfig.KeyRemoteAgentsTrustedIssuers, "type": "array", "items": map[string]any{"type": "string"},
				"title": "Trusted Issuers",
				"description": fmt.Sprintf("Issuers whose assertions the MCP endpoint accepts. Each entry is the issuer "+
					"URL, optionally followed by a space and its JWKS URL. For another Elitea deployment write "+
					"https://<host>/agent-issuer; its keys are then read from https://<host>%s. At most %d.",
					platformconfig.EliteaAgentJWKSPath, platformconfig.MaxRemoteAgentTrustedIssuers),
				"section": section, "default": []any{}, "maxItems": platformconfig.MaxRemoteAgentTrustedIssuers},
		},
	}
}

// validateRemoteAgentsValues refuses an issuer entry the verifier would
// ignore: a saved list that silently trusted nothing is worse than a form
// that says which entry is wrong.
func validateRemoteAgentsValues(values map[string]any) string {
	if raw, present := values[platformconfig.KeyRemoteAgentsOutboundAssertions]; present {
		mode, _ := raw.(string)
		switch platformconfig.OutboundAssertionsMode(mode) {
		case platformconfig.OutboundAssertionsOn, platformconfig.OutboundAssertionsOff:
		default:
			return fmt.Sprintf("%q must be on or off", platformconfig.KeyRemoteAgentsOutboundAssertions)
		}
	}
	raw, present := values[platformconfig.KeyRemoteAgentsTrustedIssuers]
	if !present {
		return ""
	}
	items, ok := raw.([]any)
	if !ok {
		return fmt.Sprintf("%q must be an array", platformconfig.KeyRemoteAgentsTrustedIssuers)
	}
	entries := make([]string, 0, len(items))
	for index, item := range items {
		entry, ok := item.(string)
		if !ok {
			return fmt.Sprintf("%q[%d] must be a string", platformconfig.KeyRemoteAgentsTrustedIssuers, index)
		}
		entries = append(entries, entry)
	}
	if _, err := platformconfig.ParseTrustedIssuers(entries); err != nil {
		return fmt.Sprintf("%q: %v", platformconfig.KeyRemoteAgentsTrustedIssuers, err)
	}
	return ""
}
