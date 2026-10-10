use std::cmp::Ordering;
use std::sync::Arc;

use adk_tool::BasicToolset;
use async_trait::async_trait;
use reqwest::Method;
use serde_json::{Map, Value, json};

use crate::toolkits::families::zephyr_rest::client::{
    ZephyrRestClient, ZephyrRestError, ZephyrRestFamily, bounded_text, resource_exhausted,
};
use crate::toolkits::families::zephyr_rest::tools::{
    Arguments, ZephyrArgumentCodes, ZephyrRestToolsetError, ZephyrToolSpec, build_toolset,
    decode_json, identifier_property, integer_property, json_text_property, object_schema,
    optional_bool_property, optional_identifier_property, optional_integer_property,
    optional_string_list_property, optional_text_property, text_property,
};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{FAMILY, Query, ZephyrScaleClient};
use super::config::ZephyrScaleToolkitConfig;
use super::folders::{FolderTree, find_by_name, parsed_tests_repr};
use super::render::{OrderedObject, json_dumps_indent, reply_str};
use crate::toolkits::families::python_repr;

const TOOLKIT_TYPE: &str = "zephyr_scale";
const MAX_CASES_PER_BATCH: usize = 50;
const MAX_ITEMS: usize = 10_000;
/// Test cases whose steps one `steps_search` may read (one request each).
const MAX_STEP_SEARCH_CASES: usize = 500;

static ARGUMENTS: ZephyrArgumentCodes = ZephyrArgumentCodes {
    label: "Zephyr Scale",
    invalid: "zephyr_scale.arguments.invalid",
    exhausted: "zephyr_scale.arguments.resource_exhausted",
};

const ADDITIONAL_FIELDS: &str = "JSON containing additional optional fields such as: 'objective' (description of the objective), 'precondition' (any conditions that need to be met), 'estimatedTime' (estimated duration in milliseconds), 'componentId' (ID of a component from Jira), 'priorityName' (the priority name), 'statusName' (the status name), 'folderId' (ID of a folder to place the entity within), 'ownerId' (Atlassian Account ID of the Jira user), 'labels' (array of labels associated to this entity), 'customFields' (object containing custom fields such as build number, release date, etc.).Dates should be in the format 'yyyy-MM-dd', and multi-line text fields should denote a new line with the <br> syntax.";
const TEST_STEPS: &str = "JSON representing the list of test steps. Each step should be an object containing either 'inline' or 'testCase'. They should only include one of these fields at a time. Example: [{'inline': {'description': 'Attempt to login to the application', 'testData': 'Username = SmartBear Password = weLoveAtlassian', 'expectedResult': 'Login succeeds, web-app redirects to the dashboard view', 'customFields': {'Build Number': 20, 'Release Date': '2020-01-01', 'Implemented': false, 'Category': ['Performance', 'Regression']}}, 'testCase': {'self': 'string', 'testCaseKey': 'PROJ-T123', 'parameters': [{'name': 'username', 'type': 'DEFAULT_VALUE', 'value': 'admin'}]}}]";
const MAX_RESULTS_HINT: &str =
    "A hint as to the maximum number of results to return in each call. Must be an integer >= 1.";
const START_AT_HINT: &str = "Zero-indexed starting position. Should be a multiple of maxResults.";

