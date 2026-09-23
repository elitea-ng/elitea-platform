package executions

import (
	"strings"
	"testing"
)

func mapSSELookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestSSEStreamLimitsFromEnvKeepsDefaultsWhenUnset(t *testing.T) {
	limits, err := SSEStreamLimitsFromEnv(mapSSELookup(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultSSEStreamLimits()
	if limits != want {
		t.Fatalf("unset limits = %+v, want defaults %+v", limits, want)
	}
}

func TestSSEStreamLimitsFromEnvAppliesOverrides(t *testing.T) {
	limits, err := SSEStreamLimitsFromEnv(mapSSELookup(map[string]string{
		"ELITEA_RUNTIME_SSE_MAX_STREAMS":               "32",
		"ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PRINCIPAL": "8",
		"ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PROJECT":   "16",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := SSEStreamLimits{MaxStreams: 32, MaxPerPrincipal: 8, MaxPerProject: 16}
	if limits != want {
		t.Fatalf("limits = %+v, want %+v", limits, want)
	}
}

func TestSSEStreamLimitsFromEnvRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"0", "-1", "007", "+4", "4.0", "abc", "3000000000"} {
		t.Run(value, func(t *testing.T) {
			environment := map[string]string{"ELITEA_RUNTIME_SSE_MAX_STREAMS": value}
			_, err := SSEStreamLimitsFromEnv(mapSSELookup(environment))
			if err == nil ||
				!strings.Contains(err.Error(), "ELITEA_RUNTIME_SSE_MAX_STREAMS") ||
				!strings.Contains(err.Error(), "canonical positive integer") {
				t.Fatalf("value %q error = %v", value, err)
			}
		})
	}
}

func TestSSEStreamLimitsFromEnvRejectsSubLimitAboveGlobal(t *testing.T) {
	tests := map[string]map[string]string{
		"principal above global": {
			"ELITEA_RUNTIME_SSE_MAX_STREAMS":               "16",
			"ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PRINCIPAL": "17",
		},
		"project above global": {
			"ELITEA_RUNTIME_SSE_MAX_STREAMS":             "16",
			"ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PROJECT": "17",
		},
	}
	for name, environment := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := SSEStreamLimitsFromEnv(mapSSELookup(environment))
			if err == nil || !strings.Contains(err.Error(), "ELITEA_RUNTIME_SSE_MAX_STREAMS") {
				t.Fatalf("%s: error = %v", name, err)
			}
		})
	}
}
