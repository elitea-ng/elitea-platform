//! The claim-materialised settings of the `web` and `host_intel` toolkits.
//!
//! Main resolves them at claim time: `providers` holds only the providers
//! that are enabled for the deployment AND configured for the project (its
//! own configuration, else a shared public-project one where the provider
//! allows sharing), so a provider absent here cannot be called. Keys are
//! secrets: never `Debug`, never cloned into a long-lived public value.

use std::collections::BTreeMap;

use serde_json::{Map, Value};
use zeroize::Zeroizing;

use super::registry;

const MAX_API_KEY_BYTES: usize = 8 * 1_024;
const MAX_DOMAINS: usize = 1_000;
const MAX_DOMAIN_BYTES: usize = 253;
const MAX_SELECTED_TOOLS: usize = 64;
/// The admin default for `web_search` results.
pub(crate) const DEFAULT_SEARCH_RESULTS_CAP: u32 = 10;
/// The admin default for `internet_host_search` results.
pub(crate) const DEFAULT_HOST_RESULTS_CAP: u32 = 20;

/// Invalid materialised settings. Carries nothing from the settings.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct WebConfigError;

/// One provider's redeemed configuration.
pub(crate) struct ProviderConfig {
    api_key: Option<Zeroizing<String>>,
}

impl ProviderConfig {
    #[cfg(test)]
    pub(crate) fn with_key(key: &str) -> Self {
        Self {
            api_key: Some(Zeroizing::new(key.to_owned())),
        }
    }

    /// The API key, for an adapter whose provider needs one.
    pub(crate) fn api_key(&self) -> Option<&str> {
        self.api_key.as_ref().map(|key| key.as_str())
    }
}

/// The `web` toolkit's settings.
pub(crate) struct WebConfig {
    pub(crate) providers: BTreeMap<&'static str, ProviderConfig>,
    pub(crate) default_search_provider: Option<&'static str>,
    pub(crate) platform_default_search_provider: Option<&'static str>,
    pub(crate) default_fetch_backend: Option<&'static str>,
    pub(crate) direct_fetch_enabled: bool,
    pub(crate) strict_options: bool,
    pub(crate) fetch_allow_domains: Vec<String>,
    pub(crate) fetch_deny_domains: Vec<String>,
    pub(crate) allow_http: bool,
    pub(crate) max_results_cap: u32,
    pub(crate) selected_tools: Vec<String>,
}

/// The `host_intel` toolkit's settings.
pub(crate) struct HostIntelConfig {
    pub(crate) providers: BTreeMap<&'static str, ProviderConfig>,
    pub(crate) max_results_cap: u32,
    pub(crate) selected_tools: Vec<String>,
}

impl WebConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, WebConfigError> {
        let providers = providers(
            settings,
            &[registry::Capability::WebSearch, registry::Capability::Fetch],
        )?;
        let default_search_provider = provider_id(settings, "default_search_provider")?;
        // The platform default never names the degraded instant-answer
        // provider (Main refuses it on save); one that does is ignored, so
        // `ddg_instant` is only ever used when named explicitly.
        let platform_default_search_provider =
            provider_id(settings, "platform_default_search_provider")?.filter(|id| {
                registry::entry(id)
                    .is_some_and(|entry| entry.lifecycle == registry::Lifecycle::Active)
            });
        let default_fetch_backend = match settings.get("default_fetch_backend") {
            None | Some(Value::Null) => Some(super::DIRECT),
            Some(Value::String(value)) if value == super::DIRECT => Some(super::DIRECT),
            Some(Value::String(value)) => Some(
                registry::entry(value)
                    .map(|entry| entry.id)
                    .ok_or(WebConfigError)?,
            ),
            Some(_) => return Err(WebConfigError),
        };
        Ok(Self {
            providers,
            default_search_provider,
            platform_default_search_provider,
            default_fetch_backend,
            direct_fetch_enabled: flag(settings, "direct_fetch_enabled", true)?,
            strict_options: flag(settings, "strict_options", true)?,
            fetch_allow_domains: domains(settings, "fetch_allow_domains")?,
            fetch_deny_domains: domains(settings, "fetch_deny_domains")?,
            allow_http: flag(settings, "allow_http", false)?,
            max_results_cap: cap(settings, "max_results_cap", DEFAULT_SEARCH_RESULTS_CAP)?,
            selected_tools: selected_tools(settings)?,
        })
    }
}

impl WebConfig {
    /// The settings a `host_intel` toolset's shared state carries: its
    /// result cap; nothing else applies to host queries.
    pub(crate) fn for_host_intel(max_results_cap: u32) -> Self {
        Self {
            providers: BTreeMap::new(),
            default_search_provider: None,
            platform_default_search_provider: None,
            default_fetch_backend: None,
            direct_fetch_enabled: false,
            strict_options: true,
            fetch_allow_domains: Vec::new(),
            fetch_deny_domains: Vec::new(),
            allow_http: false,
            max_results_cap,
            selected_tools: Vec::new(),
        }
    }
}

