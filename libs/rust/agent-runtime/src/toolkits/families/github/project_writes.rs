//! `create_issue_on_project` and `update_issue_on_project`: an issue on a
//! GitHub Project V2 board, with the board's own field values.
//!
//! The steps, answer texts and refusal sentences are the SDK's
//! (`graphql_client_wrapper.py`: `get_project`, `get_issue_repo`,
//! `get_project_fields`, `create_draft_issue`, `convert_draft_issue`,
//! `update_issue`, `update_issue_fields`, `create_issue_on_project`,
//! `update_issue_on_project`). Differences, each because the SDK's behaviour
//! is unsafe or unreadable here:
//!
//! * every GraphQL document is fixed text with variables, where the SDK
//!   interpolates ids and values into mutation strings;
//! * the creation is three requests (a draft item, its conversion to an
//!   issue, the field values) that GitHub cannot make atomic. If the
//!   conversion fails the answer says a draft item is left on the board, and
//!   if a field update fails the answer lists what was set and what was not;
//!   the SDK stops at the first failed field and reports the error text as
//!   the list of updated fields;
//! * a date that cannot be read is reported as an invalid field value; the
//!   SDK's `dateutil` fallback writes the CURRENT time instead;
//! * the board is looked up among the repository's first 20 projects (the
//!   SDK's 10), and the issue among the first 5100 board items.

use std::fmt::Write as _;

use serde_json::{Map, Value, json};

use crate::toolkits::families::python_repr::{repr, repr_str_list};

use super::client::{GitHubClient, GitHubClientErrorCode, error};
use super::writes::{Fail, Outcome, argument, optional_str, required_str, text};

const MAX_GRAPHQL_BYTES: usize = 4 * 1_024 * 1_024;
const MAX_PAGES: usize = 50;
const PAGE_SIZE: usize = 100;

const BOARD_QUERY: &str = r"
query EliteaBoard($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    id
    labels(first: 100) { nodes { id name } }
    assignableUsers(first: 100) { nodes { id name login } }
    projectsV2(first: 20) {
      nodes {
        id
        title
        number
        fields(first: 30) {
          nodes {
            ... on ProjectV2SingleSelectField { id dataType name options { id name } }
            ... on ProjectV2FieldCommon { id dataType name }
          }
        }
      }
    }
  }
}
";

const REPO_QUERY: &str = r"
query EliteaIssueRepo($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    id
    labels(first: 100) { nodes { id name } }
    assignableUsers(first: 100) { nodes { id name login } }
  }
}
";

