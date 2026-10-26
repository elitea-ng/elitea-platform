package configurations

import (
	"context"
	"errors"
)

// WebProviderPolicy is the platform policy of the web toolkits, as Main's
// admin `web_search` section states it (internal/platformconfig). The
// resolver copies it into the frozen settings, so the worker receives the
// policy beside the providers and never reads platform state itself.
type WebProviderPolicy struct {
	EnabledProviders      []string
	DirectFetchEnabled    bool
	DefaultSearchProvider string
	FetchAllowDomains     []string
	FetchDenyDomains      []string
	AllowHTTP             bool
	MaxResultsCap         int64
	HostMaxResultsCap     int64
}

func (p WebProviderPolicy) enabled(id string) bool {
	for _, enabled := range p.EnabledProviders {
		if enabled == id {
			return true
		}
	}
	return false
}

// CurrentWebProviderSource is what the resolution reads: the platform policy,
// one configuration of a type in one project, the public project, and the
// operator's toolkit-type decision for the project (host_intel is off until
// granted).
type CurrentWebProviderSource interface {
	WebProviderPolicy(context.Context) (WebProviderPolicy, error)
	// FindConfigurationByType returns the first configuration of the type in
	// the project; sharedOnly restricts it to shared rows (the public-project
	// fallback).
	FindConfigurationByType(ctx context.Context, projectID int32, configurationType string, sharedOnly bool) (CurrentExpansionConfiguration, bool, error)
	PublicProjectID(context.Context) (int32, error)
	ToolkitTypeAllowed(ctx context.Context, projectID int32, toolkitType string) (bool, error)
}

// The settings keys Main writes into a web toolkit at freeze time. A stored
// toolkit can never carry them: they are dropped from its saved settings
// before the resolution, so a project cannot name its own providers, lift
// the domain lists or forge a frozen credential.
var webServerAuthoredSettings = []string{
	"providers", "platform_default_search_provider", "direct_fetch_enabled",
	"fetch_allow_domains", "fetch_deny_domains", "allow_http", "max_results_cap",
}

// withoutWebServerAuthoredSettings is a shallow copy of a web toolkit's
// settings without the keys above; other types' settings are returned as is.
func withoutWebServerAuthoredSettings(toolkitType string, settings map[string]any) map[string]any {
	if WebProvidersFor(toolkitType) == nil || settings == nil {
		return settings
	}
	copied := make(map[string]any, len(settings))
	for key, value := range settings {
		copied[key] = value
	}
	for _, key := range webServerAuthoredSettings {
		delete(copied, key)
	}
	return copied
}

// WithWebProviders lends the resolver the source the `web` and `host_intel`
// toolkits are resolved from. Without one those toolkits freeze with no
// provider and direct fetch off, so every call is refused rather than made
// with a policy nobody read.
func (r *CurrentToolkitSettingsResolver) WithWebProviders(source CurrentWebProviderSource) *CurrentToolkitSettingsResolver {
	if r != nil {
		r.webProviders = source
	}
	return r
}