/// Build the twenty Zephyr Scale business tools.
pub(crate) fn build_zephyr_scale_toolset(
    toolkit_name: &str,
    config: ZephyrScaleToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, ZephyrRestToolsetError> {
    let (base_url, token, selected) = config.into_parts();
    let client = Arc::new(ZephyrScaleClient::new(ZephyrRestClient::new(
        base_url, token,
    )?));
    build_with_client(toolkit_name, &selected, policy, &client)
}

fn build_with_client(
    toolkit_name: &str,
    selected: &[Box<str>],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<ZephyrScaleClient>,
) -> Result<BasicToolset, ZephyrRestToolsetError> {
    build_toolset(
        toolkit_name,
        TOOLKIT_TYPE,
        &ScaleTool::ALL,
        selected,
        None,
        policy,
        client,
    )
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_client(
    toolkit_name: &str,
    selected: &[&str],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<ZephyrScaleClient>,
) -> Result<BasicToolset, ZephyrRestToolsetError> {
    let selected = selected
        .iter()
        .map(|name| Box::<str>::from(*name))
        .collect::<Vec<_>>();
    build_with_client(toolkit_name, &selected, policy, client)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_catalog() -> Vec<(&'static str, bool)> {
    ScaleTool::ALL
        .iter()
        .map(|kind| (kind.name(), kind.is_read_only()))
        .collect()
}

#[derive(Clone, Copy)]
enum ScaleTool {
    GetTests,
    GetTest,
    GetTestSteps,
    CreateTestCase,
    CreateTestCases,
    AddTestSteps,
    UpdateTestSteps,
    GetFolders,
    UpdateTestCase,
    GetLinks,
    CreateIssueLinks,
    CreateWebLinks,
    GetVersions,
    GetVersion,
    GetTestScript,
    CreateTestScript,
    SearchTestCases,
    GetTestsRecursive,
    GetTestsByFolderName,
    GetTestsByFolderPath,
}

impl ScaleTool {
    const ALL: [Self; 20] = [
        Self::GetTests,
        Self::GetTest,
        Self::GetTestSteps,
        Self::CreateTestCase,
        Self::CreateTestCases,
        Self::AddTestSteps,
        Self::UpdateTestSteps,
        Self::GetFolders,
        Self::UpdateTestCase,
        Self::GetLinks,
        Self::CreateIssueLinks,
        Self::CreateWebLinks,
        Self::GetVersions,
        Self::GetVersion,
        Self::GetTestScript,
        Self::CreateTestScript,
        Self::SearchTestCases,
        Self::GetTestsRecursive,
        Self::GetTestsByFolderName,
        Self::GetTestsByFolderPath,
    ];
}

fn key_schema(title: &str) -> Value {
    object_schema(
        title,
        &[(
            "test_case_key",
            identifier_property("The key of the test case"),
        )],
        &["test_case_key"],
    )
}

fn paging_properties(max_default: u32) -> [(&'static str, Value); 2] {
    [
        (
            "maxResults",
            json!({"type":["integer","null"],"minimum":1,"description":format!("{MAX_RESULTS_HINT} Default {max_default}.")}),
        ),
        (
            "startAt",
            json!({"type":["integer","null"],"minimum":0,"description":format!("{START_AT_HINT} Default 0.")}),
        ),
    ]
}

#[async_trait]
impl ZephyrToolSpec for ScaleTool {
    type Api = ZephyrScaleClient;

    fn name(self) -> &'static str {
        match self {
            Self::GetTests => "get_tests",
            Self::GetTest => "get_test",
            Self::GetTestSteps => "get_test_steps",
            Self::CreateTestCase => "create_test_case",
            Self::CreateTestCases => "create_test_cases",
            Self::AddTestSteps => "add_test_steps",
            Self::UpdateTestSteps => "update_test_steps",
            Self::GetFolders => "get_folders",
            Self::UpdateTestCase => "update_test_case",
            Self::GetLinks => "get_links",
            Self::CreateIssueLinks => "create_issue_links",
            Self::CreateWebLinks => "create_web_links",
            Self::GetVersions => "get_versions",
            Self::GetVersion => "get_version",
            Self::GetTestScript => "get_test_script",
            Self::CreateTestScript => "create_test_script",
            Self::SearchTestCases => "search_test_cases",
            Self::GetTestsRecursive => "get_tests_recursive",
            Self::GetTestsByFolderName => "get_tests_by_folder_name",
            Self::GetTestsByFolderPath => "get_tests_by_folder_path",
        }
    }

    fn description(self) -> &'static str {
        match self {
            Self::GetTests => {
                "Retrieves all test cases. Query parameters can be used to filter the results.\n\nArgs:\n    project_key: Jira project key filter\n    folder_id: Folder ID filter\n    maxResults: A hint as to the maximum number of results to return in each call\n    startAt: Zero-indexed starting position. Should be a multiple of maxResults\n\nEvery page from startAt on is read; the result lists id, key, name, project, precondition, priority, status and owner of each test case."
            }
            Self::GetTest => {
                "Returns a test case for the given key\n\nArgs:\n    test_case_key: The key of the test case"
            }
            Self::GetTestSteps => {
                "Returns the test steps for the given test case. Provides a paged response.\n\nArgs:\n    test_case_key: The key of the test case"
            }
            Self::CreateTestCase => {
                "Creates a test case. Fields priorityName and statusName will be set to default values if not informed.\nArgs:\n    project_key: Jira project key\n    test_case_name: Test case name\n    additional_fields: JSON string containing additional optional fields\n    steps: JSON string representing the list of test steps\n\nNOTE: Please note that if the user specifies a folder name, it is necessary to execute the get_folders() function first to find the correct mapping. Creation can duplicate on retry; read before retrying after an unknown outcome."
            }
            Self::CreateTestCases => {
                "Creates a bunch of test cases. Returns one result line per case; a failed case does not stop the others, so read the project before retrying a batch."
            }
            Self::AddTestSteps => {
                "Assigns a series of test steps to a test case.\n\nArgs:\n    test_case_key: The key of the test case\n    tc_mode: Valid values: 'APPEND', 'OVERWRITE'\n    items: JSON string representing the list of test steps\n\nAPPEND can duplicate steps on retry; read the steps before retrying after an unknown outcome."
            }
            Self::UpdateTestSteps => {
                "Updates specific test steps in a test case.\n\nArgs:\n    test_case_key: The key of the test case\n    steps_updates: JSON string representing the test steps to update. Format:\n        [{\"index\": 0, \"description\": \"Updated step description\", \"testData\": \"Updated test data\", \"expectedResult\": \"Updated expected result\"}]\n\nReads every step, applies the updates by zero-based index, and writes all steps back with OVERWRITE.\nReturns: A confirmation message with the update result"
            }
            Self::GetFolders => {
                "Retrieves all folders. Query parameters can be used to filter the results: maxResults, startAt, projectKey, folderType"
            }
            Self::UpdateTestCase => {
                "Updates an existing test case.\n\nArgs:\n    test_case_key: The key of the test case\n    test_case_id: Integer id of the test\n    name: Test case name\n    project_id: Project id\n    priority_id: Priority id\n    status_id: Status id\n    additional_fields: JSON string with any additional fields to update"
            }
            Self::GetLinks => {
                "Returns links for a test case with specified key\n\nArgs:\n    test_case_key: The key of the test case"
            }
            Self::CreateIssueLinks => {
                "Creates a link between a test case and a Jira issue\n\nArgs:\n    test_case_key: The key of the test case\n    issue_id: The ID of the Jira issue to link\nNOTE: The issue ID should be a valid Jira issue ID. If JIRA issue key is provided instead, it's requrid to get issue id first either by asking user or by using JIRA tooking (if avialable)."
            }
            Self::CreateWebLinks => {
                "Creates a link between a test case and a generic URL\n\nArgs:\n    test_case_key: The key of the test case\n    url: The URL to link\n    description: The text to display for the link\n    additional_fields: JSON string containing any additional optional fields for the web link"
            }
            Self::GetVersions => {
                "Returns all test case versions for a test case with specified key. Response is ordered by most recent first.\n\nArgs:\n    test_case_key: The key of the test case\n    maxResults: A hint as to the maximum number of results to return in each call\n    startAt: Zero-indexed starting position. Should be a multiple of maxResults"
            }
            Self::GetVersion => "Retrieves a specific version of a test case",
            Self::GetTestScript => "Returns the test script for the given test case",
            Self::CreateTestScript => {
                "Creates or updates the test script for a test case. Existing test steps of the test case are removed by the provider."
            }
            Self::SearchTestCases => {
                "Searches for test cases using custom search API.\n\nArgs:\n    project_key: Jira project key (e.g., \"SIT\", \"PROJ\")\n    search_term: Optional search term to filter test cases\n    max_results: Maximum number of results to query from the API\n    start_at: Zero-indexed starting position\n    order_by: Field to order results by\n    order_direction: Order direction (ASC or DESC)\n    archived: Include archived test cases\n    fields: Fields to include in the response (default: key, name)\n    limit_results: Maximum number of filtered results to return\n    folder_id / folder_name / exact_folder_match / folder_path / include_subfolders: folder filters\n    labels: Filter test cases by labels\n    custom_fields: JSON string containing custom field filters (e.g., {\"Country\": \"All\", \"Is Automated\": \"Yes\"})\n    steps_search: Search term to find in test case steps (description, expected result, or test data)\n    include_steps: Whether to include test steps in the response"
            }
            Self::GetTestsRecursive => {
                "Retrieves all test cases recursively from a folder and all its subfolders.\n\nArgs:\n    project_key: Jira project key filter\n    folder_id: Parent folder ID to start the recursive search from\n    maxResults: A hint as to the maximum number of results to return in each call\n    startAt: Zero-indexed starting position. Should be a multiple of maxResults\n\nReturns:\n    A string with all test cases found in the folder and its subfolders"
            }
            Self::GetTestsByFolderName => {
                "Retrieves all test cases from folders matching the specified name.\n\nArgs:\n    project_key: Jira project key filter\n    folder_name: Full or partial folder name to search for\n    exact_match: Whether to match the folder name exactly or allow partial matches\n    include_subfolders: Whether to include test cases from subfolders of matching folders\n    maxResults: A hint as to the maximum number of results to return in each call\n    startAt: Zero-indexed starting position. Should be a multiple of maxResults\n\nReturns:\n    A string with all test cases found in matching folders"
            }
            Self::GetTestsByFolderPath => {
                "Retrieves all test cases from a folder specified by its path.\n\nArgs:\n    project_key: Jira project key filter\n    folder_path: Full folder path (e.g., 'Root/Parent/Child')\n    include_subfolders: Whether to include test cases from subfolders\n    maxResults: A hint as to the maximum number of results to return in each call\n    startAt: Zero-indexed starting position. Should be a multiple of maxResults\n\nReturns:\n    A string with all test cases found in the specified folder path"
            }
        }
    }

    fn is_read_only(self) -> bool {
        !matches!(
            self,
            Self::CreateTestCase
                | Self::CreateTestCases
                | Self::AddTestSteps
                | Self::UpdateTestSteps
                | Self::UpdateTestCase
                | Self::CreateIssueLinks
                | Self::CreateWebLinks
                | Self::CreateTestScript
        )
    }

    #[allow(clippy::too_many_lines)] // One schema per SDK model keeps the contract reviewable.
    fn schema(self) -> Value {
        match self {
            Self::GetTests => {
                let [max, start] = paging_properties(10);
                object_schema(
                    "ZephyrGetTestCases",
                    &[
                        (
                            "project_key",
                            identifier_property("Jira project key filter"),
                        ),
                        (
                            "folder_id",
                            optional_identifier_property("Folder ID filter"),
                        ),
                        max,
                        start,
                    ],
                    &["project_key"],
                )
            }
            Self::GetTest | Self::GetTestSteps => key_schema("ZephyrGetTestCase"),
            Self::CreateTestCase => object_schema(
                "TestCaseInput",
                &[
                    ("project_key", identifier_property("Jira project key.")),
                    ("test_case_name", text_property("Name of the test case.")),
                    (
                        "additional_fields",
                        json_text_property(&format!("{ADDITIONAL_FIELDS} Default '{{}}'.")),
                    ),
                    ("steps", optional_text_property(TEST_STEPS)),
                ],
                &["project_key", "test_case_name"],
            ),
            Self::CreateTestCases => object_schema(
                "TestCasesInput",
                &[(
                    "create_test_cases_data",
                    json_text_property(&format!(
                        "Json with list of test cases to create in format: [{{project_key: str, test_case_name:str, additional_fields: {{obj}}, steps:[{{obj}}, ...] }}, ...] where: 'project_key' (required) - Jira project key. 'test_case_name' (required) - Name of the test case. 'additional_fields' - {ADDITIONAL_FIELDS} Could be empty if no additional fields needed: '{{}}'. 'steps' (optional) - {TEST_STEPS} At most {MAX_CASES_PER_BATCH} test cases."
                    )),
                )],
                &["create_test_cases_data"],
            ),
            Self::AddTestSteps => object_schema(
                "ZephyrTestStepsInputModel",
                &[
                    (
                        "test_case_key",
                        identifier_property(
                            "The key of the test case. Test case keys are of the format [A-Z]+-T[0-9]+",
                        ),
                    ),
                    (
                        "tc_mode",
                        text_property(
                            "Valid values: 'APPEND', 'OVERWRITE'. 'OVERWRITE' deletes and recreates the test steps and associated custom field values using the provided input. Attachments for existing steps are kept, but those for missing steps are deleted permanently. 'APPEND' only adds extra steps to your test steps.",
                        ),
                    ),
                    ("items", json_text_property(TEST_STEPS)),
                ],
                &["test_case_key", "tc_mode", "items"],
            ),
            Self::UpdateTestSteps => object_schema(
                "ZephyrUpdateTestSteps",
                &[
                    (
                        "test_case_key",
                        identifier_property("The key of the test case"),
                    ),
                    (
                        "steps_updates",
                        json_text_property(
                            "JSON string representing the test steps to update. Format: [{\"index\": 0, \"description\": \"Updated step description\", \"testData\": \"Updated test data\", \"expectedResult\": \"Updated expected result\"}]",
                        ),
                    ),
                ],
                &["test_case_key", "steps_updates"],
            ),
            Self::GetFolders => {
                let [max, start] = paging_properties(10);
                object_schema(
                    "ZephyrGetFolders",
                    &[
                        max,
                        start,
                        (
                            "projectKey",
                            optional_identifier_property(
                                "Jira project key filter. Must match the pattern [A-Z][A-Z_0-9]+.",
                            ),
                        ),
                        (
                            "folderType",
                            optional_identifier_property(
                                "Folder type filter. Valid values are 'TEST_CASE', 'TEST_PLAN', or 'TEST_CYCLE'.",
                            ),
                        ),
                    ],
                    &[],
                )
            }
            Self::UpdateTestCase => object_schema(
                "ZephyrUpdateTestCase",
                &[
                    (
                        "test_case_key",
                        identifier_property("The key of the test case"),
                    ),
                    ("test_case_id", integer_property("Integer id of the test")),
                    ("name", text_property("Test case name")),
                    ("project_id", integer_property("Project id")),
                    ("priority_id", integer_property("Priority id")),
                    ("status_id", integer_property("Status id")),
                    (
                        "additional_fields",
                        json_text_property(
                            "JSON string containing any additional fields that need to be updated. Default '{}'.",
                        ),
                    ),
                ],
                &[
                    "test_case_key",
                    "test_case_id",
                    "name",
                    "project_id",
                    "priority_id",
                    "status_id",
                ],
            ),
            Self::GetLinks => key_schema("ZephyrGetLinks"),
            Self::CreateIssueLinks => object_schema(
                "ZephyrCreateIssueLinks",
                &[
                    (
                        "test_case_key",
                        identifier_property("The key of the test case"),
                    ),
                    (
                        "issue_id",
                        integer_property("The ID of the Jira issue to link"),
                    ),
                ],
                &["test_case_key", "issue_id"],
            ),
            Self::CreateWebLinks => object_schema(
                "ZephyrCreateWebLinks",
                &[
                    (
                        "test_case_key",
                        identifier_property("The key of the test case"),
                    ),
                    ("url", text_property("The URL to link")),
                    (
                        "description",
                        text_property("The text to display for the link"),
                    ),
                    (
                        "additional_fields",
                        json_text_property(
                            "JSON string containing any additional optional fields for the web link. Default '{}'.",
                        ),
                    ),
                ],
                &["test_case_key", "url", "description"],
            ),
            Self::GetVersions => {
                let [max, start] = paging_properties(10);
                object_schema(
                    "ZephyrGetVersions",
                    &[
                        (
                            "test_case_key",
                            identifier_property("The key of the test case"),
                        ),
                        max,
                        start,
                    ],
                    &["test_case_key"],
                )
            }
            Self::GetVersion => object_schema(
                "ZephyrGetVersion",
                &[
                    (
                        "test_case_key",
                        identifier_property("The key of the test case"),
                    ),
                    ("version", identifier_property("The version number")),
                ],
                &["test_case_key", "version"],
            ),
            Self::GetTestScript => key_schema("ZephyrGetTestScript"),
            Self::CreateTestScript => object_schema(
                "ZephyrCreateTestScript",
                &[
                    (
                        "test_case_key",
                        identifier_property("The key of the test case"),
                    ),
                    ("script_type", text_property("The type of the script")),
                    ("text", text_property("The text content of the script")),
                ],
                &["test_case_key", "script_type", "text"],
            ),
            Self::SearchTestCases => search_schema(),
            Self::GetTestsRecursive => {
                let [max, start] = paging_properties(100);
                object_schema(
                    "ZephyrGetTestsRecursive",
                    &[
                        (
                            "project_key",
                            optional_identifier_property("Jira project key filter"),
                        ),
                        (
                            "folder_id",
                            optional_identifier_property(
                                "Parent folder ID to start the recursive search from",
                            ),
                        ),
                        max,
                        start,
                    ],
                    &[],
                )
            }
            Self::GetTestsByFolderName => {
                let [max, start] = paging_properties(100);
                object_schema(
                    "ZephyrGetTestsByFolderName",
                    &[
                        (
                            "project_key",
                            identifier_property("Jira project key filter"),
                        ),
                        (
                            "folder_name",
                            text_property("Full or partial folder name to search for"),
                        ),
                        (
                            "exact_match",
                            optional_bool_property(
                                "Whether to match the folder name exactly or allow partial matches. Default false.",
                            ),
                        ),
                        (
                            "include_subfolders",
                            optional_bool_property(
                                "Whether to include test cases from subfolders. Default true.",
                            ),
                        ),
                        max,
                        start,
                    ],
                    &["project_key", "folder_name"],
                )
            }
            Self::GetTestsByFolderPath => {
                let [max, start] = paging_properties(100);
                object_schema(
                    "ZephyrGetTestsByFolderPath",
                    &[
                        (
                            "project_key",
                            identifier_property("Jira project key filter"),
                        ),
                        (
                            "folder_path",
                            text_property("Full folder path (e.g., 'Root/Parent/Child')"),
                        ),
                        (
                            "include_subfolders",
                            optional_bool_property(
                                "Whether to include test cases from subfolders. Default true.",
                            ),
                        ),
                        max,
                        start,
                    ],
                    &["project_key", "folder_path"],
                )
            }
        }
    }

    fn family() -> &'static ZephyrRestFamily {
        &FAMILY
    }

    fn arguments() -> &'static ZephyrArgumentCodes {
        &ARGUMENTS
    }

    async fn execute(
        self,
        api: &ZephyrScaleClient,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        let allowed = self
            .schema()
            .get("properties")
            .and_then(Value::as_object)
            .map(|properties| properties.keys().cloned().collect::<Vec<_>>())
            .unwrap_or_default();
        let allowed = allowed.iter().map(String::as_str).collect::<Vec<_>>();
        let args = Arguments::new(arguments, &ARGUMENTS, &allowed)?;
        dispatch(self, api, &args).await
    }
}

