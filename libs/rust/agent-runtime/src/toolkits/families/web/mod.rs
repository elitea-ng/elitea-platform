//! The `web` and `host_intel` toolkit families (#1238): `web_search` and
//! `web_fetch` through a capability-keyed provider registry (Tavily,
//! Firecrawl, Brave, `DuckDuckGo` instant answers) plus a key-less `direct`
//! fetch through the egress guard; `internet_host_search` and
//! `internet_host_lookup` over Shodan.
//!
//! The families run on the cloud worker only. The desktop reaches them
//! through `remote_toolkit_call`, so no provider key and no fetch is ever on
//! the laptop. Every provider call is admitted and settled by the LLM
//! gateway's budget engine ([`meter`]); a toolset built without a
//! [`WebToolHost`] (no meter) refuses every call. Every result is untrusted
//! data and says so. How to add a provider: `README.md` beside this file.

pub(crate) mod adapters;
pub(crate) mod capability;
// Only the Office catalogue test (`toolkit-office`) reads it.
#[cfg(all(test, feature = "toolkit-office"))]
pub(crate) mod catalogue;
pub(crate) mod config;
pub(crate) mod direct;
pub mod meter;
pub(crate) mod registry;
pub(crate) mod transport;

use std::collections::BTreeMap;
use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use elitea_egress_guard::{Allowlist, Lookup, SystemLookup};
use serde_json::{Map, Value};

use crate::toolkits::invocation::admit_materialized_toolset;
use crate::toolkits::policy::ToolAdmissionPolicy;

use capability::{CallEnd, error_result, fetch, host_intel, web_search};
use config::{HostIntelConfig, WebConfig, WebConfigError};
use direct::DirectFetcher;
use meter::Meter;
use registry::{Capability, ProviderHandle, RegistryEntry};
use transport::{PinnedTransport, Transport};

/// The built-in fetch backend's id (also its metered `provider`).
pub(crate) const DIRECT: &str = "direct";
const PROVIDER_USER_AGENT: &str = "elitea-web-tools/1";

/// What the host lends the web families for one claim: the meter, the
/// deployment's egress allowlist for provider origins
/// (`ELITEA_WEB_EGRESS_ALLOWLIST`, fail-closed) and private ranges, and the
/// deployment origin the fetch `User-Agent` names.
#[derive(Clone)]
pub struct WebToolHost {
    meter: Arc<dyn Meter>,
    egress: Arc<Allowlist>,
    user_agent: String,
    lookup: Arc<dyn Lookup>,
}

impl WebToolHost {
    #[must_use]
    pub fn new(meter: Arc<dyn Meter>, egress: Allowlist, deployment_origin: &str) -> Self {
        let origin: String = deployment_origin
            .chars()
            .filter(char::is_ascii_graphic)
            .take(200)
            .collect();
        Self {
            meter,
            egress: Arc::new(egress),
            user_agent: format!("EliteaFetch/1 (+{origin})"),
            lookup: Arc::new(SystemLookup),
        }
    }

    /// This host with another name lookup (tests resolve fixture names).
    #[must_use]
    pub fn with_lookup(mut self, lookup: Arc<dyn Lookup>) -> Self {
        self.lookup = lookup;
        self
    }
}

/// The egress allowlist entries of every provider origin the registry
/// declares (`api.tavily.com:443`, ...): the default a host uses when its
/// deployment names none.
#[must_use]
pub fn provider_egress_entries() -> Vec<String> {
    let mut entries: Vec<String> = registry::REGISTRY
        .iter()
        .flat_map(|entry| entry.origins.iter())
        .filter_map(|origin| origin.strip_prefix("https://"))
        .map(|host| format!("{host}:443"))
        .collect();
    entries.sort();
    entries.dedup();
    entries
}

impl fmt::Debug for WebToolHost {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("WebToolHost")
            .finish_non_exhaustive()
    }
}

/// A web toolset could not be built from its settings.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum WebToolsetError {
    InvalidConfiguration,
    UnsupportedSelection,
    InvalidDefinition,
}

impl From<WebConfigError> for WebToolsetError {
    fn from(_: WebConfigError) -> Self {
        Self::InvalidConfiguration
    }
}

/// One call's identity: the tool call id seeds the metered event id.
pub(crate) struct CallScope<'a> {
    pub(crate) tool_call_id: &'a str,
}

/// Everything one toolset's tools share.
pub(crate) struct WebState {
    pub(crate) config: WebConfig,
    pub(crate) providers: BTreeMap<&'static str, ProviderHandle>,
    pub(crate) direct: Option<DirectFetcher>,
    meter: Option<Arc<dyn Meter>>,
    /// Extra fetch ports a test origin listens on; production has none.
    extra_fetch_ports: Vec<u16>,
}

impl WebState {
    pub(crate) fn meter(&self) -> Option<&dyn Meter> {
        self.meter.as_deref()
    }

