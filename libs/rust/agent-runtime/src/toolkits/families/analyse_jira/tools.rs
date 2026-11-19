//! The `analyse_jira` tools: bulk extraction of Jira issues (with their
//! status history) into datasets in an artifact bucket, and flow metrics
//! calculated over those datasets.

use std::fmt::Write as _;
use std::sync::Arc;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use chrono::{NaiveDate, Utc};
use serde_json::{Map, Value, json};

use crate::toolkits::families::analysis_core::csv;
use crate::toolkits::families::analysis_core::metrics::{
    Aggregate, Metric, Request, calculate as calculate_metric,
};
use crate::toolkits::families::analysis_core::table::Table;
use crate::toolkits::families::analysis_core::writer;
use crate::toolkits::families::attachment_host::AttachmentHost;
use crate::toolkits::families::integer_like::integer_like;
use crate::toolkits::families::jira::client::JiraClient;
use crate::toolkits::invocation::admit_materialized_toolset;
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::catalogue::tool_args_schema;
use super::config::{
    AnalyseJiraConfig, AnalyseJiraError, AnalyseJiraErrorCode, AnalysisSettings, project_keys,
};
use super::dataset::{ClosedBasedOn, Rules, build};
use super::extract::{Failure, JiraReader, Outcome, Pace, Query, access, extract, filter_problem};
use super::overview::{
    FIELDS_SAMPLE, fields_overview, projects_overview, sprints as read_sprints, statuses,
};

const MAX_DESCRIPTION_BYTES: usize = 1_000;
const MAX_ARGUMENT_BYTES: usize = 64 * 1_024;
/// The most rows a work-items dataset has.
const MAX_DATASET_ROWS: usize = 250_000;
/// The most rows a dataset is read back with: what a write can produce, with
/// room for a file that was extended by hand.
const MAX_READ_ROWS: usize = 300_000;
const MAX_DATASET_BYTES: usize = 32 * 1_024 * 1_024;
const PREVIEW_ROWS: usize = 40;
const MAX_FIELD_ROWS: usize = 100;
const MAX_GROUP_COLUMNS: usize = 16;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum Kind {
    NumberOfIssues,
    Issues,
    FieldsOverview,
    Sprints,
    Statuses,
    Metrics,
}

impl Kind {
    /// The order the catalogue lists the tools in.
    pub(super) const ALL: [Self; 6] = [
        Self::NumberOfIssues,
        Self::Issues,
        Self::FieldsOverview,
        Self::Sprints,
        Self::Statuses,
        Self::Metrics,
    ];

    pub(super) const fn name(self) -> &'static str {
        match self {
            Self::NumberOfIssues => "get_number_off_all_issues",
            Self::Issues => "get_jira_issues",
            Self::FieldsOverview => "get_jira_fields_overview",
            Self::Sprints => "get_jira_sprints",
            Self::Statuses => "get_jira_statuses",
            Self::Metrics => "calculate_jira_metrics",
        }
    }

    /// Whether the tool reads or writes the project's artifact store.
    const fn needs_store(self) -> bool {
        matches!(self, Self::Issues | Self::Sprints | Self::Metrics)
    }

    const fn description(self) -> &'static str {
        match self {
            Self::NumberOfIssues => {
                "Count the issues updated on or after after_date in each Jira project (all visible projects, or project_keys). Returns projects (key, name, issues_count) and projects_summary."
            }
            Self::Issues => {
                "Extract issues with their status history from Jira into a work-items dataset (CSV) and a statuses mapping (CSV) in the artifact bucket. Closed issues are decided by status (closed_issues_based_on 1) or by resolution date (2); open ones are the rest; defect types are listed again as defects. Write nothing and say why when more than max_issues would be read. Then use calculate_jira_metrics, or data_analysis on the files."
            }
            Self::FieldsOverview => {
                "Show how much each Jira field is filled in for the issues of some projects updated on or after updated_after: per field, on how many issues of each project it has a value. Reads at most 2000 issues."
            }
            Self::Sprints => {
                "Extract the sprints of the scrum boards of some projects into a sprints dataset (CSV) in the artifact bucket: name, state, start, end, complete and activated dates, goal."
            }
            Self::Statuses => "List every status of the Jira instance with its status category.",
            Self::Metrics => {
                "Calculate a flow metric over the datasets get_jira_issues wrote: lead_time, cycle_time, life_time (days), throughput (items closed per day of the month), flow_efficiency, flow_load, or the bundles closed_items, open_items, queue and flow. Groups by project, issue type, team (and month where the metric is monthly) unless group_by says otherwise. Writes the result as a CSV and shows its first rows."
            }
        }
    }

    const fn is_read_only(self) -> bool {
        !self.needs_store()
    }
}

