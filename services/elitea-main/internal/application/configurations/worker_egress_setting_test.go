package configurations

import (
	"reflect"
	"strings"
	"testing"
)

func TestWorkerEgressSettingNormalizer(t *testing.T) {
	result, err := CurrentLocalConfigurationCreateNormalizer{}.NormalizeCreate(WorkerEgressConfigurationType, map[string]any{
		"allowlist": []any{"api.example.com", "*.example.org:8443", "10.0.0.0/8"}, "unknown": "dropped",
	})
	if err != nil || !result.Complete {
		t.Fatalf("normalize = %+v, %v", result, err)
	}
	want := map[string]any{"allowlist": []any{"api.example.com", "*.example.org:8443", "10.0.0.0/8"}}
	if !reflect.DeepEqual(result.Data, want) {
		t.Fatalf("normalized = %#v, want %#v", result.Data, want)
	}
	if empty, err := (CurrentLocalConfigurationCreateNormalizer{}).NormalizeCreate(WorkerEgressConfigurationType, map[string]any{}); err != nil ||
		len(empty.Data) != 0 {
		t.Fatalf("no allowlist = %+v, %v", empty, err)
	}
	tooMany := make([]any, 257)
	for i := range tooMany {
		tooMany[i] = "example.com"
	}
	for name, data := range map[string]map[string]any{
		"a scheme":         {"allowlist": []any{"https://example.com"}},
		"a path":           {"allowlist": []any{"example.com/path"}},
		"a blank":          {"allowlist": []any{" "}},
		"a mid wildcard":   {"allowlist": []any{"api.*.example.com"}},
		"a non-string":     {"allowlist": []any{float64(1)}},
		"a non-list":       {"allowlist": "example.com"},
		"too many":         {"allowlist": tooMany},
		"an over-long one": {"allowlist": []any{strings.Repeat("a", 300) + ".com"}},
	} {
		if _, err := (CurrentLocalConfigurationCreateNormalizer{}).NormalizeCreate(WorkerEgressConfigurationType, data); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}