const ITEMS_PAGE_QUERY: &str = r"
query EliteaBoardItems($owner: String!, $name: String!, $number: Int!, $count: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    projectV2(number: $number) {
      items(first: $count, after: $after) {
        nodes {
          id
          content {
            ... on Issue {
              id
              number
              repository { nameWithOwner }
              labels(first: 20) { nodes { id name } }
              assignees(first: 20) { nodes { id name } }
            }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}
";

const CREATE_DRAFT: &str = r"
mutation EliteaDraft($projectId: ID!, $title: String!, $body: String!) {
  addProjectV2DraftIssue(input: {projectId: $projectId, title: $title, body: $body}) {
    projectItem { id }
  }
}
";

const CONVERT_DRAFT: &str = r"
mutation EliteaConvert($itemId: ID!, $repositoryId: ID!) {
  convertProjectV2DraftIssueItemToIssue(input: {itemId: $itemId, repositoryId: $repositoryId}) {
    item { id content { ... on Issue { id number } } }
  }
}
";

const UPDATE_ISSUE: &str = r"
mutation EliteaUpdateIssue($issueId: ID!, $title: String!, $body: String!) {
  updateIssue(input: {id: $issueId, title: $title, body: $body}) {
    issue { id number title body }
  }
}
";

const SET_FIELD: &str = r"
mutation EliteaSetField($projectId: ID!, $itemId: ID!, $fieldId: ID!, $value: ProjectV2FieldValue!) {
  updateProjectV2ItemFieldValue(input: {projectId: $projectId, itemId: $itemId, fieldId: $fieldId, value: $value}) {
    projectV2Item { id }
  }
}
";

const CLEAR_FIELD: &str = r"
mutation EliteaClearField($projectId: ID!, $itemId: ID!, $fieldId: ID!) {
  clearProjectV2ItemFieldValue(input: {projectId: $projectId, itemId: $itemId, fieldId: $fieldId}) {
    projectV2Item { id }
  }
}
";

const ADD_LABELS: &str = r"
mutation EliteaAddLabels($id: ID!, $ids: [ID!]!) {
  addLabelsToLabelable(input: {labelableId: $id, labelIds: $ids}) { clientMutationId }
}
";

const REMOVE_LABELS: &str = r"
mutation EliteaRemoveLabels($id: ID!, $ids: [ID!]!) {
  removeLabelsFromLabelable(input: {labelableId: $id, labelIds: $ids}) { clientMutationId }
}
";

const ADD_ASSIGNEES: &str = r"
mutation EliteaAddAssignees($id: ID!, $ids: [ID!]!) {
  addAssigneesToAssignable(input: {assignableId: $id, assigneeIds: $ids}) { clientMutationId }
}
";

const REMOVE_ASSIGNEES: &str = r"
mutation EliteaRemoveAssignees($id: ID!, $ids: [ID!]!) {
  removeAssigneesFromAssignable(input: {assignableId: $id, assigneeIds: $ids}) { clientMutationId }
}
";

// ----- requests -----------------------------------------------------------------------

/// Something that ends the tool with a sentence of the SDK's (`ToolException`
/// texts) or with a classified failure.
enum Stop {
    Say(String),
    Fail(Fail),
}

impl From<Fail> for Stop {
    fn from(fail: Fail) -> Self {
        Self::Fail(fail)
    }
}

type Step<T> = Result<T, Stop>;

impl GitHubClient {
    /// A read-only GraphQL query, through the client's READ path (its retries
    /// are safe, nothing is changed): a failure is mapped as a read, so an
    /// outage is "could not be reached", never "may have been applied".
    async fn graphql_read(&self, document: &str, variables: Value) -> Result<Value, Fail> {
        let payload = json!({"query": document, "variables": variables});
        let body = self
            .rest
            .post_graphql_json(&payload, MAX_GRAPHQL_BYTES)
            .await?;
        if let Some(errors) = body
            .get("errors")
            .and_then(Value::as_array)
            .filter(|e| !e.is_empty())
        {
            return Err(graphql_failure(errors, false));
        }
        Ok(body.get("data").cloned().unwrap_or(Value::Null))
    }

    /// One GraphQL mutation. `data` of the answer, or the failure: an HTTP
    /// status by the write rules, a GraphQL `errors` list by its `type`.
    async fn graphql(&self, document: &str, variables: Value) -> Result<Value, Fail> {
        let payload = json!({"query": document, "variables": variables});
        let request = self
            .rest
            .build_graphql_request_at(&payload, std::time::SystemTime::now())?;
        let reply = self.rest.execute_effect(request, MAX_GRAPHQL_BYTES).await?;
        if let Some(failure) = reply.failure() {
            return Err(Fail::Client(failure, reply.reason()));
        }
        let body = reply.json().ok_or_else(|| {
            Fail::Client(
                error(GitHubClientErrorCode::UnknownOutcome),
                Some("GitHub answered without a readable body".to_owned()),
            )
        })?;
        if let Some(errors) = body
            .get("errors")
            .and_then(Value::as_array)
            .filter(|e| !e.is_empty())
        {
            return Err(graphql_failure(errors, true));
        }
        Ok(body.get("data").cloned().unwrap_or(Value::Null))
    }
}

/// A GraphQL `errors` list as the cause it names. For a mutation
/// (`effect`) an `INTERNAL` or `SERVICE_UNAVAILABLE` error is an unknown
/// outcome (the server may have applied it before failing); for a read it is
/// an outage and nothing was changed.
fn graphql_failure(errors: &[Value], effect: bool) -> Fail {
    let kind = |item: &Value| {
        item.pointer("/extensions/type")
            .or_else(|| item.pointer("/type"))
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_ascii_uppercase()
    };
    let code = match errors.first().map(kind).as_deref() {
        Some("NOT_FOUND") => GitHubClientErrorCode::NotFound,
        Some("FORBIDDEN" | "INSUFFICIENT_SCOPES") => GitHubClientErrorCode::Authorization,
        Some("UNAUTHORIZED") => GitHubClientErrorCode::Authentication,
        Some("RATE_LIMITED") => GitHubClientErrorCode::RateLimited,
        Some("INTERNAL" | "SERVICE_UNAVAILABLE") if effect => GitHubClientErrorCode::UnknownOutcome,
        Some("INTERNAL" | "SERVICE_UNAVAILABLE") => GitHubClientErrorCode::DependencyUnavailable,
        _ => GitHubClientErrorCode::InvalidInput,
    };
    let reason: String = errors
        .iter()
        .filter_map(|item| item.get("message").and_then(Value::as_str))
        .collect::<Vec<_>>()
        .join("; ")
        .chars()
        .filter(|character| !character.is_control())
        .take(400)
        .collect();
    Fail::Client(error(code), (!reason.is_empty()).then_some(reason))
}

/// `_format_project_error`: a permission failure says which token scope the
/// board needs; the rest keep their cause.
fn format_project_error(message: &str) -> String {
    const PERMISSION: [&str; 7] = [
        "Resource not accessible",
        "INSUFFICIENT_SCOPES",
        "not accessible by personal access token",
        "requires authentication",
        "Bad credentials",
        "401",
        "403",
    ];
    let lower = message.to_lowercase();
    if PERMISSION
        .iter()
        .any(|needle| lower.contains(&needle.to_lowercase()))
    {
        return format!(
            "GitHub Token Permission Error\n\nYour GitHub token does not have the required 'project' permission to access GitHub Projects (ProjectsV2).\n\nTo fix this:\n1. Go to GitHub Settings > Developer settings > Personal access tokens\n2. Edit your token and enable the 'project' scope (read/write access to projects)\n3. Update your toolkit configuration with the new token\n\nTechnical details: {message}"
        );
    }
    if lower.contains("not found") {
        return format!(
            "Project not found. Please verify the project title exists and is accessible. {message}"
        );
    }
    if message.contains("No repository data found") {
        return format!(
            "Repository not found or not accessible. Please verify:\n1. The repository name is correct (format: owner/repo-name)\n2. Your token has access to this repository\n\nTechnical details: {message}"
        );
    }
    format!("Failed to access GitHub project. {message}")
}

/// A lookup failure as the SDK words it: its `ToolException` text for a
/// refused or missing board, the classified failure for the rest.
fn lookup_failure(fail: Fail) -> Stop {
    match fail {
        Fail::Client(cause, reason)
            if matches!(
                cause.code(),
                GitHubClientErrorCode::Authentication
                    | GitHubClientErrorCode::Authorization
                    | GitHubClientErrorCode::NotFound
                    | GitHubClientErrorCode::InvalidInput
            ) =>
        {
            Stop::Say(format_project_error(&format!(
                "Error occurred: {}",
                reason.unwrap_or_else(|| cause.to_string())
            )))
        }
        other => Stop::Fail(other),
    }
}

fn parse_repo(value: &str) -> Step<(String, String)> {
    match value.split_once('/') {
        Some((owner, name)) if !owner.is_empty() && !name.is_empty() && !name.contains('/') => {
            Ok((owner.to_owned(), name.to_owned()))
        }
        _ => Err(Stop::Say(format!(
            "'{value}' repo format is invalid. It should be like 'org-name/repo-name'. Error: not enough values to unpack (expected 2, got {})",
            value.split('/').count()
        ))),
    }
}

fn nodes<'a>(value: &'a Value, pointer: &str) -> Vec<&'a Value> {
    value
        .pointer(pointer)
        .and_then(Value::as_array)
        .map(|nodes| nodes.iter().filter(|node| !node.is_null()).collect())
        .unwrap_or_default()
}

fn id_of(value: &Value) -> Option<String> {
    value
        .get("id")
        .and_then(Value::as_str)
        .filter(|id| !id.is_empty() && id.len() <= 256)
        .map(str::to_owned)
}

// ----- the board -------------------------------------------------------------------------------

struct Board {
    project: Value,
    project_id: String,
    repository_id: String,
    labels: Vec<(String, String)>,
    users: Vec<(String, String, String)>,
}

fn named_ids(list: &[&Value]) -> Vec<(String, String)> {
    list.iter()
        .filter_map(|node| Some((node.get("name")?.as_str()?.to_owned(), id_of(node)?)))
        .collect()
}

fn users_of(list: &[&Value]) -> Vec<(String, String, String)> {
    list.iter()
        .filter_map(|node| {
            Some((
                node.get("login")?.as_str()?.to_owned(),
                node.get("name")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_owned(),
                id_of(node)?,
            ))
        })
        .collect()
}

async fn get_board(client: &GitHubClient, owner: &str, name: &str, title: &str) -> Step<Board> {
    let data = client
        .graphql_read(BOARD_QUERY, json!({"owner": owner, "name": name}))
        .await
        .map_err(lookup_failure)?;
    let repository = data
        .get("repository")
        .filter(|repository| !repository.is_null())
        .ok_or_else(|| Stop::Say(format_project_error("No repository data found.")))?;
    let projects = nodes(repository, "/projectsV2/nodes");
    let wanted = title.to_lowercase();
    let Some(project) = projects.iter().find(|project| {
        project
            .get("title")
            .and_then(Value::as_str)
            .is_some_and(|candidate| candidate.to_lowercase() == wanted)
    }) else {
        let available: Vec<&str> = projects
            .iter()
            .filter_map(|project| project.get("title").and_then(Value::as_str))
            .filter(|title| !title.is_empty())
            .collect();
        return Err(Stop::Say(format!(
            "Project '{title}' not found. Available projects: {}",
            repr_str_list(&available)
        )));
    };
    let project_id = id_of(project).ok_or_else(|| {
        Stop::Say(format_project_error(
            "The project has no id in GitHub's answer.",
        ))
    })?;
    let repository_id = id_of(repository)
        .ok_or_else(|| Stop::Say(format_project_error("No repository data found.")))?;
    Ok(Board {
        project: (*project).clone(),
        project_id,
        repository_id,
        labels: named_ids(&nodes(repository, "/labels/nodes")),
        users: users_of(&nodes(repository, "/assignableUsers/nodes")),
    })
}

/// The repository (id, labels, assignable users) the issue lives in when it
/// is not the board's.
async fn get_issue_repo(
    client: &GitHubClient,
    owner: &str,
    name: &str,
) -> Step<(String, Vec<(String, String)>, Vec<(String, String, String)>)> {
    let data = client
        .graphql_read(REPO_QUERY, json!({"owner": owner, "name": name}))
        .await
        .map_err(lookup_failure)?;
    let repository = data
        .get("repository")
        .filter(|repository| !repository.is_null())
        .ok_or_else(|| Stop::Say(format_project_error("No repository data found.")))?;
    let id = id_of(repository)
        .ok_or_else(|| Stop::Say(format_project_error("No repository data found.")))?;
    Ok((
        id,
        named_ids(&nodes(repository, "/labels/nodes")),
        users_of(&nodes(repository, "/assignableUsers/nodes")),
    ))
}

// ----- field values ----------------------------------------------------------------------------------

#[derive(Debug, PartialEq, Eq)]
enum FieldUpdate {
    Clear {
        title: String,
        field_id: String,
    },
    SingleSelect {
        title: String,
        field_id: String,
        option_id: String,
    },
    Date {
        title: String,
        field_id: String,
        value: String,
    },
    /// Add these labels; an empty list removes every label the issue has.
    Labels {
        title: String,
        ids: Vec<String>,
    },
    Assignees {
        title: String,
        ids: Vec<String>,
    },
}

impl FieldUpdate {
    fn title(&self) -> &str {
        match self {
            Self::Clear { title, .. }
            | Self::SingleSelect { title, .. }
            | Self::Date { title, .. }
            | Self::Labels { title, .. }
            | Self::Assignees { title, .. } => title,
        }
    }
}

/// `get_project_fields`: which requested fields can be set, and why the
/// others cannot.
#[allow(
    clippy::too_many_lines,
    reason = "one arm per SDK field type, in the SDK's order"
)]
fn plan_fields(
    project: &Value,
    requested: &Map<String, Value>,
    labels: &[(String, String)],
    users: &[(String, String, String)],
) -> (Vec<FieldUpdate>, Vec<Value>) {
    let available: Vec<&Value> = nodes(project, "/fields/nodes");
    let mut updates = Vec::new();
    let mut missing = Vec::new();
    let mut note = |field: &str, reason: String| {
        missing.push(json!({"field": field, "reason": reason}));
    };
    for (name, wanted) in requested {
        let Some(field) = available
            .iter()
            .find(|field| field.get("name").and_then(Value::as_str) == Some(name.as_str()))
        else {
            note(name, "Field not found".to_owned());
            continue;
        };
        let data_type = field
            .get("dataType")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let Some(field_id) = id_of(field) else {
            note(name, "Field not found".to_owned());
            continue;
        };
        let title = name.clone();
        match data_type {
            "SINGLE_SELECT" => {
                let Some(option) = wanted.as_str() else {
                    note(name, "Option must be given as text".to_owned());
                    continue;
                };
                if option.is_empty() {
                    updates.push(FieldUpdate::Clear { title, field_id });
                    continue;
                }
                let options = nodes(field, "/options");
                let matched = options.iter().find(|candidate| {
                    candidate
                        .get("name")
                        .and_then(Value::as_str)
                        .is_some_and(|candidate| candidate.to_lowercase() == option.to_lowercase())
                });
                if let Some(option_id) = matched.and_then(|matched| id_of(matched)) {
                    updates.push(FieldUpdate::SingleSelect {
                        title,
                        field_id,
                        option_id,
                    });
                } else {
                    let names: Vec<&str> = options
                        .iter()
                        .filter_map(|candidate| candidate.get("name").and_then(Value::as_str))
                        .collect();
                    note(
                        name,
                        format!(
                            "Option '{option}' not found. Available options: {}",
                            repr_str_list(&names)
                        ),
                    );
                }
            }
            "DATE" => {
                let Some(date) = wanted.as_str() else {
                    note(
                        name,
                        "Invalid date format: the date must be given as text".to_owned(),
                    );
                    continue;
                };
                if date.is_empty() {
                    updates.push(FieldUpdate::Clear { title, field_id });
                } else if let Some(value) = utc_date(date) {
                    updates.push(FieldUpdate::Date {
                        title,
                        field_id,
                        value,
                    });
                } else {
                    note(
                        name,
                        format!(
                            "Invalid date format: '{date}' is not a date this runtime reads (use YYYY-MM-DD or YYYY-MM-DDTHH:MM:SS)"
                        ),
                    );
                }
            }
            "LABELS" | "ASSIGNEES" => {
                let names: Vec<String> = match wanted {
                    Value::String(single) => vec![single.clone()],
                    Value::Array(items) => items
                        .iter()
                        .filter_map(|item| item.as_str().map(str::to_owned))
                        .collect(),
                    _ => Vec::new(),
                };
                let labels_field = data_type == "LABELS";
                if names.is_empty() {
                    let cleared = wanted.as_array().is_some_and(Vec::is_empty);
                    if cleared {
                        updates.push(if labels_field {
                            FieldUpdate::Labels {
                                title,
                                ids: Vec::new(),
                            }
                        } else {
                            FieldUpdate::Assignees {
                                title,
                                ids: Vec::new(),
                            }
                        });
                    } else {
                        note(
                            name,
                            format!(
                                "No valid {} entries found for the provided values.",
                                data_type.to_lowercase()
                            ),
                        );
                    }
                    continue;
                }
                let ids: Vec<String> = names
                    .iter()
                    .filter_map(|wanted| {
                        if labels_field {
                            labels
                                .iter()
                                .find(|(label, _)| label == wanted)
                                .map(|(_, id)| id.clone())
                        } else {
                            users
                                .iter()
                                .find(|(login, display, _)| {
                                    login == wanted || (!display.is_empty() && display == wanted)
                                })
                                .map(|(_, _, id)| id.clone())
                        }
                    })
                    .collect();
                if ids.is_empty() {
                    note(
                        name,
                        format!(
                            "No valid {} entries found for the provided values.",
                            data_type.to_lowercase()
                        ),
                    );
                } else if labels_field {
                    updates.push(FieldUpdate::Labels { title, ids });
                } else {
                    updates.push(FieldUpdate::Assignees { title, ids });
                }
            }
            _ => note(name, "Field type not supported".to_owned()),
        }
    }
    (updates, missing)
}

