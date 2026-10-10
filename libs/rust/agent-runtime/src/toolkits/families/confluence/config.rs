use std::collections::HashSet;
use std::fmt;

use reqwest::Url;
use reqwest::header::{HeaderName, HeaderValue};
use serde_json::{Map, Value};
use zeroize::Zeroizing;

use crate::toolkits::families::UnsupportedSetting;
use crate::toolkits::families::https_base_url::{
    self, BasePath, BaseUrlError, host_matches_domain,
};
use crate::toolkits::is_reserved_platform_header;

const MAX_URL_BYTES: usize = 2 * 1_024;
const MAX_SECRET_BYTES: usize = 16 * 1_024;
const MAX_IDENTITY_BYTES: usize = 1_024;
const MAX_SPACE_BYTES: usize = 255;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;
const MAX_LABELS: usize = 64;
const MAX_LABEL_BYTES: usize = 255;
const MAX_CUSTOM_HEADERS: usize = 32;
const MAX_HEADER_VALUE_BYTES: usize = 8 * 1_024;
/// The SDK's per-request page size default.
const DEFAULT_LIMIT: usize = 5;
/// Confluence caps a page of results at 100 whatever is asked.
const MAX_LIMIT: usize = 100;
/// The SDK's default total of pages a search or label read returns.
const DEFAULT_MAX_PAGES: usize = 10;
/// Every returned page carries its rendered content; one invocation stops
/// here whatever the toolkit asks.
const MAX_MAX_PAGES: usize = 50;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ConfluenceConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    /// A setting the SDK honours that this runtime deliberately does not
    /// (plain-HTTP origins, `verify_ssl: false`, custom headers that would
    /// replace the credential). The toolkit is left out of the agent and the
    /// run names the setting (`materialize::RefusedToolkit`).
    UnsupportedCapability(UnsupportedSetting),
}

/// Stable configuration failure that never carries credentials or origins.
pub(crate) struct ConfluenceConfigError {
    code: ConfluenceConfigErrorCode,
}

impl ConfluenceConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> ConfluenceConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for ConfluenceConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ConfluenceConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ConfluenceConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ConfluenceConfigErrorCode::InvalidConfiguration => {
                "the Confluence toolkit configuration is invalid"
            }
            ConfluenceConfigErrorCode::ResourceExhausted => {
                "the Confluence toolkit configuration exceeds its approved limit"
            }
            ConfluenceConfigErrorCode::UnsupportedCapability(_) => {
                "the Confluence toolkit configuration requires a capability this runtime does not provide"
            }
        })
    }
}

impl std::error::Error for ConfluenceConfigError {}

/// The Confluence REST API `create_page` uses: v1 `/rest/api/content`, or
/// Cloud's v2 `/api/v2/pages`. Every other operation is v1, as in the SDK.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ConfluenceApiVersion {
    V1,
    V2,
}

pub(super) enum ConfluenceCredential {
    Bearer(Zeroizing<String>),
    /// A token carrying `JSESSIONID`: the SDK sends it as session cookies.
    Cookie(Zeroizing<String>),
    Basic {
        username: Box<str>,
        password: Zeroizing<String>,
    },
}

/// Invocation-scoped Confluence authority materialized from one claim.
pub(crate) struct ConfluenceToolkitConfig {
    /// The SDK's normalised `base_url` (a Cloud `/wiki` suffix removed);
    /// page links are built from it.
    base_url: Url,
    /// The `atlassian` client's root: `base_url`, plus `/wiki` for an
    /// Atlassian-hosted URL that lacks it. Every REST path hangs off it.
    api_root: Url,
    credential: ConfluenceCredential,
    cloud: bool,
    api_version: ConfluenceApiVersion,
    space: Option<Box<str>>,
    limit: usize,
    max_pages: usize,
    labels: Vec<Box<str>>,
    custom_headers: Vec<(HeaderName, HeaderValue)>,
    selected_tools: Vec<Box<str>>,
}

