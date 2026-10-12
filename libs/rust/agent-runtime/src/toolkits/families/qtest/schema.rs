//! The served qTest tools: names, SDK descriptions and argument schemas
//! (kept SDK-compatible; `sdk_conformance` checks them).

use serde_json::{Value, json};

const MAX_TEXT_BYTES: usize = 256 * 1_024;
const MAX_ID_BYTES: usize = 256;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum QtestToolKind {
    SearchByDql,
    CreateTestCases,
    UpdateTestCase,
    UpdateTestRunStatus,
    GetTestCaseVersions,
    FindTestCaseById,
    DeleteTestCase,
    LinkTestsToJiraRequirement,
    LinkTestsToQtestRequirement,
    GetModules,
    GetAllTestCasesFieldsForProject,
    FindTestCasesByRequirementId,
    FindRequirementsByTestCaseId,
    FindTestRunsByTestCaseId,
    FindDefectsByTestRunId,
    SearchEntitiesByDql,
    FindEntityById,
}

impl QtestToolKind {
    /// The SDK's `get_available_tools` order without the two artifact
    /// uploads and the inherited index tools.
    pub(super) const ALL: [Self; 17] = [
        Self::SearchByDql,
        Self::CreateTestCases,
        Self::UpdateTestCase,
        Self::UpdateTestRunStatus,
        Self::GetTestCaseVersions,
        Self::FindTestCaseById,
        Self::DeleteTestCase,
        Self::LinkTestsToJiraRequirement,
        Self::LinkTestsToQtestRequirement,
        Self::GetModules,
        Self::GetAllTestCasesFieldsForProject,
        Self::FindTestCasesByRequirementId,
        Self::FindRequirementsByTestCaseId,
        Self::FindTestRunsByTestCaseId,
        Self::FindDefectsByTestRunId,
        Self::SearchEntitiesByDql,
        Self::FindEntityById,
    ];

