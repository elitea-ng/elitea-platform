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
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;
const MAX_LABELS: usize = 64;
const MAX_LABEL_BYTES: usize = 255;
const MAX_ADDITIONAL_FIELDS: usize = 64;
const MAX_FIELD_NAME_BYTES: usize = 256;
const MAX_CUSTOM_HEADERS: usize = 32;
const MAX_HEADER_VALUE_BYTES: usize = 8 * 1_024;
/// The SDK's default `limit` (issues per search when the call names none).
const DEFAULT_LIMIT: usize = 5;
/// The ceiling on any search total. The SDK has none (its `0` meant "no
/// cap"); one bounded invocation stops here.
pub(super) const MAX_SEARCH_RESULTS: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum JiraConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    /// A setting the SDK honours that this runtime deliberately does not
    /// (plain-HTTP origins, `verify_ssl: false`, a reserved custom header).
    /// The toolkit is left out of the agent and the run names the setting
    /// (`materialize::RefusedToolkit`), rather than failing the whole agent
    /// or vanishing silently.
    UnsupportedCapability(UnsupportedSetting),
}

/// Stable configuration failure that never carries credentials or origins.
pub(crate) struct JiraConfigError {
    code: JiraConfigErrorCode,
}

impl JiraConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> JiraConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for JiraConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("JiraConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for JiraConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            JiraConfigErrorCode::InvalidConfiguration => {
                "the Jira toolkit configuration is invalid"
            }
            JiraConfigErrorCode::ResourceExhausted => {
                "the Jira toolkit configuration exceeds its approved limit"
            }
            JiraConfigErrorCode::UnsupportedCapability(_) => {
                "the Jira toolkit configuration requires a capability this runtime does not provide"
            }
        })
    }
}

impl std::error::Error for JiraConfigError {}

/// The Jira REST API version every operation uses (`/rest/api/{2,3}`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum JiraApiVersion {
    V2,
    V3,
}

impl JiraApiVersion {
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::V2 => "2",
            Self::V3 => "3",
        }
    }
}

pub(super) enum JiraCredential {
    Bearer(Zeroizing<String>),
    /// A token carrying `JSESSIONID`: the SDK sends it as session cookies.
    Cookie(Zeroizing<String>),
    Basic {
        username: Box<str>,
        password: Zeroizing<String>,
    },
}

/// Invocation-scoped Jira authority materialized from one accepted claim.
///
/// The SDK keeps its client in a process-wide registry keyed by credential;
/// here each materialized toolset owns exactly one origin and credential, and
/// the type is deliberately neither `Clone` nor `Debug`.
pub(crate) struct JiraToolkitConfig {
    base_url: Url,
    credential: JiraCredential,
    cloud: bool,
    api_version: JiraApiVersion,
    limit: usize,
    labels: Vec<Box<str>>,
    additional_fields: Vec<Box<str>>,
    custom_headers: Vec<(HeaderName, HeaderValue)>,
    selected_tools: Vec<Box<str>>,
}