impl ConfluenceToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, ConfluenceConfigError> {
        let configuration = settings
            .get("confluence_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let raw_url = required_text(configuration, "base_url", MAX_URL_BYTES)?;
        let mut base_url = parse_base_url(raw_url.trim())?;
        let host_is_cloud = host_matches_domain(&base_url, "atlassian.net");

        // Toolkit-level `cloud` wins; otherwise the credential's hosting,
        // auto-detected from the host (`_hosting_to_cloud`).
        let cloud = match settings.get("cloud") {
            None | Some(Value::Null) => {
                match optional_text(configuration, "hosting", MAX_IDENTITY_BYTES)?
                    .map(|value| value.trim().to_ascii_lowercase())
                    .as_deref()
                {
                    Some("cloud") => true,
                    Some("server") => false,
                    _ => host_is_cloud,
                }
            }
            Some(Value::Bool(value)) => *value,
            Some(_) => return Err(invalid_configuration()),
        };
        // `validate_toolkit`: a Cloud base URL loses its `/wiki` suffix.
        if cloud && base_url.path().ends_with("/wiki") {
            let path = base_url.path().trim_end_matches("/wiki").to_owned();
            base_url.set_path(&path);
        }
        // `Confluence.__init__`: an Atlassian URL without `/wiki` gets it
        // (the library tests the URL text, so `jira.com` counts too).
        let mut api_root = base_url.clone();
        let text = base_url.as_str();
        if (text.contains("atlassian.net") || text.contains("jira.com")) && !text.contains("/wiki")
        {
            let path = format!("{}/wiki", api_root.path().trim_end_matches('/'));
            api_root.set_path(&path);
        }
        // `_resolve_confluence_api_version`.
        let api_version = match settings.get("api_version") {
            Some(Value::String(value)) if value.trim() == "1" => ConfluenceApiVersion::V1,
            Some(Value::String(value)) if value.trim() == "2" => ConfluenceApiVersion::V2,
            Some(Value::Number(value)) if value.as_u64() == Some(1) => ConfluenceApiVersion::V1,
            Some(Value::Number(value)) if value.as_u64() == Some(2) => ConfluenceApiVersion::V2,
            None | Some(Value::Null | Value::String(_)) => {
                if cloud || host_is_cloud {
                    ConfluenceApiVersion::V2
                } else {
                    ConfluenceApiVersion::V1
                }
            }
            Some(_) => return Err(invalid_configuration()),
        };

        match settings.get("verify_ssl") {
            None | Some(Value::Null | Value::Bool(true)) => {}
            Some(Value::Bool(false)) => {
                return Err(unsupported_capability(
                    UnsupportedSetting::VerifySslDisabled,
                ));
            }
            Some(_) => return Err(invalid_configuration()),
        }

        let space = optional_text(settings, "space", MAX_SPACE_BYTES)?
            .map(str::trim)
            .filter(|space| !space.is_empty())
            .map(Into::into);

        Ok(Self {
            base_url,
            api_root,
            credential: credential(configuration)?,
            cloud,
            api_version,
            space,
            limit: bounded_count(settings, "limit", DEFAULT_LIMIT, MAX_LIMIT)?,
            max_pages: bounded_count(settings, "max_pages", DEFAULT_MAX_PAGES, MAX_MAX_PAGES)?,
            labels: labels(settings)?,
            custom_headers: custom_headers(settings)?,
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) const fn base_url(&self) -> &Url {
        &self.base_url
    }

    pub(super) const fn api_root(&self) -> &Url {
        &self.api_root
    }

    pub(super) const fn credential(&self) -> &ConfluenceCredential {
        &self.credential
    }

    pub(super) const fn cloud(&self) -> bool {
        self.cloud
    }

    pub(crate) const fn api_version(&self) -> ConfluenceApiVersion {
        self.api_version
    }

    pub(super) fn space(&self) -> Option<&str> {
        self.space.as_deref()
    }

    pub(super) const fn limit(&self) -> usize {
        self.limit
    }

    pub(super) const fn max_pages(&self) -> usize {
        self.max_pages
    }

    pub(super) fn labels(&self) -> &[Box<str>] {
        &self.labels
    }

    pub(super) fn custom_headers(&self) -> &[(HeaderName, HeaderValue)] {
        &self.custom_headers
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_urls(&self) -> (&str, &str) {
        (self.base_url.as_str(), self.api_root.as_str())
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn test_cloud(&self) -> bool {
        self.cloud
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn test_paging(&self) -> (usize, usize) {
        (self.limit, self.max_pages)
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_labels(&self) -> Vec<&str> {
        self.labels.iter().map(AsRef::as_ref).collect()
    }
}

/// HTTPS origin plus an optional context path, never user info, query or
/// fragment (the shared [`https_base_url`] rule). Stored without a trailing
/// slash; plain HTTP is an unsupported capability.
fn parse_base_url(value: &str) -> Result<Url, ConfluenceConfigError> {
    https_base_url::parse(value, BasePath::Prefix).map_err(|error| match error {
        BaseUrlError::PlainHttp => unsupported_capability(UnsupportedSetting::PlainHttp),
        BaseUrlError::TooLong => resource_exhausted(),
        BaseUrlError::Invalid => invalid_configuration(),
    })
}

fn credential(
    configuration: &Map<String, Value>,
) -> Result<ConfluenceCredential, ConfluenceConfigError> {
    if let Some(token) = optional_secret(configuration, "token")? {
        let token = token.trim();
        if token.is_empty() {
            return Err(invalid_configuration());
        }
        return Ok(if token.contains("JSESSIONID") {
            ConfluenceCredential::Cookie(Zeroizing::new(token.to_owned()))
        } else {
            ConfluenceCredential::Bearer(Zeroizing::new(token.to_owned()))
        });
    }
    let username = optional_text(configuration, "username", MAX_IDENTITY_BYTES)?
        .filter(|value| !value.is_empty());
    let api_key = optional_secret(configuration, "api_key")?;
    match (username, api_key) {
        (Some(username), Some(api_key)) if !username.contains(':') => {
            Ok(ConfluenceCredential::Basic {
                username: username.into(),
                password: Zeroizing::new(api_key.to_owned()),
            })
        }
        // The SDK refuses this too: "Either 'token' or both 'api_key' and
        // 'username' must be provided for authentication."
        _ => Err(invalid_configuration()),
    }
}

fn bounded_count(
    settings: &Map<String, Value>,
    name: &str,
    default: usize,
    max: usize,
) -> Result<usize, ConfluenceConfigError> {
    match settings.get(name) {
        None | Some(Value::Null) => Ok(default),
        Some(Value::Number(value)) => {
            let value = value.as_u64().ok_or_else(invalid_configuration)?;
            if value == 0 {
                return Err(invalid_configuration());
            }
            Ok(usize::try_from(value).unwrap_or(max).min(max))
        }
        Some(_) => Err(invalid_configuration()),
    }
}

/// `parse_list`: a comma/semicolon string or a string array; blank items are
/// dropped rather than sent as empty labels.
fn labels(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, ConfluenceConfigError> {
    let items: Vec<&str> = match settings.get("labels") {
        None | Some(Value::Null) => return Ok(Vec::new()),
        Some(Value::String(value)) => value.split([',', ';']).collect(),
        Some(Value::Array(values)) => values
            .iter()
            .map(|value| value.as_str().ok_or_else(invalid_configuration))
            .collect::<Result<_, _>>()?,
        Some(_) => return Err(invalid_configuration()),
    };
    let mut out: Vec<Box<str>> = Vec::new();
    for item in items {
        let item = item.trim();
        if item.is_empty() {
            continue;
        }
        if item.len() > MAX_LABEL_BYTES {
            return Err(resource_exhausted());
        }
        if item.chars().any(char::is_control) {
            return Err(invalid_configuration());
        }
        if !out.iter().any(|existing| existing.as_ref() == item) {
            out.push(item.into());
        }
    }
    if out.len() > MAX_LABELS {
        return Err(resource_exhausted());
    }
    Ok(out)
}

fn custom_headers(
    settings: &Map<String, Value>,
) -> Result<Vec<(HeaderName, HeaderValue)>, ConfluenceConfigError> {
    let headers = match settings.get("custom_headers") {
        None | Some(Value::Null) => return Ok(Vec::new()),
        Some(Value::Object(headers)) => headers,
        Some(_) => return Err(invalid_configuration()),
    };
    if headers.len() > MAX_CUSTOM_HEADERS {
        return Err(resource_exhausted());
    }
    let mut out = Vec::with_capacity(headers.len());
    for (name, value) in headers {
        let value = value.as_str().ok_or_else(invalid_configuration)?;
        if value.len() > MAX_HEADER_VALUE_BYTES {
            return Err(resource_exhausted());
        }
        let name = HeaderName::from_bytes(name.as_bytes()).map_err(|_| invalid_configuration())?;
        if is_reserved_platform_header(name.as_str())
            || matches!(
                name.as_str(),
                "authorization"
                    | "proxy-authorization"
                    | "cookie"
                    | "host"
                    | "content-length"
                    | "transfer-encoding"
                    | "connection"
                    | "content-type"
                    | "accept"
            )
        {
            return Err(unsupported_capability(UnsupportedSetting::ReservedHeader));
        }
        let mut value = HeaderValue::from_str(value).map_err(|_| invalid_configuration())?;
        value.set_sensitive(true);
        out.push((name, value));
    }
    Ok(out)
}

fn required_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, ConfluenceConfigError> {
    optional_text(object, name, limit)?.ok_or_else(invalid_configuration)
}

fn optional_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<Option<&'a str>, ConfluenceConfigError> {
    match object.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => {
            if value.len() > limit {
                return Err(resource_exhausted());
            }
            if value.chars().any(char::is_control) {
                return Err(invalid_configuration());
            }
            Ok(Some(value))
        }
        Some(_) => Err(invalid_configuration()),
    }
}

fn optional_secret<'a>(
    object: &'a Map<String, Value>,
    name: &str,
) -> Result<Option<&'a str>, ConfluenceConfigError> {
    optional_text(object, name, MAX_SECRET_BYTES)
        .map(|value| value.filter(|value| !value.is_empty()))
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, ConfluenceConfigError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(Vec::new());
    };
    if value.is_null() {
        return Ok(Vec::new());
    }
    let values = value.as_array().ok_or_else(invalid_configuration)?;
    if values.len() > MAX_SELECTED_TOOLS {
        return Err(resource_exhausted());
    }
    let mut seen = HashSet::with_capacity(values.len());
    let mut selected = Vec::with_capacity(values.len());
    for value in values {
        let value = value.as_str().ok_or_else(invalid_configuration)?;
        if value.is_empty() || value.len() > MAX_TOOL_NAME_BYTES {
            return Err(invalid_configuration());
        }
        if seen.insert(value) {
            selected.push(value.into());
        }
    }
    Ok(selected)
}

const fn invalid_configuration() -> ConfluenceConfigError {
    ConfluenceConfigError {
        code: ConfluenceConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> ConfluenceConfigError {
    ConfluenceConfigError {
        code: ConfluenceConfigErrorCode::ResourceExhausted,
    }
}

const fn unsupported_capability(setting: UnsupportedSetting) -> ConfluenceConfigError {
    ConfluenceConfigError {
        code: ConfluenceConfigErrorCode::UnsupportedCapability(setting),
    }
}
