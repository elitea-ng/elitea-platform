//! The work-items dataset and the statuses mapping, from the extracted
//! issues: one row per issue, request type and status interval.
//!
//! Layout and rules follow the `elitea-analyse` library's output (written
//! again from its behaviour, no code taken); the differences are listed in
//! the toolkit's page:
//!
//! - a `defect` row set duplicates the rows of every issue whose type is a
//!   defect type and that was created on or after `created_after`;
//! - when closed is decided by status, the resolution date of a closed issue
//!   is the date it entered the closed status, and a closed issue resolved
//!   before `resolved_after` is left out;
//! - a closed issue with a released fix version has one more row, the pseudo
//!   status `Waiting for release`, from its resolution to the release date;
//! - the library's inward label for an outward link is the outward label.

use chrono::{NaiveDate, NaiveDateTime, NaiveTime};

use crate::toolkits::families::analysis_core::history::{
    Interval, WAITING_FOR_RELEASE, days_between, intervals, status_mapping,
};
use crate::toolkits::families::analysis_core::table::{Table, format_number};

use super::records::{Issue, RequestType};

/// The standard columns, before the custom ones.
const LEADING: [&str; 19] = [
    "issue_key",
    "issue_id",
    "project_name",
    "project_key",
    "issue_type",
    "total_time_spent",
    "priority",
    "resolution",
    "summary",
    "status",
    "labels",
    "created_date",
    "resolved_date",
    "last_updated_date",
    "start_date",
    "components",
    "subtasks",
    "linked_issues",
    "fix_versions",
];

/// The columns after the custom ones.
const TRAILING: [&str; 5] = [
    "request_type",
    "status_history",
    "from_date",
    "to_date",
    "time_in_status",
];

/// Whether `column` is one the dataset already has.
pub(super) fn is_reserved_column(column: &str) -> bool {
    LEADING.contains(&column) || TRAILING.contains(&column)
}

/// How an issue is decided closed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum ClosedBasedOn {
    /// By its status (1).
    Status,
    /// By having a resolution date (2).
    Resolution,
}

/// What the build needs beyond the issues.
pub(super) struct Rules<'a> {
    pub(super) closed_based_on: ClosedBasedOn,
    pub(super) closed_status: &'a str,
    pub(super) resolved_after: NaiveDate,
    pub(super) created_after: NaiveDate,
    pub(super) defect_types: &'a [String],
    pub(super) custom_columns: &'a [String],
    /// The most rows the dataset may have.
    pub(super) max_rows: usize,
}

/// What a build produced.
#[derive(Debug)]
pub(super) struct Built {
    pub(super) work_items: Table,
    pub(super) statuses: Table,
    pub(super) issues: usize,
    pub(super) rows: usize,
    pub(super) closed: usize,
    pub(super) open: usize,
    pub(super) defects: usize,
    pub(super) released: usize,
    /// Issues left out because they were resolved before `resolved_after`.
    pub(super) before_window: usize,
    /// Issues whose changelog could not be read in full.
    pub(super) incomplete_changelogs: usize,
}

/// The build would pass the row cap.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) struct TooManyRows {
    pub(super) limit: usize,
}

fn stamp(time: Option<NaiveDateTime>) -> String {
    time.map(|time| time.format("%Y-%m-%d %H:%M:%S").to_string())
        .unwrap_or_default()
}

fn midnight(date: NaiveDate) -> NaiveDateTime {
    date.and_time(NaiveTime::MIN)
}

struct OutRow {
    key: String,
    from: Option<NaiveDateTime>,
    cells: Vec<String>,
}