/// Every tool this family serves, with whether it needs the run's artifact
/// store (`materialize::omitted_selection` reads it for the run's notice).
pub(crate) fn tool_catalog() -> Vec<(&'static str, bool)> {
    Kind::ALL
        .iter()
        .map(|kind| (kind.name(), kind.needs_store()))
        .collect()
}

/// Build the family. Empty selection means every tool the run can serve: the
/// ones that need the artifact store only when the host lends it.
pub(crate) fn build_analyse_jira_toolset(
    toolkit_name: &str,
    config: AnalyseJiraConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    attachments: Option<AttachmentHost>,
) -> Result<BasicToolset, AnalyseJiraError> {
    let selected = config.jira.selected_tools().to_vec();
    let kinds = served_selection(&selected, attachments.is_some())?;
    if attachments.is_none() {
        tracing::warn!(
            event = "agent_toolkit_tools_skipped",
            reason_code = "attachment_host_unavailable",
            toolkit_type = "analyse_jira",
            toolkit_name,
            "the Jira analysis tools that write datasets were omitted: this run lends no artifact store"
        );
    }
    let client = JiraClient::new(config.jira)
        .map_err(|_| AnalyseJiraError::new(AnalyseJiraErrorCode::Client))?;
    admit_kinds(
        toolkit_name,
        &kinds,
        policy,
        &Arc::new(Runner::new(Arc::new(client), config.analysis, attachments)),
    )
}

pub(super) fn served_selection(
    selected: &[Box<str>],
    has_store: bool,
) -> Result<Vec<Kind>, AnalyseJiraError> {
    if selected
        .iter()
        .any(|name| !Kind::ALL.iter().any(|kind| kind.name() == name.as_ref()))
    {
        return Err(unsupported_selection());
    }
    let kinds: Vec<Kind> = Kind::ALL
        .into_iter()
        .filter(|kind| {
            (has_store || !kind.needs_store())
                && (selected.is_empty() || selected.iter().any(|name| name.as_ref() == kind.name()))
        })
        .collect();
    if kinds.is_empty() {
        return Err(unsupported_selection());
    }
    Ok(kinds)
}

const fn unsupported_selection() -> AnalyseJiraError {
    AnalyseJiraError::new(AnalyseJiraErrorCode::UnsupportedSelection)
}

fn admit_kinds(
    toolkit_name: &str,
    kinds: &[Kind],
    policy: &Arc<ToolAdmissionPolicy>,
    runner: &Arc<Runner>,
) -> Result<BasicToolset, AnalyseJiraError> {
    let tools = kinds
        .iter()
        .map(|kind| {
            Arc::new(AnalyseTool::new(*kind, toolkit_name, Arc::clone(runner))) as Arc<dyn Tool>
        })
        .collect();
    admit_materialized_toolset(toolkit_name, "analyse_jira", policy, tools).map_err(Into::into)
}

/// The tools' shared state.
pub(super) struct Runner {
    client: Arc<dyn JiraReader>,
    settings: AnalysisSettings,
    attachments: Option<AttachmentHost>,
    pub(super) page_size: usize,
    pub(super) retry_delay: Duration,
    /// The most bytes one dataset file may have.
    pub(super) max_bytes: usize,
}

impl Runner {
    pub(super) fn new(
        client: Arc<dyn JiraReader>,
        settings: AnalysisSettings,
        attachments: Option<AttachmentHost>,
    ) -> Self {
        Self {
            client,
            settings,
            attachments,
            page_size: Pace::DEFAULT_PAGE_SIZE,
            retry_delay: Pace::DEFAULT_RETRY_DELAY,
            max_bytes: MAX_DATASET_BYTES,
        }
    }

    fn pace(&self) -> Pace {
        Pace::new(self.page_size, self.retry_delay)
    }

