use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{AzureSearchApi, AzureSearchClient, AzureSearchClientError};
use super::config::{AzureSearchConfigError, AzureSearchToolkitConfig};

const TEXT_SEARCH: &str = "text_search";
const GET_DOCUMENT: &str = "get_document";
const DEFAULT_LIMIT: usize = 100;
const MAX_LIMIT: u64 = 100;
const MAX_SEARCH_TEXT_BYTES: usize = 64 * 1_024;
const MAX_DOCUMENT_ID_BYTES: usize = 4 * 1_024;
const MAX_ORDER_BY: usize = 32;
const MAX_ORDER_BY_BYTES: usize = 2 * 1_024;
const MAX_SELECTED_FIELDS: usize = 128;
const MAX_SELECTED_FIELD_BYTES: usize = 512;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AzureSearchToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Safe construction failure for the complete Azure Search read family.
pub struct AzureSearchToolsetError {
    code: AzureSearchToolsetErrorCode,
}

impl AzureSearchToolsetError {
    #[must_use]
    pub const fn code(&self) -> AzureSearchToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for AzureSearchToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AzureSearchToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AzureSearchToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AzureSearchToolsetErrorCode::InvalidConfiguration => {
                "the Azure Search toolkit configuration is invalid"
            }
            AzureSearchToolsetErrorCode::ResourceExhausted => {
                "the Azure Search toolkit configuration exceeds its approved limit"
            }
            AzureSearchToolsetErrorCode::UnsupportedSelection => {
                "the selected Azure Search tool profile is not supported"
            }
            AzureSearchToolsetErrorCode::Client => "the Azure Search client could not be created",
            AzureSearchToolsetErrorCode::InvalidDefinition => {
                "the Azure Search ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for AzureSearchToolsetError {}

impl From<AzureSearchConfigError> for AzureSearchToolsetError {
    fn from(source: AzureSearchConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::AzureSearchConfigErrorCode::InvalidConfiguration => {
                    AzureSearchToolsetErrorCode::InvalidConfiguration
                }
                super::config::AzureSearchConfigErrorCode::ResourceExhausted => {
                    AzureSearchToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<AzureSearchClientError> for AzureSearchToolsetError {
    fn from(_: AzureSearchClientError) -> Self {
        Self {
            code: AzureSearchToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for AzureSearchToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: AzureSearchToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the complete capability-disabled Azure Search read toolset.
///
/// Both tools retain their current SDK `read` classification. A trusted
/// deployment may independently classify any concrete tool as sensitive; the
/// shared durable HITL wrapper, rather than this family, owns that interrupt.
pub fn build_azure_search_read_only_toolset(
    toolkit_name: &str,
    config: AzureSearchToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, AzureSearchToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn AzureSearchApi> = Arc::new(AzureSearchClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), AzureSearchToolsetError> {
    if selected
        .iter()
        .any(|name| !matches!(name.as_ref(), TEXT_SEARCH | GET_DOCUMENT))
    {
        return Err(AzureSearchToolsetError {
            code: AzureSearchToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn AzureSearchApi>,
) -> Result<BasicToolset, AzureSearchToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(2);
    for kind in [
        AzureSearchToolKind::TextSearch,
        AzureSearchToolKind::GetDocument,
    ] {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(AzureSearchTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "azure_search", policy, tools).map_err(Into::into)
}

#[derive(Clone, Copy)]
enum AzureSearchToolKind {
    TextSearch,
    GetDocument,
}

impl AzureSearchToolKind {
    const fn name(self) -> &'static str {
        match self {
            Self::TextSearch => TEXT_SEARCH,
            Self::GetDocument => GET_DOCUMENT,
        }
    }
}

struct AzureSearchTool {
    kind: AzureSearchToolKind,
    client: Arc<dyn AzureSearchApi>,
    description: Box<str>,
}

impl AzureSearchTool {
    fn new(kind: AzureSearchToolKind, toolkit_name: &str, client: Arc<dyn AzureSearchApi>) -> Self {
        let action = match kind {
            AzureSearchToolKind::TextSearch => {
                "Search the configured Azure AI Search index and return a bounded document list."
            }
            AzureSearchToolKind::GetDocument => {
                "Retrieve one document from the configured Azure AI Search index."
            }
        };
        let description = format!("Toolkit: {toolkit_name}\n{action}");
        Self {
            kind,
            client,
            description: description
                .chars()
                .take(MAX_DESCRIPTION_BYTES)
                .collect::<String>()
                .into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for AzureSearchTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(match self.kind {
            AzureSearchToolKind::TextSearch => text_search_schema(),
            AzureSearchToolKind::GetDocument => get_document_schema(),
        })
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        match self.kind {
            AzureSearchToolKind::TextSearch => {
                reject_unknown_keys(
                    arguments,
                    &["search_text", "limit", "order_by", "selected_fields"],
                )?;
                let search_text =
                    required_text(arguments, "search_text", MAX_SEARCH_TEXT_BYTES, true)?;
                let limit = optional_limit(arguments)?;
                let order_by =
                    optional_string_list(arguments, "order_by", MAX_ORDER_BY, MAX_ORDER_BY_BYTES)?;
                let selected_fields = optional_selected_fields(arguments, "selected_fields")?;
                self.client
                    .text_search(search_text, limit, &order_by, &selected_fields)
                    .await
                    .map_err(AzureSearchClientError::into_adk)
            }
            AzureSearchToolKind::GetDocument => {
                reject_unknown_keys(arguments, &["document_id", "selected_fields"])?;
                let document_id =
                    required_text(arguments, "document_id", MAX_DOCUMENT_ID_BYTES, false)?;
                let selected_fields = optional_selected_fields(arguments, "selected_fields")?;
                self.client
                    .get_document(document_id, &selected_fields)
                    .await
                    .map_err(AzureSearchClientError::into_adk)
            }
        }
    }
}

fn text_search_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "search_text": {
                "type": "string",
                "maxLength": MAX_SEARCH_TEXT_BYTES,
                "description": "Full-text query for the configured index. Use an empty string to match all documents."
            },
            "limit": {
                "anyOf": [
                    {"type": "integer", "const": -1},
                    {"type": "integer", "minimum": 1, "maximum": MAX_LIMIT},
                    {"type": "null"}
                ],
                "default": -1,
                "description": "Maximum results. Null or -1 uses the bounded Rust default of 100."
            },
            "order_by": {
                "type": ["array", "null"],
                "items": {"type": "string", "minLength": 1, "maxLength": MAX_ORDER_BY_BYTES},
                "maxItems": MAX_ORDER_BY,
                "default": null,
                "description": "Bounded Azure Search OData ordering expressions, for example `rating desc`, `search.score() desc`, or `geo.distance(location, geography'POINT(-122.1 47.6)') asc`."
            },
            "selected_fields": selected_fields_schema()
        },
        "required": ["search_text"],
        "additionalProperties": false
    })
}

fn get_document_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "document_id": {
                "type": "string",
                "minLength": 1,
                "maxLength": MAX_DOCUMENT_ID_BYTES,
                "description": "Key of the document to retrieve."
            },
            "selected_fields": selected_fields_schema()
        },
        "required": ["document_id"],
        "additionalProperties": false
    })
}

fn selected_fields_schema() -> Value {
    json!({
        "type": ["array", "null"],
        "items": {
            "type": "string",
            "minLength": 1,
            "maxLength": MAX_SELECTED_FIELD_BYTES,
            "pattern": "^(\\*|[A-Za-z_][A-Za-z0-9_]*(/[A-Za-z_][A-Za-z0-9_]*)*)$"
        },
        "maxItems": MAX_SELECTED_FIELDS,
        "default": null,
        "description": "Fields to retrieve; use field paths such as `title` or `address/city`, or `*` for all retrievable fields. Omit or use an empty list for the provider default."
    })
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> Result<(), AdkError> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn required_text<'a>(
    arguments: &'a Map<String, Value>,
    key: &str,
    maximum: usize,
    allow_empty: bool,
) -> Result<&'a str, AdkError> {
    arguments
        .get(key)
        .and_then(Value::as_str)
        .filter(|value| {
            value.len() <= maximum
                && (allow_empty || !value.is_empty())
                && !value.chars().any(char::is_control)
        })
        .ok_or_else(invalid_arguments)
}

fn optional_limit(arguments: &Map<String, Value>) -> Result<usize, AdkError> {
    match arguments.get("limit") {
        None | Some(Value::Null) => Ok(DEFAULT_LIMIT),
        Some(value) if value.as_i64() == Some(-1) => Ok(DEFAULT_LIMIT),
        Some(value) => value
            .as_u64()
            .filter(|value| (1..=MAX_LIMIT).contains(value))
            .and_then(|value| usize::try_from(value).ok())
            .ok_or_else(invalid_arguments),
    }
}

fn optional_string_list(
    arguments: &Map<String, Value>,
    key: &str,
    maximum_items: usize,
    maximum_item_bytes: usize,
) -> Result<Vec<String>, AdkError> {
    let Some(value) = arguments.get(key) else {
        return Ok(Vec::new());
    };
    if value.is_null() {
        return Ok(Vec::new());
    }
    let values = value.as_array().ok_or_else(invalid_arguments)?;
    if values.len() > maximum_items {
        return Err(invalid_arguments());
    }
    values
        .iter()
        .map(|value| {
            value
                .as_str()
                .filter(|value| {
                    !value.trim().is_empty()
                        && value.len() <= maximum_item_bytes
                        && !value.chars().any(char::is_control)
                })
                .map(ToOwned::to_owned)
                .ok_or_else(invalid_arguments)
        })
        .collect()
}

fn optional_selected_fields(
    arguments: &Map<String, Value>,
    key: &str,
) -> Result<Vec<String>, AdkError> {
    let values = optional_string_list(
        arguments,
        key,
        MAX_SELECTED_FIELDS,
        MAX_SELECTED_FIELD_BYTES,
    )?;
    if values.iter().any(|field| !valid_selected_field(field)) {
        return Err(invalid_arguments());
    }
    Ok(values)
}

fn valid_selected_field(value: &str) -> bool {
    value == "*"
        || value.split('/').all(|segment| {
            let mut characters = segment.chars();
            characters
                .next()
                .is_some_and(|character| character.is_ascii_alphabetic() || character == '_')
                && characters.all(|character| character.is_ascii_alphanumeric() || character == '_')
        })
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "azure_search.arguments.invalid",
        "the Azure Search tool arguments are invalid",
    )
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn AzureSearchApi>,
) -> Result<BasicToolset, AzureSearchToolsetError> {
    build_with_api(toolkit_name, selected, policy, client)
}
