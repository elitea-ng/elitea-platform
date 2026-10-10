//! The effects: create/update/delete test cases, requirement links and
//! manual test-run results.

use std::fmt::Write as _;

use chrono::{SecondsFormat, TimeDelta, Timelike as _, Utc};
use reqwest::{Method, StatusCode};
use serde_json::{Map, Value, json};

use crate::toolkits::families::python_repr::{PyValue, str_of};

use super::client::{QtestCall, invalid_response, status_error};
use super::fields::{FieldDefinitions, map_properties};
use super::schema::QtestToolKind;
use super::search::{QTEST_ID, SearchOptions, quoted};
use super::tools::{
    Failure, Qtest, invalid_arguments, optional_integer, optional_text, required_text,
};

const MAX_TEST_CASES: usize = 100;
const MAX_LINKED_TEST_CASES: usize = 200;

fn truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(flag) => *flag,
        Value::Number(number) => number.as_f64().is_some_and(|number| number != 0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(items) => !items.is_empty(),
        Value::Object(object) => !object.is_empty(),
    }
}

/// A JSON object as ordered pairs (the SDK iterates the parsed dict).
fn pairs(object: &Map<String, Value>) -> Vec<(String, Value)> {
    object
        .iter()
        .map(|(key, value)| (key.clone(), value.clone()))
        .collect()
}

/// `__build_body_for_create_test_case` for one test case: the mapped
/// properties plus the core fields the input names.
fn build_body(
    test_case: &[(String, Value)],
    definitions: &FieldDefinitions,
    parent_id: Option<&Value>,
) -> Result<Value, Failure> {
    let properties = map_properties(test_case, definitions).map_err(Failure::Message)?;
    let get = |key: &str| {
        test_case
            .iter()
            .find(|(name, _)| name == key)
            .map(|(_, value)| value)
    };
    let mut body = Map::new();
    body.insert("properties".to_owned(), Value::Array(properties));
    if let Some(name) = get("Name") {
        body.insert(
            "name".to_owned(),
            if truthy(name) {
                name.clone()
            } else {
                json!("Untitled")
            },
        );
    }
    for (key, field) in [
        ("Precondition", "precondition"),
        ("Description", "description"),
    ] {
        if let Some(value) = get(key) {
            body.insert(
                field.to_owned(),
                if value.is_null() {
                    json!("")
                } else {
                    value.clone()
                },
            );
        }
    }
    if let Some(parent) = parent_id {
        body.insert("parent_id".to_owned(), parent.clone());
    }
    if let Some(steps) = get("Steps").filter(|steps| !steps.is_null()) {
        let steps = steps
            .as_array()
            .ok_or_else(|| Failure::Adk(invalid_arguments()))?
            .iter()
            .map(|step| {
                let step = step
                    .as_object()
                    .ok_or_else(|| Failure::Adk(invalid_arguments()))?;
                let mut resource = Map::new();
                for (key, field) in [
                    ("Test Step Description", "description"),
                    ("Test Step Expected Result", "expected"),
                ] {
                    if let Some(value) = step.get(key).filter(|value| !value.is_null()) {
                        resource.insert(field.to_owned(), value.clone());
                    }
                }
                Ok(Value::Object(resource))
            })
            .collect::<Result<Vec<_>, Failure>>()?;
        body.insert("test_steps".to_owned(), Value::Array(steps));
    }
    Ok(Value::Object(body))
}

impl Qtest {
    pub(super) async fn effect(
        &self,
        kind: QtestToolKind,
        arguments: &Map<String, Value>,
    ) -> Result<Value, Failure> {
        match kind {
            QtestToolKind::CreateTestCases => self.create_test_cases(arguments).await,
            QtestToolKind::UpdateTestCase => self.update_test_case(arguments).await,
            QtestToolKind::UpdateTestRunStatus => self.update_test_run_status(arguments).await,
            QtestToolKind::DeleteTestCase => {
                let qtest_id = optional_integer(arguments, "qtest_id")?
                    .filter(|id| *id > 0)
                    .ok_or_else(|| Failure::Adk(invalid_arguments()))?;
                let path = format!("test-cases/{qtest_id}");
                self.call(QtestCall {
                    method: Method::DELETE,
                    path: &path,
                    query: Vec::new(),
                    body: None,
                    effect: true,
                })
                .await?;
                Ok(Value::String(format!(
                    "Successfully deleted test case in project with id - {} and qtest id - {qtest_id}.",
                    self.project_id
                )))
            }
            _ => self.link_tests(kind, arguments).await,
        }
    }

