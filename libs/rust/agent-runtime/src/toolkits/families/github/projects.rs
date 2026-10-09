//! Bounded GitHub Project V2 read projection.
//!
//! The pinned SDK obtains the project structure and then mutates a pagination
//! accumulator. This capability-disabled Rust slice requests the same visible
//! fields and at most 100 items in one fixed query. It rejects partial GraphQL
//! responses and makes provider truncation explicit; caller input can select a
//! project on the admitted GitHub origin but cannot alter the query or endpoint.

use serde_json::{Map, Value, json};

use super::client::{
    GitHubClientError, GitHubClientErrorCode, error, invalid_response, resource_exhausted,
};

pub(super) const MAX_PROJECT_ITEMS: usize = 100;
pub(super) const MAX_PROJECT_RESPONSE_BYTES: usize = 4 * 1_024 * 1_024;
const MAX_PROJECT_OUTPUT_CHARS: usize = 200_000;
const MAX_PROJECT_FIELDS: usize = 30;
const MAX_FIELD_VALUES: usize = 30;
const MAX_LABELS: usize = 10;
const MAX_ASSIGNEES: usize = 5;
const MAX_OPTIONS: usize = 100;
const MAX_IDENTIFIER_BYTES: usize = 1_024;
const MAX_TITLE_BYTES: usize = 16 * 1_024;
const MAX_URL_BYTES: usize = 4 * 1_024;
const MAX_FIELD_TEXT_BYTES: usize = 64 * 1_024;

const LIST_PROJECT_ITEMS_QUERY: &str = r"
query EliteaProjectItems(
  $owner: String!
  $repository: String!
  $projectNumber: Int!
  $itemsCount: Int!
) {
  repository(owner: $owner, name: $repository) {
    projectV2(number: $projectNumber) {
      id
      title
      url
      fields(first: 30) {
        nodes {
          ... on ProjectV2SingleSelectField {
            id
            name
            dataType
            options { id name color }
          }
          ... on ProjectV2FieldCommon { id name dataType }
        }
      }
      items(first: $itemsCount) {
        totalCount
        pageInfo { hasNextPage }
        nodes {
          id
          type
          fieldValues(first: 30) {
            nodes {
              ... on ProjectV2ItemFieldTextValue {
                field { ... on ProjectV2FieldCommon { id name } }
                text
              }
              ... on ProjectV2ItemFieldDateValue {
                field { ... on ProjectV2FieldCommon { id name } }
                date
              }
              ... on ProjectV2ItemFieldSingleSelectValue {
                field { ... on ProjectV2FieldCommon { id name } }
                name
                optionId
              }
            }
          }
          content {
            ... on Issue {
              id number title state url
              labels(first: 10) { nodes { id name color } }
              assignees(first: 5) { nodes { id login name } }
            }
            ... on PullRequest { id number title state url }
            ... on DraftIssue { id title }
          }
        }
      }
    }
  }
}
";

pub(super) fn project_query_payload(
    owner: &str,
    repository: &str,
    project_number: u32,
    items_count: usize,
) -> Value {
    json!({
        "query": LIST_PROJECT_ITEMS_QUERY,
        "variables": {
            "owner": owner,
            "repository": repository,
            "projectNumber": project_number,
            "itemsCount": items_count
        }
    })
}

pub(super) fn project_project_issues(
    response: &Value,
    requested_items: usize,
) -> Result<Value, GitHubClientError> {
    reject_graphql_errors(response)?;
    let repository = response
        .pointer("/data/repository")
        .ok_or_else(invalid_response)?;
    if repository.is_null() {
        return Err(error(GitHubClientErrorCode::NotFound));
    }
    let project = repository.get("projectV2").ok_or_else(invalid_response)?;
    if project.is_null() {
        return Err(error(GitHubClientErrorCode::NotFound));
    }
    let project = project.as_object().ok_or_else(invalid_response)?;
    let fields = project_connection_nodes(project, "fields", MAX_PROJECT_FIELDS)?;
    let items = project
        .get("items")
        .and_then(Value::as_object)
        .ok_or_else(invalid_response)?;
    let item_nodes = bounded_nodes(items.get("nodes"), MAX_PROJECT_ITEMS)?;
    if item_nodes.len() > requested_items {
        return Err(invalid_response());
    }
    let total_count = items
        .get("totalCount")
        .and_then(Value::as_u64)
        .filter(|count| i64::try_from(*count).is_ok())
        .ok_or_else(invalid_response)?;
    let has_next_page = items
        .get("pageInfo")
        .and_then(Value::as_object)
        .and_then(|page| page.get("hasNextPage"))
        .and_then(Value::as_bool)
        .ok_or_else(invalid_response)?;
    let projected_count = u64::try_from(item_nodes.len()).map_err(|_| resource_exhausted())?;
    let output = json!({
        "id": optional_text(project.get("id"), MAX_IDENTIFIER_BYTES)?,
        "title": optional_text(project.get("title"), MAX_TITLE_BYTES)?,
        "url": optional_text(project.get("url"), MAX_URL_BYTES)?,
        "fields": project_fields(fields)?,
        "items": project_items(item_nodes)?,
        "items_total_count": total_count,
        "items_truncated": has_next_page || total_count > projected_count
    });
    bounded_output(output)
}