/// `_convert_to_standard_utc` for the forms this runtime reads: a date, or a
/// date and time (a zone is dropped, as the SDK's `replace(tzinfo=utc)` does).
fn utc_date(input: &str) -> Option<String> {
    let input = input.trim();
    let head = input.get(..10)?;
    let date = chrono::NaiveDate::parse_from_str(head, "%Y-%m-%d").ok()?;
    let rest = input.get(10..).unwrap_or_default();
    let time = if rest.is_empty() {
        chrono::NaiveTime::from_hms_opt(0, 0, 0)?
    } else {
        let clock = rest.trim_start_matches(['T', ' ']);
        let clock: String = clock
            .chars()
            .take_while(|character| character.is_ascii_digit() || matches!(character, ':' | '.'))
            .collect();
        chrono::NaiveTime::parse_from_str(&clock, "%H:%M:%S%.f")
            .or_else(|_| chrono::NaiveTime::parse_from_str(&clock, "%H:%M"))
            .ok()?
    };
    Some(date.and_time(time).format("%Y-%m-%dT%H:%M:%SZ").to_string())
}

/// What the field updates did.
struct FieldReport {
    updated: Vec<String>,
    failed: Option<(String, String)>,
    not_attempted: Vec<String>,
}

async fn apply_fields(
    client: &GitHubClient,
    project_id: &str,
    item_id: &str,
    issue_node_id: &str,
    current_labels: &[String],
    current_assignees: &[String],
    updates: &[FieldUpdate],
) -> FieldReport {
    let mut report = FieldReport {
        updated: Vec::new(),
        failed: None,
        not_attempted: Vec::new(),
    };
    for update in updates {
        if report.failed.is_some() {
            report.not_attempted.push(update.title().to_owned());
            continue;
        }
        let outcome = apply_field(
            client,
            project_id,
            item_id,
            issue_node_id,
            current_labels,
            current_assignees,
            update,
        )
        .await;
        match outcome {
            Ok(()) => report.updated.push(update.title().to_owned()),
            Err(fail) => {
                let (_, sentence) = fail.sentence("update the field");
                report.failed = Some((update.title().to_owned(), sentence));
            }
        }
    }
    report
}

