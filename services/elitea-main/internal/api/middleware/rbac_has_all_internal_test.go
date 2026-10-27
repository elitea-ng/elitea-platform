package middleware

import "testing"

// hasAll is the AND counterpart added for the governed credential decision
// (#1116). hasIntersection keeps its any-overlap answer; both are pinned
// here side by side so a change to either shows.
func TestHasAllRequiresEveryPermissionAndHasIntersectionStaysAnyOf(t *testing.T) {
	cases := []struct {
		name               string
		required, held     []string
		wantAll, wantInter bool
	}{
		{"all held", []string{"a", "b"}, []string{"a", "b", "c"}, true, true},
		{"one of two held", []string{"a", "b"}, []string{"a"}, false, true},
		{"none held", []string{"a", "b"}, []string{"c"}, false, false},
		{"nothing required", nil, []string{"a"}, false, false},
		{"nothing held", []string{"a"}, nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			required, held := permissionSet(tc.required), permissionSet(tc.held)
			if got := hasAll(required, held); got != tc.wantAll {
				t.Errorf("hasAll = %v, want %v", got, tc.wantAll)
			}
			if got := HoldsAllPermissions(tc.required, tc.held); got != tc.wantAll {
				t.Errorf("HoldsAllPermissions = %v, want %v", got, tc.wantAll)
			}
			if got := hasIntersection(required, held); got != tc.wantInter {
				t.Errorf("hasIntersection = %v, want %v", got, tc.wantInter)
			}
		})
	}
}
