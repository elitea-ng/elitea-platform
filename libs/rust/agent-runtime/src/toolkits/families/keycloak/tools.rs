use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{
    KeycloakApi, KeycloakClient, KeycloakClientError, parse_method, validate_relative_url,
};
use super::config::{KeycloakConfigError, KeycloakToolkitConfig};

const EXECUTE: &str = "execute";
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_PARAMETER_STRING_BYTES: usize = 64 * 1_024;
const MAX_PARAMETER_NODES: usize = 8 * 1_024;
const MAX_PARAMETER_DEPTH: usize = 32;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum KeycloakToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Safe construction failure for the complete one-tool Keycloak family.
pub(crate) struct KeycloakToolsetError {
    code: KeycloakToolsetErrorCode,
}

impl KeycloakToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> KeycloakToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for KeycloakToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("KeycloakToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for KeycloakToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            KeycloakToolsetErrorCode::InvalidConfiguration => {
                "the Keycloak toolkit configuration is invalid"
            }
            KeycloakToolsetErrorCode::ResourceExhausted => {
                "the Keycloak toolkit configuration exceeds its approved limit"
            }
            KeycloakToolsetErrorCode::UnsupportedSelection => {
                "the selected Keycloak tool profile is unsupported"
            }
            KeycloakToolsetErrorCode::Client => "the Keycloak client could not be created",
            KeycloakToolsetErrorCode::InvalidDefinition => {
                "the Keycloak ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for KeycloakToolsetError {}

impl From<KeycloakConfigError> for KeycloakToolsetError {
    fn from(source: KeycloakConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::KeycloakConfigErrorCode::InvalidConfiguration => {
                    KeycloakToolsetErrorCode::InvalidConfiguration
                }
                super::config::KeycloakConfigErrorCode::ResourceExhausted => {
                    KeycloakToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<KeycloakClientError> for KeycloakToolsetError {
    fn from(_: KeycloakClientError) -> Self {
        Self {
            code: KeycloakToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for KeycloakToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: KeycloakToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the complete capability-disabled Keycloak toolset.
///
/// `execute` can perform reads, writes, deletes, and action endpoints. Its
/// source `execute` group is metadata only; sensitivity and effect ownership
/// remain independent admission responsibilities.
pub(crate) fn build_keycloak_toolset(
    toolkit_name: &str,
    config: KeycloakToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, KeycloakToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn KeycloakApi> = Arc::new(KeycloakClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), KeycloakToolsetError> {
    if selected.iter().any(|name| name.as_ref() != EXECUTE) {
        return Err(KeycloakToolsetError {
            code: KeycloakToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn KeycloakApi>,
) -> Result<BasicToolset, KeycloakToolsetError> {
    let include = selected.is_empty() || selected.iter().any(|name| name == EXECUTE);
    let tools: Vec<Arc<dyn Tool>> = if include {
        vec![Arc::new(KeycloakTool::new(
            toolkit_name,
            Arc::clone(client),
        ))]
    } else {
        Vec::new()
    };
    admit_materialized_toolset(toolkit_name, "keycloak", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn KeycloakApi>,
) -> Result<BasicToolset, KeycloakToolsetError> {
    if selected.iter().any(|name| name != EXECUTE) {
        return Err(KeycloakToolsetError {
            code: KeycloakToolsetErrorCode::UnsupportedSelection,
        });
    }
    build_with_api(toolkit_name, selected, policy, client)
}

#[cfg(test)]
pub(in crate::toolkits) const fn test_catalog() -> [(&'static str, &'static str); 1] {
    [(EXECUTE, "execute")]
}

struct KeycloakTool {
    client: Arc<dyn KeycloakApi>,
    description: Box<str>,
}

impl KeycloakTool {
    fn new(toolkit_name: &str, client: Arc<dyn KeycloakApi>) -> Self {
        const ACTION: &str = "Call one Keycloak Admin REST endpoint in the configured realm. Use relative_url such as /users?first=0&max=20; tool arguments cannot change the configured authority, realm, or credentials. method accepts GET, POST, PUT, PATCH, DELETE, or another bounded HTTP token. params is a strict JSON object string sent as the body; put query parameters in relative_url. The response text is returned in a serialized result up to 512 KiB, including an empty string for no content. This operation can read confidential identity data or create, update, delete, and trigger actions, so it may independently require approval. It makes one token request and one Admin request with no automatic retry. After an unknown effect outcome, reconcile Keycloak state before retrying because a retry can repeat the effect.";
        let prefix_bytes = "Toolkit: \n".len();
        let name_budget = MAX_DESCRIPTION_BYTES.saturating_sub(prefix_bytes + ACTION.len());
        let bounded_name = truncate_utf8(toolkit_name, name_budget);
        Self {
            client,
            description: format!("Toolkit: {bounded_name}\n{ACTION}").into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for KeycloakTool {
    fn name(&self) -> &str {
        EXECUTE
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
        reject_unknown_keys(arguments, &["method", "relative_url", "params"])?;
        let method = required_string(arguments, "method")?;
        let relative_url = required_string(arguments, "relative_url")?;
        parse_method(method).map_err(KeycloakClientError::into_adk)?;
        validate_relative_url(relative_url).map_err(KeycloakClientError::into_adk)?;
        let params = parse_params(arguments.get("params"))?;
        self.client
            .execute(method, relative_url, &params)
            .await
            .map_err(KeycloakClientError::into_adk)
    }
}

fn execute_schema() -> Value {
    json!({
        "title": "ExecuteModel",
        "type": "object",
        "properties": {
            "method": {
                "type": "string",
                "minLength": 1,
                "maxLength": 32,
                "pattern": "^[!#$%&'*+.^_`|~0-9A-Za-z-]+$",
                "description": "HTTP method token for the Keycloak Admin API request, for example GET, POST, PUT, PATCH, or DELETE. Custom RFC method tokens are accepted; any method other than GET, HEAD, or OPTIONS is treated as a remote effect."
            },
            "relative_url": {
                "type": "string",
                "minLength": 1,
                "maxLength": 4096,
                "pattern": "^/[^#\\\\]*$",
                "description": "Path and optional query below /admin/realms/{configured_realm}, starting with one slash; for example /users?first=0&max=20. Schemes, authorities, fragments, backslashes, controls, and traversal segments are rejected."
            },
            "params": {
                "anyOf": [{"type": "string", "maxLength": MAX_PARAMETER_STRING_BYTES}, {"type": "null"}],
                "default": "",
                "description": "Strict JSON-encoded object sent as the request body, for example {\"enabled\":true}; an empty string, null, or omission sends {}. Arrays, scalars, single-quoted pseudo-JSON, excessive nesting, and values above 65536 UTF-8 bytes are rejected. Put URL query parameters in relative_url."
            }
        },
        "required": ["method", "relative_url"],
        "additionalProperties": false
    })
}

fn parse_params(value: Option<&Value>) -> adk_core::Result<Map<String, Value>> {
    let Some(value) = value else {
        return Ok(Map::new());
    };
    if value.is_null() {
        return Ok(Map::new());
    }
    let value = value.as_str().ok_or_else(invalid_arguments)?;
    if value.len() > MAX_PARAMETER_STRING_BYTES {
        return Err(resource_exhausted());
    }
    if value.is_empty() {
        return Ok(Map::new());
    }
    let parsed: Value = serde_json::from_str(value).map_err(|_| invalid_arguments())?;
    validate_parameter_tree(&parsed)?;
    parsed.as_object().cloned().ok_or_else(invalid_arguments)
}

fn validate_parameter_tree(value: &Value) -> adk_core::Result<()> {
    let mut nodes = 0usize;
    let mut stack = vec![(value, 0usize)];
    while let Some((node, depth)) = stack.pop() {
        nodes = nodes.checked_add(1).ok_or_else(resource_exhausted)?;
        if nodes > MAX_PARAMETER_NODES || depth > MAX_PARAMETER_DEPTH {
            return Err(resource_exhausted());
        }
        match node {
            Value::String(value) if value.len() > MAX_PARAMETER_STRING_BYTES => {
                return Err(resource_exhausted());
            }
            Value::Array(values) => {
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                if values
                    .keys()
                    .any(|key| key.len() > MAX_PARAMETER_STRING_BYTES)
                {
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
        "keycloak.arguments.invalid",
        "the Keycloak tool arguments are invalid",
    )
}

fn resource_exhausted() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "keycloak.arguments.resource_exhausted",
        "the Keycloak tool arguments exceed the approved limit",
    )
}

#[cfg(test)]
pub(in crate::toolkits) fn test_parse_params(
    value: Option<&Value>,
) -> adk_core::Result<Map<String, Value>> {
    parse_params(value)
}