async fn apply_field(
    client: &GitHubClient,
    project_id: &str,
    item_id: &str,
    issue_node_id: &str,
    current_labels: &[String],
    current_assignees: &[String],
    update: &FieldUpdate,
) -> Result<(), Fail> {
    match update {
        FieldUpdate::Clear { field_id, .. } => {
            client
                .graphql(
                    CLEAR_FIELD,
                    json!({"projectId": project_id, "itemId": item_id, "fieldId": field_id}),
                )
                .await?;
        }
        FieldUpdate::SingleSelect {
            field_id,
            option_id,
            ..
        } => {
            client
                .graphql(
                    SET_FIELD,
                    json!({"projectId": project_id, "itemId": item_id, "fieldId": field_id,
                           "value": {"singleSelectOptionId": option_id}}),
                )
                .await?;
        }
        FieldUpdate::Date {
            field_id, value, ..
        } => {
            client
                .graphql(
                    SET_FIELD,
                    json!({"projectId": project_id, "itemId": item_id, "fieldId": field_id,
                           "value": {"date": value}}),
                )
                .await?;
        }
        FieldUpdate::Labels { ids, .. } => {
            let (document, ids) = if ids.is_empty() {
                (REMOVE_LABELS, current_labels.to_vec())
            } else {
                (ADD_LABELS, ids.clone())
            };
            if !ids.is_empty() {
                client
                    .graphql(document, json!({"id": issue_node_id, "ids": ids}))
                    .await?;
            }
        }
        FieldUpdate::Assignees { ids, .. } => {
            let (document, ids) = if ids.is_empty() {
                (REMOVE_ASSIGNEES, current_assignees.to_vec())
            } else {
                (ADD_ASSIGNEES, ids.clone())
            };
            if !ids.is_empty() {
                client
                    .graphql(document, json!({"id": issue_node_id, "ids": ids}))
                    .await?;
            }
        }
    }
    Ok(())
}