// resolveWebProviders writes the providers map and the platform policy into a
// web or host_intel toolkit's settings (#1238, contract §3):
//
//   - a provider appears only when the platform enables it AND a configuration
//     resolves: the project's own, else a shared one in the public project
//     where the provider allows sharing (never for Shodan);
//   - a keyless provider (DuckDuckGo instant answers) needs no row;
//   - host_intel gets no provider unless the operator granted the type to the
//     project (it is off by default).
//
// A configuration is frozen exactly as a credential field is: sealed with
// the frozen marker in reference mode (the claim-time materializer redeems it
// through its own project's vault), redeemed in claim mode.
func (w *currentToolkitSettingsWalker) resolveWebProviders(
	ctx context.Context,
	toolkitType string,
	settings map[string]any,
	depth int,
) error {
	providers := WebProvidersFor(toolkitType)
	if providers == nil {
		return nil
	}
	source := w.resolver.webProviders
	policy := WebProviderPolicy{}
	resolved := map[string]any{}
	if source != nil {
		var err error
		policy, err = source.WebProviderPolicy(ctx)
		if err != nil {
			return currentToolkitSettingsDependencyError(ctx, err)
		}
		allowed := true
		if toolkitType == HostIntelToolkitType {
			allowed, err = source.ToolkitTypeAllowed(ctx, w.request.ProjectID, toolkitType)
			if err != nil {
				return currentToolkitSettingsDependencyError(ctx, err)
			}
		}
		if allowed {
			for _, provider := range providers {
				if !policy.enabled(provider.ID) {
					continue
				}
				if provider.Keyless {
					resolved[provider.ID] = map[string]any{}
					continue
				}
				frozen, found, err := w.resolveWebProviderConfiguration(ctx, source, provider, depth)
				if err != nil {
					return err
				}
				if found {
					resolved[provider.ID] = frozen
				}
			}
		}
	}
	if err := w.budget.addContainer(len(resolved)); err != nil {
		return err
	}
	settings["providers"] = resolved
	if toolkitType == HostIntelToolkitType {
		settings["max_results_cap"] = policy.HostMaxResultsCap
		return nil
	}
	var platformDefault any
	if policy.DefaultSearchProvider != "" && policy.enabled(policy.DefaultSearchProvider) {
		platformDefault = policy.DefaultSearchProvider
	}
	settings["platform_default_search_provider"] = platformDefault
	settings["direct_fetch_enabled"] = source != nil && policy.DirectFetchEnabled
	settings["fetch_allow_domains"] = stringsToAny(policy.FetchAllowDomains)
	settings["fetch_deny_domains"] = stringsToAny(policy.FetchDenyDomains)
	settings["allow_http"] = policy.AllowHTTP
	settings["max_results_cap"] = policy.MaxResultsCap
	if _, set := settings["default_fetch_backend"]; !set {
		settings["default_fetch_backend"] = "direct"
	}
	if _, set := settings["strict_options"]; !set {
		settings["strict_options"] = true
	}
	return nil
}

func (w *currentToolkitSettingsWalker) resolveWebProviderConfiguration(
	ctx context.Context,
	source CurrentWebProviderSource,
	provider WebProvider,
	depth int,
) (map[string]any, bool, error) {
	configuration, found, err := source.FindConfigurationByType(ctx, w.request.ProjectID, provider.ConfigurationType, false)
	if err != nil {
		return nil, false, currentToolkitSettingsDependencyError(ctx, err)
	}
	if !found && provider.SharedAllowed {
		publicProjectID, err := source.PublicProjectID(ctx)
		if err != nil {
			return nil, false, currentToolkitSettingsDependencyError(ctx, err)
		}
		if publicProjectID > 0 && publicProjectID != w.request.ProjectID {
			configuration, found, err = source.FindConfigurationByType(ctx, publicProjectID, provider.ConfigurationType, true)
			if err != nil {
				return nil, false, currentToolkitSettingsDependencyError(ctx, err)
			}
		}
	}
	if !found || IsRetiredConfigurationType(configuration.Type) {
		return nil, false, nil
	}
	if configuration.Type != provider.ConfigurationType || configuration.ProjectID <= 0 || configuration.UUID == "" {
		return nil, false, ErrCurrentToolkitSettingsDependency
	}
	data, err := w.cloneObject(ctx, configuration.Data, depth+2)
	if err != nil {
		return nil, false, err
	}
	if data == nil {
		data = map[string]any{}
	}
	if w.request.Mode == CurrentToolkitSettingsClaimMode {
		unsecreted, err := w.resolver.unsecreter.Unsecret(ctx, configuration.ProjectID, data)
		if err != nil {
			return nil, false, currentToolkitSettingsDependencyError(ctx, err)
		}
		if unsecreted == nil {
			return nil, false, errors.New("current web provider configuration did not unsecret")
		}
		data = unsecreted
	}
	data["configuration_uuid"] = configuration.UUID
	data["configuration_project_id"] = configuration.ProjectID
	data["configuration_type"] = configuration.Type
	if w.request.Mode == CurrentToolkitSettingsReferenceMode {
		data[CurrentFrozenConfigurationMarker] = true
	}
	return data, true, nil
}

func stringsToAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}
