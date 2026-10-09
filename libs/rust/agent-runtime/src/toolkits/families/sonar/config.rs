use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_URL_BYTES: usize = 2_048;
const MAX_TOKEN_BYTES: usize = 8 * 1_024;
const MAX_PROJECT_BYTES: usize = 512;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SonarConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable, data-free failure for claim-materialized Sonar settings.
pub(crate) struct SonarConfigError {
    code: SonarConfigErrorCode,
}

impl SonarConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> SonarConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for SonarConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SonarConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SonarConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SonarConfigErrorCode::InvalidConfiguration => {
                "the Sonar toolkit configuration is invalid"
            }
            SonarConfigErrorCode::ResourceExhausted => {
                "the Sonar toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for SonarConfigError {}

/// One invocation-scoped Sonar authority and selected-tool profile.
///
/// The token is intentionally neither cloneable nor debug-printable. The URL
/// and project are operational identifiers, but errors still omit both.
pub(crate) struct SonarToolkitConfig {
    base_url: Url,
    token: Zeroizing<String>,
    project: Box<str>,
    selected_tools: Vec<Box<str>>,
}

impl SonarToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, SonarConfigError> {
        let configuration = settings
            .get("sonar_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let base_url = configuration
            .get("url")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)
            .and_then(parse_base_url)?;
        let token = configuration
            .get("sonar_token")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        validate_secret(token)?;
        let project = settings
            .get("sonar_project_name")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        validate_text(project, MAX_PROJECT_BYTES)?;
        Ok(Self {
            base_url,
            token: Zeroizing::new(token.to_owned()),
            project: project.into(),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) const fn base_url(&self) -> &Url {
        &self.base_url
    }

    pub(super) fn token(&self) -> &str {
        &self.token
    }

    pub(super) fn project(&self) -> &str {
        &self.project
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn parse_base_url(value: &str) -> Result<Url, SonarConfigError> {
    if value.len() > MAX_URL_BYTES {
        return Err(resource_exhausted());
    }
    if value.chars().any(char::is_control)
        || value.contains(['%', '\\'])
        || has_dot_path_segment(value)
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
        || url.path().split('/').any(|segment| segment.contains('%'))
    {
        return Err(invalid_configuration());
    }
    let path = url.path().trim_end_matches('/').to_owned();
    url.set_path(&path);
    Ok(url)
}

fn has_dot_path_segment(value: &str) -> bool {
    value
        .split_once("://")
        .and_then(|(_, remainder)| remainder.split_once('/'))
        .is_some_and(|(_, path)| path.split('/').any(|segment| matches!(segment, "." | "..")))
}

fn validate_secret(value: &str) -> Result<(), SonarConfigError> {
    if value.len() > MAX_TOKEN_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty() || !value.bytes().all(|byte| (0x21..=0x7e).contains(&byte)) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn validate_text(value: &str, maximum: usize) -> Result<(), SonarConfigError> {
    if value.len() > maximum {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, SonarConfigError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(Vec::new());
    };
    let values = value.as_array().ok_or_else(invalid_configuration)?;
    if values.len() > MAX_SELECTED_TOOLS {
        return Err(resource_exhausted());
    }
    let mut selected = Vec::with_capacity(values.len().min(1));
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

const fn invalid_configuration() -> SonarConfigError {
    SonarConfigError {
        code: SonarConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> SonarConfigError {
    SonarConfigError {
        code: SonarConfigErrorCode::ResourceExhausted,
    }
}
