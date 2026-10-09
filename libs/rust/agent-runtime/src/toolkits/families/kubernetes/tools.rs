use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{
    KubernetesApi, KubernetesClient, KubernetesClientError, parse_method, validate_suburl,
};
use super::config::{KubernetesConfigError, KubernetesToolkitConfig};

const EXECUTE: &str = "execute_kubernetes";
const HEALTHCHECK: &str = "kubernetes_integration_healthcheck";
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_JSON_STRING_BYTES: usize = 64 * 1_024;
const MAX_JSON_STRING_SCHEMA_CHARS: usize = MAX_JSON_STRING_BYTES / 4;
const MAX_BODY_BYTES: usize = 240 * 1_024;
const MAX_OPTION_NODES: usize = 8 * 1_024;
const MAX_OPTION_DEPTH: usize = 32;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum KubernetesToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Safe construction failure for the complete two-tool Kubernetes family.
pub struct KubernetesToolsetError {
    code: KubernetesToolsetErrorCode,
}

impl KubernetesToolsetError {
    #[must_use]
    pub const fn code(&self) -> KubernetesToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for KubernetesToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("KubernetesToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for KubernetesToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            KubernetesToolsetErrorCode::InvalidConfiguration => {
                "the Kubernetes toolkit configuration is invalid"
            }
            KubernetesToolsetErrorCode::ResourceExhausted => {
                "the Kubernetes toolkit configuration exceeds its approved limit"
            }
            KubernetesToolsetErrorCode::UnsupportedSelection => {
                "the selected Kubernetes tool profile is unsupported"
            }
            KubernetesToolsetErrorCode::Client => "the Kubernetes client could not be created",
            KubernetesToolsetErrorCode::InvalidDefinition => {
                "the Kubernetes ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for KubernetesToolsetError {}

impl From<KubernetesConfigError> for KubernetesToolsetError {
    fn from(source: KubernetesConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::KubernetesConfigErrorCode::InvalidConfiguration => {
                    KubernetesToolsetErrorCode::InvalidConfiguration
                }
                super::config::KubernetesConfigErrorCode::ResourceExhausted => {
                    KubernetesToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<KubernetesClientError> for KubernetesToolsetError {
    fn from(_: KubernetesClientError) -> Self {
        Self {
            code: KubernetesToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for KubernetesToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: KubernetesToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the complete capability-disabled Kubernetes toolset.
///
/// The generic operation can read, create, update, delete, or invoke action
/// subresources. Its `execute` group is metadata only; sensitivity and effect
/// ownership remain independent admission responsibilities.
pub fn build_kubernetes_toolset(
    toolkit_name: &str,
    config: KubernetesToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, KubernetesToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let cluster_url = config.cluster_url().to_owned();
    let client: Arc<dyn KubernetesApi> = Arc::new(KubernetesClient::new(config)?);
    build_with_api(toolkit_name, &cluster_url, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), KubernetesToolsetError> {
    if selected
        .iter()
        .any(|name| !matches!(name.as_ref(), EXECUTE | HEALTHCHECK))
    {
        return Err(KubernetesToolsetError {
            code: KubernetesToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    cluster_url: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn KubernetesApi>,
) -> Result<BasicToolset, KubernetesToolsetError> {
    let kinds = [KubernetesToolKind::Execute, KubernetesToolKind::Healthcheck];
    let tools = kinds
        .into_iter()
        .filter(|kind| selected.is_empty() || selected.iter().any(|name| name == kind.name()))
        .map(|kind| {
            Arc::new(KubernetesTool::new(
                kind,
                toolkit_name,
                cluster_url,
                Arc::clone(client),
            )) as Arc<dyn Tool>
        })
        .collect::<Vec<_>>();
    admit_materialized_toolset(toolkit_name, "kubernetes", policy, tools).map_err(Into::into)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_build_with_api(
    toolkit_name: &str,
    cluster_url: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn KubernetesApi>,
) -> Result<BasicToolset, KubernetesToolsetError> {
    if selected
        .iter()
        .any(|name| !matches!(name.as_str(), EXECUTE | HEALTHCHECK))
    {
        return Err(KubernetesToolsetError {
            code: KubernetesToolsetErrorCode::UnsupportedSelection,
        });
    }
    build_with_api(toolkit_name, cluster_url, selected, policy, client)
}

#[cfg(any(test, feature = "test-support"))]
pub const fn test_catalog() -> [(&'static str, &'static str); 2] {
    [(EXECUTE, "execute"), (HEALTHCHECK, "read")]
}

#[derive(Clone, Copy)]
enum KubernetesToolKind {
    Execute,
    Healthcheck,
}

impl KubernetesToolKind {
    const fn name(self) -> &'static str {
        match self {
            Self::Execute => EXECUTE,
            Self::Healthcheck => HEALTHCHECK,
        }
    }

    const fn action(self) -> &'static str {
        match self {
            Self::Execute => {
                "Call one Kubernetes REST endpoint on the configured cluster. method accepts any bounded HTTP token; GET, HEAD, and OPTIONS are reads, while other methods can create, update, patch, delete, or trigger actions. suburl starts with / and may include a query, for example /api/v1/namespaces/default/pods?limit=50. body is an optional JSON object or JSON-object string; headers is an optional string map and cannot replace Authorization, Host, length, or hop-by-hop headers. The UTF-8 response body is returned up to a 512 KiB serialized result; provider 202 means accepted, not necessarily completed. The client makes one verified-TLS request without automatic retry. Reads can expose confidential cluster state and may independently require approval. After an unknown effect outcome, reconcile cluster state before retrying because a retry can repeat the effect."
            }
            Self::Healthcheck => {
                "Check the configured Kubernetes credential and cluster with one read-only GET /version request over verified TLS. Returns [true,\"\"] only for a successful JSON response, or [false,<stable reason>] on failure. It does not modify cluster state and does not prove authorization for other API groups, namespaces, or resources."
            }
        }
    }

    const fn read_only(self) -> bool {
        matches!(self, Self::Healthcheck)
    }
}

struct KubernetesTool {
    kind: KubernetesToolKind,
    client: Arc<dyn KubernetesApi>,
    cluster_url: Box<str>,
    description: Box<str>,
}

impl KubernetesTool {
    fn new(
        kind: KubernetesToolKind,
        toolkit_name: &str,
        cluster_url: &str,
        client: Arc<dyn KubernetesApi>,
    ) -> Self {
        let action = kind.action();
        let prefix_bytes = "Toolkit: \n".len();
        let name_budget = MAX_DESCRIPTION_BYTES.saturating_sub(prefix_bytes + action.len());
        let bounded_name = truncate_utf8(toolkit_name, name_budget);
        Self {
            kind,
            client,
            cluster_url: cluster_url.into(),
            description: format!("Toolkit: {bounded_name}\n{action}").into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for KubernetesTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        self.kind.read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.kind.read_only()
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(match self.kind {
            KubernetesToolKind::Execute => execute_schema(),
            KubernetesToolKind::Healthcheck => {
                empty_schema("KubernetesIntegrationHealthcheckModel")
            }
        })
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_json(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        match self.kind {
            KubernetesToolKind::Execute => {
                reject_unknown_keys(arguments, &["method", "suburl", "body", "headers"])?;
                let method = required_string(arguments, "method")?;
                let suburl = required_string(arguments, "suburl")?;
                parse_method(method).map_err(KubernetesClientError::into_adk)?;
                validate_suburl(&self.cluster_url, suburl)
                    .map_err(KubernetesClientError::into_adk)?;
                let body = parse_object(arguments.get("body"), MAX_BODY_BYTES)?;
                let headers =
                    parse_object(arguments.get("headers"), 32 * 1_024)?.unwrap_or_default();
                validate_header_values(&headers)?;
                self.client
                    .execute(method, suburl, body.as_ref(), &headers)
                    .await
                    .map_err(KubernetesClientError::into_adk)
            }
            KubernetesToolKind::Healthcheck => {
                reject_unknown_keys(arguments, &[])?;
                Ok(self.client.healthcheck().await)
            }
        }
    }
}

fn execute_schema() -> Value {
    json!({
        "title": "ExecuteToolModel",
        "type": "object",
        "properties": {
            "method": {
                "type": "string",
                "minLength": 1,
                "maxLength": 32,
                "pattern": "^[!#$%&'*+.^_`|~0-9A-Za-z-]+$",
                "description": "HTTP method token for the Kubernetes request, for example GET, POST, PUT, PATCH, or DELETE. Custom RFC method tokens are accepted. GET, HEAD, and OPTIONS are reads; all other methods are remote effects."
            },
            "suburl": {
                "type": "string",
                "minLength": 1,
                "maxLength": 2048,
                "pattern": "^/($|[^/])",
                "description": "Relative Kubernetes API path on the configured cluster, beginning with one slash and using at most 8192 UTF-8 bytes; for example /api/v1/namespaces/default/pods?limit=50. Queries are allowed. Alternate origins, network paths, fragments, traversal, decoded separators, and malformed escapes are rejected."
            },
            "body": object_or_string_schema(
                "Optional JSON object, or JSON-object string up to 65536 UTF-8 bytes, serialized to at most 240 KiB. Use the Kubernetes object, merge-patch, strategic-merge-patch, or JSON-patch shape required by the chosen endpoint; array-only JSON Patch is outside the source schema. Omit or use null for no body."
            ),
            "headers": headers_schema()
        },
        "required": ["method", "suburl"],
        "additionalProperties": false
    })
}

fn object_or_string_schema(description: &str) -> Value {
    json!({
        "anyOf": [
            {"type": "string", "maxLength": MAX_JSON_STRING_SCHEMA_CHARS},
            {"type": "object"},
            {"type": "null"}
        ],
        "default": null,
        "description": description
    })
}

fn headers_schema() -> Value {
    json!({
        "anyOf": [
            {"type": "string", "maxLength": MAX_JSON_STRING_SCHEMA_CHARS},
            {
                "type": "object",
                "maxProperties": 64,
                "additionalProperties": {"type": "string", "maxLength": 2048}
            },
            {"type": "null"}
        ],
        "default": null,
        "description": "Optional JSON string map, or JSON-object string up to 65536 UTF-8 bytes, with at most 64 headers, 8192 UTF-8 bytes per value, and 32 KiB total. Use Content-Type to select a Kubernetes patch media type when needed. Authorization, Host, length, proxy, and hop-by-hop headers are rejected."
    })
}

fn empty_schema(title: &str) -> Value {
    json!({
        "title": title,
        "type": "object",
        "properties": {},
        "additionalProperties": false
    })
}

fn parse_object(
    value: Option<&Value>,
    max_serialized_bytes: usize,
) -> adk_core::Result<Option<Map<String, Value>>> {
    let Some(value) = value else {
        return Ok(None);
    };
    if value.is_null() {
        return Ok(None);
    }
    let parsed = if let Some(value) = value.as_str() {
        if value.len() > MAX_JSON_STRING_BYTES {
            return Err(resource_exhausted());
        }
        if value.is_empty() {
            return Ok(None);
        }
        serde_json::from_str(value).map_err(|_| invalid_arguments())?
    } else {
        value.clone()
    };
    validate_option_tree(&parsed)?;
    if serde_json::to_vec(&parsed)
        .map_err(|_| invalid_arguments())?
        .len()
        > max_serialized_bytes
    {
        return Err(resource_exhausted());
    }
    parsed
        .as_object()
        .cloned()
        .map(Some)
        .ok_or_else(invalid_arguments)
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

fn validate_header_values(headers: &Map<String, Value>) -> adk_core::Result<()> {
    if headers.len() > 64 || headers.values().any(|value| !value.is_string()) {
        return Err(invalid_arguments());
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
    Ok(())
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
        "kubernetes.arguments.invalid",
        "the Kubernetes tool arguments are invalid",
    )
}

fn resource_exhausted() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "kubernetes.arguments.resource_exhausted",
        "the Kubernetes tool arguments exceed the approved limit",
    )
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_parse_object(
    value: Option<&Value>,
    max_serialized_bytes: usize,
) -> adk_core::Result<Option<Map<String, Value>>> {
    parse_object(value, max_serialized_bytes)
}