#[allow(clippy::too_many_lines)] // One arm per SDK operation keeps the catalogue reviewable.
async fn dispatch(
    kind: ScaleTool,
    api: &ZephyrScaleClient,
    args: &Arguments<'_>,
) -> adk_core::Result<Value> {
    match kind {
        ScaleTool::GetTests => get_tests(api, args).await,
        ScaleTool::GetTest => {
            let key = args.identifier("test_case_key")?;
            let reply = api
                .send(Method::GET, &["testcases", key], None)
                .await
                .map_err(adk)?;
            text(format!("Extracted tests: {}", reply_str(&reply)))
        }
        ScaleTool::GetTestSteps => {
            let key = args.identifier("test_case_key")?;
            let steps = api
                .paginated(&["testcases", key, "teststeps"], Vec::new())
                .await
                .map_err(adk)?;
            let lines = steps.iter().map(python_repr::str_of).collect::<Vec<_>>();
            text(format!("Extracted test steps: {}", lines.join("\n")))
        }
        ScaleTool::CreateTestCase => create_test_case(api, args).await,
        ScaleTool::CreateTestCases => create_test_cases(api, args).await,
        ScaleTool::AddTestSteps => {
            let key = args.identifier("test_case_key")?;
            let mode = args.text("tc_mode")?;
            let items = args.json("items")?;
            add_test_steps(api, key, mode, items)
                .await
                .map(Value::String)
        }
        ScaleTool::UpdateTestSteps => update_test_steps(api, args).await,
        ScaleTool::GetFolders => {
            let mut params = Query::new();
            params.push((
                "maxResults".to_owned(),
                args.optional_integer("maxResults")?
                    .unwrap_or(10)
                    .to_string(),
            ));
            params.push((
                "startAt".to_owned(),
                args.optional_integer("startAt")?.unwrap_or(0).to_string(),
            ));
            set(
                &mut params,
                "projectKey",
                args.optional_identifier("projectKey")?,
            );
            set(
                &mut params,
                "folderType",
                args.optional_identifier("folderType")?,
            );
            let folders = api.paginated(&["folders"], params).await.map_err(adk)?;
            text(format!(
                "Extracted folders: {}",
                python_repr::repr(&Value::Array(folders))
            ))
        }
        ScaleTool::UpdateTestCase => update_test_case(api, args).await,
        ScaleTool::GetLinks => {
            let key = args.identifier("test_case_key")?;
            let reply = api
                .send(Method::GET, &["testcases", key, "links"], None)
                .await
                .map_err(adk)?;
            text(format!(
                "Links for test case `{key}`: {}",
                reply_str(&reply)
            ))
        }
        ScaleTool::CreateIssueLinks => {
            let key = args.identifier("test_case_key")?;
            let issue_id = args.integer("issue_id")?;
            let reply = api
                .send(
                    Method::POST,
                    &["testcases", key, "links", "issues"],
                    Some(&json!({"issueId": issue_id})),
                )
                .await
                .map_err(adk)?;
            effect_text(format!(
                "Issue link created for test case `{key}` with issue ID `{issue_id}`: {}",
                reply_str(&reply)
            ))
        }
        ScaleTool::CreateWebLinks => create_web_links(api, args).await,
        ScaleTool::GetVersions => {
            let key = args.identifier("test_case_key")?;
            let params = vec![
                (
                    "maxResults".to_owned(),
                    args.optional_integer("maxResults")?
                        .unwrap_or(10)
                        .to_string(),
                ),
                (
                    "startAt".to_owned(),
                    args.optional_integer("startAt")?.unwrap_or(0).to_string(),
                ),
            ];
            let versions = api
                .paginated(&["testcases", key, "versions"], params)
                .await
                .map_err(adk)?;
            let lines = versions.iter().map(python_repr::str_of).collect::<Vec<_>>();
            text(format!(
                "Versions for test case `{key}`: {}",
                lines.join("\n")
            ))
        }
        ScaleTool::GetVersion => {
            let key = args.identifier("test_case_key")?;
            let version = args.identifier("version")?;
            let reply = api
                .send(Method::GET, &["testcases", key, "versions", version], None)
                .await
                .map_err(adk)?;
            text(format!(
                "Version {version} of test case `{key}`: {}",
                reply_str(&reply)
            ))
        }
        ScaleTool::GetTestScript => {
            let key = args.identifier("test_case_key")?;
            let reply = api
                .send(Method::GET, &["testcases", key, "testscript"], None)
                .await
                .map_err(adk)?;
            text(format!(
                "Test script for test case `{key}`: {}",
                reply_str(&reply)
            ))
        }
        ScaleTool::CreateTestScript => {
            let key = args.identifier("test_case_key")?;
            let body = json!({"type": args.text("script_type")?, "text": args.text("text")?});
            let reply = api
                .send(Method::POST, &["testcases", key, "testscript"], Some(&body))
                .await
                .map_err(adk)?;
            effect_text(format!(
                "Test script created/updated for test case `{key}`: {}",
                reply_str(&reply)
            ))
        }
        ScaleTool::SearchTestCases => search_test_cases(api, args).await,
        ScaleTool::GetTestsRecursive => get_tests_recursive(api, args).await,
        ScaleTool::GetTestsByFolderName => get_tests_by_folder_name(api, args).await,
        ScaleTool::GetTestsByFolderPath => get_tests_by_folder_path(api, args).await,
    }
}

