package configurations

import (
	"reflect"
	"testing"
)

func TestNormalizeComputerUseSettingIsOffByDefault(t *testing.T) {
	t.Parallel()
	defaults, err := normalizeComputerUseSetting(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if want := (map[string]any{"upload_screenshots": false}); !reflect.DeepEqual(defaults, want) {
		t.Fatalf("defaults = %v, want %v", defaults, want)
	}
	on, err := normalizeComputerUseSetting(map[string]any{"upload_screenshots": true, "extra": 1})
	if err != nil {
		t.Fatal(err)
	}
	if want := (map[string]any{"upload_screenshots": true}); !reflect.DeepEqual(on, want) {
		t.Fatalf("on = %v, want %v", on, want)
	}
	for _, data := range []map[string]any{nil, {"upload_screenshots": "yes"}, {"upload_screenshots": 1}} {
		if _, err := normalizeComputerUseSetting(data); err == nil {
			t.Errorf("%v: want a refusal", data)
		}
	}
}

func TestComputerUseSettingIsALocalProjectSetting(t *testing.T) {
	t.Parallel()
	result, err := CurrentLocalConfigurationCreateNormalizer{}.NormalizeCreate("computer_use", map[string]any{})
	if err != nil || !result.Complete || result.Data["upload_screenshots"] != false {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if !currentLocalConfigurationType("computer_use") {
		t.Fatal("computer_use is not a local configuration type")
	}
}
