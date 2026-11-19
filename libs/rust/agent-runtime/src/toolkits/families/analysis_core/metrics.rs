//! Flow metrics over a work-item dataset: lead time, cycle time, life time,
//! throughput, flow efficiency and flow load, and the four bundles that
//! combine them (closed items, open items, queue, flow).
//!
//! The definitions follow the `elitea-analyse` library's metric classes,
//! written again from their behaviour (the library declares no licence, so no
//! code is taken). The dataset is the one the extraction tools write: one row
//! per work item and status interval, with `request_type` (`closed`, `open` or
//! `defect`), `status_history`, `from_date`, `to_date` and `time_in_status`
//! (days).
//!
//! Deliberate differences from the library, each documented for users:
//!
//! - A work item with no value in a group column (no team, say) is its own
//!   group with an empty name. The library's `groupby` drops such items.
//! - A status-based metric with a closed item whose status is not in the
//!   mapping is an error naming the status, as in the library. A missing
//!   `Waiting` total counts as zero days instead of failing.
//! - Aggregations are `mean`, `median`, `min`, `max`, `sum`, `count` and `std`
//!   (sample); `mean` is always computed and is the metric's own column.

use std::collections::{BTreeMap, BTreeSet, HashMap, HashSet};

use chrono::{Datelike, NaiveDate, NaiveDateTime, NaiveTime};

use super::history::WAITING_FOR_RELEASE;
use super::table::{Table, format_number};

/// The most (work item, day) pairs a flow-load calculation may visit.
const MAX_FLOW_LOAD_CELLS: usize = 5_000_000;

/// The columns the dataset must have.
const REQUIRED_COLUMNS: [&str; 7] = [
    "issue_id",
    "issue_type",
    "project_name",
    "request_type",
    "status_history",
    "time_in_status",
    "resolved_date",
];

const MONTH: &str = "year_month";

/// A metric, or a bundle of metrics, the calculation can produce.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Metric {
    LeadTime,
    CycleTime,
    LifeTime,
    Throughput,
    FlowEfficiency,
    FlowLoad,
    ClosedItems,
    OpenItems,
    Queue,
    Flow,
}

impl Metric {
    /// Every metric, in the order the tool schema lists them.
    pub(crate) const ALL: [Self; 10] = [
        Self::LeadTime,
        Self::CycleTime,
        Self::LifeTime,
        Self::Throughput,
        Self::FlowEfficiency,
        Self::FlowLoad,
        Self::ClosedItems,
        Self::OpenItems,
        Self::Queue,
        Self::Flow,
    ];

    pub(crate) const fn name(self) -> &'static str {
        match self {
            Self::LeadTime => "lead_time",
            Self::CycleTime => "cycle_time",
            Self::LifeTime => "life_time",
            Self::Throughput => "throughput",
            Self::FlowEfficiency => "flow_efficiency",
            Self::FlowLoad => "flow_load",
            Self::ClosedItems => "closed_items",
            Self::OpenItems => "open_items",
            Self::Queue => "queue",
            Self::Flow => "flow",
        }
    }

    pub(crate) fn parse(name: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|metric| metric.name() == name)
    }

    /// Whether the metric reads the status mapping.
    const fn needs_mapping(self) -> bool {
        matches!(
            self,
            Self::CycleTime | Self::FlowEfficiency | Self::ClosedItems | Self::Flow
        )
    }
}

/// How the per-item values of a group are summarised.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Aggregate {
    Mean,
    Median,
    Min,
    Max,
    Sum,
    Count,
    Std,
}

impl Aggregate {
    pub(crate) const ALL: [Self; 7] = [
        Self::Mean,
        Self::Median,
        Self::Min,
        Self::Max,
        Self::Sum,
        Self::Count,
        Self::Std,
    ];

    pub(crate) const fn name(self) -> &'static str {
        match self {
            Self::Mean => "mean",
            Self::Median => "median",
            Self::Min => "min",
            Self::Max => "max",
            Self::Sum => "sum",
            Self::Count => "count",
            Self::Std => "std",
        }
    }

    pub(crate) fn parse(name: &str) -> Option<Self> {
        Self::ALL
            .into_iter()
            .find(|aggregate| aggregate.name() == name)
    }

    #[allow(clippy::cast_precision_loss)] // A count of work items fits an f64.
    fn apply(self, values: &[f64]) -> Option<f64> {
        if values.is_empty() {
            return None;
        }
        let count = values.len() as f64;
        let sum: f64 = values.iter().sum();
        match self {
            Self::Mean => Some(sum / count),
            Self::Sum => Some(sum),
            Self::Count => Some(count),
            Self::Min => values.iter().copied().reduce(f64::min),
            Self::Max => values.iter().copied().reduce(f64::max),
            Self::Median => {
                let mut sorted = values.to_vec();
                sorted.sort_by(f64::total_cmp);
                let middle = sorted.len() / 2;
                if sorted.len() % 2 == 1 {
                    Some(sorted[middle])
                } else {
                    Some(f64::midpoint(sorted[middle - 1], sorted[middle]))
                }
            }
            Self::Std => {
                if values.len() < 2 {
                    return None;
                }
                let mean = sum / count;
                let squares: f64 = values.iter().map(|value| (value - mean).powi(2)).sum();
                Some((squares / (count - 1.0)).sqrt())
            }
        }
    }
}

