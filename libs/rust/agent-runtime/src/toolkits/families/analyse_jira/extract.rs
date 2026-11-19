//! Bulk extraction from Jira: paged JQL search with the changelog expanded,
//! the rest of a long changelog, the field names behind the custom columns.
//!
//! One extraction is bounded: by the number of issues it may read, by pages,
//! and by a wall-clock deadline. A bound that is reached ends the extraction
//! with a sentence naming it; nothing partial is written.

use std::time::Duration;

use async_trait::async_trait;
use chrono::NaiveDate;
use serde_json::Value;
use tokio::time::{Instant, sleep};

use crate::toolkits::families::jira::client::{JiraClient, JiraClientError, JiraRead, JiraRoot};
use crate::toolkits::families::jira::config::JiraApiVersion;

use super::dataset::ClosedBasedOn;
use super::records::{CustomColumn, Issue, RequestType, parse_issue};

/// A retryable Jira failure (a 429, a 5xx, a timeout) is tried this many more times.
const MAX_RETRIES: u32 = 3;
/// The fields every extraction reads (the library's default set).
const DEFAULT_FIELDS: &str = "key,fixVersions,resolution,priority,labels,issuelinks,status,subtasks,components,id,resolutiondate,issuetype,created,updated,summary,aggregatetimespent,project";
/// Histories one changelog page asks for.
const CHANGELOG_PAGE: usize = 100;
/// Changelog pages read for one issue: at most 5,000 histories.
const MAX_CHANGELOG_PAGES: usize = 50;
const TIMED_OUT: &str = "The extraction ran past its time limit and was stopped; nothing was written. Narrow the projects, the dates or the filter, and run it again.";

/// Why a read did not give data.
pub(super) enum Failure {
    /// An infrastructure failure: it becomes the tool's error.
    Client(JiraClientError),
    /// Jira refused the request (a 4xx), with its sentence.
    Refused(String),
    /// A condition the model can act on, as a sentence.
    Told(String),
}

/// What the extraction reads Jira through. [`JiraClient`] is the one real
/// reader; the tests read a scripted Jira.
#[async_trait]
pub(super) trait JiraReader: Send + Sync {
    /// The REST version the toolkit resolved.
    fn rest_version(&self) -> JiraApiVersion;

    /// `Ok(None)` when the credential works, Jira's refusal as a sentence otherwise.
    async fn check_access(&self) -> Result<Option<String>, JiraClientError>;

    /// One GET; a 4xx comes back as [`JiraRead::Refused`].
    async fn read(
        &self,
        root: JiraRoot,
        segments: &[&str],
        query: &[(&str, &str)],
    ) -> Result<JiraRead, JiraClientError>;
}

#[async_trait]
impl JiraReader for JiraClient {
    fn rest_version(&self) -> JiraApiVersion {
        JiraClient::rest_version(self)
    }

    async fn check_access(&self) -> Result<Option<String>, JiraClientError> {
        JiraClient::check_access(self).await
    }

    async fn read(
        &self,
        root: JiraRoot,
        segments: &[&str],
        query: &[(&str, &str)],
    ) -> Result<JiraRead, JiraClientError> {
        JiraClient::read(self, root, segments, query).await
    }
}

impl From<JiraClientError> for Failure {
    fn from(error: JiraClientError) -> Self {
        Self::Client(error)
    }
}

pub(super) type Outcome<T> = Result<T, Failure>;

/// The bounds of one invocation.
pub(super) struct Pace {
    pub(super) deadline: Instant,
    pub(super) retry_delay: Duration,
    /// Issues one search page asks for.
    pub(super) page_size: usize,
    /// Pages one search reads.
    pub(super) max_pages: usize,
}

impl Pace {
    /// Issues a search page asks for when nothing says otherwise.
    pub(super) const DEFAULT_PAGE_SIZE: usize = 50;
    /// The pause before the first retry; each later one is longer.
    pub(super) const DEFAULT_RETRY_DELAY: Duration = Duration::from_secs(1);

    /// The limits of one tool call: four minutes in all.
    pub(super) fn new(page_size: usize, retry_delay: Duration) -> Self {
        Self {
            deadline: Instant::now() + Duration::from_mins(4),
            retry_delay,
            page_size,
            max_pages: 1_000,
        }
    }
}

/// What to do when a search finds more than its limit.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum Overflow {
    /// Stop with a sentence.
    Fail,
    /// Keep the first issues and say so.
    Truncate,
}

/// The issues of one search.
pub(super) struct Found {
    pub(super) issues: Vec<Value>,
    pub(super) truncated: bool,
}

