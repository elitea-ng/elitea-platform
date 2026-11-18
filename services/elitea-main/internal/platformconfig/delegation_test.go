package platformconfig

import (
	"math"
	"testing"
)

func TestDelegationSettingsDefaults(t *testing.T) {
	t.Parallel()
	settings := DelegationSettingsFromValues(Values{})
	if settings != DefaultDelegationSettings() || settings.MaxDepth != 4 || settings.Mode != DelegationEnforce || settings.Stored || !settings.PrincipalBound {
		t.Fatalf("defaults = %+v", settings)
	}
}

// A section an administrator saved is stored even when every value equals
// the default: Main sends the policy only then (the chart rolls Main out
// before the workers, and an older worker rejects the field).
func TestDelegationSettingsStoredOnlyWhenSaved(t *testing.T) {
	t.Parallel()
	saved := DelegationSettingsFromValues(Values{"max_depth": float64(4), "mode": "enforce"})
	if !saved.Stored || saved.MaxDepth != 4 || saved.Mode != DelegationEnforce {
		t.Fatalf("saved defaults = %+v", saved)
	}
}

func TestDelegationSettingsFromValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		values Values
		want   DelegationSettings
	}{
		{"saved", Values{"max_depth": float64(2), "mode": "report"}, DelegationSettings{MaxDepth: 2, Mode: DelegationReport, PrincipalBound: true, Stored: true}},
		{"highest", Values{"max_depth": float64(16)}, DelegationSettings{MaxDepth: 16, Mode: DelegationEnforce, PrincipalBound: true, Stored: true}},
		{"clamped low", Values{"max_depth": float64(0)}, DelegationSettings{MaxDepth: 1, Mode: DelegationEnforce, PrincipalBound: true, Stored: true}},
		{"clamped high", Values{"max_depth": float64(99)}, DelegationSettings{MaxDepth: 16, Mode: DelegationEnforce, PrincipalBound: true, Stored: true}},
		{"unreadable depth tightens", Values{"max_depth": "deep"}, DelegationSettings{MaxDepth: 1, Mode: DelegationEnforce, PrincipalBound: true, Stored: true}},
		{"report keeps the principal bound", Values{"mode": "report"}, DelegationSettings{MaxDepth: 4, Mode: DelegationReport, PrincipalBound: true, Stored: true}},
		{"unknown mode enforces", Values{"mode": "off"}, DelegationSettings{MaxDepth: 4, Mode: DelegationEnforce, PrincipalBound: true, Stored: true}},
		{"principal bound on", Values{"principal_bound": true}, DelegationSettings{MaxDepth: 4, Mode: DelegationEnforce, PrincipalBound: true, Stored: true}},
		{"principal bound off", Values{"principal_bound": false}, DelegationSettings{MaxDepth: 4, Mode: DelegationEnforce, Stored: true}},
		{"unreadable principal bound stays on", Values{"principal_bound": "yes"}, DelegationSettings{MaxDepth: 4, Mode: DelegationEnforce, PrincipalBound: true, Stored: true}},
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