/// Build the two tables. `now` ends the last interval of an open issue.
#[allow(clippy::too_many_lines)] // One pass over the issues; splitting it would only pass state around.
pub(super) fn build(
    issues: &[Issue],
    rules: &Rules<'_>,
    now: NaiveDateTime,
) -> Result<Built, TooManyRows> {
    let mut columns: Vec<String> = LEADING.iter().map(|name| (*name).to_owned()).collect();
    columns.extend(rules.custom_columns.iter().cloned());
    columns.extend(TRAILING.iter().map(|name| (*name).to_owned()));
    let mut rows: Vec<OutRow> = Vec::new();
    let mut histories: Vec<Vec<Interval>> = Vec::new();
    let mut built = Built {
        work_items: Table::new(&columns),
        statuses: Table::new(&[
            "status_raw",
            "status_raw_index",
            "status_mapped",
            "status_mapped_index",
            "status_type",
            "add_to_cycle_time",
        ]),
        issues: 0,
        rows: 0,
        closed: 0,
        open: 0,
        defects: 0,
        released: 0,
        before_window: 0,
        incomplete_changelogs: 0,
    };
    for issue in issues {
        let open_until = (issue.request == RequestType::Open).then_some(now);
        let history = intervals(issue.created, &issue.status, &issue.transitions, open_until);
        let mut resolved = issue.resolved;
        if rules.closed_based_on == ClosedBasedOn::Status
            && !rules.closed_status.is_empty()
            && issue.status.to_lowercase() == rules.closed_status.to_lowercase()
        {
            resolved = history.last().and_then(|interval| interval.from);
            if resolved.is_some_and(|time| time < midnight(rules.resolved_after)) {
                built.before_window += 1;
                continue;
            }
        }
        built.issues += 1;
        built.incomplete_changelogs += usize::from(!issue.changelog_complete);
        let is_defect = rules.defect_types.iter().any(|name| name == &issue.kind)
            && issue
                .created
                .is_some_and(|created| created >= midnight(rules.created_after));
        let mut request_types = vec![issue.request.name()];
        if is_defect {
            request_types.push("defect");
            built.defects += 1;
        }
        match issue.request {
            RequestType::Closed => built.closed += 1,
            RequestType::Open => built.open += 1,
        }
        for request_type in request_types {
            for interval in &history {
                if rows.len() >= rules.max_rows {
                    return Err(TooManyRows {
                        limit: rules.max_rows,
                    });
                }
                rows.push(OutRow {
                    key: issue.key.clone(),
                    from: interval.from,
                    cells: row(
                        issue,
                        resolved,
                        request_type,
                        &interval.status,
                        interval.from,
                        interval.to,
                        interval.days,
                    ),
                });
            }
        }
        // A release before the resolution has nothing to wait for.
        if issue.request == RequestType::Closed
            && let Some(release) = issue.latest_release
            && resolved.is_some_and(|time| time <= release)
        {
            if rows.len() >= rules.max_rows {
                return Err(TooManyRows {
                    limit: rules.max_rows,
                });
            }
            built.released += 1;
            rows.push(OutRow {
                key: issue.key.clone(),
                from: resolved,
                cells: row(
                    issue,
                    resolved,
                    "closed",
                    WAITING_FOR_RELEASE,
                    resolved,
                    Some(release),
                    days_between(resolved, Some(release)),
                ),
            });
        }
        histories.push(history);
    }
    // By issue key, then by when the interval began (unknown last); rows of
    // one issue and start keep the order they were built in.
    rows.sort_by(|a, b| {
        a.key
            .cmp(&b.key)
            .then_with(|| a.from.is_none().cmp(&b.from.is_none()))
            .then_with(|| a.from.cmp(&b.from))
    });
    for out in rows {
        built.work_items.push(out.cells);
    }
    built.rows = built.work_items.rows.len();
    let mapping = status_mapping(histories.iter().map(Vec::as_slice));
    for (position, entry) in mapping.iter().enumerate() {
        built.statuses.push(vec![
            entry.status.clone(),
            position.to_string(),
            entry.status.clone(),
            position.to_string(),
            entry.status_type.to_owned(),
            entry.add_to_cycle_time.to_owned(),
        ]);
    }
    if built.released > 0 {
        let position = mapping.len().to_string();
        built.statuses.push(vec![
            WAITING_FOR_RELEASE.to_owned(),
            position.clone(),
            WAITING_FOR_RELEASE.to_owned(),
            position,
            "Waiting".to_owned(),
            "n".to_owned(),
        ]);
    }
    Ok(built)
}

fn row(
    issue: &Issue,
    resolved: Option<NaiveDateTime>,
    request_type: &str,
    status: &str,
    from: Option<NaiveDateTime>,
    to: Option<NaiveDateTime>,
    days: Option<f64>,
) -> Vec<String> {
    let mut cells = vec![
        issue.key.clone(),
        issue.id.clone(),
        issue.project_name.clone(),
        issue.project_key.clone(),
        issue.kind.clone(),
        issue.total_time_spent.clone(),
        issue.priority.clone(),
        issue.resolution.clone(),
        issue.summary.clone(),
        issue.status.clone(),
        issue.labels.clone(),
        stamp(issue.created),
        stamp(resolved),
        stamp(issue.updated),
        String::new(),
        issue.components.clone(),
        issue.subtasks.clone(),
        issue.linked_issues.clone(),
        issue.fix_versions.clone(),
    ];
    cells.extend(issue.custom.iter().cloned());
    cells.push(request_type.to_owned());
    cells.push(status.to_owned());
    cells.push(stamp(from));
    cells.push(stamp(to));
    cells.push(days.map(format_number).unwrap_or_default());
    cells
}

#[cfg(test)]
mod tests {
    use chrono::{NaiveDate, NaiveDateTime};

    use crate::toolkits::families::analysis_core::history::Transition;

