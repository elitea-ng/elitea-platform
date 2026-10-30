package platformconfig

import (
	"math"
	"testing"
)

func TestDelegationSettingsDefaults(t *testing.T) {
	t.Parallel()
	settings := DelegationSettingsFromValues(Values{})
	if settings.MaxDepth != 4 || settings.Mode != DelegationEnforce || !settings.IsDefault() {
		t.Fatalf("defaults = %+v", settings)
	}
}

func TestDelegationSettingsFromValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		values Values
		want   DelegationSettings
	}{
		{"saved", Values{"max_depth": float64(2), "mode": "report"}, DelegationSettings{MaxDepth: 2, Mode: DelegationReport}},
		{"highest", Values{"max_depth": float64(16)}, DelegationSettings{MaxDepth: 16, Mode: DelegationEnforce}},
		{"clamped low", Values{"max_depth": float64(0)}, DelegationSettings{MaxDepth: 1, Mode: DelegationEnforce}},
		{"clamped high", Values{"max_depth": float64(99)}, DelegationSettings{MaxDepth: 16, Mode: DelegationEnforce}},
		{"unreadable depth tightens", Values{"max_depth": "deep"}, DelegationSettings{MaxDepth: 1, Mode: DelegationEnforce}},
		{"unknown mode enforces", Values{"mode": "off"}, DelegationSettings{MaxDepth: 4, Mode: DelegationEnforce}},
	} {
		if got := DelegationSettingsFromValues(test.values); got != test.want {
			t.Errorf("%s: got %+v, want %+v", test.name, got, test.want)
		}
	}
}

func TestValidDelegationMaxDepth(t *testing.T) {
	t.Parallel()
	for raw, ok := range map[any]bool{
		float64(1): true, float64(4): true, float64(16): true,
		float64(0): false, float64(17): false, float64(2.5): false, "4": false, math.NaN(): false,
	} {
		if ValidDelegationMaxDepth(raw) != ok {
			t.Errorf("ValidDelegationMaxDepth(%v) != %t", raw, ok)
		}
	}
}