/// The SDK's tail of both tools: what was set, what could not be, plus what
/// failed or was not tried (which the SDK folded into one error string).
fn fields_message(missing: &[Value], report: &FieldReport) -> String {
    let updated = repr_str_list(&report.updated);
    let mut message = if !missing.is_empty() {
        format!(
            "Response on update fields: {updated},\nExcept for the fields: {}.",
            repr(&Value::Array(missing.to_vec()))
        )
    } else if !report.updated.is_empty() {
        format!("Response on update fields: {updated}.")
    } else {
        String::new()
    };
    if let Some((field, sentence)) = &report.failed {
        let _ = write!(message, "\nThe field '{field}' was NOT updated: {sentence}");
        if !report.not_attempted.is_empty() {
            let _ = write!(
                message,
                "\nNot attempted after it: {}.",
                repr_str_list(&report.not_attempted)
            );
        }
    }
    message
}

fn requested_fields(arguments: &Map<String, Value>) -> Result<Map<String, Value>, Fail> {
    match arguments.get("fields") {
        None | Some(Value::Null) => Ok(Map::new()),
        Some(Value::Object(fields)) => Ok(fields.clone()),
        Some(_) => Err(argument(
            "`fields` must be an object of field names to values",
        )),
    }
}

// ----- the tools ----------------------------------------------------------------------------------------

fn finish(step: Step<Value>) -> Outcome {
    match step {
        Ok(value) => Ok(value),
        Err(Stop::Say(sentence)) => text(sentence),
        Err(Stop::Fail(fail)) => Err(fail),
    }
}