impl HostIntelConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, WebConfigError> {
        Ok(Self {
            providers: providers(settings, &[registry::Capability::HostIntel])?,
            max_results_cap: cap(settings, "max_results_cap", DEFAULT_HOST_RESULTS_CAP)?,
            selected_tools: selected_tools(settings)?,
        })
    }
}

/// The providers of this toolkit's capabilities. A provider of another
/// capability, or one the registry does not know, is refused: Main only
/// materialises what this runtime serves.
fn providers(
    settings: &Map<String, Value>,
    capabilities: &[registry::Capability],
) -> Result<BTreeMap<&'static str, ProviderConfig>, WebConfigError> {
    let mut out = BTreeMap::new();
    let Some(value) = settings.get("providers") else {
        return Ok(out);
    };
    let map = value.as_object().ok_or(WebConfigError)?;
    for (id, config) in map {
        let entry = registry::entry(id).ok_or(WebConfigError)?;
        if !entry
            .capabilities
            .iter()
            .any(|kind| capabilities.contains(kind))
        {
            return Err(WebConfigError);
        }
        let config = config.as_object().ok_or(WebConfigError)?;
        let api_key = match config.get("api_key") {
            None | Some(Value::Null) => None,
            Some(Value::String(key)) => {
                if key.len() > MAX_API_KEY_BYTES
                    || key.is_empty()
                    || !key.bytes().all(|byte| (0x21..=0x7e).contains(&byte))
                {
                    return Err(WebConfigError);
                }
                Some(Zeroizing::new(key.clone()))
            }
            Some(_) => return Err(WebConfigError),
        };
        if entry.needs_key && api_key.is_none() {
            return Err(WebConfigError);
        }
        out.insert(entry.id, ProviderConfig { api_key });
    }
    Ok(out)
}

fn provider_id(
    settings: &Map<String, Value>,
    key: &str,
) -> Result<Option<&'static str>, WebConfigError> {
    match settings.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if value.is_empty() => Ok(None),
        Some(Value::String(value)) => registry::entry(value)
            .map(|entry| Some(entry.id))
            .ok_or(WebConfigError),
        Some(_) => Err(WebConfigError),
    }
}

fn flag(settings: &Map<String, Value>, key: &str, default: bool) -> Result<bool, WebConfigError> {
    match settings.get(key) {
        None | Some(Value::Null) => Ok(default),
        Some(Value::Bool(value)) => Ok(*value),
        Some(_) => Err(WebConfigError),
    }
}

fn cap(settings: &Map<String, Value>, key: &str, default: u32) -> Result<u32, WebConfigError> {
    match settings.get(key) {
        None | Some(Value::Null) => Ok(default),
        Some(value) => value
            .as_u64()
            .and_then(|value| u32::try_from(value).ok())
            .filter(|value| (1..=100).contains(value))
            .ok_or(WebConfigError),
    }
}

/// A domain list entry: a lower-case host name, matched as itself and its
/// subdomains.
fn domains(settings: &Map<String, Value>, key: &str) -> Result<Vec<String>, WebConfigError> {
    let Some(value) = settings.get(key) else {
        return Ok(Vec::new());
    };
    if value.is_null() {
        return Ok(Vec::new());
    }
    let values = value.as_array().ok_or(WebConfigError)?;
    if values.len() > MAX_DOMAINS {
        return Err(WebConfigError);
    }
    values
        .iter()
        .map(|value| {
            let domain = value
                .as_str()
                .ok_or(WebConfigError)?
                .trim()
                .trim_start_matches("*.")
                .trim_end_matches('.')
                .to_ascii_lowercase();
            if domain.is_empty()
                || domain.len() > MAX_DOMAIN_BYTES
                || !domain
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'-'))
            {
                return Err(WebConfigError);
            }
            Ok(domain)
        })
        .collect()
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<String>, WebConfigError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(Vec::new());
    };
    let values = value.as_array().ok_or(WebConfigError)?;
    if values.len() > MAX_SELECTED_TOOLS {
        return Err(WebConfigError);
    }
    let mut out: Vec<String> = Vec::with_capacity(values.len());
    for value in values {
        let name = value.as_str().ok_or(WebConfigError)?;
        if !out.iter().any(|existing| existing == name) {
            out.push(name.to_owned());
        }
    }
    Ok(out)
}

/// Whether `host` is `domain` or one of its subdomains.
pub(crate) fn host_in_domain(host: &str, domain: &str) -> bool {
    host == domain
        || host
            .strip_suffix(domain)
            .is_some_and(|prefix| prefix.ends_with('.'))
}
