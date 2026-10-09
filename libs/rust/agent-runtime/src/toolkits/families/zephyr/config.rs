use std::collections::HashSet;
use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_BASE_URL_BYTES: usize = 2_048;
const MAX_USERNAME_BYTES: usize = 320;
const MAX_PASSWORD_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 16;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ZephyrConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure without authority or credential data.
pub(crate) struct ZephyrConfigError {
    code: ZephyrConfigErrorCode,
}

impl ZephyrConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> ZephyrConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for ZephyrConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ZephyrConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ZephyrConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ZephyrConfigErrorCode::InvalidConfiguration => {
                "the legacy Zephyr toolkit configuration is invalid"
            }
            ZephyrConfigErrorCode::ResourceExhausted => {
                "the legacy Zephyr toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for ZephyrConfigError {}

/// Invocation-scoped authority materialized from one accepted toolkit claim.
///
/// Main's frozen schema stores these fields inline for toolkit type `zephyr`;
/// it does not reference the separately registered and incompatible Zephyr
/// Scale configuration model. The password remains zeroizing and no process
/// environment fallback is permitted.
pub(crate) struct ZephyrToolkitConfig {
    base_url: Url,
    username: Box<str>,
    password: Zeroizing<String>,
    selected_tools: Vec<Box<str>>,
}

impl ZephyrToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, ZephyrConfigError> {
        let base_url = settings
            .get("base_url")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)
            .and_then(parse_base_url)?;
        let username = required_text(settings, "username", MAX_USERNAME_BYTES)?;
        let password = required_text(settings, "password", MAX_PASSWORD_BYTES)?;
        if username.contains(':') {
            return Err(invalid_configuration());
        }
        Ok(Self {
            base_url,
            username: username.into(),
            password: Zeroizing::new(password.to_owned()),
            selected_tools: selected_tools(settings.get("selected_tools"))?,
        })
    }

    pub(super) const fn base_url(&self) -> &Url {
        &self.base_url
    }

    pub(super) fn username(&self) -> &str {
        &self.username
    }

    pub(super) fn password(&self) -> &str {
        self.password.as_str()
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn parse_base_url(value: &str) -> Result<Url, ZephyrConfigError> {
    let value = value.trim();
    if value.is_empty() || value.len() > MAX_BASE_URL_BYTES {
        return Err(if value.len() > MAX_BASE_URL_BYTES {
            resource_exhausted()
        } else {
            invalid_configuration()
        });
    }
    if value.bytes().any(|byte| byte.is_ascii_control())
        || value.contains('%')
        || value.contains('\\')
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

fn required_text<'a>(
    settings: &'a Map<String, Value>,
    name: &str,
    max_bytes: usize,
) -> Result<&'a str, ZephyrConfigError> {
    let value = settings
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    if value.len() > max_bytes {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.bytes().any(|byte| byte.is_ascii_control()) {
        return Err(invalid_configuration());
    }
    Ok(value)
}

fn selected_tools(value: Option<&Value>) -> Result<Vec<Box<str>>, ZephyrConfigError> {
    let Some(value) = value else {
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
        if value.is_empty()
            || value.len() > MAX_TOOL_NAME_BYTES
            || value.bytes().any(|byte| byte.is_ascii_control())
        {
            return Err(if value.len() > MAX_TOOL_NAME_BYTES {
                resource_exhausted()
            } else {
                invalid_configuration()
            });
        }
        if seen.insert(value) {
            selected.push(value.into());
        }
    }
    Ok(selected)
}

const fn invalid_configuration() -> ZephyrConfigError {
    ZephyrConfigError {
        code: ZephyrConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> ZephyrConfigError {
    ZephyrConfigError {
        code: ZephyrConfigErrorCode::ResourceExhausted,
    }
}