    fn store(&self) -> Result<&AttachmentHost, Failure> {
        self.attachments.as_ref().ok_or_else(|| {
            Failure::Told(
                "This run has no artifact store, so no dataset can be written.".to_owned(),
            )
        })
    }

    async fn run(&self, kind: Kind, arguments: &Map<String, Value>) -> Outcome<Value> {
        let pace = self.pace();
        match kind {
            Kind::NumberOfIssues => self.number_of_issues(&pace, arguments).await,
            Kind::Issues => self.issues(&pace, arguments).await,
            Kind::FieldsOverview => self.fields_overview(&pace, arguments).await,
            Kind::Sprints => self.sprints(&pace, arguments).await,
            Kind::Statuses => self.statuses(&pace).await,
            Kind::Metrics => self.metrics(arguments).await,
        }
    }

    /// The project keys a call names, or the toolkit's; `required` says an
    /// empty answer is a sentence for the model.
    fn keys(&self, arguments: &Map<String, Value>, required: bool) -> Outcome<Vec<String>> {
        let keys = match optional_text(arguments, "project_keys")? {
            Some(text) if !text.trim().is_empty() => project_keys(text).map_err(|()| {
                told("project_keys must be project keys (letters, digits and underscores) separated by commas.")
            })?,
            _ => self.settings.project_keys.clone(),
        };
        if required && keys.is_empty() {
            return Err(told(
                "No project keys: name them in project_keys, or set the toolkit's project keys.",
            ));
        }
        Ok(keys)
    }

    async fn number_of_issues(
        &self,
        pace: &Pace,
        arguments: &Map<String, Value>,
    ) -> Outcome<Value> {
        let after = date(arguments, "after_date")?;
        let keys = self.keys(arguments, false)?;
        access(self.client.as_ref()).await?;
        let overview = projects_overview(self.client.as_ref(), pace, &keys, after).await?;
        let total: usize = overview.projects.iter().map(|project| project.issues).sum();
        let exact = overview.projects.iter().all(|project| project.exact);
        let projects: Vec<Value> = overview
            .projects
            .iter()
            .map(|project| {
                let mut entry = json!({
                    "key": project.key,
                    "name": project.name,
                    "issues_count": project.issues,
                });
                if !project.exact {
                    entry["issues_count_is_a_lower_bound"] = Value::Bool(true);
                }
                entry
            })
            .collect();
        Ok(json!({
            "projects": projects,
            "projects_summary": {
                "after_date": after.format("%Y-%m-%d").to_string(),
                "projects_count": overview.projects.len(),
                "issues_count": total,
                "issues_count_is_exact": exact,
                "unavailable_projects": overview.unavailable,
            },
        }))
    }

