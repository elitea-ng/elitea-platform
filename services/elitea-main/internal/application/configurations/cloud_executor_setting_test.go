package configurations

import (
	"reflect"
	"testing"
)

func TestCloudExecutorSettingNormalizer(t *testing.T) {
	result, err := CurrentLocalConfigurationCreateNormalizer{}.NormalizeCreate(CloudExecutorConfigurationType, map[string]any{
		"enabled": true, "shell": false, "network": nil, "history": "none",
		"network_allowlist": []any{"github.com"}, "max_segment_mb": float64(50), "unknown": "dropped",
	})
	if err != nil || !result.Complete {
		t.Fatalf("normalize = %+v, %v", result, err)
	}
	want := map[string]any{
		"enabled": true, "shell": false, "history": "none",
		"network_allowlist": []any{"github.com"}, "max_segment_mb": int64(50),
	}
	if !reflect.DeepEqual(result.Data, want) {
		t.Fatalf("normalized = %#v, want %#v", result.Data, want)
	}
	for name, data := range map[string]map[string]any{
		"a string switch":    {"shell": "yes"},
		"an unknown history": {"history": "partial"},
		"a boolean cap":      {"max_files": true},
		"a negative cap":     {"max_files": float64(-1)},
		"a non-list":         {"network_allowlist": "github.com"},
	} {
		if _, err := (CurrentLocalConfigurationCreateNormalizer{}).NormalizeCreate(CloudExecutorConfigurationType, data); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}