pub(super) async fn create_issue_on_project(
    client: &GitHubClient,
    arguments: &Map<String, Value>,
) -> Outcome {
    let board_repo = required_str(arguments, "board_repo")?;
    let project_title = required_str(arguments, "project_title")?;
    let title = required_str(arguments, "title")?;
    let body = optional_str(arguments, "body")?.ok_or_else(|| argument("`body` is required"))?;
    let issue_repo = optional_str(arguments, "issue_repo")?.filter(|repo| !repo.trim().is_empty());
    let fields = requested_fields(arguments)?;
    finish(
        create_flow(
            client,
            board_repo,
            project_title,
            title,
            body,
            issue_repo,
            &fields,
        )
        .await,
    )
}

async fn create_flow(
    client: &GitHubClient,
    board_repo: &str,
    project_title: &str,
    title: &str,
    body: &str,
    issue_repo: Option<&str>,
    fields: &Map<String, Value>,
) -> Step<Value> {
    let (owner, name) = parse_repo(board_repo)?;
    let board = get_board(client, &owner, &name, project_title).await?;
    let (repository_id, labels, users) = match issue_repo {
        Some(repo) => {
            let (issue_owner, issue_name) = parse_repo(repo)?;
            get_issue_repo(client, &issue_owner, &issue_name).await?
        }
        None => (
            board.repository_id.clone(),
            board.labels.clone(),
            board.users.clone(),
        ),
    };
    let (updates, missing) = plan_fields(&board.project, fields, &labels, &users);

    // Step 1: a draft item on the board.
    let draft = client
        .graphql(
            CREATE_DRAFT,
            json!({"projectId": board.project_id, "title": title, "body": body}),
        )
        .await
        .map_err(|fail| match fail {
            Fail::Client(cause, _) if cause.code() == GitHubClientErrorCode::UnknownOutcome => {
                Stop::Fail(fail_unknown("create the draft issue"))
            }
            other => Stop::Say(format!(
                "Draft Issue Not Created. Error: {}",
                sentence_of(other)
            )),
        })?;
    let draft_id = draft
        .pointer("/addProjectV2DraftIssue/projectItem/id")
        .and_then(Value::as_str)
        .ok_or_else(|| {
            Stop::Say("Draft Issue Not Created. Error: Failed to create draft issue: No project item found.".to_owned())
        })?
        .to_owned();

    // Step 2: the draft becomes an issue. A failure here leaves the draft.
    let converted = client
        .graphql(
            CONVERT_DRAFT,
            json!({"itemId": draft_id, "repositoryId": repository_id}),
        )
        .await
        .map_err(|fail| {
            if matches!(&fail, Fail::Client(cause, _) if cause.code() == GitHubClientErrorCode::UnknownOutcome) {
                // The request was sent and the answer lost: the conversion may
                // have been applied. Only a definite failure says "not converted".
                Stop::Say(format!(
                    "Convert Issue Failed. Error: GitHub did not confirm the conversion, so it may or may not have been applied. A draft item '{draft_id}' was added to the project '{project_title}'; check the board (the draft, or a new issue in its place) before trying again, so the issue is not created twice."
                ))
            } else {
                Stop::Say(format!(
                    "Convert Issue Failed. Error: {} A draft item '{draft_id}' was added to the project '{project_title}' and was not converted; remove it from the board before trying again.",
                    sentence_of(fail)
                ))
            }
        })?;
    let item = converted.pointer("/convertProjectV2DraftIssueItemToIssue/item");
    let number = item
        .and_then(|item| item.pointer("/content/number"))
        .and_then(Value::as_u64);
    let issue_node_id = item
        .and_then(|item| item.pointer("/content/id"))
        .and_then(Value::as_str);
    let item_id = item.and_then(|item| item.get("id")).and_then(Value::as_str);
    let (Some(number), Some(issue_node_id), Some(item_id)) = (number, issue_node_id, item_id)
    else {
        return Err(Stop::Say(format!(
            "Convert Issue Failed. Error: Failed to convert draft issue: No issue number found. A draft item '{draft_id}' was added to the project '{project_title}'; check the board before trying again."
        )));
    };

    // Step 3: the field values, on the issue that now exists.
    let report = apply_fields(
        client,
        &board.project_id,
        item_id,
        issue_node_id,
        &[],
        &[],
        &updates,
    )
    .await;
    let mut message = format!("The issue with number '{number}' has been created.\n");
    message.push_str(&fields_message(&missing, &report));
    Ok(Value::String(message))
}

pub(super) async fn update_issue_on_project(
    client: &GitHubClient,
    arguments: &Map<String, Value>,
) -> Outcome {
    let board_repo = required_str(arguments, "board_repo")?;
    let issue_number = required_str(arguments, "issue_number")?.trim();
    let project_title = required_str(arguments, "project_title")?;
    let title = required_str(arguments, "title")?;
    let body = optional_str(arguments, "body")?.ok_or_else(|| argument("`body` is required"))?;
    let issue_repo = optional_str(arguments, "issue_repo")?.filter(|repo| !repo.trim().is_empty());
    let fields = requested_fields(arguments)?;
    finish(
        update_flow(
            client,
            board_repo,
            issue_number,
            project_title,
            title,
            body,
            issue_repo,
            &fields,
        )
        .await,
    )
}