/// What to calculate over.
#[derive(Clone, Debug, Default)]
pub(crate) struct Request {
    /// The columns to group by; `None` is the default (`project_name`,
    /// `issue_type`, and `project_key` and `team` when they hold values).
    pub(crate) group_by: Option<Vec<String>>,
    /// The aggregations; empty is `mean`.
    pub(crate) aggregate_by: Vec<Aggregate>,
}

/// Why a calculation could not run. Each variant reads as a sentence for the
/// model, naming what to change.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum MetricError {
    MissingColumn(String),
    UnknownGroupColumn(String),
    MissingMapping,
    UnmappedStatus(String),
    TooManyCells,
}

impl MetricError {
    pub(crate) fn sentence(&self) -> String {
        match self {
            Self::MissingColumn(name) => format!(
                "The dataset has no `{name}` column: calculate over a work-items file written by the extraction tool."
            ),
            Self::UnknownGroupColumn(name) => format!(
                "group_by names `{name}`, which is not a column of the dataset."
            ),
            Self::MissingMapping => "This metric needs the statuses file the extraction tool wrote beside the work-items file (statuses_file).".to_owned(),
            Self::UnmappedStatus(status) => format!(
                "The status `{status}` is not in the statuses file: add it with its status_type and add_to_cycle_time, or write the files again."
            ),
            Self::TooManyCells => "The flow-load calculation spans too many items and days: narrow the dates or the projects the dataset covers.".to_owned(),
        }
    }
}

/// A metric result: group columns, value columns and one row per group.
#[derive(Clone, Debug, Default, PartialEq)]
pub(crate) struct Frame {
    keys: Vec<String>,
    values: Vec<String>,
    rows: BTreeMap<Vec<String>, BTreeMap<String, f64>>,
}

impl Frame {
    fn new(keys: Vec<String>) -> Self {
        Self {
            keys,
            values: Vec::new(),
            rows: BTreeMap::new(),
        }
    }

    fn declare(&mut self, column: &str) {
        if !self.values.iter().any(|name| name == column) {
            self.values.push(column.to_owned());
        }
    }

    fn touch(&mut self, key: Vec<String>) {
        self.rows.entry(key).or_default();
    }

    fn set(&mut self, key: Vec<String>, column: &str, value: f64) {
        self.declare(column);
        self.rows
            .entry(key)
            .or_default()
            .insert(column.to_owned(), value);
    }

    /// Both frames' groups, with both frames' columns.
    fn merge(mut self, other: Self) -> Self {
        for column in &other.values {
            self.declare(column);
        }
        for (key, values) in other.rows {
            self.rows.entry(key).or_default().extend(values);
        }
        self
    }

    /// Number of groups.
    pub(crate) fn len(&self) -> usize {
        self.rows.len()
    }

    /// The frame as a dataset: group columns, then value columns. A group
    /// with no value for a column has an empty cell.
    pub(crate) fn to_table(&self) -> Table {
        let mut columns = self.keys.clone();
        columns.extend(self.values.iter().cloned());
        let mut table = Table::new(&columns);
        for (key, values) in &self.rows {
            let mut row = key.clone();
            for column in &self.values {
                row.push(
                    values
                        .get(column)
                        .map(|value| format_number(*value))
                        .unwrap_or_default(),
                );
            }
            table.push(row);
        }
        table
    }
}

/// One row of the dataset, with the fields the metrics read parsed.
struct Row {
    cells: Vec<String>,
    issue_id: String,
    request_type: String,
    status: String,
    days: Option<f64>,
    resolved: Option<NaiveDateTime>,
    from: Option<NaiveDateTime>,
    to: Option<NaiveDateTime>,
    year_month: String,
}

/// How a group column is read.
#[derive(Clone, Copy)]
enum Selector {
    Cell(usize),
    Month,
}

struct Dataset {
    columns: Vec<String>,
    rows: Vec<Row>,
}

/// Parse a dataset timestamp: `YYYY-MM-DD HH:MM:SS`, the same with a `T`, or a date.
pub(crate) fn parse_time(text: &str) -> Option<NaiveDateTime> {
    let text = text.trim();
    for format in ["%Y-%m-%d %H:%M:%S", "%Y-%m-%dT%H:%M:%S"] {
        if let Ok(time) = NaiveDateTime::parse_from_str(text, format) {
            return Some(time);
        }
    }
    NaiveDate::parse_from_str(text, "%Y-%m-%d")
        .ok()
        .map(|date| date.and_time(NaiveTime::MIN))
}

impl Dataset {
    fn from_table(table: &Table) -> Result<Self, MetricError> {
        let mut index: HashMap<&str, usize> = HashMap::new();
        for name in REQUIRED_COLUMNS {
            let position = table
                .column(name)
                .ok_or_else(|| MetricError::MissingColumn(name.to_owned()))?;
            index.insert(name, position);
        }
        let from_column = table.column("from_date");
        let to_column = table.column("to_date");
        let cell = |row: &[String], position: usize| row.get(position).cloned().unwrap_or_default();
        let rows = table
            .rows
            .iter()
            .map(|row| {
                let resolved = parse_time(&cell(row, index["resolved_date"]));
                Row {
                    cells: row.clone(),
                    issue_id: cell(row, index["issue_id"]),
                    request_type: cell(row, index["request_type"]),
                    status: cell(row, index["status_history"]),
                    days: cell(row, index["time_in_status"])
                        .trim()
                        .parse::<f64>()
                        .ok(),
                    resolved,
                    from: from_column.and_then(|position| parse_time(&cell(row, position))),
                    to: to_column.and_then(|position| parse_time(&cell(row, position))),
                    year_month: resolved
                        .map(|time| format!("{:04}-{:02}", time.year(), time.month()))
                        .unwrap_or_default(),
                }
            })
            .collect();
        Ok(Self {
            columns: table.columns.clone(),
            rows,
        })
    }

