//! One Jira issue, as the extraction reads it: the standard fields, the
//! configured custom fields, the status transitions of its changelog and the
//! release dates of its fix versions.

use chrono::{NaiveDate, NaiveDateTime, NaiveTime};
use serde_json::Value;

use crate::toolkits::families::analysis_core::history::Transition;
use crate::toolkits::families::analysis_core::metrics::parse_time;

/// A custom dataset column and the Jira field ids it reads, in the order of
/// the names the toolkit configured (the first field with a value wins).
#[derive(Clone, Debug, Eq, PartialEq)]
pub(super) struct CustomColumn {
    pub(super) column: String,
    pub(super) field_ids: Vec<String>,
}

/// Which query an issue came from.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum RequestType {
    Closed,
    Open,
}

impl RequestType {
    pub(super) const fn name(self) -> &'static str {
        match self {
            Self::Closed => "closed",
            Self::Open => "open",
        }
    }
}

/// An issue, ready for the dataset.
#[derive(Clone, Debug, PartialEq)]
pub(super) struct Issue {
    pub(super) request: RequestType,
    pub(super) key: String,
    pub(super) id: String,
    pub(super) project_name: String,
    pub(super) project_key: String,
    pub(super) kind: String,
    pub(super) total_time_spent: String,
    pub(super) priority: String,
    pub(super) resolution: String,
    pub(super) summary: String,
    pub(super) status: String,
    pub(super) labels: String,
    pub(super) created: Option<NaiveDateTime>,
    pub(super) resolved: Option<NaiveDateTime>,
    pub(super) updated: Option<NaiveDateTime>,
    pub(super) components: String,
    pub(super) subtasks: String,
    pub(super) linked_issues: String,
    pub(super) fix_versions: String,
    /// The release date of the latest released fix version.
    pub(super) latest_release: Option<NaiveDateTime>,
    pub(super) custom: Vec<String>,
    pub(super) transitions: Vec<Transition>,
    /// Whether the changelog could be read in full.
    pub(super) changelog_complete: bool,
}

/// A timestamp as the dataset keeps it: the first 19 characters of Jira's
/// ISO time, so the wall clock of the instance without its offset (the
/// library's convention), or a date.
pub(super) fn jira_time(text: &str) -> Option<NaiveDateTime> {
    let text = text.trim();
    parse_time(text.get(..19).unwrap_or(text))
}

fn text<'a>(value: Option<&'a Value>, key: &str) -> &'a str {
    value
        .and_then(|value| value.get(key))
        .and_then(Value::as_str)
        .unwrap_or_default()
}

fn names(list: Option<&Value>, key: &str) -> String {
    list.and_then(Value::as_array)
        .map(|items| {
            items
                .iter()
                .filter_map(|item| item.get(key).and_then(Value::as_str))
                .collect::<Vec<_>>()
                .join(";")
        })
        .unwrap_or_default()
}

/// `a;b` of the links of an issue: `<direction label> <other issue key>`.
fn linked_issues(links: Option<&Value>) -> String {
    let Some(links) = links.and_then(Value::as_array) else {
        return String::new();
    };
    links
        .iter()
        .filter_map(|link| {
            let kind = link.get("type")?;
            if let Some(key) = link
                .get("inwardIssue")
                .and_then(|issue| issue.get("key"))
                .and_then(Value::as_str)
            {
                Some(format!("{} {key}", text(Some(kind), "inward")))
            } else {
                link.get("outwardIssue")
                    .and_then(|issue| issue.get("key"))
                    .and_then(Value::as_str)
                    .map(|key| format!("{} {key}", text(Some(kind), "outward")))
            }
        })
        .collect::<Vec<_>>()
        .join(";")
}

/// The latest release date among the fix versions, and all version names.
fn fix_versions(versions: Option<&Value>) -> (String, Option<NaiveDateTime>) {
    let Some(versions) = versions.and_then(Value::as_array) else {
        return (String::new(), None);
    };
    let joined = versions
        .iter()
        .filter_map(|version| version.get("name").and_then(Value::as_str))
        .collect::<Vec<_>>()
        .join(";");
    let latest = versions
        .iter()
        .filter_map(|version| version.get("releaseDate").and_then(Value::as_str))
        .filter_map(|date| {
            NaiveDate::parse_from_str(date.get(..10).unwrap_or(date), "%Y-%m-%d").ok()
        })
        .max()
        .map(|date| date.and_time(NaiveTime::MIN));
    (joined, latest)
}

