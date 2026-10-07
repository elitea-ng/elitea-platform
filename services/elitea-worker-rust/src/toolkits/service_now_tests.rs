use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Tool, Toolset};
use async_trait::async_trait;
use reqwest::{Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::service_now::client::{
    ServiceNowApi, ServiceNowClient, ServiceNowClientError, ServiceNowClientErrorCode,
    ServiceNowRequestKind, ServiceNowTransport, test_http_status,
};
use super::families::service_now::config::{ServiceNowConfigErrorCode, ServiceNowToolkitConfig};
use super::families::service_now::tools::{
    ServiceNowToolsetErrorCode, build_service_now_toolset, test_build_with_api,
};
use super::policy::ToolAdmissionPolicy;

fn settings() -> Map<String, Value> {
    json!({
        "response_fields": "number, Short_Description, sys_id, number",
        "servicenow_configuration": {
            "base_url": "https://example.service-now.com",
            "username": "worker-user",
            "password": "worker-password"
        },
        "selected_tools": []
    })
    .as_object()
    .cloned()
    .expect("fixture settings are an object")
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(
        ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("empty fixture policy is valid"),
    )
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("service-now-tool-test")
            .with_session_id("session-1")
            .with_function_call_id("call-1"),
    )
}

#[derive(Default)]
struct CapturingTransport {
    requests: Mutex<Vec<(Request, StatusCode, bool)>>,
    responses: Mutex<Vec<Value>>,
}

impl CapturingTransport {
    fn with_responses(responses: Vec<Value>) -> Self {
        Self {
            requests: Mutex::new(Vec::new()),
            responses: Mutex::new(responses.into_iter().rev().collect()),
        }
    }

    fn take_requests(&self) -> Vec<(Request, StatusCode, bool)> {
        std::mem::take(
            &mut *self
                .requests
                .lock()
                .expect("fixture request mutex is not poisoned"),
        )
    }
}

#[async_trait]
impl ServiceNowTransport for CapturingTransport {
    async fn execute_json(
        &self,
        request: Request,
        expected_status: StatusCode,
        retryable_transport: bool,
    ) -> Result<Value, ServiceNowClientError> {
        self.requests
            .lock()
            .expect("fixture request mutex is not poisoned")
            .push((request, expected_status, retryable_transport));
        self.responses
            .lock()
            .expect("fixture response mutex is not poisoned")
            .pop()
            .ok_or_else(|| fixture_error(ServiceNowClientErrorCode::InvalidResponse, false))
    }
}

struct FixtureApi {
    calls: Mutex<Vec<Value>>,
}

impl FixtureApi {
    fn new() -> Self {
        Self {
            calls: Mutex::new(Vec::new()),
        }
    }

    fn calls(&self) -> Vec<Value> {
        self.calls
            .lock()
            .expect("fixture call mutex is not poisoned")
            .clone()
    }
}

#[async_trait]
impl ServiceNowApi for FixtureApi {
    async fn get_incidents(
        &self,
        filters: &Map<String, Value>,
        limit: usize,
    ) -> Result<Value, ServiceNowClientError> {
        self.calls
            .lock()
            .expect("fixture call mutex is not poisoned")
            .push(json!({"operation":"get", "filters": filters, "limit": limit}));
        Ok(json!([{"number":"INC001"}]))
    }

    async fn create_incident(
        &self,
        fields: &Map<String, Value>,
    ) -> Result<Value, ServiceNowClientError> {
        self.calls
            .lock()
            .expect("fixture call mutex is not poisoned")
            .push(json!({"operation":"create", "fields": fields}));
        Ok(json!([{"number":"INC002"}]))
    }

    async fn update_incident(
        &self,
        sys_id: &str,
        fields: &Map<String, Value>,
    ) -> Result<Value, ServiceNowClientError> {
        self.calls
            .lock()
            .expect("fixture call mutex is not poisoned")
            .push(json!({"operation":"update", "sys_id":sys_id, "fields":fields}));
        Ok(json!([{"number":"INC003"}]))
    }
}

