package main

import (
	"errors"
	"testing"
)

func TestExpectedReplicas(t *testing.T) {
	env := func(v string, ok bool) func(string) (string, bool) {
		return func(k string) (string, bool) {
			if k != expectedReplicasEnv {
				return "", false
			}
			return v, ok
		}
	}
	accepted := []struct {
		v    string
		ok   bool
		want int
	}{
		{"", false, 1},
		{"", true, 1},
		{"  ", true, 1},
		{"1", true, 1},
		{"2", true, 2},
		{"3", true, 3},
		{" 7 ", true, 7},
		{"40", true, 40},
	}
	for _, tc := range accepted {
		got, err := expectedReplicas(env(tc.v, tc.ok))
		if err != nil || got != tc.want {
			t.Errorf("expectedReplicas(%q, set=%v) = %d, %v; want %d, nil", tc.v, tc.ok, got, err, tc.want)
		}
	}
	for _, bad := range []string{"0", "-1", "-40", "two", "1.5", "3x", "0x2"} {
		got, err := expectedReplicas(env(bad, true))
		if !errors.Is(err, errExpectedReplicas) {
			t.Errorf("expectedReplicas(%q) = %d, %v; want a refusal (errExpectedReplicas)", bad, got, err)
		}
	}
}
