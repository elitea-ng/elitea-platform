//! The `pptx` toolkit's settings: the SDK's two fields, `bucket_name` and
//! `selected_tools` (`elitea_sdk/tools/pptx/__init__.py`,
//! `toolkit_config_schema`).
//!
//! There is no credential: the authority is the live execution claim, lent
//! as the Office host (`office_host`), exactly as for the `artifact` family.

use std::fmt;

use serde_json::{Map, Value};

use super::schema::{ALL_TOOLS, PptxTool};

const MAX_BUCKET_BYTES: usize = 63;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

/// Why the settings were refused. Carries no value.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PptxConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable, data-free failure for claim-materialized pptx settings.
pub(crate) struct PptxConfigError {
    code: PptxConfigErrorCode,
}

impl PptxConfigError {
    pub(crate) const fn code(&self) -> PptxConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for PptxConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("PptxConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for PptxConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self.code {
            PptxConfigErrorCode::InvalidConfiguration => {
                "the pptx toolkit configuration is invalid"
            }
            PptxConfigErrorCode::ResourceExhausted => {
                "the pptx toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for PptxConfigError {}

/// One invocation-scoped pptx toolkit: its default bucket and the selected
/// tools.
pub(crate) struct PptxToolkitConfig {
    bucket: Box<str>,
    /// `None`: the SDK's "no selection means every tool" (an absent,
    /// null or empty list). `Some`: only these, possibly none.
    selected: Option<Vec<PptxTool>>,
}

impl PptxToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, PptxConfigError> {
        let bucket = settings
            .get("bucket_name")
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|value| !value.is_empty())
            .ok_or(INVALID)?;
        if bucket.len() > MAX_BUCKET_BYTES {
            return Err(EXHAUSTED);
        }
        if !valid_bucket_name(bucket) {
            return Err(INVALID);
        }
        Ok(Self {
            bucket: bucket.into(),
            selected: selected_tools(settings)?,
        })
    }

    pub(crate) fn bucket(&self) -> &str {
        &self.bucket
    }

    /// The tools to serve: every tool when none is selected, else the
    /// selected ones the family implements (an unknown name selects nothing,
    /// as for the `artifact` family: the stored catalogue is the SDK's and
    /// must not fail a toolkit created through the product's own form).
    pub(crate) fn tools(&self) -> Vec<PptxTool> {
        self.selected.clone().unwrap_or_else(|| ALL_TOOLS.to_vec())
    }
}

const INVALID: PptxConfigError = PptxConfigError {
    code: PptxConfigErrorCode::InvalidConfiguration,
};
const EXHAUSTED: PptxConfigError = PptxConfigError {
    code: PptxConfigErrorCode::ResourceExhausted,
};

/// The bucket-name shape the platform enforces (see the `artifact` family).
fn valid_bucket_name(bucket: &str) -> bool {
    let bytes = bucket.as_bytes();
    bytes.len() >= 2
        && bytes[0].is_ascii_lowercase()
        && bytes[1..]
            .iter()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || *b == b'-')
}

fn selected_tools(settings: &Map<String, Value>) -> Result<Option<Vec<PptxTool>>, PptxConfigError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(None);
    };
    if value.is_null() {
        return Ok(None);
    }
    let values = value.as_array().ok_or(INVALID)?;
    if values.len() > MAX_SELECTED_TOOLS {
        return Err(EXHAUSTED);
    }
    if values.is_empty() {
        return Ok(None);
    }
    let mut selected: Vec<PptxTool> = Vec::new();
    for value in values {
        let name = value.as_str().ok_or(INVALID)?.trim();
        if name.is_empty() || name.chars().any(char::is_control) {
            return Err(INVALID);
        }
        if name.len() > MAX_TOOL_NAME_BYTES {
            return Err(EXHAUSTED);
        }
        if let Some(tool) = PptxTool::parse(name)
            && !selected.contains(&tool)
        {
            selected.push(tool);
        }
    }
    Ok(Some(selected))
}
