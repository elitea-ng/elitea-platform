use std::collections::HashSet;
use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{
    GcpApi, GcpClient, GcpClientError, parse_method, validate_api_url, validate_optional_args,
    validate_scopes,
};
use super::config::{GcpConfigError, GcpToolkitConfig};

const EXECUTE_REQUEST: &str = "execute_request";
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_JSON_STRING_BYTES: usize = 64 * 1_024;
const MAX_JSON_STRING_SCHEMA_CHARS: usize = MAX_JSON_STRING_BYTES / 4;
const MAX_OPTION_NODES: usize = 8 * 1_024;
const MAX_OPTION_DEPTH: usize = 32;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum GcpToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Safe construction failure for the complete one-tool GCP family.
pub(crate) struct GcpToolsetError {
    code: GcpToolsetErrorCode,
}

impl GcpToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> GcpToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for GcpToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GcpToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for GcpToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            GcpToolsetErrorCode::InvalidConfiguration => "the GCP toolkit configuration is invalid",
            GcpToolsetErrorCode::ResourceExhausted => {
                "the GCP toolkit configuration exceeds its approved limit"
            }
            GcpToolsetErrorCode::UnsupportedSelection => {
                "the selected GCP tool profile is unsupported"
            }
            GcpToolsetErrorCode::Client => "the GCP client could not be created",
            GcpToolsetErrorCode::InvalidDefinition => "the GCP ADK tool definition is invalid",
        })
    }
}

impl std::error::Error for GcpToolsetError {}

