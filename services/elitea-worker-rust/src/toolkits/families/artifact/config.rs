use std::fmt;

use serde_json::{Map, Value};

const MAX_BUCKET_BYTES: usize = 63;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ArtifactConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable, data-free failure for claim-materialized artifact settings.
pub(crate) struct ArtifactConfigError {
    code: ArtifactConfigErrorCode,
}

impl ArtifactConfigError {
    #[must_use]
    pub(crate) const fn code(&self) -> ArtifactConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for ArtifactConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ArtifactConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ArtifactConfigError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ArtifactConfigErrorCode::InvalidConfiguration => {
                "the artifact toolkit configuration is invalid"
            }
            ArtifactConfigErrorCode::ResourceExhausted => {
                "the artifact toolkit configuration exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for ArtifactConfigError {}

/// One invocation-scoped artifact toolkit: the bucket its tools default to,
/// and the selected-tool profile.
///
/// There is no credential here, and that absence is the family's defining
/// property: the authority is the live execution claim, held separately in
/// `ArtifactToolAuthority`, not a secret redeemed into these settings.
pub(crate) struct ArtifactToolkitConfig {
    bucket: Box<str>,
    selected_tools: Vec<Box<str>>,
}

impl ArtifactToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, ArtifactConfigError> {
        let bucket = settings
            .get("bucket")
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|value| !value.is_empty())
            .ok_or_else(invalid_configuration)?;
        if bucket.len() > MAX_BUCKET_BYTES {
            return Err(resource_exhausted());
        }
        if !valid_bucket_name(bucket) {
            return Err(invalid_configuration());
        }
        Ok(Self {
            bucket: bucket.into(),
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(in crate::toolkits) fn bucket(&self) -> &str {
        &self.bucket
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}

/// The bucket-name shape the platform itself enforces
/// (`bucketNamePattern` in `internal/api/v2/artifacts/handler.go` and
/// `ObjectRef`'s own `bucketPattern`). A name outside it addresses no bucket,
/// so a toolkit configured with one would build tools that can only fail.
fn valid_bucket_name(bucket: &str) -> bool {
    let bytes = bucket.as_bytes();
    if bytes.len() < 2 {
        return false;
    }
    if !bytes[0].is_ascii_lowercase() {
        return false;
    }
    bytes[1..]
        .iter()
        .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || *byte == b'-')
}

/// The selected-tool profile, and the ONE place this family deliberately
/// differs from its neighbours: an unrecognised name is KEPT, not refused.
///
/// `sonar`'s `validate_selection` refuses a name it does not implement, which
/// is right for a family whose whole tool list it owns. The artifact type's
/// stored catalogue is the SDK's, and it names tools this runtime does not
/// serve yet (`read_multiple_files`, `get_file_metadata`, `append_data`,
/// `create_new_bucket`, and the indexing pair). Refusing the configuration
/// because it mentions one of them would fail materialization for every
/// toolkit created through the product's own form — turning a partial port
/// into a dead agent, which is exactly the outcome #906 is about. The unknown
/// names simply select nothing; `tools.rs` binds what it has.
fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<Box<str>>, ArtifactConfigError> {
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
    let mut selected: Vec<Box<str>> = Vec::with_capacity(values.len().min(8));
    for value in values {
        let name = value.as_str().ok_or_else(invalid_configuration)?.trim();
        if name.is_empty() || name.chars().any(char::is_control) {
            return Err(invalid_configuration());
        }
        if name.len() > MAX_TOOL_NAME_BYTES {
            return Err(resource_exhausted());
        }
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

const fn invalid_configuration() -> ArtifactConfigError {
    ArtifactConfigError {
        code: ArtifactConfigErrorCode::InvalidConfiguration,
    }
}

const fn resource_exhausted() -> ArtifactConfigError {
    ArtifactConfigError {
        code: ArtifactConfigErrorCode::ResourceExhausted,
    }
}
