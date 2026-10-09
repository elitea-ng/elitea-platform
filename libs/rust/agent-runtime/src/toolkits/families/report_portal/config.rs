use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_ENDPOINT_BYTES: usize = 2_048;
const MAX_PROJECT_BYTES: usize = 1_024;
const MAX_API_KEY_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ReportPortalConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that retains no `ReportPortal` authority data.
pub(crate) struct ReportPortalConfigError {
    code: ReportPortalConfigErrorCode,
}

impl ReportPortalConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> ReportPortalConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for ReportPortalConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ReportPortalConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ReportPortalConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ReportPortalConfigErrorCode::InvalidConfiguration => {
                "the ReportPortal toolkit configuration is invalid"
            }
            ReportPortalConfigErrorCode::ResourceExhausted => {
                "the ReportPortal toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for ReportPortalConfigError {}

/// Invocation-scoped authority materialized from one accepted claim.
///
/// The endpoint is an exact HTTPS origin. The API key is non-cloneable,
/// non-debuggable, and zeroized when the owning toolset is dropped.
pub(crate) struct ReportPortalToolkitConfig {
    endpoint: Url,
    project: Box<str>,
    api_key: Zeroizing<String>,
    selected_tools: Vec<Box<str>>,
}

impl ReportPortalToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, ReportPortalConfigError> {
        let configuration = settings
            .get("report_portal_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let endpoint = required_text(configuration, "endpoint", MAX_ENDPOINT_BYTES)
            .and_then(parse_endpoint)?;
        let project = required_text(configuration, "project", MAX_PROJECT_BYTES)?;
        let api_key = required_text(configuration, "api_key", MAX_API_KEY_BYTES)?;
        if !api_key.bytes().all(|byte| (0x21..=0x7e).contains(&byte)) {
            return Err(invalid_configuration());
        }

        Ok(Self {
            endpoint,
            project: project.into(),
            api_key: Zeroizing::new(api_key.to_owned()),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) const fn endpoint(&self) -> &Url {
        &self.endpoint
    }

    pub(super) fn project(&self) -> &str {
        &self.project
    }

    pub(super) fn api_key(&self) -> &str {
        &self.api_key
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn required_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, ReportPortalConfigError> {
    let value = object
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    validate_text(value, limit)?;
    Ok(value)
}

fn validate_text(value: &str, limit: usize) -> Result<(), ReportPortalConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn parse_endpoint(value: &str) -> Result<Url, ReportPortalConfigError> {
    if value.contains(['%', '\\']) {
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

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, ReportPortalConfigError> {
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
    let mut selected = Vec::with_capacity(values.len().min(9));
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

const fn invalid_configuration() -> ReportPortalConfigError {
    ReportPortalConfigError {
        code: ReportPortalConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> ReportPortalConfigError {
    ReportPortalConfigError {
        code: ReportPortalConfigErrorCode::ResourceExhausted,
    }
}
