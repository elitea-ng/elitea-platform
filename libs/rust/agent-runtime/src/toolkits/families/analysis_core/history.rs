//! Time in status: the intervals a work item spent in each status, from its
//! creation date and its status transitions, and the status mapping the
//! status-based metrics read (which status is active, which counts toward
//! cycle time).
//!
//! The rules follow the behaviour of the `elitea-analyse` library's
//! `lead_time_distribution_jira` and `statuses_order_jira`, written again from
//! that behaviour (the library declares no licence, so no code is taken):
//!
//! - the first interval starts at the creation date, each later one at the
//!   previous transition;
//! - an interval ends at the transition that left the status; the last one,
//!   in the current status, has no end, except for an open item, whose last
//!   interval runs to "now" so its life time can be counted;
//! - the length is in days.

use std::collections::BTreeMap;

use chrono::NaiveDateTime;

/// One status change of a work item.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct Transition {
    pub(crate) when: NaiveDateTime,
    /// The status left. `None` when the system did not record it: the
    /// current status stands in, as the library does.
    pub(crate) from: Option<String>,
}

/// One interval a work item spent in a status.
#[derive(Clone, Debug, PartialEq)]
pub(crate) struct Interval {
    pub(crate) status: String,
    /// The position of the interval in the item's history, from 0.
    pub(crate) index: usize,
    pub(crate) from: Option<NaiveDateTime>,
    pub(crate) to: Option<NaiveDateTime>,
    pub(crate) days: Option<f64>,
}

/// Days between two moments, when both are known.
#[allow(clippy::cast_precision_loss)] // A span of seconds fits an f64 exactly.
pub(crate) fn days_between(from: Option<NaiveDateTime>, to: Option<NaiveDateTime>) -> Option<f64> {
    let (from, to) = (from?, to?);
    Some((to - from).num_seconds() as f64 / 86_400.0)
}

/// The intervals of one work item. `transitions` may come in any order.
/// `open_until` is "now" for an open item and `None` for a closed one.
pub(crate) fn intervals(
    created: Option<NaiveDateTime>,
    current_status: &str,
    transitions: &[Transition],
    open_until: Option<NaiveDateTime>,
) -> Vec<Interval> {
    let mut ordered: Vec<&Transition> = transitions.iter().collect();
    ordered.sort_by_key(|transition| transition.when);
    let mut out = Vec::with_capacity(ordered.len() + 1);
    let mut start = created;
    for transition in ordered {
        let status = transition
            .from
            .as_deref()
            .filter(|status| !status.is_empty())
            .unwrap_or(current_status);
        out.push(Interval {
            status: status.to_owned(),
            index: out.len(),
            from: start,
            to: Some(transition.when),
            days: days_between(start, Some(transition.when)),
        });
        start = Some(transition.when);
    }
    out.push(Interval {
        status: current_status.to_owned(),
        index: out.len(),
        from: start,
        to: open_until,
        days: days_between(start, open_until),
    });
    out
}

/// Statuses that count as active work (flow efficiency), lower-cased.
const ACTIVE_STATUSES: [&str; 22] = [
    "triage",
    "collecting information",
    "in design",
    "in progress",
    "in development",
    "in pr",
    "design review",
    "in review",
    "team review",
    "qa",
    "qa in progress",
    "in qa",
    "eng qa in progress",
    "po review",
    "review",
    "team validation",
    "testing",
    "in testing",
    "po approval",
    "prod monitoring",
    "in production",
    "publish in process",
];

/// Statuses that do not count toward cycle time, lower-cased.
const NOT_IN_CYCLE_TIME: [&str; 16] = [
    "backlog",
    "open",
    "to do",
    "new",
    "in research",
    "collecting information",
    "ready",
    "ready for development",
    "selected for development",
    "cancelled",
    "closed",
    "won't do",
    "deferred",
    "approved",
    "done",
    "waiting for release",
];

/// The pseudo-status of the time between a closed item's resolution and the
/// release that shipped it.
pub(crate) const WAITING_FOR_RELEASE: &str = "Waiting for release";

/// One row of the status mapping.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct StatusMapRow {
    pub(crate) status: String,
    /// `Active` or `Waiting`.
    pub(crate) status_type: &'static str,
    /// `y` or `n`.
    pub(crate) add_to_cycle_time: &'static str,
}