fn adk(error: ZephyrRestError) -> adk_core::AdkError {
    error.into_adk(&FAMILY)
}

fn text(output: String) -> adk_core::Result<Value> {
    bounded_text(output, false).map_err(adk)
}

fn effect_text(output: String) -> adk_core::Result<Value> {
    bounded_text(output, true).map_err(adk)
}

/// Set (or, for `None`, drop) one query parameter, keeping its position.
fn set(query: &mut Query, name: &str, value: Option<&str>) {
    match value {
        Some(value) => match query.iter_mut().find(|(existing, _)| existing == name) {
            Some((_, existing)) => value.clone_into(existing),
            None => query.push((name.to_owned(), value.to_owned())),
        },
        None => query.retain(|(existing, _)| existing != name),
    }
}

/// A JSON object argument the SDK accepts as a string, `""` meaning `{}`.
fn json_object_or_empty(args: &Arguments<'_>, name: &str) -> adk_core::Result<Map<String, Value>> {
    let Some(raw) = args.raw(name) else {
        return Ok(Map::new());
    };
    let value = match raw {
        Value::String(encoded) if encoded.trim().is_empty() => return Ok(Map::new()),
        Value::String(encoded) => decode_json(encoded, &ARGUMENTS)?,
        other => other.clone(),
    };
    match value {
        Value::Object(object) => Ok(object),
        _ => Err(args.invalid()),
    }
}

async fn get_tests(api: &ZephyrScaleClient, args: &Arguments<'_>) -> adk_core::Result<Value> {
    // `if value: kwargs[...] = value` — falsy filters are not sent.
    let mut params = vec![(
        "projectKey".to_owned(),
        args.identifier("project_key")?.to_owned(),
    )];
    set(
        &mut params,
        "folderId",
        args.optional_identifier("folder_id")?,
    );
    let max_results = args.optional_integer("maxResults")?.unwrap_or(10);
    if max_results != 0 {
        params.push(("maxResults".to_owned(), max_results.to_string()));
    }
    let start_at = args.optional_integer("startAt")?.unwrap_or(0);
    if start_at != 0 {
        params.push(("startAt".to_owned(), start_at.to_string()));
    }
    let tests = api.paginated(&["testcases"], params).await.map_err(adk)?;
    text(format!("Extracted tests: {}", parsed_tests_repr(&tests)))
}

