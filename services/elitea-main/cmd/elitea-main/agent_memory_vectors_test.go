package main

import "testing"

func TestAgentMemoryVectorsSwitch(t *testing.T) {
	lookup := func(value string, set bool) func(string) (string, bool) {
		return func(key string) (string, bool) {
			if key != agentMemoryVectorsEnv {
				return "", false
			}
			return value, set
		}
	}
	for name, test := range map[string]struct {
		value string
		set   bool
		want  bool
		fails bool
	}{
		"unset is on":    {"", false, true, false},
		"blank is on":    {"  ", true, true, false},
		"true":           {"true", true, true, false},
		"false":          {"false", true, false, false},
		"zero":           {"0", true, false, false},
		"a typo is bad":  {"flase", true, false, true},
		"padded false":   {" false ", true, false, false},
		"yes is not 1/t": {"yes", true, false, true},
	} {
		got, err := agentMemoryVectorsEnabled(lookup(test.value, test.set))
		if (err != nil) != test.fails || (err == nil && got != test.want) {
			t.Errorf("%s: got %v, %v", name, got, err)
		}
	}
}