/// The credential check every call starts with.
pub(super) async fn access(client: &dyn JiraReader) -> Outcome<()> {
    match client.check_access().await {
        Ok(None) => Ok(()),
        Ok(Some(sentence)) => Err(Failure::Told(sentence)),
        Err(error) => Err(Failure::Client(error)),
    }
}

/// One GET, with the retries a rate limit or a blip deserves.
pub(super) async fn fetch(
    client: &dyn JiraReader,
    pace: &Pace,
    root: JiraRoot,
    segments: &[&str],
    query: &[(&str, &str)],
) -> Outcome<Value> {
    let mut attempt: u32 = 0;
    loop {
        if Instant::now() >= pace.deadline {
            return Err(Failure::Told(TIMED_OUT.to_owned()));
        }
        match client.read(root, segments, query).await {
            Ok(JiraRead::Accepted(value)) => return Ok(value),
            Ok(JiraRead::Refused(message)) => return Err(Failure::Refused(message)),
            Err(error) if error.retryable() && attempt < MAX_RETRIES => {
                attempt += 1;
                sleep(pace.retry_delay * attempt).await;
            }
            Err(error) => return Err(Failure::Client(error)),
        }
    }
}

/// Every issue a JQL query finds, up to `limit`. Pagination follows the REST
/// version: a token on v3, an offset on v2.
pub(super) async fn search(
    client: &dyn JiraReader,
    pace: &Pace,
    jql: &str,
    fields: &str,
    expand: Option<&str>,
    limit: usize,
    overflow: Overflow,
) -> Outcome<Found> {
    let by_token = client.rest_version() == JiraApiVersion::V3;
    let segments: &[&str] = if by_token {
        &["search", "jql"]
    } else {
        &["search"]
    };
    let size = pace.page_size.to_string();
    let mut issues: Vec<Value> = Vec::new();
    let mut token: Option<String> = None;
    for _ in 0..pace.max_pages {
        let start = issues.len().to_string();
        let mut query: Vec<(&str, &str)> =
            vec![("jql", jql), ("fields", fields), ("maxResults", &size)];
        if let Some(expand) = expand {
            query.push(("expand", expand));
        }
        if by_token {
            if let Some(token) = &token {
                query.push(("nextPageToken", token));
            }
        } else {
            query.push(("startAt", &start));
        }
        let document = fetch(client, pace, JiraRoot::Api, segments, &query).await?;
        let Some(batch) = document
            .get("issues")
            .and_then(Value::as_array)
            .filter(|batch| !batch.is_empty())
        else {
            break;
        };
        let room = limit.saturating_sub(issues.len());
        if batch.len() > room {
            if overflow == Overflow::Fail {
                return Err(Failure::Told(too_many(limit)));
            }
            issues.extend(batch.iter().take(room).cloned());
            return Ok(Found {
                issues,
                truncated: true,
            });
        }
        issues.extend(batch.iter().cloned());
        let granted = document
            .get("maxResults")
            .and_then(Value::as_u64)
            .and_then(|value| usize::try_from(value).ok())
            .unwrap_or(pace.page_size);
        if by_token {
            token = document
                .get("nextPageToken")
                .and_then(Value::as_str)
                .filter(|token| !token.is_empty())
                .map(ToOwned::to_owned);
            if document.get("isLast").and_then(Value::as_bool) == Some(true) || token.is_none() {
                break;
            }
        } else {
            let total = document
                .get("total")
                .and_then(Value::as_u64)
                .and_then(|value| usize::try_from(value).ok());
            if batch.len() < granted || total.is_some_and(|total| issues.len() >= total) {
                break;
            }
        }
    }
    Ok(Found {
        issues,
        truncated: false,
    })
}

fn too_many(limit: usize) -> String {
    format!(
        "The extraction would read more than {limit} issues, so it stopped and wrote nothing. Narrow the projects, the dates or add_filter, or raise the toolkit's max_issues (at most 50000)."
    )
}

