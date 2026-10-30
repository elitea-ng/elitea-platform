package admin

import "testing"

func TestEntitlementExportSectionIsGatedOnTheExportPermission(t *testing.T) {
	section := entitlementExportSection()
	if section["id"] != "entitlement_export" || section["required_permission"] != "entitlements.export" {
		t.Fatalf("section=%v", section)
	}
	fields, _ := section["fields"].([]map[string]any)
	if len(fields) != 1 || fields[0]["key"] != "scim_client_ids" || fields[0]["type"] != "array" {
		t.Fatalf("fields=%v", fields)
	}
	if items, _ := fields[0]["items"].(map[string]any); items["type"] != "integer" {
		t.Fatalf("items=%v", fields[0]["items"])
	}
	if defaults, _ := fields[0]["default"].([]any); len(defaults) != 0 {
		t.Fatalf("the default lists clients: %v", fields[0]["default"])
	}
	found := false
	for _, candidate := range configSections() {
		if candidate["id"] == "entitlement_export" {
			found = true
		}
	}
	if !found {
		t.Fatal("entitlement_export is not a configuration section")
	}
}

func TestValidateEntitlementExportValues(t *testing.T) {
	if got := validateEntitlementExportValues(map[string]any{"scim_client_ids": []any{float64(4)}}); got != "" {
		t.Fatalf("valid list refused: %q", got)
	}
	if got := validateEntitlementExportValues(map[string]any{"scim_client_ids": []any{"4"}}); got == "" {
		t.Fatal("a string id was accepted")
	}
}
