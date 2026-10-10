use std::cmp::Ordering;
use std::collections::BTreeSet;
use std::fmt;
use std::fmt::Write as _;
use std::sync::{Arc, OnceLock};

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use chrono::{DateTime, FixedOffset, NaiveDate, NaiveDateTime};
use regex::Regex;
use reqwest::Method;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{
    CarrierAuth, CarrierBody, CarrierClient, CarrierClientError, CarrierClientErrorCode,
    json_success, list_field, response_shape_failure, status_error,
};
use super::config::{CarrierConfigError, CarrierToolkitConfig};

const GET_TICKET_LIST: &str = "get_ticket_list";
const CREATE_TICKET: &str = "create_ticket";
const GET_REPORTS: &str = "get_reports";
const GET_REPORT_BY_ID: &str = "get_report_by_id";
const ADD_TAG_TO_REPORT: &str = "add_tag_to_report";
const CREATE_EXCEL_REPORT: &str = "create_excel_report";
const GET_TESTS: &str = "get_tests";
const GET_TEST_BY_ID: &str = "get_test_by_id";
const RUN_TEST_BY_ID: &str = "run_test_by_id";
const CREATE_BACKEND_TEST: &str = "create_backend_test";
const GET_UI_REPORTS: &str = "get_ui_reports";
const GET_UI_REPORT_BY_ID: &str = "get_ui_report_by_id";
const GET_UI_TESTS: &str = "get_ui_tests";
const RUN_UI_TEST: &str = "run_ui_test";
const UPDATE_UI_TEST_SCHEDULE: &str = "update_ui_test_schedule";
const CREATE_UI_EXCEL_REPORT: &str = "create_ui_excel_report";
const CREATE_UI_TEST: &str = "create_ui_test";
const CANCEL_UI_TEST: &str = "cancel_ui_test";

/// SDK tools this family does not serve: they download, unzip, merge and
/// re-upload result archives or build `.xlsx` workbooks.
const SDK_ONLY_TOOLS: [&str; 3] = [
    GET_REPORT_BY_ID,
    CREATE_EXCEL_REPORT,
    CREATE_UI_EXCEL_REPORT,
];

const MAX_TEXT_BYTES: usize = 16 * 1_024;
const MAX_DESCRIPTION_TEXT_BYTES: usize = 64 * 1_024;
const MAX_ID_BYTES: usize = 256;
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;
const REPORT_LINK_PREFIX: &str = "https://platform.getcarrier.io/api/v1/artifacts/artifact/default";
const FINAL_UI_STATES: [&str; 3] = ["Canceled", "Finished", "Failed"];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum CarrierToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for the Carrier family.
pub(crate) struct CarrierToolsetError {
    code: CarrierToolsetErrorCode,
}

