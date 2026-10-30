package admin

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func TestApprovalPolicySectionIsDeclared(t *testing.T) {
	t.Parallel()
	section, ok := findConfigSection(platformconfig.SectionApprovalPolicy)
	if !ok {
		t.Fatal("the approval policy section is not declared")
	}
	if _, unavailable := section.raw["unavailable_reason"]; unavailable {
		t.Fatal("the approval policy section is declared unavailable")
	}
	if section.raw["required_permission"] != "configuration.governance" {
		t.Fatalf("permission = %v", section.raw["required_permission"])
	}
	fields, _ := section.raw["fields"].([]map[string]any)
	keys := map[string]any{}
	for _, field := range fields {
		keys[field["key"].(string)] = field["default"]
	}
	for _, key := range []string{"low_approval_categories", "medium_approval_categories",
		"high_approval_categories", "critical_approval_categories", "require_hitl_tiers"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("the section has no %q field", key)
		}
	}
	if high, _ := keys["high_approval_categories"].([]any); len(high) != 3 {
		t.Errorf("high default = %v, want delete, external_send and payment", keys["high_approval_categories"])
	}
	if low, _ := keys["low_approval_categories"].([]any); len(low) != 0 {
		t.Errorf("low default = %v", keys["low_approval_categories"])
	}
}

func TestValidateApprovalPolicyValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		section string
		values  map[string]any
		ok      bool
	}{
		{platformconfig.SectionApprovalPolicy, map[string]any{"high_approval_categories": []any{"delete", "payment"}}, true},
		{platformconfig.SectionApprovalPolicy, map[string]any{"low_approval_categories": []any{}}, true},
		{platformconfig.SectionApprovalPolicy, map[string]any{"require_hitl_tiers": []any{"high", "critical"}}, true},
		{platformconfig.SectionApprovalPolicy, map[string]any{"high_approval_categories": []any{"admin"}}, false},
		{platformconfig.SectionApprovalPolicy, map[string]any{"high_approval_categories": []any{float64(1)}}, false},
		{platformconfig.SectionApprovalPolicy, map[string]any{"high_approval_categories": "delete"}, false},
		{platformconfig.SectionApprovalPolicy, map[string]any{"require_hitl_tiers": []any{"extreme"}}, false},
		{platformconfig.SectionApprovalPolicy, map[string]any{"require_hitl_tiers": "high"}, false},
		{platformconfig.SectionDelegation, map[string]any{"require_hitl_tiers": "high"}, true},
	} {
		reason := validateApprovalPolicyValues(test.section, test.values)
		if (reason == "") != test.ok {
			t.Errorf("%s %v: reason %q, want ok=%t", test.section, test.values, reason, test.ok)
		}
	}
}

func TestApprovalPolicySaveDropsTheCache(t *testing.T) {
	t.Parallel()
	cache := &countingInvalidator{}
	h := &Handler{}
	WithApprovalPolicyCache(cache)(h)
	h.invalidateApprovalPolicy(platformconfig.SectionDelegation)
	h.invalidateApprovalPolicy(platformconfig.SectionApprovalPolicy)
	if cache.calls != 1 {
		t.Fatalf("invalidations = %d, want 1", cache.calls)
	}
}
