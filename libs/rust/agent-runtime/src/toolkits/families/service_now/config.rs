use std::collections::HashSet;
use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_ORIGIN_BYTES: usize = 2_048;
const MAX_USERNAME_BYTES: usize = 1_024;
const MAX_PASSWORD_BYTES: usize = 8 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;
const MAX_RESPONSE_FIELDS: usize = 128;
const MAX_RESPONSE_FIELD_BYTES: usize = 256;
const MAX_RESPONSE_FIELDS_BYTES: usize = 16 * 1_024;

const DEFAULT_RESPONSE_FIELDS: [&str; 10] = [
    "sys_id",
    "number",
    "state",
    "short_description",
    "description",
    "priority",
    "category",
    "urgency",
    "impact",
    "creation_date",
];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ServiceNowConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable, data-free failure for one materialized `ServiceNow` configuration.
pub(crate) struct ServiceNowConfigError {
    code: ServiceNowConfigErrorCode,
}

impl ServiceNowConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> ServiceNowConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for ServiceNowConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ServiceNowConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ServiceNowConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ServiceNowConfigErrorCode::InvalidConfiguration => {
                "the ServiceNow toolkit configuration is invalid"
            }
            ServiceNowConfigErrorCode::ResourceExhausted => {
                "the ServiceNow toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for ServiceNowConfigError {}

/// Invocation-scoped authority for one `ServiceNow` incident toolset.
///
/// Main resolves the nested configuration into the admitted command. The
/// credentials are deliberately non-cloneable and non-debuggable, and one
/// materialized toolkit owns one HTTP connection pool.
pub(crate) struct ServiceNowToolkitConfig {
    origin: Url,
    username: Zeroizing<String>,
    password: Zeroizing<String>,
    response_fields: Vec<Box<str>>,
    selected_tools: Vec<Box<str>>,
}

impl ServiceNowToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, ServiceNowConfigError> {
        let configuration = settings
            .get("servicenow_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let origin = configuration
            .get("base_url")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)
            .and_then(parse_origin)?;
        let username = configuration
            .get("username")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        let password = configuration
            .get("password")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        validate_credential(username, MAX_USERNAME_BYTES)?;
        validate_credential(password, MAX_PASSWORD_BYTES)?;

        Ok(Self {
            origin,
            username: Zeroizing::new(username.to_owned()),
            password: Zeroizing::new(password.to_owned()),
            response_fields: response_fields(settings)?,
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) const fn origin(&self) -> &Url {
        &self.origin
    }

    pub(super) fn username(&self) -> &str {
        &self.username
    }

    pub(super) fn password(&self) -> &str {
        &self.password
    }

    pub(super) fn response_fields(&self) -> &[Box<str>] {
        &self.response_fields
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_response_fields(&self) -> &[Box<str>] {
        self.response_fields()
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn parse_origin(value: &str) -> Result<Url, ServiceNowConfigError> {
    if value.len() > MAX_ORIGIN_BYTES {
        return Err(resource_exhausted());
    }
    if value.chars().any(char::is_control) || value.contains(['%', '\\']) {
        return Err(invalid_configuration());
    }

    let expanded;
    let value = if value.contains("://") {
        value
    } else {
        if value.is_empty()
            || value.len() > 63
            || !value
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
            || value.starts_with('-')
            || value.ends_with('-')
        {
            return Err(invalid_configuration());
        }
        expanded = format!("https://{value}.service-now.com");
        &expanded
    };

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

fn validate_credential(value: &str, limit: usize) -> Result<(), ServiceNowConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn response_fields(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, ServiceNowConfigError> {
    let Some(raw) = settings.get("response_fields") else {
        return Ok(default_response_fields());
    };
    if raw.is_null() {
        return Ok(default_response_fields());
    }
    let raw = raw.as_str().ok_or_else(invalid_configuration)?;
    if raw.len() > MAX_RESPONSE_FIELDS_BYTES {
        return Err(resource_exhausted());
    }
    let values = raw
        .split(',')
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .collect::<Vec<_>>();
    if values.is_empty() {
        return Ok(default_response_fields());
    }
    if values.len() > MAX_RESPONSE_FIELDS {
        return Err(resource_exhausted());
    }

    let mut seen = HashSet::with_capacity(values.len());
    let mut fields = Vec::with_capacity(values.len());
    let mut total = 0usize;
    for value in values {
        let value = value.to_ascii_lowercase();
        validate_field_name(&value)?;
        total = total
            .checked_add(value.len())
            .ok_or_else(resource_exhausted)?;
        if total > MAX_RESPONSE_FIELDS_BYTES {
            return Err(resource_exhausted());
        }
        if seen.insert(value.clone()) {
            fields.push(value.into_boxed_str());
        }
    }
    Ok(fields)
}

fn validate_field_name(value: &str) -> Result<(), ServiceNowConfigError> {
    if value.len() > MAX_RESPONSE_FIELD_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty()
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.'))
    {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, ServiceNowConfigError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(Vec::new());
    };
    let values = value.as_array().ok_or_else(invalid_configuration)?;
    if values.len() > MAX_SELECTED_TOOLS {
        return Err(resource_exhausted());
    }
    let mut selected = Vec::with_capacity(values.len().min(3));
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

fn default_response_fields() -> Vec<Box<str>> {
    DEFAULT_RESPONSE_FIELDS
        .into_iter()
        .map(Box::<str>::from)
        .collect()
}

const fn invalid_configuration() -> ServiceNowConfigError {
    ServiceNowConfigError {
        code: ServiceNowConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> ServiceNowConfigError {
    ServiceNowConfigError {
        code: ServiceNowConfigErrorCode::ResourceExhausted,
    }
}