    #[allow(clippy::too_many_lines)] // One extraction: read, build, write, report.
    async fn issues(&self, pace: &Pace, arguments: &Map<String, Value>) -> Outcome<Value> {
        let based_on = match required_integer(arguments, "closed_issues_based_on")? {
            1 => ClosedBasedOn::Status,
            2 => ClosedBasedOn::Resolution,
            _ => {
                return Err(told(
                    "closed_issues_based_on must be 1 (closed by status) or 2 (closed by resolution date).",
                ));
            }
        };
        let resolved_after = date(arguments, "resolved_after")?;
        let updated_after = date(arguments, "updated_after")?;
        let created_after = date(arguments, "created_after")?;
        let filter = optional_text(arguments, "add_filter")?.unwrap_or_default();
        if let Some(problem) = filter_problem(filter) {
            return Err(told(problem));
        }
        if based_on == ClosedBasedOn::Status && self.settings.closed_status.is_empty() {
            return Err(told(
                "closed_issues_based_on 1 needs the toolkit's closed_status: set it, or use 2.",
            ));
        }
        let keys = self.keys(arguments, true)?;
        let store = self.store()?;
        let query = Query {
            projects: &keys,
            based_on,
            closed_status: &self.settings.closed_status,
            resolved_after,
            updated_after,
            add_filter: filter,
        };
        let issues = extract(
            self.client.as_ref(),
            pace,
            &self.settings.custom_fields,
            &query,
            self.settings.max_issues,
        )
        .await?;
        if issues.is_empty() {
            return Ok(Value::String(format!(
                "No Jira issues of {} matched, so nothing was written.",
                keys.join(", ")
            )));
        }
        let custom: Vec<String> = self
            .settings
            .custom_fields
            .iter()
            .map(|(column, _)| column.clone())
            .collect();
        let rules = Rules {
            closed_based_on: based_on,
            closed_status: &self.settings.closed_status,
            resolved_after,
            created_after,
            defect_types: &self.settings.defect_types,
            custom_columns: &custom,
            max_rows: MAX_DATASET_ROWS,
        };
        let built = build(&issues, &rules, Utc::now().naive_utc()).map_err(|error| {
            told(format!(
                "The dataset would have more than {} rows, so nothing was written. Narrow the projects, the dates or add_filter.",
                error.limit
            ))
        })?;
        if built.rows == 0 {
            return Ok(Value::String(format!(
                "All {} issues found were resolved before {}, so nothing was written.",
                built.before_window,
                resolved_after.format("%Y-%m-%d")
            )));
        }
        let label = file_label(&keys);
        // Both files are encoded and checked before the first is stored, so a
        // bound that is hit never leaves one behind.
        let items_file =
            writer::prepare(&built.work_items, self.max_bytes).map_err(Failure::Told)?;
        let statuses_file =
            writer::prepare(&built.statuses, self.max_bytes).map_err(Failure::Told)?;
        let items = writer::store(
            store,
            &self.settings.bucket,
            &format!("data_work_items_{label}"),
            items_file,
        )
        .await
        .map_err(Failure::Told)?;
        let statuses = writer::store(
            store,
            &self.settings.bucket,
            &format!("map_statuses_{label}"),
            statuses_file,
        )
        .await
        .map_err(|sentence| {
            told(format!(
                "{sentence} The work-items file {} was written; use it, or delete it and run again.",
                items.path()
            ))
        })?;
        let mut text = format!(
            "Data has been extracted successfully.\nWork items: {} ({} rows from {} issues: {} closed, {} open, {} also as defects, {} waiting for release)\nStatuses: {} ({} statuses)",
            items.path(),
            built.rows,
            built.issues,
            built.closed,
            built.open,
            built.defects,
            built.released,
            statuses.path(),
            built.statuses.rows.len(),
        );
        if built.before_window > 0 {
            let _ = write!(
                text,
                "\n{} closed issues resolved before {} were left out.",
                built.before_window,
                resolved_after.format("%Y-%m-%d")
            );
        }
        if built.incomplete_changelogs > 0 {
            let _ = write!(
                text,
                "\n{} issues have an incomplete status history (this Jira would not give the rest of a long changelog), so their times in status may be short.",
                built.incomplete_changelogs
            );
        }
        text.push_str("\nNext: calculate_jira_metrics over these files, or data_analysis.");
        Ok(Value::String(text))
    }

    async fn fields_overview(&self, pace: &Pace, arguments: &Map<String, Value>) -> Outcome<Value> {
        let after = date(arguments, "updated_after")?;
        let keys = self.keys(arguments, true)?;
        access(self.client.as_ref()).await?;
        let limit = self.settings.max_issues.min(FIELDS_SAMPLE);
        let overview = fields_overview(self.client.as_ref(), pace, &keys, after, limit).await?;
        let fields: Vec<Value> = overview
            .fields
            .iter()
            .take(MAX_FIELD_ROWS)
            .map(|field| {
                let by_project: Map<String, Value> = field
                    .by_project
                    .iter()
                    .map(|(project, count)| {
                        let issues = overview
                            .per_project
                            .get(project)
                            .copied()
                            .unwrap_or(1)
                            .max(1);
                        #[allow(clippy::cast_precision_loss)] // Counts of issues fit an f64.
                        let share = *count as f64 / issues as f64 * 100.0;
                        (
                            project.clone(),
                            Value::String(format!("{count} ({share:.1}%)")),
                        )
                    })
                    .collect();
                json!({
                    "field": field.name,
                    "id": field.id,
                    "used_on": field.total,
                    "by_project": by_project,
                })
            })
            .collect();
        Ok(json!({
            "issues_analysed": overview.issues,
            "sample_was_cut": overview.truncated,
            "issues_per_project": overview.per_project,
            "fields": fields,
            "fields_not_shown": overview.fields.len().saturating_sub(MAX_FIELD_ROWS),
        }))
    }