#[allow(clippy::too_many_arguments)]
async fn update_flow(
    client: &GitHubClient,
    board_repo: &str,
    issue_number: &str,
    project_title: &str,
    title: &str,
    body: &str,
    issue_repo: Option<&str>,
    fields: &Map<String, Value>,
) -> Step<Value> {
    let (owner, name) = parse_repo(board_repo)?;
    let board = get_board(client, &owner, &name, project_title).await?;
    let (_, labels, users) = match issue_repo {
        Some(repo) => {
            let (issue_owner, issue_name) = parse_repo(repo)?;
            get_issue_repo(client, &issue_owner, &issue_name).await?
        }
        None => (
            board.repository_id.clone(),
            board.labels.clone(),
            board.users.clone(),
        ),
    };
    let (updates, missing) = plan_fields(&board.project, fields, &labels, &users);

    // Find the issue's board item by repository AND number (two repositories
    // can both have #42 on one board), paging the project's items from the
    // start: the board query read its fields only.
    let project_number = board
        .project
        .get("number")
        .and_then(Value::as_u64)
        .ok_or_else(|| {
            Stop::Say("Project number not available for pagination search.".to_owned())
        })?;
    let issue_in = issue_repo.unwrap_or(board_repo);
    let located = page_for_item(
        client,
        &owner,
        &name,
        project_number,
        issue_in,
        issue_number,
    )
    .await?;

    // The title and body first; the fields only when this succeeded.
    client
        .graphql(
            UPDATE_ISSUE,
            json!({"issueId": located.issue_node_id, "title": title, "body": body}),
        )
        .await
        .map_err(|fail| {
            if matches!(&fail, Fail::Client(cause, _) if cause.code() == GitHubClientErrorCode::UnknownOutcome) {
                Stop::Fail(fail)
            } else {
                Stop::Say(format!(
                    "Issue title and body have not updated. Error: {}",
                    sentence_of(fail)
                ))
            }
        })?;
    let report = if updates.is_empty() {
        FieldReport {
            updated: Vec::new(),
            failed: None,
            not_attempted: Vec::new(),
        }
    } else {
        apply_fields(
            client,
            &board.project_id,
            &located.item_id,
            &located.issue_node_id,
            &located.label_ids,
            &located.assignee_ids,
            &updates,
        )
        .await
    };
    let mut message = format!("The issue with number '{issue_number}' has been updated.\n");
    message.push_str(&fields_message(&missing, &report));
    Ok(Value::String(message))
}

struct Located {
    item_id: String,
    issue_node_id: String,
    label_ids: Vec<String>,
    assignee_ids: Vec<String>,
}

fn find_item(items: &[&Value], repository: &str, issue_number: &str) -> Option<Located> {
    items.iter().find_map(|item| {
        let content = item.get("content").filter(|content| !content.is_null())?;
        if content.get("number")?.as_u64()?.to_string() != issue_number {
            return None;
        }
        // The same number in another repository is another issue.
        let owner_and_name = content.pointer("/repository/nameWithOwner")?.as_str()?;
        if !owner_and_name.eq_ignore_ascii_case(repository) {
            return None;
        }
        Some(Located {
            item_id: id_of(item)?,
            issue_node_id: id_of(content)?,
            label_ids: nodes(content, "/labels/nodes")
                .iter()
                .filter_map(|node| id_of(node))
                .collect(),
            assignee_ids: nodes(content, "/assignees/nodes")
                .iter()
                .filter_map(|node| id_of(node))
                .collect(),
        })
    })
}

async fn page_for_item(
    client: &GitHubClient,
    owner: &str,
    name: &str,
    project_number: u64,
    repository: &str,
    issue_number: &str,
) -> Step<Located> {
    let mut after = Value::Null;
    for _ in 0..MAX_PAGES {
        let data = client
            .graphql_read(
                ITEMS_PAGE_QUERY,
                json!({"owner": owner, "name": name, "number": project_number,
                       "count": PAGE_SIZE, "after": after}),
            )
            .await
            .map_err(|fail| {
                Stop::Say(format!(
                    "Error occurred while searching for issue: {}",
                    sentence_of(fail)
                ))
            })?;
        let connection = data
            .pointer("/repository/projectV2/items")
            .filter(|items| !items.is_null())
            .ok_or_else(|| Stop::Say("No project data found during pagination.".to_owned()))?;
        let page: Vec<&Value> = nodes(connection, "/nodes");
        if let Some(found) = find_item(&page, repository, issue_number) {
            return Ok(found);
        }
        let more = connection
            .pointer("/pageInfo/hasNextPage")
            .and_then(Value::as_bool)
            .unwrap_or(false);
        let cursor = connection
            .pointer("/pageInfo/endCursor")
            .and_then(Value::as_str);
        match (more, cursor) {
            (true, Some(cursor)) => after = Value::String(cursor.to_owned()),
            _ => break,
        }
    }
    Err(Stop::Say(format!(
        "Issue number {issue_number} not found in project."
    )))
}

/// The classified sentence of a failure, for the SDK's `Error: ...` tails.
fn sentence_of(fail: Fail) -> String {
    fail.sentence("complete the step").1
}

fn fail_unknown(action: &str) -> Fail {
    Fail::Client(
        error(GitHubClientErrorCode::UnknownOutcome),
        Some(format!("while trying to {action}")),
    )
}

