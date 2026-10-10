use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::families::python_repr::{repr, repr_str, str_of};
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{
    Params, TestRailApi, TestRailClient, TestRailClientError, TestRailClientErrorCode,
    invalid_response, resource_exhausted,
};
use super::config::{TestRailConfigError, TestRailConfigErrorCode, TestRailToolkitConfig};
use super::format::{Markup, Record, invalid_format, project_records, record_of, to_markup};
use super::schema::{TestRailToolKind, schema};

pub(super) const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_DESCRIPTION_CHARS: usize = 1_000;
const MAX_BULK_PAGES: u64 = 40;
const MAX_CASES_PER_CALL: usize = 100;
const MAX_KEYS: usize = 128;
const MAX_FILTER_MEMBERS: usize = 64;
/// `testrail_api`'s bulk page size and offset step.
const BULK_LIMIT: u64 = 250;
/// `get_cases`' supported output keys; others are reported as invalid.
const SUPPORTED_KEYS: [&str; 26] = [
    "id",
    "title",
    "section_id",
    "template_id",
    "type_id",
    "priority_id",
    "milestone_id",
    "refs",
    "created_by",
    "created_on",
    "updated_by",
    "updated_on",
    "estimate",
    "estimate_forecast",
    "suite_id",
    "display_order",
    "is_deleted",
    "case_assignedto_id",
    "custom_automation_type",
    "custom_preconds",
    "custom_steps",
    "custom_testrail_bdd_scenario",
    "custom_expected",
    "custom_steps_separated",
    "custom_mission",
    "custom_goals",
];
const SUITE_FIELDS: [&str; 9] = [
    "id",
    "name",
    "description",
    "project_id",
    "is_baseline",
    "completed_on",
    "url",
    "is_master",
    "is_completed",
];
const SECTION_FIELDS: [&str; 7] = [
    "id",
    "suite_id",
    "name",
    "description",
    "parent_id",
    "display_order",
    "depth",
];
const RUN_FIELDS: [&str; 20] = [
    "id",
    "suite_id",
    "name",
    "description",
    "milestone_id",
    "assignedto_id",
    "include_all",
    "is_completed",
    "completed_on",
    "passed_count",
    "blocked_count",
    "untested_count",
    "retest_count",
    "failed_count",
    "project_id",
    "plan_id",
    "created_on",
    "created_by",
    "refs",
    "url",
];
const RESULT_FIELDS: [&str; 11] = [
    "id",
    "test_id",
    "status_id",
    "comment",
    "version",
    "elapsed",
    "defects",
    "assignedto_id",
    "created_by",
    "created_on",
    "attachment_ids",
];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum TestRailToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for the `TestRail` family.
pub(crate) struct TestRailToolsetError {
    code: TestRailToolsetErrorCode,
}

