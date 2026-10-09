use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{ZephyrApi, ZephyrClient, ZephyrClientError, ZephyrStep};
use super::config::{ZephyrConfigError, ZephyrToolkitConfig};

const GET_TEST_CASE_STEPS: &str = "get_test_case_steps";
const ADD_NEW_TEST_CASE_STEP: &str = "add_new_test_case_step";
const ADD_TEST_CASE: &str = "add_test_case";
const ADD_TEST_CASES: &str = "add_test_cases";
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_TEXT_BYTES: usize = 64 * 1_024;
// Keep family admission aligned with PolicyBoundTool's per-string ceiling.
const MAX_JSON_STRING_BYTES: usize = 64 * 1_024;
const MAX_JSON_SCHEMA_CHARS: usize = MAX_JSON_STRING_BYTES / 4;
const MAX_STEPS_PER_CASE: usize = 50;
const MAX_CASES_PER_BATCH: usize = 20;
const MAX_STEPS_PER_BATCH: usize = 100;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ZephyrToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for the complete legacy Zephyr family.
pub struct ZephyrToolsetError {
    code: ZephyrToolsetErrorCode,
}

impl ZephyrToolsetError {
    #[must_use]
    pub const fn code(&self) -> ZephyrToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for ZephyrToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ZephyrToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ZephyrToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ZephyrToolsetErrorCode::InvalidConfiguration => {
                "the legacy Zephyr toolkit configuration is invalid"
            }
            ZephyrToolsetErrorCode::ResourceExhausted => {
                "the legacy Zephyr toolkit configuration exceeds its approved limit"
            }
            ZephyrToolsetErrorCode::UnsupportedSelection => {
                "the selected legacy Zephyr tool profile is unsupported"
            }
            ZephyrToolsetErrorCode::Client => "the legacy Zephyr client could not be created",
            ZephyrToolsetErrorCode::InvalidDefinition => {
                "the legacy Zephyr ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for ZephyrToolsetError {}

impl From<ZephyrConfigError> for ZephyrToolsetError {
    fn from(source: ZephyrConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::ZephyrConfigErrorCode::InvalidConfiguration => {
                    ZephyrToolsetErrorCode::InvalidConfiguration
                }
                super::config::ZephyrConfigErrorCode::ResourceExhausted => {
                    ZephyrToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<ZephyrClientError> for ZephyrToolsetError {
    fn from(_: ZephyrClientError) -> Self {
        Self {
            code: ZephyrToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for ZephyrToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: ZephyrToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build all four capability-disabled legacy Zephyr tools.
///
/// Read/write grouping is model metadata only. The three effects remain closed
/// until the exact-interrupt HITL and durable effect owner are composed.
pub fn build_zephyr_toolset(
    toolkit_name: &str,
    config: ZephyrToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, ZephyrToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn ZephyrApi> = Arc::new(ZephyrClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), ZephyrToolsetError> {
    if selected.iter().any(|name| {
        !ZephyrToolKind::ALL
            .iter()
            .any(|kind| kind.name() == name.as_ref())
    }) {
        return Err(ZephyrToolsetError {
            code: ZephyrToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn ZephyrApi>,
) -> Result<BasicToolset, ZephyrToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(ZephyrToolKind::ALL.len());
    for kind in ZephyrToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(ZephyrTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "zephyr", policy, tools).map_err(Into::into)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn ZephyrApi>,
) -> Result<BasicToolset, ZephyrToolsetError> {
    validate_selected_strings(selected)?;
    build_with_api(toolkit_name, selected, policy, client)
}

fn validate_selected_strings(selected: &[String]) -> Result<(), ZephyrToolsetError> {
    if selected
        .iter()
        .any(|name| !ZephyrToolKind::ALL.iter().any(|kind| kind.name() == name))
    {
        return Err(ZephyrToolsetError {
            code: ZephyrToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

#[cfg(any(test, feature = "test-support"))]
pub const fn test_catalog() -> [(&'static str, &'static str); 4] {
    [
        (GET_TEST_CASE_STEPS, "read"),
        (ADD_NEW_TEST_CASE_STEP, "write"),
        (ADD_TEST_CASE, "write"),
        (ADD_TEST_CASES, "write"),
    ]
}

#[derive(Clone, Copy)]
enum ZephyrToolKind {
    GetTestCaseSteps,
    AddNewTestCaseStep,
    AddTestCase,
    AddTestCases,
}

impl ZephyrToolKind {
    const ALL: [Self; 4] = [
        Self::GetTestCaseSteps,
        Self::AddNewTestCaseStep,
        Self::AddTestCase,
        Self::AddTestCases,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::GetTestCaseSteps => GET_TEST_CASE_STEPS,
            Self::AddNewTestCaseStep => ADD_NEW_TEST_CASE_STEP,
            Self::AddTestCase => ADD_TEST_CASE,
            Self::AddTestCases => ADD_TEST_CASES,
        }
    }

    const fn is_read_only(self) -> bool {
        matches!(self, Self::GetTestCaseSteps)
    }

    const fn description(self) -> &'static str {
        match self {
            Self::GetTestCaseSteps => {
                "Read all test steps currently returned for one Jira-backed legacy Zephyr test issue. Provide positive numeric Jira issue and project IDs. The result is either 'No Zephyr test steps found' or one bounded message containing order_id, step, data, and result for each step. This performs one read-only ZAPI request and does not fetch another page."
            }
            Self::AddNewTestCaseStep => {
                "Append exactly one step to one Jira-backed legacy Zephyr test issue. step, data, and result are separate required strings; data and result may be empty. This is a non-idempotent remote effect and can create a duplicate. Do not retry after a timeout or unknown outcome; reconcile the issue's current steps first."
            }
            Self::AddTestCase => {
                "Append 1 through 50 ordered steps to one Jira-backed legacy Zephyr test issue from a JSON object with a steps array. Every step requires string fields step, data, and result. The client validates the complete batch, then creates steps sequentially. A failure can leave a partial test case; reconcile existing steps before retrying the batch."
            }
            Self::AddTestCases => {
                "Append steps to 1 through 20 Jira-backed legacy Zephyr test issues from a JSON array, with at most 100 total steps. Each case requires positive issue_id, positive project_id, and a non-empty steps array whose entries contain string step, data, and result fields. The client validates the complete batch, then creates sequentially. A failure can leave partial effects across issues; reconcile every target before retrying."
            }
        }
    }
}

struct ZephyrTool {
    kind: ZephyrToolKind,
    client: Arc<dyn ZephyrApi>,
    description: Box<str>,
}

impl ZephyrTool {
    fn new(kind: ZephyrToolKind, toolkit_name: &str, client: Arc<dyn ZephyrApi>) -> Self {
        let prefix_bytes = "Toolkit: \n".len();
        let action = kind.description();
        let name_budget = MAX_DESCRIPTION_BYTES.saturating_sub(prefix_bytes + action.len());
        let name = truncate_utf8(toolkit_name, name_budget);
        Self {
            kind,
            client,
            description: format!("Toolkit: {name}\n{action}").into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for ZephyrTool {
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
        Some(schema_for(self.kind))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_argument_size(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        match self.kind {
            ZephyrToolKind::GetTestCaseSteps => execute_get(&self.client, arguments).await,
            ZephyrToolKind::AddNewTestCaseStep => execute_add_one(&self.client, arguments).await,
            ZephyrToolKind::AddTestCase => execute_add_case(&self.client, arguments).await,
            ZephyrToolKind::AddTestCases => execute_add_cases(&self.client, arguments).await,
        }
    }
}

async fn execute_get(
    client: &Arc<dyn ZephyrApi>,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Value> {
    reject_unknown_keys(arguments, &["issue_id", "project_id"])?;
    client
        .get_test_case_steps(
            positive_id(arguments, "issue_id")?,
            positive_id(arguments, "project_id")?,
        )
        .await
        .map_err(ZephyrClientError::into_adk)
}

async fn execute_add_one(
    client: &Arc<dyn ZephyrApi>,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Value> {
    reject_unknown_keys(
        arguments,
        &["issue_id", "project_id", "step", "data", "result"],
    )?;
    let step = step_from(arguments)?;
    client
        .add_new_test_case_step(
            positive_id(arguments, "issue_id")?,
            positive_id(arguments, "project_id")?,
            &step,
        )
        .await
        .map_err(ZephyrClientError::into_adk)
}

async fn execute_add_case(
    client: &Arc<dyn ZephyrApi>,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Value> {
    reject_unknown_keys(arguments, &["issue_id", "project_id", "steps_data"])?;
    let issue_id = positive_id(arguments, "issue_id")?;
    let project_id = positive_id(arguments, "project_id")?;
    let steps = parse_steps_payload(json_string(arguments, "steps_data")?)?;
    execute_steps(client, &[(issue_id, project_id, steps)])
        .await
        .map(|created| json!({"status":"created","created_steps":created}))
        .map_err(ZephyrClientError::into_adk)
}

async fn execute_add_cases(
    client: &Arc<dyn ZephyrApi>,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Value> {
    reject_unknown_keys(arguments, &["create_test_cases_data"])?;
    let cases = parse_cases_payload(json_string(arguments, "create_test_cases_data")?)?;
    let case_count = cases.len();
    execute_steps(client, &cases)
        .await
        .map(|created| {
            json!({"status":"created","created_cases":case_count,"created_steps":created})
        })
        .map_err(ZephyrClientError::into_adk)
}

async fn execute_steps(
    client: &Arc<dyn ZephyrApi>,
    cases: &[(u64, u64, Vec<ZephyrStep>)],
) -> Result<usize, ZephyrClientError> {
    let mut confirmed = 0_usize;
    for (issue_id, project_id, steps) in cases {
        for step in steps {
            match client
                .add_new_test_case_step(*issue_id, *project_id, step)
                .await
            {
                Ok(_) => confirmed += 1,
                Err(_) if confirmed > 0 => {
                    return Err(ZephyrClientError::after_confirmed_effect());
                }
                Err(error) => return Err(error),
            }
        }
    }
    Ok(confirmed)
}

fn parse_steps_payload(value: &str) -> adk_core::Result<Vec<ZephyrStep>> {
    let payload: Value = serde_json::from_str(value).map_err(|_| invalid_arguments())?;
    let object = payload.as_object().ok_or_else(invalid_arguments)?;
    reject_unknown_keys(object, &["steps"])?;
    steps_from_value(object.get("steps").ok_or_else(invalid_arguments)?)
}

fn parse_cases_payload(value: &str) -> adk_core::Result<Vec<(u64, u64, Vec<ZephyrStep>)>> {
    let payload: Value = serde_json::from_str(value).map_err(|_| invalid_arguments())?;
    let values = payload.as_array().ok_or_else(invalid_arguments)?;
    if values.is_empty() || values.len() > MAX_CASES_PER_BATCH {
        return Err(if values.len() > MAX_CASES_PER_BATCH {
            resource_exhausted()
        } else {
            invalid_arguments()
        });
    }
    let mut total_steps = 0_usize;
    let mut cases = Vec::with_capacity(values.len());
    for value in values {
        let object = value.as_object().ok_or_else(invalid_arguments)?;
        reject_unknown_keys(object, &["issue_id", "project_id", "steps"])?;
        let steps = steps_from_value(object.get("steps").ok_or_else(invalid_arguments)?)?;
        total_steps = total_steps
            .checked_add(steps.len())
            .ok_or_else(resource_exhausted)?;
        if total_steps > MAX_STEPS_PER_BATCH {
            return Err(resource_exhausted());
        }
        cases.push((
            positive_id(object, "issue_id")?,
            positive_id(object, "project_id")?,
            steps,
        ));
    }
    Ok(cases)
}

fn steps_from_value(value: &Value) -> adk_core::Result<Vec<ZephyrStep>> {
    let values = value.as_array().ok_or_else(invalid_arguments)?;
    if values.is_empty() || values.len() > MAX_STEPS_PER_CASE {
        return Err(if values.len() > MAX_STEPS_PER_CASE {
            resource_exhausted()
        } else {
            invalid_arguments()
        });
    }
    values
        .iter()
        .map(|value| {
            let object = value.as_object().ok_or_else(invalid_arguments)?;
            reject_unknown_keys(object, &["step", "data", "result"])?;
            step_from(object)
        })
        .collect()
}

fn step_from(arguments: &Map<String, Value>) -> adk_core::Result<ZephyrStep> {
    Ok(ZephyrStep::new(
        bounded_string(arguments, "step")?,
        bounded_string(arguments, "data")?,
        bounded_string(arguments, "result")?,
    ))
}

fn schema_for(kind: ZephyrToolKind) -> Value {
    match kind {
        ZephyrToolKind::GetTestCaseSteps => issue_project_schema(),
        ZephyrToolKind::AddNewTestCaseStep => add_one_schema(),
        ZephyrToolKind::AddTestCase => add_case_schema(),
        ZephyrToolKind::AddTestCases => add_cases_schema(),
    }
}

fn issue_project_schema() -> Value {
    json!({
        "title":"ZephyrGetTestSteps",
        "type":"object",
        "properties":{
            "issue_id":id_property("Required positive numeric Jira issue ID of the test case, for example 10000."),
            "project_id":id_property("Required positive numeric Jira project ID owning the test case, for example 10100.")
        },
        "required":["issue_id","project_id"],
        "additionalProperties":false
    })
}

fn add_one_schema() -> Value {
    json!({
        "title":"ZephyrAddNewTestStep",
        "type":"object",
        "properties":{
            "issue_id":id_property("Required positive numeric Jira issue ID to which the step is appended, for example 10000."),
            "project_id":id_property("Required positive numeric Jira project ID owning the test case, for example 10100."),
            "step":text_property("Required test instruction text, at most 64 KiB UTF-8; for example 'Click the Search button'."),
            "data":text_property("Required test-data text, at most 64 KiB UTF-8; pass an empty string when the step needs no data."),
            "result":text_property("Required expected-result text, at most 64 KiB UTF-8; pass an empty string when no separate verification is needed.")
        },
        "required":["issue_id","project_id","step","data","result"],
        "additionalProperties":false
    })
}

fn add_case_schema() -> Value {
    json!({
        "title":"ZephyrAddTestCase",
        "type":"object",
        "properties":{
            "issue_id":id_property("Required positive numeric Jira issue ID to which all steps are appended."),
            "project_id":id_property("Required positive numeric Jira project ID owning the test case."),
            "steps_data":json_payload_property("Required JSON-encoded object with exactly one non-empty steps array of at most 50 entries. Every entry requires string fields step, data, and result; data/result may be empty. Example: {\"steps\":[{\"step\":\"Click Search\",\"data\":\"term=rust\",\"result\":\"Results appear\"}]}. Maximum UTF-8 input is 64 KiB.")
        },
        "required":["issue_id","project_id","steps_data"],
        "additionalProperties":false
    })
}

fn add_cases_schema() -> Value {
    json!({
        "title":"ZephyrAddTestCases",
        "type":"object",
        "properties":{
            "create_test_cases_data":json_payload_property("Required JSON-encoded array containing 1 through 20 case objects and at most 100 total steps. Each case requires positive issue_id, positive project_id, and a non-empty steps array; every step requires string fields step, data, and result. Example: [{\"issue_id\":10000,\"project_id\":10100,\"steps\":[{\"step\":\"Open login\",\"data\":\"\",\"result\":\"Form appears\"}]}]. Maximum UTF-8 input is 64 KiB.")
        },
        "required":["create_test_cases_data"],
        "additionalProperties":false
    })
}

fn id_property(description: &str) -> Value {
    json!({"type":"integer","minimum":1,"description":description})
}

fn text_property(description: &str) -> Value {
    json!({"type":"string","maxLength":MAX_TEXT_BYTES / 4,"description":description})
}

fn json_payload_property(description: &str) -> Value {
    json!({
        "type":"string",
        "minLength":2,
        "maxLength":MAX_JSON_SCHEMA_CHARS,
        "description":description
    })
}

fn validate_argument_size(arguments: &Value) -> adk_core::Result<()> {
    if serde_json::to_vec(arguments)
        .map_err(|_| invalid_arguments())?
        .len()
        > MAX_ARGUMENT_BYTES
    {
        return Err(resource_exhausted());
    }
    Ok(())
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> adk_core::Result<()> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn positive_id(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<u64> {
    arguments
        .get(name)
        .and_then(Value::as_u64)
        .filter(|value| *value > 0)
        .ok_or_else(invalid_arguments)
}

fn bounded_string<'a>(arguments: &'a Map<String, Value>, name: &str) -> adk_core::Result<&'a str> {
    let value = arguments
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_arguments)?;
    if value.len() > MAX_TEXT_BYTES {
        return Err(resource_exhausted());
    }
    if value.contains('\0') {
        return Err(invalid_arguments());
    }
    Ok(value)
}

fn json_string<'a>(arguments: &'a Map<String, Value>, name: &str) -> adk_core::Result<&'a str> {
    let value = arguments
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(invalid_arguments)?;
    if value.len() > MAX_JSON_STRING_BYTES {
        return Err(resource_exhausted());
    }
    Ok(value)
}

fn truncate_utf8(value: &str, max_bytes: usize) -> &str {
    if value.len() <= max_bytes {
        return value;
    }
    let mut boundary = max_bytes;
    while boundary > 0 && !value.is_char_boundary(boundary) {
        boundary -= 1;
    }
    &value[..boundary]
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "zephyr.arguments.invalid",
        "the legacy Zephyr tool arguments are invalid",
    )
}

fn resource_exhausted() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "zephyr.arguments.resource_exhausted",
        "the legacy Zephyr tool arguments exceed the approved limit",
    )
}