    pub(super) const fn name(self) -> &'static str {
        match self {
            Self::SearchByDql => "search_by_dql",
            Self::CreateTestCases => "create_test_cases",
            Self::UpdateTestCase => "update_test_case",
            Self::UpdateTestRunStatus => "update_test_run_status",
            Self::GetTestCaseVersions => "get_test_case_versions",
            Self::FindTestCaseById => "find_test_case_by_id",
            Self::DeleteTestCase => "delete_test_case",
            Self::LinkTestsToJiraRequirement => "link_tests_to_jira_requirement",
            Self::LinkTestsToQtestRequirement => "link_tests_to_qtest_requirement",
            Self::GetModules => "get_modules",
            Self::GetAllTestCasesFieldsForProject => "get_all_test_cases_fields_for_project",
            Self::FindTestCasesByRequirementId => "find_test_cases_by_requirement_id",
            Self::FindRequirementsByTestCaseId => "find_requirements_by_test_case_id",
            Self::FindTestRunsByTestCaseId => "find_test_runs_by_test_case_id",
            Self::FindDefectsByTestRunId => "find_defects_by_test_run_id",
            Self::SearchEntitiesByDql => "search_entities_by_dql",
            Self::FindEntityById => "find_entity_by_id",
        }
    }

    pub(super) const fn is_read_only(self) -> bool {
        !matches!(
            self,
            Self::CreateTestCases
                | Self::UpdateTestCase
                | Self::UpdateTestRunStatus
                | Self::DeleteTestCase
                | Self::LinkTestsToJiraRequirement
                | Self::LinkTestsToQtestRequirement
        )
    }

    /// The SDK descriptions (each is cut to 1000 characters with its
    /// suffix, as the SDK cuts it).
    pub(super) const fn description(self) -> &'static str {
        match self {
            Self::SearchByDql => {
                "Search test cases in qTest using Data Query Language (DQL).\n\nCRITICAL: USE SINGLE QUOTES ONLY - DQL does not support double quotes!\n- ✓ CORRECT: Description ~ 'Forgot Password'\n- ✗ WRONG: Description ~ \"Forgot Password\"\n\nLIMITATION - CANNOT SEARCH BY LINKED OBJECTS:\n- ✗ 'Requirement Id' = 'RQ-15' will fail - use 'find_test_cases_by_requirement_id' tool instead\n- ✗ Linked defects or other relationship queries are not supported\n\nSEARCHABLE FIELDS:\n- Direct fields: Id, Name, Description, Status, Type, Priority, Automation, etc.\n- Module: Use 'Module in' syntax\n- Custom fields: Use exact field name from project configuration\n- Date fields: MUST use ISO DateTime format (e.g., '2024-01-01T00:00:00.000Z')\n\nSYNTAX RULES:\n1. ALL string values MUST use single quotes (never double quotes)\n2. Field names with spaces MUST be in single quotes: 'Created Date' > '2024-01-01T00:00:00.000Z'\n3. Use ~ for 'contains', !~ for 'not contains': Description ~ 'login'\n4. Use 'is not empty' for non-empty check: Name is 'not empty'\n5. Operators: =, !=, <, >, <=, >=, in, ~, !~\n\nEXAMPLES:\n- Id = 'TC-123'\n- Description ~ 'Forgot Password'\n- Status = 'New' and Priority = 'High'\n- Module in 'MD-78 Master Test Suite'\n- Name ~ 'login'\n- 'Created Date' > '2024-01-01T00:00:00.000Z'\n"
            }
            Self::CreateTestCases => "Create a test case in qTest.",
            Self::UpdateTestCase => "Update, change or replace data in the test case.",
            Self::UpdateTestRunStatus => {
                "Update a manual test run's execution result (status) in QTest.\n\nUse this after manually executing a test to record Pass/Fail/etc. It creates a\nnew manual execution log so the existing execution history remains unchanged.\n\nParameters:\n- test_run_id: Test run ID in format TR-123 or QTest numeric ID\n- status: One of 'Passed', 'Failed', 'Skipped', 'Blocked', 'Broken', 'No Result', 'Pending', 'Unknown', 'Incomplete' (must match project's configured status names)\n- note: Optional execution note\n- testcase_version_id: Optional numeric version ID (not the name - resolve it with get_test_case_versions). Omit to keep the run's current version.\n\nExamples:\n- Mark passed: test_run_id='TR-39', status='Passed'\n- Mark failed with note: test_run_id='TR-39', status='Failed', note='Login button unresponsive'\n- With a version: test_run_id='TR-39', status='Passed', testcase_version_id=4626964\n"
            }
            Self::GetTestCaseVersions => {
                "List the versions of a QTest test case with their numeric version IDs.\n\nUse this to resolve a version name (e.g. '2.0') to the numeric version ID that\nupdate_test_run_status expects in its testcase_version_id parameter.\n\nParameters:\n- test_case_id: Test case ID in format TC-123 or QTest numeric ID\n- version_name: Optional version name to look up (e.g. '2.0'). Omit to list every available version.\n\nReturns: test_case_id, qtest_test_case_id, total, and versions with version_id, version and name.\n\nNOTE: QTest returns approved major versions plus the latest unapproved minor\nversion, so intermediate minor versions (1.1, 2.1) are not listed.\n\nExamples:\n- List all versions: test_case_id='TC-123'\n- Resolve one version: test_case_id='TC-123', version_name='2.0'\n"
            }
            Self::FindTestCaseById => {
                "Find the test case and its fields (e.g., 'QTest Id') by test case id. Id should be in format TC-123"
            }
            Self::DeleteTestCase => {
                "Delete test case by its qtest id. Id should be in format 3534653120."
            }
            Self::LinkTestsToJiraRequirement => {
                "Link test cases to external Jira requirement. Provide Jira issue ID (e.g., PLAN-128) and list of test case IDs in format '[\"TC-123\", \"TC-234\"]'"
            }
            Self::LinkTestsToQtestRequirement => {
                "Link test cases to internal QTest requirement. Provide QTest requirement ID (e.g., RQ-15) and list of test case IDs in format '[\"TC-123\", \"TC-234\"]'"
            }
            Self::GetModules => {
                "\n        :param int project_id: ID of the project (required)\n        :param int parent_id: ID of the parent Module. Leave it blank to retrieve Modules under root\n        :param str search: The free-text to search for Modules by names. You can utilize this parameter to search for Modules. Leave it blank to retrieve all Modules under root or the parent Module\n        "
            }
            Self::GetAllTestCasesFieldsForProject => {
                "Get information about available test case fields and their valid values for the project. Shows which property values are allowed (e.g., Status: 'New', 'In Progress', 'Completed') based on the project configuration. Use force_refresh=true if project configuration has changed."
            }
            Self::FindTestCasesByRequirementId => {
                "Find all test cases linked to a QTest requirement.\n\nUse this tool to find test cases associated with a specific requirement.\nDQL search cannot query by linked requirement - use this tool instead.\n\nParameters:\n- requirement_id: QTest requirement ID in format RQ-123\n- include_details: If true, returns full test case data. If false (default), returns Id, QTest Id, Name, and Description.\n\nExamples:\n- Find test cases for RQ-15: requirement_id='RQ-15'\n- Get full details: requirement_id='RQ-15', include_details=true\n"
            }
            Self::FindRequirementsByTestCaseId => {
                "Find all requirements linked to a test case (direct link: test-case 'covers' requirements).\n\nUse this tool to discover which requirements a specific test case covers.\n\nParameters:\n- test_case_id: Test case ID in format TC-123\n\nReturns: List of linked requirements with Id, QTest Id, Name, and Description.\n\nExamples:\n- Find requirements for TC-123: test_case_id='TC-123'\n"
            }
            Self::FindTestRunsByTestCaseId => {
                "Find all test runs associated with a test case.\n\nIMPORTANT: In QTest, defects are NOT directly linked to test cases. \nDefects are linked to TEST RUNS. To find defects related to a test case:\n1. First use this tool to find test runs for the test case\n2. Then use find_defects_by_test_run_id for each test run\n\nParameters:\n- test_case_id: Test case ID in format TC-123\n\nReturns: List of test runs with Id, QTest Id, Name, and Description.\nAlso includes a hint about finding defects via test runs.\n\nExamples:\n- Find test runs for TC-123: test_case_id='TC-123'\n"
            }
            Self::FindDefectsByTestRunId => {
                "Find all defects associated with a test run.\n\nIn QTest data model, defects are linked to test runs (not directly to test cases).\nA defect found here means it was reported during execution of this specific test run.\n\nTo find defects related to a test case:\n1. First use find_test_runs_by_test_case_id to get test runs\n2. Then use this tool for each test run\n\nParameters:\n- test_run_id: Test run ID in format TR-123\n\nReturns: List of defects with Id, QTest Id, Name, and Description.\n\nExamples:\n- Find defects for TR-39: test_run_id='TR-39'\n"
            }
            Self::SearchEntitiesByDql => {
                "Search any QTest entity type using Data Query Language (DQL).\n\nThis is a unified search tool for all searchable QTest entity types.\n\nSUPPORTED ENTITY TYPES (object_type parameter):\n- 'test-cases' (TC-xxx): Test case definitions with steps\n- 'test-runs' (TR-xxx): Execution instances of test cases  \n- 'defects' (DF-xxx): Bugs/issues found during testing\n- 'requirements' (RQ-xxx): Requirements to be tested\n- 'test-suites' (TS-xxx): Collections of test runs\n- 'test-cycles' (CL-xxx): Test execution cycles\n- 'test-logs': Execution logs (date queries ONLY - see notes)\n- 'releases' (RL-xxx): Software releases\n- 'builds' (BL-xxx): Builds within releases\n\nNOTES: \n- Modules (MD-xxx) are NOT searchable via DQL. Use 'get_modules' tool instead.\n- Test-logs: Only date queries work (Execution Start Date, Execution End Date).\n  For specific test log details, use find_test_runs_by_test_case_id - \n  the test run includes 'Latest Test Log' with status and execution times.\n\nCRITICAL: USE SINGLE QUOTES ONLY - DQL does not support double quotes!\n- ✓ CORRECT: Description ~ 'Forgot Password'\n- ✗ WRONG: Description ~ \"Forgot Password\"\n"
            }
            Self::FindEntityById => {
                "Find any QTest entity by its ID.\n\nThis universal lookup tool works for entity types that have ID prefixes. \nThe entity type is automatically determined from the ID prefix.\n\nSUPPORTED ID FORMATS:\n- TC-123: Test Case\n- TR-39: Test Run  \n- DF-100: Defect\n- RQ-15: Requirement\n- TS-5: Test Suite\n- CL-3: Test Cycle\n- RL-1: Release\n- BL-2: Build\n\nNOT SUPPORTED (no ID prefix):\n- Test Logs: Get details from test run's 'Latest Test Log' field (contains Log Id, Status, Execution Start/End Date)\n- Modules: Use 'get_modules' tool instead\n\nParameters:\n- entity_id: Entity ID with prefix (e.g., TC-123, RQ-15, DF-100, TR-39)\n\nReturns: Full entity details including all properties.\n\nExamples:\n- Find test case: entity_id='TC-123'\n- Find requirement: entity_id='RQ-15'\n- Find defect: entity_id='DF-100'\n- Find test run: entity_id='TR-39'\n"
            }
        }
    }
}