/// The Jira field ids behind the configured custom columns. A configured
/// name that is no field of the instance is an error that names it.
pub(super) async fn custom_columns(
    client: &dyn JiraReader,
    pace: &Pace,
    wanted: &[(String, Vec<String>)],
) -> Outcome<Vec<CustomColumn>> {
    if wanted.iter().all(|(_, names)| names.is_empty()) {
        return Ok(wanted
            .iter()
            .map(|(column, _)| CustomColumn {
                column: column.clone(),
                field_ids: Vec::new(),
            })
            .collect());
    }
    let all = field_names(client, pace).await?;
    let mut missing: Vec<&str> = Vec::new();
    let mut columns = Vec::with_capacity(wanted.len());
    for (column, names) in wanted {
        let mut field_ids = Vec::new();
        for name in names {
            let before = field_ids.len();
            field_ids.extend(
                all.iter()
                    .filter(|(_, field_name)| field_name.eq_ignore_ascii_case(name))
                    .map(|(id, _)| id.clone()),
            );
            if field_ids.len() == before {
                missing.push(name);
            }
        }
        columns.push(CustomColumn {
            column: column.clone(),
            field_ids,
        });
    }
    if missing.is_empty() {
        Ok(columns)
    } else {
        Err(Failure::Told(format!(
            "These custom_fields names are not fields of this Jira: {}. Check the names in the toolkit's custom_fields setting (Jira's field list shows them).",
            missing.join(", ")
        )))
    }
}

/// `(id, name)` of every field of the instance.
pub(super) async fn field_names(
    client: &dyn JiraReader,
    pace: &Pace,
) -> Outcome<Vec<(String, String)>> {
    let document = fetch(client, pace, JiraRoot::Api, &["field"], &[]).await?;
    Ok(document
        .as_array()
        .map(|fields| {
            fields
                .iter()
                .filter_map(|field| {
                    Some((
                        field.get("id")?.as_str()?.to_owned(),
                        field.get("name")?.as_str()?.to_owned(),
                    ))
                })
                .collect()
        })
        .unwrap_or_default())
}

/// The histories of an issue's changelog and whether they are all of them.
/// The search expansion returns the first page of a long changelog; the rest
/// comes from the changelog endpoint, which older Jira Server versions lack
/// (the changelog is then reported incomplete).
async fn complete_changelog(
    client: &dyn JiraReader,
    pace: &Pace,
    issue: &Value,
    key: &str,
) -> Outcome<(Vec<Value>, bool)> {
    let changelog = issue.get("changelog");
    let mut histories: Vec<Value> = changelog
        .and_then(|changelog| changelog.get("histories"))
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();
    let total = changelog
        .and_then(|changelog| changelog.get("total"))
        .and_then(Value::as_u64)
        .and_then(|value| usize::try_from(value).ok())
        .unwrap_or(histories.len());
    let size = CHANGELOG_PAGE.to_string();
    for _ in 0..MAX_CHANGELOG_PAGES {
        if histories.len() >= total {
            return Ok((histories, true));
        }
        let start = histories.len().to_string();
        let reply = match fetch(
            client,
            pace,
            JiraRoot::Api,
            &["issue", key, "changelog"],
            &[("startAt", &start), ("maxResults", &size)],
        )
        .await
        {
            Ok(reply) => reply,
            Err(Failure::Refused(_)) => return Ok((histories, false)),
            Err(other) => return Err(other),
        };
        let values = reply
            .get("values")
            .and_then(Value::as_array)
            .filter(|values| !values.is_empty());
        let Some(values) = values else {
            return Ok((histories, false));
        };
        histories.extend(values.iter().cloned());
        if reply.get("isLast").and_then(Value::as_bool) == Some(true) {
            let complete = histories.len() >= total;
            return Ok((histories, complete));
        }
    }
    let complete = histories.len() >= total;
    Ok((histories, complete))
}

/// What one extraction asks for.
pub(super) struct Query<'a> {
    pub(super) projects: &'a [String],
    pub(super) based_on: ClosedBasedOn,
    pub(super) closed_status: &'a str,
    pub(super) resolved_after: NaiveDate,
    pub(super) updated_after: NaiveDate,
    pub(super) add_filter: &'a str,
}

/// A JQL string literal's content: backslashes and quotes escaped.
fn quote(text: &str) -> String {
    text.replace('\\', "\\\\").replace('"', "\\\"")
}

/// The JQL of the closed or the open query.
pub(super) fn jql(query: &Query<'_>, request: RequestType) -> String {
    let projects = query
        .projects
        .iter()
        .map(|key| format!("\"{}\"", quote(key)))
        .collect::<Vec<_>>()
        .join(", ");
    let closed = quote(query.closed_status);
    let updated = query.updated_after.format("%Y-%m-%d");
    let resolved = query.resolved_after.format("%Y-%m-%d");
    let by_status = !query.closed_status.is_empty();
    let condition = match (request, query.based_on) {
        (RequestType::Closed, ClosedBasedOn::Status) if by_status => {
            format!("status = \"{closed}\" AND updated >= \"{updated}\"")
        }
        (RequestType::Open, ClosedBasedOn::Status) if by_status => {
            format!("status != \"{closed}\" AND updated >= \"{updated}\"")
        }
        (_, ClosedBasedOn::Status) => format!("updated >= \"{updated}\""),
        (RequestType::Closed, ClosedBasedOn::Resolution) => {
            format!("resolved is not EMPTY AND resolved >= \"{resolved}\"")
        }
        (RequestType::Open, ClosedBasedOn::Resolution) => {
            format!("resolved is EMPTY AND updated >= \"{updated}\"")
        }
    };
    let filter = query.add_filter.trim();
    if filter.is_empty() {
        format!("project IN ({projects}) AND {condition}")
    } else {
        format!("project IN ({projects}) AND {condition} AND ({filter})")
    }
}

