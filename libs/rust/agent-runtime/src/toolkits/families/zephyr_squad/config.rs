use std::collections::HashSet;
use std::fmt;

use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_ACCOUNT_ID_BYTES: usize = 1_024;
const MAX_ACCESS_KEY_BYTES: usize = 16 * 1_024;
const MAX_SECRET_KEY_BYTES: usize = 16 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ZephyrSquadConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that never carries Zephyr credentials.
pub(crate) struct ZephyrSquadConfigError {
    code: ZephyrSquadConfigErrorCode,
}

impl ZephyrSquadConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> ZephyrSquadConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for ZephyrSquadConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ZephyrSquadConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ZephyrSquadConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ZephyrSquadConfigErrorCode::InvalidConfiguration => {
                "the Zephyr Squad toolkit configuration is invalid"
            }
            ZephyrSquadConfigErrorCode::ResourceExhausted => {
                "the Zephyr Squad toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for ZephyrSquadConfigError {}

/// Invocation-scoped authority materialized from one accepted claim.
///
/// Zephyr Squad has no separately registered SDK configuration; Main freezes
/// these three values directly in toolkit settings. The secrets are
/// non-cloneable and non-debuggable and cannot be sourced from the process
/// environment.
pub(crate) struct ZephyrSquadToolkitConfig {
    account_id: Box<str>,
    access_key: Zeroizing<String>,
    secret_key: Zeroizing<String>,
    selected_tools: Vec<Box<str>>,
}

impl ZephyrSquadToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, ZephyrSquadConfigError> {
        let account_id = required_text(settings, "account_id", MAX_ACCOUNT_ID_BYTES)?;
        let access_key = required_text(settings, "access_key", MAX_ACCESS_KEY_BYTES)?;
        let secret_key = required_text(settings, "secret_key", MAX_SECRET_KEY_BYTES)?;

        Ok(Self {
            account_id: account_id.into(),
            access_key: Zeroizing::new(access_key.to_owned()),
            secret_key: Zeroizing::new(secret_key.to_owned()),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) fn account_id(&self) -> &str {
        &self.account_id
    }

    pub(super) fn access_key(&self) -> &str {
        &self.access_key
    }

    pub(super) fn secret_key(&self) -> &str {
        &self.secret_key
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
) -> Result<&'a str, ZephyrSquadConfigError> {
    let value = object
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_configuration)?;
    validate_text(value, limit)?;
    Ok(value)
}

fn validate_text(value: &str, limit: usize) -> Result<(), ZephyrSquadConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, ZephyrSquadConfigError> {
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

const fn invalid_configuration() -> ZephyrSquadConfigError {
    ZephyrSquadConfigError {
        code: ZephyrSquadConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> ZephyrSquadConfigError {
    ZephyrSquadConfigError {
        code: ZephyrSquadConfigErrorCode::ResourceExhausted,
    }
}
