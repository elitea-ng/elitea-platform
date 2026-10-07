//! Focused compatibility and safety tests for the legacy Zephyr ZAPI family.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::zephyr::client::{
    ZephyrApi, ZephyrClient, ZephyrClientError, ZephyrClientErrorCode, ZephyrHttpResponse,
    ZephyrStep, ZephyrTransport,
};
use super::families::zephyr::config::{ZephyrConfigErrorCode, ZephyrToolkitConfig};
use super::families::zephyr::tools::{ZephyrToolsetErrorCode, test_build_with_api, test_catalog};
use super::policy::ToolAdmissionPolicy;

const BASE_URL: &str = "https://jira.example.test:8443/rest/zapi/latest/";
const USERNAME: &str = "qa@example.test";
const PASSWORD: &str = "test-secret-password";

fn settings(selected_tools: &[&str]) -> Map<String, Value> {
    json!({
        "base_url":BASE_URL,
        "username":USERNAME,
        "password":PASSWORD,
        "selected_tools":selected_tools
    })
    .as_object()
    .cloned()
    .expect("legacy Zephyr settings fixture is an object")
}

fn config(selected_tools: &[&str]) -> ZephyrToolkitConfig {
    ZephyrToolkitConfig::parse(&settings(selected_tools)).expect("valid legacy Zephyr config")
}

