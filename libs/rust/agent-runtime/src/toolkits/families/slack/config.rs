use std::collections::HashSet;
use std::fmt;

use serde_json::{Map, Value};
use zeroize::Zeroizing;

const MAX_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_CHANNEL_ID_BYTES: usize = 256;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SlackConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable, data-free failure for one materialized Slack configuration.
pub(crate) struct SlackConfigError {
    code: SlackConfigErrorCode,
}

impl SlackConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> SlackConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for SlackConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SlackConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SlackConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SlackConfigErrorCode::InvalidConfiguration => {
                "the Slack toolkit configuration is invalid"
            }
            SlackConfigErrorCode::ResourceExhausted => {
                "the Slack toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for SlackConfigError {}

/// Invocation-scoped authority for one Slack toolset.
///
/// Main resolves the nested configuration only for the admitted command. The
/// token is non-cloneable and non-debuggable, and no process environment or
/// alternate Slack origin participates in authority.
pub(crate) struct SlackToolkitConfig {
    token: Zeroizing<String>,
    default_channel_id: Option<Box<str>>,
    selected_tools: Vec<Box<str>>,
}

impl SlackToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, SlackConfigError> {
        let configuration = settings
            .get("slack_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let token = configuration
            .get("slack_token")
            .and_then(Value::as_str)
            .ok_or_else(invalid_configuration)?;
        validate_text(token, MAX_TOKEN_BYTES, false)?;
        let default_channel_id: Option<Box<str>> =
            optional_text(configuration, "channel_id", MAX_CHANNEL_ID_BYTES)?.map(Into::into);
        if default_channel_id
            .as_deref()
            .is_some_and(|value| !value.bytes().all(|byte| byte.is_ascii_alphanumeric()))
        {
            return Err(invalid_configuration());
        }

        Ok(Self {
            token: Zeroizing::new(token.to_owned()),
            default_channel_id,
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) fn token(&self) -> &str {
        &self.token
    }

    pub(super) fn default_channel_id(&self) -> Option<&str> {
        self.default_channel_id.as_deref()
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }

    #[cfg(test)]
    pub(in crate::toolkits) fn test_default_channel_id(&self) -> Option<&str> {
        self.default_channel_id()
    }
}

fn optional_text<'a>(
    object: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<Option<&'a str>, SlackConfigError> {
    match object.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => {
            validate_text(value, limit, false)?;
            Ok(Some(value))
        }
        Some(_) => Err(invalid_configuration()),
    }
}

fn validate_text(value: &str, limit: usize, allow_empty: bool) -> Result<(), SlackConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if (!allow_empty && value.is_empty())
        || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n'))
    {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, SlackConfigError> {
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
        validate_text(value, MAX_TOOL_NAME_BYTES, false)?;
        if seen.insert(value) {
            selected.push(value.into());
        }
    }
    Ok(selected)
}

const fn invalid_configuration() -> SlackConfigError {
    SlackConfigError {
        code: SlackConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> SlackConfigError {
    SlackConfigError {
        code: SlackConfigErrorCode::ResourceExhausted,
    }
}