impl From<GcpConfigError> for GcpToolsetError {
    fn from(source: GcpConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::GcpConfigErrorCode::InvalidConfiguration => {
                    GcpToolsetErrorCode::InvalidConfiguration
                }
                super::config::GcpConfigErrorCode::ResourceExhausted => {
                    GcpToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<GcpClientError> for GcpToolsetError {
    fn from(_: GcpClientError) -> Self {
        Self {
            code: GcpToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for GcpToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: GcpToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the complete capability-disabled GCP toolset.
///
/// `execute_request` can read, create, update, delete, or invoke action
/// endpoints. Its source `execute` group is metadata only; sensitivity and
/// effect ownership remain independent admission responsibilities.
pub(crate) fn build_gcp_toolset(
    toolkit_name: &str,
    config: GcpToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, GcpToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn GcpApi> = Arc::new(GcpClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), GcpToolsetError> {
    if selected.iter().any(|name| name.as_ref() != EXECUTE_REQUEST) {
        return Err(GcpToolsetError {
            code: GcpToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn GcpApi>,
) -> Result<BasicToolset, GcpToolsetError> {
    let tools = [GcpToolKind::ExecuteRequest]
        .into_iter()
        .filter(|_| selected.is_empty() || selected.iter().any(|name| name == GcpToolKind::name()))
        .map(|kind| Arc::new(GcpTool::new(kind, toolkit_name, Arc::clone(client))) as Arc<dyn Tool>)
        .collect::<Vec<_>>();
    admit_materialized_toolset(toolkit_name, "gcp", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn GcpApi>,
) -> Result<BasicToolset, GcpToolsetError> {
    if selected.iter().any(|name| name != EXECUTE_REQUEST) {
        return Err(GcpToolsetError {
            code: GcpToolsetErrorCode::UnsupportedSelection,
        });
    }
    build_with_api(toolkit_name, selected, policy, client)
}

#[cfg(test)]
pub(in crate::toolkits) const fn test_catalog() -> [(&'static str, &'static str); 1] {
    [(EXECUTE_REQUEST, "execute")]
}

#[derive(Clone, Copy)]
enum GcpToolKind {
    ExecuteRequest,
}

impl GcpToolKind {
    const fn name() -> &'static str {
        EXECUTE_REQUEST
    }

    const fn action() -> &'static str {
        "Call one Google Cloud REST endpoint with the configured service account. method accepts any HTTP token; GET, HEAD, and OPTIONS are reads, while other methods can create, update, patch, delete, or trigger actions. scopes contains 1 to 32 exact unique https://www.googleapis.com/auth/... scope URLs. url must use HTTPS on googleapis.com or a subdomain. optional_args accepts only params, headers, json, and data. The client performs one OAuth token exchange and one API request with verified TLS, no redirects, and no automatic retry. JSON output is bounded to 512 KiB; an empty 2xx uses the source-compatible success string, and HTTP 202 means accepted rather than completed. Confidential reads may independently require approval. After an unknown effect outcome, reconcile Google Cloud state before retrying because a retry can repeat the effect."
    }
}

struct GcpTool {
    kind: GcpToolKind,
    client: Arc<dyn GcpApi>,
    description: Box<str>,
}

impl GcpTool {
    fn new(kind: GcpToolKind, toolkit_name: &str, client: Arc<dyn GcpApi>) -> Self {
        let action = GcpToolKind::action();
        let prefix_bytes = "Toolkit: \n".len();
        let name_budget = MAX_DESCRIPTION_BYTES.saturating_sub(prefix_bytes + action.len());
        let bounded_name = truncate_utf8(toolkit_name, name_budget);
        Self {
            kind,
            client,
            description: format!("Toolkit: {bounded_name}\n{action}").into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for GcpTool {
    fn name(&self) -> &str {
        GcpToolKind::name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        false
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(execute_schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_json(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        reject_unknown_keys(arguments, &["method", "scopes", "url", "optional_args"])?;
        let method = required_string(arguments, "method")?;
        let url = required_string(arguments, "url")?;
        let scopes = required_string_list(arguments, "scopes")?;
        let optional_args = optional_object(arguments.get("optional_args"))?;
        parse_method(method).map_err(GcpClientError::into_adk)?;
        validate_scopes(&scopes).map_err(GcpClientError::into_adk)?;
        validate_api_url(url).map_err(GcpClientError::into_adk)?;
        validate_optional_args(&optional_args).map_err(GcpClientError::into_adk)?;
        self.client
            .execute(method, &scopes, url, &optional_args)
            .await
            .map_err(GcpClientError::into_adk)
    }
}

fn execute_schema() -> Value {
    json!({
        "title": "ExecuteRequestModel",
        "type": "object",
        "properties": {
            "method": {
                "type": "string",
                "minLength": 1,
                "maxLength": 32,
                "pattern": "^[!#$%&'*+.^_`|~0-9A-Za-z-]+$",
                "description": "HTTP method token for the Google API request, for example GET, POST, PUT, PATCH, or DELETE. Custom RFC method tokens are accepted. GET, HEAD, and OPTIONS are reads; every other method is a remote effect."
            },
            "scopes": {
                "type": "array",
                "minItems": 1,
                "maxItems": 32,
                "uniqueItems": true,
                "items": {
                    "type": "string",
                    "minLength": 1,
                    "maxLength": 256,
                    "pattern": "^https://www\\.googleapis\\.com/auth/[A-Za-z0-9._/-]+$"
                },
                "description": "One to 32 exact, unique Google OAuth scope URLs, each at most 256 UTF-8 bytes, for example https://www.googleapis.com/auth/cloud-platform.read-only. Request the narrowest scopes required by this call."
            },
            "url": {
                "type": "string",
                "minLength": 1,
                "maxLength": 2048,
                "pattern": "^https://([A-Za-z0-9-]+\\.)*googleapis\\.com/",
                "description": "Complete HTTPS Google API URL on googleapis.com or a subdomain, using at most 8192 UTF-8 bytes; for example https://compute.googleapis.com/compute/v1/projects/example/zones. Userinfo, custom ports, fragments, traversal, decoded separators, malformed escapes, and non-Google origins are rejected."
            },
            "optional_args": optional_args_schema()
        },
        "required": ["method", "scopes", "url"],
        "additionalProperties": false
    })
}

fn optional_args_schema() -> Value {
    json!({
        "anyOf": [
            {
                "type": "object",
                "maxProperties": 4,
                "properties": {
                    "params": {
                        "type": "object",
                        "maxProperties": 256,
                        "description": "Optional query parameters. Scalar values become one pair; arrays become repeated pairs."
                    },
                    "headers": {
                        "type": "object",
                        "maxProperties": 64,
                        "additionalProperties": {"type": "string", "maxLength": 2048},
                        "description": "Optional request headers. Authorization, Host, length, proxy, and hop-by-hop headers are rejected."
                    },
                    "json": {
                        "description": "Optional JSON request body, serialized within the 240 KiB body limit. Do not combine with data."
                    },
                    "data": {
                        "anyOf": [
                            {"type": "string", "maxLength": MAX_JSON_STRING_SCHEMA_CHARS},
                            {"type": "object", "maxProperties": 256}
                        ],
                        "description": "Optional UTF-8 request body string or form object, serialized within 240 KiB. Do not combine with json."
                    }
                },
                "additionalProperties": false
            },
            {"type": "null"}
        ],
        "default": null,
        "description": "Optional request options. Only params, headers, json, and data are accepted; filesystem, client, proxy, certificate, redirect, retry, timeout, streaming, and alternate-auth options are unavailable."
    })
}

fn optional_object(value: Option<&Value>) -> adk_core::Result<Map<String, Value>> {
    let Some(value) = value else {
        return Ok(Map::new());
    };
    if value.is_null() {
        return Ok(Map::new());
    }
    validate_option_tree(value)?;
    value.as_object().cloned().ok_or_else(invalid_arguments)
}

fn required_string_list(
    arguments: &Map<String, Value>,
    name: &str,
) -> adk_core::Result<Vec<String>> {
    let values = arguments
        .get(name)
        .and_then(Value::as_array)
        .ok_or_else(invalid_arguments)?;
    let mut seen = HashSet::with_capacity(values.len());
    let mut result = Vec::with_capacity(values.len());
    for value in values {
        let value = value.as_str().ok_or_else(invalid_arguments)?;
        if !seen.insert(value) {
            return Err(invalid_arguments());
        }
        result.push(value.to_owned());
    }
    Ok(result)
}

fn validate_option_tree(value: &Value) -> adk_core::Result<()> {
    let mut nodes = 0usize;
    let mut stack = vec![(value, 0usize)];
    while let Some((node, depth)) = stack.pop() {
        nodes = nodes.checked_add(1).ok_or_else(resource_exhausted)?;
        if nodes > MAX_OPTION_NODES || depth > MAX_OPTION_DEPTH {
            return Err(resource_exhausted());
        }
        match node {
            Value::String(value) if value.len() > MAX_JSON_STRING_BYTES => {
                return Err(resource_exhausted());
            }
            Value::Array(values) => {
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                if values.keys().any(|key| key.len() > MAX_JSON_STRING_BYTES) {
                    return Err(resource_exhausted());
                }
                stack.extend(values.values().map(|value| (value, depth + 1)));
            }
            Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => {}
        }
    }
    Ok(())
}

fn validate_json(value: &Value) -> adk_core::Result<()> {
    if serde_json::to_vec(value)
        .map_err(|_| invalid_arguments())?
        .len()
        > MAX_ARGUMENT_BYTES
    {
        return Err(resource_exhausted());
    }
    validate_option_tree(value)
}

fn required_string<'a>(arguments: &'a Map<String, Value>, name: &str) -> adk_core::Result<&'a str> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_arguments)
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> adk_core::Result<()> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
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
        "gcp.arguments.invalid",
        "the GCP tool arguments are invalid",
    )
}

fn resource_exhausted() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "gcp.arguments.resource_exhausted",
        "the GCP tool arguments exceed the approved limit",
    )
}
