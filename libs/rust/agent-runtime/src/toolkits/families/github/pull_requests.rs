//! Bounded projection for the current GitHub pull-request inspection tools.
//!
//! The Python SDK token-counts fields independently and double-stringifies
//! comments and commit summaries. This module preserves the business fields
//! while producing deterministic typed JSON: every admitted field is present,
//! collections are capped, and an over-limit response fails instead of becoming
//! a silently partial model-visible result. Changed-file completeness is bound
//! to the PR resource's declared `changed_files` count before pagination.

use chrono::{DateTime, SecondsFormat};
use serde_json::{Map, Value, json};

use super::client::{GitHubClientError, invalid_response, resource_exhausted};

pub(super) const MAX_PULL_REQUESTS: usize = 100;
pub(super) const MAX_PULL_REQUEST_FILES: usize = 300;
pub(super) const PULL_REQUEST_FILES_PER_PAGE: usize = 100;
pub(super) const MAX_PULL_REQUEST_OUTPUT_CHARS: usize = 200_000;
pub(super) const MAX_PULL_REQUEST_BODY_BYTES: usize = 128 * 1_024;
pub(super) const MAX_PULL_REQUEST_PATCH_BYTES: usize = 64 * 1_024;

const MAX_TITLE_BYTES: usize = 16 * 1_024;
const MAX_METADATA_BYTES: usize = 4 * 1_024;
const MAX_URL_BYTES: usize = 4 * 1_024;
const MAX_DETAIL_ITEMS: usize = 10;

pub(super) fn project_pull_request_list(
    value: &Value,
    max_count: usize,
) -> Result<Value, GitHubClientError> {
    if max_count == 0 || max_count > MAX_PULL_REQUESTS {
        return Err(resource_exhausted());
    }
    let pulls = value.as_array().ok_or_else(invalid_response)?;
    if pulls.len() > max_count {
        return Err(resource_exhausted());
    }
    let projected = pulls
        .iter()
        .map(project_pull_request_summary)
        .collect::<Result<Vec<_>, _>>()?;
    bounded_output(Value::Array(projected))
}

pub(super) fn project_pull_request_detail(
    pull: &Value,
    comments: &Value,
    commits: &Value,
    expected_number: u64,
) -> Result<Value, GitHubClientError> {
    let pull = pull.as_object().ok_or_else(invalid_response)?;
    if positive_number(pull, "number")? != expected_number {
        return Err(invalid_response());
    }
    let comments = project_comments(comments)?;
    let commits = project_commits(commits)?;
    let mut projected = Map::new();
    projected.insert(
        "title".to_owned(),
        Value::String(required_text(pull, "title", MAX_TITLE_BYTES)?.to_owned()),
    );
    projected.insert("number".to_owned(), Value::from(expected_number));
    projected.insert(
        "body".to_owned(),
        optional_text(pull, "body", MAX_PULL_REQUEST_BODY_BYTES)?,
    );
    projected.insert(
        "pr_url".to_owned(),
        Value::String(required_text(pull, "html_url", MAX_URL_BYTES)?.to_owned()),
    );
    projected.insert(
        "state".to_owned(),
        Value::String(pull_state(pull)?.to_owned()),
    );
    projected.insert(
        "head".to_owned(),
        Value::String(nested_ref(pull, "head")?.to_owned()),
    );
    projected.insert(
        "base".to_owned(),
        Value::String(nested_ref(pull, "base")?.to_owned()),
    );
    projected.insert("comments".to_owned(), comments);
    projected.insert("commits".to_owned(), commits);
    bounded_output(Value::Object(projected))
}

pub(super) fn pull_request_file_count(
    value: &Value,
    expected_number: u64,
) -> Result<usize, GitHubClientError> {
    let pull = value.as_object().ok_or_else(invalid_response)?;
    if positive_number(pull, "number")? != expected_number {
        return Err(invalid_response());
    }
    let count = pull
        .get("changed_files")
        .and_then(Value::as_u64)
        .and_then(|count| usize::try_from(count).ok())
        .ok_or_else(invalid_response)?;
    if count > MAX_PULL_REQUEST_FILES {
        return Err(resource_exhausted());
    }
    Ok(count)
}