impl CarrierToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> CarrierToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for CarrierToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("CarrierToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for CarrierToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            CarrierToolsetErrorCode::InvalidConfiguration => {
                "the Carrier toolkit configuration is invalid"
            }
            CarrierToolsetErrorCode::ResourceExhausted => {
                "the Carrier toolkit configuration exceeds its approved limit"
            }
            CarrierToolsetErrorCode::UnsupportedSelection => {
                "the selected Carrier tool profile is not supported"
            }
            CarrierToolsetErrorCode::Client => "the Carrier client could not be created",
            CarrierToolsetErrorCode::InvalidDefinition => {
                "the Carrier ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for CarrierToolsetError {}

impl From<CarrierConfigError> for CarrierToolsetError {
    fn from(source: CarrierConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::CarrierConfigErrorCode::InvalidConfiguration => {
                    CarrierToolsetErrorCode::InvalidConfiguration
                }
                super::config::CarrierConfigErrorCode::ResourceExhausted => {
                    CarrierToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<CarrierClientError> for CarrierToolsetError {
    fn from(_: CarrierClientError) -> Self {
        Self {
            code: CarrierToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for CarrierToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: CarrierToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the fifteen served Carrier tools.
///
/// An empty selection serves all of them. An explicit selection keeps the
/// served names and omits the three SDK-only archive tools with one bounded
/// warning (elitea-main's per-tool capability marks those unavailable); a
/// name the SDK does not declare is refused.
pub(crate) fn build_carrier_toolset(
    toolkit_name: &str,
    config: CarrierToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, CarrierToolsetError> {
    let selected = served_selection(config.selected_tools())?;
    if !config.selected_tools().is_empty() && selected.len() < config.selected_tools().len() {
        tracing::warn!(
            event = "agent_toolkit_tools_skipped",
            reason_code = "unsupported_tool_selection",
            toolkit_type = "carrier",
            toolkit_name,
            selected_count = config.selected_tools().len(),
            materialized_count = selected.len(),
            "Carrier archive tools are not served by this runtime and were omitted"
        );
    }
    let client = Arc::new(CarrierClient::new(config)?);
    build_with_client(toolkit_name, &selected, policy, &client)
}

fn served_selection(selected: &[Box<str>]) -> Result<Vec<String>, CarrierToolsetError> {
    if selected.is_empty() {
        return Ok(Vec::new());
    }
    let mut served = Vec::with_capacity(selected.len());
    for name in selected {
        if CarrierToolKind::from_name(name).is_some() {
            served.push(name.to_string());
        } else if !SDK_ONLY_TOOLS.contains(&name.as_ref()) {
            return Err(CarrierToolsetError {
                code: CarrierToolsetErrorCode::UnsupportedSelection,
            });
        }
    }
    if served.is_empty() {
        return Err(CarrierToolsetError {
            code: CarrierToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(served)
}

fn build_with_client(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<CarrierClient>,
) -> Result<BasicToolset, CarrierToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(CarrierToolKind::ALL.len());
    for kind in CarrierToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(CarrierTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "carrier", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_client(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: CarrierClient,
) -> Result<BasicToolset, CarrierToolsetError> {
    build_with_client(toolkit_name, selected, policy, &Arc::new(client))
}

#[cfg(test)]
pub(in crate::toolkits) fn test_served_selection(
    selected: &[Box<str>],
) -> Result<Vec<String>, CarrierToolsetError> {
    served_selection(selected)
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum CarrierToolKind {
    GetTicketList,
    CreateTicket,
    GetReports,
    AddTagToReport,
    GetTests,
    GetTestById,
    RunTestById,
    CreateBackendTest,
    GetUiReports,
    GetUiReportById,
    GetUiTests,
    RunUiTest,
    UpdateUiTestSchedule,
    CreateUiTest,
    CancelUiTest,
}

impl CarrierToolKind {
    /// The SDK's `tools.__all__` order, without the three archive tools.
    const ALL: [Self; 15] = [
        Self::GetTicketList,
        Self::CreateTicket,
        Self::GetReports,
        Self::AddTagToReport,
        Self::GetTests,
        Self::GetTestById,
        Self::RunTestById,
        Self::CreateBackendTest,
        Self::GetUiReports,
        Self::GetUiReportById,
        Self::GetUiTests,
        Self::RunUiTest,
        Self::UpdateUiTestSchedule,
        Self::CreateUiTest,
        Self::CancelUiTest,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::GetTicketList => GET_TICKET_LIST,
            Self::CreateTicket => CREATE_TICKET,
            Self::GetReports => GET_REPORTS,
            Self::AddTagToReport => ADD_TAG_TO_REPORT,
            Self::GetTests => GET_TESTS,
            Self::GetTestById => GET_TEST_BY_ID,
            Self::RunTestById => RUN_TEST_BY_ID,
            Self::CreateBackendTest => CREATE_BACKEND_TEST,
            Self::GetUiReports => GET_UI_REPORTS,
            Self::GetUiReportById => GET_UI_REPORT_BY_ID,
            Self::GetUiTests => GET_UI_TESTS,
            Self::RunUiTest => RUN_UI_TEST,
            Self::UpdateUiTestSchedule => UPDATE_UI_TEST_SCHEDULE,
            Self::CreateUiTest => CREATE_UI_TEST,
            Self::CancelUiTest => CANCEL_UI_TEST,
        }
    }

    fn from_name(name: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|kind| kind.name() == name)
    }

    /// Tools that can change Carrier state. `run_test_by_id`, `run_ui_test`,
    /// `create_backend_test`, `update_ui_test_schedule` and `cancel_ui_test`
    /// are effects only on their final step, but the model cannot tell which
    /// step a call will reach, so the whole tool is treated as one.
    const fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::GetTicketList
                | Self::GetReports
                | Self::GetTests
                | Self::GetTestById
                | Self::GetUiReports
                | Self::GetUiReportById
                | Self::GetUiTests
        )
    }

    /// The SDK tool descriptions, verbatim.
    const fn sdk_description(self) -> &'static str {
        match self {
            Self::GetTicketList => "Fetch tickets from a specific board.",
            Self::CreateTicket => {
                "Create a new ticket on Carrier with required fields: title, description, severity, type, board_id, start_date, end_date. Optional: external_link, engagement, assignee, tags."
            }
            Self::GetReports => "Get list of reports from the Carrier platform.",
            Self::AddTagToReport => "Add tag to backend report",
            Self::GetTests => "Get list of tests from the Carrier platform.",
            Self::GetTestById => "Get test data from the Carrier platform.",
            Self::RunTestById => "Execute test plan from the Carrier platform.",
            Self::CreateBackendTest => "Create a new backend test plan in the Carrier platform.",
            Self::GetUiReports => {
                "Get list of UI test reports from the Carrier platform. Optionally filter by time range."
            }
            Self::GetUiReportById => "Get UI report data from the Carrier platform.",
            Self::GetUiTests => {
                "Get list of UI tests from the Carrier platform. Optionally filter by name."
            }
            Self::RunUiTest => {
                "Run and execute UI tests from the Carrier platform. Use this tool when user wants to run, execute, or start a UI test. Provide either test ID or test name, or leave empty to see available tests. When no custom parameters are provided, the tool will show default configuration and ask for confirmation. You can override parameters like cpu_quota, memory_quota, cloud_settings, custom_cmd, or loops. "
            }
            Self::UpdateUiTestSchedule => {
                "Update UI test schedule on the Carrier platform. Use this tool when user wants to update, modify, or change a UI test schedule. Provide test_id, schedule_name, and cron_timer, or leave empty to see available tests."
            }
            Self::CreateUiTest => "Create a new UI test in the Carrier platform.",
            Self::CancelUiTest => {
                "Cancel a UI test or show available tests to cancel in the Carrier platform."
            }
        }
    }
}

struct CarrierTool {
    kind: CarrierToolKind,
    client: Arc<CarrierClient>,
    description: Box<str>,
}

impl CarrierTool {
    fn new(kind: CarrierToolKind, toolkit_name: &str, client: Arc<CarrierClient>) -> Self {
        // The Carrier SDK appends the toolkit line, unlike most families.
        let description = format!("{}\nToolkit: {toolkit_name}", kind.sdk_description());
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
impl Tool for CarrierTool {
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
        validate_argument_size(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        let allowed = schema(self.kind)
            .get("properties")
            .and_then(Value::as_object)
            .map(|properties| properties.keys().cloned().collect::<BTreeSet<_>>())
            .unwrap_or_default();
        if arguments.keys().any(|key| !allowed.contains(key)) {
            return Err(invalid_arguments());
        }
        let client = self.client.as_ref();
        let result = match self.kind {
            CarrierToolKind::GetTicketList => get_ticket_list(client, arguments).await,
            CarrierToolKind::CreateTicket => create_ticket(client, arguments).await,
            CarrierToolKind::GetReports => get_reports(client, arguments).await,
            CarrierToolKind::AddTagToReport => add_tag_to_report(client, arguments).await,
            CarrierToolKind::GetTests => get_tests(client).await,
            CarrierToolKind::GetTestById => get_test_by_id(client, arguments).await,
            CarrierToolKind::RunTestById => run_test_by_id(client, arguments).await,
            CarrierToolKind::CreateBackendTest => create_backend_test(client, arguments).await,
            CarrierToolKind::GetUiReports => get_ui_reports(client, arguments).await,
            CarrierToolKind::GetUiReportById => get_ui_report_by_id(client, arguments).await,
            CarrierToolKind::GetUiTests => get_ui_tests(client, arguments).await,
            CarrierToolKind::RunUiTest => run_ui_test(client, arguments).await,
            CarrierToolKind::UpdateUiTestSchedule => {
                update_ui_test_schedule(client, arguments).await
            }
            CarrierToolKind::CreateUiTest => create_ui_test(client, arguments).await,
            CarrierToolKind::CancelUiTest => cancel_ui_test(client, arguments).await,
        };
        result.map_err(ToolFailure::into_adk)
    }
}

/// A failed call: an argument the schema admits but the operation cannot
/// use, or a redacted Carrier failure.
enum ToolFailure {
    Arguments,
    Client(CarrierClientError),
}

impl ToolFailure {
    fn into_adk(self) -> AdkError {
        match self {
            Self::Arguments => invalid_arguments(),
            Self::Client(error) => error.into_adk(),
        }
    }
}

impl From<CarrierClientError> for ToolFailure {
    fn from(error: CarrierClientError) -> Self {
        Self::Client(error)
    }
}

type ToolResult = Result<Value, ToolFailure>;

// ---------------------------------------------------------------------------
// Tickets
// ---------------------------------------------------------------------------

async fn get_ticket_list(client: &CarrierClient, arguments: &Map<String, Value>) -> ToolResult {
    let board_id = required_text(arguments, "board_id", MAX_ID_BYTES)?;
    let tag_name = optional_text(arguments, "tag_name", MAX_TEXT_BYTES)?.unwrap_or_default();
    let status = optional_text(arguments, "status", MAX_TEXT_BYTES)?.unwrap_or_default();
    let tickets = client
        .rows(
            &["issues", "issues", client.project_id()],
            &[("board_id", board_id), ("limit", "100")],
        )
        .await?;
    let tag = tag_name.to_lowercase();
    let titles = tickets
        .iter()
        .filter(|ticket| {
            tag_name.is_empty()
                || ticket
                    .get("tags")
                    .and_then(Value::as_array)
                    .is_some_and(|tags| {
                        tags.iter()
                            .any(|entry| entry.get("tag").and_then(Value::as_str) == Some(&tag))
                    })
        })
        .filter(|ticket| {
            status.is_empty() || ticket.get("status").and_then(Value::as_str) == Some(status)
        })
        .map(|ticket| py_str(ticket.get("title")))
        .collect::<Vec<_>>();
    Ok(Value::String(titles.join("\n")))
}

async fn create_ticket(client: &CarrierClient, arguments: &Map<String, Value>) -> ToolResult {
    let title = required_text(arguments, "title", MAX_TEXT_BYTES)?;
    let description = required_text(arguments, "description", MAX_DESCRIPTION_TEXT_BYTES)?;
    let severity = required_text(arguments, "severity", MAX_TEXT_BYTES)?;
    let ticket_type = required_text(arguments, "type", MAX_TEXT_BYTES)?;
    let board_id = required_text(arguments, "board_id", MAX_ID_BYTES)?;
    let start_date = required_text(arguments, "start_date", MAX_ID_BYTES)?;
    let end_date = required_text(arguments, "end_date", MAX_ID_BYTES)?;
    let engagement = optional_text(arguments, "engagement", MAX_TEXT_BYTES)?;
    let external_link = optional_text(arguments, "external_link", MAX_TEXT_BYTES)?;
    let assignee = optional_text(arguments, "assignee", MAX_TEXT_BYTES)?;
    let tags = optional_string_list(arguments, "tags")?;

    // The SDK's TicketData validator: both dates must parse as YYYY-MM-DD.
    for (field, value) in [("start_date", start_date), ("end_date", end_date)] {
        if NaiveDate::parse_from_str(value, "%Y-%m-%d").is_err() {
            return Ok(Value::String(format!(
                "🚨 Validation error for ticket data.\n**Missing or invalid fields**: {field}\nPlease correct these fields and try again."
            )));
        }
    }

    // The SDK replaces an engagement NAME with its hash id, falling back to
    // the value as given.
    let engagement = match engagement {
        Some(name) => {
            let document = client
                .request_json(
                    Method::GET,
                    &["engagements", "engagements", client.project_id()],
                    &[],
                    None,
                    false,
                )
                .await?;
            let engagements = list_field(&document, "items")?;
            Some(
                engagements
                    .iter()
                    .find(|entry| entry.get("name").and_then(Value::as_str) == Some(name))
                    .and_then(|entry| entry.get("hash_id"))
                    .cloned()
                    .unwrap_or_else(|| Value::String(name.to_owned())),
            )
        }
        None => None,
    };

    let mut payload = Map::new();
    payload.insert("title".to_owned(), json!(title));
    payload.insert("description".to_owned(), json!(description));
    payload.insert("severity".to_owned(), json!(severity));
    payload.insert("type".to_owned(), json!(ticket_type));
    if let Some(engagement) = engagement.filter(|value| !value.is_null()) {
        payload.insert("engagement".to_owned(), engagement);
    }
    payload.insert("board_id".to_owned(), json!(board_id));
    payload.insert("start_date".to_owned(), json!(start_date));
    payload.insert("end_date".to_owned(), json!(end_date));
    if let Some(link) = external_link {
        payload.insert("external_link".to_owned(), json!(link));
    }
    if let Some(assignee) = assignee {
        payload.insert("assignee".to_owned(), json!(assignee));
    }
    if let Some(tags) = tags {
        payload.insert("tags".to_owned(), json!(tags));
    }
    let response = client
        .request_json(
            Method::POST,
            &["issues", "issues", client.project_id()],
            &[],
            Some(&Value::Object(payload)),
            true,
        )
        .await?;
    // The SDK refuses a 2xx answer without `item`; the ticket may exist.
    if response.get("item").is_none() {
        return Err(response_shape_failure(true).into());
    }
    let rendered = serde_json::to_string_pretty(&response)
        .map_err(|_| ToolFailure::Client(response_shape_failure(true)))?;
    Ok(Value::String(format!(
        "✅ Ticket created successfully!\n{rendered}"
    )))
}

// ---------------------------------------------------------------------------
// Backend reports and tests
// ---------------------------------------------------------------------------

async fn get_reports(client: &CarrierClient, arguments: &Map<String, Value>) -> ToolResult {
    let name = optional_text(arguments, "name", MAX_TEXT_BYTES)?.unwrap_or_default();
    let tag_name = optional_text(arguments, "tag_name", MAX_TEXT_BYTES)?.unwrap_or_default();
    let reports = client
        .rows(
            &["backend_performance", "reports", client.project_id()],
            &[],
        )
        .await?;
    let mut trimmed_reports = Vec::new();
    for report in &reports {
        if !name.is_empty() && report.get("name").and_then(Value::as_str) != Some(name) {
            continue;
        }
        let tags = report
            .get("tags")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        if !tag_name.is_empty()
            && !tags
                .iter()
                .any(|tag| tag.get("title").and_then(Value::as_str) == Some(tag_name))
        {
            continue;
        }
        let mut trimmed = keep_fields(
            report,
            &[
                "id",
                "build_id",
                "name",
                "environment",
                "type",
                "vusers",
                "test_status",
                "start_time",
                "end_time",
                "duration",
            ],
        );
        trimmed.insert(
            "tags".to_owned(),
            Value::Array(
                tags.iter()
                    .map(|tag| tag.get("title").cloned().unwrap_or(Value::Null))
                    .collect(),
            ),
        );
        let test_config = report.get("test_config");
        trimmed.insert(
            "test_parameters".to_owned(),
            name_default_pairs(test_config.and_then(|config| config.get("test_parameters"))),
        );
        if let Some(source) = test_config.and_then(|config| config.get("source")) {
            trimmed.insert("source".to_owned(), source.clone());
        }
        trimmed_reports.push(Value::Object(trimmed));
    }
    Ok(Value::Array(trimmed_reports))
}

async fn add_tag_to_report(client: &CarrierClient, arguments: &Map<String, Value>) -> ToolResult {
    let report_id = required_text(arguments, "report_id", MAX_ID_BYTES)?;
    let tag_name = required_text(arguments, "tag_name", MAX_TEXT_BYTES)?;
    let body = json!({"tags":[{"title":tag_name,"hex":"#5933c6"}]});
    let response = client
        .send(
            Method::POST,
            &[
                "backend_performance",
                "tags",
                client.project_id(),
                report_id,
            ],
            &[],
            CarrierBody::Json(&body),
            CarrierAuth::Bare,
            true,
        )
        .await?;
    // The SDK reads the answer's text whatever the status.
    Ok(Value::String(
        if response.text().contains("Tags was updated") {
            format!("Added tag {tag_name} to report id {report_id}")
        } else {
            format!("Failed to add new tag to report id {report_id}")
        },
    ))
}

async fn backend_tests(client: &CarrierClient) -> Result<Vec<Value>, CarrierClientError> {
    client
        .rows(&["backend_performance", "tests", client.project_id()], &[])
        .await
}

async fn get_tests(client: &CarrierClient) -> ToolResult {
    let tests = backend_tests(client).await?;
    Ok(Value::Array(
        tests
            .iter()
            .map(|test| {
                let mut trimmed = keep_fields(
                    test,
                    &[
                        "id",
                        "name",
                        "entrypoint",
                        "runner",
                        "location",
                        "job_type",
                        "source",
                    ],
                );
                trimmed.insert(
                    "test_parameters".to_owned(),
                    name_default_pairs(test.get("test_parameters")),
                );
                Value::Object(trimmed)
            })
            .collect(),
    ))
}

async fn get_test_by_id(client: &CarrierClient, arguments: &Map<String, Value>) -> ToolResult {
    let test_id = required_text(arguments, "test_id", MAX_ID_BYTES)?;
    let tests = backend_tests(client).await?;
    Ok(tests
        .into_iter()
        .find(|test| py_str(test.get("id")) == test_id)
        .unwrap_or_else(|| Value::Object(Map::new())))
}

const RUN_TEST_PARAMETERS_INSTRUCTION: &str = "If the user has already indicated that default parameters should be used, pass 'default_test_parameters' as 'test_parameters' and invoke the '_run' method again without prompting the user.\nIf the user wants to proceed with default parameters, respond with 'use default'.\nIn this case, the agent should pass 'default_test_parameters' as 'test_parameters' to the tool.\nIf the user provides specific overrides, parse them into a list of dictionaries in the following format:\n[{'vUsers': '5', 'duration': '120'}].\nEach dictionary should contain the parameter name and its desired value.\nEnsure that you correctly parse and validate the user's input before invoking the '_run' method.";

#[expect(
    clippy::too_many_lines,
    reason = "one SDK tool's conversational steps, kept in its source order"
)]
async fn run_test_by_id(client: &CarrierClient, arguments: &Map<String, Value>) -> ToolResult {
    let test_id = match arguments.get("test_id") {
        None => None,
        Some(Value::Number(number)) if number.is_i64() || number.is_u64() => {
            Some(number.to_string())
        }
        Some(_) => return Err(ToolFailure::Arguments),
    };
    let name = optional_text(arguments, "name", MAX_TEXT_BYTES)?;
    let location = optional_text(arguments, "location", MAX_TEXT_BYTES)?;
    let test_parameters = match arguments.get("test_parameters") {
        None => None,
        Some(Value::Array(values)) => Some(values.clone()),
        Some(_) => return Err(ToolFailure::Arguments),
    };
    let cloud_settings = match arguments.get("cloud_settings") {
        None => Map::new(),
        Some(Value::Object(values)) => values.clone(),
        Some(_) => return Err(ToolFailure::Arguments),
    };
    let test_id = test_id.filter(|id| id != "0");
    let name = name.filter(|name| !name.is_empty());
    if test_id.is_none() && name.is_none() {
        return Ok(json!({"message":"Please provide test id or test name to start"}));
    }

    // The SDK compares `str(test["id"])` with the integer argument, which can
    // never be equal, so its id lookup is dead; the id is compared as text.
    let tests = backend_tests(client).await?;
    let Some(mut test) = tests.into_iter().find(|test| {
        test_id
            .as_deref()
            .is_some_and(|id| py_str(test.get("id")) == id)
            || name.is_some_and(|name| py_str(test.get("name")) == name)
    }) else {
        return Ok(Value::String(format!(
            "Test with id {} or name {} not found.",
            test_id.as_deref().unwrap_or("None"),
            name.unwrap_or("None")
        )));
    };

    let mut default_parameters = test
        .get("test_parameters")
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();
    let Some(test_parameters) = test_parameters else {
        return Ok(json!({
            "message":"The test requires confirmation or customization of the following parameters before execution.",
            "default_test_parameters":default_parameters,
            "instruction":RUN_TEST_PARAMETERS_INSTRUCTION
        }));
    };
    let Some(user_parameters) = normalize_test_parameters(&test_parameters) else {
        return Ok(Value::String(
            "Invalid format for test_parameters. Provide as a list of 'key=value' strings or a list of dictionaries.".to_owned(),
        ));
    };
    let mut updated_parameters = Vec::with_capacity(default_parameters.len());
    for parameter in &mut default_parameters {
        let Some(object) = parameter.as_object_mut() else {
            continue;
        };
        let parameter_name = py_str(object.get("name"));
        if let Some(value) = user_parameters
            .iter()
            .find_map(|candidate| candidate.get(&parameter_name))
        {
            object.insert("default".to_owned(), value.clone());
        }
        updated_parameters.push(json!({
            "name":object.get("name").cloned().unwrap_or(Value::Null),
            "type":object.get("type").cloned().unwrap_or(Value::Null),
            "description":object.get("description").cloned().unwrap_or(Value::Null),
            "default":object.get("default").cloned().unwrap_or(Value::Null),
        }));
    }

    let locations = client
        .request_json(
            Method::GET,
            &["shared", "locations", "default", client.project_id()],
            &[],
            None,
            false,
        )
        .await?;
    let cloud_regions = locations
        .get("cloud_regions")
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();
    let Some(location) = location.filter(|location| !location.is_empty()) else {
        return Ok(json!({
            "message":"Please select a location to execute the test.",
            "available_locations":{
                "public_regions":locations.get("public_regions").cloned().unwrap_or(Value::Null),
                "project_regions":locations.get("project_regions").cloned().unwrap_or(Value::Null),
                "cloud_regions":cloud_regions.iter().map(|region| region.get("name").cloned().unwrap_or(Value::Null)).collect::<Vec<_>>(),
            },
            "instruction":"For public_regions and project_regions, provide the region name. For cloud_regions, provide the region name and optionally override cloud_settings."
        }));
    };

    let mut cloud_settings = cloud_settings;
    if let Some(region) = cloud_regions
        .iter()
        .find(|region| region.get("name").and_then(Value::as_str) == Some(location))
    {
        let mut available = region
            .get("cloud_settings")
            .and_then(Value::as_object)
            .cloned()
            .unwrap_or_default();
        available.insert("instance_type".to_owned(), json!("spot"));
        available.insert("ec2_instance_type".to_owned(), json!("t2.medium"));
        if cloud_settings.is_empty() {
            return Ok(json!({
                "message":format!("Please confirm or override the following cloud settings for the selected location: {location}"),
                "available_cloud_settings":available,
                "instruction":"Provide a dictionary to override cloud settings, e.g., {'region_name': 'us-west-1', 'instance_type': 't2.large'}. Don't provide this parameter as string! It should be a dictionary! Ensure these settings are passed to the 'cloud_settings' argument, not 'test_parameters'.Ensure these settings are passed as a valid dictionary not string"
            }));
        }
        let invalid = cloud_settings
            .keys()
            .filter(|key| !available.contains_key(*key))
            .cloned()
            .collect::<Vec<_>>();
        if !invalid.is_empty() {
            return Ok(Value::String(format!(
                "Invalid keys in cloud settings: {}. Allowed keys: {}",
                py_list(&invalid),
                py_list(&available.keys().cloned().collect::<Vec<_>>())
            )));
        }
        available.extend(cloud_settings);
        cloud_settings = available;
    }

    let mut common_params = Map::new();
    for parameter in &default_parameters {
        let parameter_name = py_str(parameter.get("name"));
        if matches!(
            parameter_name.as_str(),
            "test_name" | "test_type" | "env_type"
        ) {
            common_params.insert(parameter_name, parameter.clone());
        }
    }
    let mut env_vars = test
        .get("env_vars")
        .and_then(Value::as_object)
        .cloned()
        .unwrap_or_default();
    env_vars.insert("cloud_settings".to_owned(), Value::Object(cloud_settings));
    common_params.insert("env_vars".to_owned(), Value::Object(env_vars));
    common_params.insert(
        "parallel_runners".to_owned(),
        test.get("parallel_runners").cloned().unwrap_or(Value::Null),
    );
    common_params.insert("location".to_owned(), json!(location));
    let integrations = test
        .as_object_mut()
        .and_then(|object| object.remove("integrations"))
        .unwrap_or_else(|| Value::Object(Map::new()));
    let body = json!({
        "common_params":common_params,
        "test_parameters":updated_parameters,
        "integrations":integrations,
    });
    let run_id = py_str(test.get("id"));
    let response = client
        .request_json(
            Method::POST,
            &["backend_performance", "test", client.project_id(), &run_id],
            &[],
            Some(&body),
            true,
        )
        .await?;
    let report_id = py_str_or_empty(response.get("result_id"));
    Ok(Value::String(format!(
        "Test started. Report id: {report_id}. Link to report:{}/-/performance/backend/results?result_id={report_id}",
        client.display_url()
    )))
}

/// The SDK's `_normalize_test_parameters`: `["k=v", ...]` becomes
/// `[{"k": "v"}, ...]`; a list of objects is used as is.
fn normalize_test_parameters(values: &[Value]) -> Option<Vec<Map<String, Value>>> {
    if values
        .iter()
        .all(|value| value.as_str().is_some_and(|text| text.contains('=')))
    {
        return Some(
            values
                .iter()
                .filter_map(Value::as_str)
                .map(|text| {
                    let (key, value) = text.split_once('=').unwrap_or((text, ""));
                    let mut entry = Map::new();
                    entry.insert(key.trim().to_owned(), json!(value.trim()));
                    entry
                })
                .collect(),
        );
    }
    values
        .iter()
        .map(|value| value.as_object().cloned())
        .collect()
}

const AVAILABLE_RUNNERS: [(&str, &str); 4] = [
    ("JMeter_v5.6.3", "v5.6.3"),
    ("JMeter_v5.5", "v5.5"),
    ("Gatling_v3.7", "v3.7"),
    ("Gatling_maven", "maven"),
];

fn runners_object() -> Value {
    Value::Object(
        AVAILABLE_RUNNERS
            .iter()
            .map(|(key, value)| ((*key).to_owned(), json!(value)))
            .collect(),
    )
}

fn runner_message(message: &str) -> Value {
    json!({
        "message":message,
        "instructions":"You can choose a test runner by providing either the key or the value from the available options below. For example, you can provide 'JMeter_v5.5' or 'v5.5'.",
        "available_runners":runners_object(),
        "example":"For JMeter 5.5, you can provide either 'JMeter_v5.5' or 'v5.5'.",
    })
}

#[expect(
    clippy::too_many_lines,
    reason = "one SDK tool's conversational steps, kept in its source order"
)]
async fn create_backend_test(client: &CarrierClient, arguments: &Map<String, Value>) -> ToolResult {
    let text = |name: &str| optional_text(arguments, name, MAX_TEXT_BYTES);
    let test_name = text("test_name")?.unwrap_or_default();
    let test_type = text("test_type")?.unwrap_or_default();
    let env_type = text("env_type")?.unwrap_or_default();
    let entrypoint = text("entrypoint")?.unwrap_or_default();
    let custom_cmd = text("custom_cmd")?.unwrap_or_default();
    let runner = text("runner")?.unwrap_or_default();
    let source = optional_object(arguments, "source")?;
    let test_parameters = match arguments.get("test_parameters") {
        None => None,
        Some(Value::Array(values)) if values.iter().all(Value::is_object) => Some(values.clone()),
        Some(_) => return Err(ToolFailure::Arguments),
    };
    let email_integration = optional_object(arguments, "email_integration")?;

    if test_name.is_empty() {
        return Ok(json!({"message":"Please provide test name"}));
    }
    if test_type.is_empty() {
        return Ok(
            json!({"message":"Please provide performance test type (capacity, baseline, response time, stable, stress, etc)"}),
        );
    }
    if env_type.is_empty() {
        return Ok(json!({"message":"Please provide test env (stage, prod, dev, etc)"}));
    }
    if entrypoint.is_empty() {
        return Ok(
            json!({"message":"Please provide test entrypoint (JMeter script path or Gatling simulation path)"}),
        );
    }
    if custom_cmd.is_empty() {
        return Ok(
            json!({"message":"Please provide custom_cmd. This parameter is optional. (e.g., -l /tmp/reports/jmeter.jtl -e -o /tmp/reports/html_report)"}),
        );
    }
    if runner.is_empty() {
        return Ok(runner_message(
            "Please provide a valid test runner. The test runner specifies the tool and version to use for running the test.",
        ));
    }
    let Some(runner_value) = AVAILABLE_RUNNERS
        .iter()
        .find_map(|(key, value)| (runner == *key || runner == *value).then_some(*value))
    else {
        return Ok(runner_message(
            "Invalid test runner provided. Please choose a valid test runner from the available options.",
        ));
    };
    let Some(source) = source.filter(|source| !source.is_empty()) else {
        return Ok(json!({
            "message":"Please provide the test source configuration. The source configuration is required to specify the Git repository details for the test. Ensure all fields are provided in the correct format.",
            "instructions":"The 'source' parameter should be a dictionary with the following keys:\n- 'name' (required): The type of source (e.g., 'git_https').\n- 'repo' (required): The URL of the Git repository.\n- 'branch' (optional): The branch of the repository to use.\n- 'username' (optional): The username for accessing the repository.\n- 'password' (optional): The password or token for accessing the repository.",
            "example_source":{"name":"git_https","repo":"https://your_git_repo.git","branch":"main","username":"","password":""},
        }));
    };
    let Some(test_parameters) = test_parameters else {
        return Ok(json!({
            "message":"Do you want to add test parameters? Test parameters allow you to configure the test with specific values.",
            "instructions":"Provide test parameters as a list of dictionaries in the format:\n- {'name': 'VUSERS', 'default': '5'}\n- {'name': 'DURATION', 'default': '60'}\n- {'name': 'RAMP_UP', 'default': '30'}\nYou can provide multiple parameters as a list, e.g., [{'name': 'VUSERS', 'default': '5'}, {'name': 'DURATION', 'default': '60'}].\nIf no parameters are needed, respond with 'no'.",
            "example_parameters":[{"name":"VUSERS","default":"5"},{"name":"DURATION","default":"60"},{"name":"RAMP_UP","default":"30"}],
        }));
    };

    let integrations = client
        .request_json(
            Method::GET,
            &["integrations", "integrations", client.project_id()],
            &[("name", "reporter_email")],
            None,
            false,
        )
        .await?;
    let integrations = integrations.as_array().cloned().unwrap_or_default();
    let Some(email_integration) = email_integration else {
        return Ok(json!({
            "message":"Do you want to configure email integration?",
            "instructions":"If the user indicates no integrations are needed make sure to pass email_integration as empty dict to _run method and invoke it ones again with email_integration={}.If yes, select an integration from the available options below and provide email recipients.\nIf no, respond with 'no'. ",
            "available_integrations":integrations.iter().map(|integration| json!({
                "id":integration.get("id").cloned().unwrap_or(Value::Null),
                "name":integration.get("config").and_then(|config| config.get("name")).cloned().unwrap_or(Value::Null),
                "description":integration.get("section").and_then(|section| section.get("integration_description")).cloned().unwrap_or(Value::Null),
            })).collect::<Vec<_>>(),
            "example_response":{"integration_id":1,"recipients":["example@example.com","user@example.com"]},
        }));
    };
    let reporters = match (
        integrations.first(),
        email_integration.get("integration_id"),
        email_integration.get("recipients"),
    ) {
        (Some(first), Some(integration_id), Some(recipients)) => json!({
            "reporters":{"reporter_email":{
                "id":integration_id,
                "is_local":true,
                "project_id":first.get("project_id").cloned().unwrap_or(Value::Null),
                "recipients":recipients,
            }}
        }),
        _ => json!({}),
    };

    let data = json!({
        "common_params":{
            "name":test_name,
            "test_type":test_type,
            "env_type":env_type,
            "entrypoint":entrypoint,
            "runner":runner_value,
            "source":source,
            "env_vars":{"cpu_quota":1,"memory_quota":4,"cloud_settings":{},"custom_cmd":custom_cmd},
            "parallel_runners":1,
            "cc_env_vars":{},
            "customization":{},
            "location":"default",
        },
        "test_parameters":test_parameters,
        "integrations":reporters,
        "scheduling":[],
        "run_test":false,
    });
    let encoded = serde_json::to_string(&data).map_err(|_| ToolFailure::Arguments)?;
    let response = client
        .send(
            Method::POST,
            &["backend_performance", "tests", client.project_id()],
            &[],
            CarrierBody::Form(&[("data", encoded.as_str())]),
            CarrierAuth::Bare,
            true,
        )
        .await?;
    // The SDK reads the answer whatever the status: JSON means created.
    Ok(Value::String(match response.json() {
        Some(info) => format!("Test created successfully. {info}"),
        None => format!("Failed to create the test. {}", response.text()),
    }))
}

// ---------------------------------------------------------------------------
// UI reports and tests
// ---------------------------------------------------------------------------

const UI_REPORT_FIELDS: [&str; 13] = [
    "id",
    "name",
    "environment",
    "test_type",
    "browser",
    "browser_version",
    "test_status",
    "start_time",
    "end_time",
    "duration",
    "loops",
    "aggregation",
    "passed",
];

async fn ui_reports(client: &CarrierClient) -> Result<Vec<Value>, CarrierClientError> {
    client
        .rows(&["ui_performance", "reports", client.project_id()], &[])
        .await
}

async fn ui_tests(client: &CarrierClient) -> Result<Vec<Value>, CarrierClientError> {
    client
        .rows(&["ui_performance", "tests", client.project_id()], &[])
        .await
}

fn trimmed_ui_report(report: &Value) -> Value {
    let mut trimmed = keep_fields(report, &UI_REPORT_FIELDS);
    let test_config = report.get("test_config");
    trimmed.insert(
        "test_parameters".to_owned(),
        name_default_pairs(test_config.and_then(|config| config.get("test_parameters"))),
    );
    if let Some(source) = test_config.and_then(|config| config.get("source")) {
        trimmed.insert("source".to_owned(), source.clone());
    }
    Value::Object(trimmed)
}

fn name_matches(report: &Value, name: &str) -> bool {
    report
        .get("name")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_lowercase()
        .contains(&name.to_lowercase())
}

async fn get_ui_reports(client: &CarrierClient, arguments: &Map<String, Value>) -> ToolResult {
    // `report_id` and `current_date` are declared by the SDK and ignored.
    optional_text(arguments, "report_id", MAX_ID_BYTES)?;
    optional_text(arguments, "current_date", MAX_ID_BYTES)?;
    let name = optional_text(arguments, "name", MAX_TEXT_BYTES)?.filter(|name| !name.is_empty());
    let start_time =
        optional_text(arguments, "start_time", MAX_ID_BYTES)?.filter(|value| !value.is_empty());
    let end_time =
        optional_text(arguments, "end_time", MAX_ID_BYTES)?.filter(|value| !value.is_empty());
    if name.is_none() && start_time.is_none() && end_time.is_none() {
        return Ok(json!({
            "message":"⚠️ Please provide at least one filter parameter to search UI reports!\n\n**Available filters:** 🕵️\n- `name`\n- `start_time`\n- `end_time`\n\n**Example:**\n- start_time='2025-06-01'\n- end_time='2025-06-18'\n- name='My-Test'",
            "parameters":{"name":null,"start_time":null,"end_time":null},
        }));
    }
    let reports = ui_reports(client).await?;
    if start_time.is_none() && end_time.is_none() {
        let name = name.unwrap_or_default();
        return Ok(Value::Array(
            reports
                .iter()
                .filter(|report| name_matches(report, name))
                .map(trimmed_ui_report)
                .collect(),
        ));
    }
    let start = start_time.and_then(PyDateTime::parse);
    let end = end_time.and_then(PyDateTime::parse);
    let mut trimmed_reports = Vec::new();
    for report in &reports {
        if name.is_some_and(|name| !name_matches(report, name)) {
            continue;
        }
        if (start.is_some() || end.is_some())
            && let Some(report_start) = report.get("start_time").and_then(Value::as_str)
            && !report_start.is_empty()
        {
            let Some(report_time) = PyDateTime::parse(report_start) else {
                continue;
            };
            let mut excluded = false;
            for (bound, outside) in [(start, Ordering::Less), (end, Ordering::Greater)] {
                let Some(bound) = bound else { continue };
                // Python raises comparing a naive with an aware datetime.
                let Some(ordering) = report_time.compare(bound) else {
                    return Ok(Value::String(
                        "can't compare offset-naive and offset-aware datetimes".to_owned(),
                    ));
                };
                if ordering == outside {
                    excluded = true;
                    break;
                }
            }
            if excluded {
                continue;
            }
        }
        trimmed_reports.push(trimmed_ui_report(report));
    }
    Ok(Value::Array(trimmed_reports))
}

/// A datetime as Python's `fromisoformat` (or `strptime('%Y-%m-%d')`)
/// yields it: naive or offset-aware, which Python refuses to compare.
#[derive(Clone, Copy)]
enum PyDateTime {
    Naive(NaiveDateTime),
    Aware(DateTime<FixedOffset>),
}

impl PyDateTime {
    fn parse(value: &str) -> Option<Self> {
        let value = value.trim();
        if let Ok(aware) = DateTime::parse_from_rfc3339(value) {
            return Some(Self::Aware(aware));
        }
        for format in [
            "%Y-%m-%dT%H:%M:%S%.f%:z",
            "%Y-%m-%d %H:%M:%S%.f%:z",
            "%Y-%m-%dT%H:%M%:z",
            "%Y-%m-%d %H:%M%:z",
        ] {
            if let Ok(aware) = DateTime::parse_from_str(value, format) {
                return Some(Self::Aware(aware));
            }
        }
        for format in [
            "%Y-%m-%dT%H:%M:%S%.f",
            "%Y-%m-%d %H:%M:%S%.f",
            "%Y-%m-%dT%H:%M",
            "%Y-%m-%d %H:%M",
        ] {
            if let Ok(naive) = NaiveDateTime::parse_from_str(value, format) {
                return Some(Self::Naive(naive));
            }
        }
        NaiveDate::parse_from_str(value, "%Y-%m-%d")
            .ok()
            .and_then(|date| date.and_hms_opt(0, 0, 0))
            .map(Self::Naive)
    }

    fn compare(self, other: Self) -> Option<Ordering> {
        match (self, other) {
            (Self::Naive(left), Self::Naive(right)) => Some(left.cmp(&right)),
            (Self::Aware(left), Self::Aware(right)) => Some(left.cmp(&right)),
            _ => None,
        }
    }
}

async fn get_ui_report_by_id(client: &CarrierClient, arguments: &Map<String, Value>) -> ToolResult {
    let report_id = required_text(arguments, "report_id", MAX_ID_BYTES)?;
    let reports = ui_reports(client).await?;
    let mut report = reports
        .into_iter()
        .find(|report| py_str(report.get("id")) == report_id)
        .and_then(|report| report.as_object().cloned())
        .unwrap_or_default();
    let mut links = Vec::new();
    if let Some(uid) = report.get("uid").filter(|uid| py_truthy(uid)) {
        // The SDK swallows every failure of this lookup into an empty list.
        links = ui_report_links(client, &py_str(Some(uid)))
            .await
            .unwrap_or_default();
    }
    report.insert("report_links".to_owned(), json!(links));
    Ok(Value::Object(report))
}

fn html_name_pattern() -> Option<&'static Regex> {
    static PATTERN: OnceLock<Option<Regex>> = OnceLock::new();
    PATTERN
        .get_or_init(|| Regex::new(r"^(.+?\.html)").ok())
        .as_ref()
}

async fn ui_report_links(
    client: &CarrierClient,
    uid: &str,
) -> Result<Vec<String>, CarrierClientError> {
    let response = client
        .request_json(
            Method::GET,
            &["ui_performance", "results", client.project_id(), uid],
            &[("sort", "loop"), ("order", "asc")],
            None,
            false,
        )
        .await?;
    let items: Vec<&Value> = match &response {
        Value::Object(groups) => groups
            .values()
            .filter_map(Value::as_array)
            .flatten()
            .collect(),
        Value::Array(items) => items.iter().collect(),
        _ => Vec::new(),
    };
    let names = items
        .into_iter()
        .filter_map(|item| item.get("file_name").and_then(Value::as_str))
        .filter(|name| !name.is_empty())
        .map(|name| {
            html_name_pattern()
                .and_then(|pattern| pattern.captures(name))
                .and_then(|captures| captures.get(1))
                .map_or(name, |matched| matched.as_str())
                .to_owned()
        })
        .collect::<BTreeSet<_>>();
    Ok(names
        .into_iter()
        .map(|name| {
            format!(
                "{REPORT_LINK_PREFIX}/{}/reports/{name}",
                client.project_id()
            )
        })
        .collect())
}

#[expect(
    clippy::too_many_lines,
    reason = "one SDK tool's conversational steps, kept in its source order"
)]
async fn get_ui_tests(client: &CarrierClient, arguments: &Map<String, Value>) -> ToolResult {
    let name = optional_text(arguments, "name", MAX_TEXT_BYTES)?.filter(|name| !name.is_empty());
    let include_schedules = optional_bool(arguments, "include_schedules")?;
    let include_config = optional_bool(arguments, "include_config")?;
    let tests = ui_tests(client).await?;
    let mut result = Vec::new();
    for test in tests
        .iter()
        .filter(|test| name.is_none_or(|name| name_matches(test, name)))
    {
        let mut trimmed = keep_fields(
            test,
            &[
                "id",
                "name",
                "browser",
                "loops",
                "aggregation",
                "parallel_runners",
                "location",
                "entrypoint",
                "runner",
                "job_type",
            ],
        );
        if let Some(uid) = test.get("test_uid") {
            trimmed.insert("test_uid".to_owned(), uid.clone());
        }
        if let Some(parameters) = test.get("test_parameters") {
            trimmed.insert(
                "test_parameters".to_owned(),
                Value::Array(
                    parameters
                        .as_array()
                        .map(Vec::as_slice)
                        .unwrap_or_default()
                        .iter()
                        .map(|parameter| {
                            json!({
                                "name":parameter.get("name").cloned().unwrap_or(Value::Null),
                                "type":parameter.get("type").cloned().unwrap_or(Value::Null),
                                "default":parameter.get("default").cloned().unwrap_or(Value::Null),
                                "description":parameter.get("description").cloned().unwrap_or(Value::Null),
                            })
                        })
                        .collect(),
                ),
            );
        }
        if let Some(env_vars) = test.get("env_vars").filter(|value| py_truthy(value)) {
            let field = |key: &str| env_vars.get(key).cloned().unwrap_or(Value::Null);
            trimmed.insert("environment".to_owned(), field("ENV"));
            trimmed.insert("custom_cmd".to_owned(), field("custom_cmd"));
            trimmed.insert(
                "resources".to_owned(),
                json!({"cpu":field("cpu_quota"),"memory":field("memory_quota")}),
            );
        }
        if let Some(source) = test.get("source") {
            let field = |key: &str| source.get(key).cloned().unwrap_or(Value::Null);
            trimmed.insert(
                "source".to_owned(),
                json!({"type":field("name"),"repo":field("repo"),"branch":field("branch")}),
            );
        }
        if include_config {
            let integrations = test.get("integrations");
            if let Some(clouds) = integrations
                .and_then(|value| value.get("clouds"))
                .and_then(Value::as_object)
            {
                for (provider, cloud) in clouds {
                    let field = |key: &str| cloud.get(key).cloned().unwrap_or(Value::Null);
                    trimmed.insert(
                        "cloud".to_owned(),
                        json!({
                            "provider":provider,
                            "region":field("region_name"),
                            "instance_type":field("ec2_instance_type"),
                            "image_id":field("image_id"),
                        }),
                    );
                }
            }
            if let Some(reporters) = integrations
                .and_then(|value| value.get("reporters"))
                .and_then(Value::as_object)
                && let Some(email) = reporters.get("reporter_email")
            {
                trimmed.insert(
                    "reporters".to_owned(),
                    json!({"email_recipients":email.get("recipients").cloned().unwrap_or_else(|| json!([]))}),
                );
            }
        }
        if include_schedules && let Some(schedules) = test.get("schedules") {
            let (mut active, mut inactive) = (Vec::new(), Vec::new());
            for schedule in schedules.as_array().map(Vec::as_slice).unwrap_or_default() {
                let field = |key: &str| schedule.get(key).cloned().unwrap_or(Value::Null);
                let info = json!({"id":field("id"),"name":field("name"),"cron":field("cron")});
                if schedule.get("active").is_some_and(py_truthy) {
                    active.push(info);
                } else {
                    inactive.push(info);
                }
            }
            trimmed.insert(
                "schedules".to_owned(),
                json!({"active":active,"inactive":inactive}),
            );
        }
        result.push(Value::Object(trimmed));
    }
    Ok(Value::Array(result))
}

// ---------------------------------------------------------------------------
// run_ui_test
// ---------------------------------------------------------------------------

async fn ui_locations(client: &CarrierClient) -> Result<Value, CarrierClientError> {
    client
        .request_json(
            Method::GET,
            &["shared", "locations", client.project_id()],
            &[],
            None,
            false,
        )
        .await
}

/// The SDK's `_get_available_locations`: a failure becomes one line.
async fn available_locations(client: &CarrierClient) -> Vec<String> {
    let Ok(locations) = ui_locations(client).await else {
        return vec!["  Error loading locations".to_owned()];
    };
    let mut lines = Vec::new();
    for (key, label) in [
        ("public_regions", "  Public Regions:"),
        ("project_regions", "  Project Regions:"),
    ] {
        let regions = locations
            .get(key)
            .and_then(Value::as_array)
            .map(Vec::as_slice)
            .unwrap_or_default();
        if !regions.is_empty() {
            lines.push(label.to_owned());
            lines.extend(
                regions
                    .iter()
                    .map(|region| format!("    - {}", py_str(Some(region)))),
            );
        }
    }
    let cloud = locations
        .get("cloud_regions")
        .and_then(Value::as_array)
        .map(Vec::as_slice)
        .unwrap_or_default();
    if !cloud.is_empty() {
        lines.push("  Cloud Regions:".to_owned());
        for region in cloud {
            let name = region
                .get("name")
                .map_or_else(|| "Unknown".to_owned(), |name| py_str(Some(name)));
            let aws_region = region
                .get("cloud_settings")
                .and_then(|settings| settings.get("region_name"))
                .map_or_else(String::new, |value| py_str(Some(value)));
            lines.push(format!("    - {name} ({aws_region})"));
        }
    }
    if lines.is_empty() {
        lines.push("  No locations available".to_owned());
    }
    lines
}

fn region_names(locations: &Value, key: &str) -> Vec<String> {
    locations
        .get(key)
        .and_then(Value::as_array)
        .map(Vec::as_slice)
        .unwrap_or_default()
        .iter()
        .filter_map(Value::as_str)
        .map(ToOwned::to_owned)
        .collect()
}

fn find_cloud_region<'a>(locations: &'a Value, wanted: &str) -> Option<&'a Value> {
    let regions = locations.get("cloud_regions").and_then(Value::as_array)?;
    let wanted = wanted.to_lowercase();
    let name = |region: &Value| {
        region
            .get("name")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_lowercase()
    };
    regions
        .iter()
        .find(|region| name(region) == wanted)
        .or_else(|| regions.iter().find(|region| name(region).contains(&wanted)))
}

fn find_ui_test<'a>(tests: &'a [Value], test_id: &str, test_name: &str) -> Option<&'a Value> {
    let by_name = |name: &str| {
        let wanted = name.to_lowercase();
        let lower = |test: &Value| {
            test.get("name")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_lowercase()
        };
        tests
            .iter()
            .find(|test| lower(test) == wanted)
            .or_else(|| tests.iter().find(|test| lower(test).contains(&wanted)))
    };
    let numeric = !test_id.is_empty() && test_id.bytes().all(|byte| byte.is_ascii_digit());
    let mut found = None;
    if numeric && let Ok(id) = test_id.parse::<u64>() {
        found = tests
            .iter()
            .find(|test| test.get("id").and_then(Value::as_u64) == Some(id));
    }
    if found.is_none() && !test_name.trim().is_empty() {
        found = by_name(test_name);
    }
    if found.is_none() && !test_id.trim().is_empty() && !numeric {
        found = by_name(test_id);
    }
    found
}

