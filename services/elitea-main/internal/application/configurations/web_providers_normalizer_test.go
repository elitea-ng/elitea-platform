package configurations

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestWebProviderCreateKeepsOnlyTheKey(t *testing.T) {
	t.Parallel()
	normalizer := NewCurrentWebProviderDataNormalizer(nil)
	result, err := normalizer.Normalize(context.Background(), CurrentConfigurationNormalizationRequest{
		Operation: CurrentConfigurationNormalizationCreate, Type: "tavily",
		Data: map[string]any{"api_key": " tvly-1 ", "base_url": "http://169.254.169.254"},
	})
	if err != nil || !result.Complete || !reflect.DeepEqual(result.Data, map[string]any{"api_key": "tvly-1"}) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	result, err = normalizer.Normalize(context.Background(), CurrentConfigurationNormalizationRequest{
		Operation: CurrentConfigurationNormalizationCreate, Type: "duckduckgo_instant",
		Data: map[string]any{"anything": true},
	})
	if err != nil || !result.Complete || len(result.Data) != 0 {
		t.Fatalf("ddg result=%#v err=%v", result, err)
	}
	for _, bad := range []any{nil, "", "two words", 7} {
		_, err = normalizer.Normalize(context.Background(), CurrentConfigurationNormalizationRequest{
			Operation: CurrentConfigurationNormalizationCreate, Type: "shodan", Data: map[string]any{"api_key": bad},
		})
		var mutation *CurrentConfigurationMutationError
		if !errors.As(err, &mutation) || mutation.Field != "data.api_key" {
			t.Fatalf("api_key %#v: err=%v", bad, err)
		}
	}
}

func TestSharedConfigurationAllowedRefusesShodanOnly(t *testing.T) {
	t.Parallel()
	if SharedConfigurationAllowed(ShodanConfigurationType) {
		t.Fatal("a shared shodan configuration must be refused")
	}
	for _, typeName := range []string{"tavily", "brave_search", "github"} {
		if !SharedConfigurationAllowed(typeName) {
			t.Fatalf("%s must stay shareable", typeName)
		}
	}
}
