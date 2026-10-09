//! Bounded projection for GitHub's server-side code-search response.
//!
//! The current SDK walks a lazy paginated iterator, hardcodes the provider's
//! completeness flag and may trigger one extra content lookup per result.
//! This boundary requests one explicit page, projects only the public success
//! fields and rejects responses that exceed deterministic limits.

use serde_json::{Map, Value, json};

use super::client::{GitHubClientError, invalid_input, invalid_response, resource_exhausted};

pub(super) const MAX_CODE_SEARCH_ITEMS: usize = 100;
pub(super) const MAX_CODE_SEARCH_QUERY_BYTES: usize = 4 * 1_024;
pub(super) const MAX_CODE_SEARCH_RESPONSE_BYTES: usize = 2 * 1_024 * 1_024;
pub(super) const MAX_CODE_SEARCH_WINDOW: usize = 1_000;

const MAX_OUTPUT_CHARS: usize = 200_000;
const MAX_NAME_BYTES: usize = 1_024;
const MAX_PATH_BYTES: usize = 4 * 1_024;
const MAX_URL_BYTES: usize = 4 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 16 * 1_024;
const MAX_FRAGMENT_BYTES: usize = 16 * 1_024;
const MAX_MATCH_TEXT_BYTES: usize = 4 * 1_024;
const MAX_TEXT_MATCHES: usize = 32;
const MAX_MATCHES_PER_FRAGMENT: usize = 64;

pub(super) fn scope_code_search_query(
    query: &str,
    repository: &str,
) -> Result<String, GitHubClientError> {
    if query.trim().is_empty()
        || query.len() > MAX_CODE_SEARCH_QUERY_BYTES
        || query.chars().any(char::is_control)
    {
        return Err(invalid_input());
    }
    let lowercase = query.to_ascii_lowercase();
    let scoped = if ["repo:", "org:", "user:"]
        .iter()
        .any(|scope| lowercase.contains(scope))
    {
        query.to_owned()
    } else {
        format!("{query} repo:{repository}")
    };
    if scoped.len() > MAX_CODE_SEARCH_QUERY_BYTES {
        return Err(invalid_input());
    }
    Ok(scoped)
}

pub(super) fn validate_code_search_window(
    page: usize,
    per_page: usize,
) -> Result<(), GitHubClientError> {
    if page == 0 || !(1..=MAX_CODE_SEARCH_ITEMS).contains(&per_page) {
        return Err(invalid_input());
    }
    let offset = page
        .checked_sub(1)
        .and_then(|page| page.checked_mul(per_page))
        .ok_or_else(invalid_input)?;
    let end = offset.checked_add(per_page).ok_or_else(invalid_input)?;
    if end > MAX_CODE_SEARCH_WINDOW {
        return Err(invalid_input());
    }
    Ok(())
}

pub(super) fn project_code_search(
    value: &Value,
    page: usize,
    per_page: usize,
) -> Result<Value, GitHubClientError> {
    validate_code_search_window(page, per_page)?;
    let response = value.as_object().ok_or_else(invalid_response)?;
    let total_count = bounded_number(response, "total_count")?;
    let incomplete_results = response
        .get("incomplete_results")
        .and_then(Value::as_bool)
        .ok_or_else(invalid_response)?;
    let items = response
        .get("items")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if items.len() > per_page || items.len() > MAX_CODE_SEARCH_ITEMS {
        return Err(resource_exhausted());
    }
    let item_count = u64::try_from(items.len()).map_err(|_| resource_exhausted())?;
    if item_count > total_count {
        return Err(invalid_response());
    }
    let items = items
        .iter()
        .map(project_code_search_item)
        .collect::<Result<Vec<_>, _>>()?;
    bounded_output(json!({
        "total_count": total_count,
        "incomplete_results": incomplete_results,
        "items": items,
        "page": page,
        "per_page": per_page,
    }))
}

fn project_code_search_item(value: &Value) -> Result<Value, GitHubClientError> {
    let item = value.as_object().ok_or_else(invalid_response)?;
    let repository = item
        .get("repository")
        .and_then(Value::as_object)
        .ok_or_else(invalid_response)?;
    let mut projected = Map::new();
    projected.insert(
        "name".to_owned(),
        Value::String(required_metadata_text(item, "name", MAX_NAME_BYTES)?.to_owned()),
    );
    projected.insert(
        "path".to_owned(),
        Value::String(required_metadata_text(item, "path", MAX_PATH_BYTES)?.to_owned()),
    );
    projected.insert(
        "sha".to_owned(),
        Value::String(required_sha(item, "sha")?.to_owned()),
    );
    projected.insert(
        "html_url".to_owned(),
        Value::String(required_metadata_text(item, "html_url", MAX_URL_BYTES)?.to_owned()),
    );
    projected.insert(
        "repository".to_owned(),
        json!({
            "full_name": required_metadata_text(repository, "full_name", MAX_PATH_BYTES)?,
            "html_url": required_metadata_text(repository, "html_url", MAX_URL_BYTES)?,
            "description": optional_text(repository, "description", MAX_DESCRIPTION_BYTES)?,
            "private": repository
                .get("private")
                .and_then(Value::as_bool)
                .ok_or_else(invalid_response)?,
        }),
    );
    if let Some(matches) = project_text_matches(item)? {
        projected.insert("text_matches".to_owned(), Value::Array(matches));
    }
    Ok(Value::Object(projected))
}

