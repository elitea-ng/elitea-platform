use std::collections::HashSet;
use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_BASE_URL_BYTES: usize = 2_048;
const MAX_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ZephyrRestConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that never carries an origin or a token.
pub(crate) struct ZephyrRestConfigError {
    code: ZephyrRestConfigErrorCode,
}

impl ZephyrRestConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> ZephyrRestConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for ZephyrRestConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ZephyrRestConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ZephyrRestConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ZephyrRestConfigErrorCode::InvalidConfiguration => {
                "the Zephyr toolkit configuration is invalid"
            }
            ZephyrRestConfigErrorCode::ResourceExhausted => {
                "the Zephyr toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for ZephyrRestConfigError {}

/// The nested configuration object Main freezes under `key`.
pub(in crate::toolkits) fn configuration_object<'a>(
    settings: &'a Map<String, Value>,
    key: &str,
) -> Result<&'a Map<String, Value>, ZephyrRestConfigError> {
    settings
        .get(key)
        .and_then(Value::as_object)
        .ok_or_else(invalid_configuration)
}

/// An HTTPS base URL with an optional path prefix and no trailing slash.
///
/// Credentials, queries and fragments are refused: the base is an authority
/// boundary, and every request path is appended to it segment by segment.
pub(in crate::toolkits) fn parse_https_base(value: &str) -> Result<Url, ZephyrRestConfigError> {
    let value = value.trim();
    if value.len() > MAX_BASE_URL_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty()
        || value.bytes().any(|byte| byte.is_ascii_control())
        || value.contains(['%', '\\'])
    {
        return Err(invalid_configuration());
    }
    let mut url = Url::parse(value).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err(invalid_configuration());
    }
    let path = url.path().trim_end_matches('/').to_owned();
    url.set_path(&path);
    Ok(url)
}

/// An optional base URL: absent, null or empty selects `default`.
pub(in crate::toolkits) fn optional_https_base(
    object: &Map<String, Value>,
    key: &str,
    default: &str,
) -> Result<Url, ZephyrRestConfigError> {
    match object.get(key) {
        None | Some(Value::Null) => parse_https_base(default),
        Some(Value::String(value)) if value.trim().is_empty() => parse_https_base(default),
        Some(Value::String(value)) => parse_https_base(value),
        Some(_) => Err(invalid_configuration()),
    }
}

/// A required base URL.
pub(in crate::toolkits) fn required_https_base(
    object: &Map<String, Value>,
    key: &str,
) -> Result<Url, ZephyrRestConfigError> {
    object
        .get(key)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)
        .and_then(parse_https_base)
}

/// A required bearer token: printable ASCII only, so it is always a valid
/// header value and can never smuggle a second header.
pub(in crate::toolkits) fn bearer_token(
    object: &Map<String, Value>,
    key: &str,
) -> Result<Zeroizing<String>, ZephyrRestConfigError> {
    let value = object
        .get(key)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    let value = value.trim();
    if value.len() > MAX_TOKEN_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty() || !value.bytes().all(|byte| (0x21..=0x7e).contains(&byte)) {
        return Err(invalid_configuration());
    }
    Ok(Zeroizing::new(value.to_owned()))
}

/// The selected tool names, deduplicated in order; absent or null means all.
pub(in crate::toolkits) fn selected_tools(
    settings: &Map<String, Value>,
) -> Result<Vec<Box<str>>, ZephyrRestConfigError> {
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
        if value.len() > MAX_TOOL_NAME_BYTES {
            return Err(resource_exhausted());
        }
        if value.is_empty() || value.bytes().any(|byte| byte.is_ascii_control()) {
            return Err(invalid_configuration());
        }
        if seen.insert(value) {
            selected.push(value.into());
        }
    }
    Ok(selected)
}

pub(in crate::toolkits) const fn invalid_configuration() -> ZephyrRestConfigError {
    ZephyrRestConfigError {
        code: ZephyrRestConfigErrorCode::InvalidConfiguration,
    }
}

pub(in crate::toolkits) const fn resource_exhausted() -> ZephyrRestConfigError {
    ZephyrRestConfigError {
        code: ZephyrRestConfigErrorCode::ResourceExhausted,
    }
}
