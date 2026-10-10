use std::sync::Arc;

use adk_core::AdkError;
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value};

use crate::toolkits::families::ado::client::{AdoClientError, IntoAdk};
use crate::toolkits::families::ado::toolset::{
    AdoToolExecutor, AdoToolKind, AdoToolsetError, MAX_IDENTIFIER_BYTES, MAX_TEXT_BYTES,
    build_toolset, invalid_arguments, optional_bool, optional_id, optional_str,
    optional_string_list, reject_unknown_keys, required_id, required_id_text, required_str, schema,
};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{AdoPlansClient, MAX_CREATE_TEST_CASES, NewTestCase};
use super::config::AdoPlansToolkitConfig;

pub(crate) const TOOLKIT_TYPE: &str = "ado_plans";

const TEST_SUITE_PARAMS_DESCRIPTION: &str = "JSON of the test suite create parameters.\n    test_suite_create_params model:\n    {\n        'default_configurations': '[TestConfigurationReference]',\n        'default_testers': '[IdentityRef]',\n        'inherit_default_configurations': 'bool',\n        'name': 'str',\n        'parent_suite': '[TestSuiteReference]',\n        'query_string':'str',\n        'requirement_id':'int',\n        'suite_type': 'object'\n    }\n    default_configurations model:\n    {\n        'id':'int',\n        'name':'str'\n    }\n    default_testers model:\n    {\n        '_links':'dict',\n        'descriptor':'str',\n        'display_name':'str',\n        'url':'str',\n        'directory_alias':'str',\n        'id':'str',\n        'image_url':'str',\n        'inactive':'bool',\n        'is_aad_identity':'bool',\n        'is_container':'bool',\n        'is_deleted_in_origin':'bool',\n        'profile_url':'str',\n        'unique_name':'str'\n    }\n    parent_suite model:\n    {\n        'id':'int',\n        'name':'str'\n    }\n    ";

const TEST_STEPS_DESCRIPTION: &str = "Json or XML array string with test steps. \n    Json example: [{\"stepNumber\": 1, \"action\": \"Some action\", \"expectedResult\": \"Some expectation\"},...]\n    XML example: \n    <Steps>\n    <Step>\n      <StepNumber>1</StepNumber>\n      <Action>Some action</Action>\n      <ExpectedResult>Some expectation</ExpectedResult>\n    </Step>\n    ...\n    </Steps>\n    ";

const CREATE_TEST_CASES_DESCRIPTION: &str = "Json array where each object is separate test case to be created.\n    Input format:\n    [\n        {\n            plan_id: str\n            suite_id: str\n            title: str\n            description: str\n            test_steps: str\n            test_steps_format: str\n            additional_fields: str (optional)\n        }\n        ...\n    ]\n    Where:\n    plan_id - ID of the test plan to which test cases are to be added;\n    suite_id - ID of the test suite to which test cases are to be added\n    title - Test case title;\n    description - Test case description;\n    test_steps - Json or XML array string with test steps (see create_test_case)\n    test_steps_format - Format of provided test steps. Possible values: json, xml\n    additional_fields - (Optional) JSON string of additional custom fields as key-value pairs. Example: '{\"SDLC\": \"Development\", \"Priority\": \"High\"}'\n    ";

/// Every non-index `ado_plans` tool.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum AdoPlansToolKind {
    CreateTestPlan,
    DeleteTestPlan,
    GetTestPlan,
    CreateTestSuite,
    DeleteTestSuite,
    GetTestSuite,
    AddTestCase,
    CreateTestCase,
    CreateTestCases,
    GetTestCase,
    GetTestCases,
    GetAllTestCaseFieldsForProject,
}

impl AdoPlansToolKind {
    pub(crate) const ALL: [Self; 12] = [
        Self::CreateTestPlan,
        Self::DeleteTestPlan,
        Self::GetTestPlan,
        Self::CreateTestSuite,
        Self::DeleteTestSuite,
        Self::GetTestSuite,
        Self::AddTestCase,
        Self::CreateTestCase,
        Self::CreateTestCases,
        Self::GetTestCase,
        Self::GetTestCases,
        Self::GetAllTestCaseFieldsForProject,
    ];
}