/// The issues of both queries, closed first. Reading more than `max_issues`
/// in all stops the extraction.
pub(super) async fn extract(
    client: &dyn JiraReader,
    pace: &Pace,
    wanted: &[(String, Vec<String>)],
    query: &Query<'_>,
    max_issues: usize,
) -> Outcome<Vec<Issue>> {
    access(client).await?;
    let custom = custom_columns(client, pace, wanted).await?;
    let fields = std::iter::once(DEFAULT_FIELDS.to_owned())
        .chain(
            custom
                .iter()
                .flat_map(|column| column.field_ids.iter().cloned()),
        )
        .collect::<Vec<_>>()
        .join(",");
    let mut issues: Vec<Issue> = Vec::new();
    for request in [RequestType::Closed, RequestType::Open] {
        let found = search(
            client,
            pace,
            &jql(query, request),
            &fields,
            Some("changelog"),
            max_issues.saturating_sub(issues.len()),
            Overflow::Fail,
        )
        .await?;
        for raw in &found.issues {
            let key = raw.get("key").and_then(Value::as_str).unwrap_or_default();
            let (histories, complete) = complete_changelog(client, pace, raw, key).await?;
            if let Some(issue) = parse_issue(raw, request, &custom, &histories, complete) {
                issues.push(issue);
            }
        }
    }
    crate::run_caps::report_records(u64::try_from(issues.len()).unwrap_or(u64::MAX));
    Ok(issues)
}

#[cfg(test)]
mod tests {
    use chrono::NaiveDate;

    use super::super::dataset::ClosedBasedOn;
    use super::super::records::RequestType;
    use super::{Query, jql, quote};

    fn query<'a>(
        projects: &'a [String],
        based_on: ClosedBasedOn,
        status: &'a str,
        filter: &'a str,
    ) -> Query<'a> {
        Query {
            projects,
            based_on,
            closed_status: status,
            resolved_after: NaiveDate::from_ymd_opt(2026, 1, 1).expect("a date"),
            updated_after: NaiveDate::from_ymd_opt(2026, 2, 3).expect("a date"),
            add_filter: filter,
        }
    }

    #[test]
    fn the_four_queries_follow_the_closed_rule() {
        let projects = ["ALP".to_owned(), "BET".to_owned()];
        let by_status = query(&projects, ClosedBasedOn::Status, "Done", "");
        assert_eq!(
            jql(&by_status, RequestType::Closed),
            "project IN (\"ALP\", \"BET\") AND status = \"Done\" AND updated >= \"2026-02-03\""
        );
        assert_eq!(
            jql(&by_status, RequestType::Open),
            "project IN (\"ALP\", \"BET\") AND status != \"Done\" AND updated >= \"2026-02-03\""
        );
        let by_resolution = query(&projects, ClosedBasedOn::Resolution, "Done", "");
        assert_eq!(
            jql(&by_resolution, RequestType::Closed),
            "project IN (\"ALP\", \"BET\") AND resolved is not EMPTY AND resolved >= \"2026-01-01\""
        );
        assert_eq!(
            jql(&by_resolution, RequestType::Open),
            "project IN (\"ALP\", \"BET\") AND resolved is EMPTY AND updated >= \"2026-02-03\""
        );
    }

    #[test]
    fn a_filter_is_parenthesised_and_a_status_is_quoted() {
        let projects = ["ALP".to_owned()];
        let filtered = query(
            &projects,
            ClosedBasedOn::Status,
            "Won\"t Do",
            " labels = x OR labels = y ",
        );
        assert_eq!(
            jql(&filtered, RequestType::Closed),
            "project IN (\"ALP\") AND status = \"Won\\\"t Do\" AND updated >= \"2026-02-03\" AND (labels = x OR labels = y)"
        );
        assert_eq!(quote("a\\b\"c"), "a\\\\b\\\"c");
    }
}
