use std::collections::HashSet;
use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const DEFAULT_API_VERSION: &str = "v59.0";
const MAX_ORIGIN_BYTES: usize = 2_048;
const MAX_CLIENT_ID_BYTES: usize = 4 * 1_024;
const MAX_CLIENT_SECRET_BYTES: usize = 16 * 1_024;
const MAX_API_VERSION_BYTES: usize = 32;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SalesforceConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable, data-free failure for one materialized Salesforce configuration.
pub(crate) struct SalesforceConfigError {
    code: SalesforceConfigErrorCode,
}

impl SalesforceConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> SalesforceConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for SalesforceConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SalesforceConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SalesforceConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SalesforceConfigErrorCode::InvalidConfiguration => {
                "the Salesforce toolkit configuration is invalid"
            }
            SalesforceConfigErrorCode::ResourceExhausted => {
                "the Salesforce toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for SalesforceConfigError {}

/// Invocation-scoped authority for one Salesforce toolset.
///
/// Main resolves the nested configuration only for the admitted command. The
/// credential is non-cloneable and non-debuggable; the owning client creates
/// the bearer token lazily at the first real tool invocation.
pub(crate) struct SalesforceToolkitConfig {
    origin: Url,
    client_id: Zeroizing<String>,
    client_secret: Zeroizing<String>,
    api_version: Box<str>,
    selected_tools: Vec<Box<str>>,
}

impl SalesforceToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, SalesforceConfigError> {
        let configuration = settings
            .get("salesforce_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let origin = configuration
            .get("base_url")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)
            .and_then(parse_origin)?;
        let client_id = configuration
            .get("client_id")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        let client_secret = configuration
            .get("client_secret")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        validate_secret(client_id, MAX_CLIENT_ID_BYTES)?;
        validate_secret(client_secret, MAX_CLIENT_SECRET_BYTES)?;

        let api_version = match settings.get("api_version") {
            None | Some(Value::Null) => DEFAULT_API_VERSION,
            Some(Value::String(value)) => value,
            Some(_) => return Err(invalid_configuration()),
        };
        validate_api_version(api_version)?;

        Ok(Self {
            origin,
            client_id: Zeroizing::new(client_id.to_owned()),
            client_secret: Zeroizing::new(client_secret.to_owned()),
            api_version: api_version.into(),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) const fn origin(&self) -> &Url {
        &self.origin
    }

    pub(super) fn client_id(&self) -> &str {
        &self.client_id
    }

    pub(super) fn client_secret(&self) -> &str {
        &self.client_secret
    }

    pub(super) fn api_version(&self) -> &str {
        &self.api_version
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_origin(&self) -> &Url {
        self.origin()
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_api_version(&self) -> &str {
        self.api_version()
    }
}

fn parse_origin(value: &str) -> Result<Url, SalesforceConfigError> {
    if value.len() > MAX_ORIGIN_BYTES {
        return Err(resource_exhausted());
    }
    if value.chars().any(char::is_control) || value.contains(['%', '\\']) {
        return Err(invalid_configuration());
    }
    let mut origin = Url::parse(value).map_err(|_| invalid_configuration())?;
    if origin.scheme() != "https"
        || origin.host_str().is_none()
        || !origin.username().is_empty()
        || origin.password().is_some()
        || origin.query().is_some()
        || origin.fragment().is_some()
        || !matches!(origin.path(), "" | "/")
    {
        return Err(invalid_configuration());
    }
    origin.set_path("");
    Ok(origin)
}

fn validate_secret(value: &str, limit: usize) -> Result<(), SalesforceConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn validate_api_version(value: &str) -> Result<(), SalesforceConfigError> {
    if value.len() > MAX_API_VERSION_BYTES {
        return Err(resource_exhausted());
    }
    let Some(version) = value.strip_prefix('v') else {
        return Err(invalid_configuration());
    };
    let mut parts = version.split('.');
    let major = parts.next().unwrap_or_default();
    let minor = parts.next().unwrap_or_default();
    if parts.next().is_some()
        || major.is_empty()
        || minor.is_empty()
        || !major.bytes().all(|byte| byte.is_ascii_digit())
        || !minor.bytes().all(|byte| byte.is_ascii_digit())
    {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, SalesforceConfigError> {
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
        if value.is_empty()
            || value.len() > MAX_TOOL_NAME_BYTES
            || value.chars().any(char::is_control)
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

const fn invalid_configuration() -> SalesforceConfigError {
    SalesforceConfigError {
        code: SalesforceConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> SalesforceConfigError {
    SalesforceConfigError {
        code: SalesforceConfigErrorCode::ResourceExhausted,
    }
}