    async fn sprints(&self, pace: &Pace, arguments: &Map<String, Value>) -> Outcome<Value> {
        let keys = self.keys(arguments, true)?;
        let store = self.store()?;
        access(self.client.as_ref()).await?;
        let found = read_sprints(self.client.as_ref(), pace, &keys).await?;
        let skipped = if found.skipped.is_empty() {
            String::new()
        } else {
            format!("\nSkipped: {}", found.skipped.join("; "))
        };
        if found.table.is_empty() {
            return Ok(Value::String(format!(
                "No sprints were found on the scrum boards of {} ({} boards), so nothing was written.{skipped}",
                keys.join(", "),
                found.boards
            )));
        }
        let stored = writer::write_dataset(
            store,
            &self.settings.bucket,
            &format!("data_sprints_{}", file_label(&keys)),
            &found.table,
            self.max_bytes,
        )
        .await
        .map_err(Failure::Told)?;
        Ok(Value::String(format!(
            "Data has been extracted successfully.\nSprints: {} ({} sprints from {} boards){skipped}",
            stored.path(),
            found.table.rows.len(),
            found.boards
        )))
    }

    async fn statuses(&self, pace: &Pace) -> Outcome<Value> {
        access(self.client.as_ref()).await?;
        let statuses = statuses(self.client.as_ref(), pace).await?;
        Ok(Value::Array(
            statuses
                .into_iter()
                .map(|(name, category)| json!({"name": name, "category": category}))
                .collect(),
        ))
    }

    async fn metrics(&self, arguments: &Map<String, Value>) -> Outcome<Value> {
        let name = required_text(arguments, "metric")?;
        let metric = Metric::parse(name).ok_or_else(|| {
            told(format!(
                "metric must be one of: {}.",
                Metric::ALL.map(Metric::name).join(", ")
            ))
        })?;
        let request = Request {
            group_by: string_list(arguments, "group_by")?,
            aggregate_by: aggregates(arguments)?,
        };
        let store = self.store()?;
        let items = read_dataset(store, required_text(arguments, "work_items_file")?).await?;
        let mapping = match optional_text(arguments, "statuses_file")? {
            Some(path) if !path.trim().is_empty() => Some(read_dataset(store, path.trim()).await?),
            _ => None,
        };
        let frame = calculate_metric(metric, items, mapping.as_ref(), &request)
            .map_err(|error| told(error.sentence()))?;
        let table = frame.to_table();
        if table.is_empty() {
            return Ok(Value::String(format!(
                "{}: the dataset has no items this metric counts, so there is no result.",
                metric.name()
            )));
        }
        let stored = writer::write_dataset(
            store,
            &self.settings.bucket,
            &format!("jira_metrics_{}", metric.name()),
            &table,
            self.max_bytes,
        )
        .await
        .map_err(Failure::Told)?;
        let mut preview = Table::new(&table.columns);
        preview.rows = table.rows.iter().take(PREVIEW_ROWS).cloned().collect();
        let shown = csv::write(&preview, usize::MAX).unwrap_or_default();
        let more = table.rows.len().saturating_sub(PREVIEW_ROWS);
        Ok(Value::String(format!(
            "{}: {} groups, written to {}\n{}{}",
            metric.name(),
            frame.len(),
            stored.path(),
            String::from_utf8_lossy(&shown),
            if more > 0 {
                format!("... and {more} more rows in the file.")
            } else {
                String::new()
            }
        )))
    }
}

/// Read a dataset back from the artifact store.
async fn read_dataset(store: &AttachmentHost, path: &str) -> Outcome<Table> {
    let file = store
        .read_artifact(path, MAX_DATASET_BYTES as u64)
        .await
        .map_err(|error| Failure::Told(error.sentence()))?;
    csv::read(&file.bytes, MAX_READ_ROWS).map_err(|error| {
        Failure::Told(match error {
            csv::CsvError::TooManyRows => {
                format!("The file has more than {MAX_READ_ROWS} rows, so it was not read.")
            }
            csv::CsvError::TooLarge | csv::CsvError::Malformed => {
                "The file is not a readable CSV with a header row.".to_owned()
            }
        })
    })
}