fn reject_graphql_errors(response: &Value) -> Result<(), GitHubClientError> {
    let Some(errors) = response.get("errors") else {
        return Ok(());
    };
    let errors = errors.as_array().ok_or_else(invalid_response)?;
    if errors.is_empty() {
        return Ok(());
    }
    let mut code = None;
    for item in errors {
        let Some(error_type) = item.pointer("/extensions/type").and_then(Value::as_str) else {
            return Err(invalid_response());
        };
        let item_code = match error_type {
            "RATE_LIMITED" => GitHubClientErrorCode::RateLimited,
            "UNAUTHORIZED" => GitHubClientErrorCode::Authentication,
            "FORBIDDEN" => GitHubClientErrorCode::Authorization,
            "NOT_FOUND" => GitHubClientErrorCode::NotFound,
            "UNPROCESSABLE" => GitHubClientErrorCode::InvalidInput,
            "INTERNAL" | "SERVICE_UNAVAILABLE" => GitHubClientErrorCode::DependencyUnavailable,
            _ => return Err(invalid_response()),
        };
        if code.is_some_and(|code| code != item_code) {
            return Err(invalid_response());
        }
        code = Some(item_code);
    }
    Err(error(code.ok_or_else(invalid_response)?))
}

fn project_connection_nodes<'a>(
    owner: &'a Map<String, Value>,
    key: &str,
    maximum: usize,
) -> Result<&'a [Value], GitHubClientError> {
    let connection = owner
        .get(key)
        .and_then(Value::as_object)
        .ok_or_else(invalid_response)?;
    bounded_nodes(connection.get("nodes"), maximum)
}

fn bounded_nodes(value: Option<&Value>, maximum: usize) -> Result<&[Value], GitHubClientError> {
    value
        .and_then(Value::as_array)
        .filter(|nodes| nodes.len() <= maximum)
        .map(Vec::as_slice)
        .ok_or_else(|| {
            if value.and_then(Value::as_array).is_some() {
                resource_exhausted()
            } else {
                invalid_response()
            }
        })
}

fn project_fields(fields: &[Value]) -> Result<Vec<Value>, GitHubClientError> {
    let mut projected = Vec::with_capacity(fields.len());
    for field in fields.iter().filter(|field| !field.is_null()) {
        let field = field.as_object().ok_or_else(invalid_response)?;
        let data_type = optional_text(field.get("dataType"), MAX_IDENTIFIER_BYTES)?;
        let mut item = Map::from_iter([
            (
                "id".to_owned(),
                optional_text(field.get("id"), MAX_IDENTIFIER_BYTES)?,
            ),
            (
                "name".to_owned(),
                optional_text(field.get("name"), MAX_IDENTIFIER_BYTES)?,
            ),
            ("dataType".to_owned(), data_type.clone()),
        ]);
        if data_type.as_str() == Some("SINGLE_SELECT") && field.contains_key("options") {
            item.insert("options".to_owned(), project_options(field.get("options"))?);
        }
        projected.push(Value::Object(item));
    }
    Ok(projected)
}

fn project_options(value: Option<&Value>) -> Result<Value, GitHubClientError> {
    let options = bounded_nodes(value, MAX_OPTIONS)?;
    let mut projected = Vec::with_capacity(options.len());
    for option in options.iter().filter(|option| !option.is_null()) {
        let option = option.as_object().ok_or_else(invalid_response)?;
        projected.push(json!({
            "id": optional_text(option.get("id"), MAX_IDENTIFIER_BYTES)?,
            "name": optional_text(option.get("name"), MAX_IDENTIFIER_BYTES)?,
            "color": optional_text(option.get("color"), MAX_IDENTIFIER_BYTES)?
        }));
    }
    Ok(Value::Array(projected))
}

fn project_items(items: &[Value]) -> Result<Vec<Value>, GitHubClientError> {
    let mut projected = Vec::with_capacity(items.len());
    for item in items.iter().filter(|item| !item.is_null()) {
        let item = item.as_object().ok_or_else(invalid_response)?;
        let mut output = Map::from_iter([
            (
                "id".to_owned(),
                optional_text(item.get("id"), MAX_IDENTIFIER_BYTES)?,
            ),
            (
                "type".to_owned(),
                optional_text(item.get("type"), MAX_IDENTIFIER_BYTES)?,
            ),
        ]);
        if let Some(content) = item.get("content").filter(|content| !content.is_null()) {
            output.insert("content".to_owned(), project_content(content)?);
        }
        if item.contains_key("fieldValues") {
            let values = project_connection_nodes(item, "fieldValues", MAX_FIELD_VALUES)?;
            output.insert(
                "fieldValues".to_owned(),
                Value::Array(project_field_values(values)?),
            );
        }
        projected.push(Value::Object(output));
    }
    Ok(projected)
}