fn text(description: &str) -> Value {
    json!({"type":"string","minLength":1,"maxLength":MAX_TEXT_BYTES,"description":description})
}

fn id(description: &str) -> Value {
    json!({"type":"string","minLength":1,"maxLength":MAX_ID_BYTES,"description":description})
}

fn flag(description: &str) -> Value {
    json!({"type":["boolean","null"],"default":false,"description":description})
}

fn optional_text(description: &str) -> Value {
    json!({"type":["string","null"],"default":null,"description":description})
}

const EXTRACT_IMAGES: &str = "Whether embedded images should be processed by LLM. This runtime cannot transcribe images: embedded images are removed from the text either way.";
const PROMPT: &str = "Optional override prompt for image processing (unused in this runtime: embedded images are removed).";
const TEST_CASE_JSON: &str = "Provide test case in json format: a JSON object (or array of objects) whose keys are the project's field names. Name, Description, Precondition and Steps (a list of objects with 'Test Step Number', 'Test Step Description', 'Test Step Expected Result') are core fields; other keys map to the project's fields (use get_all_test_cases_fields_for_project for names and allowed values). Multi-select fields take a value or a list; null clears a text or multi-select field. 'QTest Id' is required to update and must be omitted to create. For updates, provide ONLY the fields you want to change. Example: {\"Name\": \"Brief title.\", \"Description\": \"Short purpose.\", \"Type\": \"Manual\", \"Status\": \"New\", \"Team\": [\"Epam\", \"EJ\"], \"Steps\": [{\"Test Step Number\": 1, \"Test Step Description\": \"Navigate to url\", \"Test Step Expected Result\": \"Page content is loaded\"}]}";
const TEST_CASE_IDS: &str = "List of the test case ids to be linked to particular requirement, as a JSON array string, e.g. '[\"TC-123\", \"TC-234\", \"TC-456\"]'.";