fn post_data(
    details: &Value,
    overrides: Option<&UiOverrides<'_>>,
    cloud: Option<(Value, String)>,
) -> Value {
    let field = |key: &str| details.get(key).cloned();
    let parameters = field("test_parameters").unwrap_or_else(|| json!([]));
    let env_vars = field("env_vars").unwrap_or_else(|| json!({}));
    let env = |key: &str| env_vars.get(key).cloned().unwrap_or(Value::Null);
    let find_parameter = |name: &str| {
        parameters
            .as_array()
            .and_then(|values| {
                values
                    .iter()
                    .find(|value| value.get("name").and_then(Value::as_str) == Some(name))
            })
            .cloned()
            .unwrap_or_else(|| json!({}))
    };
    let s3 = details
        .get("integrations")
        .and_then(|value| value.get("system"))
        .and_then(|value| value.get("s3_integration"))
        .cloned()
        .unwrap_or_else(|| json!({}));
    let location = field("location").unwrap_or_else(|| json!(""));
    let (cloud_settings, location) = match cloud {
        Some((settings, location)) => (settings, json!(location)),
        None => (
            env_vars
                .get("cloud_settings")
                .cloned()
                .unwrap_or_else(|| json!({})),
            location,
        ),
    };
    let pick =
        |value: Option<&str>, key: &str| value.map_or_else(|| env(key), |value| json!(value));
    let (cpu, memory, custom_cmd, loops) = match overrides {
        Some(overrides) => (
            pick(overrides.cpu_quota, "cpu_quota"),
            pick(overrides.memory_quota, "memory_quota"),
            overrides.custom_cmd.map_or_else(
                || {
                    env_vars
                        .get("custom_cmd")
                        .cloned()
                        .unwrap_or_else(|| json!(""))
                },
                |value| json!(value),
            ),
            overrides.loops.map_or_else(
                || field("loops").unwrap_or_else(|| json!(1)),
                |value| json!(value),
            ),
        ),
        None => (
            env("cpu_quota"),
            env("memory_quota"),
            env_vars
                .get("custom_cmd")
                .cloned()
                .unwrap_or_else(|| json!("")),
            field("loops").unwrap_or_else(|| json!(1)),
        ),
    };
    json!({
        "common_params":{
            "name":find_parameter("test_name"),
            "test_type":find_parameter("test_type"),
            "env_type":find_parameter("env_type"),
            "env_vars":{
                "cpu_quota":cpu,
                "memory_quota":memory,
                "cloud_settings":cloud_settings,
                "ENV":env_vars.get("ENV").cloned().unwrap_or_else(|| json!("prod")),
                "custom_cmd":custom_cmd,
            },
            "parallel_runners":field("parallel_runners").unwrap_or_else(|| json!(1)),
            "location":location,
        },
        "test_parameters":parameters,
        "integrations":{"system":{"s3_integration":{
            "integration_id":s3.get("integration_id").cloned().unwrap_or(Value::Null),
            "is_local":s3.get("is_local").cloned().unwrap_or(Value::Null),
        }}},
        "loops":loops,
        "aggregation":field("aggregation").unwrap_or_else(|| json!("max")),
    })
}