    fn has_values(&self, column: &str) -> bool {
        self.columns
            .iter()
            .position(|name| name == column)
            .is_some_and(|position| {
                self.rows
                    .iter()
                    .any(|row| row.cells.get(position).is_some_and(|cell| !cell.is_empty()))
            })
    }

    /// The group columns for a calculation, as names. `with_month` adds
    /// `year_month`; `extra` adds one more (`status`, for open items).
    fn group_columns(
        &self,
        request: &Request,
        with_month: bool,
        extra: Option<&str>,
    ) -> Result<Vec<String>, MetricError> {
        let mut columns: Vec<String> = if let Some(requested) = &request.group_by {
            for name in requested {
                if name != MONTH && !self.columns.iter().any(|column| column == name) {
                    return Err(MetricError::UnknownGroupColumn(name.clone()));
                }
            }
            requested.clone()
        } else {
            let mut defaults = vec!["project_name".to_owned(), "issue_type".to_owned()];
            for optional in ["project_key", "team"] {
                if self.has_values(optional) {
                    defaults.push(optional.to_owned());
                }
            }
            defaults
        };
        for add in with_month.then_some(MONTH).into_iter().chain(extra) {
            if add != MONTH && !self.columns.iter().any(|column| column == add) {
                return Err(MetricError::MissingColumn(add.to_owned()));
            }
            if !columns.iter().any(|column| column == add) {
                columns.push(add.to_owned());
            }
        }
        Ok(columns)
    }

    fn selectors(&self, columns: &[String]) -> Vec<Selector> {
        columns
            .iter()
            .map(|name| {
                if name == MONTH {
                    Selector::Month
                } else {
                    // Checked by `group_columns`.
                    Selector::Cell(
                        self.columns
                            .iter()
                            .position(|column| column == name)
                            .unwrap_or(0),
                    )
                }
            })
            .collect()
    }

    fn rows_where<'a>(&'a self, request_types: &[&str], keep_waiting: bool) -> Vec<&'a Row> {
        self.rows
            .iter()
            .filter(|row| request_types.contains(&row.request_type.as_str()))
            .filter(|row| keep_waiting || row.status != WAITING_FOR_RELEASE)
            .collect()
    }
}

fn key_of(row: &Row, selectors: &[Selector], month: Option<&str>) -> Option<Vec<String>> {
    let mut key = Vec::with_capacity(selectors.len());
    for selector in selectors {
        match selector {
            Selector::Cell(position) => {
                key.push(row.cells.get(*position).cloned().unwrap_or_default());
            }
            Selector::Month => {
                let value = month.unwrap_or(&row.year_month);
                if value.is_empty() {
                    // No resolution date: no month to group the item under.
                    return None;
                }
                key.push(value.to_owned());
            }
        }
    }
    Some(key)
}

/// The first row of each work item, in dataset order.
fn first_rows<'a>(rows: &[&'a Row]) -> Vec<&'a Row> {
    let mut seen: HashSet<&str> = HashSet::new();
    rows.iter()
        .filter(|row| seen.insert(row.issue_id.as_str()))
        .copied()
        .collect()
}

fn aggregates(request: &Request) -> Vec<Aggregate> {
    let mut list: Vec<Aggregate> = Vec::new();
    for aggregate in std::iter::once(Aggregate::Mean).chain(request.aggregate_by.iter().copied()) {
        if !list.contains(&aggregate) {
            list.push(aggregate);
        }
    }
    list
}

/// Summarise per-item values by group into a frame: the mean under `column`,
/// the others under `column_<aggregate>`.
fn summarize(
    keys: &[String],
    column: &str,
    request: &Request,
    groups: BTreeMap<Vec<String>, Vec<f64>>,
) -> Frame {
    let mut frame = Frame::new(keys.to_vec());
    frame.declare(column);
    for aggregate in aggregates(request) {
        if aggregate != Aggregate::Mean {
            frame.declare(&format!("{column}_{}", aggregate.name()));
        }
    }
    for (key, values) in groups {
        frame.touch(key.clone());
        for aggregate in aggregates(request) {
            if let Some(value) = aggregate.apply(&values) {
                let name = if aggregate == Aggregate::Mean {
                    column.to_owned()
                } else {
                    format!("{column}_{}", aggregate.name())
                };
                frame.set(key.clone(), &name, value);
            }
        }
    }
    frame
}

