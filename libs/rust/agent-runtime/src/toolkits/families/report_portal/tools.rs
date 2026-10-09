use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{
    MAX_PAGE_NUMBER, ReportFormat, ReportPortalApi, ReportPortalClient, ReportPortalClientError,
};
use super::config::{ReportPortalConfigError, ReportPortalToolkitConfig};

const GET_EXTENDED_LAUNCH_DATA_AS_RAW: &str = "get_extended_launch_data_as_raw";
const GET_EXTENDED_LAUNCH_DATA: &str = "get_extended_launch_data";
const GET_LAUNCH_DETAILS: &str = "get_launch_details";
const GET_ALL_LAUNCHES: &str = "get_all_launches";
const FIND_TEST_ITEM_BY_ID: &str = "find_test_item_by_id";
const GET_TEST_ITEMS_FOR_LAUNCH: &str = "get_test_items_for_launch";
const GET_LOGS_FOR_TEST_ITEMS: &str = "get_logs_for_test_items";
const GET_USER_INFORMATION: &str = "get_user_information";
const GET_DASHBOARD_DATA: &str = "get_dashboard_data";
const MAX_IDENTIFIER_BYTES: usize = 1_024;
const MAX_ARGUMENT_BYTES: usize = 8 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ReportPortalToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for the complete `ReportPortal` read family.
pub(crate) struct ReportPortalToolsetError {
    code: ReportPortalToolsetErrorCode,
}

