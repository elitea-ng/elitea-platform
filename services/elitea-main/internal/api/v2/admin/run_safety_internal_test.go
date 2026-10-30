package admin

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func TestRunSafetySectionsAreDeclared(t *testing.T) {
	t.Parallel()
	for _, id := range []string{platformconfig.SectionSessionPolicy, platformconfig.SectionRunCaps} {
		section, ok := findConfigSection(id)
		if !ok {
			t.Fatalf("section %q is not declared", id)
		}
		if _, unavailable := section.raw["unavailable_reason"]; unavailable {
			t.Fatalf("section %q is declared unavailable", id)
		}
		if section.raw["required_permission"] == "" {
			t.Fatalf("section %q has no permission", id)
		}
	}
}

func TestValidateRunSafetyValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		section string
		values  map[string]any
		ok      bool
	}{
		{platformconfig.SectionSessionPolicy, map[string]any{"max_idle_timeout_seconds": float64(0)}, true},
		{platformconfig.SectionSessionPolicy, map[string]any{"max_idle_timeout_seconds": float64(1800)}, true},
		{platformconfig.SectionSessionPolicy, map[string]any{"max_idle_timeout_seconds": float64(30)}, false},
		{platformconfig.SectionSessionPolicy, map[string]any{"max_absolute_lifetime_seconds": float64(1.5)}, false},
		{platformconfig.SectionSessionPolicy, map[string]any{"max_absolute_lifetime_seconds": float64(400 * 24 * 3600)}, false},
		{platformconfig.SectionRunCaps, map[string]any{"max_wall_seconds": float64(3600)}, true},
		{platformconfig.SectionRunCaps, map[string]any{"max_wall_seconds": float64(-1)}, false},
		{platformconfig.SectionRunCaps, map[string]any{"max_wall_seconds": float64(8 * 24 * 3600)}, false},
		{platformconfig.SectionWorkerEgress, map[string]any{"max_wall_seconds": float64(-1)}, true},
		// ADR-0041 decision 3: the per-tier caps.
		{platformconfig.SectionRunCaps, map[string]any{"high_max_tool_calls": float64(50), "high_max_spend": 2.5}, true},
		{platformconfig.SectionRunCaps, map[string]any{"critical_max_records_read": float64(0)}, true},
		{platformconfig.SectionRunCaps, map[string]any{"low_max_tool_calls": 2.5}, false},
		{platformconfig.SectionRunCaps, map[string]any{"medium_max_spend": 0.00001}, false},
		{platformconfig.SectionRunCaps, map[string]any{"medium_max_model_turns": float64(-1)}, false},
		{platformconfig.SectionRunCaps, map[string]any{"medium_max_model_turns": "10"}, false},
	} {
		reason := validateRunSafetyValues(test.section, test.values)
		if (reason == "") != test.ok {
			t.Errorf("%s %v: reason %q, want ok=%t", test.section, test.values, reason, test.ok)
		}
	}
}

// The Run caps form shows every tier's four caps, so the generic renderer
// needs nothing of its own.
func TestRunCapsSectionDeclaresEveryTierCap(t *testing.T) {
	t.Parallel()
	section, ok := findConfigSection(platformconfig.SectionRunCaps)
	if !ok {
		t.Fatal("run_caps is not declared")
	}
	keys := map[string]bool{}
	fields, _ := section.raw["fields"].([]map[string]any)
	for _, field := range fields {
		key, _ := field["key"].(string)
		keys[key] = true
	}
	for _, tier := range agentlifecycle.Tiers {
		for _, name := range platformconfig.RunCapNames {
			if key := platformconfig.RunCapKey(tier, name); !keys[key] {
				t.Errorf("run_caps does not declare %s (declared %v)", key, keys)
			}
		}
	}
	if !keys[platformconfig.KeyRunMaxWallSeconds] {
		t.Error("run_caps dropped max_wall_seconds")
	}
}