/// The SDK's `create_test_case`, as a result line: an API failure creating
/// the case is the tool's error; a failure adding its steps is reported in
/// the text after the creation, as the SDK reports it.
async fn create_one(
    api: &ZephyrScaleClient,
    project_key: &str,
    name: &str,
    additional: Map<String, Value>,
    steps: Option<Value>,
) -> Result<String, ZephyrRestError> {
    let mut body = Map::new();
    body.insert(
        "projectKey".to_owned(),
        Value::String(project_key.to_owned()),
    );
    body.insert("name".to_owned(), Value::String(name.to_owned()));
    body.extend(additional);
    let reply = api
        .send(Method::POST, &["testcases"], Some(&Value::Object(body)))
        .await?;
    let mut result = format!(
        "Test case with name `{name}` was created: {}",
        reply_str(&reply)
    );
    if let Some(steps) = steps {
        // The SDK reads `response['test_case_key']`, which the Cloud API
        // never returns, so its steps were never added; the key is `key`.
        let key = reply
            .json()
            .and_then(|created| created.get("key"))
            .and_then(Value::as_str)
            .map(str::to_owned);
        let added = match key {
            Some(key) => add_test_steps(api, &key, "APPEND", steps)
                .await
                .unwrap_or_else(|error| error.message),
            None => "Unable to add/update steps: the created test case has no key".to_owned(),
        };
        result.push('\n');
        result.push_str(&added);
    }
    Ok(result)
}

/// Parse the SDK's `steps` value: a JSON string or an already-decoded list.
fn steps_value(raw: Option<&Value>, args: &Arguments<'_>) -> adk_core::Result<Option<Value>> {
    match raw {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(encoded)) if encoded.is_empty() => Ok(None),
        Some(Value::String(encoded)) => decode_json(encoded, &ARGUMENTS).map(Some),
        Some(Value::Array(items)) if items.is_empty() => Ok(None),
        Some(value @ Value::Array(_)) => Ok(Some(value.clone())),
        Some(_) => Err(args.invalid()),
    }
}

async fn create_test_case(
    api: &ZephyrScaleClient,
    args: &Arguments<'_>,
) -> adk_core::Result<Value> {
    let project_key = args.identifier("project_key")?;
    let name = args.text("test_case_name")?;
    let additional = json_object_or_empty(args, "additional_fields")?;
    let steps = steps_value(args.raw("steps"), args)?;
    let result = create_one(api, project_key, name, additional, steps)
        .await
        .map_err(adk)?;
    effect_text(result)
}

async fn create_test_cases(
    api: &ZephyrScaleClient,
    args: &Arguments<'_>,
) -> adk_core::Result<Value> {
    let payload = args.json("create_test_cases_data")?;
    let cases = payload.as_array().ok_or_else(|| args.invalid())?;
    if cases.len() > MAX_CASES_PER_BATCH {
        return Err(args.exhausted());
    }
    // Every case is validated before the first effect; the SDK raised
    // mid-batch on a malformed case, after creating the ones before it.
    let mut parsed = Vec::with_capacity(cases.len());
    for case in cases {
        let object = case.as_object().ok_or_else(|| args.invalid())?;
        let field = |name: &str| {
            object
                .get(name)
                .and_then(Value::as_str)
                .filter(|text| !text.is_empty())
        };
        let project_key = field("project_key").ok_or_else(|| args.invalid())?;
        let name = field("test_case_name").ok_or_else(|| args.invalid())?;
        let additional = match object.get("additional_fields") {
            None | Some(Value::Null) => Map::new(),
            Some(Value::String(encoded)) if encoded.trim().is_empty() => Map::new(),
            Some(Value::String(encoded)) => match decode_json(encoded, &ARGUMENTS)? {
                Value::Object(object) => object,
                _ => return Err(args.invalid()),
            },
            Some(Value::Object(object)) => object.clone(),
            Some(_) => return Err(args.invalid()),
        };
        let steps = steps_value(object.get("steps"), args)?;
        parsed.push((project_key, name, additional, steps));
    }
    let mut results = Vec::with_capacity(parsed.len());
    for (project_key, name, additional, steps) in parsed {
        let line = match create_one(api, project_key, name, additional, steps).await {
            Ok(line) => line,
            Err(error) => format!(
                "Unable to create test case with name: {name}:\n{}",
                adk(error).message
            ),
        };
        results.push(Value::String(line));
    }
    effect_text(python_repr::repr(&Value::Array(results)))
}

/// The SDK's `add_test_steps`: one `POST testcases/{key}/teststeps`. Its
/// failure text is the SDK's `Unable to add/update steps ...` line.
async fn add_test_steps(
    api: &ZephyrScaleClient,
    key: &str,
    mode: &str,
    items: Value,
) -> Result<String, adk_core::AdkError> {
    let body = json!({"mode": mode, "items": items});
    match api
        .send(Method::POST, &["testcases", key, "teststeps"], Some(&body))
        .await
    {
        Ok(reply) => Ok(format!(
            "Steps for test case `{key}` were added/updated: {}",
            reply_str(&reply)
        )),
        Err(error) => {
            let mut error = adk(error);
            error.message = format!(
                "Unable to add/update steps for test case with key: {key}:\n{}",
                error.message
            );
            Err(error)
        }
    }
}

async fn update_test_steps(
    api: &ZephyrScaleClient,
    args: &Arguments<'_>,
) -> adk_core::Result<Value> {
    let key = args.identifier("test_case_key")?;
    let encoded = args.text("steps_updates")?;
    let mut steps = api
        .paginated(&["testcases", key, "teststeps"], Vec::new())
        .await
        .map_err(adk)?;
    if steps.is_empty() {
        return text(format!("No test steps found for test case: {key}"));
    }
    let updates = match serde_json::from_str::<Value>(encoded) {
        Ok(Value::Array(updates)) => updates,
        Ok(_) => return text("Steps updates must be a JSON array".to_owned()),
        Err(error) => {
            return text(format!("Invalid JSON format for steps_updates: {error}"));
        }
    };
    let last = steps.len() - 1;
    for update in &updates {
        let Some(index) = update.get("index") else {
            return text("Each update must contain an 'index' field".to_owned());
        };
        let Some(position) = index
            .as_u64()
            .and_then(|position| usize::try_from(position).ok())
            .filter(|position| *position <= last)
        else {
            return text(format!(
                "Step index {} is out of range. Valid range: 0-{last}",
                python_repr::str_of(index)
            ));
        };
        let Some(inline) = steps[position]
            .get_mut("inline")
            .and_then(Value::as_object_mut)
        else {
            return text(format!(
                "Step at index {} does not have an inline field",
                python_repr::str_of(index)
            ));
        };
        for field in ["description", "testData", "expectedResult"] {
            if let Some(value) = update.get(field) {
                inline.insert(field.to_owned(), value.clone());
            }
        }
    }
    let count = updates.len();
    api.send(
        Method::POST,
        &["testcases", key, "teststeps"],
        Some(&json!({"mode": "OVERWRITE", "items": steps})),
    )
    .await
    .map_err(adk)?;
    effect_text(format!(
        "Test steps updated for test case `{key}`: {count} step(s) modified"
    ))
}