    use super::super::records::{Issue, RequestType};
    use super::{Built, ClosedBasedOn, Rules, TooManyRows, build, is_reserved_column};

    fn at(month: u32, day: u32) -> NaiveDateTime {
        NaiveDate::from_ymd_opt(2026, month, day)
            .and_then(|date| date.and_hms_opt(0, 0, 0))
            .expect("a date")
    }

    fn issue(key: &str, request: RequestType, status: &str, kind: &str) -> Issue {
        Issue {
            request,
            key: key.to_owned(),
            id: key.replace("ALP-", "100"),
            project_name: "Alpha".to_owned(),
            project_key: "ALP".to_owned(),
            kind: kind.to_owned(),
            total_time_spent: String::new(),
            priority: String::new(),
            resolution: String::new(),
            summary: format!("Summary of {key}"),
            status: status.to_owned(),
            labels: String::new(),
            created: Some(at(1, 1)),
            resolved: None,
            updated: Some(at(1, 20)),
            components: String::new(),
            subtasks: String::new(),
            linked_issues: String::new(),
            fix_versions: String::new(),
            latest_release: None,
            custom: vec!["red".to_owned()],
            transitions: Vec::new(),
            changelog_complete: true,
        }
    }

    fn moved(month: u32, day: u32, from: &str) -> Transition {
        Transition {
            when: at(month, day),
            from: Some(from.to_owned()),
        }
    }

    fn rules<'a>(defects: &'a [String], custom: &'a [String]) -> Rules<'a> {
        Rules {
            closed_based_on: ClosedBasedOn::Resolution,
            closed_status: "Done",
            resolved_after: NaiveDate::from_ymd_opt(2026, 1, 1).expect("a date"),
            created_after: NaiveDate::from_ymd_opt(2026, 1, 1).expect("a date"),
            defect_types: defects,
            custom_columns: custom,
            max_rows: 100,
        }
    }

    fn column(built: &Built, name: &str) -> Vec<String> {
        let position = built.work_items.column(name).expect("column");
        built
            .work_items
            .rows
            .iter()
            .map(|row| row[position].clone())
            .collect()
    }

    fn closed_issue() -> Issue {
        let mut closed = issue("ALP-1", RequestType::Closed, "Done", "Story");
        closed.resolved = Some(at(1, 10));
        closed.transitions = vec![moved(1, 3, "To Do"), moved(1, 10, "In Progress")];
        closed
    }

    #[test]
    fn a_closed_issue_has_one_row_per_interval_and_the_last_has_no_days() {
        let custom = ["team".to_owned()];
        let built = build(&[closed_issue()], &rules(&[], &custom), at(2, 1)).expect("built");
        assert_eq!(built.issues, 1);
        assert_eq!(
            column(&built, "status_history"),
            ["To Do", "In Progress", "Done"]
        );
        assert_eq!(column(&built, "time_in_status"), ["2", "7", ""]);
        assert_eq!(
            column(&built, "request_type"),
            ["closed", "closed", "closed"]
        );
        assert_eq!(column(&built, "team"), ["red", "red", "red"]);
        assert_eq!(column(&built, "resolved_date")[0], "2026-01-10 00:00:00");
        assert_eq!(column(&built, "from_date")[1], "2026-01-03 00:00:00");
    }

    #[test]
    fn an_open_issue_runs_its_last_interval_to_now() {
        let mut open = issue("ALP-2", RequestType::Open, "In Progress", "Story");
        open.transitions = vec![moved(1, 5, "To Do")];
        let custom = ["team".to_owned()];
        let built = build(&[open], &rules(&[], &custom), at(1, 15)).expect("built");
        assert_eq!(column(&built, "time_in_status"), ["4", "10"]);
        assert_eq!(column(&built, "to_date")[1], "2026-01-15 00:00:00");
    }

    #[test]
    fn a_defect_gets_a_second_row_set_when_created_in_the_window() {
        let mut bug = issue("ALP-3", RequestType::Open, "To Do", "Defect");
        bug.created = Some(at(1, 2));
        let mut old_bug = issue("ALP-4", RequestType::Open, "To Do", "Defect");
        old_bug.created = Some(at(1, 1));
        let defects = ["Defect".to_owned()];
        let custom = ["team".to_owned()];
        let mut window = rules(&defects, &custom);
        window.created_after = NaiveDate::from_ymd_opt(2026, 1, 2).expect("a date");
        let built = build(&[bug, old_bug], &window, at(1, 9)).expect("built");
        assert_eq!(built.defects, 1);
        assert_eq!(
            column(&built, "request_type"),
            ["open", "defect", "open"],
            "ALP-3 as open and defect, ALP-4 as open only"
        );
    }