fn plan_id(description: &str) -> Value {
    schema::integer(description)
}

fn optional_fields() -> Value {
    schema::optional_string_array(
        "List of specific work item field names to return. If not provided, all fields are returned. Example: ['System.Title', 'System.State', 'Custom.SDLC']",
    )
}

impl AdoToolKind for AdoPlansToolKind {
    fn name(self) -> &'static str {
        match self {
            Self::CreateTestPlan => "create_test_plan",
            Self::DeleteTestPlan => "delete_test_plan",
            Self::GetTestPlan => "get_test_plan",
            Self::CreateTestSuite => "create_test_suite",
            Self::DeleteTestSuite => "delete_test_suite",
            Self::GetTestSuite => "get_test_suite",
            Self::AddTestCase => "add_test_case",
            Self::CreateTestCase => "create_test_case",
            Self::CreateTestCases => "create_test_cases",
            Self::GetTestCase => "get_test_case",
            Self::GetTestCases => "get_test_cases",
            Self::GetAllTestCaseFieldsForProject => "get_all_test_case_fields_for_project",
        }
    }

    fn description(self) -> &'static str {
        match self {
            Self::CreateTestPlan => {
                "Create a test plan in Azure DevOps from TestPlanCreateParams JSON with snake_case keys such as name, area_path, iteration, start_date and end_date."
            }
            Self::DeleteTestPlan => "Delete a test plan in Azure DevOps.",
            Self::GetTestPlan => {
                "Get a test plan or list of test plans in Azure DevOps. Without plan_id every plan of the project is listed."
            }
            Self::CreateTestSuite => "Create a test suite in Azure DevOps.",
            Self::DeleteTestSuite => "Delete a test suite in Azure DevOps.",
            Self::GetTestSuite => {
                "Get a test suite or list of test suites in Azure DevOps. Without suite_id every suite of the plan is listed."
            }
            Self::AddTestCase => {
                "Add a test case to a suite in Azure DevOps. Takes a JSON array such as [{\"work_item\":{\"id\":\"23\"}}]."
            }
            Self::CreateTestCase => {
                "Creates a new test case in specified suite in Azure DevOps: a Test Case work item with title, description and steps (JSON or XML), plus optional additional_fields, added to the suite. Use get_all_test_case_fields_for_project to find required fields."
            }
            Self::CreateTestCases => {
                "Creates new test cases in specified suite in Azure DevOps, one per entry of a JSON array (at most 50)."
            }
            Self::GetTestCase => {
                "Get a test case from a suite in Azure DevOps with all custom fields (work_item_full_details)."
            }
            Self::GetTestCases => {
                "Get test cases from a suite in Azure DevOps with all custom fields (work_item_full_details for each)."
            }
            Self::GetAllTestCaseFieldsForProject => {
                "Get formatted information about available Test Case fields and their metadata. This method helps discover which fields are required for Test Case creation."
            }
        }
    }

    #[allow(clippy::too_many_lines)] // One schema per SDK tool keeps the catalogue auditable.
    fn schema(self) -> Value {
        match self {
            Self::CreateTestPlan => schema::object(
                &[(
                    "test_plan_create_params",
                    schema::string("JSON of the test plan create parameters"),
                )],
                &["test_plan_create_params"],
            ),
            Self::DeleteTestPlan => schema::object(
                &[("plan_id", plan_id("ID of the test plan to be deleted"))],
                &["plan_id"],
            ),
            Self::GetTestPlan => schema::object(
                &[(
                    "plan_id",
                    schema::optional_integer("ID of the test plan to get"),
                )],
                &[],
            ),
            Self::CreateTestSuite => schema::object(
                &[
                    (
                        "test_suite_create_params",
                        schema::string(TEST_SUITE_PARAMS_DESCRIPTION),
                    ),
                    (
                        "plan_id",
                        plan_id("ID of the test plan that contains the suites"),
                    ),
                ],
                &["test_suite_create_params", "plan_id"],
            ),
            Self::DeleteTestSuite => schema::object(
                &[
                    (
                        "plan_id",
                        plan_id("ID of the test plan that contains the suite"),
                    ),
                    ("suite_id", plan_id("ID of the test suite to delete")),
                ],
                &["plan_id", "suite_id"],
            ),
            Self::GetTestSuite => schema::object(
                &[
                    (
                        "plan_id",
                        plan_id("ID of the test plan that contains the suites"),
                    ),
                    (
                        "suite_id",
                        schema::optional_integer("ID of the suite to get"),
                    ),
                ],
                &["plan_id"],
            ),
            Self::AddTestCase => schema::object(
                &[
                    (
                        "suite_test_case_create_update_parameters",
                        schema::string(
                            "JSON array of the suite test case create update parameters. Example: \"[{\"work_item\":{\"id\":\"23\"}}]\"",
                        ),
                    ),
                    (
                        "plan_id",
                        plan_id("ID of the test plan to which test cases are to be added"),
                    ),
                    (
                        "suite_id",
                        plan_id("ID of the test suite to which test cases are to be added"),
                    ),
                ],
                &[
                    "suite_test_case_create_update_parameters",
                    "plan_id",
                    "suite_id",
                ],
            ),
            Self::CreateTestCase => schema::object(
                &[
                    (
                        "plan_id",
                        plan_id("ID of the test plan to which test cases are to be added"),
                    ),
                    (
                        "suite_id",
                        plan_id("ID of the test suite to which test cases are to be added"),
                    ),
                    ("title", schema::string("Test case title")),
                    ("description", schema::string("Test case description")),
                    ("test_steps", schema::string(TEST_STEPS_DESCRIPTION)),
                    (
                        "test_steps_format",
                        schema::optional_string(
                            "Format of provided test steps. Possible values: json, xml",
                            Some("json"),
                        ),
                    ),
                    (
                        "additional_fields",
                        schema::optional_string(
                            "JSON string of additional custom fields as key-value pairs. Example: '{\"SDLC\": \"Development\", \"Priority\": \"High\"}'. Use get_all_test_case_fields_for_project to discover required fields.",
                            None,
                        ),
                    ),
                ],
                &["plan_id", "suite_id", "title", "description", "test_steps"],
            ),
            Self::CreateTestCases => schema::object(
                &[(
                    "create_test_cases_parameters",
                    schema::string(CREATE_TEST_CASES_DESCRIPTION),
                )],
                &["create_test_cases_parameters"],
            ),
            Self::GetTestCase => schema::object(
                &[
                    (
                        "plan_id",
                        plan_id("ID of the test plan for which test cases are requested"),
                    ),
                    (
                        "suite_id",
                        plan_id("ID of the test suite for which test cases are requested"),
                    ),
                    ("test_case_id", schema::string("Test Case Id to be fetched")),
                    ("fields", optional_fields()),
                ],
                &["plan_id", "suite_id", "test_case_id"],
            ),
            Self::GetTestCases => schema::object(
                &[
                    (
                        "plan_id",
                        plan_id("ID of the test plan for which test cases are requested"),
                    ),
                    (
                        "suite_id",
                        plan_id("ID of the test suite for which test cases are requested"),
                    ),
                    ("fields", optional_fields()),
                ],
                &["plan_id", "suite_id"],
            ),
            Self::GetAllTestCaseFieldsForProject => schema::object(
                &[(
                    "force_refresh",
                    schema::optional_bool(
                        "If True, reload field definitions from Azure DevOps API. Use this if project configuration has changed.",
                        false,
                    ),
                )],
                &[],
            ),
        }
    }

    fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::GetTestPlan
                | Self::GetTestSuite
                | Self::GetTestCase
                | Self::GetTestCases
                | Self::GetAllTestCaseFieldsForProject
        )
    }
}

