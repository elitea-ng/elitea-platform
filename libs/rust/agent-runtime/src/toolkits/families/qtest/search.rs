//! DQL search, lookups and the SDK's result parsers (`__parse_data`,
//! `__parse_entity_item`, `__format_property_value`).

use adk_core::AdkError;
use reqwest::Method;
use serde_json::{Value, json};

use crate::toolkits::families::python_repr::{PyValue, str_of};

use super::client::{
    QtestCall, QtestClientError, QtestClientErrorCode, invalid_response, resource_exhausted,
};
use super::html::{clean_html, strip_tags, unescape};
use super::tools::{Failure, Qtest};

/// `no_of_items_per_page`.
const PAGE_SIZE: u64 = 100;
/// The SDK's `max_results` default.
pub(super) const DEFAULT_MAX_RESULTS: i64 = 20;
const MAX_SEARCH_PAGES: u64 = 50;
pub(super) const QTEST_ID: &str = "QTest Id";

/// Entity types with ID prefixes, in `QTEST_OBJECT_TYPES` order.
pub(super) const OBJECT_TYPES: [(&str, &str, &str); 8] = [
    ("test-cases", "TC", "Test Case"),
    ("test-runs", "TR", "Test Run"),
    ("defects", "DF", "Defect"),
    ("requirements", "RQ", "Requirement"),
    ("test-suites", "TS", "Test Suite"),
    ("test-cycles", "CL", "Test Cycle"),
    ("releases", "RL", "Release"),
    ("builds", "BL", "Build"),
];
/// Searchable without an ID prefix.
pub(super) const SEARCH_ONLY_TYPES: [(&str, &str); 1] = [("test-logs", "Test Log")];

/// Paging for `__perform_search_by_dql`.
pub(super) struct SearchOptions {
    pub(super) max_results: i64,
    pub(super) append_test_steps: bool,
    pub(super) include_external_properties: bool,
}

impl Default for SearchOptions {
    fn default() -> Self {
        Self {
            max_results: DEFAULT_MAX_RESULTS,
            append_test_steps: false,
            include_external_properties: false,
        }
    }
}

/// A DQL value inside single quotes may not hold a quote.
pub(super) fn quoted(value: &str) -> Result<String, Failure> {
    if value.contains('\'') || value.chars().any(char::is_control) {
        return Err(Failure::Adk(super::tools::invalid_arguments()));
    }
    Ok(format!("'{value}'"))
}

impl Qtest {
    /// `SearchApi.search_artifact` with `fields` and optional paging.
    pub(super) async fn search(
        &self,
        object_type: &str,
        query: &str,
        fields: Value,
        paging: Option<(u64, u64, &SearchOptions)>,
    ) -> Result<Value, AdkError> {
        self.search_raw(object_type, query, fields, paging)
            .await
            .map_err(QtestClientError::into_adk)
    }

    async fn search_raw(
        &self,
        object_type: &str,
        query: &str,
        fields: Value,
        paging: Option<(u64, u64, &SearchOptions)>,
    ) -> Result<Value, QtestClientError> {
        let mut call = QtestCall::send(
            Method::POST,
            "search",
            json!({"object_type": object_type, "fields": fields, "query": query}),
            false,
        );
        if let Some((page_size, page, options)) = paging {
            call = call
                .query("appendTestSteps", options.append_test_steps.to_string())
                .query(
                    "includeExternalProperties",
                    options.include_external_properties.to_string(),
                )
                .query("pageSize", page_size.to_string())
                .query("page", page.to_string());
        }
        let document = self.client.call(call).await?;
        if document.is_object() {
            Ok(document)
        } else {
            Err(invalid_response())
        }
    }

    /// `__perform_search_by_dql`: test cases, 100 per page from page 1,
    /// stopping at `max_results` (`<= 0` is unlimited). The SDK always asks
    /// for page 2 when a `next` link remains; Rust follows the pages.
    pub(super) async fn search_test_cases(
        &self,
        dql: &str,
        options: &SearchOptions,
    ) -> Result<Vec<PyValue>, AdkError> {
        let unlimited = options.max_results <= 0;
        let max = usize::try_from(options.max_results.max(0)).unwrap_or(usize::MAX);
        let mut rows = Vec::new();
        let mut page = 1;
        loop {
            let response = self
                .search(
                    "test-cases",
                    dql,
                    json!(["*"]),
                    Some((PAGE_SIZE, page, options)),
                )
                .await?;
            for item in response
                .get("items")
                .and_then(Value::as_array)
                .ok_or_else(|| invalid_response().into_adk())?
            {
                rows.push(parse_test_case(item));
            }
            if !unlimited && rows.len() >= max {
                break;
            }
            let next = response
                .get("links")
                .and_then(Value::as_array)
                .and_then(|links| links.first())
                .and_then(|link| link.get("rel"))
                .and_then(Value::as_str)
                == Some("next");
            if !next {
                break;
            }
            page += 1;
            if page > MAX_SEARCH_PAGES {
                return Err(resource_exhausted(false).into_adk());
            }
        }
        if !unlimited {
            rows.truncate(max);
        }
        Ok(rows)
    }

