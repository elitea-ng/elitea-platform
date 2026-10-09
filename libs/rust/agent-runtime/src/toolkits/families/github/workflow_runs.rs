//! Bounded projection for GitHub Actions workflow-run status.
//!
//! The current Python SDK materializes every job from a lazy provider iterator.
//! This boundary preserves the run and job fields used by the tool while
//! requesting one explicit provider page. When more jobs exist, the result says
//! so instead of hiding pagination or growing memory without a limit.

use chrono::{DateTime, SecondsFormat};
use serde_json::{Map, Value, json};

use super::client::{GitHubClientError, invalid_response, resource_exhausted};

pub(super) const MAX_WORKFLOW_JOBS: usize = 100;
pub(super) const MAX_WORKFLOW_JOBS_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;

const MAX_OUTPUT_CHARS: usize = 200_000;
const MAX_NAME_BYTES: usize = 16 * 1_024;
const MAX_METADATA_BYTES: usize = 4 * 1_024;
const MAX_URL_BYTES: usize = 4 * 1_024;

pub(super) fn project_workflow_status(
    run: &Value,
    jobs: &Value,
    expected_run_id: u64,
) -> Result<Value, GitHubClientError> {
    let run = run.as_object().ok_or_else(invalid_response)?;
    if positive_number(run, "id")? != expected_run_id {
        return Err(invalid_response());
    }
    let jobs = jobs.as_object().ok_or_else(invalid_response)?;
    let total_jobs = nonnegative_number(jobs, "total_count")?;
    let provider_jobs = jobs
        .get("jobs")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if provider_jobs.len() > MAX_WORKFLOW_JOBS {
        return Err(resource_exhausted());
    }
    if u64::try_from(provider_jobs.len()).map_err(|_| resource_exhausted())? > total_jobs {
        return Err(invalid_response());
    }
    let projected_jobs = provider_jobs
        .iter()
        .map(|job| project_workflow_job(job, expected_run_id))
        .collect::<Result<Vec<_>, _>>()?;
    let returned_jobs = u64::try_from(projected_jobs.len()).map_err(|_| resource_exhausted())?;

    bounded_output(json!({
        "id": expected_run_id,
        "name": optional_text(run, "name", MAX_NAME_BYTES)?,
        "workflow_id": positive_number(run, "workflow_id")?,
        "event": required_text(run, "event", MAX_METADATA_BYTES)?,
        "status": optional_text(run, "status", MAX_METADATA_BYTES)?,
        "conclusion": optional_text(run, "conclusion", MAX_METADATA_BYTES)?,
        "created_at": optional_timestamp(run, "created_at")?,
        "updated_at": optional_timestamp(run, "updated_at")?,
        "head_branch": optional_text(run, "head_branch", MAX_METADATA_BYTES)?,
        "head_sha": required_sha(run, "head_sha")?,
        "jobs": projected_jobs,
        "jobs_total_count": total_jobs,
        "jobs_truncated": returned_jobs < total_jobs,
        "url": required_text(run, "html_url", MAX_URL_BYTES)?,
    }))
}

fn project_workflow_job(value: &Value, expected_run_id: u64) -> Result<Value, GitHubClientError> {
    let job = value.as_object().ok_or_else(invalid_response)?;
    if positive_number(job, "run_id")? != expected_run_id {
        return Err(invalid_response());
    }
    Ok(json!({
        "id": positive_number(job, "id")?,
        "name": required_text(job, "name", MAX_NAME_BYTES)?,
        "status": required_text(job, "status", MAX_METADATA_BYTES)?,
        "conclusion": optional_text(job, "conclusion", MAX_METADATA_BYTES)?,
        "started_at": optional_timestamp(job, "started_at")?,
        "completed_at": optional_timestamp(job, "completed_at")?,
        "url": optional_text(job, "html_url", MAX_URL_BYTES)?,
    }))
}

fn required_sha<'a>(
    object: &'a Map<String, Value>,
    field: &str,
) -> Result<&'a str, GitHubClientError> {
    let value = required_text(object, field, 64)?;
    if value.len() != 40 || !value.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return Err(invalid_response());
    }
    Ok(value)
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
    if value.is_empty() {
        return Err(invalid_response());
    }
    if value.len() > max_bytes {
        return Err(resource_exhausted());
    }
    if value.chars().any(char::is_control) {
        return Err(invalid_response());
    }
    Ok(value)
}

fn optional_text(
    object: &Map<String, Value>,
    field: &str,
    max_bytes: usize,
) -> Result<Value, GitHubClientError> {
    match object.get(field) {
        Some(Value::Null) => Ok(Value::Null),
        Some(Value::String(value)) => {
            if value.len() > max_bytes {
                return Err(resource_exhausted());
            }
            if value.chars().any(char::is_control) {
                return Err(invalid_response());
            }
            Ok(Value::String(value.clone()))
        }
        _ => Err(invalid_response()),
    }
}

fn optional_timestamp(
    object: &Map<String, Value>,
    field: &str,
) -> Result<Value, GitHubClientError> {
    let value = optional_text(object, field, MAX_METADATA_BYTES)?;
    let Value::String(value) = value else {
        return Ok(Value::Null);
    };
    DateTime::parse_from_rfc3339(&value)
        .map_err(|_| invalid_response())
        .map(|value| Value::String(value.to_rfc3339_opts(SecondsFormat::AutoSi, false)))
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

fn bounded_output(value: Value) -> Result<Value, GitHubClientError> {
    let encoded = serde_json::to_string(&value).map_err(|_| invalid_response())?;
    if encoded.chars().count() > MAX_OUTPUT_CHARS {
        return Err(resource_exhausted());
    }
    Ok(value)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_workflow_status(
    run: &Value,
    jobs: &Value,
    expected_run_id: u64,
) -> Result<Value, GitHubClientError> {
    project_workflow_status(run, jobs, expected_run_id)
}