/// Per work item, the sum of the days of its rows that pass `keep`, grouped.
/// `zero_if_none` counts an item with no such row as zero days (cycle time).
fn time_sums(
    rows: &[&Row],
    selectors: &[Selector],
    keep: &dyn Fn(&Row) -> bool,
    zero_if_none: bool,
) -> BTreeMap<Vec<String>, Vec<f64>> {
    let mut sums: HashMap<&str, Option<f64>> = HashMap::new();
    for row in rows {
        let entry = sums.entry(row.issue_id.as_str()).or_insert(None);
        if keep(row)
            && let Some(days) = row.days
        {
            *entry = Some(entry.unwrap_or(0.0) + days);
        }
    }
    let mut groups: BTreeMap<Vec<String>, Vec<f64>> = BTreeMap::new();
    for row in first_rows(rows) {
        let Some(key) = key_of(row, selectors, None) else {
            continue;
        };
        let values = groups.entry(key).or_default();
        let sum = sums.get(row.issue_id.as_str()).copied().flatten();
        if let Some(value) = sum.or(zero_if_none.then_some(0.0)) {
            values.push(value);
        }
    }
    groups
}

fn lead_time(
    data: &Dataset,
    request: &Request,
    keys: &[String],
    rows: &[&Row],
    name: &str,
) -> Frame {
    let selectors = data.selectors(keys);
    summarize(
        keys,
        name,
        request,
        time_sums(rows, &selectors, &|_| true, false),
    )
}

fn life_time(data: &Dataset, request: &Request, keys: &[String], rows: &[&Row]) -> Frame {
    let selectors = data.selectors(keys);
    summarize(
        keys,
        "life_time",
        request,
        time_sums(rows, &selectors, &|_| true, false),
    )
}

struct Mapping {
    kinds: HashMap<String, (bool, bool)>,
}

impl Mapping {
    fn from_table(table: &Table) -> Result<Self, MetricError> {
        let column = |name: &str| {
            table
                .column(name)
                .ok_or_else(|| MetricError::MissingColumn(name.to_owned()))
        };
        let (status, kind, cycle) = (
            column("status_raw")?,
            column("status_type")?,
            column("add_to_cycle_time")?,
        );
        let kinds = table
            .rows
            .iter()
            .map(|row| {
                (
                    row.get(status).cloned().unwrap_or_default(),
                    (
                        row.get(kind).is_some_and(|value| value == "Active"),
                        row.get(cycle).is_some_and(|value| value == "y"),
                    ),
                )
            })
            .collect();
        Ok(Self { kinds })
    }

    fn check(&self, rows: &[&Row]) -> Result<(), MetricError> {
        let mut statuses: BTreeSet<&str> = rows
            .iter()
            .filter(|row| row.request_type == "closed")
            .map(|row| row.status.as_str())
            .collect();
        statuses.remove(WAITING_FOR_RELEASE);
        match statuses
            .into_iter()
            .find(|status| !self.kinds.contains_key(*status))
        {
            Some(status) => Err(MetricError::UnmappedStatus(status.to_owned())),
            None => Ok(()),
        }
    }

    fn counts_toward_cycle(&self, status: &str) -> bool {
        self.kinds.get(status).is_some_and(|(_, cycle)| *cycle)
    }

    fn is_active(&self, status: &str) -> Option<bool> {
        self.kinds.get(status).map(|(active, _)| *active)
    }
}

fn cycle_time(
    data: &Dataset,
    request: &Request,
    keys: &[String],
    rows: &[&Row],
    mapping: &Mapping,
) -> Frame {
    let selectors = data.selectors(keys);
    summarize(
        keys,
        "cycle_time",
        request,
        time_sums(
            rows,
            &selectors,
            &|row| mapping.counts_toward_cycle(&row.status),
            true,
        ),
    )
}

fn days_in_month(year_month: &str) -> Option<f64> {
    let (year, month) = year_month.split_once('-')?;
    let (year, month) = (year.parse::<i32>().ok()?, month.parse::<u32>().ok()?);
    let first = NaiveDate::from_ymd_opt(year, month, 1)?;
    let next = if month == 12 {
        NaiveDate::from_ymd_opt(year + 1, 1, 1)?
    } else {
        NaiveDate::from_ymd_opt(year, month + 1, 1)?
    };
    #[allow(clippy::cast_precision_loss)] // At most 31.
    Some((next - first).num_days() as f64)
}

/// Items closed per day, averaged over the calendar month.
fn throughput(data: &Dataset, keys: &[String], rows: &[&Row]) -> Frame {
    let selectors = data.selectors(keys);
    let month_position = keys.iter().position(|name| name == MONTH);
    let mut counts: BTreeMap<Vec<String>, f64> = BTreeMap::new();
    for row in first_rows(rows) {
        if let Some(key) = key_of(row, &selectors, None) {
            *counts.entry(key).or_insert(0.0) += 1.0;
        }
    }
    let mut frame = Frame::new(keys.to_vec());
    frame.declare("throughput");
    for (key, count) in counts {
        let days = month_position.and_then(|position| days_in_month(&key[position]));
        match days {
            Some(days) => frame.set(key, "throughput", count / days),
            None => frame.touch(key),
        }
    }
    frame
}

/// Distinct work items per group.
fn items_count(data: &Dataset, keys: &[String], rows: &[&Row], name: &str) -> Frame {
    let selectors = data.selectors(keys);
    let mut frame = Frame::new(keys.to_vec());
    frame.declare(name);
    let mut counts: BTreeMap<Vec<String>, f64> = BTreeMap::new();
    for row in first_rows(rows) {
        if let Some(key) = key_of(row, &selectors, None) {
            *counts.entry(key).or_insert(0.0) += 1.0;
        }
    }
    for (key, count) in counts {
        frame.set(key, name, count);
    }
    frame
}

