use std::collections::HashSet;
use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_URL_BYTES: usize = 2 * 1_024;
const MAX_ORGANIZATION_BYTES: usize = 1_024;
const MAX_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_PROJECT_ID_BYTES: usize = 128;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum CarrierConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that never carries the token or origin.
pub(crate) struct CarrierConfigError {
    code: CarrierConfigErrorCode,
}

impl CarrierConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> CarrierConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for CarrierConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("CarrierConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for CarrierConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            CarrierConfigErrorCode::InvalidConfiguration => {
                "the Carrier toolkit configuration is invalid"
            }
            CarrierConfigErrorCode::ResourceExhausted => {
                "the Carrier toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for CarrierConfigError {}

/// Invocation-scoped Carrier authority materialized from one accepted claim.
///
/// The SDK reads `carrier_configuration.{url,organization,private_token}`
/// and the toolkit-level `project_id`, and refuses an empty project. This
/// type is non-`Clone` and non-`Debug`: it owns exactly one token, origin and
/// project, and the token never leaves it except as a sensitive header.
pub(crate) struct CarrierToolkitConfig {
    base: Url,
    organization: Box<str>,
    token: Zeroizing<String>,
    project_id: Box<str>,
    selected_tools: Vec<Box<str>>,
}

impl CarrierToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, CarrierConfigError> {
        let configuration = settings
            .get("carrier_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let base = parse_base(required_text(configuration, "url", MAX_URL_BYTES)?)?;
        let organization =
            required_text(configuration, "organization", MAX_ORGANIZATION_BYTES)?.into();
        let token = Zeroizing::new(
            required_text(configuration, "private_token", MAX_TOKEN_BYTES)?.to_owned(),
        );
        let project_id = project_id(settings)?;
        Ok(Self {
            base,
            organization,
            token,
            project_id,
            selected_tools: selected_tools(settings)?,
        })
    }

    /// The configured origin and optional path prefix, always ending in `/`
    /// so a relative endpoint joins beneath it.
    pub(super) fn base(&self) -> &Url {
        &self.base
    }

    /// The SDK's `url.rstrip('/')`, used in the links its results print.
    pub(super) fn display_url(&self) -> &str {
        self.base.as_str().trim_end_matches('/')
    }

    pub(super) fn organization(&self) -> &str {
        &self.organization
    }

    pub(super) fn token(&self) -> &str {
        &self.token
    }

    pub(super) fn project_id(&self) -> &str {
        &self.project_id
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn parse_base(value: &str) -> Result<Url, CarrierConfigError> {
    let mut base = Url::parse(value.trim()).map_err(|_| invalid_configuration())?;
    if base.scheme() != "https"
        || base.host_str().is_none()
        || !base.username().is_empty()
        || base.password().is_some()
        || base.query().is_some()
        || base.fragment().is_some()
    {
        return Err(invalid_configuration());
    }
    let path = base.path().trim_end_matches('/').to_owned();
    base.set_path(&format!("{path}/"));
    Ok(base)
}

/// The SDK declares `project_id` a string; a numeric value frozen by an
/// older form is accepted as its decimal text. It becomes a URL path
/// segment, so only a bounded URL-safe token is admitted.
fn project_id(settings: &Map<String, Value>) -> Result<Box<str>, CarrierConfigError> {
    let value = match settings.get("project_id") {
        Some(Value::String(value)) => value.trim().to_owned(),
        Some(Value::Number(value)) if value.is_u64() => value.to_string(),
        _ => return Err(invalid_configuration()),
    };
    if value.len() > MAX_PROJECT_ID_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty()
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
    {
        return Err(invalid_configuration());
    }
    Ok(value.into())
}

fn required_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, CarrierConfigError> {
    let value = object
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    validate_text(value, limit)?;
    Ok(value)
}

fn validate_text(value: &str, limit: usize) -> Result<(), CarrierConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, CarrierConfigError> {
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
        validate_text(value, MAX_TOOL_NAME_BYTES)?;
        if seen.insert(value) {
            selected.push(value.into());
        }
    }
    Ok(selected)
}

const fn invalid_configuration() -> CarrierConfigError {
    CarrierConfigError {
        code: CarrierConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> CarrierConfigError {
    CarrierConfigError {
        code: CarrierConfigErrorCode::ResourceExhausted,
    }
}
