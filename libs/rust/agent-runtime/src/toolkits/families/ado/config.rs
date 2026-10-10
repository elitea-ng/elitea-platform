use std::collections::HashSet;
use std::fmt;

use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_ORGANIZATION_URL_BYTES: usize = 2 * 1_024;
const MAX_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_IDENTIFIER_BYTES: usize = 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;
const DEFAULT_LIMIT: i64 = 5;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AdoConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that never carries the PAT or the organization.
pub(crate) struct AdoConfigError {
    code: AdoConfigErrorCode,
}

impl AdoConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> AdoConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for AdoConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AdoConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AdoConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AdoConfigErrorCode::InvalidConfiguration => {
                "the Azure DevOps toolkit configuration is invalid"
            }
            AdoConfigErrorCode::ResourceExhausted => {
                "the Azure DevOps toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for AdoConfigError {}

/// The one Azure DevOps authority a toolset owns: the organization (or
/// collection) URL, its project, and the redeemed personal access token.
///
/// The SDK sends the PAT as `BasicAuthentication('', token)` from a
/// class-level client; here every materialized toolset owns exactly one
/// non-cloneable, non-debuggable connection.
pub(crate) struct AdoConnection {
    organization_url: Url,
    organization_text: Box<str>,
    project: Box<str>,
    token: Zeroizing<String>,
}

impl AdoConnection {
    /// The organization URL as the SDK prints it in result links, without a
    /// trailing slash.
    pub(crate) fn organization_text(&self) -> &str {
        &self.organization_text
    }

    pub(crate) fn organization_url(&self) -> &Url {
        &self.organization_url
    }

    pub(crate) fn project(&self) -> &str {
        &self.project
    }

    pub(super) fn token(&self) -> &str {
        &self.token
    }
}

/// Settings every ADO type shares (`ado/__init__.py::get_tools` and each
/// type's `get_toolkit`), plus the per-type optional fields the four types
/// read from the same settings object.
pub(crate) struct AdoToolkitConfig {
    connection: AdoConnection,
    limit: i64,
    selected_tools: Vec<Box<str>>,
    repository_id: Option<Box<str>>,
    base_branch: Option<Box<str>>,
    active_branch: Option<Box<str>>,
    default_wiki_identifier: Option<Box<str>>,
}

impl AdoToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, AdoConfigError> {
        let configuration = settings
            .get("ado_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let organization = required_text(
            configuration,
            "organization_url",
            MAX_ORGANIZATION_URL_BYTES,
        )?;
        let (organization_url, organization_text) = parse_organization(organization)?;
        let token = required_text(configuration, "token", MAX_TOKEN_BYTES)?;
        if token.trim().is_empty() {
            return Err(invalid_configuration());
        }
        let project = required_text(settings, "project", MAX_IDENTIFIER_BYTES)?;
        validate_identifier(project)?;
        let limit = match settings.get("limit") {
            None | Some(Value::Null) => DEFAULT_LIMIT,
            Some(value) => value
                .as_i64()
                .filter(|limit| *limit > 0)
                .ok_or_else(invalid_configuration)?,
        };
        Ok(Self {
            connection: AdoConnection {
                organization_url,
                organization_text: organization_text.into(),
                project: project.into(),
                token: Zeroizing::new(token.to_owned()),
            },
            limit,
            selected_tools: selected_tools(settings)?,
            repository_id: optional_identifier(settings, "repository_id")?,
            base_branch: optional_identifier(settings, "base_branch")?,
            active_branch: optional_identifier(settings, "active_branch")?,
            default_wiki_identifier: optional_identifier(settings, "default_wiki_identifier")?,
        })
    }

    pub(crate) fn connection(&self) -> &AdoConnection {
        &self.connection
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }

    pub(crate) const fn limit(&self) -> i64 {
        self.limit
    }

    pub(crate) fn repository_id(&self) -> Option<&str> {
        self.repository_id.as_deref()
    }

    pub(crate) fn base_branch(&self) -> Option<&str> {
        self.base_branch.as_deref()
    }

    pub(crate) fn active_branch(&self) -> Option<&str> {
        self.active_branch.as_deref()
    }

    pub(crate) fn default_wiki_identifier(&self) -> Option<&str> {
        self.default_wiki_identifier.as_deref()
    }

    /// Split the configuration into the connection a client owns and the
    /// remaining per-family settings.
    pub(crate) fn into_parts(self) -> (AdoConnection, AdoFamilySettings) {
        (
            self.connection,
            AdoFamilySettings {
                limit: self.limit,
                repository_id: self.repository_id,
                base_branch: self.base_branch,
                active_branch: self.active_branch,
                default_wiki_identifier: self.default_wiki_identifier,
            },
        )
    }
}

/// The non-credential settings left once the connection moved into a client.
pub(crate) struct AdoFamilySettings {
    pub(crate) limit: i64,
    pub(crate) repository_id: Option<Box<str>>,
    pub(crate) base_branch: Option<Box<str>>,
    pub(crate) active_branch: Option<Box<str>>,
    pub(crate) default_wiki_identifier: Option<Box<str>>,
}

/// The SDK hands `organization_url` straight to `Connection(base_url=...)`, so
/// both `https://dev.azure.com/<org>` and an Azure DevOps Server collection
/// such as `https://ado.example.com/tfs/DefaultCollection` work. Only HTTPS
/// is admitted here: the PAT travels as Basic credentials.
fn parse_organization(value: &str) -> Result<(Url, String), AdoConfigError> {
    let trimmed = value.trim().trim_end_matches('/');
    let url = Url::parse(trimmed).map_err(|_| invalid_configuration())?;
    if url.scheme() != "https"
        || url.host_str().is_none_or(str::is_empty)
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || url.cannot_be_a_base()
    {
        return Err(invalid_configuration());
    }
    let text = url.as_str().trim_end_matches('/').to_owned();
    Ok((url, text))
}

fn required_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, AdoConfigError> {
    let value = object
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    validate_text(value, limit)?;
    Ok(value)
}

fn optional_identifier(
    object: &Map<String, Value>,
    name: &str,
) -> Result<Option<Box<str>>, AdoConfigError> {
    match object.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if value.is_empty() => Ok(None),
        Some(Value::String(value)) => {
            validate_text(value, MAX_IDENTIFIER_BYTES)?;
            validate_identifier(value)?;
            Ok(Some(value.as_str().into()))
        }
        Some(_) => Err(invalid_configuration()),
    }
}

fn validate_text(value: &str, limit: usize) -> Result<(), AdoConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn validate_identifier(value: &str) -> Result<(), AdoConfigError> {
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, AdoConfigError> {
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

const fn invalid_configuration() -> AdoConfigError {
    AdoConfigError {
        code: AdoConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> AdoConfigError {
    AdoConfigError {
        code: AdoConfigErrorCode::ResourceExhausted,
    }
}
