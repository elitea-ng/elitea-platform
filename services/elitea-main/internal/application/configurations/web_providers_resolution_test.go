package configurations

import (
	"context"
	"reflect"
	"testing"
)

const webTestPublicProject int32 = 1

type webProviderSourceStub struct {
	policy  WebProviderPolicy
	rows    map[int32]map[string]CurrentExpansionConfiguration // project -> type -> row
	shared  map[int32]map[string]bool
	allowed map[string]bool
	lookups []string
}

func (s *webProviderSourceStub) WebProviderPolicy(context.Context) (WebProviderPolicy, error) {
	return s.policy, nil
}

func (s *webProviderSourceStub) FindConfigurationByType(
	_ context.Context, projectID int32, configurationType string, sharedOnly bool,
) (CurrentExpansionConfiguration, bool, error) {
	s.lookups = append(s.lookups, configurationType)
	row, found := s.rows[projectID][configurationType]
	if found && sharedOnly && !s.shared[projectID][configurationType] {
		return CurrentExpansionConfiguration{}, false, nil
	}
	return row, found, nil
}

func (s *webProviderSourceStub) PublicProjectID(context.Context) (int32, error) {
	return webTestPublicProject, nil
}

func (s *webProviderSourceStub) ToolkitTypeAllowed(_ context.Context, _ int32, toolkitType string) (bool, error) {
	return s.allowed[toolkitType], nil
}

type anyProjectUnsecreter struct{ projects []int32 }

func (u *anyProjectUnsecreter) Unsecret(_ context.Context, projectID int32, data map[string]any) (map[string]any, error) {
	u.projects = append(u.projects, projectID)
	out := cloneCurrentJSONObject(data)
	if _, sealed := out["api_key"]; sealed {
		out["api_key"] = "plain-" + out["api_key"].(string)
	}
	return out, nil
}

func webRow(project int32, configurationType, key string) CurrentExpansionConfiguration {
	return CurrentExpansionConfiguration{
		UUID: configurationType + "-uuid", ProjectID: project, Type: configurationType,
		Data: map[string]any{"api_key": key},
	}
}

func webResolver(t *testing.T, source CurrentWebProviderSource, unsecreter CurrentExpansionUnsecreter) *CurrentToolkitSettingsResolver {
	t.Helper()
	deps := currentToolkitSettingsTestDependencies()
	deps.schemas.schemas = map[string]CurrentToolkitSchema{
		WebToolkitType: {Properties: map[string]any{}}, HostIntelToolkitType: {Properties: map[string]any{}},
	}
	return newCurrentToolkitSettingsResolverForTest(t, deps.schemas, deps.toolkits, deps.configurations,
		deps.models, unsecreter).WithWebProviders(source)
}

