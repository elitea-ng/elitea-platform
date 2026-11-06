package configurations

import (
	"reflect"
	"testing"
)

func TestNormalizeBrowserSetting(t *testing.T) {
	t.Parallel()
	defaults, err := normalizeBrowserSetting(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if want := (map[string]any{"enabled": false, "engine": "full", "allow_domains": []any{}}); !reflect.DeepEqual(defaults, want) {
		t.Fatalf("defaults = %v, want %v", defaults, want)
	}
	normalized, err := normalizeBrowserSetting(map[string]any{
		"enabled": true, "engine": "full", "extra": 1,
		"allow_domains": []any{"example.com", "*.example.org", "example.com", "example.net:8443"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"enabled": true, "engine": "full",
		"allow_domains": []any{"example.com", "*.example.org", "example.net:8443"}}
	if !reflect.DeepEqual(normalized, want) {
		t.Fatalf("normalized = %v, want %v", normalized, want)
	}
	many := make([]any, 129)
	for index := range many {
		many[index] = string(rune('a'+index%26)) + string(rune('a'+index/26)) + ".example.com"
	}
	for _, data := range []map[string]any{
		nil,
		{"enabled": "yes"},
		{"engine": "lite"},
		{"engine": 1},
		{"allow_domains": "example.com"},
		{"allow_domains": []any{"https://example.com/path"}},
		{"allow_domains": []any{" "}},
		{"allow_domains": []any{7}},
		{"allow_domains": many},
	} {
		if _, err := normalizeBrowserSetting(data); err == nil {
			t.Errorf("%v: want a refusal", data)
		}
	}
}

func TestBrowserSettingIsALocalProjectSetting(t *testing.T) {
	t.Parallel()
	result, err := CurrentLocalConfigurationCreateNormalizer{}.NormalizeCreate("browser", map[string]any{"enabled": true})
	if err != nil || !result.Complete || result.Data["enabled"] != true {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if !currentLocalConfigurationType("browser") {
		t.Fatal("browser is not a local configuration type")
	}
}
