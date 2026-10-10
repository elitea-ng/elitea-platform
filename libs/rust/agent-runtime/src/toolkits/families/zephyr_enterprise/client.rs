use reqwest::Method;
use serde_json::{Value, json};

use crate::toolkits::families::zephyr_rest::client::{
    ZephyrReply, ZephyrRestClient, ZephyrRestError, ZephyrRestErrorCode, ZephyrRestFamily,
    bounded_text, bounded_value, invalid_response, python_str, render,
};

/// `flex/services/rest/latest`, the SDK's `ZephyrEndpoints` prefix.
const API: [&str; 4] = ["flex", "services", "rest", "latest"];
pub(in crate::toolkits) const MAX_STEPS_PER_CALL: usize = 100;

pub(in crate::toolkits) static FAMILY: ZephyrRestFamily = ZephyrRestFamily {
    label: "Zephyr Enterprise",
    code: error_code,
};

const fn error_code(code: ZephyrRestErrorCode) -> &'static str {
    match code {
        ZephyrRestErrorCode::InvalidConfiguration => "zephyr_enterprise.configuration.invalid",
        ZephyrRestErrorCode::InvalidInput => "zephyr_enterprise.request.invalid",
        ZephyrRestErrorCode::Authentication => "zephyr_enterprise.authentication.failed",
        ZephyrRestErrorCode::Authorization => "zephyr_enterprise.authorization.failed",
        ZephyrRestErrorCode::NotFound => "zephyr_enterprise.resource.not_found",
        ZephyrRestErrorCode::Conflict => "zephyr_enterprise.resource.conflict",
        ZephyrRestErrorCode::RateLimited => "zephyr_enterprise.rate_limited",
        ZephyrRestErrorCode::Timeout => "zephyr_enterprise.timeout",
        ZephyrRestErrorCode::DependencyUnavailable => "zephyr_enterprise.unavailable",
        ZephyrRestErrorCode::InvalidResponse => "zephyr_enterprise.response.invalid",
        ZephyrRestErrorCode::ResourceExhausted => "zephyr_enterprise.response.resource_exhausted",
        ZephyrRestErrorCode::UnknownOutcome => "zephyr_enterprise.effect.unknown_outcome",
    }
}

/// One step to append, carried as the JSON values the model supplied: the
/// SDK reads `step.get("step", "")` and friends without coercion.
#[derive(Clone, Debug, PartialEq)]
pub(in crate::toolkits) struct EnterpriseStep {
    pub(in crate::toolkits) step: Value,
    pub(in crate::toolkits) data: Value,
    pub(in crate::toolkits) result: Value,
}

/// The SDK's `ZephyrClient` operations over one bearer client.
pub(crate) struct ZephyrEnterpriseClient {
    rest: ZephyrRestClient,
}

impl ZephyrEnterpriseClient {
    pub(crate) const fn new(rest: ZephyrRestClient) -> Self {
        Self { rest }
    }

    /// The instance URL the SDK appends to every tool description.
    pub(in crate::toolkits) fn instance(&self) -> String {
        self.rest.base().as_str().trim_end_matches('/').to_owned()
    }

