use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{ZephyrSquadApi, ZephyrSquadClient, ZephyrSquadClientError};
use super::config::{ZephyrSquadConfigError, ZephyrSquadToolkitConfig};

const GET_TEST_STEP: &str = "get_test_step";
const UPDATE_TEST_STEP: &str = "update_test_step";
const DELETE_TEST_STEP: &str = "delete_test_step";
const CREATE_NEW_TEST_STEP: &str = "create_new_test_step";
const GET_ALL_TEST_STEPS: &str = "get_all_test_steps";
const GET_ALL_TEST_STEP_STATUSES: &str = "get_all_test_step_statuses";
const GET_BDD_CONTENT: &str = "get_bdd_content";
const UPDATE_BDD_CONTENT: &str = "update_bdd_content";
const DELETE_BDD_CONTENT: &str = "delete_bdd_content";
const CREATE_NEW_CYCLE: &str = "create_new_cycle";
const CREATE_FOLDER: &str = "create_folder";
const ADD_TEST_TO_CYCLE: &str = "add_test_to_cycle";
const ADD_TEST_TO_FOLDER: &str = "add_test_to_folder";
const CREATE_EXECUTION: &str = "create_execution";
const GET_EXECUTION: &str = "get_execution";
const MAX_OPAQUE_ID_BYTES: usize = 256;
const MAX_JSON_BYTES: usize = 256 * 1_024;
const MAX_GHERKIN_BYTES: usize = 32 * 1_024;
const MAX_ARGUMENT_BYTES: usize = 320 * 1_024;
const MAX_JSON_DEPTH: usize = 32;
const MAX_JSON_NODES: usize = 4_096;
const MAX_JSON_FIELDS: usize = 256;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ZephyrSquadToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for the complete Zephyr Squad family.
pub struct ZephyrSquadToolsetError {
    code: ZephyrSquadToolsetErrorCode,
}