async fn update_test_case(
    api: &ZephyrScaleClient,
    args: &Arguments<'_>,
) -> adk_core::Result<Value> {
    let key = args.identifier("test_case_key")?;
    let mut body = Map::new();
    body.insert("id".to_owned(), json!(args.integer("test_case_id")?));
    body.insert("key".to_owned(), json!(key));
    body.insert("name".to_owned(), json!(args.text("name")?));
    body.insert(
        "project".to_owned(),
        json!({"id": args.integer("project_id")?}),
    );
    body.insert(
        "priority".to_owned(),
        json!({"id": args.integer("priority_id")?}),
    );
    body.insert(
        "status".to_owned(),
        json!({"id": args.integer("status_id")?}),
    );
    // The SDK forwarded the raw `additional_fields` string as a body member
    // of that name; its documented meaning is the fields to merge.
    body.extend(json_object_or_empty(args, "additional_fields")?);
    let reply = api
        .send(Method::PUT, &["testcases", key], Some(&Value::Object(body)))
        .await
        .map_err(adk)?;
    effect_text(format!(
        "Test case `{key}` was updated: {}",
        reply_str(&reply)
    ))
}

async fn create_web_links(
    api: &ZephyrScaleClient,
    args: &Arguments<'_>,
) -> adk_core::Result<Value> {
    let key = args.identifier("test_case_key")?;
    let url = args.text("url")?;
    let description = args.text("description")?;
    let mut body = Map::new();
    body.insert("url".to_owned(), json!(url));
    body.extend(json_object_or_empty(args, "additional_fields")?);
    body.insert("description".to_owned(), json!(description));
    let reply = api
        .send(
            Method::POST,
            &["testcases", key, "links", "weblinks"],
            Some(&Value::Object(body)),
        )
        .await
        .map_err(adk)?;
    effect_text(format!(
        "Web link created for test case `{key}` with URL `{url}` and link text `{description}`: {}",
        reply_str(&reply)
    ))
}

/// `_get_folders(project_key, "TEST_CASE", 1000)`.
async fn test_case_folders(
    api: &ZephyrScaleClient,
    project_key: Option<&str>,
) -> Result<Vec<Value>, ZephyrRestError> {
    let mut params = vec![("maxResults".to_owned(), "1000".to_owned())];
    set(&mut params, "projectKey", project_key);
    params.push(("folderType".to_owned(), "TEST_CASE".to_owned()));
    api.paginated(&["folders"], params).await
}

/// `_get_test_cases_from_folders`: every folder's test cases; a folder that
/// fails is skipped with a warning, as in the SDK.
async fn cases_from_folders(
    api: &ZephyrScaleClient,
    project_key: Option<&str>,
    folder_ids: &[i64],
    max_results: i64,
    start_at: i64,
    base: Query,
) -> Result<Vec<Value>, ZephyrRestError> {
    let mut base = base;
    set(&mut base, "projectKey", project_key);
    set(&mut base, "maxResults", Some(&max_results.to_string()));
    set(&mut base, "startAt", Some(&start_at.to_string()));
    let mut cases = Vec::new();
    for folder in folder_ids {
        let mut params = base.clone();
        set(&mut params, "folderId", Some(&folder.to_string()));
        match api.paginated(&["testcases"], params).await {
            Ok(found) => {
                if cases.len().saturating_add(found.len()) > MAX_ITEMS {
                    return Err(resource_exhausted(false));
                }
                cases.extend(found);
            }
            Err(error) => tracing::warn!(
                event = "zephyr_scale_folder_read_skipped",
                error_code = ?error.code(),
                "a Zephyr Scale folder's test cases could not be read and were skipped"
            ),
        }
    }
    Ok(cases)
}

fn paging(args: &Arguments<'_>, default_max: i64) -> adk_core::Result<(i64, i64)> {
    Ok((
        args.optional_integer("maxResults")?.unwrap_or(default_max),
        args.optional_integer("startAt")?.unwrap_or(0),
    ))
}

async fn get_tests_recursive(
    api: &ZephyrScaleClient,
    args: &Arguments<'_>,
) -> adk_core::Result<Value> {
    let project_key = args.optional_identifier("project_key")?;
    let folder_id = args.optional_identifier("folder_id")?;
    let (max_results, start_at) = paging(args, 100)?;
    let mut base = vec![
        ("maxResults".to_owned(), max_results.to_string()),
        ("startAt".to_owned(), start_at.to_string()),
    ];
    set(&mut base, "projectKey", project_key);
    // `int(folder_id)` in the SDK's subfolder walk.
    let parent = folder_id
        .map(|id| id.trim().parse::<i64>().map_err(|_| args.invalid()))
        .transpose()?;
    let mut cases = Vec::new();
    if let Some(folder_id) = folder_id {
        let mut params = base.clone();
        set(&mut params, "folderId", Some(folder_id));
        match api.paginated(&["testcases"], params).await {
            Ok(found) => cases.extend(found),
            Err(error) => tracing::warn!(
                event = "zephyr_scale_folder_read_skipped",
                error_code = ?error.code(),
                "a Zephyr Scale folder's test cases could not be read and were skipped"
            ),
        }
    }
    let folders = test_case_folders(api, project_key).await.map_err(adk)?;
    if let Some(parent) = parent {
        let tree = FolderTree::build(&folders);
        let subfolders = tree.collect(&[parent], false);
        if !subfolders.is_empty() {
            let found =
                cases_from_folders(api, project_key, &subfolders, max_results, start_at, base)
                    .await
                    .map_err(adk)?;
            cases.extend(found);
        }
    }
    text(format!(
        "Extracted {} tests recursively: {}",
        cases.len(),
        parsed_tests_repr(&cases)
    ))
}

async fn get_tests_by_folder_name(
    api: &ZephyrScaleClient,
    args: &Arguments<'_>,
) -> adk_core::Result<Value> {
    let project_key = args.identifier("project_key")?;
    let folder_name = args.text("folder_name")?;
    let exact = args.optional_bool("exact_match")?.unwrap_or(false);
    let include_subfolders = args.optional_bool("include_subfolders")?.unwrap_or(true);
    let (max_results, start_at) = paging(args, 100)?;
    let folders = test_case_folders(api, Some(project_key))
        .await
        .map_err(adk)?;
    let mut matching = find_by_name(&folders, folder_name, exact);
    if matching.is_empty() {
        return text(format!("No folders found matching name: {folder_name}"));
    }
    if include_subfolders {
        matching = FolderTree::build(&folders).collect(&matching, true);
    }
    let cases = cases_from_folders(
        api,
        Some(project_key),
        &matching,
        max_results,
        start_at,
        Query::new(),
    )
    .await
    .map_err(adk)?;
    text(format!(
        "Extracted {} tests from {} folders matching '{folder_name}': {}",
        cases.len(),
        matching.len(),
        parsed_tests_repr(&cases)
    ))
}

async fn get_tests_by_folder_path(
    api: &ZephyrScaleClient,
    args: &Arguments<'_>,
) -> adk_core::Result<Value> {
    let project_key = args.identifier("project_key")?;
    let folder_path = args.text("folder_path")?;
    let include_subfolders = args.optional_bool("include_subfolders")?.unwrap_or(true);
    let (max_results, start_at) = paging(args, 100)?;
    let folders = test_case_folders(api, Some(project_key))
        .await
        .map_err(adk)?;
    let tree = FolderTree::build(&folders);
    let Some(target) = tree.find_by_path(folder_path) else {
        return text(format!("No folder found with path: {folder_path}"));
    };
    let mut folder_ids = vec![target];
    if include_subfolders {
        folder_ids = tree.collect(&folder_ids, true);
    }
    let cases = cases_from_folders(
        api,
        Some(project_key),
        &folder_ids,
        max_results,
        start_at,
        Query::new(),
    )
    .await
    .map_err(adk)?;
    text(format!(
        "Extracted {} tests from folder path '{folder_path}': {}",
        cases.len(),
        parsed_tests_repr(&cases)
    ))
}

