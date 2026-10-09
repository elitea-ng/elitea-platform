use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{RallyApi, RallyClient, RallyClientError, normalized_entity_type};
use super::config::{RallyConfigError, RallyToolkitConfig};

const GET_TYPES: &str = "get_types";
const GET_ENTITIES: &str = "get_entities";
const GET_PROJECT: &str = "get_project";
const GET_WORKSPACE: &str = "get_workspace";
const GET_USER: &str = "get_user";
const GET_CONTEXT: &str = "get_context";
const CREATE_ARTIFACT: &str = "create_artifact";
const UPDATE_ARTIFACT: &str = "update_artifact";
const DEFAULT_ENTITY_TYPE: &str = "UserStory";
const DEFAULT_CREATE_ENTITY_TYPE: &str = "HierarchicalRequirement";
const DEFAULT_LIMIT: usize = 10;
const MAX_LIMIT: usize = 100;
const MAX_ENTITY_TYPE_BYTES: usize = 128;
const MAX_QUERY_BYTES: usize = 8 * 1_024;
const MAX_NAME_BYTES: usize = 1_024;
const MAX_ENTITY_JSON_BYTES: usize = 128 * 1_024;
const MAX_ENTITY_FIELDS: usize = 256;
const MAX_FIELD_NAME_BYTES: usize = 256;
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum RallyToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for the complete Rally family.
pub(crate) struct RallyToolsetError {
    code: RallyToolsetErrorCode,
}

impl RallyToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> RallyToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for RallyToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("RallyToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for RallyToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            RallyToolsetErrorCode::InvalidConfiguration => {
                "the Rally toolkit configuration is invalid"
            }
            RallyToolsetErrorCode::ResourceExhausted => {
                "the Rally toolkit configuration exceeds its approved limit"
            }
            RallyToolsetErrorCode::UnsupportedSelection => {
                "the selected Rally tool profile is not supported"
            }
            RallyToolsetErrorCode::Client => "the Rally client could not be created",
            RallyToolsetErrorCode::InvalidDefinition => "the Rally ADK tool definition is invalid",
        })
    }
}

impl std::error::Error for RallyToolsetError {}

