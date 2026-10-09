use std::collections::HashSet;
use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_URL_BYTES: usize = 2_048;
const MAX_API_KEY_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 16;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ElasticConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure without cluster or credential data.
pub(crate) struct ElasticConfigError {
    code: ElasticConfigErrorCode,
}

impl ElasticConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> ElasticConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for ElasticConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ElasticConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ElasticConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ElasticConfigErrorCode::InvalidConfiguration => {
                "the Elasticsearch toolkit configuration is invalid"
            }
            ElasticConfigErrorCode::ResourceExhausted => {
                "the Elasticsearch toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for ElasticConfigError {}

/// One invocation-scoped Elasticsearch authority and optional encoded API key.
pub(crate) struct ElasticToolkitConfig {
    base_url: Url,
    api_key: Option<Zeroizing<String>>,
    selected_tools: Vec<Box<str>>,
}

impl ElasticToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, ElasticConfigError> {
        let base_url = settings
            .get("url")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)
            .and_then(parse_base_url)?;
        let api_key = parse_api_key(settings.get("api_key"))?;
        Ok(Self {
            base_url,
            api_key,
            selected_tools: selected_tools(settings.get("selected_tools"))?,
        })
    }

    pub(super) const fn base_url(&self) -> &Url {
        &self.base_url
    }

    pub(super) fn api_key(&self) -> Option<&str> {
        self.api_key.as_deref().map(String::as_str)
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn parse_base_url(value: &str) -> Result<Url, ElasticConfigError> {
    if value.is_empty() || value.len() > MAX_URL_BYTES {
        return Err(if value.len() > MAX_URL_BYTES {
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
        || !matches!(url.path(), "" | "/")
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err(invalid_configuration());
    }
    url.set_path("");
    Ok(url)
}

fn parse_api_key(value: Option<&Value>) -> Result<Option<Zeroizing<String>>, ElasticConfigError> {
    let Some(value) = value else {
        return Ok(None);
    };
    if value.is_null() {
        return Ok(None);
    }
    let value = value.as_str().ok_or_else(invalid_configuration)?;
    if value.len() > MAX_API_KEY_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty()
        || !value.bytes().all(|byte| {
            byte.is_ascii_alphanumeric() || matches!(byte, b'+' | b'/' | b'_' | b'-' | b'=')
        })
    {
        return Err(invalid_configuration());
    }
    Ok(Some(Zeroizing::new(value.to_owned())))
}

fn selected_tools(value: Option<&Value>) -> Result<Vec<Box<str>>, ElasticConfigError> {
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

const fn invalid_configuration() -> ElasticConfigError {
    ElasticConfigError {
        code: ElasticConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> ElasticConfigError {
    ElasticConfigError {
        code: ElasticConfigErrorCode::ResourceExhausted,
    }
}
