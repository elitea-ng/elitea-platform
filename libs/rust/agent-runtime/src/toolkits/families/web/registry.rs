//! The ONE list of provider adapters. Adding a provider for an existing
//! capability is one adapter file and one entry here (README.md); no other
//! file branches on a provider id.

use std::sync::Arc;

use super::adapters::{brave, ddg_instant, firecrawl, shodan, tavily};
use super::capability::{FetchProvider, HostIntelProvider, SearchProvider};
use super::config::ProviderConfig;
use super::transport::Transport;

/// What a provider can do. Adding a kind is one `capability/<kind>.rs`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Capability {
    WebSearch,
    Fetch,
    HostIntel,
}

/// Whether a provider may be chosen by default.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Lifecycle {
    Active,
    /// Served only when named explicitly or set as the toolkit default.
    Degraded,
}

/// A provider's capability implementations; each slot is `Some` only for a
/// kind the entry declares.
#[derive(Default)]
pub(crate) struct ProviderHandle {
    pub(crate) web_search: Option<Box<dyn SearchProvider>>,
    pub(crate) fetch: Option<Box<dyn FetchProvider>>,
    pub(crate) host_intel: Option<Box<dyn HostIntelProvider>>,
}

impl ProviderHandle {
    /// The kinds this handle serves.
    pub(crate) fn kinds(&self) -> Vec<Capability> {
        let mut kinds = Vec::new();
        if self.web_search.is_some() {
            kinds.push(Capability::WebSearch);
        }
        if self.fetch.is_some() {
            kinds.push(Capability::Fetch);
        }
        if self.host_intel.is_some() {
            kinds.push(Capability::HostIntel);
        }
        kinds
    }
}

pub(crate) struct RegistryEntry {
    /// The provider id (`tavily`), also the metered `provider`.
    pub(crate) id: &'static str,
    /// Main's configuration type.
    #[cfg_attr(
        not(test),
        allow(dead_code, reason = "read by the registry contract tests")
    )]
    pub(crate) config_type: &'static str,
    /// Whether a shared public-project configuration may serve every
    /// project. Main enforces it; recorded here so the docs table and the
    /// registry agree (Shodan's terms bar sharing one account).
    #[cfg_attr(
        not(test),
        allow(dead_code, reason = "read by the registry contract tests")
    )]
    pub(crate) shared_config_allowed: bool,
    /// The only https origins the adapter reaches.
    pub(crate) origins: &'static [&'static str],
    pub(crate) lifecycle: Lifecycle,
    pub(crate) capabilities: &'static [Capability],
    pub(crate) needs_key: bool,
    pub(crate) construct: fn(&ProviderConfig, Arc<dyn Transport>) -> ProviderHandle,
}

pub(crate) const REGISTRY: &[RegistryEntry] = &[
    RegistryEntry {
        id: "tavily",
        config_type: "tavily",
        shared_config_allowed: true,
        origins: tavily::ORIGINS,
        lifecycle: Lifecycle::Active,
        capabilities: &[Capability::WebSearch, Capability::Fetch],
        needs_key: true,
        construct: tavily::construct,
    },
    RegistryEntry {
        id: "firecrawl",
        config_type: "firecrawl",
        shared_config_allowed: true,
        origins: firecrawl::ORIGINS,
        lifecycle: Lifecycle::Active,
        capabilities: &[Capability::WebSearch, Capability::Fetch],
        needs_key: true,
        construct: firecrawl::construct,
    },
    RegistryEntry {
        id: "brave",
        config_type: "brave_search",
        shared_config_allowed: true,
        origins: brave::ORIGINS,
        lifecycle: Lifecycle::Active,
        capabilities: &[Capability::WebSearch],
        needs_key: true,
        construct: brave::construct,
    },
    RegistryEntry {
        id: "ddg_instant",
        config_type: "duckduckgo_instant",
        shared_config_allowed: true,
        origins: ddg_instant::ORIGINS,
        lifecycle: Lifecycle::Degraded,
        capabilities: &[Capability::WebSearch],
        needs_key: false,
        construct: ddg_instant::construct,
    },
    RegistryEntry {
        id: "shodan",
        config_type: "shodan",
        shared_config_allowed: false,
        origins: shodan::ORIGINS,
        lifecycle: Lifecycle::Active,
        capabilities: &[Capability::HostIntel],
        needs_key: true,
        construct: shodan::construct,
    },
];

/// The entry of a provider id.
pub(crate) fn entry(id: &str) -> Option<&'static RegistryEntry> {
    REGISTRY.iter().find(|entry| entry.id == id)
}