struct UiOverrides<'a> {
    cpu_quota: Option<&'a str>,
    memory_quota: Option<&'a str>,
    cloud_settings: Option<&'a str>,
    custom_cmd: Option<&'a str>,
    loops: Option<&'a str>,
}

impl UiOverrides<'_> {
    const fn any(&self) -> bool {
        self.cpu_quota.is_some()
            || self.memory_quota.is_some()
            || self.cloud_settings.is_some()
            || self.custom_cmd.is_some()
            || self.loops.is_some()
    }
}

/// The SDK's `cloud_settings` resolution: a public or project region is a
/// location with empty settings; a cloud region (exact, then partial name)
/// carries its integration with the SDK's fixed instance types; anything
/// else is used as a location name. A locations failure falls back the same.
async fn resolve_cloud(client: &CarrierClient, wanted: &str) -> (Value, String) {
    let Ok(locations) = ui_locations(client).await else {
        return (json!({}), wanted.to_owned());
    };
    if region_names(&locations, "public_regions")
        .iter()
        .any(|name| name == wanted)
        || region_names(&locations, "project_regions")
            .iter()
            .any(|name| name == wanted)
    {
        return (json!({}), wanted.to_owned());
    }
    let Some(region) = find_cloud_region(&locations, wanted) else {
        return (json!({}), wanted.to_owned());
    };
    let config = region
        .get("cloud_settings")
        .cloned()
        .unwrap_or_else(|| json!({}));
    let field = |key: &str| config.get(key).cloned().unwrap_or(Value::Null);
    (
        json!({
            "integration_name":field("integration_name"),
            "id":field("id"),
            "project_id":field("project_id"),
            "aws_access_key":field("aws_access_key"),
            "aws_secret_access_key":config.get("aws_secret_access_key").cloned().unwrap_or_else(|| json!({})),
            "region_name":field("region_name"),
            "security_groups":field("security_groups"),
            "image_id":field("image_id"),
            "key_name":config.get("key_name").cloned().unwrap_or_else(|| json!("")),
            "instance_type":"spot",
            "ec2_instance_type":"t2.xlarge",
        }),
        region
            .get("name")
            .map_or_else(|| wanted.to_owned(), |name| py_str(Some(name))),
    )
}