    /// `__find_qtest_id_by_test_id`.
    pub(super) async fn test_case_qtest_id(&self, test_id: &str) -> Result<Value, Failure> {
        let rows = self
            .search_test_cases(
                &format!("Id = {}", quoted(test_id)?),
                &SearchOptions::default(),
            )
            .await?;
        rows.first()
            .and_then(|row| row.get(QTEST_ID))
            .map(PyValue::to_json)
            .ok_or_else(|| {
                Failure::Message(format!(
                    "Test case '{test_id}' not found in project {}.",
                    self.project_id
                ))
            })
    }

    /// `__find_qtest_internal_id`.
    pub(super) async fn internal_id(
        &self,
        object_type: &str,
        entity_id: &str,
    ) -> Result<Value, Failure> {
        let response = self
            .search(
                object_type,
                &format!("Id = {}", quoted(entity_id)?),
                json!(["*"]),
                None,
            )
            .await?;
        if response.get("total").and_then(Value::as_u64) == Some(0) {
            return Err(Failure::Message(format!(
                "{} '{entity_id}' not found in project {}. Please verify the {entity_id} ID exists.",
                capitalize(object_type),
                self.project_id
            )));
        }
        response
            .get("items")
            .and_then(|items| items.get(0))
            .and_then(|item| item.get("id"))
            .cloned()
            .ok_or_else(|| Failure::Adk(invalid_response().into_adk()))
    }

    /// `__search_entity_by_id`: a parsed entity, or `None` when it is absent
    /// or the provider refuses the search (the SDK swallows `ApiException`).
    pub(super) async fn entity(
        &self,
        object_type: &str,
        entity_id: &str,
    ) -> Result<Option<PyValue>, Failure> {
        let query = format!("Id = {}", quoted(entity_id)?);
        let response = match self
            .search_raw(object_type, &query, json!(["*"]), None)
            .await
        {
            Ok(response) => response,
            Err(error) if skippable(error.code()) => return Ok(None),
            Err(error) => return Err(Failure::Adk(error.into_adk())),
        };
        if response.get("total").and_then(Value::as_u64) == Some(0) {
            return Ok(None);
        }
        Ok(response
            .get("items")
            .and_then(|items| items.get(0))
            .map(|item| parse_entity(object_type, item)))
    }

    /// `__get_entity_pid_by_internal_id`.
    pub(super) async fn pid_of(&self, object_type: &str, internal_id: &Value) -> Option<Value> {
        let query = format!("'id' = '{}'", str_of(internal_id).replace('\'', ""));
        let response = self
            .search(object_type, &query, json!(["id", "pid"]), None)
            .await
            .ok()?;
        if response.get("total").and_then(Value::as_u64).unwrap_or(0) == 0 {
            return None;
        }
        response
            .get("items")
            .and_then(|items| items.get(0))
            .and_then(|item| item.get("pid"))
            .cloned()
    }

    /// `ObjectLinkApi.find`: the `(pid, id)` of every object linked to one
    /// artifact, those whose pid has `prefix`.
    pub(super) async fn linked(
        &self,
        source_type: &str,
        internal_id: &Value,
        prefix: &str,
    ) -> Result<Vec<(String, Value)>, AdkError> {
        let response = self
            .call(
                QtestCall::get("linked-artifacts")
                    .query("type", source_type)
                    .query("ids", str_of(internal_id)),
            )
            .await?;
        let mut linked = Vec::new();
        for container in response.as_array().map_or(&[][..], Vec::as_slice) {
            for object in container
                .get("objects")
                .and_then(Value::as_array)
                .map_or(&[][..], Vec::as_slice)
            {
                if let Some(pid) = object
                    .get("pid")
                    .and_then(Value::as_str)
                    .filter(|pid| pid.starts_with(prefix))
                {
                    linked.push((
                        pid.to_owned(),
                        object.get("id").cloned().unwrap_or(Value::Null),
                    ));
                }
            }
        }
        Ok(linked)
    }
}

