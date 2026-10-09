use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{ServiceNowApi, ServiceNowClient, ServiceNowClientError};
use super::config::{ServiceNowConfigError, ServiceNowToolkitConfig};

const GET_INCIDENTS: &str = "get_incidents";
const CREATE_INCIDENT: &str = "create_incident";
const UPDATE_INCIDENT: &str = "update_incident";
const DEFAULT_LIMIT: usize = 100;
const MAX_LIMIT: u64 = 100;
const MAX_ARGUMENT_BYTES: usize = 128 * 1_024;
const MAX_ARGUMENT_NODES: usize = 16_384;
const MAX_ARGUMENT_DEPTH: usize = 64;
const MAX_ARGUMENT_STRING_BYTES: usize = 64 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;
const INCIDENT_FIELD_GUIDANCE: &str = "category, description, short_description, impact, incident_state, urgency, or assignment_group";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ServiceNowToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Safe construction failure for the complete `ServiceNow` incident family.
pub struct ServiceNowToolsetError {
    code: ServiceNowToolsetErrorCode,
}

impl ServiceNowToolsetError {
    #[must_use]
    pub const fn code(&self) -> ServiceNowToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for ServiceNowToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ServiceNowToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ServiceNowToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ServiceNowToolsetErrorCode::InvalidConfiguration => {
                "the ServiceNow toolkit configuration is invalid"
            }
            ServiceNowToolsetErrorCode::ResourceExhausted => {
                "the ServiceNow toolkit configuration exceeds its approved limit"
            }
            ServiceNowToolsetErrorCode::UnsupportedSelection => {
                "the selected ServiceNow tool profile is not supported"
            }
            ServiceNowToolsetErrorCode::Client => "the ServiceNow client could not be created",
            ServiceNowToolsetErrorCode::InvalidDefinition => {
                "the ServiceNow ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for ServiceNowToolsetError {}

impl From<ServiceNowConfigError> for ServiceNowToolsetError {
    fn from(source: ServiceNowConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::ServiceNowConfigErrorCode::InvalidConfiguration => {
                    ServiceNowToolsetErrorCode::InvalidConfiguration
                }
                super::config::ServiceNowConfigErrorCode::ResourceExhausted => {
                    ServiceNowToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<ServiceNowClientError> for ServiceNowToolsetError {
    fn from(_: ServiceNowClientError) -> Self {
        Self {
            code: ServiceNowToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for ServiceNowToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: ServiceNowToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the complete capability-disabled `ServiceNow` incident toolset.
///
/// The SDK's `read`/`write` groups remain ordinary operation metadata. A
/// trusted deployment may independently require durable human approval for
/// any of these tools; the shared guardrail, not this family, owns that pause.
pub fn build_service_now_toolset(
    toolkit_name: &str,
    config: ServiceNowToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, ServiceNowToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn ServiceNowApi> = Arc::new(ServiceNowClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), ServiceNowToolsetError> {
    if selected.iter().any(|name| {
        !matches!(
            name.as_ref(),
            GET_INCIDENTS | CREATE_INCIDENT | UPDATE_INCIDENT
        )
    }) {
        return Err(ServiceNowToolsetError {
            code: ServiceNowToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn ServiceNowApi>,
) -> Result<BasicToolset, ServiceNowToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(3);
    for kind in [
        ServiceNowToolKind::GetIncidents,
        ServiceNowToolKind::CreateIncident,
        ServiceNowToolKind::UpdateIncident,
    ] {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(ServiceNowTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "service_now", policy, tools).map_err(Into::into)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn ServiceNowApi>,
) -> Result<BasicToolset, ServiceNowToolsetError> {
    build_with_api(toolkit_name, selected, policy, client)
}

#[derive(Clone, Copy)]
enum ServiceNowToolKind {
    GetIncidents,
    CreateIncident,
    UpdateIncident,
}

impl ServiceNowToolKind {
    const fn name(self) -> &'static str {
        match self {
            Self::GetIncidents => GET_INCIDENTS,
            Self::CreateIncident => CREATE_INCIDENT,
            Self::UpdateIncident => UPDATE_INCIDENT,
        }
    }

    const fn is_read_only(self) -> bool {
        matches!(self, Self::GetIncidents)
    }
}

struct ServiceNowTool {
    kind: ServiceNowToolKind,
    client: Arc<dyn ServiceNowApi>,
    description: Box<str>,
}

impl ServiceNowTool {
    fn new(kind: ServiceNowToolKind, toolkit_name: &str, client: Arc<dyn ServiceNowApi>) -> Self {
        let action = match kind {
            ServiceNowToolKind::GetIncidents => {
                "Retrieve incidents from the configured ServiceNow instance using optional filters."
            }
            ServiceNowToolKind::CreateIncident => {
                "Create one incident in the configured ServiceNow instance from the supplied fields."
            }
            ServiceNowToolKind::UpdateIncident => {
                "Update one existing incident, selected by its sys_id, in the configured ServiceNow instance."
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
impl Tool for ServiceNowTool {
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
        Some(match self.kind {
            ServiceNowToolKind::GetIncidents => get_incidents_schema(),
            ServiceNowToolKind::CreateIncident => create_incident_schema(),
            ServiceNowToolKind::UpdateIncident => update_incident_schema(),
        })
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_json(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        let result = match self.kind {
            ServiceNowToolKind::GetIncidents => {
                reject_unknown_keys(arguments, &["data"])?;
                let data = optional_object(arguments, "data")?;
                let limit = incident_limit(data)?;
                self.client.get_incidents(data, limit).await
            }
            ServiceNowToolKind::CreateIncident => {
                reject_unknown_keys(arguments, &["data"])?;
                let data = optional_object(arguments, "data")?;
                self.client.create_incident(data).await
            }
            ServiceNowToolKind::UpdateIncident => {
                reject_unknown_keys(arguments, &["sys_id", "update_fields"])?;
                let sys_id = required_text(arguments, "sys_id")?;
                let update_fields = required_text(arguments, "update_fields")?;
                let fields = parse_update_fields(update_fields)?;
                self.client.update_incident(sys_id, &fields).await
            }
        }
        .map_err(ServiceNowClientError::into_adk)?;

        let encoded = serde_json::to_string(&result).map_err(|_| invalid_arguments())?;
        if encoded.len() > MAX_ARGUMENT_BYTES {
            return Err(resource_exhausted());
        }
        Ok(Value::String(encoded))
    }
}

fn get_incidents_schema() -> Value {
    json!({
        "title": "getIncidents",
        "type": "object",
        "properties": {
            "data": {
                "anyOf": [{
                    "type": "object",
                    "properties": {
                        "category": {"type": "string"},
                        "description": {"type": "string"},
                        "number_of_entries": {"type": "integer", "minimum": 1, "maximum": MAX_LIMIT},
                        "creation_date": {"type": "string", "description": "Creation date in YYYY-MM-DD form."},
                        "sys_id": {"type": "string", "minLength": 32, "maxLength": 32, "pattern": "^[A-Za-z0-9]{32}$"},
                        "number": {"type": "string"}
                    },
                    "additionalProperties": false
                }, {"type": "null"}],
                "default": {},
                "description": "Filters used to retrieve incidents. Supported keys are category, description, number_of_entries (an integer from 1 through 100), creation_date (YYYY-MM-DD), sys_id, and number. An empty object retrieves the bounded first page.",
                "examples": [{"description": "Network issue", "category": "network"}]
            }
        },
        "additionalProperties": false
    })
}

fn create_incident_schema() -> Value {
    json!({
        "title": "createIncident",
        "type": "object",
        "properties": {
            "data": {
                "anyOf": [{"type": "object", "additionalProperties": true}, {"type": "null"}],
                "default": {},
                "description": format!("Bounded JSON fields used to create the incident; non-null values may be strings, numbers, booleans, arrays, or objects, while null-valued fields are omitted before the request. Common fields are {INCIDENT_FIELD_GUIDANCE}. An empty object creates a default incident.")
            }
        },
        "additionalProperties": false
    })
}

fn update_incident_schema() -> Value {
    json!({
        "title": "updateIncident",
        "type": "object",
        "properties": {
            "sys_id": {
                "type": "string",
                "minLength": 32,
                "maxLength": 32,
                "pattern": "^[A-Za-z0-9]{32}$",
                "description": "The 32-character alphanumeric sys_id of the incident to update."
            },
            "update_fields": {
                "type": "string",
                "maxLength": MAX_ARGUMENT_STRING_BYTES,
                "description": format!("A JSON object encoded as a string containing fields to update. Common fields are {INCIDENT_FIELD_GUIDANCE}.")
            }
        },
        "required": ["sys_id", "update_fields"],
        "additionalProperties": false
    })
}

fn optional_object<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
) -> adk_core::Result<&'a Map<String, Value>> {
    static EMPTY: std::sync::LazyLock<Map<String, Value>> = std::sync::LazyLock::new(Map::new);
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(&EMPTY),
        Some(Value::Object(value)) => Ok(value),
        Some(_) => Err(invalid_arguments()),
    }
}

fn incident_limit(data: &Map<String, Value>) -> adk_core::Result<usize> {
    match data.get("number_of_entries") {
        None | Some(Value::Null) => Ok(DEFAULT_LIMIT),
        Some(Value::Number(value)) => value
            .as_u64()
            .filter(|value| (1..=MAX_LIMIT).contains(value))
            .and_then(|value| usize::try_from(value).ok())
            .ok_or_else(invalid_arguments),
        Some(_) => Err(invalid_arguments()),
    }
}

fn required_text<'a>(arguments: &'a Map<String, Value>, name: &str) -> adk_core::Result<&'a str> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty() && value.len() <= MAX_ARGUMENT_STRING_BYTES)
        .ok_or_else(invalid_arguments)
}

fn parse_update_fields(raw: &str) -> adk_core::Result<Map<String, Value>> {
    if raw.len() > MAX_ARGUMENT_BYTES {
        return Err(resource_exhausted());
    }
    let value: Value = serde_json::from_str(raw).map_err(|_| invalid_arguments())?;
    validate_json(&value)?;
    value.as_object().cloned().ok_or_else(invalid_arguments)
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> adk_core::Result<()> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
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

    let mut nodes = 0usize;
    let mut stack = vec![(value, 0usize)];
    while let Some((node, depth)) = stack.pop() {
        nodes = nodes.checked_add(1).ok_or_else(resource_exhausted)?;
        if nodes > MAX_ARGUMENT_NODES || depth > MAX_ARGUMENT_DEPTH {
            return Err(resource_exhausted());
        }
        match node {
            Value::String(value) if value.len() > MAX_ARGUMENT_STRING_BYTES => {
                return Err(resource_exhausted());
            }
            Value::Array(values) => {
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                for (key, value) in values {
                    if key.len() > MAX_ARGUMENT_STRING_BYTES {
                        return Err(resource_exhausted());
                    }
                    stack.push((value, depth + 1));
                }
            }
            Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => {}
        }
    }
    Ok(())
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "service_now.arguments.invalid",
        "the ServiceNow tool arguments are invalid",
    )
}

fn resource_exhausted() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "service_now.arguments.resource_exhausted",
        "the ServiceNow tool arguments exceed the approved limit",
    )
}
