//! `ado_plans` operations (`tools/ado/test_plan/test_plan_wrapper.py`,
//! `azure-devops` `TestPlanClient` v7.0 plus the work item client the SDK
//! wrapper borrows for test case work items).

use std::collections::BTreeMap;

use reqwest::Method;
use serde_json::{Map, Value, json};
use tokio::sync::Mutex;

use crate::toolkits::families::ado::client::{
    AdoClient, AdoClientError, AdoRequest, AdoScope, bounded_output, invalid_response,
};
use crate::toolkits::families::ado::config::AdoToolkitConfig;
use crate::toolkits::families::ado::format::{as_dict, camel_case};
use crate::toolkits::families::ado::work_items::{
    FieldDefinition, create_work_item, format_work_item_type_fields, get_work_item, id_text,
    transform_work_item, work_item_result, work_item_type_fields,
};

use super::steps::{steps_from_json, steps_from_xml};

const TEST_PLAN_API: &str = "7.0";
/// `get_test_cases` reads every case's work item; this bounds that loop.
pub(crate) const MAX_TEST_CASES: usize = 200;
/// `create_test_cases` creates one work item per entry.
pub(crate) const MAX_CREATE_TEST_CASES: usize = 50;
const TEST_CASE_TYPE: &str = "Test Case";
const PROTECTED_FIELDS: &[&str] = &[
    "System.Title",
    "System.Description",
    "Microsoft.VSTS.TCM.Steps",
];

/// One msrest model the SDK builds from caller JSON with `Model(**params)`:
/// its Python attribute names, and which of them are `int`.
struct ModelShape {
    attributes: &'static [&'static str],
    integers: &'static [&'static str],
}

const TEST_PLAN_CREATE_PARAMS: ModelShape = ModelShape {
    attributes: &[
        "area_path",
        "automated_test_environment",
        "automated_test_settings",
        "build_definition",
        "build_id",
        "description",
        "end_date",
        "iteration",
        "manual_test_environment",
        "manual_test_settings",
        "name",
        "owner",
        "release_environment_definition",
        "start_date",
        "state",
        "test_outcome_settings",
    ],
    integers: &["build_id"],
};

const TEST_SUITE_CREATE_PARAMS: ModelShape = ModelShape {
    attributes: &[
        "default_configurations",
        "default_testers",
        "inherit_default_configurations",
        "name",
        "parent_suite",
        "query_string",
        "requirement_id",
        "suite_type",
    ],
    integers: &["requirement_id"],
};

const SUITE_TEST_CASE_PARAMS: ModelShape = ModelShape {
    attributes: &["point_assignments", "work_item"],
    integers: &[],
};

/// One test case to create, as `create_test_case` takes it.
pub(crate) struct NewTestCase {
    pub(crate) plan_id: u64,
    pub(crate) suite_id: u64,
    pub(crate) title: String,
    pub(crate) description: String,
    pub(crate) test_steps: String,
    pub(crate) test_steps_format: String,
    pub(crate) additional_fields: Option<String>,
}

pub(crate) struct AdoPlansClient {
    ado: AdoClient,
    type_fields: Mutex<BTreeMap<String, Vec<FieldDefinition>>>,
}

impl AdoPlansClient {
    pub(crate) fn new(config: AdoToolkitConfig) -> Result<Self, AdoClientError> {
        let (connection, _) = config.into_parts();
        Ok(Self::with_client(AdoClient::new(connection)?))
    }

    pub(crate) fn with_client(ado: AdoClient) -> Self {
        Self {
            ado,
            type_fields: Mutex::new(BTreeMap::new()),
        }
    }

    pub(crate) async fn create_test_plan(&self, params: &str) -> Result<Value, AdoClientError> {
        let body = match model_body(params, &TEST_PLAN_CREATE_PARAMS, "TestPlanCreateParams") {
            Ok(body) => body,
            Err(message) => {
                return Ok(Value::String(format!(
                    "Error creating test plan: {message}"
                )));
            }
        };
        let segments = ["testplan", "plans"];
        let created = self
            .ado
            .json(
                AdoRequest::new(Method::POST, AdoScope::Project, &segments, TEST_PLAN_API)
                    .json(body),
                true,
            )
            .await;
        match created {
            Ok(plan) => Ok(Value::String(format!(
                "Test plan {} created successfully.",
                id_text(plan.get("id").ok_or_else(invalid_response)?)
            ))),
            Err(error) => visible(error, "Error creating test plan"),
        }
    }