/// The SDK's `_validate_cloud_settings_location`; a failure admits.
async fn cloud_location_is_valid(client: &CarrierClient, wanted: &str) -> bool {
    let Ok(locations) = ui_locations(client).await else {
        return true;
    };
    region_names(&locations, "public_regions")
        .iter()
        .any(|name| name == wanted)
        || region_names(&locations, "project_regions")
            .iter()
            .any(|name| name == wanted)
        || find_cloud_region(&locations, wanted).is_some()
}

#[expect(
    clippy::too_many_lines,
    reason = "one SDK tool's conversational steps, kept in its source order"
)]
async fn run_ui_test(client: &CarrierClient, arguments: &Map<String, Value>) -> ToolResult {
    let test_id = optional_text(arguments, "test_id", MAX_ID_BYTES)?.unwrap_or_default();
    let test_name = optional_text(arguments, "test_name", MAX_TEXT_BYTES)?.unwrap_or_default();
    let overrides = UiOverrides {
        cpu_quota: optional_text(arguments, "cpu_quota", MAX_ID_BYTES)?,
        memory_quota: optional_text(arguments, "memory_quota", MAX_ID_BYTES)?,
        cloud_settings: optional_text(arguments, "cloud_settings", MAX_TEXT_BYTES)?,
        custom_cmd: optional_text(arguments, "custom_cmd", MAX_TEXT_BYTES)?,
        loops: optional_text(arguments, "loops", MAX_ID_BYTES)?,
    };
    let proceed_with_defaults = optional_bool(arguments, "proceed_with_defaults")?;

    if test_id.trim().is_empty() && test_name.trim().is_empty() {
        return Ok(run_ui_missing_input(client).await);
    }
    let tests = ui_tests(client).await?;
    let Some(test) = find_ui_test(&tests, test_id, test_name) else {
        let mut criteria = Vec::new();
        if !test_id.is_empty() {
            criteria.push(format!("ID: {test_id}"));
        }
        if !test_name.is_empty() {
            criteria.push(format!("Name: {test_name}"));
        }
        let mut lines = vec![
            format!("Test not found for {}.", criteria.join(" or ")),
            String::new(),
            "Available UI tests:".to_owned(),
        ];
        lines.extend(tests.iter().map(|test| {
            format!(
                "ID: {}, Name: {}",
                py_str(test.get("id")),
                py_str(test.get("name"))
            )
        }));
        lines.push(String::new());
        lines.push("Available runners/locations:".to_owned());
        lines.extend(available_locations(client).await);
        return Ok(Value::String(lines.join("\n")));
    };
    let ui_test_id = py_str(test.get("id"));
    let details = client
        .request_json(
            Method::GET,
            &["ui_performance", "test", client.project_id(), &ui_test_id],
            &[],
            None,
            false,
        )
        .await?;
    if !py_truthy(&details) {
        return Ok(Value::String(format!(
            "Could not retrieve test details for test ID {ui_test_id}."
        )));
    }
    if let Some(cloud) = overrides.cloud_settings.filter(|value| !value.is_empty())
        && !cloud_location_is_valid(client, cloud).await
    {
        let mut lines = vec![
            format!("❌ Invalid location/cloud_settings: '{cloud}'"),
            String::new(),
            "Available runners/locations:".to_owned(),
        ];
        lines.extend(available_locations(client).await);
        lines.push(String::new());
        lines.push("Please choose a valid location name from the list above.".to_owned());
        return Ok(Value::String(lines.join("\n")));
    }

    let body = if overrides.any() {
        let cloud = match overrides.cloud_settings.filter(|value| !value.is_empty()) {
            Some(wanted) => Some(resolve_cloud(client, wanted).await),
            None => None,
        };
        post_data(&details, Some(&overrides), cloud)
    } else if proceed_with_defaults {
        post_data(&details, None, None)
    } else {
        let env_vars = details
            .get("env_vars")
            .cloned()
            .unwrap_or_else(|| json!({}));
        let env = |key: &str| {
            env_vars
                .get(key)
                .map_or_else(|| "Not set".to_owned(), |value| py_str(Some(value)))
        };
        let mut lines = vec![
            "Current default parameters:".to_owned(),
            format!("  • CPU Quota: {}", env("cpu_quota")),
            format!("  • Memory Quota: {}", env("memory_quota")),
            format!("  • Custom Command: {}", env("custom_cmd")),
            format!(
                "  • Loops: {}",
                details
                    .get("loops")
                    .map_or_else(|| "1".to_owned(), |loops| py_str(Some(loops)))
            ),
            "  • Cloud Settings: Default location".to_owned(),
            String::new(),
            "Available parameters to override:".to_owned(),
            "  • cpu_quota - Set CPU quota for the test runner".to_owned(),
            "  • memory_quota - Set memory quota for the test runner".to_owned(),
            "  • custom_cmd - Set custom command to run with the test".to_owned(),
            "  • loops - Set number of loops to run the test".to_owned(),
            "  • cloud_settings - Set cloud settings name for the test runner".to_owned(),
            String::new(),
            "Available runners/locations:".to_owned(),
        ];
        lines.extend(available_locations(client).await);
        return Ok(Value::String(format!(
            "{}\n\nTo proceed with default configuration, type `Run test with default configuration` or specify any parameters you want to override.",
            lines.join("\n")
        )));
    };

    let response = client
        .request_json(
            Method::POST,
            &["ui_performance", "test", client.project_id(), &ui_test_id],
            &[],
            Some(&body),
            true,
        )
        .await?;
    let result_id = py_str_or_empty(response.get("result_id"));
    let location_used = match overrides.cloud_settings.filter(|value| !value.is_empty()) {
        Some(cloud) => cloud.to_owned(),
        None => body
            .get("common_params")
            .and_then(|params| params.get("location"))
            .filter(|location| py_truthy(location))
            .map_or_else(|| "default".to_owned(), |location| py_str(Some(location))),
    };
    Ok(Value::String(format!(
        "✅ UI test started successfully!\nResult ID: {result_id}\nLocation used: {location_used}\nLink to report: {}/-/performance/ui/results?result_id={result_id}",
        client.display_url()
    )))
}