pub(super) fn schema(kind: QtestToolKind) -> Value {
    let (properties, required) = match kind {
        QtestToolKind::SearchByDql
        | QtestToolKind::FindTestCaseById
        | QtestToolKind::GetModules
        | QtestToolKind::GetAllTestCasesFieldsForProject
        | QtestToolKind::SearchEntitiesByDql
        | QtestToolKind::FindEntityById => read_properties(kind),
        QtestToolKind::FindTestCasesByRequirementId
        | QtestToolKind::FindRequirementsByTestCaseId
        | QtestToolKind::FindTestRunsByTestCaseId
        | QtestToolKind::FindDefectsByTestRunId
        | QtestToolKind::GetTestCaseVersions => relation_properties(kind),
        _ => effect_properties(kind),
    };
    json!({
        "type":"object",
        "properties":properties,
        "required":required,
        "additionalProperties":false
    })
}

fn read_properties(kind: QtestToolKind) -> (Value, &'static [&'static str]) {
    match kind {
        QtestToolKind::SearchByDql => (
            json!({
                "dql":text("Qtest Data Query Language (DQL) query string"),
                "extract_images":flag(EXTRACT_IMAGES),
                "prompt":optional_text(PROMPT),
                "max_results":{
                    "type":["integer","null"], "default":20,
                    "description":"Maximum total results to fetch across all pages. Set to 0 or negative for unlimited. Default: 20"
                },
                "append_test_steps":flag("When True, include full test-step data for every result. Defaults to False for lighter, faster responses; opt in only when step-level detail is needed."),
                "include_external_properties":flag("When True, include all external properties for every result. Defaults to False for lighter responses.")
            }),
            &["dql"],
        ),
        QtestToolKind::FindTestCaseById => (
            json!({
                "test_id":id("Test case ID e.g. TC-1234"),
                "extract_images":flag(EXTRACT_IMAGES),
                "prompt":optional_text(PROMPT)
            }),
            &["test_id"],
        ),
        QtestToolKind::GetModules => (
            json!({
                "parent_id":{
                    "type":["integer","null"], "default":null,
                    "description":"ID of the parent Module. Leave it blank to retrieve Modules under root"
                },
                "search":optional_text("The free-text to search for Modules by names. Leave it blank to retrieve all Modules under root or the parent Module")
            }),
            &[],
        ),
        QtestToolKind::GetAllTestCasesFieldsForProject => (
            json!({"force_refresh":flag("Set to true to reload field definitions from API if project configuration has changed (new fields added, dropdown values modified). Default: false (uses cached data).")}),
            &[],
        ),
        QtestToolKind::SearchEntitiesByDql => (
            json!({
                "object_type":id("Entity type to search: 'test-cases', 'test-runs', 'defects', 'requirements', 'test-suites', 'test-cycles', 'test-logs', 'releases', or 'builds'. Note: test-logs only support date queries; modules are NOT searchable - use get_modules tool."),
                "dql":text("QTest Data Query Language (DQL) query string")
            }),
            &["object_type", "dql"],
        ),
        _ => (
            json!({"entity_id":id("Entity ID with prefix: TC-123 (test case), RQ-15 (requirement), DF-100 (defect), TR-39 (test run), TS-5 (test suite), CL-3 (test cycle), RL-1 (release), or BL-2 (build). Note: test-logs and modules do NOT have ID prefixes.")}),
            &["entity_id"],
        ),
    }
}