    /// 443 for https, 80 for http when the deployment allows http.
    pub(crate) fn allowed_fetch_port(&self, scheme: &str, port: u16) -> bool {
        match (scheme, port) {
            ("https", 443) => true,
            ("http", 80) => self.config.allow_http,
            _ => self.extra_fetch_ports.contains(&port),
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum WebToolKind {
    Search,
    Fetch,
    HostSearch,
    HostLookup,
}

impl WebToolKind {
    const WEB: [Self; 2] = [Self::Search, Self::Fetch];
    const HOST: [Self; 2] = [Self::HostSearch, Self::HostLookup];

    const fn name(self) -> &'static str {
        match self {
            Self::Search => web_search::NAME,
            Self::Fetch => fetch::NAME,
            Self::HostSearch => host_intel::SEARCH,
            Self::HostLookup => host_intel::LOOKUP,
        }
    }

    const fn description(self) -> &'static str {
        match self {
            Self::Search => web_search::DESCRIPTION,
            Self::Fetch => fetch::DESCRIPTION,
            Self::HostSearch => host_intel::SEARCH_DESCRIPTION,
            Self::HostLookup => host_intel::LOOKUP_DESCRIPTION,
        }
    }

    fn schema(self) -> Value {
        match self {
            Self::Search => web_search::schema(),
            Self::Fetch => fetch::schema(),
            Self::HostSearch => host_intel::search_schema(),
            Self::HostLookup => host_intel::lookup_schema(),
        }
    }
}

struct WebTool {
    kind: WebToolKind,
    description: String,
    state: Arc<WebState>,
}

#[async_trait]
impl Tool for WebTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.kind.schema())
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let Some(arguments) = arguments.as_object() else {
            return Ok(error_result(
                "invalid_arguments",
                "the arguments must be an object",
            ));
        };
        let scope = CallScope {
            tool_call_id: context.function_call_id(),
        };
        let state = self.state.as_ref();
        let outcome = match self.kind {
            WebToolKind::Search => web_search::run(state, &scope, arguments).await,
            WebToolKind::Fetch => fetch::run(state, &scope, arguments).await,
            WebToolKind::HostSearch => host_intel::run_search(state, &scope, arguments).await,
            WebToolKind::HostLookup => host_intel::run_lookup(state, &scope, arguments).await,
        };
        match outcome {
            Ok(value) | Err(CallEnd::Result(value)) => Ok(value),
            Err(CallEnd::RateLimited { retry_after_secs }) => Err(AdkError::new(
                ErrorComponent::Tool,
                ErrorCategory::RateLimited,
                "web.provider.rate_limited",
                "the provider rate limited the request",
            )
            .with_retry(RetryHint {
                should_retry: true,
                retry_after_ms: retry_after_secs.map(|secs| secs.saturating_mul(1_000)),
                max_attempts: None,
            })),
        }
    }
}

/// How a toolset's provider transports are made: pinned reqwest clients in
/// production, fixtures in tests.
pub(crate) type TransportFactory<'a> = &'a dyn Fn(&RegistryEntry) -> Arc<dyn Transport>;

/// The parts a toolset is built from (the tests' seam).
pub(crate) struct Parts<'a> {
    pub(crate) host: Option<&'a WebToolHost>,
    pub(crate) transports: TransportFactory<'a>,
    pub(crate) extra_fetch_ports: Vec<u16>,
}

fn providers(
    configs: &BTreeMap<&'static str, config::ProviderConfig>,
    transports: TransportFactory<'_>,
) -> BTreeMap<&'static str, ProviderHandle> {
    configs
        .iter()
        .filter_map(|(id, config)| {
            let entry = registry::entry(id)?;
            Some((entry.id, (entry.construct)(config, transports(entry))))
        })
        .collect()
}

fn production_transport(
    host: Option<&WebToolHost>,
) -> impl Fn(&RegistryEntry) -> Arc<dyn Transport> {
    let (egress, user_agent) = host.map_or_else(
        || {
            (
                Arc::new(Allowlist::default()),
                PROVIDER_USER_AGENT.to_owned(),
            )
        },
        |host| (Arc::clone(&host.egress), PROVIDER_USER_AGENT.to_owned()),
    );
    move |entry: &RegistryEntry| -> Arc<dyn Transport> {
        match PinnedTransport::new(entry.origins, Arc::clone(&egress), &user_agent) {
            Ok(transport) => Arc::new(transport),
            Err(error) => Arc::new(FailingTransport(error)),
        }
    }
}

/// A transport that could not be built answers every call with its error.
struct FailingTransport(transport::ProviderError);

#[async_trait]
impl Transport for FailingTransport {
    async fn send(
        &self,
        _request: reqwest::Request,
        _max_bytes: usize,
    ) -> Result<transport::HttpAnswer, transport::ProviderError> {
        Err(self.0)
    }
}

