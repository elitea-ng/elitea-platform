package platformconfig

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
)

func TestMemoryAutoMaxTierFromValues(t *testing.T) {
	cases := map[string]struct {
		values Values
		want   agentlifecycle.Tier
	}{
		"absent keeps the default":   {Values{}, agentlifecycle.TierMedium},
		"a tier is read":             {Values{KeyMemoryAutoMaxTier: "high"}, agentlifecycle.TierHigh},
		"a non-tier keeps default":   {Values{KeyMemoryAutoMaxTier: "huge"}, agentlifecycle.TierMedium},
		"a non-string keeps default": {Values{KeyMemoryAutoMaxTier: 3.0}, agentlifecycle.TierMedium},
	}
	for name, tc := range cases {
		if got := MemoryAutoMaxTierFromValues(tc.values); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}