#[test]
fn configuration_is_claim_scoped_bounded_and_redacted() {
    let parsed = ServiceNowToolkitConfig::parse(&settings())
        .expect("valid materialized ServiceNow settings parse");
    assert!(parsed.selected_tools().is_empty());

    let short = json!({
        "servicenow_configuration": {
            "base_url": "dev12345",
            "username": "name",
            "password": "secret"
        }
    });
    assert!(
        ServiceNowToolkitConfig::parse(
            short
                .as_object()
                .expect("short-instance fixture is an object")
        )
        .is_ok()
    );

    for mutation in [
        json!(null),
        json!(""),
        json!("http://example.service-now.com"),
        json!("https://user@example.service-now.com"),
        json!("https://example.service-now.com/path"),
    ] {
        let mut invalid = settings();
        invalid
            .get_mut("servicenow_configuration")
            .and_then(Value::as_object_mut)
            .expect("fixture nested configuration is an object")
            .insert("base_url".to_owned(), mutation);
        let error = ServiceNowToolkitConfig::parse(&invalid)
            .err()
            .expect("invalid origin is rejected");
        assert_eq!(
            error.code(),
            ServiceNowConfigErrorCode::InvalidConfiguration
        );
        assert!(!format!("{error:?}").contains("worker-password"));
    }

    for field in ["username", "password"] {
        let mut invalid = settings();
        invalid
            .get_mut("servicenow_configuration")
            .and_then(Value::as_object_mut)
            .expect("fixture nested configuration is an object")
            .insert(field.to_owned(), Value::Null);
        assert!(ServiceNowToolkitConfig::parse(&invalid).is_err());
    }
}

#[test]
fn response_fields_default_normalize_and_never_expand_empty_to_all() {
    let parsed = ServiceNowToolkitConfig::parse(&settings())
        .expect("valid materialized ServiceNow settings parse");
    assert_eq!(
        parsed
            .test_response_fields()
            .iter()
            .map(AsRef::as_ref)
            .collect::<Vec<&str>>(),
        ["number", "short_description", "sys_id"]
    );

    for value in [Value::Null, Value::String(String::new())] {
        let mut settings = settings();
        settings.insert("response_fields".to_owned(), value);
        let parsed = ServiceNowToolkitConfig::parse(&settings)
            .expect("null and empty fields use bounded defaults");
        assert_eq!(parsed.test_response_fields().len(), 10);
    }
}

#[tokio::test]
async fn get_wire_is_exact_bounded_and_drops_the_control_filter() {
    let transport = Arc::new(CapturingTransport::with_responses(vec![json!({
        "result": [{
            "number": {"value":"INC001", "display_value":"INC001"},
            "short_description": {"value":"raw", "display_value":"display"},
            "priority": {"value":null, "display_value":"2 - High"},
            "unpopulated": {"unexpected":"ignored"},
            "sys_id": "0123456789abcdef0123456789abcdef"
        }]
    })]));
    let client = ServiceNowClient::with_transport(
        ServiceNowToolkitConfig::parse(&settings()).expect("valid config"),
        transport.clone(),
    );
    let filters = json!({
        "description": "network issue",
        "category": "network",
        "number_of_entries": 7
    });
    let output = client
        .get_incidents(filters.as_object().expect("filter fixture is an object"), 7)
        .await
        .expect("fixture response projects");
    assert_eq!(
        output,
        json!([{
            "number":"INC001",
            "priority":"2 - High",
            "short_description":"raw",
            "unpopulated":null,
            "sys_id":"0123456789abcdef0123456789abcdef"
        }])
    );

    let requests = transport.take_requests();
    assert_eq!(requests.len(), 1);
    let (request, status, retryable) = &requests[0];
    assert_eq!(request.method(), reqwest::Method::GET);
    assert_eq!(request.url().path(), "/api/now/table/incident");
    assert_eq!(*status, StatusCode::OK);
    assert!(*retryable);
    assert!(request.headers()["authorization"].is_sensitive());
    let query = request.url().query_pairs().collect::<BTreeMap<_, _>>();
    assert_eq!(query["sysparm_display_value"], "all");
    assert_eq!(query["sysparm_fields"], "number,short_description,sys_id");
    assert_eq!(query["sysparm_limit"], "7");
    assert_eq!(query["sysparm_offset"], "0");
    assert!(query["sysparm_query"].contains("category=network"));
    assert!(query["sysparm_query"].contains("descriptionCONTAINSnetwork issue"));
    assert!(query["sysparm_query"].ends_with("ORDERBYsys_id"));
    assert!(!query["sysparm_query"].contains("number_of_entries"));
}