impl ReportPortalToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> ReportPortalToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for ReportPortalToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ReportPortalToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ReportPortalToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ReportPortalToolsetErrorCode::InvalidConfiguration => {
                "the ReportPortal toolkit configuration is invalid"
            }
            ReportPortalToolsetErrorCode::ResourceExhausted => {
                "the ReportPortal toolkit configuration exceeds its approved limit"
            }
            ReportPortalToolsetErrorCode::UnsupportedSelection => {
                "the selected ReportPortal tool profile is not supported"
            }
            ReportPortalToolsetErrorCode::Client => "the ReportPortal client could not be created",
            ReportPortalToolsetErrorCode::InvalidDefinition => {
                "the ReportPortal ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for ReportPortalToolsetError {}

impl From<ReportPortalConfigError> for ReportPortalToolsetError {
    fn from(source: ReportPortalConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::ReportPortalConfigErrorCode::InvalidConfiguration => {
                    ReportPortalToolsetErrorCode::InvalidConfiguration
                }
                super::config::ReportPortalConfigErrorCode::ResourceExhausted => {
                    ReportPortalToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<ReportPortalClientError> for ReportPortalToolsetError {
    fn from(_: ReportPortalClientError) -> Self {
        Self {
            code: ReportPortalToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for ReportPortalToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: ReportPortalToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build all nine capability-disabled `ReportPortal` read tools.
///
/// Empty selection means the complete source-order catalog. Read grouping is
/// model metadata only; the shared immutable policy still controls admission.
pub(crate) fn build_report_portal_toolset(
    toolkit_name: &str,
    config: ReportPortalToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, ReportPortalToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn ReportPortalApi> = Arc::new(ReportPortalClient::new(config)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), ReportPortalToolsetError> {
    if selected.iter().any(|name| {
        !ReportPortalToolKind::ALL
            .iter()
            .any(|kind| kind.name() == name.as_ref())
    }) {
        return Err(ReportPortalToolsetError {
            code: ReportPortalToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn ReportPortalApi>,
) -> Result<BasicToolset, ReportPortalToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(ReportPortalToolKind::ALL.len());
    for kind in ReportPortalToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(ReportPortalTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "report_portal", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn ReportPortalApi>,
) -> Result<BasicToolset, ReportPortalToolsetError> {
    build_with_api(toolkit_name, selected, policy, client)
}

#[derive(Clone, Copy)]
enum ReportPortalToolKind {
    RawExport,
    ReadableExport,
    LaunchDetails,
    AllLaunches,
    TestItem,
    TestItemsForLaunch,
    LogsForTestItem,
    UserInformation,
    Dashboard,
}

impl ReportPortalToolKind {
    const ALL: [Self; 9] = [
        Self::RawExport,
        Self::ReadableExport,
        Self::LaunchDetails,
        Self::AllLaunches,
        Self::TestItem,
        Self::TestItemsForLaunch,
        Self::LogsForTestItem,
        Self::UserInformation,
        Self::Dashboard,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::RawExport => GET_EXTENDED_LAUNCH_DATA_AS_RAW,
            Self::ReadableExport => GET_EXTENDED_LAUNCH_DATA,
            Self::LaunchDetails => GET_LAUNCH_DETAILS,
            Self::AllLaunches => GET_ALL_LAUNCHES,
            Self::TestItem => FIND_TEST_ITEM_BY_ID,
            Self::TestItemsForLaunch => GET_TEST_ITEMS_FOR_LAUNCH,
            Self::LogsForTestItem => GET_LOGS_FOR_TEST_ITEMS,
            Self::UserInformation => GET_USER_INFORMATION,
            Self::Dashboard => GET_DASHBOARD_DATA,
        }
    }

    const fn description(self) -> &'static str {
        match self {
            Self::RawExport => {
                "Choose this when exact exported launch content is needed for archiving or downstream parsing. launch_id is an opaque ID such as 65f2a91c; format defaults to html and accepts html or pdf. One result object contains format, content_type, encoding, byte_length, and content: UTF-8 for HTML or base64 for PDF. Source bodies are limited to 2 MiB, PDF source to 383 KiB, and the complete result to 512 KiB."
            }
            Self::ReadableExport => {
                "Choose this for management summaries, test-coverage analysis, failure trends, and other natural-language analysis of one launch report. launch_id is an opaque ID such as 65f2a91c. The request always exports HTML, removes markup plus script/style content, decodes common entities, and returns one whitespace-normalized UTF-8 string limited to a 512 KiB result."
            }
            Self::LaunchDetails => {
                "Choose this for root-cause analysis or status metadata about one launch rather than its formatted report. launch_id is an opaque ID such as 65f2a91c. Returns one bounded JSON object describing that launch; it does not return item logs or a paginated launch list."
            }
            Self::AllLaunches => {
                "Choose this to compare testing velocity, stability, or quality trends across a single launch-list page. page_number is the zero-based provider page index, defaults to 0, and is 0 through 10000; for example use 1 only after page.totalPages shows another page. Returns one bounded JSON page object and never follows continuation pages."
            }
            Self::TestItem => {
                "Choose this to inspect one specific test case or step, including flaky-test and historical-failure clues. item_id is an opaque item ID such as 65f2bc10. Returns one bounded JSON object for that item; use get_test_items_for_launch when a launch-wide item page is needed."
            }
            Self::TestItemsForLaunch => {
                "Choose this to summarize outcomes or failure concentration across test items in one launch. launch_id is an opaque ID such as 65f2a91c; page_number is zero-based, defaults to 0, and is 0 through 10000. Returns one bounded JSON page object and never follows continuation pages."
            }
            Self::LogsForTestItem => {
                "Choose this for debugging evidence, error correlation, and source-change clues from one test item's logs. item_id is an opaque item ID such as 65f2bc10; page_number is zero-based, defaults to 0, and is 0 through 10000. Returns one bounded JSON log-page object and never follows continuation pages."
            }
            Self::UserInformation => {
                "Choose this for user identity/activity context used in assignment, workload, or personalized report analysis. username is the exact login name, for example qa.lead. Returns one bounded JSON user object; it does not list or search users."
            }
            Self::Dashboard => {
                "Choose this for KPI, overall-health, executive-summary, or test-planning analysis already assembled in one dashboard. dashboard_id is an opaque ID such as 65f2d3e8. Returns one bounded JSON dashboard object; it does not fetch launch exports or raw logs."
            }
        }
    }
}

struct ReportPortalTool {
    kind: ReportPortalToolKind,
    client: Arc<dyn ReportPortalApi>,
    description: Box<str>,
}

impl ReportPortalTool {
    fn new(
        kind: ReportPortalToolKind,
        toolkit_name: &str,
        client: Arc<dyn ReportPortalApi>,
    ) -> Self {
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
impl Tool for ReportPortalTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
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
        let result = match self.kind {
            ReportPortalToolKind::RawExport => {
                reject_unknown_keys(arguments, &["launch_id", "format"])?;
                self.client
                    .get_extended_launch_data_as_raw(
                        identifier(arguments, "launch_id")?,
                        optional_format(arguments)?,
                    )
                    .await
            }
            ReportPortalToolKind::ReadableExport => {
                reject_unknown_keys(arguments, &["launch_id"])?;
                self.client
                    .get_extended_launch_data(identifier(arguments, "launch_id")?)
                    .await
            }
            ReportPortalToolKind::LaunchDetails => {
                reject_unknown_keys(arguments, &["launch_id"])?;
                self.client
                    .get_launch_details(identifier(arguments, "launch_id")?)
                    .await
            }
            ReportPortalToolKind::AllLaunches => {
                reject_unknown_keys(arguments, &["page_number"])?;
                self.client
                    .get_all_launches(optional_page_number(arguments)?)
                    .await
            }
            ReportPortalToolKind::TestItem => {
                reject_unknown_keys(arguments, &["item_id"])?;
                self.client
                    .find_test_item_by_id(identifier(arguments, "item_id")?)
                    .await
            }
            ReportPortalToolKind::TestItemsForLaunch => {
                reject_unknown_keys(arguments, &["launch_id", "page_number"])?;
                self.client
                    .get_test_items_for_launch(
                        identifier(arguments, "launch_id")?,
                        optional_page_number(arguments)?,
                    )
                    .await
            }
            ReportPortalToolKind::LogsForTestItem => {
                reject_unknown_keys(arguments, &["item_id", "page_number"])?;
                self.client
                    .get_logs_for_test_items(
                        identifier(arguments, "item_id")?,
                        optional_page_number(arguments)?,
                    )
                    .await
            }
            ReportPortalToolKind::UserInformation => {
                reject_unknown_keys(arguments, &["username"])?;
                self.client
                    .get_user_information(identifier(arguments, "username")?)
                    .await
            }
            ReportPortalToolKind::Dashboard => {
                reject_unknown_keys(arguments, &["dashboard_id"])?;
                self.client
                    .get_dashboard_data(identifier(arguments, "dashboard_id")?)
                    .await
            }
        };
        result.map_err(ReportPortalClientError::into_adk)
    }
}

fn schema_for(kind: ReportPortalToolKind) -> Value {
    match kind {
        ReportPortalToolKind::RawExport => json!({
            "type": "object",
            "properties": {
                "launch_id": identifier_property("Opaque launch ID to export, for example `65f2a91c`. Use the ID returned in a launch page or launch details result."),
                "format": {
                    "type": ["string", "null"],
                    "enum": ["html", "pdf", null],
                    "default": "html",
                    "description": "Export format. Omit, pass null, or pass `html` for UTF-8 HTML; pass `pdf` for base64 PDF. The result object reports the selected format, content type, encoding, original byte length, and bounded content."
                }
            },
            "required": ["launch_id"],
            "additionalProperties": false
        }),
        ReportPortalToolKind::ReadableExport => id_schema(
            "launch_id",
            "Opaque launch ID whose exported HTML should be converted to readable text, for example `65f2a91c`. Use the ID returned in a launch page or launch details result.",
        ),
        ReportPortalToolKind::LaunchDetails => id_schema(
            "launch_id",
            "Opaque launch ID to inspect, for example `65f2a91c`. Use an ID returned by get_all_launches.",
        ),
        ReportPortalToolKind::AllLaunches => page_schema(None, None),
        ReportPortalToolKind::TestItem => id_schema(
            "item_id",
            "Opaque test-item ID to inspect, for example `65f2bc10`. Use an ID returned by get_test_items_for_launch.",
        ),
        ReportPortalToolKind::TestItemsForLaunch => page_schema(
            Some("launch_id"),
            Some(
                "Opaque launch ID whose test-item page should be read, for example `65f2a91c`. Use an ID returned by get_all_launches.",
            ),
        ),
        ReportPortalToolKind::LogsForTestItem => page_schema(
            Some("item_id"),
            Some(
                "Opaque test-item ID whose log page should be read, for example `65f2bc10`. Use an ID returned by get_test_items_for_launch.",
            ),
        ),
        ReportPortalToolKind::UserInformation => id_schema(
            "username",
            "Exact ReportPortal login name to read, for example `qa.lead`. This is not a display-name search.",
        ),
        ReportPortalToolKind::Dashboard => id_schema(
            "dashboard_id",
            "Opaque dashboard ID to read, for example `65f2d3e8`. Use an ID previously returned by ReportPortal.",
        ),
    }
}

fn identifier_property(description: &str) -> Value {
    json!({
        "type": "string",
        "minLength": 1,
        "maxLength": MAX_IDENTIFIER_BYTES,
        "description": description
    })
}

fn id_schema(name: &str, description: &str) -> Value {
    let mut properties = Map::new();
    properties.insert(name.to_owned(), identifier_property(description));
    json!({
        "type": "object",
        "properties": properties,
        "required": [name],
        "additionalProperties": false
    })
}

fn page_schema(identifier: Option<&str>, identifier_description: Option<&str>) -> Value {
    let mut properties = Map::new();
    if let (Some(name), Some(description)) = (identifier, identifier_description) {
        properties.insert(name.to_owned(), identifier_property(description));
    }
    properties.insert(
        "page_number".to_owned(),
        json!({
            "type": ["integer", "null"],
            "minimum": 0,
            "maximum": MAX_PAGE_NUMBER,
            "default": 0,
            "description": "Zero-based page index from 0 through 10000. Omit or pass null for the first page (0); when the returned page.totalPages is greater than 1, pass 1 for the next page. Exactly one page is returned."
        }),
    );
    json!({
        "type": "object",
        "properties": properties,
        "required": identifier.into_iter().collect::<Vec<_>>(),
        "additionalProperties": false
    })
}

fn validate_argument_size(arguments: &Value) -> Result<(), AdkError> {
    if serde_json::to_vec(arguments)
        .map_err(|_| invalid_arguments())?
        .len()
        > MAX_ARGUMENT_BYTES
    {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> Result<(), AdkError> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn identifier<'a>(arguments: &'a Map<String, Value>, name: &str) -> Result<&'a str, AdkError> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .filter(|value| {
            !value.trim().is_empty()
                && value.len() <= MAX_IDENTIFIER_BYTES
                && !value.chars().any(char::is_control)
        })
        .ok_or_else(invalid_arguments)
}

fn optional_format(arguments: &Map<String, Value>) -> Result<ReportFormat, AdkError> {
    match arguments.get("format") {
        None | Some(Value::Null) => Ok(ReportFormat::Html),
        Some(Value::String(value)) if value == "html" => Ok(ReportFormat::Html),
        Some(Value::String(value)) if value == "pdf" => Ok(ReportFormat::Pdf),
        _ => Err(invalid_arguments()),
    }
}

fn optional_page_number(arguments: &Map<String, Value>) -> Result<u64, AdkError> {
    match arguments.get("page_number") {
        None | Some(Value::Null) => Ok(0),
        Some(value) => value
            .as_u64()
            .filter(|page| *page <= MAX_PAGE_NUMBER)
            .ok_or_else(invalid_arguments),
    }
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "report_portal.arguments.invalid",
        "the ReportPortal tool arguments are invalid",
    )
}