async fn run_ui_missing_input(client: &CarrierClient) -> Value {
    let Ok(tests) = ui_tests(client).await else {
        return json!({
            "message":"Please provide test ID or test name of your UI test.",
            "parameters":{"test_id":null,"test_name":null},
            "available_tests":"Error fetching available tests.",
            "available_locations":"Error fetching available locations.",
        });
    };
    let available_tests = if tests.is_empty() {
        "No UI tests found.".to_owned()
    } else {
        let mut lines = vec!["Available UI Tests:".to_owned()];
        lines.extend(tests.iter().map(|test| {
            format!(
                "- ID: {}, Name: {}, Runner: {}",
                py_str(test.get("id")),
                py_str(test.get("name")),
                py_str(test.get("runner"))
            )
        }));
        lines.join("\n")
    };
    let location_info = format!(
        "Available runners/locations for cloud_settings:\n{}",
        available_locations(client).await.join("\n")
    );
    json!({
        "message":{"text":format!(
            "Please provide test ID or test name of your UI test.\n\nAvailable UI tests:\n{available_tests}\n\nAvailable runners/locations for cloud_settings:\n{location_info}"
        )},
        "parameters":{"test_id":null,"test_name":null},
    })
}

// ---------------------------------------------------------------------------
// update_ui_test_schedule
// ---------------------------------------------------------------------------

const INVALID_CRON_HELP: &str = "**Cron format should be:** `minute hour day month weekday`\n\n## Valid Examples:\n- `0 2 * * *` - Daily at 2:00 AM\n- `30 14 * * 1` - Every Monday at 2:30 PM  \n- `0 */6 * * *` - Every 6 hours\n- `15 10 1 * *` - First day of every month at 10:15 AM\n- `0 9 * * 1-5` - Weekdays at 9:00 AM\n\n## Format Rules:\n- **Minute:** 0-59\n- **Hour:** 0-23\n- **Day:** 1-31\n- **Month:** 1-12\n- **Weekday:** 0-7 (0 and 7 are Sunday)\n- Use **`*`** for \"any value\"\n- Use **`,`** for multiple values\n- Use **`-`** for ranges\n- Use **`/`** for step values";

fn valid_cron(cron: &str) -> bool {
    let parts = cron.split_whitespace().collect::<Vec<_>>();
    parts.len() == 5
        && parts.iter().all(|part| {
            part.bytes()
                .all(|byte| byte.is_ascii_digit() || matches!(byte, b'*' | b',' | b'/' | b'-'))
        })
}

#[expect(
    clippy::too_many_lines,
    reason = "one SDK tool's conversational steps, kept in its source order"
)]
async fn update_ui_test_schedule(
    client: &CarrierClient,
    arguments: &Map<String, Value>,
) -> ToolResult {
    let test_id = optional_text(arguments, "test_id", MAX_ID_BYTES)?.unwrap_or_default();
    let schedule_name =
        optional_text(arguments, "schedule_name", MAX_TEXT_BYTES)?.unwrap_or_default();
    let cron_timer = optional_text(arguments, "cron_timer", MAX_ID_BYTES)?.unwrap_or_default();
    let blank = |value: &str| value.trim().is_empty();

    if blank(test_id) && blank(schedule_name) && blank(cron_timer) {
        let tests = ui_tests(client).await?;
        if tests.is_empty() {
            return Ok(Value::String("❌ **No UI tests found.**".to_owned()));
        }
        let mut lines = vec![
            "# 📋 Update UI Test Schedule\n".to_owned(),
            "## Available UI Tests:".to_owned(),
        ];
        lines.extend(tests.iter().map(|test| {
            format!(
                "- **ID: {}**, Name: `{}`, Runner: `{}`",
                py_str(test.get("id")),
                py_str(test.get("name")),
                py_str(test.get("runner"))
            )
        }));
        lines.extend(
            [
                "\n## 📝 Instructions:",
                "For updating UI test schedule, please provide me:",
                "- **`test_id`** - The ID of the test you want to update",
                "- **`schedule_name`** - A name for your new schedule",
                "- **`cron_timer`** - Cron expression for timing (e.g., `0 2 * * *` for daily at 2 AM)",
                "\n## 💡 Example:",
                "```",
                "test_id: 42",
                "schedule_name: Daily Morning Test",
                "cron_timer: 0 2 * * *",
                "```",
            ]
            .map(ToOwned::to_owned),
        );
        return Ok(Value::String(lines.join("\n")));
    }
    if blank(test_id) {
        return Ok(Value::String("# ❌ Missing Test ID\n\n**For updating UI test schedule, please provide me:**\n- **`test_id`** - The ID of the test you want to update  \n- **`schedule_name`** - A name for your new schedule\n- **`cron_timer`** - Cron expression for timing\n\nUse the tool without parameters to see available tests.".to_owned()));
    }
    if blank(schedule_name) || blank(cron_timer) {
        let mut lines = vec![
            format!("# ❌ Missing Parameters for Test ID: {test_id}\n"),
            "**Missing parameters:**".to_owned(),
        ];
        if blank(schedule_name) {
            lines.push("- **`schedule_name`**".to_owned());
        }
        if blank(cron_timer) {
            lines.push("- **`cron_timer`**".to_owned());
        }
        lines.extend(
            [
                "\n**For updating UI test schedule, please provide:**",
                "- **`test_id`** ✅ (provided)",
                "- **`schedule_name`** - A name for your new schedule",
                "- **`cron_timer`** - Cron expression for timing (e.g., `0 2 * * *`)",
            ]
            .map(ToOwned::to_owned),
        );
        return Ok(Value::String(lines.join("\n")));
    }
    if !valid_cron(cron_timer) {
        return Ok(Value::String(format!(
            "# ❌ Invalid Cron Timer Format\n\n**Provided:** `{cron_timer}`\n\n{INVALID_CRON_HELP}"
        )));
    }
    let tests = ui_tests(client).await?;
    let test = test_id
        .bytes()
        .all(|byte| byte.is_ascii_digit())
        .then(|| test_id.parse::<u64>().ok())
        .flatten()
        .and_then(|id| {
            tests
                .iter()
                .find(|test| test.get("id").and_then(Value::as_u64) == Some(id))
        });
    let Some(test) = test else {
        let available = tests
            .iter()
            .map(|test| {
                format!(
                    "- ID: {}, Name: {}",
                    py_str(test.get("id")),
                    py_str(test.get("name"))
                )
            })
            .collect::<Vec<_>>();
        return Ok(Value::String(format!(
            "❌ **Test not found for ID: {test_id}**\n\n**Available UI tests:**\n{}",
            available.join("\n")
        )));
    };
    let id = py_str(test.get("id"));
    let details = client
        .request_json(
            Method::GET,
            &["ui_performance", "test", client.project_id(), &id],
            &[],
            None,
            false,
        )
        .await?;
    if !py_truthy(&details) {
        return Ok(Value::String(format!(
            "❌ **Could not retrieve test details for test ID {id}.**"
        )));
    }
    let body = schedule_update_body(&details, schedule_name, cron_timer);
    client
        .request_json(
            Method::PUT,
            &["ui_performance", "test", client.project_id(), &id],
            &[],
            Some(&body),
            true,
        )
        .await?;
    let test_name = test
        .get("name")
        .map_or_else(|| "Unknown".to_owned(), |name| py_str(Some(name)));
    Ok(Value::String(format!(
        "# ✅ UI Test Schedule Updated Successfully!\n\n## Test Information:\n- **Test Name:** `{test_name}`\n- **Test ID:** `{id}`\n\n## New Schedule Added:\n- **Schedule Name:** `{schedule_name}`\n- **Cron Timer:** `{cron_timer}`\n- **Status:** Active ✅\n\n## 🎯 What happens next:\nThe test will now run automatically according to the specified schedule. You can view and manage schedules in the Carrier platform UI.\n\n**Schedule will execute:** Based on cron expression `{cron_timer}`"
    )))
}

/// The SDK's `_parse_and_update_test_data`: the test's own configuration,
/// its existing schedules, and the new active one.
fn schedule_update_body(details: &Value, schedule_name: &str, cron_timer: &str) -> Value {
    let field = |key: &str, default: Value| details.get(key).cloned().unwrap_or(default);
    let mut env_type = json!("");
    let mut test_type = json!("");
    for parameter in details
        .get("test_parameters")
        .and_then(Value::as_array)
        .map(Vec::as_slice)
        .unwrap_or_default()
    {
        let default = parameter
            .get("default")
            .cloned()
            .unwrap_or_else(|| json!(""));
        match parameter.get("name").and_then(Value::as_str) {
            Some("env_type") => env_type = default,
            Some("test_type") => test_type = default,
            _ => {}
        }
    }
    let integrations = details.get("integrations");
    let integration = |key: &str| {
        integrations
            .and_then(|value| value.get(key))
            .cloned()
            .unwrap_or_else(|| json!({}))
    };
    let mut schedules = details
        .get("schedules")
        .and_then(Value::as_array)
        .map(Vec::as_slice)
        .unwrap_or_default()
        .iter()
        .map(|schedule| {
            let field = |key: &str| schedule.get(key).cloned().unwrap_or(Value::Null);
            json!({
                "active":schedule.get("active").cloned().unwrap_or(json!(false)),
                "cron":schedule.get("cron").cloned().unwrap_or_else(|| json!("")),
                "cron_radio":"custom",
                "errors":{},
                "id":field("id"),
                "name":schedule.get("name").cloned().unwrap_or_else(|| json!("")),
                "project_id":field("project_id"),
                "rpc_kwargs":field("rpc_kwargs"),
                "test_id":field("test_id"),
                "test_params":schedule.get("test_params").cloned().unwrap_or_else(|| json!([])),
            })
        })
        .collect::<Vec<_>>();
    schedules.push(json!({
        "active":true,
        "cron":cron_timer,
        "cron_radio":"custom",
        "errors":{},
        "id":null,
        "name":schedule_name,
        "test_params":[],
    }));
    json!({
        "common_params":{
            "aggregation":field("aggregation", json!("max")),
            "cc_env_vars":field("cc_env_vars", json!({})),
            "entrypoint":field("entrypoint", json!("")),
            "env_type":env_type,
            "env_vars":field("env_vars", json!({})),
            "location":field("location", json!("")),
            "loops":field("loops", json!(1)),
            "name":field("name", json!("")),
            "parallel_runners":field("parallel_runners", json!(1)),
            "runner":field("runner", json!("")),
            "source":field("source", json!({})),
            "test_type":test_type,
        },
        "integrations":{"reporters":integration("reporters"),"system":integration("system")},
        "run_test":false,
        "schedules":schedules,
        "test_parameters":[],
    })
}

// ---------------------------------------------------------------------------
// create_ui_test
// ---------------------------------------------------------------------------

const UI_RUNNERS: &str = "Lighthouse-NPM_V12, Lighthouse-Nodejs, Lighthouse-NPM, Lighthouse-NPM_V11, Sitespeed (Browsertime), Sitespeed (New Entrypoint BETA), Sitespeed (New Version BETA), Sitespeed V36";