fn policy(blocked: &[(&str, &[&str])]) -> Arc<ToolAdmissionPolicy> {
    let blocked = blocked
        .iter()
        .map(|(toolkit, tools)| {
            (
                (*toolkit).to_owned(),
                tools.iter().map(|tool| (*tool).to_owned()).collect(),
            )
        })
        .collect::<BTreeMap<_, _>>();
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("legacy Zephyr test policy"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("legacy-zephyr-test")
            .with_session_id("session-1")
            .with_function_call_id("zephyr-call-1"),
    )
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct RecordedRequest {
    method: Method,
    url: String,
    authorization: Option<String>,
    content_type: Option<String>,
    body: Vec<u8>,
}

struct FixtureTransport {
    responses: Mutex<VecDeque<Result<ZephyrHttpResponse, ZephyrClientError>>>,
    requests: Mutex<Vec<RecordedRequest>>,
}

impl FixtureTransport {
    fn new(
        responses: impl IntoIterator<Item = Result<ZephyrHttpResponse, ZephyrClientError>>,
    ) -> Self {
        Self {
            responses: Mutex::new(responses.into_iter().collect()),
            requests: Mutex::new(Vec::new()),
        }
    }

    fn requests(&self) -> Vec<RecordedRequest> {
        self.requests
            .lock()
            .expect("legacy Zephyr request fixture lock")
            .clone()
    }
}

#[async_trait]
impl ZephyrTransport for FixtureTransport {
    async fn execute(&self, request: Request) -> Result<ZephyrHttpResponse, ZephyrClientError> {
        self.requests
            .lock()
            .expect("legacy Zephyr request fixture lock")
            .push(RecordedRequest {
                method: request.method().clone(),
                url: request.url().to_string(),
                authorization: header(&request, reqwest::header::AUTHORIZATION),
                content_type: header(&request, reqwest::header::CONTENT_TYPE),
                body: request
                    .body()
                    .and_then(reqwest::Body::as_bytes)
                    .map_or_else(Vec::new, <[u8]>::to_vec),
            });
        self.responses
            .lock()
            .expect("legacy Zephyr response fixture lock")
            .pop_front()
            .unwrap_or_else(|| {
                Err(ZephyrClientError::fixture(
                    ZephyrClientErrorCode::InvalidResponse,
                    false,
                ))
            })
    }
}

fn header(request: &Request, name: reqwest::header::HeaderName) -> Option<String> {
    request
        .headers()
        .get(name)
        .and_then(|value| value.to_str().ok())
        .map(str::to_owned)
}

struct FixtureApi {
    calls: Mutex<Vec<Value>>,
    responses: Mutex<VecDeque<Result<Value, ZephyrClientError>>>,
}

impl FixtureApi {
    fn success() -> Self {
        Self::new([])
    }

    fn new(responses: impl IntoIterator<Item = Result<Value, ZephyrClientError>>) -> Self {
        Self {
            calls: Mutex::new(Vec::new()),
            responses: Mutex::new(responses.into_iter().collect()),
        }
    }

    fn calls(&self) -> Vec<Value> {
        self.calls
            .lock()
            .expect("legacy Zephyr API fixture lock")
            .clone()
    }

    fn response(&self, fallback: Value) -> Result<Value, ZephyrClientError> {
        self.responses
            .lock()
            .expect("legacy Zephyr API response fixture lock")
            .pop_front()
            .unwrap_or(Ok(fallback))
    }
}

#[async_trait]
impl ZephyrApi for FixtureApi {
    async fn get_test_case_steps(
        &self,
        issue_id: u64,
        project_id: u64,
    ) -> Result<Value, ZephyrClientError> {
        self.calls
            .lock()
            .expect("legacy Zephyr API fixture lock")
            .push(json!({"tool":"get","issue_id":issue_id,"project_id":project_id}));
        self.response(Value::String("No Zephyr test steps found".to_owned()))
    }

    async fn add_new_test_case_step(
        &self,
        issue_id: u64,
        project_id: u64,
        step: &ZephyrStep,
    ) -> Result<Value, ZephyrClientError> {
        self.calls
            .lock()
            .expect("legacy Zephyr API fixture lock")
            .push(json!({
                "tool":"add",
                "issue_id":issue_id,
                "project_id":project_id,
                "step":step.step.as_ref(),
                "data":step.data.as_ref(),
                "result":step.result.as_ref()
            }));
        self.response(Value::String("New test step created: {}".to_owned()))
    }
}

fn client_with(transport: Arc<FixtureTransport>) -> ZephyrClient {
    let transport_trait: Arc<dyn ZephyrTransport> = transport;
    ZephyrClient::with_transport(config(&[]), transport_trait)
}

async fn tools_for(api: Arc<dyn ZephyrApi>, selected: &[String]) -> Vec<Arc<dyn Tool>> {
    let toolset = test_build_with_api("server-zapi", selected, &policy(&[]), &api)
        .expect("legacy Zephyr fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    toolset.tools(readonly).await.expect("legacy Zephyr tools")
}

#[test]
fn inline_configuration_is_fixed_origin_deduplicated_and_redacted() {
    let parsed = config(&[
        "get_test_case_steps",
        "get_test_case_steps",
        "add_test_case",
    ]);
    assert_eq!(
        parsed.selected_tools(),
        [
            Box::<str>::from("get_test_case_steps"),
            Box::<str>::from("add_test_case")
        ]
    );

    for invalid in [
        json!({"username":USERNAME,"password":PASSWORD}),
        json!({"base_url":BASE_URL,"password":PASSWORD}),
        json!({"base_url":BASE_URL,"username":USERNAME}),
        json!({"base_url":"http://jira.example.test","username":USERNAME,"password":PASSWORD}),
        json!({"base_url":"https://user@jira.example.test","username":USERNAME,"password":PASSWORD}),
        json!({"base_url":"https://jira.example.test/rest?x=1","username":USERNAME,"password":PASSWORD}),
        json!({"base_url":BASE_URL,"username":"bad:user","password":PASSWORD}),
    ] {
        let Err(error) = ZephyrToolkitConfig::parse(
            invalid
                .as_object()
                .expect("invalid legacy Zephyr config is an object"),
        ) else {
            panic!("invalid legacy Zephyr configuration must fail");
        };
        assert_eq!(error.code(), ZephyrConfigErrorCode::InvalidConfiguration);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains(PASSWORD));
        assert!(!diagnostic.contains("jira.example"));
    }

    let mut oversized = settings(&[]);
    oversized.insert("password".to_owned(), json!("p".repeat(16 * 1_024 + 1)));
    let Err(error) = ZephyrToolkitConfig::parse(&oversized) else {
        panic!("oversized legacy Zephyr password must fail");
    };
    assert_eq!(error.code(), ZephyrConfigErrorCode::ResourceExhausted);
}

#[tokio::test]
async fn catalog_schemas_descriptions_selection_and_policy_are_truthful() {
    let api: Arc<dyn ZephyrApi> = Arc::new(FixtureApi::success());
    let tools = tools_for(Arc::clone(&api), &[]).await;
    assert_eq!(
        test_catalog(),
        [
            ("get_test_case_steps", "read"),
            ("add_new_test_case_step", "write"),
            ("add_test_case", "write"),
            ("add_test_cases", "write")
        ]
    );
    assert_eq!(tools.len(), 4);
    assert!(tools[0].is_read_only());
    assert!(tools[0].is_concurrency_safe());
    for tool in &tools[1..] {
        assert!(!tool.is_read_only());
        assert!(!tool.is_concurrency_safe());
        assert!(tool.description().contains("reconcile"));
    }
    for tool in &tools {
        assert!(tool.description().len() <= 1_000);
        assert!(!tool.description().contains(BASE_URL));
        assert!(!tool.description().contains(PASSWORD));
    }
    assert!(
        tools[0]
            .description()
            .contains("order_id, step, data, and result")
    );
    assert!(
        tools[2]
            .description()
            .contains("1 through 50 ordered steps")
    );
    assert!(tools[3].description().contains("at most 100 total steps"));

    let add_one = tools[1].parameters_schema().expect("add-one schema");
    assert_eq!(add_one["title"], "ZephyrAddNewTestStep");
    assert_eq!(
        add_one["required"],
        json!(["issue_id", "project_id", "step", "data", "result"])
    );
    assert_eq!(add_one["properties"]["step"]["maxLength"], 16_384);
    assert!(
        add_one["properties"]["data"]["description"]
            .as_str()
            .is_some_and(|value| value.contains("empty string"))
    );
    let batch = tools[3].parameters_schema().expect("batch schema");
    assert_eq!(
        batch["properties"]["create_test_cases_data"]["maxLength"],
        16_384
    );
    assert!(
        batch["properties"]["create_test_cases_data"]["description"]
            .as_str()
            .is_some_and(|value| value.contains("64 KiB"))
    );

    let selected = tools_for(api, &["add_test_case".to_owned()]).await;
    assert_eq!(selected.len(), 1);
    assert_eq!(selected[0].name(), "add_test_case");
    let unknown = test_build_with_api(
        "server-zapi",
        &["unknown".to_owned()],
        &policy(&[]),
        &(Arc::new(FixtureApi::success()) as Arc<dyn ZephyrApi>),
    );
    assert!(matches!(
        unknown,
        Err(error) if error.code() == ZephyrToolsetErrorCode::UnsupportedSelection
    ));

    let blocked = test_build_with_api(
        "server-zapi",
        &[],
        &policy(&[("zephyr", &["add_test_case"])]),
        &(Arc::new(FixtureApi::success()) as Arc<dyn ZephyrApi>),
    )
    .expect("blocked legacy Zephyr definition");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let names = blocked
        .tools(readonly)
        .await
        .expect("filtered legacy Zephyr tools")
        .into_iter()
        .map(|tool| tool.name().to_owned())
        .collect::<Vec<_>>();
    assert!(!names.iter().any(|name| name == "add_test_case"));
}

#[tokio::test]
async fn exact_get_and_post_wire_preserve_zapi_path_basic_auth_and_body() {
    let transport = Arc::new(FixtureTransport::new([
        Ok(ZephyrHttpResponse::fixture(
            StatusCode::OK,
            Some("application/json; charset=utf-8"),
            br#"{"stepBeanCollection":[{"orderId":1,"step":"Open","data":"","result":"Visible","ignored":true}]}"#,
        )),
        Ok(ZephyrHttpResponse::fixture(
            StatusCode::OK,
            Some("text/plain"),
            br#"{"id":7}"#,
        )),
    ]));
    let client = client_with(Arc::clone(&transport));
    let read = client
        .get_test_case_steps(10000, 10100)
        .await
        .expect("legacy Zephyr read result");
    assert_eq!(
        read,
        Value::String(
            "Found 1 test steps:\n[{\"data\":\"\",\"order_id\":1,\"result\":\"Visible\",\"step\":\"Open\"}]"
                .to_owned()
        )
    );
    let write = client
        .add_new_test_case_step(
            10000,
            10100,
            &ZephyrStep::new("Click", "language=Rust", "Results appear"),
        )
        .await
        .expect("legacy Zephyr create result");
    assert_eq!(
        write,
        Value::String("New test step created: {\"id\":7}".to_owned())
    );

    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[0].method, Method::GET);
    assert_eq!(
        requests[0].url,
        "https://jira.example.test:8443/rest/zapi/latest/teststep/10000?projectId=10100"
    );
    assert!(requests[0].body.is_empty());
    assert_eq!(requests[1].method, Method::POST);
    assert_eq!(requests[1].url, requests[0].url);
    assert_eq!(
        requests[1].content_type.as_deref(),
        Some("application/json")
    );
    assert_eq!(
        serde_json::from_slice::<Value>(&requests[1].body).expect("POST body JSON"),
        json!({"step":"Click","data":"language=Rust","result":"Results appear"})
    );
    let expected_auth = format!(
        "Basic {}",
        STANDARD.encode(format!("{USERNAME}:{PASSWORD}"))
    );
    assert_eq!(
        requests[0].authorization.as_deref(),
        Some(expected_auth.as_str())
    );
    assert_eq!(
        requests[1].authorization.as_deref(),
        Some(expected_auth.as_str())
    );
}

