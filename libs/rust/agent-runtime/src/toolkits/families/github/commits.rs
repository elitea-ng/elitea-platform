//! Bounded projections for GitHub commit listing, inspection, and comparison.
//!
//! GitHub's commit and compare payloads can contain large patches and partially
//! paginated collections. This module keeps the current SDK success fields but
//! fails on ambiguous completeness, normalizes timestamps, supplies the SDK's
//! documented `Unknown` author fallback, and enforces one deterministic output
//! budget instead of returning silently incomplete data.

use chrono::{DateTime, SecondsFormat};
use serde_json::{Map, Value, json};

use super::client::{GitHubClientError, invalid_response, resource_exhausted};

pub(super) const MAX_COMMITS: usize = 100;
pub(super) const MAX_COMMIT_FILES: usize = 300;
pub(super) const COMMIT_FILES_PER_PAGE: usize = 100;
pub(super) const MAX_COMPARE_FILES: usize = 299;

const MAX_OUTPUT_CHARS: usize = 200_000;
const MAX_MESSAGE_BYTES: usize = 128 * 1_024;
const MAX_PATCH_BYTES: usize = 64 * 1_024;
const MAX_METADATA_BYTES: usize = 4 * 1_024;
const MAX_URL_BYTES: usize = 4 * 1_024;

pub(super) fn project_commit_list(
    value: &Value,
    max_count: usize,
) -> Result<Value, GitHubClientError> {
    if max_count == 0 || max_count > MAX_COMMITS {
        return Err(resource_exhausted());
    }
    let commits = value.as_array().ok_or_else(invalid_response)?;
    if commits.len() > max_count {
        return Err(resource_exhausted());
    }
    let projected = commits
        .iter()
        .map(project_commit_summary)
        .collect::<Result<Vec<_>, _>>()?;
    bounded_output(Value::Array(projected))
}

pub(super) fn commit_response_sha(value: &Value) -> Result<String, GitHubClientError> {
    let commit = value.as_object().ok_or_else(invalid_response)?;
    response_sha(commit).map(ToOwned::to_owned)
}

pub(super) fn append_commit_file_page(
    value: &Value,
    expected_sha: &str,
    files: &mut Vec<Value>,
) -> Result<usize, GitHubClientError> {
    let commit = value.as_object().ok_or_else(invalid_response)?;
    if response_sha(commit)? != expected_sha {
        return Err(invalid_response());
    }
    let page = commit
        .get("files")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if page.len() > COMMIT_FILES_PER_PAGE
        || files.len().saturating_add(page.len()) > MAX_COMMIT_FILES
    {
        return Err(resource_exhausted());
    }
    for file in page {
        files.push(project_changed_file(file)?);
    }
    Ok(page.len())
}

pub(super) fn finish_commit_changes(
    first_page: &Value,
    files: &[Value],
) -> Result<Value, GitHubClientError> {
    let commit = first_page.as_object().ok_or_else(invalid_response)?;
    let nested = nested_commit(commit)?;
    let (author, date) = author_and_date(commit)?;
    let (additions, deletions) = changed_file_totals(files)?;
    let file_count = files.len();
    bounded_output(json!({
        "commit_sha": response_sha(commit)?,
        "commit_message": required_text(nested, "message", MAX_MESSAGE_BYTES)?,
        "author": author,
        "date": date,
        "total_files_changed": file_count,
        "total_additions": additions,
        "total_deletions": deletions,
        "files": files,
    }))
}

