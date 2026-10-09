use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{AzureApi, AzureClient, AzureClientError, parse_method};
use super::config::{AzureConfigError, AzureToolkitConfig};

const EXECUTE: &str = "execute";
const HEALTHCHECK: &str = "azure_integration_healthcheck";
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_OPTIONAL_ARGS_STRING_BYTES: usize = 64 * 1_024;
const MAX_OPTION_NODES: usize = 8 * 1_024;
const MAX_OPTION_DEPTH: usize = 32;
const MAX_DESCRIPTION_BYTES: usize = 1_000;
const SUBSCRIPTION_ID_EXAMPLE: &str = "00000000-0000-0000-0000-000000000000";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AzureToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Safe construction failure for the complete two-tool Azure family.
pub(crate) struct AzureToolsetError {
    code: AzureToolsetErrorCode,
}

impl AzureToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> AzureToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for AzureToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AzureToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AzureToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AzureToolsetErrorCode::InvalidConfiguration => {
                "the Azure toolkit configuration is invalid"
            }
            AzureToolsetErrorCode::ResourceExhausted => {
                "the Azure toolkit configuration exceeds its approved limit"
            }
            AzureToolsetErrorCode::UnsupportedSelection => {
                "the selected Azure tool profile is unsupported"
            }
            AzureToolsetErrorCode::Client => "the Azure client could not be created",
            AzureToolsetErrorCode::InvalidDefinition => "the Azure ADK tool definition is invalid",
        })
    }
}

impl std::error::Error for AzureToolsetError {}