/// A custom field's value as one cell: an option's `value`, a named
/// object's `name` or `displayName`, a list joined by commas, a plain value
/// as it is.
pub(super) fn field_value(value: &Value) -> String {
    match value {
        Value::Null => String::new(),
        Value::String(text) => text.clone(),
        Value::Bool(flag) => flag.to_string(),
        Value::Number(number) => number.to_string(),
        Value::Array(items) => items
            .iter()
            .map(field_value)
            .filter(|cell| !cell.is_empty())
            .collect::<Vec<_>>()
            .join(","),
        Value::Object(object) => ["value", "name", "displayName"]
            .iter()
            .find_map(|key| object.get(*key).and_then(Value::as_str))
            .unwrap_or_default()
            .to_owned(),
    }
}

/// The status transitions of a changelog's histories, in any order.
pub(super) fn transitions(histories: &[Value]) -> Vec<Transition> {
    let mut found = Vec::new();
    for history in histories {
        let Some(when) = history
            .get("created")
            .and_then(Value::as_str)
            .and_then(jira_time)
        else {
            continue;
        };
        let Some(items) = history.get("items").and_then(Value::as_array) else {
            continue;
        };
        for item in items {
            if item.get("field").and_then(Value::as_str) == Some("status") {
                found.push(Transition {
                    when,
                    from: item
                        .get("fromString")
                        .and_then(Value::as_str)
                        .map(ToOwned::to_owned),
                });
            }
        }
    }
    found
}