/// Whether a search failure is one the SDK skips rather than a bound.
pub(super) const fn skippable(code: QtestClientErrorCode) -> bool {
    !matches!(code, QtestClientErrorCode::ResourceExhausted)
}

/// `str.capitalize`.
fn capitalize(value: &str) -> String {
    let mut characters = value.chars();
    characters.next().map_or_else(String::new, |first| {
        first
            .to_uppercase()
            .chain(characters.flat_map(char::to_lowercase))
            .collect()
    })
}

/// `__format_property_value`: a bracketed `field_value_name` is a
/// multi-select list, a plain one a single-select label, an empty one a text
/// field's `field_value`.
fn property_value(property: &Value) -> PyValue {
    let name = property.get("field_value_name");
    let has_name = match name {
        None | Some(Value::Null) => false,
        Some(Value::String(text)) => !text.is_empty(),
        Some(_) => true,
    };
    if !has_name {
        return match property.get("field_value") {
            None | Some(Value::Null) => PyValue::text(""),
            Some(Value::String(text)) if text.is_empty() => PyValue::text(""),
            Some(value) => PyValue::from_json(value),
        };
    }
    match name {
        Some(Value::String(text))
            if text.starts_with('[') && text.ends_with(']') && text.len() >= 2 =>
        {
            let inner = text[1..text.len() - 1].trim();
            if inner.is_empty() {
                PyValue::List(Vec::new())
            } else {
                PyValue::List(
                    inner
                        .split(',')
                        .map(|part| PyValue::text(part.trim()))
                        .collect(),
                )
            }
        }
        Some(other) => PyValue::from_json(other),
        None => PyValue::text(""),
    }
}

fn cleaned(item: &Value, key: &str) -> String {
    clean_html(item.get(key).and_then(Value::as_str).unwrap_or_default())
}

/// `__parse_data` for one test case.
pub(super) fn parse_test_case(item: &Value) -> PyValue {
    let steps = item
        .get("test_steps")
        .and_then(Value::as_array)
        .map_or(&[][..], Vec::as_slice)
        .iter()
        .enumerate()
        .map(|(index, step)| {
            PyValue::Dict(vec![
                (
                    "Test Step Number".to_owned(),
                    PyValue::Json(json!(index + 1)),
                ),
                (
                    "Test Step Description".to_owned(),
                    PyValue::text(cleaned(step, "description")),
                ),
                (
                    "Test Step Expected Result".to_owned(),
                    PyValue::text(cleaned(step, "expected")),
                ),
            ])
        })
        .collect();
    let mut row = PyValue::Dict(vec![
        (
            "Id".to_owned(),
            PyValue::from_json(item.get("pid").unwrap_or(&Value::Null)),
        ),
        (
            "Name".to_owned(),
            PyValue::from_json(item.get("name").unwrap_or(&Value::Null)),
        ),
        (
            "Description".to_owned(),
            PyValue::text(cleaned(item, "description")),
        ),
        (
            "Precondition".to_owned(),
            PyValue::text(cleaned(item, "precondition")),
        ),
        (
            QTEST_ID.to_owned(),
            PyValue::from_json(item.get("id").unwrap_or(&Value::Null)),
        ),
        ("Steps".to_owned(), PyValue::List(steps)),
    ]);
    add_properties(&mut row, item, false);
    row
}

fn add_properties(row: &mut PyValue, item: &Value, strip_text: bool) {
    for property in item
        .get("properties")
        .and_then(Value::as_array)
        .map_or(&[][..], Vec::as_slice)
    {
        let Some(name) = property
            .get("field_name")
            .and_then(Value::as_str)
            .filter(|name| !name.is_empty())
        else {
            continue;
        };
        if row.get(name).is_some() {
            continue;
        }
        let mut value = property_value(property);
        if strip_text
            && let PyValue::Json(Value::String(text)) = &value
            && text.contains(['<', '&'])
        {
            value = PyValue::text(unescape(&strip_tags(text)));
        }
        row.set(name, value);
    }
}

fn truthy(value: Option<&Value>) -> bool {
    match value {
        None | Some(Value::Null) => false,
        Some(Value::Bool(flag)) => *flag,
        Some(Value::Number(number)) => number.as_f64().is_some_and(|number| number != 0.0),
        Some(Value::String(text)) => !text.is_empty(),
        Some(Value::Array(items)) => !items.is_empty(),
        Some(Value::Object(object)) => !object.is_empty(),
    }
}