    pub(crate) async fn delete_test_plan(&self, plan_id: u64) -> Result<Value, AdoClientError> {
        let plan = plan_id.to_string();
        let segments = ["testplan", "plans", plan.as_str()];
        self.ado
            .send(
                AdoRequest::new(Method::DELETE, AdoScope::Project, &segments, TEST_PLAN_API),
                true,
            )
            .await?;
        Ok(Value::String(format!(
            "Test plan {plan_id} deleted successfully."
        )))
    }

    pub(crate) async fn get_test_plan(
        &self,
        plan_id: Option<u64>,
    ) -> Result<Value, AdoClientError> {
        if let Some(plan_id) = plan_id {
            let plan = plan_id.to_string();
            let segments = ["testplan", "plans", plan.as_str()];
            let value = self
                .ado
                .json(AdoRequest::get(&segments, TEST_PLAN_API), false)
                .await?;
            return bounded_output(as_dict(&value));
        }
        let segments = ["testplan", "plans"];
        let plans = self
            .ado
            .collection(AdoRequest::get(&segments, TEST_PLAN_API))
            .await?;
        bounded_output(Value::Array(plans.iter().map(as_dict).collect()))
    }

    pub(crate) async fn create_test_suite(
        &self,
        params: &str,
        plan_id: u64,
    ) -> Result<Value, AdoClientError> {
        let body = match model_body(params, &TEST_SUITE_CREATE_PARAMS, "TestSuiteCreateParams") {
            Ok(body) => body,
            Err(message) => {
                return Ok(Value::String(format!(
                    "Error creating test suite: {message}"
                )));
            }
        };
        let plan = plan_id.to_string();
        let segments = ["testplan", "Plans", plan.as_str(), "suites"];
        match self
            .ado
            .json(
                AdoRequest::new(Method::POST, AdoScope::Project, &segments, TEST_PLAN_API)
                    .json(body),
                true,
            )
            .await
        {
            Ok(suite) => Ok(Value::String(format!(
                "Test suite {} created successfully.",
                id_text(suite.get("id").ok_or_else(invalid_response)?)
            ))),
            Err(error) => visible(error, "Error creating test suite"),
        }
    }

    pub(crate) async fn delete_test_suite(
        &self,
        plan_id: u64,
        suite_id: u64,
    ) -> Result<Value, AdoClientError> {
        let plan = plan_id.to_string();
        let suite = suite_id.to_string();
        let segments = ["testplan", "Plans", plan.as_str(), "Suites", suite.as_str()];
        self.ado
            .send(
                AdoRequest::new(Method::DELETE, AdoScope::Project, &segments, TEST_PLAN_API),
                true,
            )
            .await?;
        Ok(Value::String(format!(
            "Test suite {suite_id} deleted successfully."
        )))
    }

    pub(crate) async fn get_test_suite(
        &self,
        plan_id: u64,
        suite_id: Option<u64>,
    ) -> Result<Value, AdoClientError> {
        let plan = plan_id.to_string();
        if let Some(suite_id) = suite_id {
            let suite = suite_id.to_string();
            let segments = ["testplan", "Plans", plan.as_str(), "Suites", suite.as_str()];
            let value = self
                .ado
                .json(AdoRequest::get(&segments, TEST_PLAN_API), false)
                .await?;
            return bounded_output(as_dict(&value));
        }
        let segments = ["testplan", "Plans", plan.as_str(), "suites"];
        let suites = self
            .ado
            .collection(AdoRequest::get(&segments, TEST_PLAN_API))
            .await?;
        bounded_output(Value::Array(suites.iter().map(as_dict).collect()))
    }

