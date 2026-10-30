package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/remoteagent"
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
		"description": "How agents authenticate calls to agents outside the run. Outbound: a call a run makes to a " +
			"remote MCP server can carry a short-lived assertion signed with the agent assertion key " +
			"(header Elitea-Agent-Assertion; keys at /.well-known/elitea-agent-jwks.json). The assertion tells the " +
			"remote server the user, the project, the run, the mandate and the run's toolkit types. Runs under the " +
			"governed credential profile always send one. Inbound: the MCP endpoint accepts an assertion only from " +
			"the issuers listed below.",
		"order":               6,
		"icon":                "hub",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			{"key": platformconfig.KeyRemoteAgentsOutboundAssertions, "type": "string", "title": "Outbound Assertions",
				"description": "off (the default) sends no assertion; allowlist sends one only to the origins listed in " +
					"Assertion Audiences; on sends one to every remote MCP server. Governed runs always send one.",
				"section": section, "default": string(defaults.OutboundAssertions),
				"enum": []string{string(platformconfig.OutboundAssertionsOff), string(platformconfig.OutboundAssertionsAllowlist),
					string(platformconfig.OutboundAssertionsOn)}},
			{"key": platformconfig.KeyRemoteAgentsAssertionAudiences, "type": "array", "items": map[string]any{"type": "string"},
				"title": "Assertion Audiences",
				"description": fmt.Sprintf("With allowlist: the origins (https://host or https://host:port) that "+
					"receive assertions. At most %d.", platformconfig.MaxRemoteAgentAudiences),
				"section": section, "default": []any{}, "maxItems": platformconfig.MaxRemoteAgentAudiences},
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
		if !platformconfig.ValidOutboundAssertionsMode(platformconfig.OutboundAssertionsMode(mode)) {
			return fmt.Sprintf("%q must be off, allowlist or on", platformconfig.KeyRemoteAgentsOutboundAssertions)
		}
	}
	if reason := validateAssertionAudiences(values); reason != "" {
		return reason
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

// validateAssertionAudiences refuses an entry that is not an https origin:
// the mint compares origins, so a path or a typo would never match.
func validateAssertionAudiences(values map[string]any) string {
	raw, present := values[platformconfig.KeyRemoteAgentsAssertionAudiences]
	if !present {
		return ""
	}
	items, ok := raw.([]any)
	if !ok {
		return fmt.Sprintf("%q must be an array", platformconfig.KeyRemoteAgentsAssertionAudiences)
	}
	if len(items) > platformconfig.MaxRemoteAgentAudiences {
		return fmt.Sprintf("%q holds at most %d entries", platformconfig.KeyRemoteAgentsAssertionAudiences, platformconfig.MaxRemoteAgentAudiences)
	}
	for index, item := range items {
		entry, _ := item.(string)
		if _, ok := remoteagent.AudienceOrigin(entry); !ok {
			return fmt.Sprintf("%q[%d] must be an https origin (https://host or https://host:port)",
				platformconfig.KeyRemoteAgentsAssertionAudiences, index)
		}
	}
	return ""
}
