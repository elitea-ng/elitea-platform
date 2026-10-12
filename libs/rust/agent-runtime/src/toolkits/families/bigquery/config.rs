use std::collections::HashSet;
use std::fmt;

use serde_json::{Map, Value};

use crate::toolkits::families::gcp::config::{GcpConfigErrorCode, GcpToolkitConfig};

const MAX_SERVICE_ACCOUNT_JSON_BYTES: usize = 128 * 1_024;
const MAX_PROJECT_BYTES: usize = 128;
const MAX_LOCATION_BYTES: usize = 64;
const MAX_DATASET_BYTES: usize = 1_024;
const MAX_TABLE_BYTES: usize = 1_024;
const MAX_SELECTED_TOOLS: usize = 64;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum BigQueryConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure without service-account identity or key data.
pub(crate) struct BigQueryConfigError {
    code: BigQueryConfigErrorCode,
}

impl BigQueryConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> BigQueryConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for BigQueryConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("BigQueryConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for BigQueryConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            BigQueryConfigErrorCode::InvalidConfiguration => {
                "the BigQuery toolkit configuration is invalid"
            }
            BigQueryConfigErrorCode::ResourceExhausted => {
                "the BigQuery toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for BigQueryConfigError {}

/// Claim-scoped `BigQuery` authority: one service account and the optional
/// default project, location, dataset and table of `bigquery_configuration`.
///
/// Main seals `api_key` (the complete service-account JSON). The signing key
/// itself is held by the `gcp` family's non-cloneable, non-debuggable
/// [`GcpToolkitConfig`]; this type adds only the non-secret table defaults.
pub(crate) struct BigQueryToolkitConfig {
    credentials: GcpToolkitConfig,
    /// `bigquery_configuration.project`, the SDK's `Client(project=...)`.
    project: Option<Box<str>>,
    /// The service account's own `project_id`: the project the SDK's client
    /// falls back to for jobs when `project` is unset.
    credential_project: Option<Box<str>>,
    location: Option<Box<str>>,
    dataset: Option<Box<str>>,
    table: Option<Box<str>>,
    selected_tools: Vec<Box<str>>,
}

impl BigQueryToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, BigQueryConfigError> {
        let configuration = settings
            .get("bigquery_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        // The SDK's `validate_auth`: "You must provide a BigQuery API key."
        let api_key = match configuration.get("api_key") {
            Some(Value::String(value)) if !value.is_empty() => value,
            _ => return Err(invalid_configuration()),
        };
        if api_key.len() > MAX_SERVICE_ACCOUNT_JSON_BYTES {
            return Err(resource_exhausted());
        }
        let credential_project = credential_project(api_key)?;
        let mut credential_settings = Map::new();
        credential_settings.insert("api_key".to_owned(), Value::String(api_key.clone()));
        let credentials =
            GcpToolkitConfig::parse(&credential_settings).map_err(|error| match error.code() {
                GcpConfigErrorCode::InvalidConfiguration => invalid_configuration(),
                GcpConfigErrorCode::ResourceExhausted => resource_exhausted(),
            })?;
        let project = optional_text(configuration, "project", MAX_PROJECT_BYTES)?;
        if project.is_some_and(|value| !valid_project(value)) {
            return Err(invalid_configuration());
        }
        let location = optional_text(configuration, "location", MAX_LOCATION_BYTES)?;
        if location.is_some_and(|value| !valid_location(value)) {
            return Err(invalid_configuration());
        }
        let dataset = optional_text(configuration, "dataset", MAX_DATASET_BYTES)?;
        if dataset.is_some_and(|value| !valid_dataset(value)) {
            return Err(invalid_configuration());
        }
        let table = optional_text(configuration, "table", MAX_TABLE_BYTES)?;
        if table.is_some_and(|value| !valid_table(value)) {
            return Err(invalid_configuration());
        }
        Ok(Self {
            credentials,
            project: project.map(Into::into),
            credential_project,
            location: location.map(Into::into),
            dataset: dataset.map(Into::into),
            table: table.map(Into::into),
            selected_tools: selected_tools(settings.get("selected_tools"))?,
        })
    }

    pub(super) const fn credentials(&self) -> &GcpToolkitConfig {
        &self.credentials
    }

    /// The configured default project, if any.
    pub(super) fn project(&self) -> Option<&str> {
        self.project.as_deref()
    }

    /// The project jobs run in: the configured project, else the service
    /// account's own, as `google.cloud.bigquery.Client` resolves it.
    pub(super) fn job_project(&self) -> Option<&str> {
        self.project
            .as_deref()
            .or(self.credential_project.as_deref())
    }

    pub(super) fn location(&self) -> Option<&str> {
        self.location.as_deref()
    }

    pub(super) fn dataset(&self) -> Option<&str> {
        self.dataset.as_deref()
    }

    pub(super) fn table(&self) -> Option<&str> {
        self.table.as_deref()
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn credential_project(raw: &str) -> Result<Option<Box<str>>, BigQueryConfigError> {
    let document: Value = serde_json::from_str(raw).map_err(|_| invalid_configuration())?;
    match document.get("project_id") {
        Some(Value::String(value)) if valid_project(value) => Ok(Some(value.as_str().into())),
        None | Some(Value::Null) => Ok(None),
        Some(_) => Err(invalid_configuration()),
    }
}

fn optional_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<Option<&'a str>, BigQueryConfigError> {
    match object.get(name) {
        None | Some(Value::Null) => Ok(None),
        // The UI stores a cleared field as the empty string; the SDK treats it
        // as unset (`if not (self.project and ...)`).
        Some(Value::String(value)) if value.is_empty() => Ok(None),
        Some(Value::String(value)) if value.len() > limit => Err(resource_exhausted()),
        Some(Value::String(value)) => Ok(Some(value)),
        Some(_) => Err(invalid_configuration()),
    }
}

/// A Google Cloud project ID, including the legacy `domain:project` form.
pub(super) fn valid_project(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_PROJECT_BYTES
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'.' | b':' | b'_'))
}

fn valid_location(value: &str) -> bool {
    value
        .bytes()
        .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
}

/// A `BigQuery` dataset ID: letters, digits and underscores.
pub(super) fn valid_dataset(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_DATASET_BYTES
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
}

/// A `BigQuery` table ID. Unicode letters, marks, digits, connectors, dashes
/// and spaces are legal; a backtick, a backslash, a slash or a control
/// character would escape the quoted identifier or the REST path.
pub(super) fn valid_table(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_TABLE_BYTES
        && !value.chars().any(|character| {
            matches!(character, '`' | '\\' | '/' | '?' | '#' | '%') || character.is_control()
        })
}

fn selected_tools(value: Option<&Value>) -> Result<Vec<Box<str>>, BigQueryConfigError> {
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

const fn invalid_configuration() -> BigQueryConfigError {
    BigQueryConfigError {
        code: BigQueryConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> BigQueryConfigError {
    BigQueryConfigError {
        code: BigQueryConfigErrorCode::ResourceExhausted,
    }
}