    /// `add_test_case`: a JSON array of `SuiteTestCaseCreateUpdateParameters`.
    pub(crate) async fn add_test_case(
        &self,
        parameters: &str,
        plan_id: u64,
        suite_id: u64,
    ) -> Result<Value, AdoClientError> {
        let body = match suite_test_case_body(parameters) {
            Ok(body) => body,
            Err(message) => {
                return Ok(Value::String(format!("Error adding test case: {message}")));
            }
        };
        match self.add_test_cases(body, plan_id, suite_id).await {
            Ok(cases) => bounded_output(cases),
            Err(error) => visible(error, "Error adding test case"),
        }
    }

    async fn add_test_cases(
        &self,
        body: Value,
        plan_id: u64,
        suite_id: u64,
    ) -> Result<Value, AdoClientError> {
        let plan = plan_id.to_string();
        let suite = suite_id.to_string();
        let segments = [
            "testplan",
            "Plans",
            plan.as_str(),
            "Suites",
            suite.as_str(),
            "TestCase",
        ];
        let added = self
            .ado
            .json(
                AdoRequest::new(Method::POST, AdoScope::Project, &segments, TEST_PLAN_API)
                    .json(body),
                true,
            )
            .await?;
        let cases = match added {
            Value::Object(mut object) => object.remove("value").unwrap_or(Value::Null),
            other => other,
        };
        let cases = cases.as_array().ok_or_else(invalid_response)?;
        Ok(Value::Array(cases.iter().map(as_dict).collect()))
    }

    /// `create_test_case`: build the steps XML, create the `Test Case` work
    /// item, then add it to the suite.
    pub(crate) async fn create_test_case(
        &self,
        test_case: &NewTestCase,
    ) -> Result<Value, AdoClientError> {
        let steps = match test_case.test_steps_format.as_str() {
            "json" => steps_from_json(&test_case.test_steps),
            "xml" => steps_from_xml(&test_case.test_steps),
            other => {
                return Ok(Value::String(format!("Unknown test steps format: {other}")));
            }
        };
        let steps = match steps {
            Ok(steps) => steps,
            Err(message) => return Ok(Value::String(message)),
        };
        let mut fields = Map::new();
        fields.insert(
            "System.Title".to_owned(),
            Value::String(test_case.title.clone()),
        );
        fields.insert(
            "System.Description".to_owned(),
            Value::String(test_case.description.clone()),
        );
        fields.insert("Microsoft.VSTS.TCM.Steps".to_owned(), Value::String(steps));
        if let Some(additional) = test_case
            .additional_fields
            .as_deref()
            .filter(|value| !value.is_empty())
        {
            let parsed: Value = match serde_json::from_str(additional) {
                Ok(parsed) => parsed,
                Err(error) => {
                    return Ok(Value::String(format!(
                        "Invalid JSON format for additional_fields: {error}"
                    )));
                }
            };
            let Some(additional) = parsed.as_object() else {
                return Ok(Value::String(
                    "Invalid JSON format for additional_fields: expected a JSON object".to_owned(),
                ));
            };
            for (name, value) in additional {
                // The SDK logs and ignores an attempt to override these.
                if !PROTECTED_FIELDS.contains(&name.as_str()) {
                    fields.insert(name.clone(), value.clone());
                }
            }
        }
        let document = transform_work_item(&json!({"fields":fields}).to_string())
            .map_err(|_| invalid_response())?;
        let created = match create_work_item(&self.ado, document, TEST_CASE_TYPE).await {
            Ok(created) => created,
            Err(error) => {
                let Some(message) = error.model_visible_message() else {
                    return Err(error);
                };
                let message = format!("Error creating work item: {message}");
                if message.contains("TF401320") || message.to_lowercase().contains("validation") {
                    return Ok(Value::String(format!(
                        "{message}\n\n💡 To discover all required fields for Test Case work items in your project:\n   • Use the get_all_test_case_fields_for_project() tool\n   • Provide missing required fields via the additional_fields parameter\n   • Example: additional_fields='{{\"Custom.SDLC\": \"Development\"}}'"
                    )));
                }
                return Ok(Value::String(message));
            }
        };
        let id = created.get("id").cloned().ok_or_else(invalid_response)?;
        match self
            .add_test_cases(
                json!([{"workItem":{"id":id}}]),
                test_case.plan_id,
                test_case.suite_id,
            )
            .await
        {
            Ok(cases) => bounded_output(cases),
            Err(error) => visible(error, "Error adding test case"),
        }
    }

