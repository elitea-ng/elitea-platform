//! The smaller reads of the toolkit: issue counts per project, scrum sprints,
//! the statuses of the instance and how much each field is used.

use std::collections::{BTreeMap, HashSet};

use chrono::NaiveDate;
use serde_json::Value;

use crate::toolkits::families::analysis_core::table::Table;
use crate::toolkits::families::jira::client::JiraRoot;
use crate::toolkits::families::jira::config::JiraApiVersion;

use super::extract::{Failure, JiraReader, Outcome, Overflow, Pace, fetch, field_names, search};
use super::records::jira_time;

/// Projects one listing reads (50 per page).
const MAX_PROJECT_PAGES: usize = 40;
/// Issues one project count reads on a Jira that does not give a total.
const COUNT_CAP: usize = 10_000;
/// Key-only pages are cheap: a count asks for 100 at a time.
const COUNT_PAGE_SIZE: usize = 100;
const MAX_BOARD_PAGES: usize = 20;
const MAX_SPRINT_PAGES: usize = 100;
const MAX_SPRINTS: usize = 5_000;
/// Issues the fields overview reads at most.
pub(super) const FIELDS_SAMPLE: usize = 2_000;

/// The Jira field ids the fields overview counts besides custom fields.
const STANDARD_FIELDS: [&str; 22] = [
    "assignee",
    "attachment",
    "comment",
    "components",
    "created",
    "creator",
    "description",
    "fixVersions",
    "issuelinks",
    "issuetype",
    "labels",
    "lastViewed",
    "priority",
    "project",
    "reporter",
    "resolution",
    "resolutiondate",
    "status",
    "subtasks",
    "summary",
    "updated",
    "versions",
];

/// One project of the overview.
pub(super) struct ProjectCount {
    pub(super) key: String,
    pub(super) name: String,
    pub(super) issues: usize,
    /// Whether the count is the whole count (it is a floor when the read hit its cap).
    pub(super) exact: bool,
}

pub(super) struct ProjectsOverview {
    pub(super) projects: Vec<ProjectCount>,
    /// Requested keys the credential cannot see.
    pub(super) unavailable: Vec<String>,
}

fn quoted_projects(keys: &[String]) -> String {
    keys.iter()
        .map(|key| format!("\"{key}\""))
        .collect::<Vec<_>>()
        .join(", ")
}

async fn list_projects(client: &dyn JiraReader, pace: &Pace) -> Outcome<Vec<(String, String)>> {
    let mut projects: Vec<(String, String)> = Vec::new();
    let mut read = |values: &[Value]| {
        projects.extend(values.iter().filter_map(|project| {
            Some((
                project.get("key")?.as_str()?.to_owned(),
                project
                    .get("name")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_owned(),
            ))
        }));
    };
    if client.rest_version() == JiraApiVersion::V3 {
        for page in 0..MAX_PROJECT_PAGES {
            let start = (page * 50).to_string();
            let document = fetch(
                client,
                pace,
                JiraRoot::Api,
                &["project", "search"],
                &[("startAt", &start), ("maxResults", "50")],
            )
            .await?;
            let values = document
                .get("values")
                .and_then(Value::as_array)
                .cloned()
                .unwrap_or_default();
            read(&values);
            if values.is_empty() || document.get("isLast").and_then(Value::as_bool) == Some(true) {
                break;
            }
        }
    } else {
        let document = fetch(client, pace, JiraRoot::Api, &["project"], &[]).await?;
        read(document.as_array().map_or(&[][..], Vec::as_slice));
    }
    Ok(projects)
}

/// How many issues a JQL query finds: Jira's own total when it gives one,
/// otherwise a count of the pages (up to a cap).
async fn count_issues(client: &dyn JiraReader, pace: &Pace, jql: &str) -> Outcome<(usize, bool)> {
    if client.rest_version() != JiraApiVersion::V3 {
        let document = fetch(
            client,
            pace,
            JiraRoot::Api,
            &["search"],
            &[("jql", jql), ("maxResults", "0"), ("fields", "key")],
        )
        .await?;
        if let Some(total) = document
            .get("total")
            .and_then(Value::as_u64)
            .and_then(|total| usize::try_from(total).ok())
        {
            return Ok((total, true));
        }
    }
    if client.rest_version() == JiraApiVersion::V3
        && let Some(count) = client.approximate_count(jql).await?
    {
        // Jira Cloud's own count: one request instead of paging the issues.
        return Ok((usize::try_from(count).unwrap_or(usize::MAX), true));
    }
    let counting = Pace {
        deadline: pace.deadline,
        retry_delay: pace.retry_delay,
        page_size: pace.page_size.max(COUNT_PAGE_SIZE),
        max_pages: pace.max_pages,
    };
    let found = search(
        client,
        &counting,
        jql,
        "key",
        None,
        COUNT_CAP,
        Overflow::Truncate,
    )
    .await?;
    Ok((found.issues.len(), !found.truncated))
}