#[cfg(test)]
pub(in crate::toolkits) fn test_plan_fields(
    project: &Value,
    requested: &Map<String, Value>,
    labels: &[(String, String)],
    users: &[(String, String, String)],
) -> (usize, Vec<Value>) {
    let (updates, missing) = plan_fields(project, requested, labels, users);
    (updates.len(), missing)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_utc_date(input: &str) -> Option<String> {
    utc_date(input)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dates_are_read_in_the_forms_the_tool_documents_and_never_guessed() {
        assert_eq!(
            utc_date("2026-10-30").as_deref(),
            Some("2026-10-30T00:00:00Z")
        );
        assert_eq!(
            utc_date("2026-10-30T12:30:15").as_deref(),
            Some("2026-10-30T12:30:15Z")
        );
        assert_eq!(
            utc_date("2026-10-30 12:30:15+02:00").as_deref(),
            Some("2026-10-30T12:30:15Z"),
            "the zone is dropped, as the SDK does"
        );
        assert_eq!(
            utc_date("2026-10-30T12:30").as_deref(),
            Some("2026-10-30T12:30:00Z")
        );
        assert_eq!(utc_date("next Tuesday"), None);
        assert_eq!(utc_date("30/10/2026"), None);
        assert_eq!(utc_date("2026-13-40"), None);
    }

    #[test]
    fn project_errors_are_worded_as_the_sdk_words_them() {
        assert!(
            format_project_error(
                "Error occurred: Resource not accessible by personal access token"
            )
            .starts_with("GitHub Token Permission Error")
        );
        assert!(format_project_error("boom not found somewhere").starts_with("Project not found."));
        assert!(
            format_project_error("No repository data found.")
                .starts_with("Repository not found or not accessible.")
        );
        assert_eq!(
            format_project_error("odd"),
            "Failed to access GitHub project. odd"
        );
    }

    fn project() -> Value {
        json!({"fields": {"nodes": [
            {"id": "F1", "dataType": "SINGLE_SELECT", "name": "Status",
             "options": [{"id": "O1", "name": "Todo"}, {"id": "O2", "name": "Done"}]},
            {"id": "F2", "dataType": "DATE", "name": "Due"},
            {"id": "F3", "dataType": "LABELS", "name": "Labels"},
            {"id": "F4", "dataType": "ASSIGNEES", "name": "Assignees"},
            {"id": "F5", "dataType": "NUMBER", "name": "Estimate"},
            null
        ]}})
    }

    fn plan(requested: &Value) -> (Vec<FieldUpdate>, Vec<Value>) {
        let labels = vec![("bug".to_owned(), "L1".to_owned())];
        let users = vec![("octo".to_owned(), "Octo Cat".to_owned(), "U1".to_owned())];
        plan_fields(
            &project(),
            requested.as_object().expect("object"),
            &labels,
            &users,
        )
    }

    #[test]
    fn fields_are_matched_to_the_boards_options_labels_and_people() {
        let (updates, missing) = plan(&json!({
            "Status": "done", "Due": "2026-11-01", "Labels": ["bug", "ghost"],
            "Assignees": ["Octo Cat"]
        }));
        assert!(missing.is_empty(), "{missing:?}");
        assert!(updates.contains(&FieldUpdate::SingleSelect {
            title: "Status".into(),
            field_id: "F1".into(),
            option_id: "O2".into()
        }));
        assert!(updates.contains(&FieldUpdate::Date {
            title: "Due".into(),
            field_id: "F2".into(),
            value: "2026-11-01T00:00:00Z".into()
        }));
        assert!(updates.contains(&FieldUpdate::Labels {
            title: "Labels".into(),
            ids: vec!["L1".into()]
        }));
        assert!(updates.contains(&FieldUpdate::Assignees {
            title: "Assignees".into(),
            ids: vec!["U1".into()]
        }));
    }

    #[test]
    fn empty_values_clear_and_unusable_values_are_reported_with_the_reason() {
        let (updates, missing) = plan(&json!({
            "Status": "", "Due": "", "Labels": [], "Assignees": ["nobody"]
        }));
        assert_eq!(updates.len(), 3);
        assert!(updates.contains(&FieldUpdate::Clear {
            title: "Status".into(),
            field_id: "F1".into()
        }));
        assert!(updates.contains(&FieldUpdate::Labels {
            title: "Labels".into(),
            ids: Vec::new()
        }));
        assert_eq!(
            missing,
            [
                json!({"field": "Assignees", "reason": "No valid assignees entries found for the provided values."})
            ]
        );
        let (_, missing) =
            plan(&json!({"Status": "Blocked", "Due": "soon", "Estimate": "3", "Nope": "x"}));
        let reasons: Vec<String> = missing
            .iter()
            .map(|entry| entry["reason"].as_str().expect("reason").to_owned())
            .collect();
        assert!(
            reasons.contains(
                &"Option 'Blocked' not found. Available options: ['Todo', 'Done']".to_owned()
            ),
            "{reasons:?}"
        );
        assert!(
            reasons
                .iter()
                .any(|reason| reason.starts_with("Invalid date format: 'soon'")),
            "{reasons:?}"
        );
        assert!(reasons.contains(&"Field type not supported".to_owned()));
        assert!(reasons.contains(&"Field not found".to_owned()));
    }

    #[test]
    fn a_repo_that_is_not_owner_slash_name_is_refused_in_the_sdk_words() {
        let Err(Stop::Say(sentence)) = parse_repo("justname") else {
            panic!("refused");
        };
        assert!(sentence.starts_with(
            "'justname' repo format is invalid. It should be like 'org-name/repo-name'."
        ));
    }
}
