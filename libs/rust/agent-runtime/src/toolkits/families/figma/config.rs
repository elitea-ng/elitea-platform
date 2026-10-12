use std::collections::HashSet;
use std::fmt;

use serde_json::{Map, Value};
use zeroize::Zeroizing;

/// The SDK's `GLOBAL_LIMIT`: characters of serialized output.
pub(crate) const DEFAULT_GLOBAL_LIMIT: u64 = 1_000_000;
const MAX_TOKEN_BYTES: usize = 16 * 1_024;
const MAX_REGEXP_BYTES: usize = 4 * 1_024;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum FigmaConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable configuration failure that never carries the token.
pub(crate) struct FigmaConfigError {
    code: FigmaConfigErrorCode,
}

impl FigmaConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> FigmaConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for FigmaConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("FigmaConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for FigmaConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            FigmaConfigErrorCode::InvalidConfiguration => {
                "the Figma toolkit configuration is invalid"
            }
            FigmaConfigErrorCode::ResourceExhausted => {
                "the Figma toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for FigmaConfigError {}

/// Invocation-scoped Figma authority materialized from one accepted claim.
///
/// The SDK reads `figma_configuration.token` (sent as `X-Figma-Token`) and
/// the toolkit-level `global_limit` and `global_regexp` output defaults; it
/// refuses a toolkit without a token and a `global_regexp` that does not
/// compile. The indexing and LLM-prompt settings (`pgvector_configuration`,
/// `embedding_model`, `number_of_threads`) belong to tools this family does
/// not serve and are ignored. Non-`Clone` and non-`Debug`.
pub(crate) struct FigmaToolkitConfig {
    token: Zeroizing<String>,
    global_limit: u64,
    global_regexp: Option<Box<str>>,
    selected_tools: Vec<Box<str>>,
}

impl FigmaToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, FigmaConfigError> {
        let configuration = settings
            .get("figma_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid_configuration)?;
        let token = match configuration.get("token") {
            Some(Value::String(token)) => {
                validate_text(token, MAX_TOKEN_BYTES)?;
                Zeroizing::new(token.clone())
            }
            _ => return Err(invalid_configuration()),
        };
        let global_limit = match settings.get("global_limit") {
            None | Some(Value::Null) => DEFAULT_GLOBAL_LIMIT,
            Some(value) => value
                .as_u64()
                .filter(|limit| *limit > 0)
                .ok_or_else(invalid_configuration)?,
        };
        let global_regexp = match settings.get("global_regexp") {
            None | Some(Value::Null) => None,
            Some(Value::String(pattern)) if pattern.is_empty() => None,
            Some(Value::String(pattern)) => {
                if pattern.len() > MAX_REGEXP_BYTES {
                    return Err(resource_exhausted());
                }
                fancy_regex::Regex::new(pattern).map_err(|_| invalid_configuration())?;
                Some(pattern.as_str().into())
            }
            Some(_) => return Err(invalid_configuration()),
        };
        Ok(Self {
            token,
            global_limit,
            global_regexp,
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) fn token(&self) -> &str {
        &self.token
    }

    pub(super) const fn global_limit(&self) -> u64 {
        self.global_limit
    }

    pub(super) fn global_regexp(&self) -> Option<&str> {
        self.global_regexp.as_deref()
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

fn validate_text(value: &str, limit: usize) -> Result<(), FigmaConfigError> {
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if value.trim().is_empty() || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n')) {
        return Err(invalid_configuration());
    }
    Ok(())
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, FigmaConfigError> {
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

const fn invalid_configuration() -> FigmaConfigError {
    FigmaConfigError {
        code: FigmaConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> FigmaConfigError {
    FigmaConfigError {
        code: FigmaConfigErrorCode::ResourceExhausted,
    }
}
