use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{
    ElasticApi, ElasticClient, ElasticClientError, validate_index, validate_query,
};
use super::config::{ElasticConfigError, ElasticToolkitConfig};

const SEARCH_ELASTIC_INDEX: &str = "search_elastic_index";
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_QUERY_STRING_BYTES: usize = 64 * 1_024;
const MAX_QUERY_SCHEMA_CHARS: usize = MAX_QUERY_STRING_BYTES / 4;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ElasticToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Safe construction failure for the complete Elasticsearch read family.
pub(crate) struct ElasticToolsetError {
    code: ElasticToolsetErrorCode,
}

impl ElasticToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> ElasticToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for ElasticToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ElasticToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ElasticToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ElasticToolsetErrorCode::InvalidConfiguration => {
                "the Elasticsearch toolkit configuration is invalid"
            }
            ElasticToolsetErrorCode::ResourceExhausted => {
                "the Elasticsearch toolkit configuration exceeds its approved limit"
            }
            ElasticToolsetErrorCode::UnsupportedSelection => {
                "the selected Elasticsearch tool profile is unsupported"
            }
            ElasticToolsetErrorCode::Client => "the Elasticsearch client could not be created",
            ElasticToolsetErrorCode::InvalidDefinition => {
                "the Elasticsearch ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for ElasticToolsetError {}

impl From<ElasticConfigError> for ElasticToolsetError {
    fn from(source: ElasticConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::ElasticConfigErrorCode::InvalidConfiguration => {
                    ElasticToolsetErrorCode::InvalidConfiguration
                }
                super::config::ElasticConfigErrorCode::ResourceExhausted => {
                    ElasticToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<ElasticClientError> for ElasticToolsetError {
    fn from(_: ElasticClientError) -> Self {
        Self {
            code: ElasticToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for ElasticToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: ElasticToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the complete capability-disabled Elasticsearch read toolset.
pub(crate) fn build_elastic_toolset(
    toolkit_name: &str,
    config: ElasticToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, ElasticToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn ElasticApi> = Arc::new(ElasticClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), ElasticToolsetError> {
    if selected
        .iter()
        .any(|name| name.as_ref() != SEARCH_ELASTIC_INDEX)
    {
        return Err(ElasticToolsetError {
            code: ElasticToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn ElasticApi>,
) -> Result<BasicToolset, ElasticToolsetError> {
    let include = selected.is_empty() || selected.iter().any(|name| name == SEARCH_ELASTIC_INDEX);
    let tools: Vec<Arc<dyn Tool>> = if include {
        vec![Arc::new(ElasticTool::new(toolkit_name, Arc::clone(client)))]
    } else {
        Vec::new()
    };
    admit_materialized_toolset(toolkit_name, "elastic", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn ElasticApi>,
) -> Result<BasicToolset, ElasticToolsetError> {
    if selected.iter().any(|name| name != SEARCH_ELASTIC_INDEX) {
        return Err(ElasticToolsetError {
            code: ElasticToolsetErrorCode::UnsupportedSelection,
        });
    }
    build_with_api(toolkit_name, selected, policy, client)
}

#[cfg(test)]
pub(in crate::toolkits) const fn test_catalog() -> [(&'static str, &'static str); 1] {
    [(SEARCH_ELASTIC_INDEX, "read")]
}

struct ElasticTool {
    client: Arc<dyn ElasticApi>,
    description: Box<str>,
}

impl ElasticTool {
    fn new(toolkit_name: &str, client: Arc<dyn ElasticApi>) -> Self {
        let action = "Search one Elasticsearch or REST-compatible OpenSearch index, data stream, alias, wildcard, or comma-separated expression with a Query DSL JSON object. The index parameter selects the target, for example logs-2026.08-* or logs-current,logs-archive. query must be a JSON-object string; size defaults to 10 and is limited to 100, while from is limited to 10000. The tool performs one read-only POST /{index}/_search request over verified TLS without redirect, automatic retry, scroll, or continuation fetching. It returns the complete first bounded search response object, including hits, aggregations, took, and shard metadata when provided, up to 512 KiB. Queries and results can expose confidential indexed data and may independently require approval. Broad wildcards, scripts, runtime fields, and large aggregations can be expensive; use the narrowest index and query that answer the question.";
        let prefix_bytes = "Toolkit: \n".len();
        let name_budget = MAX_DESCRIPTION_BYTES.saturating_sub(prefix_bytes + action.len());
        let bounded_name = truncate_utf8(toolkit_name, name_budget);
        Self {
            client,
            description: format!("Toolkit: {bounded_name}\n{action}").into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for ElasticTool {
    fn name(&self) -> &str {
        SEARCH_ELASTIC_INDEX
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
        Some(parameters_schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        if serde_json::to_vec(&arguments)
            .map_err(|_| invalid_arguments())?
            .len()
            > MAX_ARGUMENT_BYTES
        {
            return Err(resource_exhausted());
        }
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        reject_unknown_keys(arguments)?;
        let index = required_string(arguments, "index")?;
        let query = required_string(arguments, "query")?;
        if query.len() > MAX_QUERY_STRING_BYTES {
            return Err(resource_exhausted());
        }
        let query: Value = serde_json::from_str(query).map_err(|_| invalid_arguments())?;
        validate_index(index).map_err(ElasticClientError::into_adk)?;
        validate_query(&query).map_err(ElasticClientError::into_adk)?;
        self.client
            .search(index, &query)
            .await
            .map_err(ElasticClientError::into_adk)
    }
}

fn parameters_schema() -> Value {
    json!({
        "title": "SearchElasticIndexModel",
        "type": "object",
        "properties": {
            "index": {
                "type": "string",
                "minLength": 1,
                "maxLength": 1024,
                "pattern": "^[A-Za-z0-9][A-Za-z0-9._*?,-]*$",
                "description": "Required Elasticsearch or OpenSearch index, data-stream, or alias expression, at most 1024 ASCII bytes; for example logs-2026.08-* or logs-current,logs-archive. Comma-separated expressions and suffix/infix * or ? wildcards are supported. Empty parts, path separators, traversal, date-math syntax, remote-cluster colons, and expressions beginning with -, _, +, *, or ? are rejected."
            },
            "query": {
                "type": "string",
                "minLength": 2,
                "maxLength": MAX_QUERY_SCHEMA_CHARS,
                "description": "Required JSON-encoded Elasticsearch or OpenSearch Query DSL object, not an array or scalar, using at most 64 KiB of UTF-8 input. Example: {\"size\":10,\"query\":{\"match\":{\"message\":\"timeout\"}}}. Use only clauses supported by the configured cluster because the products have diverged since Elasticsearch 7.10.2. size defaults to 10 and is limited to 100; from is limited to 10000. The tool does not fetch scroll or continuation pages."
            }
        },
        "required": ["index", "query"],
        "additionalProperties": false
    })
}

fn required_string<'a>(arguments: &'a Map<String, Value>, name: &str) -> adk_core::Result<&'a str> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_arguments)
}

fn reject_unknown_keys(arguments: &Map<String, Value>) -> adk_core::Result<()> {
    if arguments
        .keys()
        .any(|key| !matches!(key.as_str(), "index" | "query"))
    {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn truncate_utf8(value: &str, max_bytes: usize) -> &str {
    if value.len() <= max_bytes {
        return value;
    }
    let mut end = max_bytes;
    while end != 0 && !value.is_char_boundary(end) {
        end -= 1;
    }
    &value[..end]
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "elastic.arguments.invalid",
        "the Elasticsearch tool arguments are invalid",
    )
}

fn resource_exhausted() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "elastic.arguments.resource_exhausted",
        "the Elasticsearch tool arguments exceed the approved limit",
    )
}