fn project_text_matches(
    item: &Map<String, Value>,
) -> Result<Option<Vec<Value>>, GitHubClientError> {
    let matches = match item.get("text_matches") {
        None | Some(Value::Null) => return Ok(None),
        Some(Value::Array(matches)) if matches.is_empty() => return Ok(None),
        Some(Value::Array(matches)) => matches,
        Some(_) => return Err(invalid_response()),
    };
    if matches.len() > MAX_TEXT_MATCHES {
        return Err(resource_exhausted());
    }
    matches
        .iter()
        .map(project_text_match)
        .collect::<Result<Vec<_>, _>>()
        .map(Some)
}

fn project_text_match(value: &Value) -> Result<Value, GitHubClientError> {
    let text_match = value.as_object().ok_or_else(invalid_response)?;
    let matches = text_match
        .get("matches")
        .and_then(Value::as_array)
        .ok_or_else(invalid_response)?;
    if matches.len() > MAX_MATCHES_PER_FRAGMENT {
        return Err(resource_exhausted());
    }
    let matches = matches
        .iter()
        .map(project_match_range)
        .collect::<Result<Vec<_>, _>>()?;
    Ok(json!({
        "fragment": required_text(text_match, "fragment", MAX_FRAGMENT_BYTES)?,
        "matches": matches,
    }))
}

fn project_match_range(value: &Value) -> Result<Value, GitHubClientError> {
    let range = value.as_object().ok_or_else(invalid_response)?;
    let indices = range
        .get("indices")
        .and_then(Value::as_array)
        .filter(|indices| indices.len() == 2)
        .ok_or_else(invalid_response)?;
    let start = indices[0]
        .as_u64()
        .filter(|value| i64::try_from(*value).is_ok())
        .ok_or_else(invalid_response)?;
    let end = indices[1]
        .as_u64()
        .filter(|value| i64::try_from(*value).is_ok() && *value >= start)
        .ok_or_else(invalid_response)?;
    Ok(json!({
        "text": required_text(range, "text", MAX_MATCH_TEXT_BYTES)?,
        "indices": [start, end],
    }))
}

fn required_sha<'a>(
    object: &'a Map<String, Value>,
    field: &str,
) -> Result<&'a str, GitHubClientError> {
    let sha = required_text(object, field, 64)?;
    if !matches!(sha.len(), 40 | 64) || !sha.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return Err(invalid_response());
    }
    Ok(sha)
}

fn required_text<'a>(
    object: &'a Map<String, Value>,
    field: &str,
    max_bytes: usize,
) -> Result<&'a str, GitHubClientError> {
    let value = object
        .get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty() && !value.chars().any(unsafe_human_control))
        .ok_or_else(invalid_response)?;
    if value.len() > max_bytes {
        return Err(resource_exhausted());
    }
    Ok(value)
}

fn required_metadata_text<'a>(
    object: &'a Map<String, Value>,
    field: &str,
    max_bytes: usize,
) -> Result<&'a str, GitHubClientError> {
    let value = required_text(object, field, max_bytes)?;
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
        None | Some(Value::Null) => Ok(Value::Null),
        Some(Value::String(value))
            if value.len() <= max_bytes && !value.chars().any(unsafe_human_control) =>
        {
            Ok(Value::String(value.clone()))
        }
        Some(Value::String(value)) if value.len() > max_bytes => Err(resource_exhausted()),
        Some(_) => Err(invalid_response()),
    }
}

fn unsafe_human_control(character: char) -> bool {
    character.is_control() && !matches!(character, '\n' | '\r' | '\t')
}

fn bounded_number(object: &Map<String, Value>, field: &str) -> Result<u64, GitHubClientError> {
    object
        .get(field)
        .and_then(Value::as_u64)
        .filter(|value| i64::try_from(*value).is_ok())
        .ok_or_else(invalid_response)
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
pub(in crate::toolkits) fn test_scope_code_search_query(
    query: &str,
    repository: &str,
) -> Result<String, GitHubClientError> {
    scope_code_search_query(query, repository)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_code_search(
    value: &Value,
    page: usize,
    per_page: usize,
) -> Result<Value, GitHubClientError> {
    project_code_search(value, page, per_page)
}