/// The share of a closed item's lead time spent in active statuses.
fn flow_efficiency(
    data: &Dataset,
    request: &Request,
    keys: &[String],
    rows: &[&Row],
    mapping: &Mapping,
) -> Frame {
    let selectors = data.selectors(keys);
    let mut totals: HashMap<&str, (f64, f64)> = HashMap::new();
    for row in rows {
        let (Some(days), Some(active)) = (row.days, mapping.is_active(&row.status)) else {
            continue;
        };
        let entry = totals.entry(row.issue_id.as_str()).or_insert((0.0, 0.0));
        if active {
            entry.0 += days;
        } else {
            entry.1 += days;
        }
    }
    let mut groups: BTreeMap<Vec<String>, Vec<f64>> = BTreeMap::new();
    for row in first_rows(rows) {
        let Some(key) = key_of(row, &selectors, None) else {
            continue;
        };
        let values = groups.entry(key).or_default();
        if let Some((active, waiting)) = totals.get(row.issue_id.as_str()) {
            let total = active + waiting;
            if total > 0.0 {
                values.push(active / total);
            }
        }
    }
    summarize(keys, "flow_efficiency", request, groups)
}

/// Work items in progress per calendar month: the distinct items that were in
/// a status on some day of the month. An item is in a status on the days
/// whose midnight falls inside the interval.
fn flow_load(data: &Dataset, keys: &[String]) -> Result<Frame, MetricError> {
    if !data.columns.iter().any(|name| name == "from_date") {
        return Err(MetricError::MissingColumn("from_date".to_owned()));
    }
    if !data.columns.iter().any(|name| name == "to_date") {
        return Err(MetricError::MissingColumn("to_date".to_owned()));
    }
    let selectors = data.selectors(keys);
    let rows = data.rows_where(&["closed", "open"], false);
    let projects: BTreeSet<&str> = rows
        .iter()
        .filter_map(|row| {
            data.columns
                .iter()
                .position(|name| name == "project_name")
                .and_then(|position| row.cells.get(position))
                .map(String::as_str)
        })
        .collect();
    let project_position = data.columns.iter().position(|name| name == "project_name");
    let mut seen: BTreeMap<Vec<String>, HashSet<&str>> = BTreeMap::new();
    let mut visited = 0_usize;
    for project in projects {
        let of_project: Vec<&&Row> = rows
            .iter()
            .filter(|row| {
                project_position
                    .and_then(|position| row.cells.get(position))
                    .is_some_and(|name| name == project)
            })
            .collect();
        if !of_project.iter().any(|row| row.request_type == "closed") {
            continue;
        }
        let resolved: Vec<NaiveDateTime> =
            of_project.iter().filter_map(|row| row.resolved).collect();
        let (Some(first), Some(last)) = (resolved.iter().min(), resolved.iter().max()) else {
            continue;
        };
        let Some(start) = NaiveDate::from_ymd_opt(first.year(), first.month(), 1) else {
            continue;
        };
        let Some(end) = month_end(last.date()) else {
            continue;
        };
        for row in of_project {
            let (Some(from), Some(to)) = (row.from, row.to) else {
                continue;
            };
            let first_day = if from.time() == NaiveTime::MIN {
                Some(from.date())
            } else {
                from.date().succ_opt()
            };
            let Some(mut day) = first_day.map(|day| day.max(start)) else {
                continue;
            };
            let last_day = to.date().min(end);
            while day <= last_day {
                visited += 1;
                if visited > MAX_FLOW_LOAD_CELLS {
                    return Err(MetricError::TooManyCells);
                }
                let month = format!("{:04}-{:02}", day.year(), day.month());
                if let Some(key) = key_of(row, &selectors, Some(&month)) {
                    seen.entry(key).or_default().insert(row.issue_id.as_str());
                }
                let Some(next) = day.succ_opt() else { break };
                day = next;
            }
        }
    }
    let mut frame = Frame::new(keys.to_vec());
    frame.declare("flow_load");
    for (key, items) in seen {
        #[allow(clippy::cast_precision_loss)] // A count of work items fits an f64.
        frame.set(key, "flow_load", items.len() as f64);
    }
    Ok(frame)
}

fn month_end(date: NaiveDate) -> Option<NaiveDate> {
    let next = if date.month() == 12 {
        NaiveDate::from_ymd_opt(date.year() + 1, 1, 1)?
    } else {
        NaiveDate::from_ymd_opt(date.year(), date.month() + 1, 1)?
    };
    next.pred_opt()
}