/// `__parse_entity_item`.
pub(super) fn parse_entity(object_type: &str, item: &Value) -> PyValue {
    let field = |key: &str| PyValue::from_json(item.get(key).unwrap_or(&Value::Null));
    let mut row = PyValue::Dict(vec![
        ("Id".to_owned(), field("pid")),
        (QTEST_ID.to_owned(), field("id")),
    ]);
    if truthy(item.get("name")) {
        row.set("Name", field("name"));
    }
    if truthy(item.get("description")) {
        row.set("Description", PyValue::text(cleaned(item, "description")));
    }
    if truthy(item.get("web_url")) {
        row.set("Web URL", field("web_url"));
    }
    if object_type == "test-cases" {
        if truthy(item.get("precondition")) {
            row.set("Precondition", PyValue::text(cleaned(item, "precondition")));
        }
        if truthy(item.get("test_steps")) {
            let steps = item
                .get("test_steps")
                .and_then(Value::as_array)
                .map_or(&[][..], Vec::as_slice)
                .iter()
                .enumerate()
                .map(|(index, step)| {
                    PyValue::Dict(vec![
                        (
                            "Test Step Number".to_owned(),
                            PyValue::Json(json!(index + 1)),
                        ),
                        (
                            "Test Step Description".to_owned(),
                            PyValue::text(cleaned(step, "description")),
                        ),
                        (
                            "Test Step Expected Result".to_owned(),
                            PyValue::text(cleaned(step, "expected")),
                        ),
                    ])
                })
                .collect();
            row.set("Steps", PyValue::List(steps));
        }
    }
    if object_type == "test-runs" {
        if truthy(item.get("testCaseId")) {
            row.set("Test Case Id", field("testCaseId"));
        }
        if truthy(item.get("automation")) {
            row.set("Automation", field("automation"));
        }
        if let Some(log) = item.get("latest_test_log").filter(|log| truthy(Some(log))) {
            let member = |key: &str| PyValue::from_json(log.get(key).unwrap_or(&Value::Null));
            row.set(
                "Latest Test Log",
                PyValue::Dict(vec![
                    ("Log Id".to_owned(), member("id")),
                    ("Status".to_owned(), member("status")),
                    ("Execution Start".to_owned(), member("exe_start_date")),
                    ("Execution End".to_owned(), member("exe_end_date")),
                ]),
            );
        }
        if truthy(item.get("test_case_version")) {
            row.set("Test Case Version", field("test_case_version"));
        }
    }
    add_properties(&mut row, item, true);
    row
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::{capitalize, parse_entity, parse_test_case};

    #[test]
    fn parsers_keep_sdk_order_and_formats() {
        let row = parse_test_case(&json!({
            "pid":"TC-1","name":"Login","description":"<p>Open &amp; log in</p>","precondition":null,"id":501,
            "test_steps":[{"description":"<b>Go</b>","expected":"Done"}],
            "properties":[
                {"field_name":"Name","field_value":"x"},
                {"field_name":"Team","field_value":"[1,2]","field_value_name":"[A, B]"},
                {"field_name":"Priority","field_value":3,"field_value_name":"High"},
                {"field_name":"Notes","field_value":"free","field_value_name":""}
            ]
        }));
        assert_eq!(
            row.repr(),
            "{'Id': 'TC-1', 'Name': 'Login', 'Description': ' Open & log in ', 'Precondition': '', 'QTest Id': 501, 'Steps': [{'Test Step Number': 1, 'Test Step Description': ' Go ', 'Test Step Expected Result': 'Done'}], 'Team': ['A', 'B'], 'Priority': 'High', 'Notes': 'free'}"
        );
        let run = parse_entity(
            "test-runs",
            &json!({"pid":"TR-3","id":9,"name":"Run","testCaseId":501,
                "latest_test_log":{"id":77,"status":"Passed","exe_start_date":"a","exe_end_date":"b"},
                "properties":[{"field_name":"Note","field_value":"<i>x</i> &lt;y&gt;","field_value_name":null}]}),
        );
        assert_eq!(
            run.repr(),
            "{'Id': 'TR-3', 'QTest Id': 9, 'Name': 'Run', 'Test Case Id': 501, 'Latest Test Log': {'Log Id': 77, 'Status': 'Passed', 'Execution Start': 'a', 'Execution End': 'b'}, 'Note': ' x  <y>'}"
        );
        assert_eq!(capitalize("test-runs"), "Test-runs");
    }
}