    /// The parent module id of a full folder name: the SDK joins the ids of
    /// every module whose full name matches.
    async fn parent_id(&self, folder: &str) -> Result<Option<Value>, Failure> {
        let modules = self.modules().await?;
        if folder.is_empty() {
            return Ok(None);
        }
        let matches = modules
            .iter()
            .filter(|(_, full_name)| full_name == folder)
            .map(|(id, _)| id)
            .collect::<Vec<_>>();
        Ok(match matches.as_slice() {
            [] => None,
            [single] => Some((*single).clone()),
            many => Some(Value::String(many.iter().map(|id| str_of(id)).collect())),
        })
    }

    async fn create_test_cases(&self, arguments: &Map<String, Value>) -> Result<Value, Failure> {
        let content = required_text(arguments, "test_case_content")?;
        let folder = optional_text(arguments, "folder_to_place_test_cases_to")?.unwrap_or("");
        let parsed = serde_json::from_str::<Value>(content)
            .map_err(|_| Failure::Adk(invalid_arguments()))?;
        let test_cases = match parsed {
            Value::Array(items) if items.len() <= MAX_TEST_CASES => items,
            Value::Array(_) => return Err(Failure::Adk(invalid_arguments())),
            other => vec![other],
        };
        let definitions = self.field_definitions().await?;
        let parent = self.parent_id(folder).await?;
        let mut bodies = Vec::with_capacity(test_cases.len());
        for test_case in &test_cases {
            let test_case = test_case
                .as_object()
                .ok_or_else(|| Failure::Adk(invalid_arguments()))?;
            bodies.push(build_body(
                &pairs(test_case),
                &definitions,
                parent.as_ref(),
            )?);
        }
        let mut created = Vec::with_capacity(bodies.len());
        for body in bodies {
            let response = self
                .call(QtestCall::send(Method::POST, "test-cases", body, true))
                .await?;
            let member = |key: &str| response.get(key).cloned().unwrap_or(Value::Null);
            created.push(json!({
                "test_case_id": member("pid"),
                "qtest_id": member("id"),
                "test_case_name": member("name"),
                "url": member("web_url"),
            }));
        }
        Ok(json!({"qtest_folder": folder, "test_cases": created}))
    }

    async fn update_test_case(&self, arguments: &Map<String, Value>) -> Result<Value, Failure> {
        let test_id = required_text(arguments, "test_id")?;
        let content = required_text(arguments, "test_case_content")?;
        let parsed = serde_json::from_str::<Value>(content)
            .map_err(|_| Failure::Adk(invalid_arguments()))?;
        let test_case = match &parsed {
            Value::Array(items) => items.first(),
            other => Some(other),
        }
        .and_then(Value::as_object)
        .ok_or_else(|| Failure::Adk(invalid_arguments()))?;
        let mut merged = PyValue::from_json(&Value::Object(test_case.clone()));
        let provided = test_case
            .get(QTEST_ID)
            .filter(|id| !id.is_null() && id.as_str() != Some(""));
        let qtest_id = if let Some(id) = provided {
            id.clone()
        } else {
            let dql = format!("Id = {}", quoted(test_id)?);
            let rows = self
                .search_test_cases(&dql, &SearchOptions::default())
                .await?;
            let Some(mut actual) = rows.into_iter().next() else {
                return Err(Failure::Message(format!(
                    "Test case '{test_id}' not found in project {}.",
                    self.project_id
                )));
            };
            // `actual_test_case | test_case`.
            if let PyValue::Dict(members) = merged {
                for (key, value) in members {
                    actual.set(&key, value);
                }
            }
            merged = actual;
            merged.get(QTEST_ID).map_or(Value::Null, PyValue::to_json)
        };
        if !matches!(&qtest_id, Value::Number(_) | Value::String(_)) {
            return Err(Failure::Adk(invalid_arguments()));
        }
        let qtest_path = str_of(&qtest_id);
        if qtest_path.is_empty() || !qtest_path.bytes().all(|byte| byte.is_ascii_digit()) {
            return Err(Failure::Adk(invalid_arguments()));
        }
        let definitions = self.field_definitions().await?;
        // The SDK reads the module tree for every body it builds.
        self.parent_id("").await?;
        let merged_json = merged.to_json();
        let fields = merged_json.as_object().map(pairs).unwrap_or_default();
        let body = build_body(&fields, &definitions, None)?;
        let path = format!("test-cases/{qtest_path}");
        let response = self
            .call(QtestCall::send(Method::PUT, &path, body, true))
            .await?;
        Ok(Value::String(format!(
            "Successfully updated test case in project with id - {}.\n            Updated test case id - {}.\n            Test id of updated test case - {test_id}.\n            Updated with content:\n{}",
            self.project_id,
            response
                .get("pid")
                .map_or_else(|| "None".to_owned(), str_of),
            merged.repr()
        )))
    }