#[tokio::test]
async fn empty_and_malformed_read_responses_are_distinct_and_bounded() {
    let empty = Arc::new(FixtureTransport::new([Ok(ZephyrHttpResponse::fixture(
        StatusCode::OK,
        Some("application/json"),
        br#"{"stepBeanCollection":[]}"#,
    ))]));
    assert_eq!(
        client_with(empty)
            .get_test_case_steps(1, 2)
            .await
            .expect("empty step collection"),
        Value::String("No Zephyr test steps found".to_owned())
    );

    for (content_type, body) in [
        (
            Some("text/html"),
            br#"{"stepBeanCollection":[]}"#.as_slice(),
        ),
        (Some("application/json"), br#"{"wrong":[]}"#.as_slice()),
        (
            Some("application/json"),
            br#"{"stepBeanCollection":[{}]}"#.as_slice(),
        ),
    ] {
        let transport = Arc::new(FixtureTransport::new([Ok(ZephyrHttpResponse::fixture(
            StatusCode::OK,
            content_type,
            body,
        ))]));
        let error = client_with(transport)
            .get_test_case_steps(1, 2)
            .await
            .expect_err("malformed legacy Zephyr response must fail");
        assert_eq!(error.code(), ZephyrClientErrorCode::InvalidResponse);
    }
}

#[tokio::test]
async fn batch_tools_prevalidate_then_preserve_case_and_step_order() {
    let api = Arc::new(FixtureApi::success());
    let api_trait: Arc<dyn ZephyrApi> = api.clone();
    let tools = tools_for(api_trait, &["add_test_cases".to_owned()]).await;
    let result = tools[0]
        .execute(
            context() as Arc<dyn ToolContext>,
            json!({
                "create_test_cases_data":serde_json::to_string(&json!([
                    {"issue_id":10,"project_id":20,"steps":[
                        {"step":"a","data":"a-data","result":"a-result"},
                        {"step":"b","data":"","result":"b-result"}
                    ]},
                    {"issue_id":11,"project_id":21,"steps":[
                        {"step":"c","data":"c-data","result":""}
                    ]}
                ])).expect("batch fixture JSON")
            }),
        )
        .await
        .expect("bounded legacy Zephyr batch");
    assert_eq!(
        result,
        json!({"status":"created","created_cases":2,"created_steps":3})
    );
    assert_eq!(
        api.calls(),
        [
            json!({"tool":"add","issue_id":10,"project_id":20,"step":"a","data":"a-data","result":"a-result"}),
            json!({"tool":"add","issue_id":10,"project_id":20,"step":"b","data":"","result":"b-result"}),
            json!({"tool":"add","issue_id":11,"project_id":21,"step":"c","data":"c-data","result":""})
        ]
    );
}

#[tokio::test]
async fn invalid_batch_never_calls_provider_and_partial_batch_requires_reconciliation() {
    let api = Arc::new(FixtureApi::success());
    let tools = tools_for(
        api.clone() as Arc<dyn ZephyrApi>,
        &["add_test_case".to_owned()],
    )
    .await;
    let invalid = tools[0]
        .execute(
            context() as Arc<dyn ToolContext>,
            json!({"issue_id":1,"project_id":2,"steps_data":"{\"steps\":[{\"step\":\"x\",\"data\":\"\"}]}"}),
        )
        .await
        .expect_err("incomplete batch must fail before dispatch");
    assert_eq!(invalid.code, "tool.execution.invalid_input");
    assert!(api.calls().is_empty());

    let oversized = tools[0]
        .execute(
            context() as Arc<dyn ToolContext>,
            json!({
                "issue_id":1,
                "project_id":2,
                "steps_data":"x".repeat(64 * 1_024 + 1)
            }),
        )
        .await
        .expect_err("a JSON-string payload above the shared limit must fail before dispatch");
    assert_eq!(oversized.code, "tool.arguments.resource_exhausted");
    assert!(api.calls().is_empty());

    let partial_api = Arc::new(FixtureApi::new([
        Ok(json!({"created":true})),
        Err(ZephyrClientError::fixture(
            ZephyrClientErrorCode::InvalidInput,
            false,
        )),
    ]));
    let partial_tools = tools_for(
        partial_api.clone() as Arc<dyn ZephyrApi>,
        &["add_test_case".to_owned()],
    )
    .await;
    let error = partial_tools[0]
        .execute(
            context() as Arc<dyn ToolContext>,
            json!({
                "issue_id":1,
                "project_id":2,
                "steps_data":"{\"steps\":[{\"step\":\"first\",\"data\":\"\",\"result\":\"ok\"},{\"step\":\"second\",\"data\":\"\",\"result\":\"ok\"}]}"
            }),
        )
        .await
        .expect_err("partial batch must require reconciliation");
    assert_eq!(error.code, "tool.execution.internal");
    assert!(!error.retry.should_retry);
    assert_eq!(partial_api.calls().len(), 2);
}

#[tokio::test]
async fn status_taxonomy_distinguishes_safe_reads_from_ambiguous_effects() {
    for (status, code, retryable) in [
        (
            StatusCode::BAD_REQUEST,
            ZephyrClientErrorCode::InvalidInput,
            false,
        ),
        (
            StatusCode::UNAUTHORIZED,
            ZephyrClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            ZephyrClientErrorCode::Authorization,
            false,
        ),
        (
            StatusCode::NOT_FOUND,
            ZephyrClientErrorCode::NotFound,
            false,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            ZephyrClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            ZephyrClientErrorCode::DependencyUnavailable,
            true,
        ),
    ] {
        let transport = Arc::new(FixtureTransport::new([Ok(ZephyrHttpResponse::fixture(
            status,
            Some("application/json"),
            br#"{"secret":"provider-body"}"#,
        ))]));
        let error = client_with(transport)
            .get_test_case_steps(1, 2)
            .await
            .expect_err("read status must map safely");
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
        assert!(!format!("{error:?} {error}").contains("provider-body"));
    }

    for status in [
        StatusCode::REQUEST_TIMEOUT,
        StatusCode::TOO_MANY_REQUESTS,
        StatusCode::SERVICE_UNAVAILABLE,
    ] {
        let transport = Arc::new(FixtureTransport::new([Ok(ZephyrHttpResponse::fixture(
            status,
            Some("application/json"),
            b"{}",
        ))]));
        let error = client_with(transport)
            .add_new_test_case_step(1, 2, &ZephyrStep::new("step", "", "result"))
            .await
            .expect_err("effect status must remain ambiguous");
        assert_eq!(error.code(), ZephyrClientErrorCode::UnknownOutcome);
        assert!(!error.retryable());
    }
}

#[tokio::test]
async fn accepted_but_unprojectable_effect_is_unknown_and_never_retried() {
    let oversized = vec![b'x'; 512 * 1_024 + 1];
    let transport = Arc::new(FixtureTransport::new([Ok(ZephyrHttpResponse::fixture(
        StatusCode::CREATED,
        Some("text/plain"),
        &oversized,
    ))]));
    let error = client_with(Arc::clone(&transport))
        .add_new_test_case_step(1, 2, &ZephyrStep::new("step", "", "result"))
        .await
        .expect_err("accepted oversized result is an ambiguous effect");
    assert_eq!(error.code(), ZephyrClientErrorCode::UnknownOutcome);
    assert_eq!(transport.requests().len(), 1);
}

#[test]
fn two_clients_never_cross_origin_or_basic_credentials() {
    let first = client_with(Arc::new(FixtureTransport::new([])));
    let second_settings = json!({
        "base_url":"https://second.example.test/zapi",
        "username":"second@example.test",
        "password":"second-secret",
        "selected_tools":[]
    });
    let second = ZephyrClient::with_transport(
        ZephyrToolkitConfig::parse(
            second_settings
                .as_object()
                .expect("second legacy Zephyr settings object"),
        )
        .expect("second legacy Zephyr config"),
        Arc::new(FixtureTransport::new([])),
    );
    let first_request = first
        .test_request(Method::GET, 1, 2, None)
        .expect("first legacy Zephyr request");
    let second_request = second
        .test_request(Method::GET, 3, 4, None)
        .expect("second legacy Zephyr request");
    assert_eq!(first_request.url().host_str(), Some("jira.example.test"));
    assert_eq!(second_request.url().host_str(), Some("second.example.test"));
    assert_ne!(
        header(&first_request, reqwest::header::AUTHORIZATION),
        header(&second_request, reqwest::header::AUTHORIZATION)
    );
    let rendered = format!("{first_request:?} {second_request:?}");
    assert!(!rendered.contains(PASSWORD));
    assert!(!rendered.contains("second-secret"));
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn ZephyrApi> = Arc::new(FixtureApi::success());
    super::sdk_conformance::assert_sdk_conformance("zephyr", &tools_for(api, &[]).await);
}