/// Build the `web` toolset from Main's materialised settings.
pub(crate) fn build_web_toolset(
    toolkit_name: &str,
    settings: &Map<String, Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    host: Option<&WebToolHost>,
) -> Result<BasicToolset, WebToolsetError> {
    let transports = production_transport(host);
    build_web_with(
        toolkit_name,
        WebConfig::parse(settings)?,
        policy,
        &Parts {
            host,
            transports: &transports,
            extra_fetch_ports: Vec::new(),
        },
    )
}

/// Build the `host_intel` toolset from Main's materialised settings.
pub(crate) fn build_host_intel_toolset(
    toolkit_name: &str,
    settings: &Map<String, Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    host: Option<&WebToolHost>,
) -> Result<BasicToolset, WebToolsetError> {
    let transports = production_transport(host);
    build_host_intel_with(
        toolkit_name,
        &HostIntelConfig::parse(settings)?,
        policy,
        &Parts {
            host,
            transports: &transports,
            extra_fetch_ports: Vec::new(),
        },
    )
}

pub(crate) fn build_web_with(
    toolkit_name: &str,
    config: WebConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    parts: &Parts<'_>,
) -> Result<BasicToolset, WebToolsetError> {
    let providers = providers(&config.providers, parts.transports);
    let serves = |kind: Capability| {
        providers
            .values()
            .any(|handle| handle.kinds().contains(&kind))
    };
    let direct = config.direct_fetch_enabled.then(|| {
        let (lookup, permits, user_agent) = parts.host.map_or_else(
            || {
                (
                    Arc::new(SystemLookup) as Arc<dyn Lookup>,
                    Allowlist::default().private_permits(),
                    "EliteaFetch/1".to_owned(),
                )
            },
            |host| {
                (
                    Arc::clone(&host.lookup),
                    host.egress.private_permits(),
                    host.user_agent.clone(),
                )
            },
        );
        DirectFetcher::new(lookup, permits, user_agent)
    });
    let mut offered = Vec::new();
    if serves(Capability::WebSearch) {
        offered.push(WebToolKind::Search);
    }
    if direct.is_some() || serves(Capability::Fetch) {
        offered.push(WebToolKind::Fetch);
    }
    let selected = config.selected_tools.clone();
    let state = Arc::new(WebState {
        config,
        providers,
        direct,
        meter: parts.host.map(|host| Arc::clone(&host.meter)),
        extra_fetch_ports: parts.extra_fetch_ports.clone(),
    });
    assemble(
        toolkit_name,
        "web",
        policy,
        &WebToolKind::WEB,
        &offered,
        &selected,
        &state,
    )
}

pub(crate) fn build_host_intel_with(
    toolkit_name: &str,
    config: &HostIntelConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    parts: &Parts<'_>,
) -> Result<BasicToolset, WebToolsetError> {
    let providers = providers(&config.providers, parts.transports);
    let offered = if providers.values().any(|handle| handle.host_intel.is_some()) {
        WebToolKind::HOST.to_vec()
    } else {
        Vec::new()
    };
    let selected = config.selected_tools.clone();
    let state = Arc::new(WebState {
        config: WebConfig::for_host_intel(config.max_results_cap),
        providers,
        direct: None,
        meter: parts.host.map(|host| Arc::clone(&host.meter)),
        extra_fetch_ports: Vec::new(),
    });
    assemble(
        toolkit_name,
        "host_intel",
        policy,
        &WebToolKind::HOST,
        &offered,
        &selected,
        &state,
    )
}

fn assemble(
    toolkit_name: &str,
    toolkit_type: &str,
    policy: &Arc<ToolAdmissionPolicy>,
    family: &[WebToolKind],
    offered: &[WebToolKind],
    selected: &[String],
    state: &Arc<WebState>,
) -> Result<BasicToolset, WebToolsetError> {
    if selected
        .iter()
        .any(|name| !family.iter().any(|kind| kind.name() == name))
    {
        return Err(WebToolsetError::UnsupportedSelection);
    }
    let tools: Vec<Arc<dyn Tool>> = offered
        .iter()
        .filter(|kind| selected.is_empty() || selected.iter().any(|name| name == kind.name()))
        .map(|kind| {
            Arc::new(WebTool {
                kind: *kind,
                description: format!("Toolkit: {toolkit_name}\n{}", kind.description()),
                state: Arc::clone(state),
            }) as Arc<dyn Tool>
        })
        .collect();
    admit_materialized_toolset(toolkit_name, toolkit_type, policy, tools)
        .map_err(|_| WebToolsetError::InvalidDefinition)
}

/// Every tool the `web` family serves (`materialize::omitted_selection`).
pub(crate) fn web_tool_catalog() -> Vec<(&'static str, bool)> {
    WebToolKind::WEB
        .iter()
        .map(|kind| (kind.name(), false))
        .collect()
}

/// Every tool the `host_intel` family serves.
pub(crate) fn host_intel_tool_catalog() -> Vec<(&'static str, bool)> {
    WebToolKind::HOST
        .iter()
        .map(|kind| (kind.name(), false))
        .collect()
}