/// The part of a dataset file name that names the projects.
fn file_label(keys: &[String]) -> String {
    let joined = keys
        .iter()
        .map(|key| key.to_ascii_lowercase().replace('_', "-"))
        .collect::<Vec<_>>()
        .join("-");
    let label: String = joined.chars().take(40).collect();
    let label = label.trim_matches('-');
    if label.is_empty() {
        "projects".to_owned()
    } else {
        label.to_owned()
    }
}

pub(super) struct AnalyseTool {
    kind: Kind,
    runner: Arc<Runner>,
    description: Box<str>,
}

impl AnalyseTool {
    pub(super) fn new(kind: Kind, toolkit_name: &str, runner: Arc<Runner>) -> Self {
        let description = format!("Toolkit: {toolkit_name}\n{}", kind.description());
        Self {
            kind,
            runner,
            description: description
                .chars()
                .take(MAX_DESCRIPTION_BYTES)
                .collect::<String>()
                .into_boxed_str(),
        }
    }

    fn schema(&self) -> Value {
        tool_args_schema(self.kind.name()).unwrap_or_else(|| json!({"type": "object"}))
    }
}

#[async_trait]
impl Tool for AnalyseTool {
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
        Some(self.schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        if serde_json::to_vec(&arguments)
            .map_err(|_| invalid_arguments())?
            .len()
            > MAX_ARGUMENT_BYTES
        {
            return Err(invalid_arguments());
        }
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        let schema = self.schema();
        let allowed = schema
            .get("properties")
            .and_then(Value::as_object)
            .ok_or_else(invalid_arguments)?;
        if arguments.keys().any(|key| !allowed.contains_key(key)) {
            return Err(invalid_arguments());
        }
        match self.runner.run(self.kind, arguments).await {
            Ok(value) => Ok(value),
            Err(Failure::Told(sentence)) => Ok(Value::String(sentence)),
            Err(Failure::Refused(message)) => Ok(Value::String(format!(
                "Jira refused the request: {message}"
            ))),
            Err(Failure::Client(error)) => Err(error.into_adk()),
        }
    }
}

fn told(sentence: impl Into<String>) -> Failure {
    Failure::Told(sentence.into())
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "analyse_jira.arguments.invalid",
        "the Jira analysis tool arguments are invalid",
    )
}

fn required_text<'a>(arguments: &'a Map<String, Value>, name: &str) -> Outcome<&'a str> {
    match arguments.get(name) {
        Some(Value::String(value)) if !value.trim().is_empty() => Ok(value),
        _ => Err(told(format!("{name} is required."))),
    }
}

/// An optional text; null means absent.
fn optional_text<'a>(arguments: &'a Map<String, Value>, name: &str) -> Outcome<Option<&'a str>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => Ok(Some(value)),
        Some(_) => Err(told(format!("{name} must be text."))),
    }
}

fn required_integer(arguments: &Map<String, Value>, name: &str) -> Outcome<i64> {
    arguments
        .get(name)
        .and_then(integer_like)
        .ok_or_else(|| told(format!("{name} must be a whole number.")))
}

fn date(arguments: &Map<String, Value>, name: &str) -> Outcome<NaiveDate> {
    let text = required_text(arguments, name)?;
    NaiveDate::parse_from_str(text.trim(), "%Y-%m-%d")
        .map_err(|_| told(format!("{name} must be a date like 2026-01-31.")))
}

fn string_list(arguments: &Map<String, Value>, name: &str) -> Outcome<Option<Vec<String>>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Array(items)) if items.len() <= MAX_GROUP_COLUMNS => items
            .iter()
            .map(|item| {
                item.as_str()
                    .map(ToOwned::to_owned)
                    .ok_or_else(|| told(format!("{name} must be a list of text.")))
            })
            .collect::<Outcome<Vec<String>>>()
            .map(Some),
        Some(_) => Err(told(format!(
            "{name} must be a list of at most {MAX_GROUP_COLUMNS} texts."
        ))),
    }
}

fn aggregates(arguments: &Map<String, Value>) -> Outcome<Vec<Aggregate>> {
    let Some(names) = string_list(arguments, "aggregate_by")? else {
        return Ok(Vec::new());
    };
    names
        .iter()
        .map(|name| {
            Aggregate::parse(name).ok_or_else(|| {
                told(format!(
                    "aggregate_by takes: {}.",
                    Aggregate::ALL.map(Aggregate::name).join(", ")
                ))
            })
        })
        .collect()
}