/// Calculate a metric over a work-items dataset. `mapping` is the statuses
/// table the extraction wrote, needed by the status-based metrics.
pub(crate) fn calculate(
    metric: Metric,
    items: &Table,
    mapping: Option<&Table>,
    request: &Request,
) -> Result<Frame, MetricError> {
    let data = Dataset::from_table(items)?;
    let mapping = if metric.needs_mapping() {
        Some(Mapping::from_table(
            mapping.ok_or(MetricError::MissingMapping)?,
        )?)
    } else {
        None
    };
    let closed = data.rows_where(&["closed"], false);
    let open = data.rows_where(&["open"], false);
    if let Some(mapping) = &mapping {
        mapping.check(&closed)?;
    }
    // The bundles and the monthly metrics group by month too.
    let monthly = matches!(
        metric,
        Metric::Throughput | Metric::FlowLoad | Metric::ClosedItems | Metric::Queue | Metric::Flow
    );
    let keys = data.group_columns(
        request,
        monthly,
        (metric == Metric::OpenItems).then_some("status"),
    )?;
    let needs_mapping = || mapping.as_ref().ok_or(MetricError::MissingMapping);
    Ok(match metric {
        Metric::LeadTime => lead_time(&data, request, &keys, &closed, "lead_time"),
        Metric::CycleTime => cycle_time(&data, request, &keys, &closed, needs_mapping()?),
        Metric::LifeTime => life_time(&data, request, &keys, &open),
        Metric::Throughput => throughput(&data, &keys, &data.rows_where(&["closed"], true)),
        Metric::FlowEfficiency => flow_efficiency(&data, request, &keys, &closed, needs_mapping()?),
        Metric::FlowLoad => flow_load(&data, &keys)?,
        Metric::ClosedItems => lead_time(&data, request, &keys, &closed, "lead_time")
            .merge(cycle_time(&data, request, &keys, &closed, needs_mapping()?))
            .merge(items_count(&data, &keys, &closed, "items_count")),
        Metric::OpenItems => life_time(&data, request, &keys, &open).merge(items_count(
            &data,
            &keys,
            &open,
            "items_count",
        )),
        Metric::Queue => {
            let mut frame = lead_time(&data, request, &keys, &closed, "lead_time")
                .merge(throughput(&data, &keys, &closed));
            let position = |name: &str| frame.values.iter().position(|column| column == name);
            if position("lead_time").is_some() && position("throughput").is_some() {
                frame.declare("WIP");
                let keys: Vec<Vec<String>> = frame.rows.keys().cloned().collect();
                for key in keys {
                    let values = &frame.rows[&key];
                    if let (Some(lead), Some(rate)) =
                        (values.get("lead_time"), values.get("throughput"))
                    {
                        let wip = lead * rate;
                        frame.set(key, "WIP", wip);
                    }
                }
            }
            frame
        }
        Metric::Flow => flow_efficiency(&data, request, &keys, &closed, needs_mapping()?)
            .merge(flow_load(&data, &keys)?)
            .merge(lead_time(&data, request, &keys, &closed, "flow_time"))
            .merge(items_count(
                &data,
                &keys,
                &data.rows_where(&["closed"], true),
                "flow_velocity",
            )),
    })
}

#[cfg(test)]
mod tests {
    use super::{Aggregate, Metric, MetricError, Request, calculate, parse_time};
    use crate::toolkits::families::analysis_core::table::Table;

    const COLUMNS: [&str; 13] = [
        "issue_id",
        "issue_key",
        "issue_type",
        "project_name",
        "project_key",
        "team",
        "request_type",
        "status_history",
        "from_date",
        "to_date",
        "time_in_status",
        "resolved_date",
        "status",
    ];

    /// issue id, type, team, request type, status, from, to, days, resolved.
    #[allow(clippy::type_complexity)]
    fn items(rows: &[(&str, &str, &str, &str, &str, &str, &str, &str, &str)]) -> Table {
        let mut table = Table::new(&COLUMNS);
        for &(id, kind, team, request, status, from, to, days, resolved) in rows {
            table.push(
                [
                    id,
                    id,
                    kind,
                    "Alpha",
                    "ALP",
                    team,
                    request,
                    status,
                    from,
                    to,
                    days,
                    resolved,
                    if request == "open" {
                        "In Progress"
                    } else {
                        "Done"
                    },
                ]
                .iter()
                .map(|cell| (*cell).to_owned())
                .collect(),
            );
        }
        table
    }

    fn mapping() -> Table {
        let mut table = Table::new(&["status_raw", "status_type", "add_to_cycle_time"]);
        for (status, kind, cycle) in [
            ("To Do", "Waiting", "n"),
            ("In Progress", "Active", "y"),
            ("In Review", "Active", "y"),
            ("Blocked", "Waiting", "y"),
            ("Done", "Waiting", "n"),
        ] {
            table.push(vec![status.to_owned(), kind.to_owned(), cycle.to_owned()]);
        }
        table
    }