pub(super) fn project_commit_comparison(value: &Value) -> Result<Value, GitHubClientError> {
    let comparison = value.as_object().ok_or_else(invalid_response)?;
    let status = required_text(comparison, "status", MAX_METADATA_BYTES)?;
    if !matches!(status, "ahead" | "behind" | "identical" | "diverged") {
        return Err(invalid_response());
    }
    let commits = comparison
        .get("commits")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    let total_commits = nonnegative_number(comparison, "total_commits")?;
    let total_commits_usize = usize::try_from(total_commits).map_err(|_| resource_exhausted())?;
    if total_commits_usize > MAX_COMMITS || commits.len() != total_commits_usize {
        return Err(resource_exhausted());
    }
    let files = comparison
        .get("files")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if files.len() > MAX_COMPARE_FILES {
        return Err(resource_exhausted());
    }
    let files = files
        .iter()
        .map(project_changed_file)
        .collect::<Result<Vec<_>, _>>()?;
    let projected_commits = commits
        .iter()
        .map(project_commit_summary)
        .collect::<Result<Vec<_>, _>>()?;
    let base = comparison.get("base_commit").ok_or_else(invalid_response)?;
    let head = if let Some(head) = commits.last() {
        head
    } else if status == "identical" {
        base
    } else if status == "behind" {
        comparison
            .get("merge_base_commit")
            .ok_or_else(invalid_response)?
    } else {
        return Err(invalid_response());
    };
    let (additions, deletions) = changed_file_totals(&files)?;
    let file_count = files.len();
    bounded_output(json!({
        "base_commit": project_commit_identity(base)?,
        "head_commit": project_commit_identity(head)?,
        "status": status,
        "ahead_by": nonnegative_number(comparison, "ahead_by")?,
        "behind_by": nonnegative_number(comparison, "behind_by")?,
        "total_commits": total_commits,
        "commits": projected_commits,
        "files": files,
        "summary": {
            "total_files_changed": file_count,
            "total_additions": additions,
            "total_deletions": deletions,
        }
    }))
}

fn project_commit_summary(value: &Value) -> Result<Value, GitHubClientError> {
    let commit = value.as_object().ok_or_else(invalid_response)?;
    let nested = nested_commit(commit)?;
    let (author, date) = author_and_date(commit)?;
    Ok(json!({
        "sha": response_sha(commit)?,
        "author": author,
        "date": date,
        "message": required_text(nested, "message", MAX_MESSAGE_BYTES)?,
        "url": required_text(commit, "html_url", MAX_URL_BYTES)?,
    }))
}

fn project_commit_identity(value: &Value) -> Result<Value, GitHubClientError> {
    let commit = value.as_object().ok_or_else(invalid_response)?;
    let nested = nested_commit(commit)?;
    let (author, date) = author_and_date(commit)?;
    Ok(json!({
        "sha": response_sha(commit)?,
        "message": required_text(nested, "message", MAX_MESSAGE_BYTES)?,
        "author": author,
        "date": date,
    }))
}

fn project_changed_file(value: &Value) -> Result<Value, GitHubClientError> {
    let file = value.as_object().ok_or_else(invalid_response)?;
    let status = required_text(file, "status", MAX_METADATA_BYTES)?;
    if !matches!(
        status,
        "added" | "removed" | "modified" | "renamed" | "copied" | "changed" | "unchanged"
    ) {
        return Err(invalid_response());
    }
    let mut projected = Map::new();
    projected.insert(
        "filename".to_owned(),
        Value::String(required_text(file, "filename", MAX_METADATA_BYTES)?.to_owned()),
    );
    projected.insert("status".to_owned(), Value::String(status.to_owned()));
    for field in ["additions", "deletions", "changes"] {
        projected.insert(
            field.to_owned(),
            Value::from(nonnegative_number(file, field)?),
        );
    }
    for field in ["patch", "blob_url", "raw_url"] {
        let max_bytes = if field == "patch" {
            MAX_PATCH_BYTES
        } else {
            MAX_URL_BYTES
        };
        projected.insert(field.to_owned(), optional_text(file, field, max_bytes)?);
    }
    if status == "renamed" {
        projected.insert(
            "previous_filename".to_owned(),
            Value::String(required_text(file, "previous_filename", MAX_METADATA_BYTES)?.to_owned()),
        );
    }
    Ok(Value::Object(projected))
}