impl From<AzureConfigError> for AzureToolsetError {
    fn from(source: AzureConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::AzureConfigErrorCode::InvalidConfiguration => {
                    AzureToolsetErrorCode::InvalidConfiguration
                }
                super::config::AzureConfigErrorCode::ResourceExhausted => {
                    AzureToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<AzureClientError> for AzureToolsetError {
    fn from(_: AzureClientError) -> Self {
        Self {
            code: AzureToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for AzureToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: AzureToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the complete capability-disabled Azure toolset.
///
/// `execute` can perform reads, writes, deletes, and action endpoints. Its
/// source `execute` group is metadata only; sensitivity and effect ownership
/// remain independent admission responsibilities.
pub(crate) fn build_azure_toolset(
    toolkit_name: &str,
    config: AzureToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, AzureToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let subscription_id = config.subscription_id().to_owned();
    let client: Arc<dyn AzureApi> = Arc::new(AzureClient::new(config)?);
    build_with_api(toolkit_name, &subscription_id, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), AzureToolsetError> {
    if selected
        .iter()
        .any(|name| !matches!(name.as_ref(), EXECUTE | HEALTHCHECK))
    {
        return Err(AzureToolsetError {
            code: AzureToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    subscription_id: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn AzureApi>,
) -> Result<BasicToolset, AzureToolsetError> {
    let kinds = [AzureToolKind::Execute, AzureToolKind::Healthcheck];
    let tools = kinds
        .into_iter()
        .filter(|kind| selected.is_empty() || selected.iter().any(|name| name == kind.name()))
        .map(|kind| {
            Arc::new(AzureTool::new(
                kind,
                toolkit_name,
                subscription_id,
                Arc::clone(client),
            )) as Arc<dyn Tool>
        })
        .collect::<Vec<_>>();
    admit_materialized_toolset(toolkit_name, "azure", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    subscription_id: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn AzureApi>,
) -> Result<BasicToolset, AzureToolsetError> {
    if selected
        .iter()
        .any(|name| !matches!(name.as_str(), EXECUTE | HEALTHCHECK))
    {
        return Err(AzureToolsetError {
            code: AzureToolsetErrorCode::UnsupportedSelection,
        });
    }
    build_with_api(toolkit_name, subscription_id, selected, policy, client)
}

#[cfg(test)]
pub(in crate::toolkits) const fn test_catalog() -> [(&'static str, &'static str); 2] {
    [(EXECUTE, "execute"), (HEALTHCHECK, "read")]
}

#[derive(Clone, Copy)]
enum AzureToolKind {
    Execute,
    Healthcheck,
}

impl AzureToolKind {
    const fn name(self) -> &'static str {
        match self {
            Self::Execute => EXECUTE,
            Self::Healthcheck => HEALTHCHECK,
        }
    }

    const fn action(self) -> &'static str {
        match self {
            Self::Execute => {
                "Call one Azure Resource Manager control-plane endpoint inside the configured subscription. Use an absolute URL such as https://management.azure.com/subscriptions/00000000-0000-0000-0000-000000000000/resourcegroups?api-version=2021-04-01. method accepts any bounded HTTP token; GET, HEAD, and OPTIONS are reads, while other methods can create, update, delete, or trigger actions. optional_args supports bounded headers, params, json, data, and inline multipart files; it never reads a local path, and cannot override credentials or transport headers. The UTF-8 body is returned in a serialized result up to 512 KiB; provider 202 means accepted, not necessarily completed. One token request and one ARM request are made without automatic retry. Reads can expose confidential metadata and may independently require approval. After an unknown effect outcome, reconcile Azure state before retrying because a retry can repeat the effect."
            }
            Self::Healthcheck => {
                "Check the configured Microsoft Entra credential and Azure subscription with one read-only Resource Manager request that lists resource groups using API version 2021-04-01. Returns [true,\"\"] on success or [false,<stable reason>] on failure. It does not modify resources and does not prove access to every resource provider or role."
            }
        }
    }

    const fn read_only(self) -> bool {
        matches!(self, Self::Healthcheck)
    }
}

struct AzureTool {
    kind: AzureToolKind,
    client: Arc<dyn AzureApi>,
    subscription_id: Box<str>,
    description: Box<str>,
}

impl AzureTool {
    fn new(
        kind: AzureToolKind,
        toolkit_name: &str,
        subscription_id: &str,
        client: Arc<dyn AzureApi>,
    ) -> Self {
        let action = kind.action();
        let prefix_bytes = "Toolkit: \n".len();
        let name_budget = MAX_DESCRIPTION_BYTES.saturating_sub(prefix_bytes + action.len());
        let bounded_name = truncate_utf8(toolkit_name, name_budget);
        Self {
            kind,
            client,
            subscription_id: subscription_id.into(),
            description: format!("Toolkit: {bounded_name}\n{action}").into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for AzureTool {
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
            AzureToolKind::Execute => execute_schema(),
            AzureToolKind::Healthcheck => empty_schema("AzureIntegrationHealthcheckModel"),
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
            AzureToolKind::Execute => {
                reject_unknown_keys(arguments, &["method", "url", "optional_args"])?;
                let method = required_string(arguments, "method")?;
                let url = required_string(arguments, "url")?;
                parse_method(method).map_err(AzureClientError::into_adk)?;
                super::client::validate_arm_url(url, &self.subscription_id)
                    .map_err(AzureClientError::into_adk)?;
                let optional_args = parse_optional_args(arguments.get("optional_args"))?;
                self.client
                    .execute(method, url, &optional_args)
                    .await
                    .map_err(AzureClientError::into_adk)
            }
            AzureToolKind::Healthcheck => {
                reject_unknown_keys(arguments, &[])?;
                Ok(self.client.healthcheck().await)
            }
        }
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
                "description": "HTTP method token for the ARM request, for example GET, POST, PUT, PATCH, or DELETE. Custom RFC method tokens are accepted. GET, HEAD, and OPTIONS are reads; all other methods are remote effects."
            },
            "url": {
                "type": "string",
                "minLength": 1,
                "maxLength": 8192,
                "pattern": "^https://management\\.azure\\.com/subscriptions/",
                "description": format!("Absolute public-cloud ARM URL inside the configured subscription, with the endpoint's api-version query; for example https://management.azure.com/subscriptions/{SUBSCRIPTION_ID_EXAMPLE}/resourcegroups?api-version=2021-04-01. Other origins, subscriptions, credentials, ports, fragments, traversal, and redirects are rejected.")
            },
            "optional_args": optional_args_schema()
        },
        "required": ["method", "url"],
        "additionalProperties": false
    })
}

fn optional_args_schema() -> Value {
    json!({
        "anyOf": [
            {"type": "string", "maxLength": MAX_OPTIONAL_ARGS_STRING_BYTES},
            {
                "type": "object",
                "maxProperties": 5,
                "properties": {
                    "headers": {
                        "type": "object",
                        "maxProperties": 64,
                        "additionalProperties": {"type": "string", "maxLength": 8192}
                    },
                    "params": {
                        "type": "object",
                        "maxProperties": 256,
                        "additionalProperties": true
                    },
                    "json": {},
                    "data": {"anyOf": [{"type": "string"}, {"type": "object"}]},
                    "files": {
                        "type": "object",
                        "minProperties": 1,
                        "maxProperties": 16,
                        "additionalProperties": {
                            "anyOf": [
                                {"type": "string", "maxLength": 245_760},
                                {"type": "array", "minItems": 2, "maxItems": 4}
                            ]
                        }
                    }
                },
                "additionalProperties": false
            },
            {"type": "null"}
        ],
        "default": null,
        "description": "Optional strict JSON object, or a JSON-object string up to 65536 UTF-8 bytes. Allowed keys: headers (string map; Authorization/Host/length/hop-by-hop headers are rejected), params (scalar or scalar-array query values), json (JSON body), data (raw string or form object), and files (up to 16 inline text parts, 240 KiB total). A file value is text or [filename,text,content_type?,headers?]; local paths and artifact reads are never performed. Use only one of json or data; files may be combined only with object data. The complete tool argument remains below 256 KiB."
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

fn parse_optional_args(value: Option<&Value>) -> adk_core::Result<Map<String, Value>> {
    let Some(value) = value else {
        return Ok(Map::new());
    };
    if value.is_null() {
        return Ok(Map::new());
    }
    let parsed = if let Some(value) = value.as_str() {
        if value.len() > MAX_OPTIONAL_ARGS_STRING_BYTES {
            return Err(resource_exhausted());
        }
        if value.is_empty() {
            return Ok(Map::new());
        }
        serde_json::from_str(value).map_err(|_| invalid_arguments())?
    } else {
        value.clone()
    };
    validate_option_tree(&parsed)?;
    parsed.as_object().cloned().ok_or_else(invalid_arguments)
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
            Value::String(value) if value.len() > MAX_OPTIONAL_ARGS_STRING_BYTES => {
                return Err(resource_exhausted());
            }
            Value::Array(values) => {
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                if values
                    .keys()
                    .any(|key| key.len() > MAX_OPTIONAL_ARGS_STRING_BYTES)
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
        "azure.arguments.invalid",
        "the Azure tool arguments are invalid",
    )
}

fn resource_exhausted() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "azure.arguments.resource_exhausted",
        "the Azure tool arguments exceed the approved limit",
    )
}

#[cfg(test)]
pub(in crate::toolkits) fn test_parse_optional_args(
    value: Option<&Value>,
) -> adk_core::Result<Map<String, Value>> {
    parse_optional_args(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_validate_arm_url(
    value: &str,
    subscription_id: &str,
) -> Result<reqwest::Url, AzureClientError> {
    super::client::validate_arm_url(value, subscription_id)
}