fn search_schema() -> Value {
    object_schema(
        "ZephyrSearchTestCases",
        &[
            (
                "project_key",
                identifier_property("Jira project key filter"),
            ),
            (
                "search_term",
                optional_text_property("Optional search term to filter test cases"),
            ),
            (
                "max_results",
                json!({"type":["integer","null"],"minimum":1,"description":"Maximum number of results to query from the API. Default 1000."}),
            ),
            (
                "start_at",
                optional_integer_property("Zero-indexed starting position. Default 0."),
            ),
            (
                "order_by",
                optional_text_property("Field to order results by. Default 'name'."),
            ),
            (
                "order_direction",
                optional_text_property("Order direction. Default 'ASC'."),
            ),
            (
                "archived",
                optional_bool_property(
                    "Include archived test cases. Accepted for compatibility; the SDK never sends it, so it has no effect.",
                ),
            ),
            (
                "fields",
                optional_string_list_property(
                    "Fields to include in the response (default: key, name). Regular fields include key, name, id, labels, folder, etc. Custom fields can be included in the following ways:Individual custom fields via customFields.field_name format, All custom fields via customFields in the fields list",
                ),
            ),
            (
                "limit_results",
                json!({"type":["integer","null"],"minimum":1,"description":"Maximum number of filtered results to return. Without it every match is returned."}),
            ),
            (
                "folder_id",
                optional_identifier_property("Filter test cases by folder ID"),
            ),
            (
                "folder_name",
                optional_text_property("Filter test cases by folder name (full or partial)"),
            ),
            (
                "exact_folder_match",
                optional_bool_property(
                    "Whether to match the folder name exactly or allow partial matches. Default false.",
                ),
            ),
            (
                "folder_path",
                optional_text_property(
                    "Filter test cases by folder path (e.g., 'Root/Parent/Child')",
                ),
            ),
            (
                "include_subfolders",
                optional_bool_property(
                    "Include test cases from subfolders of matching folders. Default true.",
                ),
            ),
            (
                "labels",
                optional_string_list_property("Filter test cases by labels"),
            ),
            (
                "custom_fields",
                optional_text_property(
                    "JSON string containing custom field filters (e.g., {\"Country\": \"All\", \"Is Automated\": \"Yes\"}).",
                ),
            ),
            (
                "steps_search",
                optional_text_property(
                    "Search term to find in test case steps (description, expected result, or test data)",
                ),
            ),
            (
                "include_steps",
                optional_bool_property(
                    "Whether to include test steps in the response. Default false.",
                ),
            ),
        ],
        &["project_key"],
    )
}

#[allow(clippy::struct_excessive_bools)] // The SDK's search flags, one per argument.
struct SearchRequest<'a> {
    project_key: &'a str,
    search_term: Option<&'a str>,
    max_results: i64,
    start_at: i64,
    order_by: &'a str,
    descending: bool,
    fields: Vec<&'a str>,
    limit_results: Option<usize>,
    folder_id: Option<String>,
    folder_name: Option<&'a str>,
    exact_folder_match: bool,
    folder_path: Option<&'a str>,
    include_subfolders: bool,
    labels: Option<Vec<&'a str>>,
    custom_fields: Option<&'a str>,
    steps_search: Option<&'a str>,
    include_steps: bool,
}

impl<'a> SearchRequest<'a> {
    fn parse(args: &Arguments<'a>) -> adk_core::Result<Self> {
        let positive = |name: &str, default: Option<i64>| -> adk_core::Result<Option<i64>> {
            match args.optional_integer(name)?.or(default) {
                Some(value) if value <= 0 => Err(args.invalid()),
                other => Ok(other),
            }
        };
        let _archived = args.optional_bool("archived")?;
        Ok(Self {
            project_key: args.identifier("project_key")?,
            search_term: args
                .optional_text("search_term")?
                .filter(|term| !term.is_empty()),
            max_results: positive("max_results", Some(1_000))?.unwrap_or(1_000),
            start_at: args.optional_integer("start_at")?.unwrap_or(0),
            order_by: args.optional_text("order_by")?.unwrap_or("name"),
            descending: args
                .optional_text("order_direction")?
                .unwrap_or("ASC")
                .eq_ignore_ascii_case("DESC"),
            fields: args
                .optional_string_list("fields")?
                .unwrap_or_else(|| vec!["key", "name"]),
            limit_results: positive("limit_results", None)?
                .map(|limit| usize::try_from(limit).unwrap_or(usize::MAX)),
            folder_id: args.optional_identifier("folder_id")?.map(str::to_owned),
            folder_name: args
                .optional_text("folder_name")?
                .filter(|name| !name.is_empty()),
            exact_folder_match: args.optional_bool("exact_folder_match")?.unwrap_or(false),
            folder_path: args
                .optional_text("folder_path")?
                .filter(|path| !path.is_empty()),
            include_subfolders: args.optional_bool("include_subfolders")?.unwrap_or(true),
            labels: args.optional_string_list("labels")?,
            custom_fields: args
                .optional_text("custom_fields")?
                .filter(|text| !text.is_empty()),
            steps_search: args
                .optional_text("steps_search")?
                .filter(|text| !text.is_empty()),
            include_steps: args.optional_bool("include_steps")?.unwrap_or(false),
        })
    }
}

/// The SDK's `search_test_cases` (client-side filtering over the listed
/// test cases), rendered as `Found N test cases matching ...: [json]`.
async fn search_test_cases(
    api: &ZephyrScaleClient,
    args: &Arguments<'_>,
) -> adk_core::Result<Value> {
    let mut request = SearchRequest::parse(args)?;
    let mut targets = Vec::new();
    if request.folder_name.is_some() || request.folder_path.is_some() {
        let folders = test_case_folders(api, Some(request.project_key))
            .await
            .map_err(adk)?;
        let tree = FolderTree::build(&folders);
        if let Some(name) = request.folder_name {
            targets.extend(find_by_name(&folders, name, request.exact_folder_match));
        }
        if let Some(path) = request.folder_path
            && let Some(id) = tree.find_by_path(path)
        {
            targets.push(id);
        }
        if request.include_subfolders && !targets.is_empty() {
            targets = tree.collect(&targets, true);
        }
        if let (Some(first), None) = (targets.first(), &request.folder_id) {
            request.folder_id = Some(first.to_string());
        }
    }
    let mut params = vec![
        ("projectKey".to_owned(), request.project_key.to_owned()),
        ("maxResults".to_owned(), request.max_results.to_string()),
        ("startAt".to_owned(), request.start_at.to_string()),
    ];
    set(&mut params, "folderId", request.folder_id.as_deref());
    let custom_fields = match request.custom_fields {
        None => None,
        Some(encoded) => match serde_json::from_str::<Value>(encoded) {
            Ok(Value::Object(object)) => Some(object),
            Ok(other) => {
                return text(format!(
                    "Error processing custom fields: '{}' object has no attribute 'items'",
                    python_type(&other)
                ));
            }
            Err(error) => return text(format!("Error processing custom fields: {error}")),
        },
    };
    let cases = if !targets.is_empty() && request.include_subfolders {
        cases_from_folders(
            api,
            Some(request.project_key),
            &targets,
            request.max_results,
            request.start_at,
            params,
        )
        .await
    } else {
        api.paginated(&["testcases"], params).await
    }
    .map_err(adk)?;
    let mut filtered = cases
        .into_iter()
        .filter(|case| matches_case(case, &request, custom_fields.as_ref()))
        .collect::<Vec<_>>();
    if let Some(term) = request.steps_search {
        filtered = filter_by_steps(api, filtered, term, request.include_steps).await?;
    }
    if !request.order_by.is_empty() && !filtered.is_empty() {
        let field = request.order_by;
        filtered.sort_by(|left, right| {
            let ordering = python_order(
                left.get(field).unwrap_or(&Value::String(String::new())),
                right.get(field).unwrap_or(&Value::String(String::new())),
            );
            if request.descending {
                ordering.reverse()
            } else {
                ordering
            }
        });
    }
    if let Some(limit) = request.limit_results {
        filtered.truncate(limit);
    }
    let rows = filtered
        .iter()
        .map(|case| project_fields(case, &request.fields))
        .collect::<Vec<_>>();
    let message = search_message(&request, rows.len());
    text(format!("{message}: {}", json_dumps_indent(&rows)))
}

