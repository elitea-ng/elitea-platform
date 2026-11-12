package admin

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// Issue 1660: memory_auto_max_tier "" or absent is the default, so a
// whole-section save or reset passes; only a non-empty non-tier is refused.
func TestValidateAgentLifecycleValuesMemoryAutoMaxTier(t *testing.T) {
	key := platformconfig.KeyMemoryAutoMaxTier
	cases := []struct {
		name    string
		values  map[string]any
		refused bool
	}{
		{"absent", map[string]any{}, false},
		{"empty string is the default", map[string]any{key: ""}, false},
		{"null is the default", map[string]any{key: nil}, false},
		{"a tier", map[string]any{key: "high"}, false},
		{"not a tier", map[string]any{key: "huge"}, true},
		{"not a string", map[string]any{key: 3.0}, true},
	}
	for _, tc := range cases {
		got := validateAgentLifecycleValues(platformconfig.SectionAgentLifecycle, tc.values)
		if (got != "") != tc.refused {
			t.Errorf("%s: message %q, refused want %v", tc.name, got, tc.refused)
		}
	}
}
