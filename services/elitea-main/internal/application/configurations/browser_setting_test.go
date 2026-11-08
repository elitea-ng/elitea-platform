package configurations

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
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

// The bake-off admitted `auto` and not `lite` (ADR-0049 appendix, run
// 42513255705): a project may store `full` or `auto`, and `lite` is refused
// with the message naming the appendix. Once lite is admitted the stored
// engine is kept.
func TestBrowserSettingEnginesFollowAdmission(t *testing.T) {
	t.Parallel()
	if !platformconfig.BrowserAutoAdmitted || platformconfig.BrowserLiteAdmitted {
		t.Fatal("the admission no longer matches the recorded ADR-0049 appendix (auto admitted, lite not)")
	}
	for _, engine := range []string{"full", "auto"} {
		normalized, err := normalizeBrowserSetting(map[string]any{"engine": engine})
		if err != nil || normalized["engine"] != engine {
			t.Errorf("%s: normalized=%v err=%v", engine, normalized, err)
		}
	}
	_, err := normalizeBrowserSetting(map[string]any{"engine": "lite"})
	var refusal *CurrentConfigurationMutationError
	if !errors.As(err, &refusal) || refusal.Code != CurrentConfigurationMutationEngineNotAdmitted ||
		refusal.Field != "data.engine" {
		t.Fatalf("lite: err = %v, want engine_not_admitted on data.engine", err)
	}
	if !strings.Contains(BrowserEngineNotAdmittedMessage, "ADR-0049") ||
		!strings.Contains(BrowserEngineNotAdmittedMessage, "Appendix A") {
		t.Fatalf("the refusal does not name the appendix: %q", BrowserEngineNotAdmittedMessage)
	}
	// A value that is no engine at all stays a plain invalid field.
	_, err = normalizeBrowserSetting(map[string]any{"engine": "turbo"})
	if !errors.As(err, &refusal) || refusal.Code != CurrentConfigurationMutationInvalid {
		t.Fatalf("turbo: err = %v, want invalid", err)
	}
	admitted := platformconfig.BrowserEngines(platformconfig.BrowserAdmission{Auto: true, Lite: true})
	for _, engine := range []string{"full", "lite", "auto"} {
		normalized, err := normalizeBrowserSettingFor(map[string]any{"engine": engine}, admitted)
		if err != nil || normalized["engine"] != engine {
			t.Errorf("%s: normalized=%v err=%v", engine, normalized, err)
		}
	}
	if _, err := normalizeBrowserSettingFor(map[string]any{"engine": "turbo"}, admitted); err == nil {
		t.Error("an unknown engine was accepted")
	}
}

// The served schema offers exactly the admitted engines.
func TestBrowserSettingSchemaOffersTheAdmittedEngines(t *testing.T) {
	t.Parallel()
	if got, want := platformconfig.BrowserEngines(platformconfig.AdmittedBrowserEngines), []string{"full", "auto"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("engines = %v, want %v", got, want)
	}
	if got := browserEngineDescription(platformconfig.AdmittedBrowserEngines); strings.Contains(got, "lite") ||
		!strings.Contains(got, "auto") {
		t.Fatalf("description = %q", got)
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