/// `_format_test_case_results`' message: the count and every truthy search
/// criterion, in the SDK's order.
fn search_message(request: &SearchRequest<'_>, count: usize) -> String {
    let mut message = format!("Found {count} test cases");
    let labels = request.labels.as_ref().map(|labels| {
        python_repr::repr_str_list(
            &labels
                .iter()
                .map(|label| (*label).to_owned())
                .collect::<Vec<_>>(),
        )
    });
    let criteria = [
        ("folder name", request.folder_name.map(str::to_owned)),
        ("folder path", request.folder_path.map(str::to_owned)),
        ("folder ID", request.folder_id.clone()),
        ("search term", request.search_term.map(str::to_owned)),
        (
            "labels",
            labels.filter(|_| {
                request
                    .labels
                    .as_ref()
                    .is_some_and(|labels| !labels.is_empty())
            }),
        ),
        ("custom fields", request.custom_fields.map(str::to_owned)),
        ("steps containing", request.steps_search.map(str::to_owned)),
    ];
    let details = criteria
        .iter()
        .filter_map(|(name, value)| value.as_ref().map(|value| format!("{name} '{value}'")))
        .collect::<Vec<_>>();
    if !details.is_empty() {
        message.push_str(" matching ");
        message.push_str(&details.join(" and "));
    }
    message
}

fn python_type(value: &Value) -> &'static str {
    match value {
        Value::Null => "NoneType",
        Value::Bool(_) => "bool",
        Value::Number(number) if number.is_f64() => "float",
        Value::Number(_) => "int",
        Value::String(_) => "str",
        Value::Array(_) => "list",
        Value::Object(_) => "dict",
    }
}

/// `_filter_test_cases`: search term over the case's values, any of the
/// labels, and every custom-field filter.
fn matches_case(
    case: &Value,
    request: &SearchRequest<'_>,
    custom_fields: Option<&Map<String, Value>>,
) -> bool {
    if let Some(term) = request.search_term {
        // `term.lower() in str(tc.values()).lower()`.
        let values = case
            .as_object()
            .map(|object| {
                let mut members = object.iter().collect::<Vec<_>>();
                members.sort_by(|left, right| left.0.cmp(right.0));
                members
                    .into_iter()
                    .map(|(_, value)| python_repr::repr(value))
                    .collect::<Vec<_>>()
                    .join(", ")
            })
            .unwrap_or_default();
        let haystack = format!("dict_values([{values}])").to_lowercase();
        if !haystack.contains(&term.to_lowercase()) {
            return false;
        }
    }
    if let Some(labels) = request.labels.as_ref().filter(|labels| !labels.is_empty()) {
        let case_labels = case
            .get("labels")
            .and_then(Value::as_array)
            .map(Vec::as_slice)
            .unwrap_or_default();
        if !labels.iter().any(|label| {
            case_labels
                .iter()
                .any(|value| value.as_str() == Some(label))
        }) {
            return false;
        }
    }
    if let Some(filters) = custom_fields.filter(|filters| !filters.is_empty()) {
        let Some(actual_fields) = case.get("customFields").and_then(Value::as_object) else {
            return false;
        };
        for (name, expected) in filters {
            let Some(actual) = actual_fields.get(name) else {
                return false;
            };
            let matched = match (actual, expected) {
                (Value::Array(actual), Value::Array(expected)) => {
                    expected.iter().any(|value| actual.contains(value))
                }
                (Value::Array(actual), expected) => actual.contains(expected),
                (actual, expected) => actual == expected,
            };
            if !matched {
                return false;
            }
        }
    }
    true
}

/// `_filter_test_steps`: keep the cases with a step whose description, test
/// data or expected result contains `term` (ignoring case); a case whose steps
/// cannot be read is skipped with a warning, as in the SDK.
async fn filter_by_steps(
    api: &ZephyrScaleClient,
    cases: Vec<Value>,
    term: &str,
    include_steps: bool,
) -> adk_core::Result<Vec<Value>> {
    if cases.len() > MAX_STEP_SEARCH_CASES {
        return Err(adk(resource_exhausted(false)));
    }
    let term = term.to_lowercase();
    let mut kept = Vec::new();
    for mut case in cases {
        let Some(key) = case.get("key").and_then(Value::as_str).map(str::to_owned) else {
            continue;
        };
        let steps = match api
            .paginated(&["testcases", &key, "teststeps"], Vec::new())
            .await
        {
            Ok(steps) => steps,
            Err(error) => {
                tracing::warn!(
                    event = "zephyr_scale_step_read_skipped",
                    error_code = ?error.code(),
                    "a Zephyr Scale test case's steps could not be read and it was skipped"
                );
                continue;
            }
        };
        let matching = steps
            .into_iter()
            .filter(|step| {
                let Some(inline) = step.get("inline").and_then(Value::as_object) else {
                    return false;
                };
                ["description", "testData", "expectedResult"]
                    .iter()
                    .any(|field| {
                        inline
                            .get(*field)
                            .and_then(Value::as_str)
                            .is_some_and(|text| text.to_lowercase().contains(&term))
                    })
            })
            .collect::<Vec<_>>();
        if matching.is_empty() {
            continue;
        }
        if include_steps && let Some(object) = case.as_object_mut() {
            object.insert("steps".to_owned(), Value::Array(matching));
        }
        kept.push(case);
    }
    Ok(kept)
}

/// A total order for `list.sort(key=lambda x: x.get(order_by, ''))`: values
/// of one type compare as Python would; mixed types (a `TypeError` in the
/// SDK) order by type instead of failing the search.
fn python_order(left: &Value, right: &Value) -> Ordering {
    fn rank(value: &Value) -> u8 {
        match value {
            Value::Null => 0,
            Value::Bool(_) | Value::Number(_) => 1,
            Value::String(_) => 2,
            Value::Array(_) => 3,
            Value::Object(_) => 4,
        }
    }
    fn number(value: &Value) -> Option<f64> {
        match value {
            Value::Bool(flag) => Some(f64::from(u8::from(*flag))),
            Value::Number(number) => number.as_f64(),
            _ => None,
        }
    }
    match (left, right) {
        (Value::String(left), Value::String(right)) => left.cmp(right),
        _ => match (number(left), number(right)) {
            (Some(left), Some(right)) => left.partial_cmp(&right).unwrap_or(Ordering::Equal),
            _ => rank(left)
                .cmp(&rank(right))
                .then_with(|| python_repr::repr(left).cmp(&python_repr::repr(right))),
        },
    }
}

/// `_format_test_case_results` for one case: the requested fields in order,
/// `customFields.<name>` collected under one `customFields` object.
fn project_fields(case: &Value, fields: &[&str]) -> OrderedObject {
    fn put(row: &mut OrderedObject, key: &str, value: Value) {
        match row.iter_mut().find(|(existing, _)| existing == key) {
            Some((_, existing)) => *existing = value,
            None => row.push((key.to_owned(), value)),
        }
    }
    let mut row: OrderedObject = Vec::new();
    for field in fields {
        if let Some(value) = case.get(*field) {
            put(&mut row, field, value.clone());
        } else if let Some(name) = field.strip_prefix("customFields.")
            && let Some(value) = case.get("customFields").and_then(|custom| custom.get(name))
        {
            let mut custom = row
                .iter()
                .find(|(existing, _)| existing == "customFields")
                .and_then(|(_, value)| value.as_object().cloned())
                .unwrap_or_default();
            custom.insert(name.to_owned(), value.clone());
            put(&mut row, "customFields", Value::Object(custom));
        }
    }
    row
}
