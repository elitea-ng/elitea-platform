use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{ZephyrRestError, ZephyrRestFamily};
use super::config::{ZephyrRestConfigError, ZephyrRestConfigErrorCode};

const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;
/// Keep family admission aligned with `PolicyBoundTool`'s per-string ceiling.
pub(in crate::toolkits) const MAX_JSON_STRING_BYTES: usize = 64 * 1_024;
const MAX_TEXT_BYTES: usize = 64 * 1_024;
const MAX_IDENTIFIER_BYTES: usize = 256;
const MAX_JSON_DEPTH: usize = 32;
const MAX_JSON_NODES: usize = 8_192;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ZephyrRestToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for one Zephyr REST family.
pub(crate) struct ZephyrRestToolsetError {
    code: ZephyrRestToolsetErrorCode,
}

impl ZephyrRestToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> ZephyrRestToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for ZephyrRestToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ZephyrRestToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ZephyrRestToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ZephyrRestToolsetErrorCode::InvalidConfiguration => {
                "the Zephyr toolkit configuration is invalid"
            }
            ZephyrRestToolsetErrorCode::ResourceExhausted => {
                "the Zephyr toolkit configuration exceeds its approved limit"
            }
            ZephyrRestToolsetErrorCode::UnsupportedSelection => {
                "the selected Zephyr tools are not served by this runtime"
            }
            ZephyrRestToolsetErrorCode::Client => "the Zephyr client could not be created",
            ZephyrRestToolsetErrorCode::InvalidDefinition => {
                "the Zephyr ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for ZephyrRestToolsetError {}

impl From<ZephyrRestConfigError> for ZephyrRestToolsetError {
    fn from(source: ZephyrRestConfigError) -> Self {
        Self {
            code: match source.code() {
                ZephyrRestConfigErrorCode::InvalidConfiguration => {
                    ZephyrRestToolsetErrorCode::InvalidConfiguration
                }
                ZephyrRestConfigErrorCode::ResourceExhausted => {
                    ZephyrRestToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<ZephyrRestError> for ZephyrRestToolsetError {
    fn from(_: ZephyrRestError) -> Self {
        Self {
            code: ZephyrRestToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for ZephyrRestToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: ZephyrRestToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// The argument-error namespace of one family.
pub(in crate::toolkits) struct ZephyrArgumentCodes {
    pub(in crate::toolkits) label: &'static str,
    pub(in crate::toolkits) invalid: &'static str,
    pub(in crate::toolkits) exhausted: &'static str,
}

impl ZephyrArgumentCodes {
    pub(in crate::toolkits) fn invalid(&self) -> AdkError {
        AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::InvalidInput,
            self.invalid,
            format!("the {} tool arguments are invalid", self.label),
        )
    }

    pub(in crate::toolkits) fn exhausted(&self) -> AdkError {
        AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::InvalidInput,
            self.exhausted,
            format!(
                "the {} tool arguments exceed the approved limit",
                self.label
            ),
        )
    }
}

/// One operation of a Zephyr REST family: its SDK name, model metadata,
/// argument schema and behavior over the family's API.
#[async_trait]
pub(in crate::toolkits) trait ZephyrToolSpec:
    Copy + Send + Sync + 'static
{
    type Api: ?Sized + Send + Sync;

    fn name(self) -> &'static str;
    fn description(self) -> &'static str;
    fn is_read_only(self) -> bool;
    fn schema(self) -> Value;
    fn family() -> &'static ZephyrRestFamily;
    fn arguments() -> &'static ZephyrArgumentCodes;

    async fn execute(
        self,
        api: &Self::Api,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value>;
}

/// Build the tools `selected` names (all of `kinds` when it is empty).
///
/// A selected name the family does not serve — an indexing tool, or an SDK
/// operation this runtime cannot perform — is omitted with a warning, the way
/// the catalogue marks it unavailable; a selection that leaves nothing is an
/// unsupported selection, so the toolkit is skipped instead of bound empty.
pub(in crate::toolkits) fn build_toolset<K: ZephyrToolSpec>(
    toolkit_name: &str,
    toolkit_type: &str,
    kinds: &[K],
    selected: &[Box<str>],
    description_suffix: Option<&str>,
    policy: &Arc<ToolAdmissionPolicy>,
    api: &Arc<K::Api>,
) -> Result<BasicToolset, ZephyrRestToolsetError> {
    let chosen = select(kinds, selected)?;
    let tools = chosen
        .into_iter()
        .map(|kind| {
            Arc::new(ZephyrTool::new(
                kind,
                toolkit_name,
                description_suffix,
                Arc::clone(api),
            )) as Arc<dyn Tool>
        })
        .collect();
    admit_materialized_toolset(toolkit_name, toolkit_type, policy, tools).map_err(Into::into)
}

fn select<K: ZephyrToolSpec>(
    kinds: &[K],
    selected: &[Box<str>],
) -> Result<Vec<K>, ZephyrRestToolsetError> {
    if selected.is_empty() {
        return Ok(kinds.to_vec());
    }
    let chosen = kinds
        .iter()
        .copied()
        .filter(|kind| selected.iter().any(|name| name.as_ref() == kind.name()))
        .collect::<Vec<_>>();
    if chosen.is_empty() {
        return Err(ZephyrRestToolsetError {
            code: ZephyrRestToolsetErrorCode::UnsupportedSelection,
        });
    }
    let omitted = selected.len().saturating_sub(chosen.len());
    if omitted > 0 {
        tracing::warn!(
            event = "zephyr_tool_selection_partially_materialized",
            selected_tool_count = selected.len(),
            materialized_tool_count = chosen.len(),
            omitted_tool_count = omitted,
            "Zephyr operations this runtime does not serve were omitted from the native toolset"
        );
    }
    Ok(chosen)
}

struct ZephyrTool<K: ZephyrToolSpec> {
    kind: K,
    api: Arc<K::Api>,
    description: Box<str>,
}

impl<K: ZephyrToolSpec> ZephyrTool<K> {
    fn new(kind: K, toolkit_name: &str, suffix: Option<&str>, api: Arc<K::Api>) -> Self {
        // The SDK's `f"Toolkit: {name}\n{description}{suffix}"[:1000]`.
        let mut description = format!("Toolkit: {toolkit_name}\n{}", kind.description());
        if let Some(suffix) = suffix {
            description.push_str(suffix);
        }
        Self {
            kind,
            api,
            description: description
                .chars()
                .take(MAX_DESCRIPTION_BYTES)
                .collect::<String>()
                .into_boxed_str(),
        }
    }
}

#[async_trait]
impl<K: ZephyrToolSpec> Tool for ZephyrTool<K> {
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
        let codes = K::arguments();
        if serde_json::to_vec(&arguments)
            .map_err(|_| codes.invalid())?
            .len()
            > MAX_ARGUMENT_BYTES
        {
            return Err(codes.exhausted());
        }
        let Value::Object(mut arguments) = arguments else {
            return Err(codes.invalid());
        };
        // BaseAction drops explicit nulls before dispatch, so a null optional
        // argument means "use the default".
        arguments.retain(|_, value| !value.is_null());
        self.kind.execute(self.api.as_ref(), &arguments).await
    }
}

/// Argument reading with one family's error namespace.
pub(in crate::toolkits) struct Arguments<'a> {
    values: &'a Map<String, Value>,
    codes: &'static ZephyrArgumentCodes,
}

impl<'a> Arguments<'a> {
    pub(in crate::toolkits) fn new(
        values: &'a Map<String, Value>,
        codes: &'static ZephyrArgumentCodes,
        allowed: &[&str],
    ) -> adk_core::Result<Self> {
        if values.keys().any(|key| !allowed.contains(&key.as_str())) {
            return Err(codes.invalid());
        }
        Ok(Self { values, codes })
    }

    pub(in crate::toolkits) fn invalid(&self) -> AdkError {
        self.codes.invalid()
    }

    pub(in crate::toolkits) fn exhausted(&self) -> AdkError {
        self.codes.exhausted()
    }

    /// A required identifier that becomes one URL path segment.
    pub(in crate::toolkits) fn identifier(&self, name: &str) -> adk_core::Result<&'a str> {
        self.optional_identifier(name)?
            .ok_or_else(|| self.codes.invalid())
    }

    pub(in crate::toolkits) fn optional_identifier(
        &self,
        name: &str,
    ) -> adk_core::Result<Option<&'a str>> {
        let Some(value) = self.values.get(name) else {
            return Ok(None);
        };
        let value = value.as_str().ok_or_else(|| self.codes.invalid())?;
        if value.len() > MAX_IDENTIFIER_BYTES {
            return Err(self.codes.exhausted());
        }
        if value.is_empty() || matches!(value, "." | "..") || value.chars().any(char::is_control) {
            return Err(self.codes.invalid());
        }
        Ok(Some(value))
    }

    /// A required free-text argument.
    pub(in crate::toolkits) fn text(&self, name: &str) -> adk_core::Result<&'a str> {
        self.optional_text(name)?
            .ok_or_else(|| self.codes.invalid())
    }

    pub(in crate::toolkits) fn optional_text(
        &self,
        name: &str,
    ) -> adk_core::Result<Option<&'a str>> {
        let Some(value) = self.values.get(name) else {
            return Ok(None);
        };
        let value = value.as_str().ok_or_else(|| self.codes.invalid())?;
        if value.len() > MAX_TEXT_BYTES {
            return Err(self.codes.exhausted());
        }
        if value.contains('\0') {
            return Err(self.codes.invalid());
        }
        Ok(Some(value))
    }

    pub(in crate::toolkits) fn optional_integer(
        &self,
        name: &str,
    ) -> adk_core::Result<Option<i64>> {
        match self.values.get(name) {
            None => Ok(None),
            Some(value) => value.as_i64().map(Some).ok_or_else(|| self.codes.invalid()),
        }
    }

    pub(in crate::toolkits) fn integer(&self, name: &str) -> adk_core::Result<i64> {
        self.optional_integer(name)?
            .ok_or_else(|| self.codes.invalid())
    }

    pub(in crate::toolkits) fn optional_bool(&self, name: &str) -> adk_core::Result<Option<bool>> {
        match self.values.get(name) {
            None => Ok(None),
            Some(value) => value
                .as_bool()
                .map(Some)
                .ok_or_else(|| self.codes.invalid()),
        }
    }

    pub(in crate::toolkits) fn optional_string_list(
        &self,
        name: &str,
    ) -> adk_core::Result<Option<Vec<&'a str>>> {
        let Some(value) = self.values.get(name) else {
            return Ok(None);
        };
        let values = value.as_array().ok_or_else(|| self.codes.invalid())?;
        if values.len() > 256 {
            return Err(self.codes.exhausted());
        }
        values
            .iter()
            .map(|value| {
                let value = value.as_str().ok_or_else(|| self.codes.invalid())?;
                if value.len() > MAX_IDENTIFIER_BYTES {
                    return Err(self.codes.exhausted());
                }
                Ok(value)
            })
            .collect::<adk_core::Result<Vec<_>>>()
            .map(Some)
    }

    /// The raw argument value, for SDK arguments typed as arbitrary JSON.
    pub(in crate::toolkits) fn raw(&self, name: &str) -> Option<&'a Value> {
        self.values.get(name)
    }

    /// A required JSON-encoded string argument, decoded and bounded.
    pub(in crate::toolkits) fn json(&self, name: &str) -> adk_core::Result<Value> {
        let encoded = self
            .values
            .get(name)
            .and_then(Value::as_str)
            .ok_or_else(|| self.codes.invalid())?;
        decode_json(encoded, self.codes)
    }
}

