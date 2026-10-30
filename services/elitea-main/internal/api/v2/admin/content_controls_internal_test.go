package admin

import (
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func TestValidateContentControlsValues(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		section string
		values  map[string]any
		refused bool
	}{
		"marker":               {platformconfig.SectionAIDisclosure, map[string]any{"marker_text": "AI", "opt_out_toolkit_types": []any{"slack", "Jira"}}, false},
		"long marker":          {platformconfig.SectionAIDisclosure, map[string]any{"marker_text": strings.Repeat("a", 201)}, true},
		"unknown opt-out":      {platformconfig.SectionAIDisclosure, map[string]any{"opt_out_toolkit_types": []any{"confluence"}}, true},
		"opt-out not a list":   {platformconfig.SectionAIDisclosure, map[string]any{"opt_out_toolkit_types": "slack"}, true},
		"preamble":             {platformconfig.SectionPlatformPreamble, map[string]any{"text": "Be careful."}, false},
		"long preamble":        {platformconfig.SectionPlatformPreamble, map[string]any{"text": strings.Repeat("a", 8193)}, true},
		"NUL preamble":         {platformconfig.SectionPlatformPreamble, map[string]any{"text": "a\x00b"}, true},
		"rule tiers":           {platformconfig.SectionApprovalPolicy, map[string]any{"untrusted_read_then_act_tiers": []any{"low"}}, false},
		"unknown rule tier":    {platformconfig.SectionApprovalPolicy, map[string]any{"untrusted_read_then_act_tiers": []any{"top"}}, true},
		"other section passes": {"guardrails", map[string]any{"text": 7}, false},
	} {
		if refused := validateContentControlsValues(test.section, test.values) != ""; refused != test.refused {
			t.Errorf("%s: refused = %v, want %v", name, refused, test.refused)
		}
	}
}

func TestContentControlSectionsAreRegistered(t *testing.T) {
	t.Parallel()
	found := map[string]map[string]any{}
	for _, section := range configSections() {
		id, _ := section["id"].(string)
		found[id] = section
	}
	for _, id := range []string{platformconfig.SectionAIDisclosure, platformconfig.SectionPlatformPreamble} {
		if found[id] == nil || found[id]["required_permission"] != "configuration.governance" {
			t.Errorf("section %q is not registered under configuration.governance", id)
		}
	}
	keys := []string{}
	for _, field := range found[platformconfig.SectionApprovalPolicy]["fields"].([]map[string]any) {
		keys = append(keys, field["key"].(string))
	}
	if !strings.Contains(strings.Join(keys, ","), platformconfig.KeyApprovalUntrustedReadThenActTiers) {
		t.Errorf("approval policy fields = %v", keys)
	}
}
