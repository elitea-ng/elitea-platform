use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{SalesforceApi, SalesforceClient, SalesforceClientError, SalesforceMethod};
use super::config::{SalesforceConfigError, SalesforceToolkitConfig};

const CREATE_CASE: &str = "create_case";
const CREATE_LEAD: &str = "create_lead";
const SEARCH_SALESFORCE: &str = "search_salesforce";
const UPDATE_CASE: &str = "update_case";
const UPDATE_LEAD: &str = "update_lead";
const EXECUTE_GENERIC: &str = "execute_generic_rq";
const MAX_TEXT_BYTES: usize = 16 * 1_024;
const MAX_QUERY_BYTES: usize = 64 * 1_024;
const MAX_PARAMS_BYTES: usize = 128 * 1_024;
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_ARGUMENT_NODES: usize = 16_384;
const MAX_ARGUMENT_DEPTH: usize = 64;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SalesforceToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for the complete Salesforce family.
pub(crate) struct SalesforceToolsetError {
    code: SalesforceToolsetErrorCode,
}

impl SalesforceToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> SalesforceToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for SalesforceToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SalesforceToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SalesforceToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SalesforceToolsetErrorCode::InvalidConfiguration => {
                "the Salesforce toolkit configuration is invalid"
            }
            SalesforceToolsetErrorCode::ResourceExhausted => {
                "the Salesforce toolkit configuration exceeds its approved limit"
            }
            SalesforceToolsetErrorCode::UnsupportedSelection => {
                "the selected Salesforce tool profile is not supported"
            }
            SalesforceToolsetErrorCode::Client => "the Salesforce client could not be created",
            SalesforceToolsetErrorCode::InvalidDefinition => {
                "the Salesforce ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for SalesforceToolsetError {}

impl From<SalesforceConfigError> for SalesforceToolsetError {
    fn from(source: SalesforceConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::SalesforceConfigErrorCode::InvalidConfiguration => {
                    SalesforceToolsetErrorCode::InvalidConfiguration
                }
                super::config::SalesforceConfigErrorCode::ResourceExhausted => {
                    SalesforceToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<SalesforceClientError> for SalesforceToolsetError {
    fn from(_: SalesforceClientError) -> Self {
        Self {
            code: SalesforceToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for SalesforceToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: SalesforceToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build all six capability-disabled Salesforce tools.
///
/// Operation grouping is model metadata, not authorization. Production
/// assembly must apply the shared exact-interrupt guard and retain dispatched
/// effect ownership through a known or explicitly unknown provider outcome.
pub(crate) fn build_salesforce_toolset(
    toolkit_name: &str,
    config: SalesforceToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, SalesforceToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn SalesforceApi> = Arc::new(SalesforceClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), SalesforceToolsetError> {
    if selected.iter().any(|name| {
        !matches!(
            name.as_ref(),
            CREATE_CASE
                | CREATE_LEAD
                | SEARCH_SALESFORCE
                | UPDATE_CASE
                | UPDATE_LEAD
                | EXECUTE_GENERIC
        )
    }) {
        return Err(SalesforceToolsetError {
            code: SalesforceToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn SalesforceApi>,
) -> Result<BasicToolset, SalesforceToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(6);
    for kind in SalesforceToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(SalesforceTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "salesforce", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn SalesforceApi>,
) -> Result<BasicToolset, SalesforceToolsetError> {
    build_with_api(toolkit_name, selected, policy, client)
}

#[derive(Clone, Copy)]
enum SalesforceToolKind {
    CreateCase,
    CreateLead,
    Search,
    UpdateCase,
    UpdateLead,
    ExecuteGeneric,
}

impl SalesforceToolKind {
    const ALL: [Self; 6] = [
        Self::CreateCase,
        Self::CreateLead,
        Self::Search,
        Self::UpdateCase,
        Self::UpdateLead,
        Self::ExecuteGeneric,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::CreateCase => CREATE_CASE,
            Self::CreateLead => CREATE_LEAD,
            Self::Search => SEARCH_SALESFORCE,
            Self::UpdateCase => UPDATE_CASE,
            Self::UpdateLead => UPDATE_LEAD,
            Self::ExecuteGeneric => EXECUTE_GENERIC,
        }
    }

    const fn is_read_only(self) -> bool {
        matches!(self, Self::Search)
    }
}

struct SalesforceTool {
    kind: SalesforceToolKind,
    client: Arc<dyn SalesforceApi>,
    description: Box<str>,
}

impl SalesforceTool {
    fn new(kind: SalesforceToolKind, toolkit_name: &str, client: Arc<dyn SalesforceApi>) -> Self {
        let action = match kind {
            SalesforceToolKind::CreateCase => {
                "Create one Salesforce Case when a new support record is required. Required: subject, description, origin, and status; origin and status must match values configured in the org. This can create duplicates and is not safe to retry after an unknown outcome."
            }
            SalesforceToolKind::CreateLead => {
                "Create one Salesforce Lead. Required: last_name, company, email, and phone. This can create duplicates and is not safe to retry after an unknown outcome."
            }
            SalesforceToolKind::Search => {
                "Run one read-only SOQL SELECT and return the first Salesforce query page. object_type is the expected object API name and compatibility label; query is the complete SOQL string. Continuations are not fetched."
            }
            SalesforceToolKind::UpdateCase => {
                "Update one existing Case by Salesforce record ID. status is required; omit or pass an empty description to leave Description unchanged. This tool cannot clear Description."
            }
            SalesforceToolKind::UpdateLead => {
                "Update Email and/or Phone on one existing Lead by Salesforce record ID. Provide at least one non-empty change; omitted or empty fields remain unchanged."
            }
            SalesforceToolKind::ExecuteGeneric => {
                "Call a versioned Salesforce REST resource not covered by the dedicated tools. Prefer dedicated Case and Lead tools. GET is read-only; POST, PATCH, and DELETE can create, change, or delete data and are not safe to retry after an unknown outcome."
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
impl Tool for SalesforceTool {
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
            SalesforceToolKind::CreateCase => create_case_schema(),
            SalesforceToolKind::CreateLead => create_lead_schema(),
            SalesforceToolKind::Search => search_schema(),
            SalesforceToolKind::UpdateCase => update_case_schema(),
            SalesforceToolKind::UpdateLead => update_lead_schema(),
            SalesforceToolKind::ExecuteGeneric => generic_schema(),
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
            SalesforceToolKind::CreateCase => {
                reject_unknown_keys(arguments, &["subject", "description", "origin", "status"])?;
                self.client
                    .create_case(
                        required_text(arguments, "subject", MAX_TEXT_BYTES, true)?,
                        required_text(arguments, "description", MAX_TEXT_BYTES, true)?,
                        required_text(arguments, "origin", MAX_TEXT_BYTES, true)?,
                        required_text(arguments, "status", MAX_TEXT_BYTES, true)?,
                    )
                    .await
            }
            SalesforceToolKind::CreateLead => {
                reject_unknown_keys(arguments, &["last_name", "company", "email", "phone"])?;
                self.client
                    .create_lead(
                        required_text(arguments, "last_name", MAX_TEXT_BYTES, true)?,
                        required_text(arguments, "company", MAX_TEXT_BYTES, true)?,
                        required_text(arguments, "email", MAX_TEXT_BYTES, true)?,
                        required_text(arguments, "phone", MAX_TEXT_BYTES, true)?,
                    )
                    .await
            }
            SalesforceToolKind::Search => {
                reject_unknown_keys(arguments, &["object_type", "query"])?;
                self.client
                    .search_salesforce(
                        required_text(arguments, "object_type", MAX_TEXT_BYTES, false)?,
                        required_text(arguments, "query", MAX_QUERY_BYTES, false)?,
                    )
                    .await
            }
            SalesforceToolKind::UpdateCase => {
                reject_unknown_keys(arguments, &["case_id", "status", "description"])?;
                self.client
                    .update_case(
                        required_text(arguments, "case_id", 18, false)?,
                        required_text(arguments, "status", MAX_TEXT_BYTES, true)?,
                        optional_text(arguments, "description", MAX_TEXT_BYTES)?,
                    )
                    .await
            }
            SalesforceToolKind::UpdateLead => {
                reject_unknown_keys(arguments, &["lead_id", "email", "phone"])?;
                self.client
                    .update_lead(
                        required_text(arguments, "lead_id", 18, false)?,
                        optional_text(arguments, "email", MAX_TEXT_BYTES)?,
                        optional_text(arguments, "phone", MAX_TEXT_BYTES)?,
                    )
                    .await
            }
            SalesforceToolKind::ExecuteGeneric => {
                reject_unknown_keys(arguments, &["method", "relative_url", "params"])?;
                let method = SalesforceMethod::parse(required_text(arguments, "method", 6, false)?)
                    .map_err(SalesforceClientError::into_adk)?;
                let relative_url = required_text(arguments, "relative_url", 4 * 1_024, false)?;
                let params = parse_params(arguments)?;
                self.client
                    .execute_generic(method, relative_url, &params)
                    .await
            }
        }
        .map_err(SalesforceClientError::into_adk)?;
        bounded_result(result)
    }
}

fn create_case_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "subject": text_schema("Subject for the new Case.", true),
            "description": text_schema("Description for the new Case.", true),
            "origin": text_schema("Case origin configured in the Salesforce org, for example `Web` or `Phone`.", true),
            "status": text_schema("Initial Case status configured in the Salesforce org, for example `New`.", true)
        },
        "required": ["subject", "description", "origin", "status"],
        "additionalProperties": false
    })
}

fn create_lead_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "last_name": text_schema("Last name for the new Lead.", true),
            "company": text_schema("Company name for the new Lead.", true),
            "email": text_schema("Email address for the new Lead.", true),
            "phone": text_schema("Phone number for the new Lead.", true)
        },
        "required": ["last_name", "company", "email", "phone"],
        "additionalProperties": false
    })
}

fn search_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "object_type": {
                "type": "string",
                "minLength": 1,
                "maxLength": MAX_TEXT_BYTES,
                "description": "Expected Salesforce object API name, for example `Case`, `Lead`, or `Custom__c`. This required compatibility label does not rewrite or restrict the SOQL query."
            },
            "query": {
                "type": "string",
                "minLength": 1,
                "maxLength": MAX_QUERY_BYTES,
                "description": "Complete read-only SOQL SELECT query, for example `SELECT Id, Subject FROM Case WHERE Status = 'New' LIMIT 20`. Only the first query page is returned."
            }
        },
        "required": ["object_type", "query"],
        "additionalProperties": false
    })
}