#[tokio::test]
async fn create_and_update_use_one_post_then_get_patch_without_write_retry() {
    let transport = Arc::new(CapturingTransport::with_responses(vec![
        json!({"result":{"number":{"value":"INC002"},"extra":{"value":"created"}}}),
        json!({"result":{"sys_id":{"value":"0123456789abcdef0123456789abcdef"}}}),
        json!({"result":{"number":{"value":"INC002"},"extra":{"value":"updated"}}}),
    ]));
    let client = ServiceNowClient::with_transport(
        ServiceNowToolkitConfig::parse(&settings()).expect("valid config"),
        transport.clone(),
    );

    assert_eq!(
        client
            .create_incident(
                json!({"short_description":"created", "ignored":null})
                    .as_object()
                    .expect("create fixture is an object")
            )
            .await
            .expect("create projects"),
        json!([{"number":"INC002","extra":"created"}])
    );
    assert_eq!(
        client
            .update_incident(
                "0123456789abcdef0123456789abcdef",
                json!({
                    "short_description":"updated",
                    "assignment_group":"network-operations",
                    "ignored":null
                })
                .as_object()
                .expect("update fixture is an object")
            )
            .await
            .expect("update projects"),
        json!([{"number":"INC002","extra":"updated"}])
    );

    let requests = transport.take_requests();
    assert_eq!(requests.len(), 3);
    assert_eq!(requests[0].0.method(), reqwest::Method::POST);
    assert_eq!(requests[1].0.method(), reqwest::Method::GET);
    assert_eq!(requests[2].0.method(), reqwest::Method::PATCH);
    assert!(!requests[0].2);
    assert!(requests[1].2);
    assert!(!requests[2].2);
    let create_body = request_json(&requests[0].0);
    let update_body = request_json(&requests[2].0);
    assert_eq!(create_body, json!({"short_description":"created"}));
    assert_eq!(
        update_body,
        json!({
            "short_description":"updated",
            "assignment_group":"network-operations"
        })
    );
    assert!(
        requests[2]
            .0
            .url()
            .path()
            .ends_with("/0123456789abcdef0123456789abcdef")
    );
}

#[test]
fn request_validation_rejects_query_injection_bad_ids_and_oversize() {
    let client = ServiceNowClient::with_transport(
        ServiceNowToolkitConfig::parse(&settings()).expect("valid config"),
        Arc::new(CapturingTransport::default()),
    );
    let injected = json!({"description":"safe^ORactive=true"});
    assert_eq!(
        client
            .test_request(ServiceNowRequestKind::GetIncidents {
                filters: injected.as_object().expect("fixture is an object"),
                limit: 1
            })
            .expect_err("query injection is rejected")
            .code(),
        ServiceNowClientErrorCode::InvalidInput
    );
    let empty = Map::new();
    for sys_id in ["", "not-a-32-byte-sys-id", "../../incident"] {
        assert!(
            client
                .test_request(ServiceNowRequestKind::UpdateIncident {
                    sys_id,
                    fields: &empty
                })
                .is_err()
        );
    }
    assert!(
        client
            .test_request(ServiceNowRequestKind::GetIncidents {
                filters: &empty,
                limit: 101
            })
            .is_err()
    );
}

fn assert_service_now_model_contract(tools: &[Arc<dyn Tool>]) {
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        ["get_incidents", "create_incident", "update_incident"]
    );
    assert!(tools[0].is_read_only());
    assert!(tools[0].is_concurrency_safe());
    assert!(!tools[1].is_read_only());
    assert!(!tools[1].is_concurrency_safe());
    assert!(!tools[2].is_read_only());

    assert!(tools[0].description().contains("optional filters"));
    assert!(tools[1].description().contains("supplied fields"));
    assert!(tools[2].description().contains("selected by its sys_id"));
    let schemas = tools
        .iter()
        .map(|tool| {
            tool.parameters_schema()
                .expect("every ServiceNow tool has an argument schema")
        })
        .collect::<Vec<_>>();
    assert_eq!(schemas[0]["title"], "getIncidents");
    assert!(
        schemas[0]["properties"]["data"]["description"]
            .as_str()
            .expect("get data description is text")
            .contains("number_of_entries (an integer from 1 through 100)")
    );
    assert_eq!(
        schemas[0]["properties"]["data"]["examples"][0],
        json!({"description":"Network issue", "category":"network"})
    );
    assert_eq!(
        schemas[0]["properties"]["data"]["anyOf"][0]["additionalProperties"],
        json!(false)
    );
    assert_eq!(
        schemas[0]["properties"]["data"]["anyOf"][0]["properties"]["number_of_entries"]["maximum"],
        json!(100)
    );
    assert_eq!(schemas[0]["additionalProperties"], json!(false));
    assert!(
        schemas[1]["properties"]["data"]["description"]
            .as_str()
            .expect("create data description is text")
            .contains("short_description")
    );
    assert!(
        schemas[1]["properties"]["data"]["description"]
            .as_str()
            .expect("create data description is text")
            .contains("null-valued fields are omitted")
    );
    assert_eq!(
        schemas[1]["properties"]["data"]["anyOf"][0]["additionalProperties"],
        json!(true)
    );
    assert_eq!(schemas[2]["required"], json!(["sys_id", "update_fields"]));
    assert_eq!(
        schemas[2]["properties"]["sys_id"]["pattern"],
        json!("^[A-Za-z0-9]{32}$")
    );
    assert_eq!(
        schemas[2]["properties"]["update_fields"]["maxLength"],
        json!(65_536)
    );
    assert!(
        schemas[2]["properties"]["update_fields"]["description"]
            .as_str()
            .expect("update_fields description is text")
            .contains("JSON object encoded as a string")
    );
}

