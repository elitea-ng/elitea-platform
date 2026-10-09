use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_URL_BYTES: usize = 2_048;
const MAX_API_KEY_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AhaConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that retains no origin or credential data.
pub(crate) struct AhaConfigError {
    code: AhaConfigErrorCode,
}

impl AhaConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> AhaConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for AhaConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AhaConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AhaConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AhaConfigErrorCode::InvalidConfiguration => "the Aha toolkit configuration is invalid",
            AhaConfigErrorCode::ResourceExhausted => {
                "the Aha toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for AhaConfigError {}

/// Invocation-scoped authority for one exact Aha origin.
pub(crate) struct AhaToolkitConfig {
    base_url: Url,
    api_key: Zeroizing<String>,
    selected_tools: Vec<Box<str>>,
}

impl AhaToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, AhaConfigError> {
        let configuration = settings
            .get("aha_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let base_url = required_text(configuration, "base_url", MAX_URL_BYTES)?;
        let api_key = required_text(configuration, "api_key", MAX_API_KEY_BYTES)?;
        if !api_key.bytes().all(|byte| (0x21..=0x7e).contains(&byte)) {
            return Err(invalid_configuration());
        }
        Ok(Self {
            base_url: parse_base_url(base_url)?,
            api_key: Zeroizing::new(api_key.to_owned()),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) const fn base_url(&self) -> &Url {
        &self.base_url
    }

    pub(super) fn api_key(&self) -> &str {
        &self.api_key
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn parse_base_url(value: &str) -> Result<Url, AhaConfigError> {
    if value.contains(['%', '\\']) {
        return Err(invalid_configuration());
    }
    let mut url = Url::parse(value).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || !matches!(url.path(), "" | "/")
    {
        return Err(invalid_configuration());
    }
    url.set_path("");
    Ok(url)
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, AhaConfigError> {
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
    let mut selected = Vec::with_capacity(values.len().min(33));
    for value in values {
        let name = value.as_str().ok_or_else(invalid_configuration)?;
        validate_text(name, MAX_TOOL_NAME_BYTES)?;
        if !selected
            .iter()
            .map(AsRef::as_ref)
            .any(|existing: &str| existing == name)
        {
            selected.push(name.into());
        }
    }
    Ok(selected)
}

fn required_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, AhaConfigError> {
    let value = object
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    validate_text(value, limit)?;
    Ok(value)
}

fn validate_text(value: &str, limit: usize) -> Result<(), AhaConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_configuration());
    }
    Ok(())
}

const fn invalid_configuration() -> AhaConfigError {
    AhaConfigError {
        code: AhaConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> AhaConfigError {
    AhaConfigError {
        code: AhaConfigErrorCode::ResourceExhausted,
    }
}
