//! The ADK tool, selection and argument plumbing the four ADO families share.

use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::AdoClientError;
use super::config::{AdoConfigError, AdoConfigErrorCode};

const MAX_DESCRIPTION_CHARS: usize = 1_000;
pub(crate) const MAX_TEXT_BYTES: usize = 256 * 1_024;
pub(crate) const MAX_IDENTIFIER_BYTES: usize = 1_024;
const MAX_LIST_ITEMS: usize = 256;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AdoToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for an ADO family.
pub(crate) struct AdoToolsetError {
    code: AdoToolsetErrorCode,
}

impl AdoToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> AdoToolsetErrorCode {
        self.code
    }

    pub(crate) const fn new(code: AdoToolsetErrorCode) -> Self {
        Self { code }
    }
}

impl fmt::Debug for AdoToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AdoToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AdoToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AdoToolsetErrorCode::InvalidConfiguration => {
                "the Azure DevOps toolkit configuration is invalid"
            }
            AdoToolsetErrorCode::ResourceExhausted => {
                "the Azure DevOps toolkit configuration exceeds its approved limit"
            }
            AdoToolsetErrorCode::UnsupportedSelection => {
                "none of the selected Azure DevOps tools is served by this runtime"
            }
            AdoToolsetErrorCode::Client => "the Azure DevOps client could not be created",
            AdoToolsetErrorCode::InvalidDefinition => {
                "the Azure DevOps ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for AdoToolsetError {}

impl From<AdoConfigError> for AdoToolsetError {
    fn from(source: AdoConfigError) -> Self {
        Self {
            code: match source.code() {
                AdoConfigErrorCode::InvalidConfiguration => {
                    AdoToolsetErrorCode::InvalidConfiguration
                }
                AdoConfigErrorCode::ResourceExhausted => AdoToolsetErrorCode::ResourceExhausted,
            },
        }
    }
}

impl From<AdoClientError> for AdoToolsetError {
    fn from(_: AdoClientError) -> Self {
        Self {
            code: AdoToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for AdoToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: AdoToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// One family's tool catalogue entry.
pub(crate) trait AdoToolKind: Copy + Send + Sync + 'static {
    fn name(self) -> &'static str;
    fn description(self) -> &'static str;
    fn schema(self) -> Value;
    fn is_read_only(self) -> bool;
}

/// One family's dispatcher: arguments already stripped of explicit nulls.
#[async_trait]
pub(crate) trait AdoToolExecutor<K: AdoToolKind>: Send + Sync {
    async fn execute(&self, kind: K, arguments: &Map<String, Value>) -> adk_core::Result<Value>;
}

struct AdoTool<K: AdoToolKind> {
    kind: K,
    description: Box<str>,
    executor: Arc<dyn AdoToolExecutor<K>>,
}

#[async_trait]
impl<K: AdoToolKind> Tool for AdoTool<K> {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        self.kind.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.kind.is_read_only()
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.kind.schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let Value::Object(mut arguments) = arguments else {
            return Err(invalid_arguments());
        };
        arguments.retain(|_, value| !value.is_null());
        self.executor.execute(self.kind, &arguments).await
    }
}

/// Build the family's tools for `selected` (all served tools when empty).
/// `description_suffix` is appended to every description (the wiki's
/// `Default wiki: <name>` line).
///
/// A selected SDK tool this runtime does not serve (the index tools, and
/// any per-family gap the capability snapshot lists) is omitted with a
/// warning, as the partial `SharePoint` and GitHub families do; a selection
/// with nothing served left is unsupported, so the toolkit is skipped.
pub(crate) fn build_toolset<K: AdoToolKind>(
    toolkit_name: &str,
    toolkit_type: &'static str,
    all: &[K],
    selected: &[Box<str>],
    policy: &Arc<ToolAdmissionPolicy>,
    executor: &Arc<dyn AdoToolExecutor<K>>,
    description_suffix: &str,
) -> Result<BasicToolset, AdoToolsetError> {
    let kinds: Vec<K> = if selected.is_empty() {
        all.to_vec()
    } else {
        let kinds = all
            .iter()
            .copied()
            .filter(|kind| selected.iter().any(|name| name.as_ref() == kind.name()))
            .collect::<Vec<_>>();
        if kinds.is_empty() {
            return Err(AdoToolsetError::new(
                AdoToolsetErrorCode::UnsupportedSelection,
            ));
        }
        let omitted = selected.len().saturating_sub(kinds.len());
        if omitted > 0 {
            tracing::warn!(
                event = "ado_tool_selection_partially_materialized",
                toolkit_type,
                selected_tool_count = selected.len(),
                materialized_tool_count = kinds.len(),
                omitted_tool_count = omitted,
                "Azure DevOps tools this runtime does not serve were omitted from the native toolset"
            );
        }
        kinds
    };
    let tools = kinds
        .into_iter()
        .map(|kind| {
            let description = format!(
                "Toolkit: {toolkit_name}\n{}{description_suffix}",
                kind.description()
            );
            Arc::new(AdoTool {
                kind,
                description: description
                    .chars()
                    .take(MAX_DESCRIPTION_CHARS)
                    .collect::<String>()
                    .into_boxed_str(),
                executor: Arc::clone(executor),
            }) as Arc<dyn Tool>
        })
        .collect();
    admit_materialized_toolset(toolkit_name, toolkit_type, policy, tools).map_err(Into::into)
}

pub(crate) fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "ado.arguments.invalid",
        "the Azure DevOps tool arguments are invalid",
    )
}