    fn path<'a>(tail: &[&'a str]) -> Vec<&'a str> {
        let mut path = API.to_vec();
        path.extend_from_slice(tail);
        path
    }

    /// `GET testcase/{id}`: the provider object, or `""` for an empty body.
    pub(in crate::toolkits) async fn get_test_case(
        &self,
        testcase_id: &str,
    ) -> Result<Value, ZephyrRestError> {
        let reply = self
            .rest
            .call(
                Method::GET,
                &Self::path(&["testcase", testcase_id]),
                &[],
                None,
            )
            .await?;
        bounded_value(reply.into_value(), false)
    }

    /// `POST advancesearch/zql` with the decoded ZQL document. A search: a
    /// failure is a read failure, never an unknown effect.
    pub(in crate::toolkits) async fn search_by_zql(
        &self,
        zql: &Value,
    ) -> Result<Value, ZephyrRestError> {
        let reply = self
            .rest
            .post_read(&Self::path(&["advancesearch", "zql"]), zql)
            .await?;
        bounded_value(reply.into_value(), false)
    }

    /// `POST testcase/` (the SDK's route keeps the trailing slash).
    pub(in crate::toolkits) async fn create_testcase(
        &self,
        body: &Value,
    ) -> Result<Value, ZephyrRestError> {
        let reply = self
            .rest
            .call(
                Method::POST,
                &Self::path(&["testcase", ""]),
                &[],
                Some(body),
            )
            .await?;
        bounded_value(reply.into_value(), true)
    }

    /// `GET testcase?zqlquery=...`, rendered the SDK's way: one line per
    /// result, or its fixed sentence when `resultSize` is 0.
    pub(in crate::toolkits) async fn get_testcases_by_zql(
        &self,
        zql: &str,
    ) -> Result<Value, ZephyrRestError> {
        let response = self
            .rest
            .get_json(&Self::path(&["testcase"]), &[("zqlquery", zql.to_owned())])
            .await?;
        let object = response.as_object().ok_or_else(invalid_response)?;
        if object.get("resultSize").and_then(Value::as_i64) == Some(0) {
            return Ok(Value::String(
                "No test cases found for the provided ZQL query.".to_owned(),
            ));
        }
        let results = object
            .get("results")
            .and_then(Value::as_array)
            .ok_or_else(invalid_response)?;
        let mut lines = Vec::with_capacity(results.len());
        for result in results {
            let testcase = result.get("testcase").ok_or_else(invalid_response)?;
            lines.push(format!(
                "Test case ID: {}, Test case: {}",
                python_str(result.get("id")),
                render(testcase)?
            ));
        }
        bounded_text(lines.join("\n"), false)
    }

    /// The SDK's `add_steps`: resolve the test case's last version, read its
    /// current steps for the highest order, then append each step in order,
    /// chaining the returned step-set id.
    ///
    /// Before the first confirmed append every failure is the provider's; after
    /// it, any failure is an unknown outcome to reconcile.
    pub(in crate::toolkits) async fn add_steps(
        &self,
        testcase_tree_id: &str,
        steps: &[EnterpriseStep],
    ) -> Result<Value, ZephyrRestError> {
        let version_id = self.last_version(testcase_tree_id).await?;
        let version_segment = python_str(Some(&version_id));
        let current = self
            .rest
            .get_optional_json(
                &Self::path(&["testcase", &version_segment, "teststep"]),
                &[],
            )
            .await?
            .filter(is_truthy);
        let (mut order, mut steps_id) = match &current {
            Some(current) => {
                let max = match current.get("steps") {
                    Some(Value::Array(existing)) => existing
                        .iter()
                        .map(|step| order_id(step.get("orderId")))
                        .collect::<Result<Vec<_>, _>>()?
                        .into_iter()
                        .max()
                        .unwrap_or(0),
                    Some(Value::Null) => 0,
                    _ => return Err(invalid_response()),
                };
                (
                    max,
                    current.get("id").cloned().ok_or_else(invalid_response)?,
                )
            }
            None => (0, Value::Null),
        };
        let route = Self::path(&[
            "testcase",
            &version_segment,
            "teststep",
            "detail",
            testcase_tree_id,
        ]);
        let mut added = Vec::with_capacity(steps.len());
        for step in steps {
            order += 1;
            let body = json!({
                "tcId": version_id,
                "maxId": order,
                "step": {
                    "step": step.step,
                    "data": step.data,
                    "result": step.result,
                    "orderId": order
                },
                "tctId": testcase_tree_id,
                "id": steps_id
            });
            let reply = match self.rest.call(Method::POST, &route, &[], Some(&body)).await {
                Ok(reply) => reply,
                Err(_) if !added.is_empty() => {
                    return Err(ZephyrRestError::after_confirmed_effect());
                }
                Err(error) => return Err(error),
            };
            steps_id = match &reply {
                ZephyrReply::Json(value) => value.get("id").cloned(),
                ZephyrReply::Text(_) | ZephyrReply::Empty => None,
            }
            .ok_or_else(ZephyrRestError::after_confirmed_effect)?;
            added.push(format!(
                "Step added: {}, data: {}, result: {}",
                python_str(Some(&step.step)),
                python_str(Some(&step.data)),
                python_str(Some(&step.result))
            ));
        }
        bounded_text(added.join(";"), true)
    }

    /// The SDK's `get_last_version`: the tree node's `testcase.testcaseId`,
    /// then the last entry of `testcase/versions?testcaseid=`.
    async fn last_version(&self, testcase_tree_id: &str) -> Result<Value, ZephyrRestError> {
        let tree = self
            .rest
            .get_json(&Self::path(&["testcase", testcase_tree_id]), &[])
            .await?;
        let testcase_id = tree
            .get("testcase")
            .and_then(|testcase| testcase.get("testcaseId"))
            .filter(|value| !value.is_null())
            .ok_or_else(invalid_response)?;
        let versions = self
            .rest
            .get_json(
                &Self::path(&["testcase", "versions"]),
                &[("testcaseid", python_str(Some(testcase_id)))],
            )
            .await?;
        let id = versions
            .as_array()
            .and_then(|versions| versions.last())
            .and_then(|version| version.get("id"))
            .filter(|id| id.is_i64() || id.is_u64() || id.is_string())
            .cloned()
            .ok_or_else(invalid_response)?;
        if let Value::String(text) = &id
            && (text.is_empty() || text.contains('/'))
        {
            return Err(invalid_response());
        }
        Ok(id)
    }
}

/// Python truthiness of a decoded body: `None`, `{}`, `[]` and `""` are falsy.
fn is_truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Object(object) => !object.is_empty(),
        Value::Array(values) => !values.is_empty(),
        Value::String(text) => !text.is_empty(),
        Value::Bool(flag) => *flag,
        Value::Number(_) => true,
    }
}

/// `int(step["orderId"])`: an integer, or a string holding one.
fn order_id(value: Option<&Value>) -> Result<i64, ZephyrRestError> {
    match value {
        Some(Value::Number(number)) => number.as_i64().ok_or_else(invalid_response),
        Some(Value::String(text)) => text.trim().parse().map_err(|_| invalid_response()),
        _ => Err(invalid_response()),
    }
}
