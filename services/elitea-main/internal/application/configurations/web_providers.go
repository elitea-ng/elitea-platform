package configurations

// The configuration types behind the `web` and `host_intel` toolkit types
// (#1238). Each provider is resolved per project at admission, not chosen by
// a field of the toolkit: the toolkit names the provider, the project's own
// configuration of that type supplies the key, and a shared configuration in
// the public project is the fallback where the provider's terms allow one.
const (
	TavilyConfigurationType            = "tavily"
	FirecrawlConfigurationType         = "firecrawl"
	BraveSearchConfigurationType       = "brave_search"
	DuckDuckGoInstantConfigurationType = "duckduckgo_instant"
	ShodanConfigurationType            = "shodan"
)

// The two toolkit types the providers serve.
const (
	WebToolkitType       = "web"
	HostIntelToolkitType = "host_intel"
)

// WebProvider is one provider a web or host_intel toolkit can call: the id the
// worker's registry knows it by, and the configuration type that holds its key.
type WebProvider struct {
	ID                string
	ConfigurationType string
	// Keyless providers need no configuration row: enabling them is enough.
	Keyless bool
	// SharedAllowed is false where the provider's terms bar one account from
	// serving every project (Shodan, terms 6.5 and 9.2): only a project's own
	// configuration is used.
	SharedAllowed bool
}

// webSearchProviders are the providers of the `web` toolkit, in the order the
// worker's registry lists them.
var webSearchProviders = []WebProvider{
	{ID: "tavily", ConfigurationType: TavilyConfigurationType, SharedAllowed: true},
	{ID: "firecrawl", ConfigurationType: FirecrawlConfigurationType, SharedAllowed: true},
	{ID: "brave", ConfigurationType: BraveSearchConfigurationType, SharedAllowed: true},
	{ID: "ddg_instant", ConfigurationType: DuckDuckGoInstantConfigurationType, Keyless: true, SharedAllowed: true},
}

var hostIntelProviders = []WebProvider{
	{ID: "shodan", ConfigurationType: ShodanConfigurationType, SharedAllowed: false},
}

// WebProvidersFor lists the providers a toolkit type serves; nil for any other
// type. The slice is a copy.
func WebProvidersFor(toolkitType string) []WebProvider {
	switch toolkitType {
	case WebToolkitType:
		return append([]WebProvider(nil), webSearchProviders...)
	case HostIntelToolkitType:
		return append([]WebProvider(nil), hostIntelProviders...)
	default:
		return nil
	}
}

// IsWebProviderID reports a provider id of either toolkit type.
func IsWebProviderID(id string) bool {
	for _, provider := range append(WebProvidersFor(WebToolkitType), WebProvidersFor(HostIntelToolkitType)...) {
		if provider.ID == id {
			return true
		}
	}
	return false
}

// SharedConfigurationAllowed reports whether a configuration of this type may
// be saved with shared=true. Shodan's terms bar sharing one account's results
// across projects, so a shared Shodan configuration is refused on save and
// ignored at claim time.
func SharedConfigurationAllowed(configurationType string) bool {
	return configurationType != ShodanConfigurationType
}