/// Read one issue of a search page. `histories` are the changelog's
/// histories, complete or not; `complete` says which. An issue with no key,
/// no id or no fields is not an issue.
pub(super) fn parse_issue(
    issue: &Value,
    request: RequestType,
    custom: &[CustomColumn],
    histories: &[Value],
    complete: bool,
) -> Option<Issue> {
    let key = issue.get("key")?.as_str()?.to_owned();
    let id = match issue.get("id")? {
        Value::String(id) => id.clone(),
        other => other.to_string(),
    };
    let fields = issue.get("fields")?;
    let when = |name: &str| fields.get(name).and_then(Value::as_str).and_then(jira_time);
    let (fix_version_names, latest_release) = fix_versions(fields.get("fixVersions"));
    Some(Issue {
        request,
        key,
        id,
        project_name: text(fields.get("project"), "name").to_owned(),
        project_key: text(fields.get("project"), "key").to_owned(),
        kind: text(fields.get("issuetype"), "name").to_owned(),
        total_time_spent: fields
            .get("aggregatetimespent")
            .and_then(Value::as_i64)
            .map(|seconds| seconds.to_string())
            .unwrap_or_default(),
        priority: text(fields.get("priority"), "name").to_owned(),
        resolution: text(fields.get("resolution"), "name").to_owned(),
        summary: fields
            .get("summary")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned(),
        status: text(fields.get("status"), "name").to_owned(),
        labels: fields
            .get("labels")
            .and_then(Value::as_array)
            .map(|labels| {
                labels
                    .iter()
                    .filter_map(Value::as_str)
                    .collect::<Vec<_>>()
                    .join(";")
            })
            .unwrap_or_default(),
        created: when("created"),
        resolved: when("resolutiondate"),
        updated: when("updated"),
        components: names(fields.get("components"), "name"),
        subtasks: names(fields.get("subtasks"), "key"),
        linked_issues: linked_issues(fields.get("issuelinks")),
        fix_versions: fix_version_names,
        latest_release,
        custom: custom
            .iter()
            .map(|column| {
                column
                    .field_ids
                    .iter()
                    .filter_map(|id| fields.get(id.as_str()))
                    .map(field_value)
                    .find(|cell| !cell.is_empty())
                    .unwrap_or_default()
            })
            .collect(),
        transitions: transitions(histories),
        changelog_complete: complete,
    })
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::{CustomColumn, RequestType, field_value, jira_time, parse_issue};

    #[test]
    fn jira_times_drop_the_offset_and_a_date_is_midnight() {
        let time = jira_time("2026-01-03T10:20:30.123+0200").expect("time");
        assert_eq!(time.to_string(), "2026-01-03 10:20:30");
        assert_eq!(
            jira_time("2026-02-01").expect("date").to_string(),
            "2026-02-01 00:00:00"
        );
        assert_eq!(jira_time(""), None);
        assert_eq!(jira_time("last week"), None);
    }

    #[test]
    fn custom_field_values_flatten_like_cells() {
        assert_eq!(field_value(&json!({"value": "Blue"})), "Blue");
        assert_eq!(field_value(&json!({"name": "Team A"})), "Team A");
        assert_eq!(field_value(&json!({"displayName": "Ada"})), "Ada");
        assert_eq!(field_value(&json!([{"value": "a"}, {"value": "b"}])), "a,b");
        assert_eq!(field_value(&json!(3.5)), "3.5");
        assert_eq!(field_value(&json!(null)), "");
        assert_eq!(field_value(&json!({"other": 1})), "");
    }

    fn issue() -> serde_json::Value {
        json!({
            "id": "10001",
            "key": "ALP-1",
            "fields": {
                "project": {"key": "ALP", "name": "Alpha"},
                "issuetype": {"name": "Story"},
                "aggregatetimespent": 3600,
                "priority": {"name": "High"},
                "resolution": {"name": "Done"},
                "summary": "Do the thing",
                "status": {"name": "Done"},
                "labels": ["a", "b"],
                "created": "2026-01-01T09:00:00.000+0000",
                "resolutiondate": "2026-01-10T12:00:00.000+0000",
                "updated": "2026-01-11T08:00:00.000+0000",
                "components": [{"name": "API"}, {"name": "UI"}],
                "subtasks": [{"key": "ALP-2"}],
                "issuelinks": [
                    {"type": {"inward": "is blocked by", "outward": "blocks"}, "inwardIssue": {"key": "ALP-3"}},
                    {"type": {"inward": "is blocked by", "outward": "blocks"}, "outwardIssue": {"key": "ALP-4"}}
                ],
                "fixVersions": [
                    {"name": "1.0", "releaseDate": "2026-01-15"},
                    {"name": "1.1", "releaseDate": "2026-02-15"},
                    {"name": "next"}
                ],
                "customfield_1": null,
                "customfield_2": {"value": "red"}
            }
        })
    }

    #[test]
    fn an_issue_reads_into_one_record() {
        let custom = [CustomColumn {
            column: "team".to_owned(),
            field_ids: vec!["customfield_1".to_owned(), "customfield_2".to_owned()],
        }];
        let histories = [json!({
            "created": "2026-01-03T10:00:00.000+0000",
            "items": [
                {"field": "status", "fromString": "To Do", "toString": "In Progress"},
                {"field": "assignee", "fromString": null, "toString": "Ada"}
            ]
        })];
        let record = parse_issue(&issue(), RequestType::Closed, &custom, &histories, true)
            .expect("an issue");
        assert_eq!(record.key, "ALP-1");
        assert_eq!(record.id, "10001");
        assert_eq!(record.project_name, "Alpha");
        assert_eq!(record.total_time_spent, "3600");
        assert_eq!(record.labels, "a;b");
        assert_eq!(record.components, "API;UI");
        assert_eq!(record.subtasks, "ALP-2");
        assert_eq!(record.linked_issues, "is blocked by ALP-3;blocks ALP-4");
        assert_eq!(record.fix_versions, "1.0;1.1;next");
        assert_eq!(
            record.latest_release.expect("release").to_string(),
            "2026-02-15 00:00:00"
        );
        assert_eq!(record.custom, ["red"]);
        assert_eq!(record.transitions.len(), 1);
        assert_eq!(record.transitions[0].from.as_deref(), Some("To Do"));
        assert!(record.changelog_complete);
    }

    #[test]
    fn a_value_that_is_not_an_issue_is_refused() {
        for value in [
            json!({"key": "A-1", "fields": {}}),
            json!({"id": "1", "fields": {}}),
            json!({"id": "1", "key": "A-1"}),
            json!("A-1"),
        ] {
            assert!(parse_issue(&value, RequestType::Open, &[], &[], true).is_none());
        }
    }
}