/// The projects (all the credential sees, or `keys`) with the number of
/// issues updated on or after `after`, most issues first.
pub(super) async fn projects_overview(
    client: &dyn JiraReader,
    pace: &Pace,
    keys: &[String],
    after: NaiveDate,
) -> Outcome<ProjectsOverview> {
    let listed = list_projects(client, pace).await?;
    let (chosen, unavailable): (Vec<(String, String)>, Vec<String>) = if keys.is_empty() {
        (listed, Vec::new())
    } else {
        let chosen = listed
            .iter()
            .filter(|(key, _)| keys.contains(key))
            .cloned()
            .collect();
        let unavailable = keys
            .iter()
            .filter(|key| !listed.iter().any(|(listed_key, _)| listed_key == *key))
            .cloned()
            .collect();
        (chosen, unavailable)
    };
    let mut projects = Vec::with_capacity(chosen.len());
    for (key, name) in chosen {
        let jql = format!(
            "project = \"{key}\" AND updated >= \"{}\"",
            after.format("%Y-%m-%d")
        );
        let (issues, exact) = count_issues(client, pace, &jql).await?;
        projects.push(ProjectCount {
            key,
            name,
            issues,
            exact,
        });
    }
    projects.sort_by(|a, b| b.issues.cmp(&a.issues).then_with(|| a.key.cmp(&b.key)));
    Ok(ProjectsOverview {
        projects,
        unavailable,
    })
}

/// The statuses of the instance: name and category, by name.
pub(super) async fn statuses(
    client: &dyn JiraReader,
    pace: &Pace,
) -> Outcome<Vec<(String, String)>> {
    let document = fetch(client, pace, JiraRoot::Api, &["status"], &[]).await?;
    let mut statuses: Vec<(String, String)> = document
        .as_array()
        .map(|list| {
            list.iter()
                .filter_map(|status| {
                    Some((
                        status.get("name")?.as_str()?.to_owned(),
                        status
                            .get("statusCategory")
                            .and_then(|category| category.get("name"))
                            .and_then(Value::as_str)
                            .unwrap_or_default()
                            .to_owned(),
                    ))
                })
                .collect()
        })
        .unwrap_or_default();
    statuses.sort();
    statuses.dedup();
    Ok(statuses)
}

/// The sprints of the scrum boards of some projects.
pub(super) struct Sprints {
    pub(super) table: Table,
    pub(super) boards: usize,
    /// Why a project or board gave no sprints (no scrum board, no Agile API).
    pub(super) skipped: Vec<String>,
}

fn id_text(value: Option<&Value>) -> String {
    match value {
        Some(Value::Number(number)) => number.to_string(),
        Some(Value::String(text)) => text.clone(),
        _ => String::new(),
    }
}

fn stamp(value: Option<&Value>) -> String {
    value
        .and_then(Value::as_str)
        .and_then(jira_time)
        .map(|time| time.format("%Y-%m-%d %H:%M:%S").to_string())
        .unwrap_or_default()
}

#[allow(clippy::too_many_lines)] // Boards, then sprints, each paged; one loop nest.
pub(super) async fn sprints(
    client: &dyn JiraReader,
    pace: &Pace,
    keys: &[String],
) -> Outcome<Sprints> {
    let mut found = Sprints {
        table: Table::new(&[
            "project_key",
            "board_id",
            "sprint_id",
            "name",
            "state",
            "start_date",
            "end_date",
            "complete_date",
            "activated_date",
            "goal",
        ]),
        boards: 0,
        skipped: Vec::new(),
    };
    for key in keys {
        let mut boards: Vec<String> = Vec::new();
        for page in 0..MAX_BOARD_PAGES {
            let start = (page * 50).to_string();
            let document = match fetch(
                client,
                pace,
                JiraRoot::Agile,
                &["board"],
                &[
                    ("type", "scrum"),
                    ("projectKeyOrId", key),
                    ("startAt", &start),
                    ("maxResults", "50"),
                ],
            )
            .await
            {
                Ok(document) => document,
                Err(Failure::Refused(message)) => {
                    found.skipped.push(format!("{key}: {message}"));
                    break;
                }
                Err(other) => return Err(other),
            };
            let values = document
                .get("values")
                .and_then(Value::as_array)
                .cloned()
                .unwrap_or_default();
            boards.extend(values.iter().map(|board| id_text(board.get("id"))));
            if values.is_empty() || document.get("isLast").and_then(Value::as_bool) == Some(true) {
                break;
            }
        }
        found.boards += boards.len();
        let mut seen: HashSet<String> = HashSet::new();
        for board in boards.iter().filter(|board| !board.is_empty()) {
            for page in 0..MAX_SPRINT_PAGES {
                let start = (page * 50).to_string();
                let document = match fetch(
                    client,
                    pace,
                    JiraRoot::Agile,
                    &["board", board, "sprint"],
                    &[("startAt", &start), ("maxResults", "50")],
                )
                .await
                {
                    Ok(document) => document,
                    Err(Failure::Refused(message)) => {
                        found.skipped.push(format!("board {board}: {message}"));
                        break;
                    }
                    Err(other) => return Err(other),
                };
                let values = document
                    .get("values")
                    .and_then(Value::as_array)
                    .cloned()
                    .unwrap_or_default();
                for sprint in &values {
                    let id = id_text(sprint.get("id"));
                    if !seen.insert(id.clone()) {
                        continue;
                    }
                    if found.table.rows.len() >= MAX_SPRINTS {
                        return Err(Failure::Told(format!(
                            "The projects have more than {MAX_SPRINTS} sprints, so the extraction stopped and wrote nothing. Ask for fewer projects."
                        )));
                    }
                    found.table.push(vec![
                        key.clone(),
                        board.clone(),
                        id,
                        id_text(sprint.get("name")),
                        id_text(sprint.get("state")),
                        stamp(sprint.get("startDate")),
                        stamp(sprint.get("endDate")),
                        stamp(sprint.get("completeDate")),
                        stamp(sprint.get("activatedDate")),
                        id_text(sprint.get("goal")),
                    ]);
                }
                if values.is_empty()
                    || document.get("isLast").and_then(Value::as_bool) == Some(true)
                {
                    break;
                }
            }
        }
    }
    Ok(found)
}