    #[test]
    fn a_closed_issue_with_a_released_version_waits_for_release() {
        let mut shipped = closed_issue();
        shipped.latest_release = Some(at(1, 20));
        let custom = ["team".to_owned()];
        let built = build(&[shipped], &rules(&[], &custom), at(2, 1)).expect("built");
        assert_eq!(built.released, 1);
        assert_eq!(
            column(&built, "status_history"),
            ["To Do", "In Progress", "Done", "Waiting for release"]
        );
        assert_eq!(column(&built, "time_in_status")[3], "10");
        let statuses: Vec<&str> = built
            .statuses
            .rows
            .iter()
            .map(|row| row[0].as_str())
            .collect();
        assert_eq!(statuses.last(), Some(&"Waiting for release"));
        let last = built.statuses.rows.last().expect("a mapping row");
        assert_eq!(&last[4..], ["Waiting", "n"]);
    }

    #[test]
    fn a_release_before_the_resolution_adds_no_waiting_row() {
        let mut early = closed_issue();
        early.latest_release = Some(at(1, 5));
        let custom = ["team".to_owned()];
        let built = build(&[early], &rules(&[], &custom), at(2, 1)).expect("built");
        assert_eq!(built.released, 0);
        assert_eq!(
            column(&built, "status_history"),
            ["To Do", "In Progress", "Done"]
        );
    }

    #[test]
    fn the_closed_status_matches_without_regard_to_case() {
        let mut by_status = issue("ALP-5", RequestType::Closed, "Done", "Story");
        by_status.transitions = vec![moved(1, 12, "In Progress")];
        let custom = ["team".to_owned()];
        let mut window = rules(&[], &custom);
        window.closed_based_on = ClosedBasedOn::Status;
        window.closed_status = "done";
        let built = build(&[by_status], &window, at(2, 1)).expect("built");
        assert_eq!(column(&built, "resolved_date")[0], "2026-01-12 00:00:00");
    }

    #[test]
    fn closed_by_status_takes_the_resolution_from_the_last_transition() {
        let mut by_status = issue("ALP-5", RequestType::Closed, "Done", "Story");
        by_status.transitions = vec![moved(1, 12, "In Progress")];
        let mut stale = issue("ALP-6", RequestType::Closed, "Done", "Story");
        stale.transitions = vec![moved(1, 3, "In Progress")];
        let custom = ["team".to_owned()];
        let mut window = rules(&[], &custom);
        window.closed_based_on = ClosedBasedOn::Status;
        window.resolved_after = NaiveDate::from_ymd_opt(2026, 1, 10).expect("a date");
        let built = build(&[by_status, stale], &window, at(2, 1)).expect("built");
        assert_eq!(built.issues, 1);
        assert_eq!(built.before_window, 1);
        assert_eq!(column(&built, "resolved_date")[0], "2026-01-12 00:00:00");
    }

    #[test]
    fn rows_are_ordered_by_key_and_start_and_the_mapping_follows_the_workflow() {
        let mut second = issue("ALP-2", RequestType::Open, "In Progress", "Story");
        second.transitions = vec![moved(1, 5, "To Do")];
        let mut first = issue("ALP-1", RequestType::Open, "In Progress", "Story");
        first.transitions = vec![moved(1, 4, "To Do")];
        let custom = ["team".to_owned()];
        let built = build(&[second, first], &rules(&[], &custom), at(1, 9)).expect("built");
        assert_eq!(
            column(&built, "issue_key"),
            ["ALP-1", "ALP-1", "ALP-2", "ALP-2"]
        );
        let mapped: Vec<(&str, &str)> = built
            .statuses
            .rows
            .iter()
            .map(|row| (row[0].as_str(), row[1].as_str()))
            .collect();
        assert_eq!(mapped, [("To Do", "0"), ("In Progress", "1")]);
    }

    #[test]
    fn the_row_cap_refuses_a_dataset_that_is_too_big() {
        let custom = ["team".to_owned()];
        let mut capped = rules(&[], &custom);
        capped.max_rows = 2;
        let error = build(&[closed_issue()], &capped, at(2, 1)).expect_err("too many rows");
        assert_eq!(error, TooManyRows { limit: 2 });
    }

    #[test]
    fn an_issue_with_an_unreadable_changelog_is_counted() {
        let mut partial = issue("ALP-7", RequestType::Open, "To Do", "Story");
        partial.changelog_complete = false;
        let custom = ["team".to_owned()];
        let built = build(&[partial], &rules(&[], &custom), at(1, 9)).expect("built");
        assert_eq!(built.incomplete_changelogs, 1);
    }

    #[test]
    fn standard_column_names_are_reserved() {
        assert!(is_reserved_column("status"));
        assert!(is_reserved_column("time_in_status"));
        assert!(!is_reserved_column("team"));
    }
}