pub(crate) fn reject_unknown_keys(
    arguments: &Map<String, Value>,
    allowed: &[&str],
) -> Result<(), AdkError> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn bounded_text(value: &str, limit: usize) -> bool {
    value.len() <= limit && !value.contains('\0')
}

/// A required string argument (may be empty, as pydantic `str` allows).
pub(crate) fn required_str<'a>(
    arguments: &'a Map<String, Value>,
    key: &str,
    limit: usize,
) -> Result<&'a str, AdkError> {
    optional_str(arguments, key, limit)?.ok_or_else(invalid_arguments)
}

pub(crate) fn optional_str<'a>(
    arguments: &'a Map<String, Value>,
    key: &str,
    limit: usize,
) -> Result<Option<&'a str>, AdkError> {
    match arguments.get(key) {
        None => Ok(None),
        Some(Value::String(value)) if bounded_text(value, limit) => Ok(Some(value)),
        Some(_) => Err(invalid_arguments()),
    }
}

/// A string-typed SDK identifier the model may also send as a number.
pub(crate) fn required_id_text(
    arguments: &Map<String, Value>,
    key: &str,
) -> Result<String, AdkError> {
    match arguments.get(key) {
        Some(Value::String(value)) if bounded_text(value, MAX_IDENTIFIER_BYTES) => {
            Ok(value.clone())
        }
        Some(Value::Number(number)) => Ok(number.to_string()),
        _ => Err(invalid_arguments()),
    }
}

/// An SDK `int`: a JSON integer, or a decimal string pydantic would coerce.
pub(crate) fn optional_i64(
    arguments: &Map<String, Value>,
    key: &str,
) -> Result<Option<i64>, AdkError> {
    match arguments.get(key) {
        None => Ok(None),
        Some(Value::Number(number)) => number.as_i64().map(Some).ok_or_else(invalid_arguments),
        Some(Value::String(text)) => text
            .trim()
            .parse::<i64>()
            .map(Some)
            .map_err(|_| invalid_arguments()),
        Some(_) => Err(invalid_arguments()),
    }
}

pub(crate) fn required_i64(arguments: &Map<String, Value>, key: &str) -> Result<i64, AdkError> {
    optional_i64(arguments, key)?.ok_or_else(invalid_arguments)
}