    async fn link_tests(
        &self,
        kind: QtestToolKind,
        arguments: &Map<String, Value>,
    ) -> Result<Value, Failure> {
        let list = required_text(arguments, "json_list_of_test_case_ids")?;
        let test_cases = serde_json::from_str::<Vec<String>>(list)
            .ok()
            .filter(|ids| ids.len() <= MAX_LINKED_TEST_CASES)
            .ok_or_else(|| Failure::Adk(invalid_arguments()))?;
        let mut internal_ids = Vec::with_capacity(test_cases.len());
        for test_case in &test_cases {
            internal_ids.push(self.test_case_qtest_id(test_case).await?);
        }
        let (requirement, label, internal) = if kind == QtestToolKind::LinkTestsToJiraRequirement {
            let external = required_text(arguments, "requirement_external_id")?;
            let query = format!("'External Id' = {}", quoted(external)?);
            let response = self
                .search("requirements", &query, json!(["*"]), None)
                .await?;
            if response.get("total").and_then(Value::as_u64) == Some(0) {
                return Err(Failure::Message(format!(
                    "Jira requirement '{external}' not found in QTest project {}. Please ensure the Jira issue is linked to QTest as a requirement.",
                    self.project_id
                )));
            }
            let internal = response
                .get("items")
                .and_then(|items| items.get(0))
                .and_then(|item| item.get("id"))
                .cloned()
                .ok_or_else(|| Failure::Adk(invalid_response().into_adk()))?;
            (external, "Jira requirement", internal)
        } else {
            let requirement = required_text(arguments, "requirement_id")?;
            let internal = self.internal_id("requirements", requirement).await?;
            (requirement, "QTest requirement", internal)
        };
        let path = format!("requirements/{}/link", str_of(&internal));
        let response = self
            .call(
                QtestCall::send(Method::POST, &path, Value::Array(internal_ids), true)
                    .query("type", "test-cases"),
            )
            .await?;
        let linked = response
            .get(0)
            .and_then(|container| container.get("objects"))
            .and_then(Value::as_array)
            .map_or(&[][..], Vec::as_slice)
            .iter()
            .map(|object| object.get("pid").map_or_else(|| "None".to_owned(), str_of))
            .collect::<Vec<_>>();
        Ok(Value::String(format!(
            "Successfully linked {} test case(s) to {label} '{requirement}' in project {}.\nLinked test cases: {}",
            linked.len(),
            self.project_id,
            linked.join(", ")
        )))
    }

    /// `__resolve_test_run_status`: the project's execution-status id for a
    /// name, exact first, then case-insensitively.
    async fn execution_status(&self, status: &str) -> Result<Value, Failure> {
        let statuses = self
            .call(QtestCall::get("test-runs/execution-statuses"))
            .await?;
        let named = statuses
            .as_array()
            .map_or(&[][..], Vec::as_slice)
            .iter()
            .filter_map(|item| {
                let name = item
                    .get("name")
                    .and_then(Value::as_str)
                    .filter(|name| !name.is_empty())?;
                let id = item.get("id").filter(|id| !id.is_null())?;
                Some((name, id))
            })
            .collect::<Vec<_>>();
        let selected = named
            .iter()
            .rev()
            .find(|(name, _)| *name == status)
            .or_else(|| {
                let wanted = status.to_lowercase();
                named.iter().find(|(name, _)| name.to_lowercase() == wanted)
            });
        if let Some((_, id)) = selected {
            return Ok((*id).clone());
        }
        let mut allowed = named.iter().map(|(name, _)| *name).collect::<Vec<_>>();
        allowed.sort_unstable();
        allowed.dedup();
        Err(Failure::Message(format!(
            "Status '{status}' is not a valid execution status in project {}. Allowed values: {}.",
            self.project_id,
            allowed.join(", ")
        )))
    }

