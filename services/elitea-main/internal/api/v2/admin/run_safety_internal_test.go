package admin

import (
	"testing"

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
	} {
		reason := validateRunSafetyValues(test.section, test.values)
		if (reason == "") != test.ok {
			t.Errorf("%s %v: reason %q, want ok=%t", test.section, test.values, reason, test.ok)
		}
	}
}