fn update_case_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "case_id": record_id_schema("Salesforce Case record ID; exactly 15 or 18 ASCII alphanumeric characters."),
            "status": text_schema("New Case status configured in the Salesforce org.", true),
            "description": {
                "type": ["string", "null"],
                "maxLength": MAX_TEXT_BYTES,
                "default": "",
                "description": "Optional updated Description. Omit, null, or an empty string to leave the existing Description unchanged; this tool cannot clear it."
            }
        },
        "required": ["case_id", "status"],
        "additionalProperties": false
    })
}

fn update_lead_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "lead_id": record_id_schema("Salesforce Lead record ID; exactly 15 or 18 ASCII alphanumeric characters."),
            "email": {
                "type": ["string", "null"],
                "maxLength": MAX_TEXT_BYTES,
                "default": "",
                "description": "Optional new Email. Omit, null, or empty to leave Email unchanged."
            },
            "phone": {
                "type": ["string", "null"],
                "maxLength": MAX_TEXT_BYTES,
                "default": "",
                "description": "Optional new Phone. Omit, null, or empty to leave Phone unchanged."
            }
        },
        "required": ["lead_id"],
        "description": "At least one of email or phone must be non-empty.",
        "additionalProperties": false
    })
}

fn generic_schema() -> Value {
    json!({
        "type": "object",
        "properties": {
            "method": {
                "type": "string",
                "enum": ["GET", "POST", "PATCH", "DELETE"],
                "description": "Exact uppercase HTTP method. GET reads; POST, PATCH, and DELETE can create, change, or delete Salesforce data."
            },
            "relative_url": {
                "type": "string",
                "minLength": 1,
                "maxLength": 4 * 1_024,
                "pattern": "^/",
                "description": "Version-relative Salesforce REST path beneath `/services/data/{api_version}`, for example `/sobjects/Case/500000000000000AAA`. Schemes, authorities, traversal, query strings, and fragments are rejected."
            },
            "params": {
                "type": ["string", "null"],
                "maxLength": MAX_PARAMS_BYTES,
                "default": "{}",
                "description": "Optional JSON object encoded as a string (default `{}`). GET uses scalar members as query parameters; POST, PATCH, and DELETE use the object as the JSON request body."
            }
        },
        "required": ["method", "relative_url"],
        "additionalProperties": false
    })
}