/// Decode one JSON document an SDK tool accepts as a string.
pub(in crate::toolkits) fn decode_json(
    encoded: &str,
    codes: &ZephyrArgumentCodes,
) -> adk_core::Result<Value> {
    if encoded.len() > MAX_JSON_STRING_BYTES {
        return Err(codes.exhausted());
    }
    let value: Value = serde_json::from_str(encoded).map_err(|_| codes.invalid())?;
    let mut nodes = 0_usize;
    bound_json(&value, 0, &mut nodes, codes)?;
    Ok(value)
}

/// Bound a JSON value an argument carries (depth, node count, NUL bytes).
pub(in crate::toolkits) fn bound_json(
    value: &Value,
    depth: usize,
    nodes: &mut usize,
    codes: &ZephyrArgumentCodes,
) -> adk_core::Result<()> {
    if depth > MAX_JSON_DEPTH {
        return Err(codes.exhausted());
    }
    *nodes = nodes.saturating_add(1);
    if *nodes > MAX_JSON_NODES {
        return Err(codes.exhausted());
    }
    match value {
        Value::Object(object) => {
            for (key, value) in object {
                if key.contains('\0') {
                    return Err(codes.invalid());
                }
                bound_json(value, depth + 1, nodes, codes)?;
            }
        }
        Value::Array(values) => {
            for value in values {
                bound_json(value, depth + 1, nodes, codes)?;
            }
        }
        Value::String(text) if text.contains('\0') => return Err(codes.invalid()),
        _ => {}
    }
    Ok(())
}