async fn create_ui_test(client: &CarrierClient, arguments: &Map<String, Value>) -> ToolResult {
    let text = |name: &str| required_text(arguments, name, MAX_TEXT_BYTES);
    // `message` is declared by the SDK and unused by it.
    optional_text(arguments, "message", MAX_DESCRIPTION_TEXT_BYTES)?;
    let name = text("name")?;
    let test_type = text("test_type")?;
    let env_type = text("env_type")?;
    let entrypoint = text("entrypoint")?;
    let runner = text("runner")?;
    let repo = text("repo")?;
    let branch = text("branch")?;
    let username = text("username")?;
    let password = text("password")?;
    let cpu_quota = required_integer(arguments, "cpu_quota")?;
    let memory_quota = required_integer(arguments, "memory_quota")?;
    let parallel_runners = required_integer(arguments, "parallel_runners")?;
    let loops = required_integer(arguments, "loops")?;
    let custom_cmd = optional_text(arguments, "custom_cmd", MAX_TEXT_BYTES)?.unwrap_or_default();

    let mut env_vars =
        json!({"cpu_quota":cpu_quota,"memory_quota":memory_quota,"cloud_settings":{}});
    if !custom_cmd.trim().is_empty()
        && let Some(object) = env_vars.as_object_mut()
    {
        object.insert("custom_cmd".to_owned(), json!(custom_cmd));
    }
    let body = json!({
        "common_params":{
            "name":name,
            "test_type":test_type,
            "env_type":env_type,
            "entrypoint":entrypoint,
            "runner":runner,
            "source":{"name":"git_https","repo":repo,"branch":branch,"username":username,"password":password},
            "env_vars":env_vars,
            "parallel_runners":parallel_runners,
            "cc_env_vars":{},
            "location":"default",
            "loops":loops,
            "aggregation":"max",
        },
        "test_parameters":[],
        "integrations":{},
        "schedules":[],
        "run_test":false,
    });
    let encoded = serde_json::to_string(&body).map_err(|_| ToolFailure::Arguments)?;
    let segments = ["ui_performance", "tests", client.project_id()];
    let response = client
        .send(
            Method::POST,
            &segments,
            &[],
            CarrierBody::Form(&[("data", encoded.as_str())]),
            CarrierAuth::Session,
            true,
        )
        .await?;
    let status = response.status();
    if !status.is_success() {
        let error = status_error(status, true);
        // A definite refusal keeps the SDK's model-facing report; an
        // ambiguous one (5xx, 408, 429) may have created the test.
        if error.code() == CarrierClientErrorCode::UnknownOutcome {
            return Err(error.into());
        }
        let message = format!(
            "Request to {}/api/v1/{} failed with status {}",
            client.display_url(),
            segments.join("/"),
            status.as_u16()
        );
        return Ok(Value::String(if status.as_u16() == 400 {
            format!(
                "# ❌ UI Test Creation Failed - Validation Error\n\n## 🚫 Invalid Input Parameters:\nThe Carrier platform rejected your request due to validation errors.\n\n## 📋 Validation Errors:\n{message}\n\n## 💡 Common Issues:\n- **Test name**: Only letters, numbers, and \"_\" are allowed\n- **Repository URL**: Must be a valid Git repository URL\n- **Runner**: Must be one of the available runner types: {UI_RUNNERS}\n- **Numeric values**: CPU quota, memory quota, parallel runners, and loops must be positive integers\n\n## 🔧 Please fix the validation errors above and try again."
            )
        } else {
            format!(
                "# ❌ UI Test Creation Failed\n\n## 🚫 API Error:\n```\n{message}\n```\n\n## 💡 Please check your parameters and try again."
            )
        }));
    }
    let created = json_success(&response, true)?;
    if !py_truthy(&created) {
        return Ok(Value::String(
            "❌ **Failed to create UI test. Please check your parameters and try again.**"
                .to_owned(),
        ));
    }
    let test_id = if created.is_object() {
        py_str(created.get("id"))
    } else {
        "Unknown".to_owned()
    };
    let custom_line = if custom_cmd.is_empty() {
        String::new()
    } else {
        format!("- **Custom Command:** `{custom_cmd}`")
    };
    Ok(Value::String(format!(
        "# ✅ UI Test Created Successfully!\n\n## Test Information:\n- **Test ID:** `{test_id}`\n- **Name:** `{name}`\n- **Type:** `{test_type}`\n- **Environment:** `{env_type}`\n- **Runner:** `{runner}`\n- **Repository:** `{repo}`\n- **Branch:** `{branch}`\n- **Entry Point:** `{entrypoint}`\n\n## Configuration:\n- **CPU Quota:** {cpu_quota} cores\n- **Memory Quota:** {memory_quota} GB\n- **Parallel Runners:** {parallel_runners}\n- **Loops:** {loops}\n- **Aggregation:** max\n{custom_line}\n\n## 🎯 Next Steps:\n- Your UI test has been created and is ready to run\n- You can execute it using the UI test runner tools\n- Configure schedules and integrations as needed"
    )))
}

// ---------------------------------------------------------------------------
// cancel_ui_test
// ---------------------------------------------------------------------------

fn cancel_pattern() -> Option<&'static Regex> {
    static PATTERN: OnceLock<Option<Regex>> = OnceLock::new();
    PATTERN
        .get_or_init(|| Regex::new(r"cancel\s+ui\s+test\s+(\d+)").ok())
        .as_ref()
}

fn ui_status(report: &Value) -> (Map<String, Value>, String) {
    let status = report
        .get("test_status")
        .and_then(Value::as_object)
        .cloned()
        .unwrap_or_default();
    let current = status
        .get("status")
        .map_or_else(|| "Unknown".to_owned(), |value| py_str(Some(value)));
    (status, current)
}

async fn cancel_ui_test(client: &CarrierClient, arguments: &Map<String, Value>) -> ToolResult {
    let message = required_text(arguments, "message", MAX_DESCRIPTION_TEXT_BYTES)?;
    let lowered = message.to_lowercase();
    let test_id = cancel_pattern()
        .and_then(|pattern| pattern.captures(&lowered))
        .and_then(|captures| captures.get(1))
        .map(|matched| matched.as_str().to_owned());
    match test_id {
        Some(test_id) => cancel_specific_ui_test(client, &test_id).await,
        None => Ok(Value::String(show_cancelable_ui_tests(client).await)),
    }
}

async fn show_cancelable_ui_tests(client: &CarrierClient) -> String {
    let reports = match ui_reports(client).await {
        Ok(reports) => reports,
        Err(error) => return format!("❌ **Error fetching tests:** {error}"),
    };
    if reports.is_empty() {
        return "❌ **No UI tests found.**".to_owned();
    }
    let mut response = String::new();
    for report in &reports {
        let (status, current) = ui_status(report);
        if FINAL_UI_STATES.contains(&current.as_str()) {
            continue;
        }
        let _ = write!(
            response,
            "\n### 🔸 Test ID: `{}`\n- **Name:** `{}`\n- **Status:** `{current}`\n- **Progress:** {}%\n- **Description:** {}\n",
            py_str(report.get("id")),
            report
                .get("name")
                .map_or_else(|| "Unknown".to_owned(), |name| py_str(Some(name))),
            status
                .get("percentage")
                .map_or_else(|| "0".to_owned(), |value| py_str(Some(value))),
            status
                .get("description")
                .map_or_else(String::new, |value| py_str(Some(value))),
        );
    }
    if response.is_empty() {
        return "# ℹ️ No Tests Available for Cancellation\n\nAll UI tests are already in final states (Canceled, Finished, or Failed).\n\n## 🔍 To cancel a specific test:\nUse the command: `Cancel UI test <test_id>`\n\nExample: `Cancel UI test 12345`".to_owned();
    }
    format!(
        "# 🚫 UI Tests Available for Cancellation\n\nThe following tests are currently running and can be canceled:\n\n## 📋 Active Tests:\n{response}\n## 🚫 To cancel a specific test:\nUse the command: `Cancel UI test <test_id>`\n\nExample: `Cancel UI test 12345`"
    )
}

async fn cancel_specific_ui_test(client: &CarrierClient, test_id: &str) -> ToolResult {
    let reports = match ui_reports(client).await {
        Ok(reports) => reports,
        Err(error) => {
            return Ok(Value::String(format!(
                "❌ **Error processing cancellation for test `{test_id}`: {error}**"
            )));
        }
    };
    let Some(target) = reports
        .iter()
        .find(|report| py_str(report.get("id")) == test_id)
    else {
        return Ok(Value::String(format!(
            "❌ **Test with ID `{test_id}` not found.**"
        )));
    };
    let name = target
        .get("name")
        .map_or_else(|| "Unknown".to_owned(), |name| py_str(Some(name)));
    let (_, current) = ui_status(target);
    if FINAL_UI_STATES.contains(&current.as_str()) {
        return Ok(Value::String(format!(
            "# ❌ Cannot Cancel Test\n\n## Test Information:\n- **Test ID:** `{test_id}`\n- **Name:** `{name}`\n- **Current Status:** `{current}`\n\n## 🚫 Reason:\nThis test cannot be canceled because it is already in a final state (`{current}`).\n\nOnly tests with status **not** in `Canceled`, `Finished`, or `Failed` can be canceled."
        )));
    }
    let body = json!({"test_status":{"status":"Canceled","percentage":100,"description":"Test was canceled"}});
    match client
        .request_json(
            Method::PUT,
            &[
                "ui_performance",
                "report_status",
                client.project_id(),
                test_id,
            ],
            &[],
            Some(&body),
            true,
        )
        .await
    {
        Ok(_) => Ok(Value::String(format!(
            "# ✅ UI Test Canceled Successfully!\n\n## Test Information:\n- **Test ID:** `{test_id}`\n- **Name:** `{name}`\n- **Previous Status:** `{current}`\n- **New Status:** `Canceled`\n\n## 🎯 Result:\nThe test has been successfully canceled and will stop executing."
        ))),
        // An ambiguous outcome is not reported as a failure: the cancel may
        // have applied.
        Err(error) if error.code() == CarrierClientErrorCode::UnknownOutcome => Err(error.into()),
        Err(error) => Ok(Value::String(format!(
            "# ❌ Failed to Cancel Test\n\n## Test Information:\n- **Test ID:** `{test_id}`\n- **Name:** `{name}`\n- **Current Status:** `{current}`\n\n## 🚫 Error:\n{error}\n\nPlease check the test ID and try again."
        ))),
    }
}

// ---------------------------------------------------------------------------
// Schemas
// ---------------------------------------------------------------------------

fn string_property(description: &str, max: usize) -> Value {
    json!({"type":"string","maxLength":max,"description":description})
}

fn defaulted_string(description: &str, max: usize, default: &str) -> Value {
    json!({"type":"string","maxLength":max,"default":default,"description":description})
}

fn nullable_string(description: &str, max: usize) -> Value {
    json!({"type":["string","null"],"maxLength":max,"default":null,"description":description})
}

fn object_schema(properties: Value, required: &[&str]) -> Value {
    let mut schema = Map::new();
    schema.insert("type".to_owned(), json!("object"));
    schema.insert("properties".to_owned(), properties);
    schema.insert("additionalProperties".to_owned(), json!(false));
    if !required.is_empty() {
        schema.insert("required".to_owned(), json!(required));
    }
    Value::Object(schema)
}