    pub(crate) async fn create_test_cases(
        &self,
        test_cases: &[NewTestCase],
    ) -> Result<Value, AdoClientError> {
        let mut results = Vec::with_capacity(test_cases.len());
        for test_case in test_cases {
            results.push(self.create_test_case(test_case).await?);
        }
        bounded_output(Value::Array(results))
    }

    /// `get_test_case`: the suite's reference plus the full work item.
    pub(crate) async fn get_test_case(
        &self,
        plan_id: u64,
        suite_id: u64,
        test_case_id: &str,
        fields: Option<&[String]>,
    ) -> Result<Value, AdoClientError> {
        let plan = plan_id.to_string();
        let suite = suite_id.to_string();
        let segments = [
            "testplan",
            "Plans",
            plan.as_str(),
            "Suites",
            suite.as_str(),
            "TestCase",
            test_case_id,
        ];
        let cases = self
            .ado
            .collection(AdoRequest::get(&segments, TEST_PLAN_API))
            .await?;
        let Some(first) = cases.first() else {
            return Ok(Value::String(format!(
                "No test cases found per given criteria: project {}, plan {plan_id}, suite {suite_id}, test case id {test_case_id}",
                self.ado.project()
            )));
        };
        bounded_output(self.with_work_item(as_dict(first), fields).await)
    }

    pub(crate) async fn get_test_cases(
        &self,
        plan_id: u64,
        suite_id: u64,
        fields: Option<&[String]>,
    ) -> Result<Value, AdoClientError> {
        let plan = plan_id.to_string();
        let suite = suite_id.to_string();
        let segments = [
            "testplan",
            "Plans",
            plan.as_str(),
            "Suites",
            suite.as_str(),
            "TestCase",
        ];
        let cases = self
            .ado
            .collection(AdoRequest::get(&segments, TEST_PLAN_API))
            .await?;
        if cases.len() > MAX_TEST_CASES {
            return Ok(Value::String(format!(
                "Suite {suite_id} holds {} test cases, more than the {MAX_TEST_CASES} one call reads with their work items. Read them by id with get_test_case.",
                cases.len()
            )));
        }
        let mut result = Vec::with_capacity(cases.len());
        for case in &cases {
            result.push(self.with_work_item(as_dict(case), fields).await);
        }
        bounded_output(Value::Array(result))
    }

    /// Add `work_item_full_details`; like the SDK, a failed work item read
    /// leaves the test case without it.
    async fn with_work_item(&self, mut case: Value, fields: Option<&[String]>) -> Value {
        let id = case
            .get("work_item")
            .and_then(|item| item.get("id"))
            .and_then(|id| match id {
                Value::Number(number) => number.as_u64(),
                Value::String(text) => text.parse().ok(),
                _ => None,
            });
        if let Some(id) = id.filter(|id| *id > 0) {
            let fields = fields.filter(|fields| !fields.is_empty());
            let expand = if fields.is_some() {
                None
            } else {
                Some("Relations")
            };
            if let Ok(raw) = get_work_item(&self.ado, id, fields, None, expand).await
                && let Ok(details) =
                    work_item_result(self.ado.organization_text(), &raw, fields, expand)
                && let Some(object) = case.as_object_mut()
            {
                object.insert("work_item_full_details".to_owned(), details);
            }
        }
        case
    }