fn author_and_date(commit: &Map<String, Value>) -> Result<(String, Value), GitHubClientError> {
    let nested = nested_commit(commit)?;
    match nested.get("author") {
        Some(Value::Object(author)) => {
            let name = author
                .get("name")
                .and_then(Value::as_str)
                .filter(|name| !name.is_empty() && name.len() <= MAX_METADATA_BYTES)
                .unwrap_or("Unknown")
                .to_owned();
            let date = optional_timestamp(author, "date")?;
            Ok((name, date))
        }
        None | Some(Value::Null) => {
            let name = commit
                .get("author")
                .and_then(Value::as_object)
                .and_then(|author| author.get("login"))
                .and_then(Value::as_str)
                .filter(|login| !login.is_empty() && login.len() <= MAX_METADATA_BYTES)
                .unwrap_or("Unknown")
                .to_owned();
            Ok((name, Value::Null))
        }
        Some(_) => Err(invalid_response()),
    }
}

fn changed_file_totals(files: &[Value]) -> Result<(u64, u64), GitHubClientError> {
    files.iter().try_fold((0_u64, 0_u64), |totals, file| {
        let file = file.as_object().ok_or_else(invalid_response)?;
        let additions = file
            .get("additions")
            .and_then(Value::as_u64)
            .ok_or_else(invalid_response)?;
        let deletions = file
            .get("deletions")
            .and_then(Value::as_u64)
            .ok_or_else(invalid_response)?;
        Ok((
            totals
                .0
                .checked_add(additions)
                .ok_or_else(resource_exhausted)?,
            totals
                .1
                .checked_add(deletions)
                .ok_or_else(resource_exhausted)?,
        ))
    })
}

fn nested_commit(commit: &Map<String, Value>) -> Result<&Map<String, Value>, GitHubClientError> {
    commit
        .get("commit")
        .and_then(Value::as_object)
        .ok_or_else(invalid_response)
}

fn response_sha(commit: &Map<String, Value>) -> Result<&str, GitHubClientError> {
    let sha = required_text(commit, "sha", MAX_METADATA_BYTES)?;
    if !matches!(sha.len(), 40 | 64) || !sha.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return Err(invalid_response());
    }
    Ok(sha)
}

fn optional_timestamp(
    object: &Map<String, Value>,
    field: &str,
) -> Result<Value, GitHubClientError> {
    match object.get(field) {
        None | Some(Value::Null) => Ok(Value::Null),
        Some(Value::String(value)) if value.len() <= MAX_METADATA_BYTES => {
            DateTime::parse_from_rfc3339(value)
                .map_err(|_| invalid_response())
                .map(|timestamp| {
                    Value::String(timestamp.to_rfc3339_opts(SecondsFormat::AutoSi, false))
                })
        }
        Some(Value::String(_)) => Err(resource_exhausted()),
        Some(_) => Err(invalid_response()),
    }
}

fn optional_text(
    object: &Map<String, Value>,
    field: &str,
    max_bytes: usize,
) -> Result<Value, GitHubClientError> {
    match object.get(field) {
        None | Some(Value::Null) => Ok(Value::Null),
        Some(Value::String(value)) if value.len() <= max_bytes => Ok(Value::String(value.clone())),
        Some(Value::String(_)) => Err(resource_exhausted()),
        Some(_) => Err(invalid_response()),
    }
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
    if characters > MAX_OUTPUT_CHARS {
        return Err(resource_exhausted());
    }
    Ok(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_commit_list(
    value: &Value,
    max_count: usize,
) -> Result<Value, GitHubClientError> {
    project_commit_list(value, max_count)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_commit_changes(
    pages: &[Value],
) -> Result<Value, GitHubClientError> {
    let first = pages.first().ok_or_else(invalid_response)?;
    let expected_sha = commit_response_sha(first)?;
    let mut files = Vec::new();
    for page in pages {
        let _ = append_commit_file_page(page, &expected_sha, &mut files)?;
    }
    finish_commit_changes(first, &files)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_commit_comparison(
    value: &Value,
) -> Result<Value, GitHubClientError> {
    project_commit_comparison(value)
}
