//! The served `TestRail` tools: names, descriptions and argument schemas
//! (kept SDK-compatible; `sdk_conformance` checks them).

use serde_json::{Value, json};

const MAX_ID_BYTES: usize = 20;
const MAX_TITLE_BYTES: usize = 4 * 1_024;
const MAX_JSON_BYTES: usize = 128 * 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum TestRailToolKind {
    GetCase,
    GetCases,
    GetCasesByFilter,
    AddCase,
    AddCases,
    UpdateCase,
    DeleteCase,
    GetSuites,
    GetSections,
    AddSection,
    DeleteSection,
    GetRun,
    GetRuns,
    GetResultsForRun,
    GetResultsForCase,
    GetResults,
}

impl TestRailToolKind {
    /// The SDK's `get_available_tools` order without `add_file_to_case` and
    /// the inherited index tools.
    pub(super) const ALL: [Self; 16] = [
        Self::GetCase,
        Self::GetCases,
        Self::GetCasesByFilter,
        Self::AddCase,
        Self::AddCases,
        Self::UpdateCase,
        Self::DeleteCase,
        Self::GetSuites,
        Self::GetSections,
        Self::AddSection,
        Self::DeleteSection,
        Self::GetRun,
        Self::GetRuns,
        Self::GetResultsForRun,
        Self::GetResultsForCase,
        Self::GetResults,
    ];

    pub(super) const fn name(self) -> &'static str {
        match self {
            Self::GetCase => "get_case",
            Self::GetCases => "get_cases",
            Self::GetCasesByFilter => "get_cases_by_filter",
            Self::AddCase => "add_case",
            Self::AddCases => "add_cases",
            Self::UpdateCase => "update_case",
            Self::DeleteCase => "delete_case",
            Self::GetSuites => "get_suites",
            Self::GetSections => "get_sections",
            Self::AddSection => "add_section",
            Self::DeleteSection => "delete_section",
            Self::GetRun => "get_run",
            Self::GetRuns => "get_runs",
            Self::GetResultsForRun => "get_results_for_run",
            Self::GetResultsForCase => "get_results_for_case",
            Self::GetResults => "get_results",
        }
    }

    pub(super) const fn is_read_only(self) -> bool {
        !matches!(
            self,
            Self::AddCase
                | Self::AddCases
                | Self::UpdateCase
                | Self::DeleteCase
                | Self::AddSection
                | Self::DeleteSection
        )
    }

    /// The SDK docstrings, condensed.
    pub(super) const fn description(self) -> &'static str {
        match self {
            Self::GetCase => "Extracts information about single test case from Testrail",
            Self::GetCases => {
                "Extracts a list of test cases in the specified format: `json`, `csv`, or `markdown`. Returns the selected case field keys (default ['title', 'id']) for every case of the project, or of one suite when the project is in multiple-suite mode."
            }
            Self::GetCasesByFilter => {
                "Extracts test cases from a specified project based on given case attributes (suite_id, section_id, created_after, priority_id, type_id, filter, limit, offset, ...), rendered as `json`, `csv` or `markdown`, optionally reduced to the given keys."
            }
            Self::AddCase => {
                "Adds new test case into Testrail per defined parameters: the section id, the title and a JSON object of case properties (template_id, type_id, priority_id, estimate, refs, custom_* fields such as custom_steps or custom_steps_separated)."
            }
            Self::AddCases => {
                "Adds new test cases into Testrail per defined parameters: a JSON array of objects with section_id, title and optional case_properties."
            }
            Self::UpdateCase => {
                "Updates an existing test case. Partial updates are supported. Pass case_properties as a JSON string with the fields to update. Supports custom fields (custom_steps, custom_preconds, custom_expected, etc.) and inline image embedding via HTML img tags."
            }
            Self::DeleteCase => {
                "Deletes an existing test case. soft_delete=true (default) marks the case as deleted; false permanently deletes it."
            }
            Self::GetSuites => "Extracts a list of test suites for a given project from Testrail",
            Self::GetSections => {
                "Extracts a list of sections for a given project from Testrail. For projects in multiple suite mode (suite_mode 2 or 3), pass a suite_id to scope the result; if omitted, sections from all suites are returned."
            }
            Self::AddSection => {
                "Adds a new section into Testrail per defined parameters: the project id, the section name and optional properties (description, suite_id, parent_id)."
            }
            Self::DeleteSection => {
                "Deletes an existing section. In TestRail, deleting a section is permanent and also removes all of its test cases, tests and results. soft_delete=true only previews the affected data without deleting anything."
            }
            Self::GetRun => {
                "Extracts a single test run from Testrail: the run's metadata and status counts, returned as a single-element list in the chosen format."
            }
            Self::GetRuns => {
                "Extracts a list of test runs for a given project from Testrail. Only returns test runs that are not part of a test plan. Pass an optional run_filter (JSON string or dict) to narrow the result, e.g. {\"is_completed\": 0} for active runs or {\"suite_id\": 6} for a specific suite."
            }
            Self::GetResultsForRun => {
                "Extracts all test results for a given test run from Testrail. Returns every result recorded in the run (auto-paginated). Pass an optional result_filter to narrow by status, defect or creation date, e.g. {\"status_id\": [5]} for failed results only."
            }
            Self::GetResultsForCase => {
                "Extracts the test results for a run + case combination from Testrail. A 'test' is the instance of a case within a run; this returns that test's result history (auto-paginated). Pass an optional result_filter to narrow by status or defect."
            }
            Self::GetResults => {
                "Extracts the result history for a single test from Testrail. A 'test' is an instance of a case within a run. Results are auto-paginated. Supported filters: status_id, defects_filter."
            }
        }
    }
}