    /// `get_all_test_case_fields_for_project`: the work item wrapper's
    /// `get_work_item_type_fields("Test Case")` with its own cache.
    pub(crate) async fn get_all_test_case_fields_for_project(
        &self,
        force_refresh: bool,
    ) -> Result<Value, AdoClientError> {
        let mut cache = self.type_fields.lock().await;
        if force_refresh || !cache.contains_key(TEST_CASE_TYPE) {
            match work_item_type_fields(&self.ado, TEST_CASE_TYPE).await {
                Ok(definitions) => {
                    cache.insert(TEST_CASE_TYPE.to_owned(), definitions);
                }
                Err(_) => {
                    return Ok(Value::String(format_work_item_type_fields(
                        self.ado.project(),
                        TEST_CASE_TYPE,
                        &[],
                    )));
                }
            }
        }
        let definitions = cache.get(TEST_CASE_TYPE).cloned().unwrap_or_default();
        drop(cache);
        bounded_output(Value::String(format_work_item_type_fields(
            self.ado.project(),
            TEST_CASE_TYPE,
            &definitions,
        )))
    }
}

/// The SDK returns a provider 400/422 message as `<prefix>: <message>`.
fn visible(error: AdoClientError, prefix: &str) -> Result<Value, AdoClientError> {
    match error.model_visible_message() {
        Some(message) => Ok(Value::String(format!("{prefix}: {message}"))),
        None => Err(error),
    }
}

/// `Model(**json.loads(params))` serialized to REST JSON: known Python
/// attribute names only (an unknown one is the constructor's `TypeError`),
/// renamed to camelCase, with nested dicts renamed the same way.
fn model_body(params: &str, shape: &ModelShape, model: &str) -> Result<Value, String> {
    let parsed: Value = serde_json::from_str(params).map_err(|error| error.to_string())?;
    let object = parsed
        .as_object()
        .ok_or_else(|| format!("{model}() argument after ** must be a mapping"))?;
    model_object(object, shape, model).map(Value::Object)
}

fn model_object(
    object: &Map<String, Value>,
    shape: &ModelShape,
    model: &str,
) -> Result<Map<String, Value>, String> {
    let mut body = Map::new();
    for (name, value) in object {
        if !shape.attributes.contains(&name.as_str()) {
            return Err(format!(
                "{model}.__init__() got an unexpected keyword argument '{name}'"
            ));
        }
        if value.is_null() {
            continue;
        }
        let value = if shape.integers.contains(&name.as_str()) {
            integer(value).ok_or_else(|| format!("invalid literal for int(): {value}"))?
        } else {
            camel_keys(value)
        };
        body.insert(camel_case(name), value);
    }
    Ok(body)
}

/// `SuiteTestCaseCreateUpdateParameters(**param)` for each array entry;
/// `work_item.id` is an `int` the serializer coerces from a string.
fn suite_test_case_body(parameters: &str) -> Result<Value, String> {
    let parsed: Value = serde_json::from_str(parameters).map_err(|error| error.to_string())?;
    let entries = parsed.as_array().ok_or_else(|| {
        "suite_test_case_create_update_parameters must be a JSON array".to_owned()
    })?;
    let mut body = Vec::with_capacity(entries.len());
    for entry in entries {
        let object = entry.as_object().ok_or_else(|| {
            "SuiteTestCaseCreateUpdateParameters() argument after ** must be a mapping".to_owned()
        })?;
        let mut converted = model_object(
            object,
            &SUITE_TEST_CASE_PARAMS,
            "SuiteTestCaseCreateUpdateParameters",
        )?;
        if let Some(Value::Object(work_item)) = converted.get_mut("workItem")
            && let Some(id) = work_item.get("id").cloned()
        {
            let id = integer(&id).ok_or_else(|| format!("invalid literal for int(): {id}"))?;
            work_item.insert("id".to_owned(), id);
        }
        body.push(Value::Object(converted));
    }
    Ok(Value::Array(body))
}

fn integer(value: &Value) -> Option<Value> {
    match value {
        Value::Number(number) if number.is_i64() || number.is_u64() => Some(value.clone()),
        Value::String(text) => text.trim().parse::<i64>().ok().map(Value::from),
        _ => None,
    }
}

fn camel_keys(value: &Value) -> Value {
    match value {
        Value::Object(object) => Value::Object(
            object
                .iter()
                .filter(|(_, value)| !value.is_null())
                .map(|(key, value)| (camel_case(key), camel_keys(value)))
                .collect(),
        ),
        Value::Array(values) => Value::Array(values.iter().map(camel_keys).collect()),
        other => other.clone(),
    }
}