fn text_schema(description: &'static str, allow_empty: bool) -> Value {
    let mut schema = json!({
        "type": "string",
        "maxLength": MAX_TEXT_BYTES,
        "description": description
    });
    if !allow_empty {
        schema["minLength"] = json!(1);
    }
    schema
}

fn record_id_schema(description: &'static str) -> Value {
    json!({
        "anyOf": [
            {"type": "string", "minLength": 15, "maxLength": 15, "pattern": "^[A-Za-z0-9]{15}$"},
            {"type": "string", "minLength": 18, "maxLength": 18, "pattern": "^[A-Za-z0-9]{18}$"}
        ],
        "description": description
    })
}

fn parse_params(arguments: &Map<String, Value>) -> adk_core::Result<Map<String, Value>> {
    let raw = match arguments.get("params") {
        None | Some(Value::Null) => "{}",
        Some(Value::String(value)) if value.len() <= MAX_PARAMS_BYTES => value.as_str(),
        Some(Value::String(_)) => return Err(resource_exhausted()),
        Some(_) => return Err(invalid_arguments()),
    };
    let value: Value = serde_json::from_str(raw).map_err(|_| invalid_arguments())?;
    validate_json(&value)?;
    value.as_object().cloned().ok_or_else(invalid_arguments)
}