/// A positive provider id.
pub(crate) fn required_id(arguments: &Map<String, Value>, key: &str) -> Result<u64, AdkError> {
    optional_id(arguments, key)?.ok_or_else(invalid_arguments)
}

pub(crate) fn optional_id(
    arguments: &Map<String, Value>,
    key: &str,
) -> Result<Option<u64>, AdkError> {
    match optional_i64(arguments, key)? {
        None => Ok(None),
        Some(value) if value > 0 => Ok(Some(value.cast_unsigned())),
        Some(_) => Err(invalid_arguments()),
    }
}

pub(crate) fn optional_bool(
    arguments: &Map<String, Value>,
    key: &str,
    default: bool,
) -> Result<bool, AdkError> {
    match arguments.get(key) {
        None => Ok(default),
        Some(Value::Bool(value)) => Ok(*value),
        Some(_) => Err(invalid_arguments()),
    }
}

pub(crate) fn optional_string_list(
    arguments: &Map<String, Value>,
    key: &str,
) -> Result<Option<Vec<String>>, AdkError> {
    match arguments.get(key) {
        None => Ok(None),
        Some(Value::Array(values)) if values.len() <= MAX_LIST_ITEMS => values
            .iter()
            .map(|value| {
                value
                    .as_str()
                    .filter(|value| bounded_text(value, MAX_IDENTIFIER_BYTES))
                    .map(ToOwned::to_owned)
                    .ok_or_else(invalid_arguments)
            })
            .collect::<Result<Vec<_>, _>>()
            .map(Some),
        Some(_) => Err(invalid_arguments()),
    }
}

pub(crate) fn required_id_list(
    arguments: &Map<String, Value>,
    key: &str,
) -> Result<Vec<u64>, AdkError> {
    match arguments.get(key) {
        Some(Value::Array(values)) if values.len() <= MAX_LIST_ITEMS => values
            .iter()
            .map(|value| match value {
                Value::Number(number) => number
                    .as_u64()
                    .filter(|value| *value > 0)
                    .ok_or_else(invalid_arguments),
                Value::String(text) => text
                    .trim()
                    .parse::<u64>()
                    .ok()
                    .filter(|value| *value > 0)
                    .ok_or_else(invalid_arguments),
                _ => Err(invalid_arguments()),
            })
            .collect(),
        _ => Err(invalid_arguments()),
    }
}

/// JSON-schema helpers mirroring the pydantic models in the SDK snapshot.
pub(crate) mod schema {
    use serde_json::{Map, Value, json};

    pub(crate) fn object(properties: &[(&str, Value)], required: &[&str]) -> Value {
        let mut map = Map::new();
        for (name, property) in properties {
            map.insert((*name).to_owned(), property.clone());
        }
        json!({
            "type":"object",
            "properties":map,
            "required":required,
            "additionalProperties":false
        })
    }

    pub(crate) fn string(description: &str) -> Value {
        json!({"type":"string","description":description})
    }

    pub(crate) fn optional_string(description: &str, default: Option<&str>) -> Value {
        json!({"type":["string","null"],"default":default,"description":description})
    }

    pub(crate) fn integer(description: &str) -> Value {
        json!({"type":"integer","description":description})
    }

    pub(crate) fn optional_integer(description: &str) -> Value {
        json!({"type":["integer","null"],"default":null,"description":description})
    }

    pub(crate) fn optional_bool(description: &str, default: bool) -> Value {
        json!({"type":["boolean","null"],"default":default,"description":description})
    }

    pub(crate) fn optional_string_array(description: &str) -> Value {
        json!({
            "type":["array","null"],
            "items":{"type":"string"},
            "default":null,
            "description":description
        })
    }

    pub(crate) fn integer_array(description: &str) -> Value {
        json!({"type":"array","items":{"type":"integer"},"description":description})
    }
}