impl ZephyrSquadToolsetError {
    #[must_use]
    pub const fn code(&self) -> ZephyrSquadToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for ZephyrSquadToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ZephyrSquadToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ZephyrSquadToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ZephyrSquadToolsetErrorCode::InvalidConfiguration => {
                "the Zephyr Squad toolkit configuration is invalid"
            }
            ZephyrSquadToolsetErrorCode::ResourceExhausted => {
                "the Zephyr Squad toolkit configuration exceeds its approved limit"
            }
            ZephyrSquadToolsetErrorCode::UnsupportedSelection => {
                "the selected Zephyr Squad tool profile is not supported"
            }
            ZephyrSquadToolsetErrorCode::Client => "the Zephyr Squad client could not be created",
            ZephyrSquadToolsetErrorCode::InvalidDefinition => {
                "the Zephyr Squad ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for ZephyrSquadToolsetError {}

impl From<ZephyrSquadConfigError> for ZephyrSquadToolsetError {
    fn from(source: ZephyrSquadConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::ZephyrSquadConfigErrorCode::InvalidConfiguration => {
                    ZephyrSquadToolsetErrorCode::InvalidConfiguration
                }
                super::config::ZephyrSquadConfigErrorCode::ResourceExhausted => {
                    ZephyrSquadToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<ZephyrSquadClientError> for ZephyrSquadToolsetError {
    fn from(_: ZephyrSquadClientError) -> Self {
        Self {
            code: ZephyrSquadToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for ZephyrSquadToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: ZephyrSquadToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build all fifteen capability-disabled Zephyr Squad tools.
///
/// Read/write/delete groups guide selection only. Trusted sensitivity policy
/// and the future exact-interrupt effect owner independently decide approval.
pub fn build_zephyr_squad_toolset(
    toolkit_name: &str,
    config: ZephyrSquadToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, ZephyrSquadToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn ZephyrSquadApi> = Arc::new(ZephyrSquadClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), ZephyrSquadToolsetError> {
    if selected.iter().any(|name| {
        !ZephyrSquadToolKind::ALL
            .iter()
            .any(|kind| kind.name() == name.as_ref())
    }) {
        return Err(ZephyrSquadToolsetError {
            code: ZephyrSquadToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn ZephyrSquadApi>,
) -> Result<BasicToolset, ZephyrSquadToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(ZephyrSquadToolKind::ALL.len());
    for kind in ZephyrSquadToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(ZephyrSquadTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "zephyr_squad", policy, tools).map_err(Into::into)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn ZephyrSquadApi>,
) -> Result<BasicToolset, ZephyrSquadToolsetError> {
    build_with_api(toolkit_name, selected, policy, client)
}

#[derive(Clone, Copy)]
enum ZephyrSquadToolKind {
    GetTestStep,
    UpdateTestStep,
    DeleteTestStep,
    CreateNewTestStep,
    GetAllTestSteps,
    GetAllTestStepStatuses,
    GetBddContent,
    UpdateBddContent,
    DeleteBddContent,
    CreateNewCycle,
    CreateFolder,
    AddTestToCycle,
    AddTestToFolder,
    CreateExecution,
    GetExecution,
}

impl ZephyrSquadToolKind {
    const ALL: [Self; 15] = [
        Self::GetTestStep,
        Self::UpdateTestStep,
        Self::DeleteTestStep,
        Self::CreateNewTestStep,
        Self::GetAllTestSteps,
        Self::GetAllTestStepStatuses,
        Self::GetBddContent,
        Self::UpdateBddContent,
        Self::DeleteBddContent,
        Self::CreateNewCycle,
        Self::CreateFolder,
        Self::AddTestToCycle,
        Self::AddTestToFolder,
        Self::CreateExecution,
        Self::GetExecution,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::GetTestStep => GET_TEST_STEP,
            Self::UpdateTestStep => UPDATE_TEST_STEP,
            Self::DeleteTestStep => DELETE_TEST_STEP,
            Self::CreateNewTestStep => CREATE_NEW_TEST_STEP,
            Self::GetAllTestSteps => GET_ALL_TEST_STEPS,
            Self::GetAllTestStepStatuses => GET_ALL_TEST_STEP_STATUSES,
            Self::GetBddContent => GET_BDD_CONTENT,
            Self::UpdateBddContent => UPDATE_BDD_CONTENT,
            Self::DeleteBddContent => DELETE_BDD_CONTENT,
            Self::CreateNewCycle => CREATE_NEW_CYCLE,
            Self::CreateFolder => CREATE_FOLDER,
            Self::AddTestToCycle => ADD_TEST_TO_CYCLE,
            Self::AddTestToFolder => ADD_TEST_TO_FOLDER,
            Self::CreateExecution => CREATE_EXECUTION,
            Self::GetExecution => GET_EXECUTION,
        }
    }

    const fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::GetTestStep
                | Self::GetAllTestSteps
                | Self::GetAllTestStepStatuses
                | Self::GetBddContent
                | Self::GetExecution
        )
    }

    const fn description(self) -> &'static str {
        match self {
            Self::GetTestStep => {
                "Read one Jira-backed Zephyr Squad test step by numeric issue/project ID and opaque step ID. Returns bounded provider JSON or text and does not modify data."
            }
            Self::UpdateTestStep => {
                "Replace one test step using a JSON object. The object requires id and step and may include data, result, and customFieldValues. This changes test data and is not safe to retry after an unknown outcome."
            }
            Self::DeleteTestStep => {
                "Permanently delete one test step by numeric issue/project ID and opaque step ID. This destructive effect is not safe to retry after an unknown outcome."
            }
            Self::CreateNewTestStep => {
                "Append one test step to a Jira-backed test case from a JSON object with required step and optional data/result. Creation can duplicate and is not safe to retry after an unknown outcome."
            }
            Self::GetAllTestSteps => {
                "Read the bounded provider response containing test steps for one numeric Jira issue/project pair using Zephyr Squad's v2 test-step endpoint."
            }
            Self::GetAllTestStepStatuses => {
                "Read the bounded Zephyr Squad test-step status catalog. This read takes no arguments."
            }
            Self::GetBddContent => {
                "Read the current bounded Gherkin BDD content for one numeric Jira issue ID."
            }
            Self::UpdateBddContent => {
                "Replace an issue's BDD content with supplied Gherkin text. Pass ordinary text with literal newlines; the client performs JSON encoding. This effect is not safe to retry after an unknown outcome."
            }
            Self::DeleteBddContent => {
                "Permanently remove BDD content for one numeric Jira issue ID. This destructive effect is not safe to retry after an unknown outcome."
            }
            Self::CreateNewCycle => {
                "Create a Zephyr Squad test cycle from a JSON object. name and projectId are required; build, environment, description, startDate, and endDate are optional. Omit versionId only for the provider's unscheduled-version behavior. Creation can duplicate."
            }
            Self::CreateFolder => {
                "Create a uniquely named folder in a cycle from a JSON object with name, cycleId, projectId, and versionId; description is optional. This effect is not safe to retry after an unknown outcome."
            }
            Self::AddTestToCycle => {
                "Add tests to cycle_id using a JSON object. method 1 uses issues, method 2 uses jql, and method 3 copies from fromVersionId/fromCycleId; projectId and versionId are required. This effect can duplicate on retry."
            }
            Self::AddTestToFolder => {
                "Add issue keys to folder_id using a JSON object with issues, assigneeType, method, versionId, projectId, and cycleId. This effect is not safe to retry after an unknown outcome."
            }
            Self::CreateExecution => {
                "Create a test execution from a JSON object with projectId, issueId, and versionId; status, id, cycleId, assigneeType, and assignee are optional. Creation can duplicate on retry."
            }
            Self::GetExecution => {
                "Read one execution and its status by opaque execution ID plus numeric issue/project IDs. Returns bounded provider JSON or text."
            }
        }
    }
}

struct ZephyrSquadTool {
    kind: ZephyrSquadToolKind,
    client: Arc<dyn ZephyrSquadApi>,
    description: Box<str>,
}

impl ZephyrSquadTool {
    fn new(kind: ZephyrSquadToolKind, toolkit_name: &str, client: Arc<dyn ZephyrSquadApi>) -> Self {
        let description = format!("Toolkit: {toolkit_name}\n{}", kind.description());
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
impl Tool for ZephyrSquadTool {
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
        execute_kind(self.kind, &self.client, arguments).await
    }
}

async fn execute_kind(
    kind: ZephyrSquadToolKind,
    client: &Arc<dyn ZephyrSquadApi>,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Value> {
    if kind.is_read_only() {
        execute_read(kind, client, arguments).await
    } else {
        execute_effect(kind, client, arguments).await
    }
}

async fn execute_read(
    kind: ZephyrSquadToolKind,
    client: &Arc<dyn ZephyrSquadApi>,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Value> {
    let result = match kind {
        ZephyrSquadToolKind::GetTestStep => {
            reject_unknown_keys(arguments, &["issue_id", "step_id", "project_id"])?;
            client
                .get_test_step(
                    positive_id(arguments, "issue_id")?,
                    opaque_id(arguments, "step_id")?,
                    positive_id(arguments, "project_id")?,
                )
                .await
        }
        ZephyrSquadToolKind::GetAllTestSteps => {
            reject_unknown_keys(arguments, &["issue_id", "project_id"])?;
            client
                .get_all_test_steps(
                    positive_id(arguments, "issue_id")?,
                    positive_id(arguments, "project_id")?,
                )
                .await
        }
        ZephyrSquadToolKind::GetAllTestStepStatuses => {
            reject_unknown_keys(arguments, &[])?;
            client.get_all_test_step_statuses().await
        }
        ZephyrSquadToolKind::GetBddContent => {
            reject_unknown_keys(arguments, &["issue_id"])?;
            client
                .get_bdd_content(positive_id(arguments, "issue_id")?)
                .await
        }
        ZephyrSquadToolKind::GetExecution => {
            reject_unknown_keys(arguments, &["execution_id", "issue_id", "project_id"])?;
            client
                .get_execution(
                    opaque_id(arguments, "execution_id")?,
                    positive_id(arguments, "issue_id")?,
                    positive_id(arguments, "project_id")?,
                )
                .await
        }
        _ => return Err(invalid_arguments()),
    }
    .map_err(ZephyrSquadClientError::into_adk)?;
    Ok(result)
}

async fn execute_effect(
    kind: ZephyrSquadToolKind,
    client: &Arc<dyn ZephyrSquadApi>,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Value> {
    let result = match kind {
        ZephyrSquadToolKind::UpdateTestStep => {
            reject_unknown_keys(arguments, &["issue_id", "step_id", "project_id", "json"])?;
            let body = json_object(arguments, PayloadKind::UpdateStep)?;
            client
                .update_test_step(
                    positive_id(arguments, "issue_id")?,
                    opaque_id(arguments, "step_id")?,
                    positive_id(arguments, "project_id")?,
                    &body,
                )
                .await
        }
        ZephyrSquadToolKind::DeleteTestStep => {
            reject_unknown_keys(arguments, &["issue_id", "step_id", "project_id"])?;
            client
                .delete_test_step(
                    positive_id(arguments, "issue_id")?,
                    opaque_id(arguments, "step_id")?,
                    positive_id(arguments, "project_id")?,
                )
                .await
        }
        ZephyrSquadToolKind::CreateNewTestStep => {
            reject_unknown_keys(arguments, &["issue_id", "project_id", "json"])?;
            let body = json_object(arguments, PayloadKind::CreateStep)?;
            client
                .create_new_test_step(
                    positive_id(arguments, "issue_id")?,
                    positive_id(arguments, "project_id")?,
                    &body,
                )
                .await
        }
        ZephyrSquadToolKind::UpdateBddContent => {
            reject_unknown_keys(arguments, &["issue_id", "new_content"])?;
            client
                .update_bdd_content(positive_id(arguments, "issue_id")?, gherkin(arguments)?)
                .await
        }
        ZephyrSquadToolKind::DeleteBddContent => {
            reject_unknown_keys(arguments, &["issue_id"])?;
            client
                .delete_bdd_content(positive_id(arguments, "issue_id")?)
                .await
        }
        ZephyrSquadToolKind::CreateNewCycle => {
            reject_unknown_keys(arguments, &["json"])?;
            let body = json_object(arguments, PayloadKind::Cycle)?;
            client.create_new_cycle(&body).await
        }
        ZephyrSquadToolKind::CreateFolder => {
            reject_unknown_keys(arguments, &["json"])?;
            let body = json_object(arguments, PayloadKind::Folder)?;
            client.create_folder(&body).await
        }
        ZephyrSquadToolKind::AddTestToCycle => {
            reject_unknown_keys(arguments, &["cycle_id", "json"])?;
            let body = json_object(arguments, PayloadKind::TestsToCycle)?;
            client
                .add_test_to_cycle(opaque_id(arguments, "cycle_id")?, &body)
                .await
        }
        ZephyrSquadToolKind::AddTestToFolder => {
            reject_unknown_keys(arguments, &["folder_id", "json"])?;
            let body = json_object(arguments, PayloadKind::TestsToFolder)?;
            client
                .add_test_to_folder(opaque_id(arguments, "folder_id")?, &body)
                .await
        }
        ZephyrSquadToolKind::CreateExecution => {
            reject_unknown_keys(arguments, &["json"])?;
            let body = json_object(arguments, PayloadKind::Execution)?;
            client.create_execution(&body).await
        }
        _ => return Err(invalid_arguments()),
    }
    .map_err(ZephyrSquadClientError::into_adk)?;
    Ok(result)
}

fn schema_for(kind: ZephyrSquadToolKind) -> Value {
    match kind {
        ZephyrSquadToolKind::GetTestStep | ZephyrSquadToolKind::DeleteTestStep => {
            issue_project_step_schema(None)
        }
        ZephyrSquadToolKind::UpdateTestStep => issue_project_step_schema(Some(
            "JSON-encoded object, at most 256 KiB. Requires string fields 'id' and 'step'; optional fields include 'data', 'result', and 'customFieldValues', for example [{\"customFieldId\":\"3ce1c679-7c43-4d37-89f6-757603379e31\",\"value\":{\"value\":\"08/21/2018\"}}]. Arrays and scalars are rejected.",
        )),
        ZephyrSquadToolKind::CreateNewTestStep => issue_project_json_schema(
            "JSON-encoded object, at most 256 KiB. Requires 'step'; optional 'data' and 'result', for example {\"step\":\"Open the login page\",\"result\":\"The form is visible\"}.",
        ),
        ZephyrSquadToolKind::GetAllTestSteps => issue_project_schema(),
        ZephyrSquadToolKind::GetAllTestStepStatuses => empty_schema(),
        ZephyrSquadToolKind::GetBddContent | ZephyrSquadToolKind::DeleteBddContent => {
            issue_schema()
        }
        ZephyrSquadToolKind::UpdateBddContent => update_bdd_schema(),
        ZephyrSquadToolKind::CreateNewCycle => json_only_schema(
            "JSON-encoded object, at most 256 KiB. Requires 'name' and positive 'projectId'; 'versionId' may be omitted for the provider's unscheduled version. Optional fields are 'build', 'environment', 'description', 'startDate', and 'endDate'. Example: {\"name\":\"Release 1 regression\",\"projectId\":10100,\"versionId\":10000}.",
        ),
        ZephyrSquadToolKind::CreateFolder => json_only_schema(
            "JSON-encoded object, at most 256 KiB. Requires 'name', 'cycleId', positive 'projectId', and 'versionId'; optional 'description'. Example: {\"name\":\"Smoke\",\"cycleId\":\"0001513838430954-242ac112-0001\",\"projectId\":10100,\"versionId\":10000}.",
        ),
        ZephyrSquadToolKind::AddTestToCycle => opaque_and_json_schema(
            "cycle_id",
            "Opaque Zephyr Squad cycle ID, 1 through 256 URL-safe characters, for example '0001513838430954-242ac112-0001'.",
            "JSON-encoded object, at most 256 KiB. Requires 'method', 'projectId', and 'versionId': method '1' also requires 'issues'; method '2' requires 'jql'; method '3' requires 'fromVersionId' and 'fromCycleId'.",
        ),
        ZephyrSquadToolKind::AddTestToFolder => opaque_and_json_schema(
            "folder_id",
            "Opaque Zephyr Squad folder ID, 1 through 256 URL-safe characters.",
            "JSON-encoded object, at most 256 KiB. Requires 'issues', 'assigneeType', integer 'method', 'versionId', positive 'projectId', and 'cycleId'.",
        ),
        ZephyrSquadToolKind::CreateExecution => json_only_schema(
            "JSON-encoded object, at most 256 KiB. Requires positive 'projectId', positive 'issueId', and 'versionId'; optional 'status', 'id', 'cycleId', 'assigneeType' ('currentUser' or 'assignee'), and 'assignee'.",
        ),
        ZephyrSquadToolKind::GetExecution => execution_schema(),
    }
}

fn issue_schema() -> Value {
    json!({
        "type":"object",
        "properties":{
            "issue_id":numeric_id_property(
                "Positive numeric Jira issue ID for the test case or BDD issue, for example 10000."
            )
        },
        "required":["issue_id"],
        "additionalProperties":false
    })
}

fn issue_project_schema() -> Value {
    json!({
        "type":"object",
        "properties":{
            "issue_id":numeric_id_property(
                "Positive numeric Jira issue ID for the test case, for example 10000."
            ),
            "project_id":numeric_id_property(
                "Positive numeric Jira project ID owning the test case, for example 10100."
            )
        },
        "required":["issue_id","project_id"],
        "additionalProperties":false
    })
}

fn issue_project_step_schema(json_description: Option<&str>) -> Value {
    let mut properties = Map::new();
    properties.insert(
        "issue_id".to_owned(),
        numeric_id_property("Positive numeric Jira issue ID for the test case, for example 10000."),
    );
    properties.insert(
        "project_id".to_owned(),
        numeric_id_property(
            "Positive numeric Jira project ID owning the test case, for example 10100.",
        ),
    );
    properties.insert(
        "step_id".to_owned(),
        opaque_id_property(
            "Opaque Zephyr Squad step ID, 1 through 256 URL-safe characters, for example '0001481146115453-3a0480a3ffffc384-0001'.",
        ),
    );
    let mut required = vec!["issue_id", "step_id", "project_id"];
    if let Some(description) = json_description {
        properties.insert("json".to_owned(), json_string_property(description));
        required.push("json");
    }
    json!({
        "type":"object",
        "properties":properties,
        "required":required,
        "additionalProperties":false
    })
}

fn issue_project_json_schema(description: &str) -> Value {
    json!({
        "type":"object",
        "properties":{
            "issue_id":numeric_id_property(
                "Positive numeric Jira issue ID for the test case, for example 10000."
            ),
            "project_id":numeric_id_property(
                "Positive numeric Jira project ID owning the test case, for example 10100."
            ),
            "json":json_string_property(description)
        },
        "required":["issue_id","project_id","json"],
        "additionalProperties":false
    })
}

fn update_bdd_schema() -> Value {
    json!({
        "type":"object",
        "properties":{
            "issue_id":numeric_id_property(
                "Positive numeric Jira issue ID whose BDD content will be replaced, for example 10000."
            ),
            "new_content":{
                "type":"string",
                "minLength":1,
                "maxLength":MAX_GHERKIN_BYTES,
                "description":"Gherkin feature, background, or scenario text, up to 32768 UTF-8 bytes. Literal newlines are accepted; JSON escaping is handled by the client."
            }
        },
        "required":["issue_id","new_content"],
        "additionalProperties":false
    })
}

fn json_only_schema(description: &str) -> Value {
    json!({
        "type":"object",
        "properties":{"json":json_string_property(description)},
        "required":["json"],
        "additionalProperties":false
    })
}

fn opaque_and_json_schema(id_name: &str, id_description: &str, json_description: &str) -> Value {
    json!({
        "type":"object",
        "properties":{
            id_name:opaque_id_property(id_description),
            "json":json_string_property(json_description)
        },
        "required":[id_name,"json"],
        "additionalProperties":false
    })
}

fn execution_schema() -> Value {
    json!({
        "type":"object",
        "properties":{
            "execution_id":opaque_id_property(
                "Opaque Zephyr Squad execution ID, 1 through 256 URL-safe characters, for example '0001456664462103-5a6ee13a3f0b-0001'."
            ),
            "issue_id":numeric_id_property(
                "Positive numeric Jira issue ID associated with the execution."
            ),
            "project_id":numeric_id_property(
                "Positive numeric Jira project ID associated with the execution."
            )
        },
        "required":["execution_id","issue_id","project_id"],
        "additionalProperties":false
    })
}

fn numeric_id_property(description: &str) -> Value {
    json!({
        "type":"integer",
        "minimum":1,
        "maximum":i64::MAX,
        "description":description
    })
}

fn opaque_id_property(description: &str) -> Value {
    json!({
        "type":"string",
        "minLength":1,
        "maxLength":MAX_OPAQUE_ID_BYTES,
        "pattern":"^[A-Za-z0-9_-]+$",
        "description":description
    })
}

fn json_string_property(description: &str) -> Value {
    json!({
        "type":"string",
        "minLength":2,
        "maxLength":MAX_JSON_BYTES,
        "description":description
    })
}

fn empty_schema() -> Value {
    json!({"type":"object","properties":{},"additionalProperties":false})
}

fn validate_argument_size(arguments: &Value) -> Result<(), AdkError> {
    if serde_json::to_vec(arguments)
        .map_err(|_| invalid_arguments())?
        .len()
        > MAX_ARGUMENT_BYTES
    {
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

fn positive_id(arguments: &Map<String, Value>, key: &str) -> Result<u64, AdkError> {
    arguments
        .get(key)
        .and_then(Value::as_u64)
        .filter(|value| *value > 0 && *value <= i64::MAX.cast_unsigned())
        .ok_or_else(invalid_arguments)
}

fn opaque_id<'a>(arguments: &'a Map<String, Value>, key: &str) -> Result<&'a str, AdkError> {
    arguments
        .get(key)
        .and_then(Value::as_str)
        .filter(|value| {
            !value.is_empty()
                && value.len() <= MAX_OPAQUE_ID_BYTES
                && value
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
        })
        .ok_or_else(invalid_arguments)
}

fn gherkin(arguments: &Map<String, Value>) -> Result<&str, AdkError> {
    arguments
        .get("new_content")
        .and_then(Value::as_str)
        .filter(|value| {
            !value.is_empty() && value.len() <= MAX_GHERKIN_BYTES && !value.contains('\0')
        })
        .ok_or_else(invalid_arguments)
}

#[derive(Clone, Copy)]
enum PayloadKind {
    UpdateStep,
    CreateStep,
    Cycle,
    Folder,
    TestsToCycle,
    TestsToFolder,
    Execution,
}

fn json_object(arguments: &Map<String, Value>, kind: PayloadKind) -> Result<Value, AdkError> {
    let encoded = arguments
        .get("json")
        .and_then(Value::as_str)
        .filter(|value| (2..=MAX_JSON_BYTES).contains(&value.len()))
        .ok_or_else(invalid_arguments)?;
    let value: Value = serde_json::from_str(encoded).map_err(|_| invalid_arguments())?;
    let object = value.as_object().ok_or_else(invalid_arguments)?;
    let mut nodes = 0;
    validate_json_bounds(&value, 0, &mut nodes)?;
    validate_payload(kind, object)?;
    Ok(value)
}

fn validate_json_bounds(value: &Value, depth: usize, nodes: &mut usize) -> Result<(), AdkError> {
    if depth > MAX_JSON_DEPTH {
        return Err(resource_exhausted_arguments());
    }
    *nodes = nodes.saturating_add(1);
    if *nodes > MAX_JSON_NODES {
        return Err(resource_exhausted_arguments());
    }
    match value {
        Value::Object(object) => {
            if object.len() > MAX_JSON_FIELDS {
                return Err(resource_exhausted_arguments());
            }
            for (key, value) in object {
                if key.is_empty() || key.len() > MAX_OPAQUE_ID_BYTES || key.contains('\0') {
                    return Err(invalid_arguments());
                }
                validate_json_bounds(value, depth + 1, nodes)?;
            }
        }
        Value::Array(values) => {
            for value in values {
                validate_json_bounds(value, depth + 1, nodes)?;
            }
        }
        Value::String(value) if value.len() > MAX_JSON_BYTES || value.contains('\0') => {
            return Err(resource_exhausted_arguments());
        }
        _ => {}
    }
    Ok(())
}

fn validate_payload(kind: PayloadKind, object: &Map<String, Value>) -> Result<(), AdkError> {
    match kind {
        PayloadKind::UpdateStep => {
            require_string(object, "id")?;
            require_string(object, "step")?;
        }
        PayloadKind::CreateStep => {
            require_string(object, "step")?;
        }
        PayloadKind::Cycle => {
            require_string(object, "name")?;
            require_positive_number(object, "projectId")?;
        }
        PayloadKind::Folder => {
            require_string(object, "name")?;
            require_string(object, "cycleId")?;
            require_positive_number(object, "projectId")?;
            require_integer(object, "versionId")?;
        }
        PayloadKind::TestsToCycle => validate_tests_to_cycle(object)?,
        PayloadKind::TestsToFolder => {
            require_nonempty_array(object, "issues")?;
            require_string(object, "assigneeType")?;
            require_integer(object, "method")?;
            require_integer(object, "versionId")?;
            require_positive_number(object, "projectId")?;
            require_string(object, "cycleId")?;
        }
        PayloadKind::Execution => {
            require_positive_number(object, "projectId")?;
            require_positive_number(object, "issueId")?;
            require_integer(object, "versionId")?;
        }
    }
    Ok(())
}

fn validate_tests_to_cycle(object: &Map<String, Value>) -> Result<(), AdkError> {
    require_integer(object, "versionId")?;
    require_positive_number(object, "projectId")?;
    let method = object
        .get("method")
        .and_then(Value::as_str)
        .ok_or_else(invalid_arguments)?;
    match method {
        "1" => require_nonempty_array(object, "issues"),
        "2" => require_string(object, "jql").map(drop),
        "3" => {
            require_integer(object, "fromVersionId")?;
            require_string(object, "fromCycleId").map(drop)
        }
        _ => Err(invalid_arguments()),
    }
}

fn require_string<'a>(object: &'a Map<String, Value>, key: &str) -> Result<&'a str, AdkError> {
    object
        .get(key)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .ok_or_else(invalid_arguments)
}

fn require_integer(object: &Map<String, Value>, key: &str) -> Result<i64, AdkError> {
    object
        .get(key)
        .and_then(Value::as_i64)
        .ok_or_else(invalid_arguments)
}

fn require_positive_number(object: &Map<String, Value>, key: &str) -> Result<u64, AdkError> {
    object
        .get(key)
        .and_then(Value::as_u64)
        .filter(|value| *value > 0 && *value <= i64::MAX.cast_unsigned())
        .ok_or_else(invalid_arguments)
}

fn require_nonempty_array(object: &Map<String, Value>, key: &str) -> Result<(), AdkError> {
    object
        .get(key)
        .and_then(Value::as_array)
        .filter(|value| !value.is_empty())
        .map(|_| ())
        .ok_or_else(invalid_arguments)
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "zephyr_squad.arguments.invalid",
        "the Zephyr Squad tool arguments are invalid",
    )
}

fn resource_exhausted_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "zephyr_squad.arguments.resource_exhausted",
        "the Zephyr Squad tool arguments exceed the approved limit",
    )
}
