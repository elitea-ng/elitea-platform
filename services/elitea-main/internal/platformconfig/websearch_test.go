package platformconfig

import (
	"reflect"
	"testing"
)

func TestWebSearchSettingsDefaults(t *testing.T) {
	t.Parallel()
	got := WebSearchSettingsFromValues(Values{})
	if len(got.EnabledProviders) != 0 || !got.DirectFetchEnabled || got.AllowHTTP ||
		got.DefaultSearchProvider != "" || got.MaxResultsCap != 10 || got.HostMaxResultsCap != 20 {
		t.Fatalf("defaults = %+v", got)
	}
}

func TestWebSearchSettingsOverlay(t *testing.T) {
	t.Parallel()
	got := WebSearchSettingsFromValues(Values{
		KeyWebSearchEnabledProviders:      []any{"tavily", "nope", "tavily", "shodan", 7},
		KeyWebSearchDefaultSearchProvider: "tavily",
		KeyWebSearchFetchAllowDomains:     []any{"Docs.RS", "bad host", "*.github.com"},
		KeyWebSearchMaxResultsCap:         float64(500),
		KeyWebSearchHostMaxResultsCap:     float64(5),
		KeyWebSearchAllowHTTP:             true,
	})
	if !reflect.DeepEqual(got.EnabledProviders, []string{"tavily", "shodan"}) {
		t.Fatalf("enabled = %v", got.EnabledProviders)
	}
	if got.DefaultSearchProvider != "tavily" || !got.AllowHTTP || got.MaxResultsCap != 10 || got.HostMaxResultsCap != 5 {
		t.Fatalf("overlay = %+v", got)
	}
	if !reflect.DeepEqual(got.FetchAllowDomains, []string{"docs.rs", "*.github.com"}) {
		t.Fatalf("allow = %v", got.FetchAllowDomains)
	}
	if !got.ProviderEnabled("shodan") || got.ProviderEnabled("brave") {
		t.Fatal("ProviderEnabled disagrees with the list")
	}
}

// ddg_instant answers instant answers only, so it may never be chosen
// implicitly: a saved default naming it is ignored.
func TestWebSearchDefaultRefusesDDGInstant(t *testing.T) {
	t.Parallel()
	got := WebSearchSettingsFromValues(Values{KeyWebSearchDefaultSearchProvider: "ddg_instant"})
	if got.DefaultSearchProvider != "" {
		t.Fatalf("default = %q", got.DefaultSearchProvider)
	}
}