impl TestRailToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> TestRailToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for TestRailToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TestRailToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for TestRailToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            TestRailToolsetErrorCode::InvalidConfiguration => {
                "the TestRail toolkit configuration is invalid"
            }
            TestRailToolsetErrorCode::ResourceExhausted => {
                "the TestRail toolkit configuration exceeds its approved limit"
            }
            TestRailToolsetErrorCode::UnsupportedSelection => {
                "the selected TestRail tool profile is not supported"
            }
            TestRailToolsetErrorCode::Client => "the TestRail client could not be created",
            TestRailToolsetErrorCode::InvalidDefinition => {
                "the TestRail ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for TestRailToolsetError {}

impl From<TestRailConfigError> for TestRailToolsetError {
    fn from(source: TestRailConfigError) -> Self {
        Self {
            code: match source.code() {
                TestRailConfigErrorCode::InvalidConfiguration => {
                    TestRailToolsetErrorCode::InvalidConfiguration
                }
                TestRailConfigErrorCode::ResourceExhausted => {
                    TestRailToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<TestRailClientError> for TestRailToolsetError {
    fn from(_: TestRailClientError) -> Self {
        Self {
            code: TestRailToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for TestRailToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: TestRailToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the sixteen served `TestRail` tools.
///
/// `add_file_to_case` (it reads raw bytes from artifact storage, which this
/// runtime's artifact authority does not expose) and the six index tools are
/// not served. A selection keeps the served tools it names; one that names
/// none of them is `UnsupportedSelection`, which skips the toolkit.
pub(crate) fn build_testrail_toolset(
    toolkit_name: &str,
    config: TestRailToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, TestRailToolsetError> {
    let selected = served_selection(config.selected_tools())?;
    let instance = config.instance().to_owned();
    let client: Arc<dyn TestRailApi> = Arc::new(TestRailClient::new(config)?);
    build_with_api(toolkit_name, &instance, &selected, policy, &client)
}

fn served_selection(selected: &[Box<str>]) -> Result<Vec<String>, TestRailToolsetError> {
    let served = selected
        .iter()
        .filter(|name| {
            TestRailToolKind::ALL
                .iter()
                .any(|kind| kind.name() == name.as_ref())
        })
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    if !selected.is_empty() && served.is_empty() {
        return Err(TestRailToolsetError {
            code: TestRailToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(served)
}

fn build_with_api(
    toolkit_name: &str,
    instance: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn TestRailApi>,
) -> Result<BasicToolset, TestRailToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(TestRailToolKind::ALL.len());
    for kind in TestRailToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(TestRailTool::new(
                kind,
                toolkit_name,
                instance,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "testrail", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    instance: &str,
    selected: &[&str],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn TestRailApi>,
) -> Result<BasicToolset, TestRailToolsetError> {
    let selected = served_selection(
        &selected
            .iter()
            .map(|name| (*name).into())
            .collect::<Vec<Box<str>>>(),
    )?;
    build_with_api(toolkit_name, instance, &selected, policy, client)
}

struct TestRailTool {
    kind: TestRailToolKind,
    client: Arc<dyn TestRailApi>,
    description: Box<str>,
}

impl TestRailTool {
    fn new(
        kind: TestRailToolKind,
        toolkit_name: &str,
        instance: &str,
        client: Arc<dyn TestRailApi>,
    ) -> Self {
        let description = format!(
            "Toolkit: {toolkit_name}\n{}\nTestrail instance: {instance}",
            kind.description()
        );
        Self {
            kind,
            client,
            description: description
                .chars()
                .take(MAX_DESCRIPTION_CHARS)
                .collect::<String>()
                .into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for TestRailTool {
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
        Some(schema(self.kind))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let size = serde_json::to_vec(&arguments)
            .map_err(|_| invalid_arguments())?
            .len();
        if size > MAX_ARGUMENT_BYTES {
            return Err(invalid_arguments());
        }
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        let allowed = schema(self.kind);
        let allowed = allowed["properties"]
            .as_object()
            .ok_or_else(invalid_arguments)?;
        if arguments.keys().any(|key| !allowed.contains_key(key)) {
            return Err(invalid_arguments());
        }
        let operations = Operations {
            client: self.client.as_ref(),
        };
        let output = match self.kind {
            TestRailToolKind::GetCase
            | TestRailToolKind::GetCases
            | TestRailToolKind::GetCasesByFilter => {
                operations.case_read(self.kind, arguments).await?
            }
            TestRailToolKind::AddCase
            | TestRailToolKind::AddCases
            | TestRailToolKind::UpdateCase
            | TestRailToolKind::DeleteCase
            | TestRailToolKind::AddSection
            | TestRailToolKind::DeleteSection => operations.effect(self.kind, arguments).await?,
            TestRailToolKind::GetSuites | TestRailToolKind::GetSections => {
                operations.structure_read(self.kind, arguments).await?
            }
            _ => operations.run_read(self.kind, arguments).await?,
        };
        bounded(output)
    }
}

fn bounded(output: Value) -> Result<Value, AdkError> {
    let size = match &output {
        Value::String(text) => text.len(),
        other => serde_json::to_vec(other)
            .map_err(|_| invalid_response().into_adk())?
            .len(),
    };
    if size > MAX_OUTPUT_BYTES {
        return Err(resource_exhausted(false).into_adk());
    }
    Ok(output)
}

struct Operations<'a> {
    client: &'a dyn TestRailApi,
}

impl Operations<'_> {
    async fn get(&self, endpoint: &str, params: &Params) -> Result<Value, AdkError> {
        self.client
            .get(endpoint, params)
            .await
            .map_err(TestRailClientError::into_adk)
    }

    async fn post(&self, endpoint: &str, params: &Params, body: &Value) -> Result<Value, AdkError> {
        self.client
            .post(endpoint, params, body)
            .await
            .map_err(TestRailClientError::into_adk)
    }

    /// `_is_suite_id_required`: suite mode 2 (baselines) or 3 (multiple).
    async fn suite_required(&self, project: u64) -> Result<bool, AdkError> {
        let project = self
            .get(&format!("get_project/{project}"), &Vec::new())
            .await?;
        let mode = project
            .as_object()
            .ok_or_else(|| invalid_response().into_adk())?
            .get("suite_mode")
            .and_then(Value::as_u64)
            .unwrap_or(1);
        Ok(matches!(mode, 2 | 3))
    }

    /// `_get_raw_suites`: a bare list, or the list under `suites`.
    async fn raw_suites(&self, project: u64) -> Result<Vec<Value>, AdkError> {
        let response = self
            .get(&format!("get_suites/{project}"), &Vec::new())
            .await?;
        Ok(member_list(response, "suites"))
    }

    async fn suite_ids(&self, project: u64) -> Result<Vec<String>, AdkError> {
        Ok(self
            .raw_suites(project)
            .await?
            .iter()
            .filter_map(|suite| suite.get("id"))
            .map(str_of)
            .collect())
    }

    /// `_fetch_cases_with_suite_handling`: one page of `get_cases` per suite
    /// in multi-suite projects, one page for the project otherwise. A suite
    /// (or single-suite project) whose read fails is skipped, as the SDK
    /// skips a `StatusCodeError`.
    async fn cases(
        &self,
        project: u64,
        suite: Option<u64>,
        params: &Params,
    ) -> Result<Vec<Value>, AdkError> {
        let endpoint = format!("get_cases/{project}");
        let with_suite = |suite: &str| {
            let mut all = vec![("suite_id".to_owned(), suite.to_owned())];
            all.extend(params.iter().cloned());
            all
        };
        if !self.suite_required(project).await? {
            return Ok(match self.client.get(&endpoint, params).await {
                Ok(response) => member_list(response, "cases"),
                Err(error) if skippable(&error) => Vec::new(),
                Err(error) => return Err(error.into_adk()),
            });
        }
        if let Some(suite) = suite {
            let response = self.get(&endpoint, &with_suite(&suite.to_string())).await?;
            return Ok(member_list(response, "cases"));
        }
        let mut cases = Vec::new();
        for suite in self.suite_ids(project).await? {
            match self.client.get(&endpoint, &with_suite(&suite)).await {
                Ok(response) => cases.extend(member_list(response, "cases")),
                Err(error) if skippable(&error) => {}
                Err(error) => return Err(error.into_adk()),
            }
        }
        Ok(cases)
    }

    async fn case_read(
        &self,
        kind: TestRailToolKind,
        arguments: &Map<String, Value>,
    ) -> Result<Value, AdkError> {
        match kind {
            TestRailToolKind::GetCase => {
                let case = required_id(arguments, "testcase_id")?;
                let case = self.get(&format!("get_case/{case}"), &Vec::new()).await?;
                Ok(Value::String(format!(
                    "Extracted test case:\n{}",
                    repr(&case)
                )))
            }
            TestRailToolKind::GetCases => self.get_cases(arguments).await,
            _ => self.get_cases_by_filter(arguments).await,
        }
    }

    async fn get_cases(&self, arguments: &Map<String, Value>) -> Result<Value, AdkError> {
        let project = required_id(arguments, "project_id")?;
        let format = output_format(arguments, true)?;
        let keys = match arguments.get("keys") {
            None | Some(Value::Null) => vec!["title".to_owned(), "id".to_owned()],
            Some(_) => string_list(arguments, "keys")?.unwrap_or_default(),
        };
        let suite = optional_id(arguments, "suite_id")?;
        let cases = self.cases(project, suite, &Vec::new()).await?;
        let records = cases
            .iter()
            .map(|case| {
                keys.iter()
                    .map(|key| {
                        (
                            key.clone(),
                            case.get(key)
                                .cloned()
                                .unwrap_or_else(|| Value::String("N/A".to_owned())),
                        )
                    })
                    .collect::<Record>()
            })
            .collect::<Vec<_>>();
        Ok(Value::String(with_invalid_keys(
            markup(&records, &format),
            &keys,
        )))
    }

    async fn get_cases_by_filter(&self, arguments: &Map<String, Value>) -> Result<Value, AdkError> {
        let project = required_id(arguments, "project_id")?;
        let format = output_format(arguments, false)?;
        let keys = string_list(arguments, "keys")?;
        if keys.as_ref().is_some_and(Vec::is_empty) {
            // The SDK references an unbound `invalid_keys` for `keys=[]`.
            return Err(invalid_arguments());
        }
        let mut filter = match filter_object(arguments, "json_case_arguments", true)? {
            Ok(filter) => filter,
            Err(message) => {
                return Ok(Value::String(
                    message.replace("{name}", "json_case_arguments"),
                ));
            }
        };
        let suite = match filter.remove("suite_id") {
            None | Some(Value::Null | Value::Bool(false)) => None,
            Some(Value::Number(number)) if number.as_u64() == Some(0) => None,
            Some(Value::String(text)) if text.is_empty() => None,
            Some(value) => match id_value(&value) {
                Some(suite) => Some(suite),
                None => {
                    return Ok(Value::String(format!(
                        "Invalid parameter for json_case_arguments: invalid literal for int() with base 10: {}",
                        repr_str(&str_of(&value))
                    )));
                }
            },
        };
        let params = query_params(&filter)?;
        let cases = self.cases(project, suite, &params).await?;
        let Some(keys) = keys else {
            let records = cases.iter().filter_map(record_of).collect::<Vec<_>>();
            return Ok(Value::String(markup(&records, &format)));
        };
        let records = cases
            .iter()
            .filter_map(|case| {
                let record = keys
                    .iter()
                    .filter_map(|key| case.get(key).map(|value| (key.clone(), value.clone())))
                    .collect::<Record>();
                (!record.is_empty()).then_some(record)
            })
            .collect::<Vec<_>>();
        Ok(Value::String(with_invalid_keys(
            markup(&records, &format),
            &keys,
        )))
    }

    async fn structure_read(
        &self,
        kind: TestRailToolKind,
        arguments: &Map<String, Value>,
    ) -> Result<Value, AdkError> {
        let project = required_id(arguments, "project_id")?;
        let format = output_format(arguments, true)?;
        if kind == TestRailToolKind::GetSuites {
            let suites = self.raw_suites(project).await?;
            if suites.is_empty() {
                return Ok(Value::String(
                    "No test suites found for the specified project.".to_owned(),
                ));
            }
            let records = project_records(&suites, &SUITE_FIELDS, "suite", &["completed_on"]);
            return Ok(Value::String(markup(&records, &format)));
        }
        let suite = optional_id(arguments, "suite_id")?;
        let sections = self.sections(project, suite).await?;
        if sections.is_empty() {
            return Ok(Value::String(
                "No sections found for the specified project.".to_owned(),
            ));
        }
        let records = project_records(&sections, &SECTION_FIELDS, "section", &[]);
        Ok(Value::String(markup(&records, &format)))
    }

    /// `_fetch_sections_with_suite_handling`, first page per request.
    async fn sections(&self, project: u64, suite: Option<u64>) -> Result<Vec<Value>, AdkError> {
        let endpoint = format!("get_sections/{project}");
        let page = |suite: Option<&str>| {
            let mut params = vec![
                ("limit".to_owned(), BULK_LIMIT.to_string()),
                ("offset".to_owned(), "0".to_owned()),
            ];
            if let Some(suite) = suite {
                params.push(("suite_id".to_owned(), suite.to_owned()));
            }
            params
        };
        if !self.suite_required(project).await? {
            return Ok(match self.client.get(&endpoint, &page(None)).await {
                Ok(response) => member_list(response, "sections"),
                Err(error) if skippable(&error) => Vec::new(),
                Err(error) => return Err(error.into_adk()),
            });
        }
        if let Some(suite) = suite {
            let response = self.get(&endpoint, &page(Some(&suite.to_string()))).await?;
            return Ok(member_list(response, "sections"));
        }
        let mut sections = Vec::new();
        for suite in self.suite_ids(project).await? {
            match self.client.get(&endpoint, &page(Some(&suite))).await {
                Ok(response) => sections.extend(member_list(response, "sections")),
                Err(error) if skippable(&error) => {}
                Err(error) => return Err(error.into_adk()),
            }
        }
        Ok(sections)
    }

    async fn run_read(
        &self,
        kind: TestRailToolKind,
        arguments: &Map<String, Value>,
    ) -> Result<Value, AdkError> {
        let format = output_format(arguments, true)?;
        match kind {
            TestRailToolKind::GetRun => {
                let Some(run) = numeric(arguments, "run_id")? else {
                    return Ok(not_numeric("run_id", arguments));
                };
                let run = self.get(&format!("get_run/{run}"), &Vec::new()).await?;
                let records =
                    project_records(&[run], &RUN_FIELDS, "run", &["created_on", "completed_on"]);
                Ok(Value::String(markup(&records, &format)))
            }
            TestRailToolKind::GetRuns => {
                let Some(project) = numeric(arguments, "project_id")? else {
                    return Ok(not_numeric("project_id", arguments));
                };
                let mut filter = match filter_object(arguments, "run_filter", false)? {
                    Ok(filter) => filter,
                    Err(message) => {
                        return Ok(Value::String(message.replace("{name}", "run_filter")));
                    }
                };
                if let Some(flag) = filter.get_mut("is_completed") {
                    *flag = Value::from(u8::from(truthy_flag(flag)));
                }
                let response = self
                    .get(&format!("get_runs/{project}"), &query_params(&filter)?)
                    .await?;
                let runs = member_list(response, "runs");
                let records =
                    project_records(&runs, &RUN_FIELDS, "run", &["created_on", "completed_on"]);
                Ok(Value::String(markup(&records, &format)))
            }
            _ => self.results(kind, arguments, &format).await,
        }
    }

    /// `_read_results` over the `*_bulk` endpoints: `limit=250` pages from
    /// offset 0 until a page reports `size < 250`.
    async fn results(
        &self,
        kind: TestRailToolKind,
        arguments: &Map<String, Value>,
        format: &str,
    ) -> Result<Value, AdkError> {
        let endpoint = match kind {
            TestRailToolKind::GetResultsForRun => {
                let Some(run) = numeric(arguments, "run_id")? else {
                    return Ok(not_numeric("run_id", arguments));
                };
                format!("get_results_for_run/{run}")
            }
            TestRailToolKind::GetResultsForCase => {
                let (Some(run), Some(case)) = (
                    numeric(arguments, "run_id")?,
                    numeric(arguments, "case_id")?,
                ) else {
                    return Ok(Value::String(format!(
                        "run_id and case_id must be numeric, got: run_id={}, case_id={}",
                        repr_arg(arguments, "run_id"),
                        repr_arg(arguments, "case_id")
                    )));
                };
                format!("get_results_for_case/{run}/{case}")
            }
            _ => {
                let Some(test) = numeric(arguments, "test_id")? else {
                    return Ok(not_numeric("test_id", arguments));
                };
                format!("get_results/{test}")
            }
        };
        let mut filter = match filter_object(arguments, "result_filter", false)? {
            Ok(filter) => filter,
            Err(message) => return Ok(Value::String(message.replace("{name}", "result_filter"))),
        };
        filter.remove("limit");
        filter.remove("offset");
        let filters = query_params(&filter)?;
        let mut results = Vec::new();
        for page in 0..=MAX_BULK_PAGES {
            if page == MAX_BULK_PAGES {
                return Err(resource_exhausted(false).into_adk());
            }
            let mut params = vec![
                ("offset".to_owned(), (page * BULK_LIMIT).to_string()),
                ("limit".to_owned(), BULK_LIMIT.to_string()),
            ];
            params.extend(filters.iter().cloned());
            let response = self.get(&endpoint, &params).await?;
            let object = response
                .as_object()
                .ok_or_else(|| invalid_response().into_adk())?;
            let size = object
                .get("size")
                .and_then(Value::as_u64)
                .ok_or_else(|| invalid_response().into_adk())?;
            results.extend(
                object
                    .get("results")
                    .and_then(Value::as_array)
                    .cloned()
                    .unwrap_or_default(),
            );
            if size < BULK_LIMIT {
                break;
            }
        }
        let records = project_records(&results, &RESULT_FIELDS, "result", &["created_on"]);
        Ok(Value::String(markup(&records, format)))
    }

    async fn effect(
        &self,
        kind: TestRailToolKind,
        arguments: &Map<String, Value>,
    ) -> Result<Value, AdkError> {
        match kind {
            TestRailToolKind::AddCase => {
                let section = required_id(arguments, "section_id")?;
                let title = required_text(arguments, "title")?;
                let properties = json_properties(arguments.get("case_properties"))?;
                self.add_case(section, title, properties).await
            }
            TestRailToolKind::AddCases => self.add_cases(arguments).await,
            TestRailToolKind::UpdateCase => {
                let case = required_id(arguments, "case_id")?;
                let properties = json_properties(arguments.get("case_properties"))?;
                let updated = self
                    .post(
                        &format!("update_case/{case}"),
                        &Vec::new(),
                        &Value::Object(properties),
                    )
                    .await?;
                Ok(Value::String(format!(
                    "Test case #{case} has been updated at '{}'",
                    member_text(&updated, "updated_on")
                )))
            }
            TestRailToolKind::DeleteCase => {
                let case = required_id(arguments, "case_id")?;
                let soft = optional_bool(arguments, "soft_delete", true)?;
                self.post(
                    &format!("delete_case/{case}"),
                    &vec![("soft".to_owned(), u8::from(soft).to_string())],
                    &json!({}),
                )
                .await?;
                let outcome = if soft {
                    "soft deleted (marked as deleted)"
                } else {
                    "permanently deleted"
                };
                Ok(Value::String(format!(
                    "Test case #{case} has been {outcome} successfully."
                )))
            }
            TestRailToolKind::AddSection => {
                let project = required_id(arguments, "project_id")?;
                let name = required_text(arguments, "name")?;
                let properties = json_properties(arguments.get("section_properties"))?;
                if properties.contains_key("name") {
                    return Err(invalid_arguments());
                }
                let mut body = Map::new();
                body.insert("name".to_owned(), Value::String(name.to_owned()));
                body.extend(properties);
                let created = self
                    .post(
                        &format!("add_section/{project}"),
                        &Vec::new(),
                        &Value::Object(body),
                    )
                    .await?;
                Ok(Value::String(format!(
                    "New section has been created: id - {} - '{}'",
                    member_text(&created, "id"),
                    member_text(&created, "name")
                )))
            }
            _ => {
                let section = required_id(arguments, "section_id")?;
                let soft = optional_bool(arguments, "soft_delete", false)?;
                self.post(
                    &format!("delete_section/{section}"),
                    &vec![("soft".to_owned(), u8::from(soft).to_string())],
                    &json!({}),
                )
                .await?;
                let outcome = if soft {
                    "previewed for deletion (soft dry run, nothing removed)"
                } else {
                    "permanently deleted"
                };
                Ok(Value::String(format!(
                    "Section #{section} has been {outcome} successfully."
                )))
            }
        }
    }

    async fn add_cases(&self, arguments: &Map<String, Value>) -> Result<Value, AdkError> {
        let source = required_text(arguments, "add_test_cases_data")?;
        let cases = serde_json::from_str::<Value>(source)
            .ok()
            .and_then(|value| value.as_array().cloned())
            .filter(|cases| cases.len() <= MAX_CASES_PER_CALL)
            .ok_or_else(invalid_arguments)?;
        let mut planned = Vec::with_capacity(cases.len());
        for case in &cases {
            let section = case
                .get("section_id")
                .and_then(id_value)
                .ok_or_else(invalid_arguments)?;
            let title = case
                .get("title")
                .and_then(Value::as_str)
                .filter(|title| !title.is_empty())
                .ok_or_else(invalid_arguments)?;
            let properties = match case.get("case_properties") {
                None | Some(Value::Null) => Map::new(),
                Some(Value::Object(properties)) => properties.clone(),
                Some(_) => return Err(invalid_arguments()),
            };
            planned.push((section, title, properties));
        }
        let mut messages = Vec::with_capacity(planned.len());
        for (section, title, properties) in planned {
            messages.push(self.add_case(section, title, properties).await?);
        }
        Ok(Value::Array(messages))
    }

    async fn add_case(
        &self,
        section: u64,
        title: &str,
        properties: Map<String, Value>,
    ) -> Result<Value, AdkError> {
        if properties.contains_key("title") {
            // `dict(title=title, **kwargs)` refuses a second title.
            return Err(invalid_arguments());
        }
        let mut body = Map::new();
        body.insert("title".to_owned(), Value::String(title.to_owned()));
        body.extend(properties);
        let created = self
            .post(
                &format!("add_case/{section}"),
                &Vec::new(),
                &Value::Object(body),
            )
            .await?;
        Ok(Value::String(format!(
            "New test case has been created: id - {} at '{}'",
            member_text(&created, "id"),
            member_text(&created, "created_on")
        )))
    }
}

/// Whether a per-suite read failure is one the SDK skips (an HTTP status),
/// rather than a bound this runtime enforces.
fn skippable(error: &TestRailClientError) -> bool {
    !matches!(
        error.code(),
        TestRailClientErrorCode::ResourceExhausted | TestRailClientErrorCode::UnknownOutcome
    )
}

/// `response[key]` when it is a dict holding the list, the response itself
/// when it is a list, else nothing.
fn member_list(response: Value, key: &str) -> Vec<Value> {
    match response {
        Value::Array(items) => items,
        Value::Object(mut object) => match object.remove(key) {
            Some(Value::Array(items)) => items,
            _ => Vec::new(),
        },
        _ => Vec::new(),
    }
}

fn member_text(value: &Value, key: &str) -> String {
    value.get(key).map_or_else(|| "None".to_owned(), str_of)
}

fn markup(records: &[Record], format: &str) -> String {
    Markup::parse(format).map_or_else(
        || invalid_format(format),
        |markup| to_markup(records, markup),
    )
}

fn with_invalid_keys(result: String, keys: &[String]) -> String {
    let invalid = keys
        .iter()
        .filter(|key| !SUPPORTED_KEYS.contains(&key.as_str()))
        .map(|key| repr_str(key))
        .collect::<Vec<_>>();
    if invalid.is_empty() {
        result
    } else {
        format!("{result}\n\nInvalid keys: [{}]", invalid.join(", "))
    }
}

/// A JSON-object filter argument given as an object or a JSON string. The
/// inner `Err` is the SDK's returned (not raised) message, with `{name}` for
/// the argument name.
fn filter_object(
    arguments: &Map<String, Value>,
    name: &str,
    required: bool,
) -> Result<Result<Map<String, Value>, String>, AdkError> {
    let parsed = match arguments.get(name) {
        None | Some(Value::Null) if !required => return Ok(Ok(Map::new())),
        Some(Value::Object(object)) => Value::Object(object.clone()),
        Some(Value::String(text)) => match serde_json::from_str::<Value>(text) {
            Ok(value) => value,
            Err(error) => {
                return Ok(Err(format!(
                    "Invalid parameter for {{name}}: {}",
                    json_error_text(&error)
                )));
            }
        },
        _ => return Err(invalid_arguments()),
    };
    match parsed {
        Value::Object(object) if object.len() <= MAX_FILTER_MEMBERS => Ok(Ok(object)),
        Value::Object(_) => Err(invalid_arguments()),
        // `get_cases_by_filter` passes the non-dict to `params.pop`, which
        // raises; the others return this sentence.
        _ if required => Err(invalid_arguments()),
        other => Ok(Err(format!(
            "{{name}} must be a JSON object of filters, got {}.",
            python_type(&other)
        ))),
    }
}

/// The first line of `json.JSONDecodeError`'s message, roughly.
fn json_error_text(error: &serde_json::Error) -> String {
    format!(
        "Expecting value: line {} column {}",
        error.line(),
        error.column()
    )
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

/// `testrail_api`'s GET converter plus `requests`' encoding: `None` is
/// dropped, a list becomes `1,2,3`, a bool `0`/`1`.
fn query_params(filter: &Map<String, Value>) -> Result<Params, AdkError> {
    let mut params = Vec::with_capacity(filter.len());
    for (key, value) in filter {
        if key.is_empty()
            || key.len() > 64
            || !key
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
        {
            return Err(invalid_arguments());
        }
        let text = match value {
            Value::Null => continue,
            Value::Bool(flag) => u8::from(*flag).to_string(),
            Value::Array(items) => items
                .iter()
                .map(|item| match item {
                    Value::Bool(true) => "True".to_owned(),
                    Value::Bool(false) => "False".to_owned(),
                    other => str_of(other),
                })
                .collect::<Vec<_>>()
                .join(","),
            Value::Object(_) => return Err(invalid_arguments()),
            other => str_of(other),
        };
        if text.chars().any(char::is_control) {
            return Err(invalid_arguments());
        }
        params.push((key.clone(), text));
    }
    Ok(params)
}

/// `_coerce_bool_flag`.
fn truthy_flag(value: &Value) -> bool {
    match value {
        Value::String(text) => matches!(text.trim().to_lowercase().as_str(), "1" | "true" | "yes"),
        Value::Bool(flag) => *flag,
        Value::Number(number) => number.as_f64().is_some_and(|number| number != 0.0),
        Value::Null => false,
        Value::Array(items) => !items.is_empty(),
        Value::Object(object) => !object.is_empty(),
    }
}

/// `json.loads(case_properties)` for a string, the object itself otherwise;
/// it must be a JSON object to be spread into the request.
fn json_properties(value: Option<&Value>) -> Result<Map<String, Value>, AdkError> {
    let parsed = match value {
        None | Some(Value::Null) => return Ok(Map::new()),
        Some(Value::String(text)) => {
            serde_json::from_str::<Value>(text).map_err(|_| invalid_arguments())?
        }
        Some(Value::Object(object)) => Value::Object(object.clone()),
        Some(_) => return Err(invalid_arguments()),
    };
    match parsed {
        Value::Object(object)
            if object.len() <= 256
                && object.keys().all(|key| {
                    !key.is_empty()
                        && key.len() <= 128
                        && key
                            .bytes()
                            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
                }) =>
        {
            Ok(object)
        }
        _ => Err(invalid_arguments()),
    }
}

fn output_format(arguments: &Map<String, Value>, defaulted: bool) -> Result<String, AdkError> {
    match arguments.get("output_format") {
        None if defaulted => Ok("json".to_owned()),
        Some(Value::String(format)) if format.len() <= 64 && !format.contains(['\r', '\n']) => {
            Ok(format.clone())
        }
        _ => Err(invalid_arguments()),
    }
}

fn string_list(
    arguments: &Map<String, Value>,
    name: &str,
) -> Result<Option<Vec<String>>, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Array(items)) if items.len() <= MAX_KEYS => items
            .iter()
            .map(|item| {
                item.as_str()
                    .filter(|key| key.len() <= 128)
                    .map(ToOwned::to_owned)
                    .ok_or_else(invalid_arguments)
            })
            .collect::<Result<Vec<_>, _>>()
            .map(Some),
        Some(_) => Err(invalid_arguments()),
    }
}

/// A `TestRail` ID: digits (as a JSON string, the SDK's type, or a number).
fn id_value(value: &Value) -> Option<u64> {
    match value {
        Value::Number(number) => number.as_u64(),
        Value::String(text) if !text.trim().is_empty() && text.len() <= 20 => {
            text.trim().parse::<u64>().ok()
        }
        _ => None,
    }
}

fn required_id(arguments: &Map<String, Value>, name: &str) -> Result<u64, AdkError> {
    arguments
        .get(name)
        .and_then(id_value)
        .ok_or_else(invalid_arguments)
}

fn optional_id(arguments: &Map<String, Value>, name: &str) -> Result<Option<u64>, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(text)) if text.is_empty() => Ok(None),
        Some(value) => id_value(value).map(Some).ok_or_else(invalid_arguments),
    }
}

/// `int(value)` for an ID the SDK checks itself; `None` when it is not one.
fn numeric(arguments: &Map<String, Value>, name: &str) -> Result<Option<u64>, AdkError> {
    match arguments.get(name) {
        Some(value @ (Value::String(_) | Value::Number(_))) => Ok(id_value(value)),
        _ => Err(invalid_arguments()),
    }
}

fn repr_arg(arguments: &Map<String, Value>, name: &str) -> String {
    arguments.get(name).map_or_else(|| "None".to_owned(), repr)
}

fn not_numeric(name: &str, arguments: &Map<String, Value>) -> Value {
    Value::String(format!(
        "{name} must be numeric, got: {}",
        repr_arg(arguments, name)
    ))
}

fn required_text<'a>(arguments: &'a Map<String, Value>, name: &str) -> Result<&'a str, AdkError> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .filter(|text| !text.is_empty())
        .ok_or_else(invalid_arguments)
}

fn optional_bool(
    arguments: &Map<String, Value>,
    name: &str,
    default: bool,
) -> Result<bool, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(default),
        Some(Value::Bool(flag)) => Ok(*flag),
        Some(_) => Err(invalid_arguments()),
    }
}

pub(super) fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "testrail.arguments.invalid",
        "the TestRail tool arguments are invalid",
    )
}
