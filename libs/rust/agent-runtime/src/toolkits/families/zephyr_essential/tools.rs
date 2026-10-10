use std::sync::Arc;

use adk_tool::BasicToolset;
use async_trait::async_trait;
use reqwest::Method;
use serde_json::{Map, Value};

use crate::toolkits::families::zephyr_rest::client::{
    ZephyrRestClient, ZephyrRestError, ZephyrRestFamily, invalid_response, python_str,
};
use crate::toolkits::families::zephyr_rest::tools::{
    Arguments, ZephyrArgumentCodes, ZephyrRestToolsetError, ZephyrToolSpec, build_toolset,
    identifier_property, integer_property, json_text_property, object_schema,
    optional_identifier_property, optional_integer_property, text_property,
};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{FAMILY, ZephyrEssentialClient};
use super::config::ZephyrEssentialToolkitConfig;

const TOOLKIT_TYPE: &str = "zephyr_essential";

static ARGUMENTS: ZephyrArgumentCodes = ZephyrArgumentCodes {
    label: "Zephyr Essential",
    invalid: "zephyr_essential.arguments.invalid",
    exhausted: "zephyr_essential.arguments.resource_exhausted",
};

/// Build the served Zephyr Essential business tools.
pub(crate) fn build_zephyr_essential_toolset(
    toolkit_name: &str,
    config: ZephyrEssentialToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, ZephyrRestToolsetError> {
    let (base_url, token, selected) = config.into_parts();
    let client = Arc::new(ZephyrEssentialClient::new(ZephyrRestClient::new(
        base_url, token,
    )?));
    build_with_client(toolkit_name, &selected, policy, &client)
}

fn build_with_client(
    toolkit_name: &str,
    selected: &[Box<str>],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<ZephyrEssentialClient>,
) -> Result<BasicToolset, ZephyrRestToolsetError> {
    let kinds = OPERATIONS.iter().map(EssentialTool).collect::<Vec<_>>();
    build_toolset(
        toolkit_name,
        TOOLKIT_TYPE,
        &kinds,
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
    client: &Arc<ZephyrEssentialClient>,
) -> Result<BasicToolset, ZephyrRestToolsetError> {
    let selected = selected
        .iter()
        .map(|name| Box::<str>::from(*name))
        .collect::<Vec<_>>();
    build_with_client(toolkit_name, &selected, policy, client)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_catalog() -> Vec<(&'static str, bool)> {
    OPERATIONS
        .iter()
        .map(|operation| (operation.name, operation.verb == Verb::Get))
        .collect()
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum Verb {
    Get,
    Post,
    Put,
    Delete,
}

#[derive(Clone, Copy)]
enum Segment {
    Literal(&'static str),
    Argument(&'static str),
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum ArgumentKind {
    /// A required string identifier (a path segment).
    Identifier,
    /// An optional string filter (a query parameter).
    OptionalIdentifier,
    /// A required integer (a path segment).
    Integer,
    /// An optional integer (a query parameter).
    OptionalInteger,
    /// The required JSON-encoded request body.
    Json,
    /// A required free-text value.
    Text,
}

struct Argument {
    name: &'static str,
    kind: ArgumentKind,
    description: &'static str,
}

/// What the tool does around its one request.
#[derive(Clone, Copy)]
enum Special {
    None,
    /// The SDK returns only the page's `values` array.
    Values,
    /// The SDK requires `issueId` in the body before linking.
    IssueLink(&'static str),
    /// The SDK resolves `parentName` into `parentId` before creating.
    CreateFolder,
    /// The SDK's client-side folder lookup by name.
    FindFolder,
}

struct Operation {
    name: &'static str,
    title: &'static str,
    description: &'static str,
    verb: Verb,
    path: &'static [Segment],
    arguments: &'static [Argument],
    /// `(API query name, argument name)`, sent only when the argument is set.
    query: &'static [(&'static str, &'static str)],
    special: Special,
}

const fn arg(name: &'static str, kind: ArgumentKind, description: &'static str) -> Argument {
    Argument {
        name,
        kind,
        description,
    }
}

use ArgumentKind::{Identifier, Integer, Json, OptionalIdentifier, OptionalInteger, Text};
use Segment::{Argument as A, Literal as L};

const MAX_RESULTS: Argument = arg(
    "max_results",
    OptionalInteger,
    "Maximum number of results to return.",
);
const START_AT: Argument = arg(
    "start_at",
    OptionalInteger,
    "Starting index of the results.",
);
const PAGING: [(&str, &str); 2] = [("maxResults", "max_results"), ("startAt", "start_at")];

const ISSUE_LINK_JSON: &str = "JSON body to create an issue link. Example: {\"issueId\": 10100} where issueId - Jira issue id (numeric; an issue key is not accepted).";
const WEB_LINK_JSON: &str = "JSON body to create a web link. Example: {\"url\": \"https://example.com\", \"description\": \"Web Link Description\"}";

macro_rules! effect_note {
    ($text:literal) => {
        concat!(
            $text,
            " This is a remote change; after a timeout or unknown outcome, read the current state before retrying."
        )
    };
}

static OPERATIONS: [Operation; 41] = [
    Operation {
        name: "list_test_cases",
        title: "ListTestCases",
        description: "List test cases with optional filters. Returns the page's array of test case objects.",
        verb: Verb::Get,
        path: &[L("testcases")],
        arguments: &[
            arg(
                "project_key",
                OptionalIdentifier,
                "Project key to filter test cases.",
            ),
            arg(
                "folder_id",
                OptionalIdentifier,
                "Folder ID to filter test cases.",
            ),
            MAX_RESULTS,
            START_AT,
        ],
        query: &[
            ("projectKey", "project_key"),
            ("folderId", "folder_id"),
            ("maxResults", "max_results"),
            ("startAt", "start_at"),
        ],
        special: Special::Values,
    },
    Operation {
        name: "create_test_case",
        title: "CreateTestCase",
        description: effect_note!("Create a new test case."),
        verb: Verb::Post,
        path: &[L("testcases")],
        arguments: &[arg(
            "json",
            Json,
            "JSON body to create a test case. Example: {\"name\": \"Test Case Name\", \"description\": \"Test Case Description\", \"projectKey\": \"PROJECT_KEY\", \"folderId\": \"FOLDER_ID\"}",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "get_test_case",
        title: "GetTestCase",
        description: "Retrieve details of a specific test case.",
        verb: Verb::Get,
        path: &[L("testcases"), A("test_case_key")],
        arguments: &[arg(
            "test_case_key",
            Identifier,
            "Key of the test case to retrieve.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "update_test_case",
        title: "UpdateTestCase",
        description: effect_note!(
            "Update an existing test case. The body replaces the test case, so send the full object (fetch it with get_test_case first)."
        ),
        verb: Verb::Put,
        path: &[L("testcases"), A("test_case_key")],
        arguments: &[
            arg(
                "test_case_key",
                Identifier,
                "Key of the test case to update.",
            ),
            arg(
                "json",
                Json,
                "JSON body to update a test case, the full test case object. Example: {\"id\": 1, \"key\": \"SA-T10\", \"name\": \"Check axial pump\", \"project\": {\"id\": 10005}, \"objective\": \"To ensure the axial pump can be enabled\", \"precondition\": \"Latest version of the axial pump available\", \"estimatedTime\": 138000, \"labels\": [\"Regression\"], \"priority\": {\"id\": 10002}, \"status\": {\"id\": 10000}, \"folder\": {\"id\": 100006}, \"owner\": {\"accountId\": \"5b10a2844c20165700ede21g\"}, \"customFields\": {\"Build Number\": 20}}",
            ),
        ],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "get_test_case_links",
        title: "GetTestCaseLinks",
        description: "Retrieve links associated with a test case.",
        verb: Verb::Get,
        path: &[L("testcases"), A("test_case_key"), L("links")],
        arguments: &[arg(
            "test_case_key",
            Identifier,
            "Key of the test case to retrieve links for.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "create_test_case_issue_link",
        title: "CreateTestCaseIssueLink",
        description: effect_note!("Create an issue link for a test case."),
        verb: Verb::Post,
        path: &[L("testcases"), A("test_case_key"), L("links"), L("issues")],
        arguments: &[
            arg(
                "test_case_key",
                Identifier,
                "Key of the test case to link an issue to.",
            ),
            arg("json", Json, ISSUE_LINK_JSON),
        ],
        query: &[],
        special: Special::IssueLink("test case"),
    },
    Operation {
        name: "create_test_case_web_link",
        title: "CreateTestCaseWebLink",
        description: effect_note!("Create a web link for a test case."),
        verb: Verb::Post,
        path: &[
            L("testcases"),
            A("test_case_key"),
            L("links"),
            L("weblinks"),
        ],
        arguments: &[
            arg(
                "test_case_key",
                Identifier,
                "Key of the test case to link a web link to.",
            ),
            arg("json", Json, WEB_LINK_JSON),
        ],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "list_test_case_versions",
        title: "ListTestCaseVersions",
        description: "List versions of a test case.",
        verb: Verb::Get,
        path: &[L("testcases"), A("test_case_key"), L("versions")],
        arguments: &[
            arg(
                "test_case_key",
                Identifier,
                "Key of the test case to list versions for.",
            ),
            MAX_RESULTS,
            START_AT,
        ],
        query: &PAGING,
        special: Special::None,
    },
    Operation {
        name: "get_test_case_version",
        title: "GetTestCaseVersion",
        description: "Retrieve a specific version of a test case.",
        verb: Verb::Get,
        path: &[
            L("testcases"),
            A("test_case_key"),
            L("versions"),
            A("version"),
        ],
        arguments: &[
            arg(
                "test_case_key",
                Identifier,
                "Key of the test case to retrieve a specific version for.",
            ),
            arg("version", Integer, "Version number to retrieve."),
        ],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "get_test_case_test_script",
        title: "GetTestCaseTestScript",
        description: "Retrieve the test script of a test case.",
        verb: Verb::Get,
        path: &[L("testcases"), A("test_case_key"), L("testscript")],
        arguments: &[arg(
            "test_case_key",
            Identifier,
            "Key of the test case to retrieve the test script for.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "create_test_case_test_script",
        title: "CreateTestCaseTestScript",
        description: effect_note!(
            "Create a test script for a test case. A test case with test steps loses them when a script is created."
        ),
        verb: Verb::Post,
        path: &[L("testcases"), A("test_case_key"), L("testscript")],
        arguments: &[
            arg(
                "test_case_key",
                Identifier,
                "Key of the test case to create a test script for.",
            ),
            arg(
                "json",
                Json,
                "JSON body to create a test script. Example: {\"type\": \"bdd\", \"text\": \"Attempt to login to the application\"} where type - test script type, plain or bdd.",
            ),
        ],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "get_test_case_test_steps",
        title: "GetTestCaseTestSteps",
        description: "List test steps of a test case. Returns the page's array of test step objects.",
        verb: Verb::Get,
        path: &[L("testcases"), A("test_case_key"), L("teststeps")],
        arguments: &[
            arg(
                "test_case_key",
                Identifier,
                "Key of the test case to retrieve test steps for.",
            ),
            MAX_RESULTS,
            START_AT,
        ],
        query: &PAGING,
        special: Special::Values,
    },
    Operation {
        name: "create_test_case_test_steps",
        title: "CreateTestCaseTestSteps",
        description: effect_note!(
            "Create test steps for a test case. APPEND adds steps; OVERWRITE replaces every existing step."
        ),
        verb: Verb::Post,
        path: &[L("testcases"), A("test_case_key"), L("teststeps")],
        arguments: &[
            arg(
                "test_case_key",
                Identifier,
                "Key of the test case to create test steps for.",
            ),
            arg(
                "json",
                Json,
                "JSON body to create test steps. Example: {\"mode\": \"APPEND\", \"items\": [{\"inline\": {\"description\": \"Attempt to login to the application\", \"testData\": \"Username = SmartBear Password = weLoveAtlassian\", \"expectedResult\": \"Login succeeds, web-app redirects to the dashboard view\"}}]} where mode is required (\"APPEND\" or \"OVERWRITE\") and each item contains either inline or testCase ({\"testCaseKey\": \"PROJ-T123\", \"parameters\": [...]}), never both.",
            ),
        ],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "list_test_cycles",
        title: "ListTestCycles",
        description: "List test cycles with optional filters.",
        verb: Verb::Get,
        path: &[L("testcycles")],
        arguments: &[
            arg(
                "project_key",
                OptionalIdentifier,
                "Project key to filter test cycles.",
            ),
            arg(
                "folder_id",
                OptionalIdentifier,
                "Folder ID to filter test cycles.",
            ),
            arg(
                "jira_project_version_id",
                OptionalIdentifier,
                "JIRA project version ID to filter test cycles.",
            ),
            MAX_RESULTS,
            START_AT,
        ],
        query: &[
            ("projectKey", "project_key"),
            ("folderId", "folder_id"),
            ("jiraProjectVersionId", "jira_project_version_id"),
            ("maxResults", "max_results"),
            ("startAt", "start_at"),
        ],
        special: Special::None,
    },
    Operation {
        name: "create_test_cycle",
        title: "CreateTestCycle",
        description: effect_note!("Create a new test cycle."),
        verb: Verb::Post,
        path: &[L("testcycles")],
        arguments: &[arg(
            "json",
            Json,
            "JSON body to create a test cycle. Example: {\"name\": \"Test Cycle Name\", \"description\": \"Test Cycle Description\", \"projectKey\": \"PROJECT_KEY\"}",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "get_test_cycle",
        title: "GetTestCycle",
        description: "Retrieve details of a specific test cycle.",
        verb: Verb::Get,
        path: &[L("testcycles"), A("test_cycle_id_or_key")],
        arguments: &[arg(
            "test_cycle_id_or_key",
            Identifier,
            "ID or key of the test cycle to retrieve.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "update_test_cycle",
        title: "UpdateTestCycle",
        description: effect_note!(
            "Update an existing test cycle. The body replaces the test cycle, so send the full object (fetch it with get_test_cycle first)."
        ),
        verb: Verb::Put,
        path: &[L("testcycles"), A("test_cycle_id_or_key")],
        arguments: &[
            arg(
                "test_cycle_id_or_key",
                Identifier,
                "ID or key of the test cycle to update.",
            ),
            arg(
                "json",
                Json,
                "JSON body to update a test cycle, the full test cycle object. Example: {\"id\": 1, \"key\": \"SA-R40\", \"name\": \"Sprint 1 Regression Test Cycle\", \"project\": {\"id\": 10005}, \"jiraProjectVersion\": {\"id\": 10000}, \"status\": {\"id\": 10000}, \"folder\": {\"id\": 100006}, \"description\": \"Regression test cycle 1 to ensure no breaking changes\", \"plannedStartDate\": \"2018-05-19T13:15:13Z\", \"plannedEndDate\": \"2018-05-20T13:15:13Z\", \"owner\": {\"accountId\": \"5b10a2844c20165700ede21g\"}, \"customFields\": {\"Build Number\": 20}}",
            ),
        ],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "get_test_cycle_links",
        title: "GetTestCycleLinks",
        description: "Retrieve links associated with a test cycle.",
        verb: Verb::Get,
        path: &[L("testcycles"), A("test_cycle_id_or_key"), L("links")],
        arguments: &[arg(
            "test_cycle_id_or_key",
            Identifier,
            "ID or key of the test cycle to retrieve links for.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "create_test_cycle_issue_link",
        title: "CreateTestCycleIssueLink",
        description: effect_note!("Create an issue link for a test cycle."),
        verb: Verb::Post,
        path: &[
            L("testcycles"),
            A("test_cycle_id_or_key"),
            L("links"),
            L("issues"),
        ],
        arguments: &[
            arg(
                "test_cycle_id_or_key",
                Identifier,
                "ID or key of the test cycle to link an issue to.",
            ),
            arg("json", Json, ISSUE_LINK_JSON),
        ],
        query: &[],
        special: Special::IssueLink("test cycle"),
    },
    Operation {
        name: "create_test_cycle_web_link",
        title: "CreateTestCycleWebLink",
        description: effect_note!("Create a web link for a test cycle."),
        verb: Verb::Post,
        path: &[
            L("testcycles"),
            A("test_cycle_id_or_key"),
            L("links"),
            L("weblinks"),
        ],
        arguments: &[
            arg(
                "test_cycle_id_or_key",
                Identifier,
                "ID or key of the test cycle to link a web link to.",
            ),
            arg("json", Json, WEB_LINK_JSON),
        ],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "list_test_executions",
        title: "ListTestExecutions",
        description: "List test executions with optional filters.",
        verb: Verb::Get,
        path: &[L("testexecutions")],
        arguments: &[
            arg(
                "project_key",
                OptionalIdentifier,
                "Project key to filter test executions.",
            ),
            arg(
                "test_cycle",
                OptionalIdentifier,
                "Test cycle to filter test executions.",
            ),
            arg(
                "test_case",
                OptionalIdentifier,
                "Test case to filter test executions.",
            ),
            MAX_RESULTS,
            START_AT,
        ],
        query: &[
            ("projectKey", "project_key"),
            ("testCycle", "test_cycle"),
            ("testCase", "test_case"),
            ("maxResults", "max_results"),
            ("startAt", "start_at"),
        ],
        special: Special::None,
    },
    Operation {
        name: "create_test_execution",
        title: "CreateTestExecution",
        description: effect_note!("Create a new test execution."),
        verb: Verb::Post,
        path: &[L("testexecutions")],
        arguments: &[arg(
            "json",
            Json,
            "JSON body to create a test execution. Example: {\"projectKey\": \"TIS\", \"testCaseKey\": \"SA-T10\", \"testCycleKey\": \"SA-R10\", \"statusName\": \"In Progress\", \"testScriptResults\": [{\"statusName\": \"In Progress\", \"actualEndDate\": \"2018-05-20T13:15:13Z\", \"actualResult\": \"User logged in successfully\"}], \"environmentName\": \"Chrome Latest Version\", \"actualEndDate\": \"2018-05-20T13:15:13Z\", \"executionTime\": 120000, \"executedById\": \"5b10a2844c20165700ede21g\", \"assignedToId\": \"5b10a2844c20165700ede21g\", \"comment\": \"Test failed user could not login\"}",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "get_test_execution",
        title: "GetTestExecution",
        description: "Retrieve details of a specific test execution.",
        verb: Verb::Get,
        path: &[L("testexecutions"), A("test_execution_id_or_key")],
        arguments: &[arg(
            "test_execution_id_or_key",
            Identifier,
            "ID or key of the test execution to retrieve.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "update_test_execution",
        title: "UpdateTestExecution",
        description: effect_note!("Update an existing test execution."),
        verb: Verb::Put,
        path: &[L("testexecutions"), A("test_execution_id_or_key")],
        arguments: &[
            arg(
                "test_execution_id_or_key",
                Identifier,
                "ID or key of the test execution to update.",
            ),
            arg(
                "json",
                Json,
                "JSON body to update a test execution. Example: {\"statusName\": \"In Progress\", \"environmentName\": \"Chrome Latest Version\", \"actualEndDate\": \"2018-05-20T13:15:13Z\", \"executionTime\": 120000, \"executedById\": \"5b10a2844c20165700ede21g\", \"assignedToId\": \"5b10a2844c20165700ede21g\", \"comment\": \"Test failed user could not login\"}",
            ),
        ],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "get_test_execution_test_steps",
        title: "GetTestExecutionTestSteps",
        description: "List test steps of a test execution.",
        verb: Verb::Get,
        path: &[
            L("testexecutions"),
            A("test_execution_id_or_key"),
            L("teststeps"),
        ],
        arguments: &[
            arg(
                "test_execution_id_or_key",
                Identifier,
                "ID or key of the test execution to retrieve test steps for.",
            ),
            MAX_RESULTS,
            START_AT,
        ],
        query: &PAGING,
        special: Special::None,
    },
    Operation {
        name: "update_test_execution_test_steps",
        title: "UpdateTestExecutionTestSteps",
        description: effect_note!("Update test steps of a test execution."),
        verb: Verb::Put,
        path: &[
            L("testexecutions"),
            A("test_execution_id_or_key"),
            L("teststeps"),
        ],
        arguments: &[
            arg(
                "test_execution_id_or_key",
                Identifier,
                "ID or key of the test execution to update test steps for.",
            ),
            arg(
                "json",
                Json,
                "JSON body to update test steps. Example: {\"steps\": [{\"actualResult\": \"User logged in successfully\", \"statusName\": \"In Progress\"}]}",
            ),
        ],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "sync_test_execution_script",
        title: "SyncTestExecutionScript",
        description: effect_note!(
            "Sync the test execution script with the current test case script."
        ),
        verb: Verb::Post,
        path: &[
            L("testexecutions"),
            A("test_execution_id_or_key"),
            L("teststeps"),
            L("sync"),
        ],
        arguments: &[arg(
            "test_execution_id_or_key",
            Identifier,
            "ID or key of the test execution to sync the script for.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "list_test_execution_links",
        title: "ListTestExecutionLinks",
        description: "List links associated with a test execution.",
        verb: Verb::Get,
        path: &[
            L("testexecutions"),
            A("test_execution_id_or_key"),
            L("links"),
        ],
        arguments: &[arg(
            "test_execution_id_or_key",
            Identifier,
            "ID or key of the test execution to retrieve links for.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "create_test_execution_issue_link",
        title: "CreateTestExecutionIssueLink",
        description: effect_note!("Create an issue link for a test execution."),
        verb: Verb::Post,
        path: &[
            L("testexecutions"),
            A("test_execution_id_or_key"),
            L("links"),
            L("issues"),
        ],
        arguments: &[
            arg(
                "test_execution_id_or_key",
                Identifier,
                "ID or key of the test execution to link an issue to.",
            ),
            arg("json", Json, ISSUE_LINK_JSON),
        ],
        query: &[],
        special: Special::IssueLink("test execution"),
    },
    Operation {
        name: "list_projects",
        title: "ListProjects",
        description: "List all projects.",
        verb: Verb::Get,
        path: &[L("projects")],
        arguments: &[MAX_RESULTS, START_AT],
        query: &PAGING,
        special: Special::None,
    },
    Operation {
        name: "get_project",
        title: "GetProject",
        description: "Retrieve details of a specific project.",
        verb: Verb::Get,
        path: &[L("projects"), A("project_id_or_key")],
        arguments: &[arg(
            "project_id_or_key",
            Identifier,
            "ID or key of the project to retrieve.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "list_folders",
        title: "ListFolders",
        description: "List folders with optional filters.",
        verb: Verb::Get,
        path: &[L("folders")],
        arguments: &[
            arg(
                "project_key",
                OptionalIdentifier,
                "Project key to filter folders.",
            ),
            arg(
                "folder_type",
                OptionalIdentifier,
                "Folder type to filter folders.",
            ),
            MAX_RESULTS,
            START_AT,
        ],
        query: &[
            ("projectKey", "project_key"),
            ("folderType", "folder_type"),
            ("maxResults", "max_results"),
            ("startAt", "start_at"),
        ],
        special: Special::None,
    },
    Operation {
        name: "create_folder",
        title: "CreateFolder",
        description: effect_note!(
            "Create a new folder. Give parentId, or parentName to have the parent folder looked up by name."
        ),
        verb: Verb::Post,
        path: &[L("folders")],
        arguments: &[arg(
            "json",
            Json,
            "JSON body to create a folder. Example: {\"parentId\": 123456, \"parentName\": \"parentFolder\", \"name\": \"ZephyrEssential_test\", \"projectKey\": \"PRJ\", \"folderType\": \"TEST_CASE\"}. Possible folder types: \"TEST_CASE\", \"TEST_PLAN\", \"TEST_CYCLE\".",
        )],
        query: &[],
        special: Special::CreateFolder,
    },
    Operation {
        name: "get_folder",
        title: "GetFolder",
        description: "Retrieve details of a specific folder.",
        verb: Verb::Get,
        path: &[L("folders"), A("folder_id")],
        arguments: &[arg(
            "folder_id",
            Identifier,
            "ID of the folder to retrieve.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "find_folder_by_name",
        title: "GetFolder",
        description: "Find a folder by its name, ignoring case.\n:param name: The name of the folder to search for.\n:param project_key: Optional filter by project key.\n:param folder_type: Optional filter by folder type.\n:return: The folder details if found, otherwise None.",
        verb: Verb::Get,
        path: &[L("folders")],
        arguments: &[
            arg("name", Text, "Name of the folder to retrieve."),
            arg("project_key", OptionalIdentifier, "Project key"),
            arg(
                "folder_type",
                OptionalIdentifier,
                "Folder type. Possible values: \"TEST_CASE\", \"TEST_PLAN\", \"TEST_CYCLE\"",
            ),
        ],
        query: &[],
        special: Special::FindFolder,
    },
    Operation {
        name: "delete_link",
        title: "DeleteLink",
        description: effect_note!("Delete a specific link. The deletion is permanent."),
        verb: Verb::Delete,
        path: &[L("links"), A("link_id")],
        arguments: &[arg("link_id", Identifier, "ID of the link to delete.")],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "get_issue_link_test_cases",
        title: "GetIssueLinkTestCases",
        description: "Retrieve test cases linked to an issue.",
        verb: Verb::Get,
        path: &[L("issuelinks"), A("issue_key"), L("testcases")],
        arguments: &[arg(
            "issue_key",
            Identifier,
            "Key of the issue to retrieve linked test cases for.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "get_issue_link_test_cycles",
        title: "GetIssueLinkTestCycles",
        description: "Retrieve test cycles linked to an issue.",
        verb: Verb::Get,
        path: &[L("issuelinks"), A("issue_key"), L("testcycles")],
        arguments: &[arg(
            "issue_key",
            Identifier,
            "Key of the issue to retrieve linked test cycles for.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "get_issue_link_test_plans",
        title: "GetIssueLinkTestPlans",
        description: "Retrieve test plans linked to an issue.",
        verb: Verb::Get,
        path: &[L("issuelinks"), A("issue_key"), L("testplans")],
        arguments: &[arg(
            "issue_key",
            Identifier,
            "Key of the issue to retrieve linked test plans for.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "get_issue_link_test_executions",
        title: "GetIssueLinkTestExecutions",
        description: "Retrieve test executions linked to an issue.",
        verb: Verb::Get,
        path: &[L("issuelinks"), A("issue_key"), L("executions")],
        arguments: &[arg(
            "issue_key",
            Identifier,
            "Key of the issue to retrieve linked test executions for.",
        )],
        query: &[],
        special: Special::None,
    },
    Operation {
        name: "healthcheck",
        title: "NoInput",
        description: "Perform a health check on the API.",
        verb: Verb::Get,
        path: &[L("healthcheck")],
        arguments: &[],
        query: &[],
        special: Special::None,
    },
];

#[derive(Clone, Copy)]
struct EssentialTool(&'static Operation);

#[async_trait]
impl ZephyrToolSpec for EssentialTool {
    type Api = ZephyrEssentialClient;

    fn name(self) -> &'static str {
        self.0.name
    }

    fn description(self) -> &'static str {
        self.0.description
    }

    fn is_read_only(self) -> bool {
        self.0.verb == Verb::Get
    }

    fn schema(self) -> Value {
        let properties = self
            .0
            .arguments
            .iter()
            .map(|argument| {
                let schema = match argument.kind {
                    Identifier => identifier_property(argument.description),
                    OptionalIdentifier => optional_identifier_property(argument.description),
                    Integer => integer_property(argument.description),
                    OptionalInteger => optional_integer_property(argument.description),
                    Json => json_text_property(argument.description),
                    Text => text_property(argument.description),
                };
                (argument.name, schema)
            })
            .collect::<Vec<_>>();
        let required = self
            .0
            .arguments
            .iter()
            .filter(|argument| matches!(argument.kind, Identifier | Integer | Json | Text))
            .map(|argument| argument.name)
            .collect::<Vec<_>>();
        object_schema(self.0.title, &properties, &required)
    }

    fn family() -> &'static ZephyrRestFamily {
        &FAMILY
    }

    fn arguments() -> &'static ZephyrArgumentCodes {
        &ARGUMENTS
    }

    async fn execute(
        self,
        api: &ZephyrEssentialClient,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        let operation = self.0;
        let allowed = operation
            .arguments
            .iter()
            .map(|argument| argument.name)
            .collect::<Vec<_>>();
        let args = Arguments::new(arguments, &ARGUMENTS, &allowed)?;
        let into_adk = |error: ZephyrRestError| error.into_adk(&FAMILY);

        if let Special::FindFolder = operation.special {
            let found = api
                .find_folder_by_name(
                    args.text("name")?,
                    args.optional_identifier("project_key")?,
                    args.optional_identifier("folder_type")?,
                )
                .await
                .map_err(into_adk)?;
            return Ok(found.unwrap_or(Value::Null));
        }

        // Every argument is read and validated before the request is built.
        let mut rendered = Vec::with_capacity(operation.arguments.len());
        for argument in operation.arguments {
            let value = match argument.kind {
                Identifier => Some(args.identifier(argument.name)?.to_owned()),
                OptionalIdentifier => args.optional_identifier(argument.name)?.map(str::to_owned),
                Integer => Some(args.integer(argument.name)?.to_string()),
                OptionalInteger => args
                    .optional_integer(argument.name)?
                    .map(|value| value.to_string()),
                Json | Text => None,
            };
            rendered.push((argument.name, value));
        }
        let value_of = |name: &str| {
            rendered
                .iter()
                .find(|(argument, _)| *argument == name)
                .and_then(|(_, value)| value.as_deref())
        };
        let segments = operation
            .path
            .iter()
            .map(|segment| match segment {
                Segment::Literal(literal) => Ok(*literal),
                Segment::Argument(name) => value_of(name).ok_or_else(|| args.invalid()),
            })
            .collect::<adk_core::Result<Vec<_>>>()?;
        let query = operation
            .query
            .iter()
            .filter_map(|(api_name, argument)| {
                value_of(argument).map(|value| (*api_name, value.to_owned()))
            })
            .collect::<Vec<_>>();
        let mut body = if operation
            .arguments
            .iter()
            .any(|argument| argument.kind == Json)
        {
            Some(args.json("json")?)
        } else {
            None
        };

        match operation.special {
            Special::IssueLink(entity) => {
                if let Some(message) = issue_link_problem(body.as_ref(), entity, &args)? {
                    return Ok(Value::String(message));
                }
            }
            Special::CreateFolder => {
                if let Some(message) = resolve_parent_folder(api, body.as_mut(), &args).await? {
                    return Ok(Value::String(message));
                }
            }
            Special::None | Special::Values | Special::FindFolder => {}
        }

        let method = match operation.verb {
            Verb::Get => Method::GET,
            Verb::Post => Method::POST,
            Verb::Put => Method::PUT,
            Verb::Delete => Method::DELETE,
        };
        let result = api
            .request(method, &segments, &query, body.as_ref())
            .await
            .map_err(into_adk)?;
        match operation.special {
            Special::Values => result
                .get("values")
                .cloned()
                .ok_or_else(invalid_response)
                .map_err(into_adk),
            _ => Ok(result),
        }
    }
}

/// The SDK's `_validate_issue_link_data`: the API links by numeric
/// `issueId`, and the SDK explains how to get one when given an `issueKey`.
fn issue_link_problem(
    body: Option<&Value>,
    entity: &str,
    args: &Arguments<'_>,
) -> adk_core::Result<Option<String>> {
    let object = body
        .and_then(Value::as_object)
        .ok_or_else(|| args.invalid())?;
    if object.contains_key("issueId") {
        return Ok(None);
    }
    if let Some(issue_key) = object.get("issueKey") {
        let issue_key = python_str(Some(issue_key));
        return Ok(Some(format!(
            "Zephyr Essential API requires 'issueId' (numeric Jira issue ID), not 'issueKey'. You provided issueKey='{issue_key}'. To find the issueId, Jira toolkit can be used or raw Jira API: GET /rest/api/2/issue/{issue_key} and look for the 'id' field in the response."
        )));
    }
    Ok(Some(format!(
        "Missing required field 'issueId' in JSON payload for {entity} issue link. Example: {{\"issueId\": 10100}}"
    )))
}

/// The SDK's `create_folder` prelude: without `parentId`, a `parentName` is
/// resolved to the folder of that name. The lookup is scoped to the payload's
/// `projectKey` and `folderType` when present (the SDK searched every
/// project, so a same-named folder elsewhere could become the parent).
async fn resolve_parent_folder(
    api: &ZephyrEssentialClient,
    body: Option<&mut Value>,
    args: &Arguments<'_>,
) -> adk_core::Result<Option<String>> {
    let object = body
        .and_then(Value::as_object_mut)
        .ok_or_else(|| args.invalid())?;
    if object.contains_key("parentId") {
        return Ok(None);
    }
    let Some(parent_name) = object.get("parentName") else {
        return Ok(None);
    };
    let parent_name = parent_name
        .as_str()
        .ok_or_else(|| args.invalid())?
        .to_owned();
    let project_key = object
        .get("projectKey")
        .and_then(Value::as_str)
        .map(str::to_owned);
    let folder_type = object
        .get("folderType")
        .and_then(Value::as_str)
        .map(str::to_owned);
    let parent = api
        .find_folder_by_name(&parent_name, project_key.as_deref(), folder_type.as_deref())
        .await
        .map_err(|error| error.into_adk(&FAMILY))?;
    let Some(parent) = parent else {
        return Ok(Some(format!(
            "Parent folder with name '{parent_name}' not found."
        )));
    };
    let parent_id = parent
        .get("id")
        .cloned()
        .ok_or_else(|| invalid_response().into_adk(&FAMILY))?;
    object.insert("parentId".to_owned(), parent_id);
    Ok(None)
}