func resolveWeb(t *testing.T, resolver *CurrentToolkitSettingsResolver, toolkitType string, mode CurrentToolkitSettingsMode, settings map[string]any) map[string]any {
	t.Helper()
	got, err := resolver.Resolve(context.Background(), CurrentToolkitSettingsRequest{
		ToolkitType: toolkitType, Settings: settings, ProjectID: 7, UserID: 42, Mode: mode,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return got
}

func TestWebResolvesEnabledProvidersOnly(t *testing.T) {
	t.Parallel()
	source := &webProviderSourceStub{
		policy: WebProviderPolicy{EnabledProviders: []string{"tavily", "ddg_instant"}, DirectFetchEnabled: true,
			DefaultSearchProvider: "tavily", MaxResultsCap: 10, FetchDenyDomains: []string{"evil.example"}},
		rows: map[int32]map[string]CurrentExpansionConfiguration{7: {
			"tavily": webRow(7, "tavily", "{{secret.t}}"), "brave_search": webRow(7, "brave_search", "{{secret.b}}"),
		}},
	}
	got := resolveWeb(t, webResolver(t, source, &anyProjectUnsecreter{}), WebToolkitType,
		CurrentToolkitSettingsReferenceMode,
		// A stored toolkit cannot name its own providers or lift the lists.
		map[string]any{"providers": map[string]any{"brave": map[string]any{"api_key": "forged"}},
			"fetch_deny_domains": []any{}, "default_search_provider": "tavily"})
	providers := got["providers"].(map[string]any)
	if len(providers) != 2 || providers["ddg_instant"] == nil || providers["brave"] != nil {
		t.Fatalf("providers = %#v", providers)
	}
	tavily := providers["tavily"].(map[string]any)
	if tavily["api_key"] != "{{secret.t}}" || tavily[CurrentFrozenConfigurationMarker] != true ||
		tavily["configuration_project_id"] != int32(7) || tavily["configuration_type"] != "tavily" {
		t.Fatalf("tavily = %#v", tavily)
	}
	want := map[string]any{
		"platform_default_search_provider": "tavily", "direct_fetch_enabled": true, "allow_http": false,
		"fetch_allow_domains": []any{}, "fetch_deny_domains": []any{"evil.example"}, "max_results_cap": int64(10),
		"default_fetch_backend": "direct", "strict_options": true, "default_search_provider": "tavily",
	}
	for key, value := range want {
		if !reflect.DeepEqual(got[key], value) {
			t.Errorf("%s = %#v, want %#v", key, got[key], value)
		}
	}
}

func TestWebProjectConfigBeatsShared(t *testing.T) {
	t.Parallel()
	source := &webProviderSourceStub{
		policy: WebProviderPolicy{EnabledProviders: []string{"tavily", "brave"}},
		rows: map[int32]map[string]CurrentExpansionConfiguration{
			7:                    {"tavily": webRow(7, "tavily", "own")},
			webTestPublicProject: {"tavily": webRow(1, "tavily", "shared"), "brave_search": webRow(1, "brave_search", "shared-brave")},
		},
		shared: map[int32]map[string]bool{webTestPublicProject: {"tavily": true, "brave_search": true}},
	}
	unsecreter := &anyProjectUnsecreter{}
	got := resolveWeb(t, webResolver(t, source, unsecreter), WebToolkitType, CurrentToolkitSettingsClaimMode, map[string]any{})
	providers := got["providers"].(map[string]any)
	if key := providers["tavily"].(map[string]any)["api_key"]; key != "plain-own" {
		t.Fatalf("tavily key = %v, want the project's own", key)
	}
	brave := providers["brave"].(map[string]any)
	if brave["api_key"] != "plain-shared-brave" || brave["configuration_project_id"] != webTestPublicProject {
		t.Fatalf("brave = %#v, want the public project's shared row", brave)
	}
	if _, marked := brave[CurrentFrozenConfigurationMarker]; marked {
		t.Fatal("claim mode must not stamp the frozen marker")
	}
	if !reflect.DeepEqual(unsecreter.projects, []int32{7, webTestPublicProject}) {
		t.Fatalf("vaults opened = %v; each row must be redeemed through its own project", unsecreter.projects)
	}
}

func TestShodanIgnoresSharedConfig(t *testing.T) {
	t.Parallel()
	source := &webProviderSourceStub{
		policy:  WebProviderPolicy{EnabledProviders: []string{"shodan"}, HostMaxResultsCap: 20},
		rows:    map[int32]map[string]CurrentExpansionConfiguration{webTestPublicProject: {"shodan": webRow(1, "shodan", "shared")}},
		shared:  map[int32]map[string]bool{webTestPublicProject: {"shodan": true}},
		allowed: map[string]bool{HostIntelToolkitType: true},
	}
	got := resolveWeb(t, webResolver(t, source, &anyProjectUnsecreter{}), HostIntelToolkitType, CurrentToolkitSettingsReferenceMode, map[string]any{})
	if providers := got["providers"].(map[string]any); len(providers) != 0 {
		t.Fatalf("providers = %#v; a shared shodan row must be ignored", providers)
	}
	if got["max_results_cap"] != int64(20) {
		t.Fatalf("max_results_cap = %#v", got["max_results_cap"])
	}
	source.rows[7] = map[string]CurrentExpansionConfiguration{"shodan": webRow(7, "shodan", "own")}
	got = resolveWeb(t, webResolver(t, source, &anyProjectUnsecreter{}), HostIntelToolkitType, CurrentToolkitSettingsReferenceMode, map[string]any{})
	if providers := got["providers"].(map[string]any); providers["shodan"] == nil {
		t.Fatalf("providers = %#v; the project's own shodan key must resolve", providers)
	}
}

func TestHostIntelResolvesNoProviderWithoutGrant(t *testing.T) {
	t.Parallel()
	source := &webProviderSourceStub{
		policy: WebProviderPolicy{EnabledProviders: []string{"shodan"}},
		rows:   map[int32]map[string]CurrentExpansionConfiguration{7: {"shodan": webRow(7, "shodan", "own")}},
	}
	got := resolveWeb(t, webResolver(t, source, &anyProjectUnsecreter{}), HostIntelToolkitType, CurrentToolkitSettingsReferenceMode, map[string]any{})
	if providers := got["providers"].(map[string]any); len(providers) != 0 || len(source.lookups) != 0 {
		t.Fatalf("providers=%#v lookups=%v; host_intel without a grant reads no credential", providers, source.lookups)
	}
}

func TestWebWithoutSourceFreezesNoProviderAndNoDirectFetch(t *testing.T) {
	t.Parallel()
	got := resolveWeb(t, webResolver(t, nil, &anyProjectUnsecreter{}), WebToolkitType, CurrentToolkitSettingsReferenceMode, map[string]any{})
	if len(got["providers"].(map[string]any)) != 0 || got["direct_fetch_enabled"] != false {
		t.Fatalf("settings = %#v", got)
	}
}