fn id(description: &str) -> Value {
    json!({"type":"string","minLength":1,"maxLength":MAX_ID_BYTES,"description":description})
}

fn optional_id(description: &str) -> Value {
    json!({"type":["string","null"],"maxLength":MAX_ID_BYTES,"default":null,"description":description})
}

fn output_format() -> Value {
    json!({
        "type":"string", "default":"json", "maxLength":64,
        "description":"Desired output format. Supported values: 'json', 'csv', 'markdown'. Defaults to 'json'."
    })
}

fn filter(description: &str) -> Value {
    json!({
        "type":["string","object","null"], "default":null,
        "description":format!("[Optional] {description}")
    })
}

const RESULT_FILTER: &str = "Optional filters for the results, as a JSON string or dict. Supported keys: status_id (list[int] or comma-separated string; built-in: 1 Passed, 2 Blocked, 4 Retest, 5 Failed), defects_filter (a single Defect ID), created_after / created_before (UNIX timestamps, run-level only), created_by (user IDs, run-level only). Pagination is handled automatically; 'limit'/'offset' are ignored.";

pub(super) fn schema(kind: TestRailToolKind) -> Value {
    let (properties, required): (Value, &[&str]) = match kind {
        TestRailToolKind::GetCase
        | TestRailToolKind::GetCases
        | TestRailToolKind::GetCasesByFilter
        | TestRailToolKind::GetSuites
        | TestRailToolKind::GetSections => read_properties(kind),
        TestRailToolKind::GetRun
        | TestRailToolKind::GetRuns
        | TestRailToolKind::GetResultsForRun
        | TestRailToolKind::GetResultsForCase
        | TestRailToolKind::GetResults => run_properties(kind),
        _ => effect_properties(kind),
    };
    json!({
        "type":"object",
        "properties":properties,
        "required":required,
        "additionalProperties":false
    })
}

fn read_properties(kind: TestRailToolKind) -> (Value, &'static [&'static str]) {
    match kind {
        TestRailToolKind::GetCase => (json!({"testcase_id":id("Testcase id")}), &["testcase_id"]),
        TestRailToolKind::GetCases => (
            json!({
                "project_id":id("Project id"),
                "output_format":output_format(),
                "keys":{
                    "type":["array","null"], "items":{"type":"string"}, "maxItems":128,
                    "default":["title","id"],
                    "description":"A list of case field keys to include in the data output. If None, defaults to ['title', 'id']."
                },
                "suite_id":optional_id("[Optional] Suite id for test cases extraction in case project is in multiple suite mode (setting 3)")
            }),
            &["project_id"],
        ),
        TestRailToolKind::GetCasesByFilter => (
            json!({
                "project_id":id("Project id"),
                "json_case_arguments":{
                    "type":["string","object"], "maxLength":MAX_JSON_BYTES,
                    "description":"JSON (as a string or dictionary) of the test case arguments used to filter test cases. Supported args: suite_id, created_after, created_before, created_by, filter (title substring), limit, milestone_id, offset, priority_id, refs, section_id, template_id, type_id, updated_after, updated_before, updated_by."
                },
                "output_format":{
                    "type":"string", "maxLength":64,
                    "description":"Desired output format. Supported values: 'json', 'csv', 'markdown'. Defaults to 'json'."
                },
                "keys":{
                    "type":["array","null"], "items":{"type":"string"}, "maxItems":128,
                    "default":null,
                    "description":"A list of case field keys to include in the data output"
                }
            }),
            &["project_id", "json_case_arguments", "output_format"],
        ),
        TestRailToolKind::GetSuites => (
            json!({"project_id":id("Project id"), "output_format":output_format()}),
            &["project_id"],
        ),
        _ => (
            json!({
                "project_id":id("Project id"),
                "suite_id":optional_id("[Optional] Suite id for sections extraction in case project is in multiple suite mode (setting 2 or 3). If omitted in multi-suite mode, sections from all suites are returned."),
                "output_format":output_format()
            }),
            &["project_id"],
        ),
    }
}