#[tokio::test]
async fn tools_preserve_catalog_groups_defaults_json_strings_and_policy() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn ServiceNowApi> = api.clone();
    let toolset = test_build_with_api("ITSM", &[], &policy(), &api_trait)
        .expect("complete fixture toolset builds");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset
        .tools(readonly)
        .await
        .expect("fixture ServiceNow tools resolve");
    assert_service_now_model_contract(&tools);

    let get = tools[0]
        .execute(context(), json!({"data": null}))
        .await
        .expect("get executes");
    let create = tools[1]
        .execute(
            context(),
            json!({"data":{"impact":2,"metadata":{"source":"monitor"}}}),
        )
        .await
        .expect("create accepts the SDK's arbitrary JSON field values");
    let update = tools[2]
        .execute(
            context(),
            json!({
                "sys_id":"0123456789abcdef0123456789abcdef",
                "update_fields":"{\"urgency\":\"1\",\"ignored\":null}"
            }),
        )
        .await
        .expect("update executes");
    assert_eq!(get, Value::String("[{\"number\":\"INC001\"}]".to_owned()));
    assert_eq!(
        create,
        Value::String("[{\"number\":\"INC002\"}]".to_owned())
    );
    assert_eq!(
        update,
        Value::String("[{\"number\":\"INC003\"}]".to_owned())
    );
    assert_eq!(
        api.calls(),
        [
            json!({"operation":"get", "filters":{}, "limit":100}),
            json!({
                "operation":"create",
                "fields":{"impact":2,"metadata":{"source":"monitor"}}
            }),
            json!({
                "operation":"update",
                "sys_id":"0123456789abcdef0123456789abcdef",
                "fields":{"urgency":"1","ignored":null}
            })
        ]
    );
}

#[tokio::test]
async fn selection_and_arguments_fail_closed_before_client_use() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn ServiceNowApi> = api.clone();
    let selected = vec!["get_incidents".to_owned()];
    let toolset = test_build_with_api("ITSM", &selected, &policy(), &api_trait)
        .expect("selected fixture toolset builds");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert_eq!(
        toolset
            .tools(readonly)
            .await
            .expect("selected ServiceNow tools resolve")
            .len(),
        1
    );

    let mut blocked = BTreeMap::new();
    blocked.insert("service_now".to_owned(), vec!["get_incidents".to_owned()]);
    let blocked_policy =
        Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("blocked fixture policy is valid"));
    let blocked = test_build_with_api("ITSM", &selected, &blocked_policy, &api_trait)
        .expect("blocked fixture toolset builds");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        blocked
            .tools(readonly)
            .await
            .expect("blocked ServiceNow tools resolve")
            .is_empty()
    );
    assert!(api.calls().is_empty());
}

#[test]
fn client_errors_are_stable_redacted_and_effect_retry_safe() {
    for (code, retryable) in [
        (ServiceNowClientErrorCode::Authentication, false),
        (ServiceNowClientErrorCode::Authorization, false),
        (ServiceNowClientErrorCode::NotFound, false),
        (ServiceNowClientErrorCode::RateLimited, true),
        (ServiceNowClientErrorCode::Timeout, true),
        (ServiceNowClientErrorCode::DependencyUnavailable, true),
        (ServiceNowClientErrorCode::InvalidResponse, false),
        (ServiceNowClientErrorCode::ResourceExhausted, false),
    ] {
        let error = fixture_error(code, retryable);
        assert_eq!(error.retryable(), retryable);
        let rendered = format!("{error:?} {error}");
        assert!(!rendered.contains("worker-password"));
        assert!(!rendered.contains("example.service-now.com"));
    }
}