    async fn update_test_run_status(
        &self,
        arguments: &Map<String, Value>,
    ) -> Result<Value, Failure> {
        let test_run = required_text(arguments, "test_run_id")?;
        let status = required_text(arguments, "status")?;
        let note = optional_text(arguments, "note")?;
        let version = optional_integer(arguments, "testcase_version_id")?;
        let status_id = self.execution_status(status).await?;
        let Some(run) = self.entity("test-runs", test_run).await? else {
            return Err(Failure::Message(format!(
                "Test run {test_run} not found in project {}.",
                self.project_id
            )));
        };
        let Some(run_id) = run.get(QTEST_ID).map(PyValue::to_json).filter(truthy) else {
            return Err(Failure::Message(format!(
                "Test run {test_run} has no numeric QTest ID in project {}; a new execution log cannot be submitted.",
                self.project_id
            )));
        };
        if let Some(version) = version
            && let Some(test_case) = run.get("Test Case Id").map(PyValue::to_json).filter(truthy)
        {
            self.validate_version(&test_case, version).await?;
        }
        let end = Utc::now().with_nanosecond(0).unwrap_or_else(Utc::now);
        let start = end - TimeDelta::minutes(1);
        let mut body = Map::new();
        body.insert("status".to_owned(), json!({"id": status_id}));
        body.insert(
            "exe_start_date".to_owned(),
            json!(start.to_rfc3339_opts(SecondsFormat::Secs, false)),
        );
        body.insert(
            "exe_end_date".to_owned(),
            json!(end.to_rfc3339_opts(SecondsFormat::Secs, false)),
        );
        if let Some(note) = note {
            body.insert("note".to_owned(), json!(note));
        }
        if let Some(version) = version {
            body.insert("test_case_version_id".to_owned(), json!(version));
        }
        let path = format!("test-runs/{}/test-logs", str_of(&run_id));
        let response = self
            .call(QtestCall::send(
                Method::POST,
                &path,
                Value::Object(body),
                true,
            ))
            .await?;
        let mut message = format!(
            "Successfully recorded test run {test_run} status as '{status}' in project {} by creating a new manual execution log.",
            self.project_id
        );
        if let Some(version) = version {
            let _ = write!(message, " Test case version id: {version}.");
        }
        if let Some(log) = response.get("id").filter(|id| !id.is_null()) {
            let _ = write!(message, " Test log id: {}.", str_of(log));
        }
        Ok(Value::String(message))
    }

    /// `__validate_test_case_version_id`: a 400/404 from the per-version
    /// endpoint means the version is not this test case's.
    async fn validate_version(&self, test_case: &Value, version: i64) -> Result<(), Failure> {
        let path = format!("test-cases/{}/versions/{version}", str_of(test_case));
        let response = self
            .client
            .exchange(QtestCall::get(&path))
            .await
            .map_err(|error| Failure::Adk(error.into_adk()))?;
        if matches!(
            response.status,
            StatusCode::BAD_REQUEST | StatusCode::NOT_FOUND
        ) {
            let known = match self.versions(test_case).await {
                Ok(versions) if !versions.is_empty() => format!(
                    " Known versions of test case {}: {}.",
                    str_of(test_case),
                    versions
                        .iter()
                        .map(|version| format!(
                            "{} (id={})",
                            version
                                .get("version")
                                .map_or_else(|| "None".to_owned(), str_of),
                            version
                                .get("version_id")
                                .map_or_else(|| "None".to_owned(), str_of)
                        ))
                        .collect::<Vec<_>>()
                        .join(", ")
                ),
                _ => String::new(),
            };
            return Err(Failure::Message(format!(
                "Test case version ID {version} does not exist for test case {} in project {}.{known} Use the get_test_case_versions tool to resolve a version name to its ID.",
                str_of(test_case),
                self.project_id
            )));
        }
        if !response.status.is_success() {
            return Err(Failure::Adk(
                status_error(response.status, false).into_adk(),
            ));
        }
        Ok(())
    }
}