    /// Two closed stories (A: 2 + 3 + 1 days, resolved 2026-01-10 and B: 4 + 2
    /// days, resolved 2026-01-20) and one open bug.
    #[allow(clippy::too_many_lines)] // One row per interval keeps the arithmetic checkable.
    fn fixture() -> Table {
        items(&[
            (
                "1",
                "Story",
                "red",
                "closed",
                "To Do",
                "2026-01-01 00:00:00",
                "2026-01-03 00:00:00",
                "2",
                "2026-01-10 00:00:00",
            ),
            (
                "1",
                "Story",
                "red",
                "closed",
                "In Progress",
                "2026-01-03 00:00:00",
                "2026-01-06 00:00:00",
                "3",
                "2026-01-10 00:00:00",
            ),
            (
                "1",
                "Story",
                "red",
                "closed",
                "Blocked",
                "2026-01-06 00:00:00",
                "2026-01-07 00:00:00",
                "1",
                "2026-01-10 00:00:00",
            ),
            (
                "1",
                "Story",
                "red",
                "closed",
                "Done",
                "2026-01-07 00:00:00",
                "",
                "",
                "2026-01-10 00:00:00",
            ),
            (
                "2",
                "Story",
                "red",
                "closed",
                "To Do",
                "2026-01-05 00:00:00",
                "2026-01-09 00:00:00",
                "4",
                "2026-01-20 00:00:00",
            ),
            (
                "2",
                "Story",
                "red",
                "closed",
                "In Review",
                "2026-01-09 00:00:00",
                "2026-01-11 00:00:00",
                "2",
                "2026-01-20 00:00:00",
            ),
            (
                "2",
                "Story",
                "red",
                "closed",
                "Waiting for release",
                "2026-01-20 00:00:00",
                "2026-01-31 00:00:00",
                "11",
                "2026-01-20 00:00:00",
            ),
            (
                "3",
                "Bug",
                "red",
                "open",
                "To Do",
                "2026-01-12 00:00:00",
                "2026-01-14 00:00:00",
                "2",
                "",
            ),
            (
                "3",
                "Bug",
                "red",
                "open",
                "In Progress",
                "2026-01-14 00:00:00",
                "2026-01-18 00:00:00",
                "4",
                "",
            ),
        ])
    }

    fn value(frame: &Table, group: &[&str], column: &str) -> String {
        let position = frame.column(column).expect("column");
        let keys = group.len();
        frame
            .rows
            .iter()
            .find(|row| {
                row[..keys]
                    .iter()
                    .map(String::as_str)
                    .eq(group.iter().copied())
            })
            .map_or_else(
                || panic!("no group {group:?} in {frame:?}"),
                |row| row[position].clone(),
            )
    }

    fn run(metric: Metric, request: &Request) -> Table {
        calculate(metric, &fixture(), Some(&mapping()), request)
            .expect("calculated")
            .to_table()
    }

    #[test]
    fn lead_time_is_the_sum_of_the_days_per_item_averaged_per_group() {
        // Item 1: 2 + 3 + 1 = 6 days (the open-ended Done row has none).
        // Item 2: 4 + 2 = 6 days (the release wait is not lead time).
        let table = run(Metric::LeadTime, &Request::default());
        assert_eq!(
            table.columns,
            [
                "project_name",
                "issue_type",
                "project_key",
                "team",
                "lead_time"
            ]
        );
        assert_eq!(
            value(&table, &["Alpha", "Story", "ALP", "red"], "lead_time"),
            "6"
        );
        assert_eq!(table.rows.len(), 1, "the open bug is not a closed item");
    }

    #[test]
    fn lead_time_aggregations_add_their_columns_after_the_mean() {
        let request = Request {
            group_by: Some(vec!["issue_type".to_owned()]),
            aggregate_by: vec![Aggregate::Max, Aggregate::Min, Aggregate::Mean],
        };
        let table = run(Metric::LeadTime, &request);
        assert_eq!(
            table.columns,
            ["issue_type", "lead_time", "lead_time_max", "lead_time_min"]
        );
        assert_eq!(value(&table, &["Story"], "lead_time_max"), "6");
    }

    #[test]
    fn cycle_time_counts_only_the_statuses_the_mapping_marks() {
        // Item 1: In Progress 3 + Blocked 1 = 4. Item 2: In Review 2 = 2.
        let table = run(Metric::CycleTime, &Request::default());
        assert_eq!(
            value(&table, &["Alpha", "Story", "ALP", "red"], "cycle_time"),
            "3"
        );
    }

    #[test]
    fn life_time_is_the_age_of_the_open_items() {
        let table = run(Metric::LifeTime, &Request::default());
        assert_eq!(
            value(&table, &["Alpha", "Bug", "ALP", "red"], "life_time"),
            "6"
        );
    }

    #[test]
    fn throughput_is_closed_items_per_day_of_the_resolution_month() {
        // Two items resolved in January 2026 (31 days).
        let table = run(Metric::Throughput, &Request::default());
        let got: f64 = value(
            &table,
            &["Alpha", "Story", "ALP", "red", "2026-01"],
            "throughput",
        )
        .parse()
        .expect("a number");
        assert!((got - 2.0 / 31.0).abs() < 1e-6, "{got}");
    }

    #[test]
    fn flow_efficiency_is_the_active_share_of_each_closed_items_time() {
        // Item 1: active 3 of 6 days = 0.5. Item 2: active 2 of 6 days.
        let table = run(Metric::FlowEfficiency, &Request::default());
        let got: f64 = value(&table, &["Alpha", "Story", "ALP", "red"], "flow_efficiency")
            .parse()
            .expect("a number");
        assert!((got - f64::midpoint(0.5, 2.0 / 6.0)).abs() < 1e-6, "{got}");
    }

    #[test]
    fn flow_load_counts_the_items_in_a_status_on_a_day_of_the_month() {
        // January: items 1 (days 1-7), 2 (5-11, 20-31 waiting row is excluded)
        // and 3 (12-18) were each in progress at some midnight.
        let table = run(Metric::FlowLoad, &Request::default());
        assert_eq!(
            value(
                &table,
                &["Alpha", "Story", "ALP", "red", "2026-01"],
                "flow_load"
            ),
            "2"
        );
        assert_eq!(
            value(
                &table,
                &["Alpha", "Bug", "ALP", "red", "2026-01"],
                "flow_load"
            ),
            "1"
        );
    }