#[test]
fn http_taxonomy_is_stable_and_write_ambiguity_is_never_retryable() {
    for (status, code, read_retryable) in [
        (
            StatusCode::BAD_REQUEST,
            ServiceNowClientErrorCode::InvalidInput,
            false,
        ),
        (
            StatusCode::UNAUTHORIZED,
            ServiceNowClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            ServiceNowClientErrorCode::Authorization,
            false,
        ),
        (
            StatusCode::NOT_FOUND,
            ServiceNowClientErrorCode::NotFound,
            false,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            ServiceNowClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::GATEWAY_TIMEOUT,
            ServiceNowClientErrorCode::Timeout,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            ServiceNowClientErrorCode::DependencyUnavailable,
            true,
        ),
    ] {
        let read =
            test_http_status(status, StatusCode::OK, true).expect_err("fixture status must fail");
        assert_eq!(read.code(), code);
        assert_eq!(read.retryable(), read_retryable);

        let write = test_http_status(status, StatusCode::CREATED, false)
            .expect_err("fixture write status must fail");
        assert_eq!(write.code(), code);
        assert!(!write.retryable());
    }
}

#[test]
fn invocation_scoped_clients_do_not_overwrite_endpoint_credentials_or_fields() {
    let mut first = settings();
    first.insert("response_fields".to_owned(), json!("number"));
    let first_configuration = first
        .get_mut("servicenow_configuration")
        .and_then(Value::as_object_mut)
        .expect("first fixture nested configuration is an object");
    first_configuration.insert(
        "base_url".to_owned(),
        json!("https://first.service-now.com"),
    );
    first_configuration.insert("password".to_owned(), json!("first-secret"));

    let mut second = settings();
    second.insert("response_fields".to_owned(), json!("category"));
    let second_configuration = second
        .get_mut("servicenow_configuration")
        .and_then(Value::as_object_mut)
        .expect("second fixture nested configuration is an object");
    second_configuration.insert(
        "base_url".to_owned(),
        json!("https://second.service-now.com"),
    );
    second_configuration.insert("password".to_owned(), json!("second-secret"));

    let first = ServiceNowClient::with_transport(
        ServiceNowToolkitConfig::parse(&first).expect("first config"),
        Arc::new(CapturingTransport::default()),
    );
    let second = ServiceNowClient::with_transport(
        ServiceNowToolkitConfig::parse(&second).expect("second config"),
        Arc::new(CapturingTransport::default()),
    );
    let empty = Map::new();
    let first = first
        .test_request(ServiceNowRequestKind::GetIncidents {
            filters: &empty,
            limit: 1,
        })
        .expect("first request");
    let second = second
        .test_request(ServiceNowRequestKind::GetIncidents {
            filters: &empty,
            limit: 1,
        })
        .expect("second request");
    assert_eq!(first.url().host_str(), Some("first.service-now.com"));
    assert_eq!(second.url().host_str(), Some("second.service-now.com"));
    assert_ne!(
        first.headers()["authorization"],
        second.headers()["authorization"]
    );
    let first_query = first.url().query_pairs().collect::<BTreeMap<_, _>>();
    let second_query = second.url().query_pairs().collect::<BTreeMap<_, _>>();
    assert_eq!(first_query["sysparm_fields"], "sys_id,number");
    assert_eq!(second_query["sysparm_fields"], "sys_id,category");
}

#[test]
fn unsupported_selection_fails_before_provider_use() {
    let mut settings = settings();
    settings.insert("selected_tools".to_owned(), json!(["delete_incident"]));
    let config =
        ServiceNowToolkitConfig::parse(&settings).expect("bounded unknown selection parses");
    let error = build_service_now_toolset("ITSM", config, &policy())
        .err()
        .expect("unknown selected tool fails closed");
    assert_eq!(
        error.code(),
        ServiceNowToolsetErrorCode::UnsupportedSelection
    );
}

fn request_json(request: &Request) -> Value {
    let bytes = request
        .body()
        .and_then(reqwest::Body::as_bytes)
        .expect("fixture request carries an in-memory body");
    serde_json::from_slice(bytes).expect("fixture request body is JSON")
}

fn fixture_error(code: ServiceNowClientErrorCode, retryable: bool) -> ServiceNowClientError {
    // Test-only construction is intentionally kept in the family module by
    // parsing an error response through the public stable fields below.
    ServiceNowClientError::fixture(code, retryable)
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn ServiceNowApi> = Arc::new(FixtureApi::new());
    let toolset =
        test_build_with_api("gate", &[], &policy(), &api).expect("complete service_now toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("service_now tools");
    super::sdk_conformance::assert_sdk_conformance("service_now", &tools);
}
