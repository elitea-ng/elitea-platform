package platformconfig

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SectionWebSearch is the platform policy of the `web` and `host_intel`
// toolkit types (#1238): which providers may be used at all, the platform
// default search provider, and the rules of the direct fetch backend. The
// admin Configuration page writes it; Main reads it when it freezes a web
// toolkit's settings for a run, so the worker receives the policy with the
// providers and never reads platform state itself.
//
// Spend is NOT limited here: web and host calls are admitted and billed by the
// LLM budget engine, so the project and member budgets are their limits.
const SectionWebSearch = "web_search"

// Field keys of SectionWebSearch.
const (
	KeyWebSearchEnabledProviders      = "enabled_providers"
	KeyWebSearchDirectFetchEnabled    = "direct_fetch_enabled"
	KeyWebSearchDefaultSearchProvider = "default_search_provider"
	KeyWebSearchFetchAllowDomains     = "fetch_allow_domains"
	KeyWebSearchFetchDenyDomains      = "fetch_deny_domains"
	KeyWebSearchAllowHTTP             = "allow_http"
	KeyWebSearchMaxResultsCap         = "max_results_cap"
	KeyWebSearchHostMaxResultsCap     = "host_max_results_cap"
)

// The provider ids the section accepts, in registry order. ddg_instant is a
// degraded provider: it may be enabled and named per call or per toolkit, but
// never be the platform default (it answers instant answers only).
var webSearchProviderIDs = []string{"tavily", "firecrawl", "brave", "ddg_instant", "shodan"}

// WebSearchDefaultableProviders are the ids default_search_provider accepts.
func WebSearchDefaultableProviders() []string { return []string{"tavily", "firecrawl", "brave"} }

// WebSearchProviderIDs lists every provider id the section accepts.
func WebSearchProviderIDs() []string { return append([]string(nil), webSearchProviderIDs...) }

// Bounds the loader applies.
const (
	MaxWebSearchDomains       = 200
	DefaultWebMaxResultsCap   = 10
	MaxWebMaxResultsCap       = 20
	DefaultHostMaxResultsCap  = 20
	MaxHostMaxResultsCap      = 100
	maxWebSearchDomainEntries = 253
)

// WebSearchSettings is the section over its defaults.
type WebSearchSettings struct {
	EnabledProviders      []string
	DirectFetchEnabled    bool
	DefaultSearchProvider string
	FetchAllowDomains     []string
	FetchDenyDomains      []string
	AllowHTTP             bool
	MaxResultsCap         int64
	HostMaxResultsCap     int64
}

// DefaultWebSearchSettings is a deployment that never saved the section: no
// provider enabled (a key alone does not turn a vendor on), direct fetch on,
// https only.
func DefaultWebSearchSettings() WebSearchSettings {
	return WebSearchSettings{
		EnabledProviders:   []string{},
		DirectFetchEnabled: true,
		FetchAllowDomains:  []string{},
		FetchDenyDomains:   []string{},
		MaxResultsCap:      DefaultWebMaxResultsCap,
		HostMaxResultsCap:  DefaultHostMaxResultsCap,
	}
}

// LoadWebSearchSettings reads the section. A query error answers the defaults
// and the error; the caller decides whether that fails closed.
func LoadWebSearchSettings(ctx context.Context, pool *pgxpool.Pool) (WebSearchSettings, error) {
	values, err := Load(ctx, pool, SectionWebSearch)
	if err != nil {
		return DefaultWebSearchSettings(), err
	}
	return WebSearchSettingsFromValues(values), nil
}

// WebSearchSettingsFromValues is the overlay. A value of the wrong type, an
// unknown provider id or an out-of-range cap is ABSENT and keeps its default.
func WebSearchSettingsFromValues(values Values) WebSearchSettings {
	settings := DefaultWebSearchSettings()
	known := map[string]bool{}
	for _, id := range webSearchProviderIDs {
		known[id] = true
	}
	seen := map[string]bool{}
	for _, id := range values.Strings(KeyWebSearchEnabledProviders) {
		id = strings.TrimSpace(id)
		if known[id] && !seen[id] {
			seen[id] = true
			settings.EnabledProviders = append(settings.EnabledProviders, id)
		}
	}
	settings.DirectFetchEnabled = values.Bool(KeyWebSearchDirectFetchEnabled, settings.DirectFetchEnabled)
	if candidate := values.String(KeyWebSearchDefaultSearchProvider, ""); candidate != "" {
		for _, id := range WebSearchDefaultableProviders() {
			if id == candidate {
				settings.DefaultSearchProvider = candidate
			}
		}
	}
	settings.FetchAllowDomains = webSearchDomains(values.Strings(KeyWebSearchFetchAllowDomains))
	settings.FetchDenyDomains = webSearchDomains(values.Strings(KeyWebSearchFetchDenyDomains))
	settings.AllowHTTP = values.Bool(KeyWebSearchAllowHTTP, settings.AllowHTTP)
	if n, ok := values.Int(KeyWebSearchMaxResultsCap); ok && n >= 1 && n <= MaxWebMaxResultsCap {
		settings.MaxResultsCap = n
	}
	if n, ok := values.Int(KeyWebSearchHostMaxResultsCap); ok && n >= 1 && n <= MaxHostMaxResultsCap {
		settings.HostMaxResultsCap = n
	}
	return settings
}

// ProviderEnabled reports whether the section enables a provider id.
func (s WebSearchSettings) ProviderEnabled(id string) bool {
	for _, enabled := range s.EnabledProviders {
		if enabled == id {
			return true
		}
	}
	return false
}

// webSearchDomains keeps the valid host names of a domain list, lowercased,
// deduplicated, at most MaxWebSearchDomains. It never answers nil.
func webSearchDomains(domains []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if len(domain) > maxWebSearchDomainEntries || !ValidCloudExecutorHost(domain) || seen[domain] {
			continue
		}
		if len(out) == MaxWebSearchDomains {
			break
		}
		seen[domain] = true
		out = append(out, domain)
	}
	return out
}