/// A JSON object schema: `required` lists the SDK-required properties.
pub(in crate::toolkits) fn object_schema(
    title: &str,
    properties: &[(&str, Value)],
    required: &[&str],
) -> Value {
    let properties = properties
        .iter()
        .map(|(name, schema)| ((*name).to_owned(), schema.clone()))
        .collect::<Map<_, _>>();
    json!({
        "title":title,
        "type":"object",
        "properties":properties,
        "required":required,
        "additionalProperties":false
    })
}

pub(in crate::toolkits) fn identifier_property(description: &str) -> Value {
    json!({
        "type":"string",
        "minLength":1,
        "maxLength":MAX_IDENTIFIER_BYTES,
        "description":description
    })
}

pub(in crate::toolkits) fn optional_identifier_property(description: &str) -> Value {
    json!({
        "type":["string","null"],
        "maxLength":MAX_IDENTIFIER_BYTES,
        "description":description
    })
}

pub(in crate::toolkits) fn text_property(description: &str) -> Value {
    json!({
        "type":"string",
        "maxLength":MAX_TEXT_BYTES / 4,
        "description":description
    })
}

pub(in crate::toolkits) fn optional_text_property(description: &str) -> Value {
    json!({
        "type":["string","null"],
        "maxLength":MAX_TEXT_BYTES / 4,
        "description":description
    })
}

pub(in crate::toolkits) fn json_text_property(description: &str) -> Value {
    json!({
        "type":"string",
        "maxLength":MAX_JSON_STRING_BYTES / 4,
        "description":description
    })
}

pub(in crate::toolkits) fn integer_property(description: &str) -> Value {
    json!({"type":"integer","description":description})
}

pub(in crate::toolkits) fn optional_integer_property(description: &str) -> Value {
    json!({"type":["integer","null"],"description":description})
}

pub(in crate::toolkits) fn optional_bool_property(description: &str) -> Value {
    json!({"type":["boolean","null"],"description":description})
}

pub(in crate::toolkits) fn optional_string_list_property(description: &str) -> Value {
    json!({
        "type":["array","null"],
        "items":{"type":"string"},
        "maxItems":256,
        "description":description
    })
}