fn relation_properties(kind: QtestToolKind) -> (Value, &'static [&'static str]) {
    match kind {
        QtestToolKind::FindTestCasesByRequirementId => (
            json!({
                "requirement_id":id("QTest requirement ID in format RQ-123. This will find all test cases linked to this requirement."),
                "include_details":flag("If true, returns full test case details. If false (default), returns Id, QTest Id, Name, and Description fields.")
            }),
            &["requirement_id"],
        ),
        QtestToolKind::FindRequirementsByTestCaseId => (
            json!({"test_case_id":id("Test case ID in format TC-123. This will find all requirements linked to this test case.")}),
            &["test_case_id"],
        ),
        QtestToolKind::FindTestRunsByTestCaseId => (
            json!({"test_case_id":id("Test case ID in format TC-123. This will find all test runs associated with this test case.")}),
            &["test_case_id"],
        ),
        QtestToolKind::FindDefectsByTestRunId => (
            json!({"test_run_id":id("Test run ID in format TR-123. This will find all defects associated with this test run.")}),
            &["test_run_id"],
        ),
        _ => (
            json!({
                "test_case_id":id("Test case ID in format TC-123 or QTest numeric ID"),
                "version_name":optional_text("Optional version name to look up, e.g. '2.0'. If omitted, every version qTest lists is returned.")
            }),
            &["test_case_id"],
        ),
    }
}

fn effect_properties(kind: QtestToolKind) -> (Value, &'static [&'static str]) {
    match kind {
        QtestToolKind::CreateTestCases => (
            json!({
                "test_case_content":text(TEST_CASE_JSON),
                "folder_to_place_test_cases_to":{
                    "type":"string", "default":"", "maxLength":MAX_ID_BYTES,
                    "description":"Folder to place test cases to (the module's full name, e.g. 'MD-78 Master Test Suite'). Default is empty value"
                }
            }),
            &["test_case_content"],
        ),
        QtestToolKind::UpdateTestCase => (
            json!({"test_id":id("Test ID e.g. TC-1234"), "test_case_content":text(TEST_CASE_JSON)}),
            &["test_id", "test_case_content"],
        ),
        QtestToolKind::UpdateTestRunStatus => (
            json!({
                "test_run_id":id("Test run ID in format TR-123 or QTest numeric ID"),
                "status":id("Manual test run status. Standard values: 'Passed', 'Failed', 'Skipped', 'Blocked', 'Broken', 'No Result', 'Pending', 'Unknown', 'Incomplete'. Must match a status name configured in the project's Field Settings."),
                "note":optional_text("Optional execution note/comment to attach to the test log."),
                "testcase_version_id":{
                    "type":["integer","null"], "default":null,
                    "description":"Optional numeric test case version ID, not the version name. Use get_test_case_versions to resolve a name like '2.0'. If omitted, the test run's current version is used."
                }
            }),
            &["test_run_id", "status"],
        ),
        QtestToolKind::DeleteTestCase => (
            json!({"qtest_id":{"type":"integer","minimum":1,"description":"Qtest id e.g. 3253490123"}}),
            &["qtest_id"],
        ),
        QtestToolKind::LinkTestsToJiraRequirement => (
            json!({
                "requirement_external_id":id("Qtest requirement external id which represent jira issue id linked to Qtest as a requirement e.g. SITEPOD-4038"),
                "json_list_of_test_case_ids":text(TEST_CASE_IDS)
            }),
            &["requirement_external_id", "json_list_of_test_case_ids"],
        ),
        _ => (
            json!({
                "requirement_id":id("QTest internal requirement ID in format RQ-123"),
                "json_list_of_test_case_ids":text(TEST_CASE_IDS)
            }),
            &["requirement_id", "json_list_of_test_case_ids"],
        ),
    }
}