fn required_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    limit: usize,
    allow_empty: bool,
) -> adk_core::Result<&'a str> {
    let value = arguments
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_arguments)?;
    if value.len() > limit {
        return Err(resource_exhausted());
    }
    if (!allow_empty && value.is_empty()) || value.bytes().any(|byte| byte == 0) {
        return Err(invalid_arguments());
    }
    Ok(value)
}

fn optional_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> adk_core::Result<Option<&'a str>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if value.len() <= limit => {
            if value.bytes().any(|byte| byte == 0) {
                Err(invalid_arguments())
            } else {
                Ok(Some(value))
            }
        }
        Some(Value::String(_)) => Err(resource_exhausted()),
        Some(_) => Err(invalid_arguments()),
    }
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
            Value::String(value) if value.len() > MAX_PARAMS_BYTES => {
                return Err(resource_exhausted());
            }
            Value::Array(values) => {
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                for (key, value) in values {
                    if key.len() > MAX_TEXT_BYTES {
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

fn bounded_result(value: Value) -> adk_core::Result<Value> {
    if serde_json::to_vec(&value)
        .map_err(|_| invalid_arguments())?
        .len()
        > MAX_ARGUMENT_BYTES
    {
        return Err(resource_exhausted());
    }
    Ok(value)
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "salesforce.arguments.invalid",
        "the Salesforce tool arguments are invalid",
    )
}

fn resource_exhausted() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "salesforce.arguments.resource_exhausted",
        "the Salesforce tool arguments exceed the approved limit",
    )
}