    #[test]
    fn the_closed_items_bundle_joins_lead_time_cycle_time_and_the_count() {
        let table = run(Metric::ClosedItems, &Request::default());
        let group = ["Alpha", "Story", "ALP", "red", "2026-01"];
        assert_eq!(value(&table, &group, "lead_time"), "6");
        assert_eq!(value(&table, &group, "cycle_time"), "3");
        assert_eq!(value(&table, &group, "items_count"), "2");
    }

    #[test]
    fn the_queue_bundle_multiplies_throughput_by_lead_time_into_wip() {
        let table = run(Metric::Queue, &Request::default());
        let group = ["Alpha", "Story", "ALP", "red", "2026-01"];
        let wip: f64 = value(&table, &group, "WIP").parse().expect("a number");
        assert!((wip - 6.0 * 2.0 / 31.0).abs() < 1e-5, "{wip}");
    }

    #[test]
    fn the_open_items_bundle_groups_by_status() {
        let table = run(Metric::OpenItems, &Request::default());
        let group = ["Alpha", "Bug", "ALP", "red", "In Progress"];
        assert_eq!(value(&table, &group, "items_count"), "1");
        assert_eq!(value(&table, &group, "life_time"), "6");
    }

    #[test]
    fn the_flow_bundle_has_all_four_flow_columns() {
        let table = run(Metric::Flow, &Request::default());
        for column in ["flow_efficiency", "flow_load", "flow_time", "flow_velocity"] {
            assert!(table.column(column).is_some(), "{column}");
        }
        let group = ["Alpha", "Story", "ALP", "red", "2026-01"];
        assert_eq!(value(&table, &group, "flow_velocity"), "2");
        assert_eq!(value(&table, &group, "flow_time"), "6");
    }

    #[test]
    fn a_missing_group_value_is_its_own_group() {
        let mut table = fixture();
        let team = table.column("team").expect("team");
        for row in &mut table.rows {
            if row[0] == "2" {
                row[team].clear();
            }
        }
        let frame = calculate(Metric::LeadTime, &table, None, &Request::default())
            .expect("calculated")
            .to_table();
        assert_eq!(frame.rows.len(), 2);
    }

    #[test]
    fn a_status_missing_from_the_mapping_names_itself() {
        let mut short = mapping();
        short.rows.retain(|row| row[0] != "In Review");
        let error = calculate(
            Metric::CycleTime,
            &fixture(),
            Some(&short),
            &Request::default(),
        )
        .expect_err("refused");
        assert_eq!(error, MetricError::UnmappedStatus("In Review".to_owned()));
        assert!(error.sentence().contains("In Review"));
    }

    #[test]
    fn a_status_metric_without_the_mapping_and_a_bad_group_column_are_refused() {
        assert_eq!(
            calculate(Metric::CycleTime, &fixture(), None, &Request::default())
                .expect_err("refused"),
            MetricError::MissingMapping
        );
        let request = Request {
            group_by: Some(vec!["nope".to_owned()]),
            aggregate_by: Vec::new(),
        };
        assert_eq!(
            calculate(Metric::LeadTime, &fixture(), None, &request).expect_err("refused"),
            MetricError::UnknownGroupColumn("nope".to_owned())
        );
        let bare = Table::new(&["issue_id"]);
        assert!(matches!(
            calculate(Metric::LeadTime, &bare, None, &Request::default()),
            Err(MetricError::MissingColumn(_))
        ));
    }

    #[test]
    fn an_empty_dataset_gives_an_empty_result() {
        let empty = items(&[]);
        let table = calculate(Metric::LeadTime, &empty, None, &Request::default())
            .expect("calculated")
            .to_table();
        assert_eq!(table.rows.len(), 0);
    }

    #[test]
    fn aggregates_and_names_round_trip() {
        for aggregate in Aggregate::ALL {
            assert_eq!(Aggregate::parse(aggregate.name()), Some(aggregate));
        }
        for metric in Metric::ALL {
            assert_eq!(Metric::parse(metric.name()), Some(metric));
        }
        assert_eq!(Metric::parse("velocity"), None);
        let values = [1.0, 2.0, 3.0, 4.0];
        assert_eq!(Aggregate::Median.apply(&values), Some(2.5));
        assert_eq!(Aggregate::Sum.apply(&values), Some(10.0));
        assert_eq!(Aggregate::Count.apply(&values), Some(4.0));
        let std = Aggregate::Std.apply(&values).expect("std");
        assert!((std - 1.290_994).abs() < 1e-5);
        assert_eq!(Aggregate::Std.apply(&[1.0]), None);
        assert_eq!(Aggregate::Mean.apply(&[]), None);
    }

    #[test]
    fn timestamps_are_read_in_the_dataset_formats() {
        assert!(parse_time("2026-01-02 03:04:05").is_some());
        assert!(parse_time("2026-01-02T03:04:05").is_some());
        assert!(parse_time("2026-01-02").is_some());
        assert!(parse_time("").is_none());
        assert!(parse_time("soon").is_none());
    }
}
