package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// catalogueSection is the connector catalogue mode (ADR-0044 decision 1,
// #1501). It is LIVE: the toolkit type catalogue, the toolkit and MCP write
// and discovery paths, the agent tool freeze and every worker command's
// preparation read it. It shares the governance permission
// (`configuration.governance`) with Worker Egress; the approval queue has its
// own permission (connector.approve, shared 0207).
func catalogueSection() map[string]any {
	const section = platformconfig.SectionCatalogue
	defaults := platformconfig.DefaultCatalogueSettings()
	return map[string]any{
		"id":    section,
		"title": "Connector Catalogue",
		"description": "Which toolkits and MCP servers projects may use. Open (the default) offers every toolkit type " +
			"the toolkit policy and guardrails allow. Approved only makes a toolkit type, a pre-built MCP server or a " +
			"custom MCP server URL usable only by the projects an approved connector request names; approval also " +
			"adds the request's hosts to the worker egress allowlist. Runs under the governed credential profile " +
			"always use approved only. Review requests under Connector Requests.",
		"order":               6,
		"icon":                "verified_user",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			{"key": platformconfig.KeyCatalogueMode, "type": "string", "title": "Mode",
				"description": "open (the default) or approved_only.", "section": section,
				"default": string(defaults.Mode),
				"enum":    []string{string(platformconfig.CatalogueOpen), string(platformconfig.CatalogueApprovedOnly)}},
			{"key": platformconfig.KeyCatalogueRequireArchitectureAttestation, "type": "boolean",
				"title": "Require an Architecture Attestation",
				"description": "A connector request is approved only while the requesting project holds a valid " +
					"architecture attestation of it (subject connector, the request id).",
				"section": section, "default": defaults.RequireArchitectureAttestation},
		},
	}
}

// validateCatalogueValues refuses a mode the readers would read as
// approved_only by accident.
func validateCatalogueValues(values map[string]any) string {
	if raw, present := values[platformconfig.KeyCatalogueMode]; present {
		mode, _ := raw.(string)
		switch platformconfig.CatalogueMode(mode) {
		case platformconfig.CatalogueOpen, platformconfig.CatalogueApprovedOnly:
		default:
			return fmt.Sprintf("%q must be open or approved_only", platformconfig.KeyCatalogueMode)
		}
	}
	if raw, present := values[platformconfig.KeyCatalogueRequireArchitectureAttestation]; present {
		if _, ok := raw.(bool); !ok {
			return fmt.Sprintf("%q must be a boolean", platformconfig.KeyCatalogueRequireArchitectureAttestation)
		}
	}
	return ""
}
