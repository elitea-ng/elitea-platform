use std::collections::HashSet;
use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

/// The SDK's `XrayApiWrapper._default_base_url`.
pub(super) const DEFAULT_BASE_URL: &str = "https://xray.cloud.getxray.app";
/// `get_tools` reads `settings.get('limit', 20)`.
const DEFAULT_LIMIT: u64 = 20;
const MAX_LIMIT: u64 = 1_000;
const MAX_URL_BYTES: usize = 2 * 1_024;
const MAX_CLIENT_ID_BYTES: usize = 1_024;
const MAX_SECRET_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum XrayConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that never carries the client credentials.
pub(crate) struct XrayConfigError {
    code: XrayConfigErrorCode,
}

impl XrayConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> XrayConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for XrayConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("XrayConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for XrayConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            XrayConfigErrorCode::InvalidConfiguration => {
                "the Xray Cloud toolkit configuration is invalid"
            }
            XrayConfigErrorCode::ResourceExhausted => {
                "the Xray Cloud toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for XrayConfigError {}

/// Invocation-scoped Xray Cloud authority materialized from one accepted
/// claim: `xray_configuration.{base_url,client_id,client_secret}` plus the
/// toolkit's `limit` (the `getTests` page size).
///
/// Non-`Clone` and non-`Debug` so the client secret cannot be copied or
/// logged.
pub(crate) struct XrayToolkitConfig {
    base: Url,
    client_id: Box<str>,
    client_secret: Zeroizing<String>,
    limit: u64,
    selected_tools: Vec<Box<str>>,
}

impl XrayToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, XrayConfigError> {
        let configuration = settings
            .get("xray_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let base = match configuration.get("base_url") {
            None | Some(Value::Null) => DEFAULT_BASE_URL,
            Some(Value::String(value)) if value.trim().is_empty() => DEFAULT_BASE_URL,
            Some(Value::String(value)) => value,
            Some(_) => return Err(invalid_configuration()),
        };
        let base = parse_base(base)?;
        let client_id = required_text(configuration, "client_id", MAX_CLIENT_ID_BYTES)?;
        let client_secret = required_text(configuration, "client_secret", MAX_SECRET_BYTES)?;
        let limit = match settings.get("limit") {
            None | Some(Value::Null) => DEFAULT_LIMIT,
            Some(value) => value
                .as_u64()
                .filter(|limit| (1..=MAX_LIMIT).contains(limit))
                .ok_or_else(invalid_configuration)?,
        };
        Ok(Self {
            base,
            client_id: client_id.into(),
            client_secret: Zeroizing::new(client_secret.to_owned()),
            limit,
            selected_tools: selected_tools(settings)?,
        })
    }

    /// The instance root without a trailing slash, as the SDK prints it.
    pub(super) fn base_url(&self) -> &str {
        self.base.as_str().trim_end_matches('/')
    }

    pub(super) fn client_id(&self) -> &str {
        &self.client_id
    }

    pub(super) fn client_secret(&self) -> &str {
        &self.client_secret
    }

    pub(super) const fn limit(&self) -> u64 {
        self.limit
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

/// HTTPS origin, optional path prefix, no userinfo/query/fragment.
fn parse_base(value: &str) -> Result<Url, XrayConfigError> {
    let value = value.trim();
    if value.len() > MAX_URL_BYTES {
        return Err(resource_exhausted());
    }
    if value.chars().any(char::is_control) || value.contains(['\\', '%', '?', '#']) {
        return Err(invalid_configuration());
    }
    let mut url = Url::parse(value).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url
            .path()
            .split('/')
            .any(|segment| matches!(segment, "." | ".."))
    {
        return Err(invalid_configuration());
    }
    let path = url.path().trim_end_matches('/').to_owned();
    url.set_path(&path);
    Ok(url)
}

fn required_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, XrayConfigError> {
    let value = object
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_configuration());
    }
    Ok(value)
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, XrayConfigError> {
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
        if value.is_empty() || value.len() > MAX_TOOL_NAME_BYTES || value.contains('\0') {
            return Err(invalid_configuration());
        }
        if seen.insert(value) {
            selected.push(value.into());
        }
    }
    Ok(selected)
}

const fn invalid_configuration() -> XrayConfigError {
    XrayConfigError {
        code: XrayConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> XrayConfigError {
    XrayConfigError {
        code: XrayConfigErrorCode::ResourceExhausted,
    }
}