impl JiraToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, JiraConfigError> {
        let configuration = settings
            .get("jira_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let raw_url = required_text(configuration, "base_url", MAX_URL_BYTES)?;
        let base_url = parse_base_url(raw_url.trim())?;
        let host_is_cloud = host_matches_domain(&base_url, "atlassian.net");

        // Toolkit-level `cloud` wins during the SDK's transition period;
        // otherwise the credential's hosting decides, auto-detecting from
        // the host (`_hosting_to_cloud`).
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
        // `_resolve_api_version`: an explicit 2/3 wins; anything else
        // (Auto, auto, null) resolves from cloud, then from the host.
        let api_version = match settings.get("api_version") {
            Some(Value::String(value)) if value.trim() == "2" => JiraApiVersion::V2,
            Some(Value::String(value)) if value.trim() == "3" => JiraApiVersion::V3,
            Some(Value::Number(value)) if value.as_u64() == Some(2) => JiraApiVersion::V2,
            Some(Value::Number(value)) if value.as_u64() == Some(3) => JiraApiVersion::V3,
            None | Some(Value::Null | Value::String(_)) => {
                if cloud || host_is_cloud {
                    JiraApiVersion::V3
                } else {
                    JiraApiVersion::V2
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

        Ok(Self {
            base_url,
            credential: credential(configuration)?,
            cloud,
            api_version,
            limit: limit(settings)?,
            labels: list_setting(settings, "labels", &[',', ';'], MAX_LABELS, MAX_LABEL_BYTES)?,
            additional_fields: list_setting(
                settings,
                "additional_fields",
                &[','],
                MAX_ADDITIONAL_FIELDS,
                MAX_FIELD_NAME_BYTES,
            )?,
            custom_headers: custom_headers(settings)?,
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) const fn base_url(&self) -> &Url {
        &self.base_url
    }

    pub(super) const fn credential(&self) -> &JiraCredential {
        &self.credential
    }

    pub(super) const fn cloud(&self) -> bool {
        self.cloud
    }

    pub(crate) const fn api_version(&self) -> JiraApiVersion {
        self.api_version
    }

    pub(super) const fn limit(&self) -> usize {
        self.limit
    }

    pub(super) fn labels(&self) -> &[Box<str>] {
        &self.labels
    }

    pub(super) fn additional_fields(&self) -> &[Box<str>] {
        &self.additional_fields
    }

    pub(super) fn custom_headers(&self) -> &[(HeaderName, HeaderValue)] {
        &self.custom_headers
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_base_url(&self) -> &str {
        self.base_url.as_str()
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn test_cloud(&self) -> bool {
        self.cloud
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn test_limit(&self) -> usize {
        self.limit
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_labels(&self) -> Vec<&str> {
        self.labels.iter().map(AsRef::as_ref).collect()
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_additional_fields(&self) -> Vec<&str> {
        self.additional_fields.iter().map(AsRef::as_ref).collect()
    }
}

/// HTTPS origin plus an optional context path (`https://host/jira`), never
/// user info, query or fragment (the shared [`https_base_url`] rule). Stored
/// without a trailing slash; plain HTTP is an unsupported capability.
fn parse_base_url(value: &str) -> Result<Url, JiraConfigError> {
    https_base_url::parse(value, BasePath::Prefix).map_err(|error| match error {
        BaseUrlError::PlainHttp => unsupported_capability(UnsupportedSetting::PlainHttp),
        BaseUrlError::TooLong => resource_exhausted(),
        BaseUrlError::Invalid => invalid_configuration(),
    })
}

fn credential(configuration: &Map<String, Value>) -> Result<JiraCredential, JiraConfigError> {
    if let Some(token) = optional_secret(configuration, "token")? {
        let token = token.trim();
        if token.is_empty() {
            return Err(invalid_configuration());
        }
        return Ok(if token.contains("JSESSIONID") {
            JiraCredential::Cookie(Zeroizing::new(token.to_owned()))
        } else {
            JiraCredential::Bearer(Zeroizing::new(token.to_owned()))
        });
    }
    let username = optional_text(configuration, "username", MAX_IDENTITY_BYTES)?
        .filter(|value| !value.is_empty());
    let api_key = optional_secret(configuration, "api_key")?;
    match (username, api_key) {
        (Some(username), Some(api_key)) => {
            if username.contains(':') {
                return Err(invalid_configuration());
            }
            Ok(JiraCredential::Basic {
                username: username.into(),
                password: Zeroizing::new(api_key.to_owned()),
            })
        }
        _ => Err(invalid_configuration()),
    }
}

fn limit(settings: &Map<String, Value>) -> Result<usize, JiraConfigError> {
    match settings.get("limit") {
        None | Some(Value::Null) => Ok(DEFAULT_LIMIT),
        Some(Value::Number(value)) => {
            let value = value.as_u64().ok_or_else(invalid_configuration)?;
            if value == 0 {
                return Err(invalid_configuration());
            }
            Ok(usize::try_from(value)
                .unwrap_or(MAX_SEARCH_RESULTS)
                .min(MAX_SEARCH_RESULTS))
        }
        Some(_) => Err(invalid_configuration()),
    }
}

/// A comma/semicolon string (`parse_list`) or a string array; blank items
/// are dropped rather than sent as empty labels or field ids.
fn list_setting(
    settings: &Map<String, Value>,
    name: &str,
    separators: &[char],
    max_items: usize,
    max_item_bytes: usize,
) -> Result<Vec<Box<str>>, JiraConfigError> {
    let items: Vec<&str> = match settings.get(name) {
        None | Some(Value::Null) => return Ok(Vec::new()),
        Some(Value::String(value)) => value.split(separators).collect(),
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
        if item.len() > max_item_bytes {
            return Err(resource_exhausted());
        }
        if item.chars().any(char::is_control) {
            return Err(invalid_configuration());
        }
        if !out.iter().any(|existing| existing.as_ref() == item) {
            out.push(item.into());
        }
    }
    if out.len() > max_items {
        return Err(resource_exhausted());
    }
    Ok(out)
}

fn custom_headers(
    settings: &Map<String, Value>,
) -> Result<Vec<(HeaderName, HeaderValue)>, JiraConfigError> {
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
        // The credential is the configuration's, not a header's: a custom
        // header may not replace it, nor carry platform identity or framing.
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
) -> Result<&'a str, JiraConfigError> {
    optional_text(object, name, limit)?.ok_or_else(invalid_configuration)
}

fn optional_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<Option<&'a str>, JiraConfigError> {
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
) -> Result<Option<&'a str>, JiraConfigError> {
    optional_text(object, name, MAX_SECRET_BYTES)
        .map(|value| value.filter(|value| !value.is_empty()))
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, JiraConfigError> {
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

const fn invalid_configuration() -> JiraConfigError {
    JiraConfigError {
        code: JiraConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> JiraConfigError {
    JiraConfigError {
        code: JiraConfigErrorCode::ResourceExhausted,
    }
}

const fn unsupported_capability(setting: UnsupportedSetting) -> JiraConfigError {
    JiraConfigError {
        code: JiraConfigErrorCode::UnsupportedCapability(setting),
    }
}
