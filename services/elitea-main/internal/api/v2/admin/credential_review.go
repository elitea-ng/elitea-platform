package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The Credential Review Configuration section (ADR-0044 decision 5,
// #1501): when a credential is stale, what the daily main.credential_staleness
// routine does with it, and the window of the permission trimming report.
// It is LIVE: the routines and the inventory and trimming reads load it
// each time. It uses configuration.governance, the permission of the other
// runtime governance sections, so no migration seeds a new one for it.
func credentialReviewSection() map[string]any {
	const section = platformconfig.SectionCredentialReview
	defaults := platformconfig.DefaultCredentialReviewPolicy()
	return map[string]any{
		"id":    section,
		"title": "Credential Review",
		"description": "When a credential an agent can use counts as stale, what happens to it, and how far back " +
			"the permission trimming report looks for use. Revoke applies to personal access tokens and " +
			"credential use grants only; toolkit secrets, MCP OAuth tokens and credential references are " +
			"flagged, because the system that issued them rotates and revokes them.",
		"order":               9,
		"icon":                "key",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			{
				"key":   platformconfig.KeyStaleAfterDays,
				"type":  "integer",
				"title": "Stale After (days)",
				"description": fmt.Sprintf("A credential not used for this many days, or never used and created "+
					"that long ago, is stale. From %d to %d.", platformconfig.MinStaleAfterDays,
					platformconfig.MaxStaleAfterDays),
				"section": section,
				"default": defaults.StaleAfterDays,
				"minimum": platformconfig.MinStaleAfterDays,
				"maximum": platformconfig.MaxStaleAfterDays,
			},
			{
				"key":   platformconfig.KeyStaleAction,
				"type":  "string",
				"enum":  []string{string(platformconfig.StaleActionFlag), string(platformconfig.StaleActionRevoke)},
				"title": "Stale Credential Action",
				"description": "flag lists the credential as stale and notifies its owner. revoke also revokes a " +
					"stale personal access token or credential use grant, audited.",
				"section": section,
				"default": string(defaults.StaleAction),
			},
			{
				"key":   platformconfig.KeyTrimmingWindowDays,
				"type":  "integer",
				"title": "Permission Trimming Window (days)",
				"description": fmt.Sprintf("A granted tool, toolkit, credential grant or egress host an agent did "+
					"not use in this many days is a removal candidate. From %d to %d.",
					platformconfig.MinTrimmingWindowDays, platformconfig.MaxTrimmingWindowDays),
				"section": section,
				"default": defaults.TrimmingWindowDays,
				"minimum": platformconfig.MinTrimmingWindowDays,
				"maximum": platformconfig.MaxTrimmingWindowDays,
			},
		},
	}
}

// validateCredentialReviewValues applies the integrality the schema cannot
// enforce.
func validateCredentialReviewValues(section string, values map[string]any) string {
	if section != platformconfig.SectionCredentialReview {
		return ""
	}
	for _, b := range []struct {
		key            string
		minimum, limit int64
	}{
		{platformconfig.KeyStaleAfterDays, platformconfig.MinStaleAfterDays, platformconfig.MaxStaleAfterDays},
		{platformconfig.KeyTrimmingWindowDays, platformconfig.MinTrimmingWindowDays, platformconfig.MaxTrimmingWindowDays},
	} {
		raw, present := values[b.key]
		if !present {
			continue
		}
		number, ok := raw.(float64)
		if !ok || number != float64(int64(number)) || int64(number) < b.minimum || int64(number) > b.limit {
			return fmt.Sprintf("%q must be a whole number from %d to %d", b.key, b.minimum, b.limit)
		}
	}
	if raw, present := values[platformconfig.KeyStaleAction]; present {
		text, _ := raw.(string)
		if !platformconfig.StaleAction(text).Valid() {
			return fmt.Sprintf("%q must be flag or revoke", platformconfig.KeyStaleAction)
		}
	}
	return ""
}