fn project_content(content: &Value) -> Result<Value, GitHubClientError> {
    let content = content.as_object().ok_or_else(invalid_response)?;
    let mut output = Map::from_iter([
        (
            "id".to_owned(),
            optional_text(content.get("id"), MAX_IDENTIFIER_BYTES)?,
        ),
        ("number".to_owned(), optional_number(content.get("number"))?),
        (
            "title".to_owned(),
            optional_text(content.get("title"), MAX_TITLE_BYTES)?,
        ),
        (
            "url".to_owned(),
            optional_text(content.get("url"), MAX_URL_BYTES)?,
        ),
        (
            "state".to_owned(),
            optional_text(content.get("state"), MAX_IDENTIFIER_BYTES)?,
        ),
    ]);
    if content.contains_key("labels") {
        let labels = project_connection_nodes(content, "labels", MAX_LABELS)?;
        output.insert("labels".to_owned(), project_named_nodes(labels, true)?);
    }
    if content.contains_key("assignees") {
        let assignees = project_connection_nodes(content, "assignees", MAX_ASSIGNEES)?;
        output.insert(
            "assignees".to_owned(),
            project_named_nodes(assignees, false)?,
        );
    }
    Ok(Value::Object(output))
}

fn project_named_nodes(nodes: &[Value], label: bool) -> Result<Value, GitHubClientError> {
    let mut projected = Vec::with_capacity(nodes.len());
    for node in nodes.iter().filter(|node| !node.is_null()) {
        let node = node.as_object().ok_or_else(invalid_response)?;
        projected.push(if label {
            json!({
                "id": optional_text(node.get("id"), MAX_IDENTIFIER_BYTES)?,
                "name": optional_text(node.get("name"), MAX_IDENTIFIER_BYTES)?,
                "color": optional_text(node.get("color"), MAX_IDENTIFIER_BYTES)?
            })
        } else {
            json!({
                "id": optional_text(node.get("id"), MAX_IDENTIFIER_BYTES)?,
                "login": optional_text(node.get("login"), MAX_IDENTIFIER_BYTES)?,
                "name": optional_text(node.get("name"), MAX_IDENTIFIER_BYTES)?
            })
        });
    }
    Ok(Value::Array(projected))
}

fn project_field_values(values: &[Value]) -> Result<Vec<Value>, GitHubClientError> {
    let mut projected = Vec::with_capacity(values.len());
    for value in values.iter().filter(|value| !value.is_null()) {
        let value = value.as_object().ok_or_else(invalid_response)?;
        let field = value.get("field").and_then(Value::as_object);
        let mut output = Map::new();
        output.insert(
            "field".to_owned(),
            json!({
                "id": optional_text(field.and_then(|field| field.get("id")), MAX_IDENTIFIER_BYTES)?,
                "name": optional_text(field.and_then(|field| field.get("name")), MAX_IDENTIFIER_BYTES)?
            }),
        );
        for key in ["text", "date", "optionId"] {
            if value.contains_key(key) {
                output.insert(
                    key.to_owned(),
                    optional_text(value.get(key), MAX_FIELD_TEXT_BYTES)?,
                );
            }
        }
        if value.contains_key("name") && !value.contains_key("text") && !value.contains_key("date")
        {
            output.insert(
                "optionName".to_owned(),
                optional_text(value.get("name"), MAX_FIELD_TEXT_BYTES)?,
            );
        }
        projected.push(Value::Object(output));
    }
    Ok(projected)
}

fn optional_text(value: Option<&Value>, maximum: usize) -> Result<Value, GitHubClientError> {
    match value {
        None | Some(Value::Null) => Ok(Value::Null),
        Some(Value::String(text)) if text.len() <= maximum => Ok(Value::String(text.clone())),
        Some(Value::String(_)) => Err(resource_exhausted()),
        Some(_) => Err(invalid_response()),
    }
}

fn optional_number(value: Option<&Value>) -> Result<Value, GitHubClientError> {
    match value {
        None | Some(Value::Null) => Ok(Value::Null),
        Some(value)
            if value
                .as_u64()
                .is_some_and(|number| i64::try_from(number).is_ok()) =>
        {
            Ok(value.clone())
        }
        Some(_) => Err(invalid_response()),
    }
}

fn bounded_output(output: Value) -> Result<Value, GitHubClientError> {
    let characters = serde_json::to_string(&output)
        .map_err(|_| invalid_response())?
        .chars()
        .count();
    if characters > MAX_PROJECT_OUTPUT_CHARS {
        return Err(resource_exhausted());
    }
    Ok(output)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_project_issues(
    response: &Value,
    requested_items: usize,
) -> Result<Value, GitHubClientError> {
    project_project_issues(response, requested_items)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_project_query_payload(
    owner: &str,
    repository: &str,
    project_number: u32,
    items_count: usize,
) -> Value {
    project_query_payload(owner, repository, project_number, items_count)
}
