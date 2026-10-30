package admin

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func TestDelegationSectionIsDeclared(t *testing.T) {
	t.Parallel()
	section, ok := findConfigSection(platformconfig.SectionDelegation)
	if !ok {
		t.Fatal("the delegation section is not declared")
	}
	if _, unavailable := section.raw["unavailable_reason"]; unavailable {
		t.Fatal("the delegation section is declared unavailable")
	}
	if section.raw["required_permission"] != "configuration.governance" {
		t.Fatalf("permission = %v", section.raw["required_permission"])
	}
}

func TestValidateDelegationValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		section string
		values  map[string]any
		ok      bool
	}{
		{platformconfig.SectionDelegation, map[string]any{"max_depth": float64(4), "mode": "enforce"}, true},
		{platformconfig.SectionDelegation, map[string]any{"max_depth": float64(16), "mode": "report"}, true},
		{platformconfig.SectionDelegation, map[string]any{"max_depth": float64(0)}, false},
		{platformconfig.SectionDelegation, map[string]any{"max_depth": float64(17)}, false},
		{platformconfig.SectionDelegation, map[string]any{"max_depth": float64(2.5)}, false},
		{platformconfig.SectionDelegation, map[string]any{"mode": "off"}, false},
		{platformconfig.SectionRunCaps, map[string]any{"max_depth": float64(0)}, true},
	} {
		reason := validateDelegationValues(test.section, test.values)
		if (reason == "") != test.ok {
			t.Errorf("%s %v: reason %q, want ok=%t", test.section, test.values, reason, test.ok)
		}
	}
}

type countingInvalidator struct{ calls int }

func (c *countingInvalidator) Invalidate() { c.calls++ }

func TestDelegationSaveDropsTheCache(t *testing.T) {
	t.Parallel()
	cache := &countingInvalidator{}
	h := &Handler{}
	WithDelegationCache(cache)(h)
	h.invalidateDelegation(platformconfig.SectionRunCaps)
	h.invalidateDelegation(platformconfig.SectionDelegation)
	if cache.calls != 1 {
		t.Fatalf("invalidations = %d, want 1", cache.calls)
	}
}