fn run_properties(kind: TestRailToolKind) -> (Value, &'static [&'static str]) {
    match kind {
        TestRailToolKind::GetRun => (
            json!({"run_id":id("Test run id"), "output_format":output_format()}),
            &["run_id"],
        ),
        TestRailToolKind::GetRuns => (
            json!({
                "project_id":id("Project id. Only returns test runs that are NOT part of a test plan; use a test-plan tool for plan-bound runs."),
                "run_filter":filter("Optional filters for the test runs, as a JSON string or dict. Supported keys: created_after, created_before (UNIX timestamps), created_by, is_completed (bool or 0/1), limit, offset, milestone_id, refs_filter, suite_id."),
                "output_format":output_format()
            }),
            &["project_id"],
        ),
        TestRailToolKind::GetResultsForRun => (
            json!({
                "run_id":id("Test run id"),
                "result_filter":filter(RESULT_FILTER),
                "output_format":output_format()
            }),
            &["run_id"],
        ),
        TestRailToolKind::GetResultsForCase => (
            json!({
                "run_id":id("Test run id"),
                "case_id":id("Test case id"),
                "result_filter":filter(RESULT_FILTER),
                "output_format":output_format()
            }),
            &["run_id", "case_id"],
        ),
        _ => (
            json!({
                "test_id":id("Test id (a 'test' is an instance of a case within a run)"),
                "result_filter":filter(RESULT_FILTER),
                "output_format":output_format()
            }),
            &["test_id"],
        ),
    }
}

fn effect_properties(kind: TestRailToolKind) -> (Value, &'static [&'static str]) {
    let case_properties = json!({
        "type":"string", "default":"{}", "maxLength":MAX_JSON_BYTES,
        "description":"JSON string of test case properties, e.g. {\"template_id\": 1, \"priority_id\": 2, \"custom_preconds\": \"...\", \"custom_steps\": \"...\", \"custom_expected\": \"...\"} or {\"template_id\": 2, \"custom_steps_separated\": [{\"content\": \"Step 1\", \"expected\": \"Result 1\"}]}. Custom fields use their system name prefixed with 'custom_'."
    });
    match kind {
        TestRailToolKind::AddCase => (
            json!({
                "section_id":id("Section id"),
                "title":{"type":"string","minLength":1,"maxLength":MAX_TITLE_BYTES,"description":"Title"},
                "case_properties":case_properties
            }),
            &["section_id", "title"],
        ),
        TestRailToolKind::AddCases => (
            json!({
                "add_test_cases_data":{
                    "type":"string", "minLength":2, "maxLength":MAX_JSON_BYTES,
                    "description":"Json string with array of test cases to create in format [{section_id: str, title: str, case_properties: obj}, ...] where section_id and title are required and case_properties (default {}) are the test case properties."
                }
            }),
            &["add_test_cases_data"],
        ),
        TestRailToolKind::UpdateCase => (
            json!({"case_id":id("Case ID"), "case_properties":case_properties}),
            &["case_id"],
        ),
        TestRailToolKind::DeleteCase => (
            json!({
                "case_id":id("The ID of the test case to delete"),
                "soft_delete":{
                    "type":"boolean", "default":true,
                    "description":"If True, marks the case as deleted (soft delete). If False, permanently deletes the case. Default is True."
                }
            }),
            &["case_id"],
        ),
        TestRailToolKind::AddSection => (
            json!({
                "project_id":id("Project id the section belongs to"),
                "name":{"type":"string","minLength":1,"maxLength":MAX_TITLE_BYTES,"description":"Name of the new section"},
                "section_properties":{
                    "type":["string","object","null"], "default":"{}",
                    "description":"Section properties as a JSON string or dict. Supported keys: description (str), suite_id (int; ignored in single-suite mode, required otherwise), parent_id (int; the parent section)."
                }
            }),
            &["project_id", "name"],
        ),
        _ => (
            json!({
                "section_id":id("The ID of the section to delete"),
                "soft_delete":{
                    "type":"boolean", "default":false,
                    "description":"If True, performs a TestRail 'soft' dry run: returns the data that would be affected WITHOUT deleting anything. If False (default), permanently deletes the section and all of its test cases, tests and results (this cannot be undone)."
                }
            }),
            &["section_id"],
        ),
    }
}