/// Build the served `ado_plans` tools.
pub(crate) fn build_ado_plans_toolset(
    toolkit_name: &str,
    config: AdoPlansToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, AdoToolsetError> {
    let selected = config.selected_tools().to_vec();
    let client = Arc::new(AdoPlansClient::new(config.into_inner())?);
    build_with_client(toolkit_name, &selected, policy, client)
}

pub(in crate::toolkits) fn build_with_client(
    toolkit_name: &str,
    selected: &[Box<str>],
    policy: &Arc<ToolAdmissionPolicy>,
    client: Arc<AdoPlansClient>,
) -> Result<BasicToolset, AdoToolsetError> {
    let executor: Arc<dyn AdoToolExecutor<AdoPlansToolKind>> = client;
    build_toolset(
        toolkit_name,
        TOOLKIT_TYPE,
        &AdoPlansToolKind::ALL,
        selected,
        policy,
        &executor,
        "",
    )
}

#[async_trait]
impl AdoToolExecutor<AdoPlansToolKind> for AdoPlansClient {
    async fn execute(
        &self,
        kind: AdoPlansToolKind,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        dispatch(self, kind, arguments)
            .await?
            .map_err(AdoClientError::into_adk)
    }
}

fn new_test_case(arguments: &Map<String, Value>) -> Result<NewTestCase, AdkError> {
    Ok(NewTestCase {
        plan_id: required_id(arguments, "plan_id")?,
        suite_id: required_id(arguments, "suite_id")?,
        title: required_str(arguments, "title", MAX_TEXT_BYTES)?.to_owned(),
        description: required_str(arguments, "description", MAX_TEXT_BYTES)?.to_owned(),
        test_steps: required_str(arguments, "test_steps", MAX_TEXT_BYTES)?.to_owned(),
        test_steps_format: optional_str(arguments, "test_steps_format", MAX_IDENTIFIER_BYTES)?
            .unwrap_or("json")
            .to_owned(),
        additional_fields: optional_str(arguments, "additional_fields", MAX_TEXT_BYTES)?
            .map(ToOwned::to_owned),
    })
}

/// `create_test_cases_parameters`: every entry is checked before the first
/// work item is created, so a malformed entry cannot leave a partial batch.
fn new_test_cases(encoded: &str) -> Result<Vec<NewTestCase>, AdkError> {
    let parsed: Value = serde_json::from_str(encoded).map_err(|_| invalid_arguments())?;
    let entries = parsed.as_array().ok_or_else(invalid_arguments)?;
    if entries.is_empty() || entries.len() > MAX_CREATE_TEST_CASES {
        return Err(invalid_arguments());
    }
    entries
        .iter()
        .map(|entry| {
            let entry = entry.as_object().ok_or_else(invalid_arguments)?;
            let mut normalized = entry.clone();
            normalized.retain(|_, value| !value.is_null());
            // `additional_fields` may be a JSON string or an object.
            if let Some(Value::Object(fields)) = normalized.get("additional_fields") {
                let encoded = Value::Object(fields.clone()).to_string();
                normalized.insert("additional_fields".to_owned(), Value::String(encoded));
            }
            new_test_case(&normalized)
        })
        .collect()
}

#[allow(clippy::too_many_lines)] // One source-ordered argument ledger per tool.
async fn dispatch(
    client: &AdoPlansClient,
    kind: AdoPlansToolKind,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Result<Value, AdoClientError>> {
    Ok(match kind {
        AdoPlansToolKind::CreateTestPlan => {
            reject_unknown_keys(arguments, &["test_plan_create_params"])?;
            client
                .create_test_plan(required_str(
                    arguments,
                    "test_plan_create_params",
                    MAX_TEXT_BYTES,
                )?)
                .await
        }
        AdoPlansToolKind::DeleteTestPlan => {
            reject_unknown_keys(arguments, &["plan_id"])?;
            client
                .delete_test_plan(required_id(arguments, "plan_id")?)
                .await
        }
        AdoPlansToolKind::GetTestPlan => {
            reject_unknown_keys(arguments, &["plan_id"])?;
            // `if plan_id:` — zero lists every plan.
            let plan_id = match arguments.get("plan_id") {
                Some(Value::Number(number)) if number.as_i64() == Some(0) => None,
                _ => optional_id(arguments, "plan_id")?,
            };
            client.get_test_plan(plan_id).await
        }
        AdoPlansToolKind::CreateTestSuite => {
            reject_unknown_keys(arguments, &["test_suite_create_params", "plan_id"])?;
            client
                .create_test_suite(
                    required_str(arguments, "test_suite_create_params", MAX_TEXT_BYTES)?,
                    required_id(arguments, "plan_id")?,
                )
                .await
        }
        AdoPlansToolKind::DeleteTestSuite => {
            reject_unknown_keys(arguments, &["plan_id", "suite_id"])?;
            client
                .delete_test_suite(
                    required_id(arguments, "plan_id")?,
                    required_id(arguments, "suite_id")?,
                )
                .await
        }
        AdoPlansToolKind::GetTestSuite => {
            reject_unknown_keys(arguments, &["plan_id", "suite_id"])?;
            let suite_id = match arguments.get("suite_id") {
                Some(Value::Number(number)) if number.as_i64() == Some(0) => None,
                _ => optional_id(arguments, "suite_id")?,
            };
            client
                .get_test_suite(required_id(arguments, "plan_id")?, suite_id)
                .await
        }
        AdoPlansToolKind::AddTestCase => {
            reject_unknown_keys(
                arguments,
                &[
                    "suite_test_case_create_update_parameters",
                    "plan_id",
                    "suite_id",
                ],
            )?;
            client
                .add_test_case(
                    required_str(
                        arguments,
                        "suite_test_case_create_update_parameters",
                        MAX_TEXT_BYTES,
                    )?,
                    required_id(arguments, "plan_id")?,
                    required_id(arguments, "suite_id")?,
                )
                .await
        }
        AdoPlansToolKind::CreateTestCase => {
            reject_unknown_keys(
                arguments,
                &[
                    "plan_id",
                    "suite_id",
                    "title",
                    "description",
                    "test_steps",
                    "test_steps_format",
                    "additional_fields",
                ],
            )?;
            client.create_test_case(&new_test_case(arguments)?).await
        }
        AdoPlansToolKind::CreateTestCases => {
            reject_unknown_keys(arguments, &["create_test_cases_parameters"])?;
            let cases = new_test_cases(required_str(
                arguments,
                "create_test_cases_parameters",
                MAX_TEXT_BYTES,
            )?)?;
            client.create_test_cases(&cases).await
        }
        AdoPlansToolKind::GetTestCase => {
            reject_unknown_keys(
                arguments,
                &["plan_id", "suite_id", "test_case_id", "fields"],
            )?;
            let test_case_id = required_id_text(arguments, "test_case_id")?;
            if test_case_id.trim().is_empty() {
                return Err(invalid_arguments());
            }
            let fields = optional_string_list(arguments, "fields")?;
            client
                .get_test_case(
                    required_id(arguments, "plan_id")?,
                    required_id(arguments, "suite_id")?,
                    test_case_id.trim(),
                    fields.as_deref(),
                )
                .await
        }
        AdoPlansToolKind::GetTestCases => {
            reject_unknown_keys(arguments, &["plan_id", "suite_id", "fields"])?;
            let fields = optional_string_list(arguments, "fields")?;
            client
                .get_test_cases(
                    required_id(arguments, "plan_id")?,
                    required_id(arguments, "suite_id")?,
                    fields.as_deref(),
                )
                .await
        }
        AdoPlansToolKind::GetAllTestCaseFieldsForProject => {
            reject_unknown_keys(arguments, &["force_refresh"])?;
            client
                .get_all_test_case_fields_for_project(optional_bool(
                    arguments,
                    "force_refresh",
                    false,
                )?)
                .await
        }
    })
}