/// How much one field is used.
pub(super) struct FieldUse {
    pub(super) id: String,
    pub(super) name: String,
    pub(super) total: usize,
    pub(super) by_project: BTreeMap<String, usize>,
}

pub(super) struct FieldsOverview {
    pub(super) issues: usize,
    pub(super) per_project: BTreeMap<String, usize>,
    /// Fields with a value on at least one issue, most used first.
    pub(super) fields: Vec<FieldUse>,
    pub(super) truncated: bool,
}

/// A value that counts as "not filled in": null, empty text, list or
/// object, false and zero.
fn is_blank(value: &Value) -> bool {
    match value {
        Value::Null => true,
        Value::String(text) => text.is_empty(),
        Value::Array(items) => items.is_empty(),
        Value::Object(fields) => fields.is_empty(),
        Value::Bool(flag) => !flag,
        Value::Number(number) => number.as_f64().is_some_and(|value| value == 0.0),
    }
}

pub(super) async fn fields_overview(
    client: &dyn JiraReader,
    pace: &Pace,
    keys: &[String],
    after: NaiveDate,
    limit: usize,
) -> Outcome<FieldsOverview> {
    let jql = format!(
        "project IN ({}) AND updated >= \"{}\"",
        quoted_projects(keys),
        after.format("%Y-%m-%d")
    );
    let found = search(client, pace, &jql, "*all", None, limit, Overflow::Truncate).await?;
    let names: BTreeMap<String, String> = field_names(client, pace).await?.into_iter().collect();
    let mut per_project: BTreeMap<String, usize> = BTreeMap::new();
    let mut counts: BTreeMap<String, BTreeMap<String, usize>> = BTreeMap::new();
    for issue in &found.issues {
        let Some(fields) = issue.get("fields").and_then(Value::as_object) else {
            continue;
        };
        let project = fields
            .get("project")
            .and_then(|project| project.get("key"))
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        *per_project.entry(project.clone()).or_insert(0) += 1;
        for (id, value) in fields {
            if is_blank(value)
                || !(STANDARD_FIELDS.contains(&id.as_str()) || id.starts_with("customfield_"))
            {
                continue;
            }
            *counts
                .entry(id.clone())
                .or_default()
                .entry(project.clone())
                .or_insert(0) += 1;
        }
    }
    let mut fields: Vec<FieldUse> = counts
        .into_iter()
        .map(|(id, by_project)| FieldUse {
            name: names.get(&id).cloned().unwrap_or_else(|| id.clone()),
            total: by_project.values().sum(),
            by_project,
            id,
        })
        .collect();
    fields.sort_by(|a, b| b.total.cmp(&a.total).then_with(|| a.name.cmp(&b.name)));
    Ok(FieldsOverview {
        issues: found.issues.len(),
        per_project,
        fields,
        truncated: found.truncated,
    })
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::is_blank;

    #[test]
    fn blank_means_nothing_filled_in() {
        for value in [
            json!(null),
            json!(""),
            json!([]),
            json!({}),
            json!(false),
            json!(0),
        ] {
            assert!(is_blank(&value), "{value}");
        }
        for value in [
            json!("x"),
            json!([1]),
            json!({"a": 1}),
            json!(true),
            json!(2),
        ] {
            assert!(!is_blank(&value), "{value}");
        }
    }
}