impl From<RallyConfigError> for RallyToolsetError {
    fn from(source: RallyConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::RallyConfigErrorCode::InvalidConfiguration => {
                    RallyToolsetErrorCode::InvalidConfiguration
                }
                super::config::RallyConfigErrorCode::ResourceExhausted => {
                    RallyToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<RallyClientError> for RallyToolsetError {
    fn from(_: RallyClientError) -> Self {
        Self {
            code: RallyToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for RallyToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: RallyToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build all eight capability-disabled Rally tools.
///
/// Read/write groups guide model selection only. Trusted sensitivity policy
/// and the future exact-interrupt wrapper independently decide approval.
pub(crate) fn build_rally_toolset(
    toolkit_name: &str,
    config: RallyToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, RallyToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn RallyApi> = Arc::new(RallyClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), RallyToolsetError> {
    if selected.iter().any(|name| {
        !matches!(
            name.as_ref(),
            GET_TYPES
                | GET_ENTITIES
                | GET_PROJECT
                | GET_WORKSPACE
                | GET_USER
                | GET_CONTEXT
                | CREATE_ARTIFACT
                | UPDATE_ARTIFACT
        )
    }) {
        return Err(RallyToolsetError {
            code: RallyToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn RallyApi>,
) -> Result<BasicToolset, RallyToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(8);
    for kind in RallyToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(RallyTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "rally", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn RallyApi>,
) -> Result<BasicToolset, RallyToolsetError> {
    build_with_api(toolkit_name, selected, policy, client)
}

#[derive(Clone, Copy)]
enum RallyToolKind {
    GetTypes,
    GetEntities,
    GetProject,
    GetWorkspace,
    GetUser,
    GetContext,
    CreateArtifact,
    UpdateArtifact,
}

impl RallyToolKind {
    const ALL: [Self; 8] = [
        Self::GetTypes,
        Self::GetEntities,
        Self::GetProject,
        Self::GetWorkspace,
        Self::GetUser,
        Self::GetContext,
        Self::CreateArtifact,
        Self::UpdateArtifact,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::GetTypes => GET_TYPES,
            Self::GetEntities => GET_ENTITIES,
            Self::GetProject => GET_PROJECT,
            Self::GetWorkspace => GET_WORKSPACE,
            Self::GetUser => GET_USER,
            Self::GetContext => GET_CONTEXT,
            Self::CreateArtifact => CREATE_ARTIFACT,
            Self::UpdateArtifact => UPDATE_ARTIFACT,
        }
    }

    const fn is_read_only(self) -> bool {
        !matches!(self, Self::CreateArtifact | Self::UpdateArtifact)
    }
}

struct RallyTool {
    kind: RallyToolKind,
    client: Arc<dyn RallyApi>,
    description: Box<str>,
}

impl RallyTool {
    fn new(kind: RallyToolKind, toolkit_name: &str, client: Arc<dyn RallyApi>) -> Self {
        let action = match kind {
            RallyToolKind::GetTypes => {
                "List Rally WSAPI entity type names that can be used with get_entities, create_artifact, and update_artifact. This is a bounded read and returns entity API names, not artifacts."
            }
            RallyToolKind::GetEntities => {
                "Read the first bounded Rally WSAPI page for one entity type. Use an optional complete Rally query expression to filter results; no continuation pages are fetched. Returns a JSON array with at most 100 entities."
            }
            RallyToolKind::GetProject => {
                "Read Rally projects by exact name. Omit project_name to use the toolkit's configured project when present, otherwise return a bounded current-scope project page. Returns a JSON array."
            }
            RallyToolKind::GetWorkspace => {
                "Read Rally workspaces by exact name. Omit workspace_name to use the toolkit's configured workspace when present, otherwise return a bounded accessible-workspace page. Returns a JSON array."
            }
            RallyToolKind::GetUser => {
                "Read Rally users, optionally by exact UserName. Omit user_name for a bounded current-scope user page. Returns a JSON array and never fetches continuation pages."
            }
            RallyToolKind::GetContext => {
                "Read the configured/default Rally project, workspace, and user context. The three bounded reads run concurrently and return a JSON object with project, workspace, and user arrays."
            }
            RallyToolKind::CreateArtifact => {
                "Create one Rally artifact of the requested WSAPI entity type from a JSON object. The effect can create duplicates and is not safe to retry after an unknown outcome. Returns a success message with the provider's FormattedID or ObjectID."
            }
            RallyToolKind::UpdateArtifact => {
                "Update one existing Rally artifact. entity_json must contain ObjectID or FormattedID plus at least one changed field. This mutates Rally and is not safe to retry after an unknown outcome. Returns a success message with the provider identifier."
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
impl Tool for RallyTool {
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
            RallyToolKind::GetTypes | RallyToolKind::GetContext => empty_schema(),
            RallyToolKind::GetEntities => get_entities_schema(),
            RallyToolKind::GetProject => named_schema(
                "project_name",
                "Exact Rally project Name. Omit or pass null to use the configured project when present, otherwise the current bounded project scope.",
            ),
            RallyToolKind::GetWorkspace => named_schema(
                "workspace_name",
                "Exact Rally workspace Name. Omit or pass null to use the configured workspace when present, otherwise the accessible bounded workspace scope.",
            ),
            RallyToolKind::GetUser => named_schema(
                "user_name",
                "Exact Rally UserName, for example `engineer@example.com`. Omit or pass null for a bounded current-scope user page.",
            ),
            RallyToolKind::CreateArtifact => create_schema(),
            RallyToolKind::UpdateArtifact => update_schema(),
        })
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_argument_size(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        let result = match self.kind {
            RallyToolKind::GetTypes => {
                reject_unknown_keys(arguments, &[])?;
                self.client.get_types().await
            }
            RallyToolKind::GetEntities => {
                reject_unknown_keys(arguments, &["entity_type", "query", "fetch", "limit"])?;
                let entity_type = optional_string(arguments, "entity_type", MAX_ENTITY_TYPE_BYTES)?
                    .unwrap_or(DEFAULT_ENTITY_TYPE);
                normalized_entity_type(entity_type).map_err(RallyClientError::into_adk)?;
                self.client
                    .get_entities(
                        entity_type,
                        optional_query(arguments)?,
                        optional_bool(arguments, "fetch", true)?,
                        result_limit(arguments)?,
                    )
                    .await
            }
            RallyToolKind::GetProject => {
                reject_unknown_keys(arguments, &["project_name"])?;
                self.client
                    .get_project(optional_name(arguments, "project_name")?)
                    .await
            }
            RallyToolKind::GetWorkspace => {
                reject_unknown_keys(arguments, &["workspace_name"])?;
                self.client
                    .get_workspace(optional_name(arguments, "workspace_name")?)
                    .await
            }
            RallyToolKind::GetUser => {
                reject_unknown_keys(arguments, &["user_name"])?;
                self.client
                    .get_user(optional_name(arguments, "user_name")?)
                    .await
            }
            RallyToolKind::GetContext => {
                reject_unknown_keys(arguments, &[])?;
                self.client.get_context().await
            }
            RallyToolKind::CreateArtifact => {
                reject_unknown_keys(arguments, &["entity_json", "entity_type"])?;
                let fields = entity_fields(arguments)?;
                let entity_type = optional_string(arguments, "entity_type", MAX_ENTITY_TYPE_BYTES)?
                    .unwrap_or(DEFAULT_CREATE_ENTITY_TYPE);
                normalized_entity_type(entity_type).map_err(RallyClientError::into_adk)?;
                self.client.create_artifact(entity_type, &fields).await
            }
            RallyToolKind::UpdateArtifact => {
                reject_unknown_keys(arguments, &["entity_json", "entity_type"])?;
                let fields = entity_fields(arguments)?;
                let entity_type = required_string(arguments, "entity_type", MAX_ENTITY_TYPE_BYTES)?;
                normalized_entity_type(entity_type).map_err(RallyClientError::into_adk)?;
                validate_update_fields(&fields)?;
                self.client.update_artifact(entity_type, &fields).await
            }
        }
        .map_err(RallyClientError::into_adk)?;
        Ok(result)
    }
}

fn get_entities_schema() -> Value {
    json!({
        "type":"object",
        "properties":{
            "entity_type":entity_type_property(DEFAULT_ENTITY_TYPE, "Rally WSAPI entity API name, for example `HierarchicalRequirement`, `Defect`, `UserStory`, or `PortfolioItem/Feature`. `Story`, `UserStory`, and `User Story` resolve to HierarchicalRequirement."),
            "query":{
                "type":["string","null"], "maxLength":MAX_QUERY_BYTES, "default":null,
                "description":"Complete optional Rally WSAPI query expression, for example `ScheduleState = \"In-Progress\"`. It is sent as one bounded provider query; omit or pass null for no filter."
            },
            "fetch":{
                "type":"boolean", "default":true,
                "description":"When true (default), request full entity fields; false requests Rally shell/reference fields."
            },
            "limit":{
                "type":"integer", "minimum":1, "maximum":MAX_LIMIT, "default":DEFAULT_LIMIT,
                "description":"Maximum entities returned from the first provider page. Defaults to 10; accepted range is 1 through 100."
            }
        },
        "additionalProperties":false
    })
}

fn named_schema(name: &str, description: &str) -> Value {
    json!({
        "type":"object",
        "properties":{
            name:{
                "type":["string","null"], "minLength":1, "maxLength":MAX_NAME_BYTES,
                "default":null, "description":description
            }
        },
        "additionalProperties":false
    })
}

fn create_schema() -> Value {
    json!({
        "type":"object",
        "properties":{
            "entity_json":{
                "type":"string", "minLength":2, "maxLength":MAX_ENTITY_JSON_BYTES,
                "description":"JSON object containing canonical Rally WSAPI field names and values. Example for a Defect: `{\"Name\":\"Checkout failure\",\"Severity\":\"Major Problem\",\"Priority\":\"High Attention\",\"State\":\"Open\"}`. Relationship collections may use canonical `{\"_ref\":\"entity/123\"}` objects or the source-compatible `entity/123` shorthand. Maximum encoded size is 131072 bytes."
            },
            "entity_type":entity_type_property(DEFAULT_CREATE_ENTITY_TYPE, "Rally WSAPI entity API name to create. Defaults to `HierarchicalRequirement`; examples include `Defect`, `Task`, and `PortfolioItem/Feature`.")
        },
        "required":["entity_json"],
        "additionalProperties":false
    })
}

fn update_schema() -> Value {
    json!({
        "type":"object",
        "properties":{
            "entity_json":{
                "type":"string", "minLength":2, "maxLength":MAX_ENTITY_JSON_BYTES,
                "description":"JSON object containing exactly one target identity (`ObjectID` or `FormattedID`) plus one or more canonical Rally fields to change. Example: `{\"FormattedID\":\"DE1234\",\"Description\":\"Updated description\",\"ScheduleState\":\"In-Progress\"}`. Relationship collections may use canonical `_ref` objects or `entity/123` shorthand."
            },
            "entity_type":{
                "type":"string", "minLength":1, "maxLength":MAX_ENTITY_TYPE_BYTES,
                "description":"Required Rally WSAPI entity API name of the target, for example `Defect`, `HierarchicalRequirement`, or `PortfolioItem/Feature`."
            }
        },
        "required":["entity_json","entity_type"],
        "additionalProperties":false
    })
}

fn entity_type_property(default: &str, description: &str) -> Value {
    json!({
        "type":["string","null"], "minLength":1, "maxLength":MAX_ENTITY_TYPE_BYTES,
        "default":default, "description":description
    })
}

fn empty_schema() -> Value {
    json!({"type":"object","properties":{},"additionalProperties":false})
}

fn validate_argument_size(arguments: &Value) -> Result<(), AdkError> {
    let size = serde_json::to_vec(arguments)
        .map_err(|_| invalid_arguments())?
        .len();
    if size > MAX_ARGUMENT_BYTES {
        return Err(resource_exhausted_arguments());
    }
    Ok(())
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> Result<(), AdkError> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn required_string<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, AdkError> {
    optional_string(arguments, name, limit)?.ok_or_else(invalid_arguments)
}

fn optional_string<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<Option<&'a str>, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value))
            if !value.is_empty()
                && value.len() <= limit
                && !value.contains('\0')
                && !value.contains(['\r', '\n']) =>
        {
            Ok(Some(value))
        }
        Some(_) => Err(invalid_arguments()),
    }
}

fn optional_name<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
) -> Result<Option<&'a str>, AdkError> {
    let value = optional_string(arguments, name, MAX_NAME_BYTES)?;
    if value.is_some_and(|value| value.contains(['"', '\\'])) {
        return Err(invalid_arguments());
    }
    Ok(value)
}

fn optional_query(arguments: &Map<String, Value>) -> Result<Option<&str>, AdkError> {
    match arguments.get("query") {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value))
            if !value.is_empty()
                && value.len() <= MAX_QUERY_BYTES
                && !value.contains('\0')
                && !value.contains(['\r', '\n']) =>
        {
            Ok(Some(value))
        }
        Some(_) => Err(invalid_arguments()),
    }
}

fn optional_bool(
    arguments: &Map<String, Value>,
    name: &str,
    default: bool,
) -> Result<bool, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(default),
        Some(Value::Bool(value)) => Ok(*value),
        Some(_) => Err(invalid_arguments()),
    }
}

fn result_limit(arguments: &Map<String, Value>) -> Result<usize, AdkError> {
    match arguments.get("limit") {
        None | Some(Value::Null) => Ok(DEFAULT_LIMIT),
        Some(value) => value
            .as_u64()
            .and_then(|value| usize::try_from(value).ok())
            .filter(|value| (1..=MAX_LIMIT).contains(value))
            .ok_or_else(invalid_arguments),
    }
}

fn entity_fields(arguments: &Map<String, Value>) -> Result<Map<String, Value>, AdkError> {
    let source = required_string(arguments, "entity_json", MAX_ENTITY_JSON_BYTES)?;
    let fields = serde_json::from_str::<Value>(source)
        .ok()
        .and_then(|value| value.as_object().cloned())
        .filter(|fields| !fields.is_empty() && fields.len() <= MAX_ENTITY_FIELDS)
        .ok_or_else(invalid_arguments)?;
    if fields.keys().any(|name| {
        name.is_empty()
            || name.len() > MAX_FIELD_NAME_BYTES
            || !name.bytes().enumerate().all(|(index, byte)| {
                if index == 0 {
                    byte.is_ascii_alphabetic()
                } else {
                    byte.is_ascii_alphanumeric() || byte == b'_'
                }
            })
    }) {
        return Err(invalid_arguments());
    }
    Ok(fields)
}

fn validate_update_fields(fields: &Map<String, Value>) -> Result<(), AdkError> {
    if fields.contains_key("ObjectID") == fields.contains_key("FormattedID")
        || fields
            .keys()
            .all(|key| matches!(key.as_str(), "ObjectID" | "FormattedID"))
    {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "rally.arguments.invalid",
        "the Rally tool arguments are invalid",
    )
}

fn resource_exhausted_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "rally.arguments.resource_exhausted",
        "the Rally tool arguments exceed the approved limit",
    )
}
