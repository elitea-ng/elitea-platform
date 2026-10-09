use std::collections::HashSet;
use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_SERVER_BYTES: usize = 2 * 1_024;
const MAX_SECRET_BYTES: usize = 16 * 1_024;
const MAX_IDENTITY_BYTES: usize = 1_024;
const MAX_CONTEXT_BYTES: usize = 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum RallyConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that never carries credentials or origins.
pub(crate) struct RallyConfigError {
    code: RallyConfigErrorCode,
}

impl RallyConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> RallyConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for RallyConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("RallyConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for RallyConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            RallyConfigErrorCode::InvalidConfiguration => {
                "the Rally toolkit configuration is invalid"
            }
            RallyConfigErrorCode::ResourceExhausted => {
                "the Rally toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for RallyConfigError {}

enum RallyCredential {
    ApiKey(Zeroizing<String>),
    Basic {
        username: Box<str>,
        password: Zeroizing<String>,
    },
}

/// Invocation-scoped Rally authority materialized from one accepted claim.
///
/// The SDK stores its client in a class attribute, which lets a later toolkit
/// instance replace an earlier instance's endpoint and credentials. This type
/// is intentionally non-`Clone` and non-`Debug`; each materialized toolset owns
/// exactly one credential and origin.
pub(crate) struct RallyToolkitConfig {
    origin: Url,
    credential: RallyCredential,
    workspace: Option<Box<str>>,
    project: Option<Box<str>>,
    selected_tools: Vec<Box<str>>,
}

impl RallyToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, RallyConfigError> {
        let configuration = settings
            .get("rally_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let server = required_text(configuration, "server", MAX_SERVER_BYTES)?;
        let origin = parse_origin(server)?;

        let api_key = optional_credential(configuration, "api_key", MAX_SECRET_BYTES)?;
        let username = optional_credential(configuration, "username", MAX_IDENTITY_BYTES)?;
        let password = optional_credential(configuration, "password", MAX_SECRET_BYTES)?;
        let credential = if let Some(api_key) = api_key {
            RallyCredential::ApiKey(Zeroizing::new(api_key.to_owned()))
        } else {
            let username = username.ok_or_else(invalid_configuration)?;
            let password = password.ok_or_else(invalid_configuration)?;
            RallyCredential::Basic {
                username: username.into(),
                password: Zeroizing::new(password.to_owned()),
            }
        };

        let workspace = optional_text(settings, "workspace", MAX_CONTEXT_BYTES)?.map(Into::into);
        let project = optional_text(settings, "project", MAX_CONTEXT_BYTES)?.map(Into::into);
        validate_context_name(workspace.as_deref())?;
        validate_context_name(project.as_deref())?;

        Ok(Self {
            origin,
            credential,
            workspace,
            project,
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) fn origin(&self) -> &Url {
        &self.origin
    }

    pub(super) fn api_key(&self) -> Option<&str> {
        match &self.credential {
            RallyCredential::ApiKey(value) => Some(value),
            RallyCredential::Basic { .. } => None,
        }
    }

    pub(super) fn basic(&self) -> Option<(&str, &str)> {
        match &self.credential {
            RallyCredential::ApiKey(_) => None,
            RallyCredential::Basic { username, password } => Some((username, password)),
        }
    }

    pub(super) fn workspace(&self) -> Option<&str> {
        self.workspace.as_deref()
    }

    pub(super) fn project(&self) -> Option<&str> {
        self.project.as_deref()
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_origin(&self) -> &Url {
        self.origin()
    }
}

fn parse_origin(value: &str) -> Result<Url, RallyConfigError> {
    let normalized = if value.contains("://") {
        value.to_owned()
    } else {
        format!("https://{value}")
    };
    let mut origin = Url::parse(&normalized).map_err(|_| invalid_configuration())?;
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
    origin.set_path("/");
    Ok(origin)
}

fn required_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, RallyConfigError> {
    optional_text(object, name, limit)?.ok_or_else(invalid_configuration)
}

fn optional_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<Option<&'a str>, RallyConfigError> {
    match object.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => {
            validate_text(value, limit)?;
            Ok(Some(value))
        }
        Some(_) => Err(invalid_configuration()),
    }
}

fn optional_credential<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<Option<&'a str>, RallyConfigError> {
    match object.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if value.is_empty() => Ok(None),
        Some(Value::String(value)) => {
            validate_text(value, limit)?;
            Ok(Some(value))
        }
        Some(_) => Err(invalid_configuration()),
    }
}

fn validate_text(value: &str, limit: usize) -> Result<(), RallyConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn validate_context_name(value: Option<&str>) -> Result<(), RallyConfigError> {
    if value.is_some_and(|value| value.contains(['"', '\\']) || value.chars().any(char::is_control))
    {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, RallyConfigError> {
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

const fn invalid_configuration() -> RallyConfigError {
    RallyConfigError {
        code: RallyConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> RallyConfigError {
    RallyConfigError {
        code: RallyConfigErrorCode::ResourceExhausted,
    }
}