pub(super) fn append_pull_request_file_page(
    value: &Value,
    files: &mut Vec<Value>,
) -> Result<(), GitHubClientError> {
    let page = value.as_array().ok_or_else(invalid_response)?;
    if page.len() > PULL_REQUEST_FILES_PER_PAGE
        || files.len().saturating_add(page.len()) > MAX_PULL_REQUEST_FILES
    {
        return Err(resource_exhausted());
    }
    for file in page {
        files.push(project_pull_request_file(file)?);
    }
    Ok(())
}

pub(super) fn finish_pull_request_files(
    files: Vec<Value>,
    expected_count: usize,
) -> Result<Value, GitHubClientError> {
    if files.len() != expected_count {
        return Err(invalid_response());
    }
    bounded_output(Value::Array(files))
}

fn project_pull_request_summary(value: &Value) -> Result<Value, GitHubClientError> {
    let pull = value.as_object().ok_or_else(invalid_response)?;
    Ok(json!({
        "number": positive_number(pull, "number")?,
        "title": required_text(pull, "title", MAX_TITLE_BYTES)?,
        "state": pull_state(pull)?,
        "created_at": optional_timestamp(pull, "created_at")?,
        "updated_at": optional_timestamp(pull, "updated_at")?,
        "html_url": required_text(pull, "html_url", MAX_URL_BYTES)?,
        "user": nested_optional_text(pull, "user", "login", MAX_METADATA_BYTES)?,
        "head": nested_ref(pull, "head")?,
        "base": nested_ref(pull, "base")?,
    }))
}

fn project_comments(value: &Value) -> Result<Value, GitHubClientError> {
    let comments = bounded_array(value, MAX_DETAIL_ITEMS)?;
    comments
        .iter()
        .map(|comment| {
            let comment = comment.as_object().ok_or_else(invalid_response)?;
            Ok(json!({
                "body": optional_text(comment, "body", MAX_PULL_REQUEST_BODY_BYTES)?,
                "user": nested_optional_text(comment, "user", "login", MAX_METADATA_BYTES)?,
            }))
        })
        .collect::<Result<Vec<_>, _>>()
        .map(Value::Array)
}

fn project_commits(value: &Value) -> Result<Value, GitHubClientError> {
    let commits = bounded_array(value, MAX_DETAIL_ITEMS)?;
    commits
        .iter()
        .map(|commit| {
            let commit = commit.as_object().ok_or_else(invalid_response)?;
            let nested = commit
                .get("commit")
                .and_then(Value::as_object)
                .ok_or_else(invalid_response)?;
            Ok(json!({
                "message": required_text(nested, "message", MAX_PULL_REQUEST_BODY_BYTES)?,
            }))
        })
        .collect::<Result<Vec<_>, _>>()
        .map(Value::Array)
}

fn project_pull_request_file(value: &Value) -> Result<Value, GitHubClientError> {
    let file = value.as_object().ok_or_else(invalid_response)?;
    let path = required_text(file, "filename", MAX_METADATA_BYTES)?;
    let status = required_text(file, "status", MAX_METADATA_BYTES)?;
    if !matches!(
        status,
        "added" | "removed" | "modified" | "renamed" | "copied" | "changed" | "unchanged"
    ) {
        return Err(invalid_response());
    }
    Ok(json!({
        "path": path,
        "patch": optional_text(file, "patch", MAX_PULL_REQUEST_PATCH_BYTES)?,
        "filename": path,
        "status": status,
        "additions": nonnegative_number(file, "additions")?,
        "deletions": nonnegative_number(file, "deletions")?,
        "changes": nonnegative_number(file, "changes")?,
    }))
}

fn bounded_array(value: &Value, max_items: usize) -> Result<&[Value], GitHubClientError> {
    let values = value.as_array().ok_or_else(invalid_response)?;
    if values.len() > max_items {
        return Err(resource_exhausted());
    }
    Ok(values)
}