#[expect(
    clippy::too_many_lines,
    reason = "one schema per SDK tool, kept side by side"
)]
fn schema(kind: CarrierToolKind) -> Value {
    match kind {
        CarrierToolKind::GetTicketList => object_schema(
            json!({
                "board_id":string_property("Board ID from which tickets will be fetched", MAX_ID_BYTES),
                "tag_name":defaulted_string("Tag name from which tickets titles will be fetched", MAX_TEXT_BYTES, ""),
                "status":defaulted_string("Status for tickets titles will be fetched", MAX_TEXT_BYTES, ""),
            }),
            &["board_id"],
        ),
        CarrierToolKind::CreateTicket => object_schema(
            json!({
                "title":string_property("Title of the ticket.", MAX_TEXT_BYTES),
                "description":string_property("Detailed description of the ticket.", MAX_DESCRIPTION_TEXT_BYTES),
                "severity":string_property("Severity level, e.g., 'Critical', 'High', 'Medium', 'Low'.", MAX_TEXT_BYTES),
                "type":string_property("Type of ticket, e.g., 'Activity', 'Bug', 'Task'.", MAX_TEXT_BYTES),
                "engagement":nullable_string("Engagement ID (e.g., f09b73a0-c547-426a-aeef-...)", MAX_TEXT_BYTES),
                "board_id":string_property("ID of the board where the ticket is created.", MAX_ID_BYTES),
                "start_date":string_property("Start date in YYYY-MM-DD format.", MAX_ID_BYTES),
                "end_date":string_property("End date in YYYY-MM-DD format.", MAX_ID_BYTES),
                "external_link":nullable_string("External link for added context.", MAX_TEXT_BYTES),
                "assignee":nullable_string("User to whom the ticket is assigned, e.g. 'Select'", MAX_TEXT_BYTES),
                "tags":{"type":["array","null"],"items":{"type":"string","maxLength":MAX_TEXT_BYTES},"maxItems":256,"default":null,"description":"List of tags to add to the ticket."},
            }),
            &[
                "title",
                "description",
                "severity",
                "type",
                "board_id",
                "start_date",
                "end_date",
            ],
        ),
        CarrierToolKind::GetReports => object_schema(
            json!({
                "name":defaulted_string("Optional parameter. Report name to filter reports", MAX_TEXT_BYTES, ""),
                "tag_name":defaulted_string("Optional parameter. Tag name to filter reports", MAX_TEXT_BYTES, ""),
            }),
            &[],
        ),
        CarrierToolKind::AddTagToReport => object_schema(
            json!({
                "report_id":string_property("Report id to update", MAX_ID_BYTES),
                "tag_name":string_property("Tag name to add to report", MAX_TEXT_BYTES),
            }),
            &["report_id", "tag_name"],
        ),
        CarrierToolKind::GetTests => object_schema(json!({}), &[]),
        CarrierToolKind::GetTestById => object_schema(
            json!({"test_id":string_property("Test id to retrieve", MAX_ID_BYTES)}),
            &["test_id"],
        ),
        CarrierToolKind::RunTestById => object_schema(
            json!({
                "test_id":{"type":"integer","default":null,"description":"Test id to execute. Use test_id if user provide id in int format"},
                "name":{"type":"string","maxLength":MAX_TEXT_BYTES,"default":null,"description":"Test name to execute. Use name if user provide name in str format"},
                "test_parameters":{"type":"array","items":{},"maxItems":256,"default":null,"description":"Test parameters to override. Provide as a list of dictionaries, e.g., [{'vUsers': '5', 'duration': '120'}]. Each dictionary should contain parameter names and their values."},
                "location":{"type":"string","maxLength":MAX_TEXT_BYTES,"default":null,"description":"Location to execute the test. Choose from public_regions, project_regions, or cloud_regions. For cloud_regions, additional parameters may be required."},
                "cloud_settings":{"type":"object","additionalProperties":true,"default":{},"description":"Additional parameters for cloud_regions. Provide as a dictionary, e.g., {'region_name': 'us-west-1', 'instance_type': 't2.large'}. Don't provide this parameter as string! It should be a dictionary!If no changes are needed, respond with 'use default'.Ensure these settings are passed as a valid dictionary not string"},
            }),
            &[],
        ),
        CarrierToolKind::CreateBackendTest => object_schema(
            json!({
                "test_name":string_property("Test name", MAX_TEXT_BYTES),
                "test_type":string_property("Test type", MAX_TEXT_BYTES),
                "env_type":string_property("Env type", MAX_TEXT_BYTES),
                "entrypoint":string_property("Entrypoint for the test (JMeter script path or Gatling simulation path)", MAX_TEXT_BYTES),
                "custom_cmd":string_property("Custom command line to execute the test (e.g., -l /tmp/reports/jmeter.jtl -e -o /tmp/reports/html_report)", MAX_TEXT_BYTES),
                "runner":string_property("Test runner (Gatling or JMeter)", MAX_TEXT_BYTES),
                "source":{"anyOf":[{"type":"object","additionalProperties":{"anyOf":[{"type":"string"},{"type":"null"}]}},{"type":"null"}],"default":null,"description":"Test source configuration (Git repo). The dictionary should include the following keys:\n- 'name' (required): The type of source (e.g., 'git_https').\n- 'repo' (required): The URL of the Git repository.\n- 'branch' (optional): The branch of the repository to use.\n- 'username' (optional): The username for accessing the repository.\n- 'password' (optional): The password or token for accessing the repository."},
                "test_parameters":{"anyOf":[{"type":"array","maxItems":256,"items":{"type":"object","additionalProperties":{"type":"string"}}},{"type":"null"}],"default":null,"description":"Test parameters as a list of dictionaries. Each dictionary should include the following keys:\n- 'name' (required): The name of the parameter (e.g., 'VUSERS').\n- 'default' (required): The value of the parameter (e.g., '5')."},
                "email_integration":{"anyOf":[{"type":"object","additionalProperties":{"anyOf":[{"type":"integer"},{"type":"array","items":{"type":"string"}},{"type":"null"}]}},{"type":"null"}],"default":null,"description":"Email integration configuration. The dictionary should include the following keys:\n- 'integration_id' (required): The ID of the selected email integration (integer).\n- 'recipients' (required): A list of email addresses to receive notifications."},
            }),
            &[
                "test_name",
                "test_type",
                "env_type",
                "entrypoint",
                "custom_cmd",
                "runner",
            ],
        ),
        CarrierToolKind::GetUiReports => object_schema(
            json!({
                "report_id":string_property("UI Report id to retrieve", MAX_ID_BYTES),
                "current_date":string_property("Current date in YYYY-MM-DD format (auto-filled)", MAX_ID_BYTES),
                "name":nullable_string("Optional. Filter reports by name (case-insensitive, partial match)", MAX_TEXT_BYTES),
                "start_time":nullable_string("Start date/time for filtering reports (YYYY-MM-DD or ISO format)", MAX_ID_BYTES),
                "end_time":nullable_string("End date/time for filtering reports (YYYY-MM-DD or ISO format)", MAX_ID_BYTES),
            }),
            &[],
        ),
        CarrierToolKind::GetUiReportById => object_schema(
            json!({"report_id":string_property("UI Report id to retrieve", MAX_ID_BYTES)}),
            &["report_id"],
        ),
        CarrierToolKind::GetUiTests => object_schema(
            json!({
                "name":nullable_string("Optional. Filter tests by name (case-insensitive, partial match)", MAX_TEXT_BYTES),
                "include_schedules":{"type":"boolean","default":false,"description":"Optional. Include test schedules in the response"},
                "include_config":{"type":"boolean","default":false,"description":"Optional. Include detailed configuration in the response"},
            }),
            &[],
        ),
        CarrierToolKind::RunUiTest => object_schema(
            json!({
                "test_id":defaulted_string("Test ID to execute", MAX_ID_BYTES, ""),
                "test_name":defaulted_string("Test name to execute", MAX_TEXT_BYTES, ""),
                "cpu_quota":nullable_string("CPU quota for the test runner", MAX_ID_BYTES),
                "memory_quota":nullable_string("Memory quota for the test runner", MAX_ID_BYTES),
                "cloud_settings":nullable_string("Cloud settings name for the test runner", MAX_TEXT_BYTES),
                "custom_cmd":nullable_string("Custom command to run with the test", MAX_TEXT_BYTES),
                "loops":nullable_string("Number of loops to run the test", MAX_ID_BYTES),
                "proceed_with_defaults":{"type":"boolean","default":false,"description":"Proceed with default configuration. True ONLY when user directly wants to run the test with default parameters. If cpu_quota, memory_quota, cloud_settings, custom_cmd, or loops are provided, proceed_with_defaults must be False"},
            }),
            &[],
        ),
        CarrierToolKind::UpdateUiTestSchedule => object_schema(
            json!({
                "test_id":defaulted_string("Test ID to update schedule for", MAX_ID_BYTES, ""),
                "schedule_name":defaulted_string("Name for the new schedule", MAX_TEXT_BYTES, ""),
                "cron_timer":defaulted_string("Cron expression for schedule timing (e.g., '0 2 * * *')", MAX_ID_BYTES, ""),
            }),
            &[],
        ),
        CarrierToolKind::CreateUiTest => object_schema(
            json!({
                "message":string_property("User request message for creating UI test", MAX_DESCRIPTION_TEXT_BYTES),
                "name":string_property("Test name (e.g., 'My UI Test')", MAX_TEXT_BYTES),
                "test_type":string_property("Test type (e.g., 'performance')", MAX_TEXT_BYTES),
                "env_type":string_property("Environment type (e.g., 'staging')", MAX_TEXT_BYTES),
                "entrypoint":string_property("Entry point file (e.g., 'my_test.js')", MAX_TEXT_BYTES),
                "runner":string_property(&format!("Test runner type. Available runners: {UI_RUNNERS}"), MAX_TEXT_BYTES),
                "repo":string_property("Git repository URL (e.g., 'https://github.com/user/repo.git')", MAX_TEXT_BYTES),
                "branch":string_property("Git branch name (e.g., 'main')", MAX_TEXT_BYTES),
                "username":string_property("Git username", MAX_TEXT_BYTES),
                "password":string_property("Git password", MAX_TEXT_BYTES),
                "cpu_quota":{"type":"integer","description":"CPU quota in cores (e.g., 2)"},
                "memory_quota":{"type":"integer","description":"Memory quota in GB (e.g., 5)"},
                "parallel_runners":{"type":"integer","description":"Number of parallel runners (e.g., 1)"},
                "loops":{"type":"integer","description":"Number of loops (e.g., 1)"},
                "custom_cmd":defaulted_string("Optional custom command (e.g., '--login=\"qwerty\"')", MAX_TEXT_BYTES, ""),
            }),
            &[
                "message",
                "name",
                "test_type",
                "env_type",
                "entrypoint",
                "runner",
                "repo",
                "branch",
                "username",
                "password",
                "cpu_quota",
                "memory_quota",
                "parallel_runners",
                "loops",
            ],
        ),
        CarrierToolKind::CancelUiTest => object_schema(
            json!({"message":string_property("User input message (e.g., 'Cancel UI test' or 'Cancel UI test 12345')", MAX_DESCRIPTION_TEXT_BYTES)}),
            &["message"],
        ),
    }
}

// ---------------------------------------------------------------------------
// Python-compatible projection helpers
// ---------------------------------------------------------------------------

/// `str(value)` for the JSON values Carrier returns; an absent value is
/// Python's `None`.
fn py_str(value: Option<&Value>) -> String {
    match value {
        None | Some(Value::Null) => "None".to_owned(),
        Some(Value::String(text)) => text.clone(),
        Some(Value::Bool(true)) => "True".to_owned(),
        Some(Value::Bool(false)) => "False".to_owned(),
        Some(other) => other.to_string(),
    }
}

/// `response.get(key, "")` rendered with `str`.
fn py_str_or_empty(value: Option<&Value>) -> String {
    value.map_or_else(String::new, |value| py_str(Some(value)))
}

/// Python truthiness of a JSON value.
fn py_truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(value) => *value,
        Value::Number(number) => number.as_f64().is_some_and(|value| value != 0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(values) => !values.is_empty(),
        Value::Object(values) => !values.is_empty(),
    }
}

/// `str(list_of_strings)`: `['a', 'b']`.
fn py_list(values: &[String]) -> String {
    crate::toolkits::families::python_repr::repr_str_list(values)
}

fn keep_fields(source: &Value, fields: &[&str]) -> Map<String, Value> {
    fields
        .iter()
        .filter_map(|field| {
            source
                .get(*field)
                .map(|value| ((*field).to_owned(), value.clone()))
        })
        .collect()
}

/// `[{"name": p["name"], "default": p["default"]} for p in params]`.
fn name_default_pairs(parameters: Option<&Value>) -> Value {
    Value::Array(
        parameters
            .and_then(Value::as_array)
            .map(Vec::as_slice)
            .unwrap_or_default()
            .iter()
            .map(|parameter| {
                json!({
                    "name":parameter.get("name").cloned().unwrap_or(Value::Null),
                    "default":parameter.get("default").cloned().unwrap_or(Value::Null),
                })
            })
            .collect(),
    )
}

// ---------------------------------------------------------------------------
// Argument helpers
// ---------------------------------------------------------------------------

fn required_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, ToolFailure> {
    optional_text(arguments, name, limit)?.ok_or(ToolFailure::Arguments)
}

fn optional_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<Option<&'a str>, ToolFailure> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if value.len() <= limit && !value.contains('\0') => {
            Ok(Some(value))
        }
        Some(_) => Err(ToolFailure::Arguments),
    }
}

fn optional_bool(arguments: &Map<String, Value>, name: &str) -> Result<bool, ToolFailure> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(false),
        Some(Value::Bool(value)) => Ok(*value),
        Some(_) => Err(ToolFailure::Arguments),
    }
}

fn required_integer(arguments: &Map<String, Value>, name: &str) -> Result<i64, ToolFailure> {
    arguments
        .get(name)
        .and_then(Value::as_i64)
        .ok_or(ToolFailure::Arguments)
}

fn optional_object(
    arguments: &Map<String, Value>,
    name: &str,
) -> Result<Option<Map<String, Value>>, ToolFailure> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Object(values)) => Ok(Some(values.clone())),
        Some(_) => Err(ToolFailure::Arguments),
    }
}

fn optional_string_list(
    arguments: &Map<String, Value>,
    name: &str,
) -> Result<Option<Vec<String>>, ToolFailure> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Array(values)) => values
            .iter()
            .map(|value| value.as_str().map(ToOwned::to_owned))
            .collect::<Option<Vec<_>>>()
            .map(Some)
            .ok_or(ToolFailure::Arguments),
        Some(_) => Err(ToolFailure::Arguments),
    }
}

fn validate_argument_size(arguments: &Value) -> Result<(), AdkError> {
    let size = serde_json::to_vec(arguments)
        .map_err(|_| invalid_arguments())?
        .len();
    if size > MAX_ARGUMENT_BYTES {
        return Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::InvalidInput,
            "carrier.arguments.resource_exhausted",
            "the Carrier tool arguments exceed the approved limit",
        ));
    }
    Ok(())
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "carrier.arguments.invalid",
        "the Carrier tool arguments are invalid",
    )
}