/// The status mapping of a set of histories: each status once, placed where
/// it most often occurs in an item's history (ties go to the earlier
/// position, then the name), so the rows read in workflow order.
pub(crate) fn status_mapping<'a>(
    histories: impl IntoIterator<Item = &'a [Interval]>,
) -> Vec<StatusMapRow> {
    let mut occurrences: BTreeMap<(&str, usize), usize> = BTreeMap::new();
    for history in histories {
        for interval in history {
            *occurrences
                .entry((interval.status.as_str(), interval.index))
                .or_insert(0) += 1;
        }
    }
    // Per status, the position it occurs at most often.
    let mut best: BTreeMap<&str, (usize, usize)> = BTreeMap::new();
    for ((status, index), count) in &occurrences {
        let entry = best.entry(*status).or_insert((*index, *count));
        if *count > entry.1 || (*count == entry.1 && *index < entry.0) {
            *entry = (*index, *count);
        }
    }
    let mut ordered: Vec<(&str, usize, usize)> = best
        .into_iter()
        .map(|(status, (index, count))| (status, index, count))
        .collect();
    ordered.sort_by(|a, b| {
        a.1.cmp(&b.1)
            .then_with(|| b.2.cmp(&a.2))
            .then_with(|| a.0.cmp(b.0))
    });
    ordered
        .into_iter()
        .map(|(status, _, _)| {
            let lower = status.to_lowercase();
            StatusMapRow {
                status: status.to_owned(),
                status_type: if ACTIVE_STATUSES.contains(&lower.as_str()) {
                    "Active"
                } else {
                    "Waiting"
                },
                add_to_cycle_time: if NOT_IN_CYCLE_TIME.contains(&lower.as_str()) {
                    "n"
                } else {
                    "y"
                },
            }
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use chrono::NaiveDate;

    use super::{Transition, days_between, intervals, status_mapping};

    fn at(day: u32, hour: u32) -> chrono::NaiveDateTime {
        NaiveDate::from_ymd_opt(2026, 1, day)
            .and_then(|date| date.and_hms_opt(hour, 0, 0))
            .expect("a valid date")
    }

    fn moved(day: u32, hour: u32, from: &str) -> Transition {
        Transition {
            when: at(day, hour),
            from: Some(from.to_owned()),
        }
    }

    #[test]
    fn a_closed_item_has_one_interval_per_status_and_the_last_has_no_end() {
        let history = intervals(
            Some(at(1, 0)),
            "Done",
            &[moved(3, 0, "To Do"), moved(6, 12, "In Progress")],
            None,
        );
        let shown: Vec<(&str, Option<f64>)> = history
            .iter()
            .map(|interval| (interval.status.as_str(), interval.days))
            .collect();
        assert_eq!(
            shown,
            [
                ("To Do", Some(2.0)),
                ("In Progress", Some(3.5)),
                ("Done", None)
            ]
        );
        assert_eq!(history[2].from, Some(at(6, 12)));
        assert_eq!(history[2].index, 2);
    }

    #[test]
    fn an_open_item_runs_its_last_interval_to_now() {
        let history = intervals(
            Some(at(1, 0)),
            "In Progress",
            &[moved(2, 0, "To Do")],
            Some(at(5, 0)),
        );
        assert_eq!(history[1].days, Some(3.0));
        assert_eq!(history[1].to, Some(at(5, 0)));
    }

    #[test]
    fn transitions_are_ordered_and_an_unrecorded_origin_is_the_current_status() {
        let history = intervals(
            Some(at(1, 0)),
            "Done",
            &[
                moved(5, 0, "B"),
                Transition {
                    when: at(2, 0),
                    from: None,
                },
            ],
            None,
        );
        let statuses: Vec<&str> = history
            .iter()
            .map(|interval| interval.status.as_str())
            .collect();
        assert_eq!(statuses, ["Done", "B", "Done"]);
        assert_eq!(history[1].from, Some(at(2, 0)));
    }

    #[test]
    fn an_item_with_no_transitions_has_one_interval_from_creation() {
        let history = intervals(Some(at(1, 0)), "To Do", &[], Some(at(4, 0)));
        assert_eq!(history.len(), 1);
        assert_eq!(history[0].days, Some(3.0));
        assert_eq!(days_between(None, Some(at(1, 0))), None);
    }

    #[test]
    fn the_mapping_orders_statuses_by_where_they_most_often_occur() {
        let a = intervals(
            Some(at(1, 0)),
            "Done",
            &[
                moved(2, 0, "To Do"),
                moved(3, 0, "In Progress"),
                moved(4, 0, "QA"),
            ],
            None,
        );
        let b = intervals(
            Some(at(1, 0)),
            "Done",
            &[moved(2, 0, "To Do"), moved(3, 0, "In Progress")],
            None,
        );
        let map = status_mapping([a.as_slice(), b.as_slice()]);
        let rows: Vec<(&str, &str, &str)> = map
            .iter()
            .map(|row| (row.status.as_str(), row.status_type, row.add_to_cycle_time))
            .collect();
        assert_eq!(
            rows,
            [
                ("To Do", "Waiting", "n"),
                ("In Progress", "Active", "y"),
                ("Done", "Waiting", "n"),
                ("QA", "Active", "y"),
            ]
        );
    }
}
