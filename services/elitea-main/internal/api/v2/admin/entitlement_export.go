package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// entitlementExportSection is the SCIM opt-in of the entitlement export
// (ADR-0044 decision 5, #1501). It is LIVE: the SCIM tree at
// /api/v2/scim/v2/entitlements reads it on every request and refuses (403)
// any client not listed, so an existing provisioning credential reveals no
// more after the upgrade than before. It shares the export's own permission
// (`entitlements.export`, seeded on super_admin and admin by shared 0209): who
// may download the CSV decides which tool may read the same data over SCIM.
// Writes go through the administration config save, which the admin-surface
// audit middleware records like every other section change.
func entitlementExportSection() map[string]any {
	const section = platformconfig.SectionEntitlementExport
	return map[string]any{
		"id":    section,
		"title": "Entitlement Export",
		"description": "Which SCIM clients may read the read-only entitlement tree at /api/v2/scim/v2/entitlements " +
			"(who holds which role and what it allows, for access reviews). Empty (the default) refuses every " +
			"client. The admin CSV download on the Users page is not affected.",
		"order":               7,
		"icon":                "fact_check",
		"always_visible":      true,
		"required_permission": "entitlements.export",
		"fields": []map[string]any{
			{
				"key":   platformconfig.KeyEntitlementExportSCIMClientIDs,
				"type":  "array",
				"items": map[string]any{"type": "integer"},
				"title": "SCIM Clients Allowed",
				"description": fmt.Sprintf("Ids of the SCIM clients (Configuration › Authentication › SCIM clients) "+
					"that may read the entitlement tree. A provisioning client not listed here keeps its provisioning "+
					"access and gets 403 on the tree. At most %d.", platformconfig.MaxEntitlementExportSCIMClients),
				"section":  section,
				"default":  []any{},
				"maxItems": platformconfig.MaxEntitlementExportSCIMClients,
			},
		},
	}
}

// validateEntitlementExportValues refuses a list entry the SCIM gate would
// drop: a saved id that silently matched nothing is worse than a form that
// says which entry is wrong.
func validateEntitlementExportValues(values map[string]any) string {
	if err := platformconfig.ValidateEntitlementExportValues(values); err != nil {
		return err.Error()
	}
	return ""
}
