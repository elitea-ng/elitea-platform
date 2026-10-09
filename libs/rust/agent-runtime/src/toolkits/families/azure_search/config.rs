use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_ENDPOINT_BYTES: usize = 2_048;
const MAX_API_KEY_BYTES: usize = 8 * 1_024;
const MAX_INDEX_NAME_BYTES: usize = 128;
const MIN_INDEX_NAME_BYTES: usize = 2;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AzureSearchConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable, data-free failure for claim-materialized Azure Search settings.
pub(crate) struct AzureSearchConfigError {
    code: AzureSearchConfigErrorCode,
}

impl AzureSearchConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> AzureSearchConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for AzureSearchConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AzureSearchConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AzureSearchConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AzureSearchConfigErrorCode::InvalidConfiguration => {
                "the Azure Search toolkit configuration is invalid"
            }
            AzureSearchConfigErrorCode::ResourceExhausted => {
                "the Azure Search toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for AzureSearchConfigError {}

/// Invocation-scoped Azure Search authority.
///
/// Main resolves the nested configuration into the admitted command. The key
/// remains non-cloneable and non-debuggable; one toolset owns one HTTP pool.
pub(crate) struct AzureSearchToolkitConfig {
    endpoint: Url,
    api_key: Zeroizing<String>,
    index_name: Box<str>,
    selected_tools: Vec<Box<str>>,
}

impl AzureSearchToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, AzureSearchConfigError> {
        let configuration = settings
            .get("azure_search_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let endpoint = configuration
            .get("endpoint")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)
            .and_then(parse_endpoint)?;
        let api_key = configuration
            .get("api_key")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        validate_secret(api_key)?;
        let index_name = settings
            .get("index_name")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        validate_index_name(index_name)?;
        Ok(Self {
            endpoint,
            api_key: Zeroizing::new(api_key.to_owned()),
            index_name: index_name.into(),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) const fn endpoint(&self) -> &Url {
        &self.endpoint
    }

    pub(super) fn api_key(&self) -> &str {
        &self.api_key
    }

    pub(super) fn index_name(&self) -> &str {
        &self.index_name
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn parse_endpoint(value: &str) -> Result<Url, AzureSearchConfigError> {
    if value.len() > MAX_ENDPOINT_BYTES {
        return Err(resource_exhausted());
    }
    if value.chars().any(char::is_control) || value.contains(['%', '\\']) {
        return Err(invalid_configuration());
    }
    let mut endpoint = Url::parse(value).map_err(|_| invalid_configuration())?;
    if endpoint.scheme() != "https"
        || endpoint.host_str().is_none()
        || !endpoint.username().is_empty()
        || endpoint.password().is_some()
        || endpoint.query().is_some()
        || endpoint.fragment().is_some()
        || !matches!(endpoint.path(), "" | "/")
    {
        return Err(invalid_configuration());
    }
    endpoint.set_path("");
    Ok(endpoint)
}

fn validate_secret(value: &str) -> Result<(), AzureSearchConfigError> {
    if value.len() > MAX_API_KEY_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty() || !value.bytes().all(|byte| (0x21..=0x7e).contains(&byte)) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn validate_index_name(value: &str) -> Result<(), AzureSearchConfigError> {
    if value.len() > MAX_INDEX_NAME_BYTES {
        return Err(resource_exhausted());
    }
    let bytes = value.as_bytes();
    let valid = value.len() >= MIN_INDEX_NAME_BYTES
        && bytes.first().is_some_and(u8::is_ascii_alphanumeric)
        && bytes.last().is_some_and(u8::is_ascii_alphanumeric)
        && bytes
            .iter()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || *byte == b'-')
        && !value.contains("--");
    if !valid {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, AzureSearchConfigError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(Vec::new());
    };
    let values = value.as_array().ok_or_else(invalid_configuration)?;
    if values.len() > MAX_SELECTED_TOOLS {
        return Err(resource_exhausted());
    }
    let mut selected = Vec::with_capacity(values.len().min(2));
    for value in values {
        let name = value.as_str().ok_or_else(invalid_configuration)?;
        if name.is_empty()
            || name.len() > MAX_TOOL_NAME_BYTES
            || name.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n'))
        {
            return Err(if name.len() > MAX_TOOL_NAME_BYTES {
                resource_exhausted()
            } else {
                invalid_configuration()
            });
        }
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

const fn invalid_configuration() -> AzureSearchConfigError {
    AzureSearchConfigError {
        code: AzureSearchConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> AzureSearchConfigError {
    AzureSearchConfigError {
        code: AzureSearchConfigErrorCode::ResourceExhausted,
    }
}