fn pull_state(pull: &Map<String, Value>) -> Result<&str, GitHubClientError> {
    let state = required_text(pull, "state", MAX_METADATA_BYTES)?;
    matches!(state, "open" | "closed")
        .then_some(state)
        .ok_or_else(invalid_response)
}

fn nested_ref<'a>(
    object: &'a Map<String, Value>,
    field: &str,
) -> Result<&'a str, GitHubClientError> {
    let nested = object
        .get(field)
        .and_then(Value::as_object)
        .ok_or_else(invalid_response)?;
    required_text(nested, "ref", MAX_METADATA_BYTES)
}

fn nested_optional_text(
    object: &Map<String, Value>,
    parent: &str,
    field: &str,
    max_bytes: usize,
) -> Result<Value, GitHubClientError> {
    match object.get(parent) {
        Some(Value::Null) => Ok(Value::Null),
        Some(Value::Object(nested)) => {
            required_text(nested, field, max_bytes).map(|value| Value::String(value.to_owned()))
        }
        _ => Err(invalid_response()),
    }
}

fn optional_timestamp(
    object: &Map<String, Value>,
    field: &str,
) -> Result<Value, GitHubClientError> {
    let Some(value) = object.get(field) else {
        return Err(invalid_response());
    };
    if value.is_null() {
        return Ok(Value::Null);
    }
    let value = value.as_str().ok_or_else(invalid_response)?;
    if value.len() > MAX_METADATA_BYTES {
        return Err(resource_exhausted());
    }
    DateTime::parse_from_rfc3339(value)
        .map_err(|_| invalid_response())
        .map(|value| Value::String(value.to_rfc3339_opts(SecondsFormat::AutoSi, false)))
}

fn optional_text(
    object: &Map<String, Value>,
    field: &str,
    max_bytes: usize,
) -> Result<Value, GitHubClientError> {
    match object.get(field) {
        Some(Value::Null) => Ok(Value::Null),
        Some(Value::String(value)) if value.len() <= max_bytes => Ok(Value::String(value.clone())),
        Some(Value::String(_)) => Err(resource_exhausted()),
        _ => Err(invalid_response()),
    }
}

fn positive_number(object: &Map<String, Value>, field: &str) -> Result<u64, GitHubClientError> {
    object
        .get(field)
        .and_then(Value::as_u64)
        .filter(|value| *value > 0 && i64::try_from(*value).is_ok())
        .ok_or_else(invalid_response)
}

fn nonnegative_number(object: &Map<String, Value>, field: &str) -> Result<u64, GitHubClientError> {
    object
        .get(field)
        .and_then(Value::as_u64)
        .filter(|value| i64::try_from(*value).is_ok())
        .ok_or_else(invalid_response)
}

fn required_text<'a>(
    object: &'a Map<String, Value>,
    field: &str,
    max_bytes: usize,
) -> Result<&'a str, GitHubClientError> {
    let value = object
        .get(field)
        .and_then(Value::as_str)
        .ok_or_else(invalid_response)?;
    if value.len() > max_bytes {
        return Err(resource_exhausted());
    }
    Ok(value)
}

fn bounded_output(value: Value) -> Result<Value, GitHubClientError> {
    let characters = serde_json::to_string(&value)
        .map_err(|_| invalid_response())?
        .chars()
        .count();
    if characters > MAX_PULL_REQUEST_OUTPUT_CHARS {
        return Err(resource_exhausted());
    }
    Ok(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_pull_request_list(
    value: &Value,
    max_count: usize,
) -> Result<Value, GitHubClientError> {
    project_pull_request_list(value, max_count)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_pull_request_detail(
    pull: &Value,
    comments: &Value,
    commits: &Value,
    expected_number: u64,
) -> Result<Value, GitHubClientError> {
    project_pull_request_detail(pull, comments, commits, expected_number)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_pull_request_files(
    pull: &Value,
    pages: &[Value],
    expected_number: u64,
) -> Result<Value, GitHubClientError> {
    let expected = pull_request_file_count(pull, expected_number)?;
    let mut files = Vec::new();
    for page in pages {
        append_pull_request_file_page(page, &mut files)?;
    }
    finish_pull_request_files(files, expected)
}
