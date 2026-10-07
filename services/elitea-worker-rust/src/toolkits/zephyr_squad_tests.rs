use std::collections::BTreeMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, ToolContext, Toolset};
use async_trait::async_trait;
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE, HeaderName};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::zephyr_squad::client::{
    ZephyrSquadApi, ZephyrSquadClient, ZephyrSquadClientError, ZephyrSquadClientErrorCode,
    ZephyrSquadHttpResponse, ZephyrSquadTransport,
};
use super::families::zephyr_squad::config::{ZephyrSquadConfigErrorCode, ZephyrSquadToolkitConfig};
use super::families::zephyr_squad::tools::{
    ZephyrSquadToolsetErrorCode, build_zephyr_squad_toolset, test_build_with_api,
};
use super::policy::ToolAdmissionPolicy;

const ZAPI_ACCESS_KEY: HeaderName = HeaderName::from_static("zapiaccesskey");
const FIXED_EPOCH: u64 = 1_700_000_000;

fn settings(selected_tools: &[&str]) -> Map<String, Value> {
    json!({
        "account_id":"acct",
        "access_key":"access-secret",
        "secret_key":"signing-secret",
        "selected_tools":selected_tools
    })
    .as_object()
    .cloned()
    .expect("Zephyr Squad fixture settings are an object")
}

fn config(selected_tools: &[&str]) -> ZephyrSquadToolkitConfig {
    ZephyrSquadToolkitConfig::parse(&settings(selected_tools)).expect("valid Zephyr Squad fixture")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Zephyr Squad policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("zephyr-squad-test").with_function_call_id("zephyr-squad-call"))
}

#[test]
fn inline_materialized_configuration_is_bounded_deduplicated_and_redacted() {
    let parsed = config(&["get_test_step", "get_test_step", "delete_bdd_content"]);
    assert_eq!(
        parsed.selected_tools(),
        [
            Box::<str>::from("get_test_step"),
            Box::<str>::from("delete_bdd_content")
        ]
    );

    for invalid in [
        json!({"access_key":"a","secret_key":"s"}),
        json!({"account_id":"a","secret_key":"s"}),
        json!({"account_id":"a","access_key":"k"}),
        json!({"account_id":"","access_key":"k","secret_key":"s"}),
        json!({"account_id":"a","access_key":7,"secret_key":"s"}),
    ] {
        let Err(error) = ZephyrSquadToolkitConfig::parse(
            invalid
                .as_object()
                .expect("invalid configuration fixture is an object"),
        ) else {
            panic!("invalid inline credentials must fail");
        };
        assert_eq!(
            error.code(),
            ZephyrSquadConfigErrorCode::InvalidConfiguration
        );
    }

    let mut oversized = settings(&[]);
    oversized.insert(
        "secret_key".to_owned(),
        Value::String("s".repeat(16 * 1_024 + 1)),
    );
    let Err(error) = ZephyrSquadToolkitConfig::parse(&oversized) else {
        panic!("oversized secret must fail");
    };
    assert_eq!(error.code(), ZephyrSquadConfigErrorCode::ResourceExhausted);
    let rendered = format!("{error:?}");
    assert!(!rendered.contains("signing-secret"));
    assert!(!rendered.contains("access-secret"));
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct CapturedRequest {
    method: Method,
    url: String,
    body: Option<Value>,
    authorization: String,
    access_key: String,
    sensitive_header_count: u8,
    content_type: Option<String>,
    kind: CapturedRequestKind,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum CapturedRequestKind {
    Read,
    Effect,
}

type Handler = dyn Fn(&Request, bool, usize) -> Result<ZephyrSquadHttpResponse, ZephyrSquadClientError>
    + Send
    + Sync;

struct FixtureTransport {
    requests: Mutex<Vec<CapturedRequest>>,
    handler: Box<Handler>,
}

impl FixtureTransport {
    fn success() -> Self {
        Self::new(|_, _, _| {
            Ok(ZephyrSquadHttpResponse::fixture(
                StatusCode::OK,
                json!({"provider":"ok"}),
            ))
        })
    }

    fn new(
        handler: impl Fn(
            &Request,
            bool,
            usize,
        ) -> Result<ZephyrSquadHttpResponse, ZephyrSquadClientError>
        + Send
        + Sync
        + 'static,
    ) -> Self {
        Self {
            requests: Mutex::new(Vec::new()),
            handler: Box::new(handler),
        }
    }

    fn requests(&self) -> Vec<CapturedRequest> {
        self.requests
            .lock()
            .expect("Zephyr Squad request fixture lock")
            .clone()
    }
}

#[async_trait]
impl ZephyrSquadTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<ZephyrSquadHttpResponse, ZephyrSquadClientError> {
        let body = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .and_then(|bytes| serde_json::from_slice(bytes).ok());
        let authorization = request
            .headers()
            .get(AUTHORIZATION)
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default()
            .to_owned();
        let access_key = request
            .headers()
            .get(&ZAPI_ACCESS_KEY)
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default()
            .to_owned();
        let captured = CapturedRequest {
            method: request.method().clone(),
            url: request.url().as_str().to_owned(),
            body,
            authorization,
            access_key,
            sensitive_header_count: u8::from(
                request
                    .headers()
                    .get(AUTHORIZATION)
                    .is_some_and(reqwest::header::HeaderValue::is_sensitive),
            ) + u8::from(
                request
                    .headers()
                    .get(&ZAPI_ACCESS_KEY)
                    .is_some_and(reqwest::header::HeaderValue::is_sensitive),
            ),
            content_type: request
                .headers()
                .get(CONTENT_TYPE)
                .and_then(|value| value.to_str().ok())
                .map(ToOwned::to_owned),
            kind: if effect {
                CapturedRequestKind::Effect
            } else {
                CapturedRequestKind::Read
            },
        };
        let mut requests = self
            .requests
            .lock()
            .expect("Zephyr Squad request fixture lock");
        let index = requests.len();
        requests.push(captured);
        drop(requests);
        (self.handler)(&request, effect, index)
    }
}

fn fixture_client(transport: Arc<FixtureTransport>) -> ZephyrSquadClient {
    ZephyrSquadClient::fixture(config(&[]), transport, FIXED_EPOCH).expect("valid fixture client")
}

#[tokio::test]
async fn jwt_and_query_route_match_the_pinned_sdk_contract() {
    let transport = Arc::new(FixtureTransport::success());
    let client = fixture_client(Arc::clone(&transport));
    let result = client
        .get_test_step(100, "step-1", 200)
        .await
        .expect("test-step read");
    assert_eq!(result, json!({"provider":"ok"}));
    client
        .get_all_test_step_statuses()
        .await
        .expect("status catalog read");
    let requests = transport.requests();
    let request = &requests[0];
    assert_eq!(request.method, Method::GET);
    assert_eq!(
        request.url,
        "https://prod-api.zephyr4jiracloud.com/connect/public/rest/api/1.0/teststep/100/step-1?projectId=200"
    );
    assert_eq!(request.access_key, "access-secret");
    assert_eq!(request.sensitive_header_count, 2);
    assert_eq!(request.kind, CapturedRequestKind::Read);
    assert_eq!(
        request.authorization,
        "JWT eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjE3MDAwMDAzMDAsImlhdCI6MTcwMDAwMDAwMCwiaXNzIjoiYWNjZXNzLXNlY3JldCIsInFzaCI6IjFiMTkxYzg1NDI1MTYxMWIzOWM4NjQ0MzMwZWQyMDA4YmRlN2UwN2IyMmJmODNjNWI5NzJmN2EyZjNmMGUzMjEiLCJzdWIiOiJhY2N0In0.pbbeoWRiC6OSSZlKmcbmTKYqMeqjdSKmzZ76gDrrvrI"
    );
    assert_eq!(
        requests[1].authorization,
        "JWT eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjE3MDAwMDAzMDAsImlhdCI6MTcwMDAwMDAwMCwiaXNzIjoiYWNjZXNzLXNlY3JldCIsInFzaCI6IjU3YzE2YzAwYWIzOGVkZjQ0MjE0NmRhNTM2N2U0MWFhNjUwNjg4ZjlhMmVlMTc3ZjYzOWUxMThlM2U0MmY5NWIiLCJzdWIiOiJhY2N0In0.xhHy7SeMuwrjVP5zfHhQTKnKGZYktftzshmrt3Ur6Kk"
    );
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn all_fifteen_methods_use_exact_routes_versions_queries_and_bodies() {
    let transport = Arc::new(FixtureTransport::success());
    let client = fixture_client(Arc::clone(&transport));
    let object = json!({"step":"Do it","id":"step-1","projectId":200,"issueId":100,"versionId":-1});

    client
        .update_test_step(100, "step-1", 200, &object)
        .await
        .expect("update step");
    client
        .delete_test_step(100, "step-1", 200)
        .await
        .expect("delete step");
    client
        .create_new_test_step(100, 200, &object)
        .await
        .expect("create step");
    client
        .get_all_test_steps(100, 200)
        .await
        .expect("all steps");
    client
        .get_all_test_step_statuses()
        .await
        .expect("step statuses");
    client.get_bdd_content(100).await.expect("BDD read");
    client
        .update_bdd_content(100, "Feature: Login\nScenario: success")
        .await
        .expect("BDD update");
    client.delete_bdd_content(100).await.expect("BDD delete");
    client
        .create_new_cycle(&object)
        .await
        .expect("cycle create");
    client.create_folder(&object).await.expect("folder create");
    client
        .add_test_to_cycle("cycle-1", &object)
        .await
        .expect("cycle add");
    client
        .add_test_to_folder("folder-1", &object)
        .await
        .expect("folder add");
    client
        .create_execution(&object)
        .await
        .expect("execution create");
    client
        .get_execution("execution-1", 100, 200)
        .await
        .expect("execution read");

    let requests = transport.requests();
    assert_eq!(requests.len(), 14);
    let expected = [
        (
            Method::PUT,
            "/connect/public/rest/api/1.0/teststep/100/step-1?projectId=200",
            true,
        ),
        (
            Method::DELETE,
            "/connect/public/rest/api/1.0/teststep/100/step-1?projectId=200",
            true,
        ),
        (
            Method::POST,
            "/connect/public/rest/api/1.0/teststep/100?projectId=200",
            true,
        ),
        (
            Method::GET,
            "/connect/public/rest/api/2.0/teststep/100?projectId=200",
            false,
        ),
        (
            Method::GET,
            "/connect/public/rest/api/1.0/teststep/statuses",
            false,
        ),
        (
            Method::GET,
            "/connect/public/rest/api/1.0/integration/bddcontent/100",
            false,
        ),
        (
            Method::POST,
            "/connect/public/rest/api/1.0/integration/bddcontent/100",
            true,
        ),
        (
            Method::DELETE,
            "/connect/public/rest/api/1.0/integration/bddcontent/100",
            true,
        ),
        (Method::POST, "/connect/public/rest/api/1.0/cycle", true),
        (Method::POST, "/connect/public/rest/api/1.0/folder", true),
        (
            Method::POST,
            "/connect/public/rest/api/1.0/executions/add/cycle/cycle-1",
            true,
        ),
        (
            Method::POST,
            "/connect/public/rest/api/1.0/executions/add/folder/folder-1",
            true,
        ),
        (Method::POST, "/connect/public/rest/api/1.0/execution", true),
        (
            Method::GET,
            "/connect/public/rest/api/1.0/execution/execution-1?issueId=100&projectId=200",
            false,
        ),
    ];
    for (request, (method, path_and_query, effect)) in requests.iter().zip(expected) {
        assert_eq!(request.method, method);
        assert!(request.url.ends_with(path_and_query));
        assert_eq!(
            request.kind,
            if effect {
                CapturedRequestKind::Effect
            } else {
                CapturedRequestKind::Read
            }
        );
    }
    assert_eq!(
        requests[6].body,
        Some(json!({"content":"Feature: Login\nScenario: success"}))
    );
    assert_eq!(requests[7].body, Some(json!([])));
    assert_eq!(
        requests[0].content_type.as_deref(),
        Some("application/json")
    );
    assert_eq!(requests[1].content_type, None);
}

#[tokio::test]
async fn status_failures_are_typed_redacted_and_effect_retry_is_suppressed() {
    for (status, effect, code, retryable) in [
        (
            StatusCode::UNAUTHORIZED,
            false,
            ZephyrSquadClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            false,
            ZephyrSquadClientErrorCode::Authorization,
            false,
        ),
        (
            StatusCode::NOT_FOUND,
            false,
            ZephyrSquadClientErrorCode::NotFound,
            false,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            false,
            ZephyrSquadClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::REQUEST_TIMEOUT,
            false,
            ZephyrSquadClientErrorCode::Timeout,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            false,
            ZephyrSquadClientErrorCode::DependencyUnavailable,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            true,
            ZephyrSquadClientErrorCode::UnknownOutcome,
            false,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            true,
            ZephyrSquadClientErrorCode::UnknownOutcome,
            false,
        ),
        (
            StatusCode::REQUEST_TIMEOUT,
            true,
            ZephyrSquadClientErrorCode::UnknownOutcome,
            false,
        ),
    ] {
        let transport = Arc::new(FixtureTransport::new(move |_, _, _| {
            Ok(ZephyrSquadHttpResponse::fixture(
                status,
                Value::String("provider-body-secret".to_owned()),
            ))
        }));
        let client = fixture_client(transport);
        let error = if effect {
            client
                .delete_bdd_content(100)
                .await
                .expect_err("effect status must fail")
        } else {
            client
                .get_bdd_content(100)
                .await
                .expect_err("read status must fail")
        };
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
        let rendered = format!("{error:?} {error}");
        assert!(!rendered.contains("provider-body-secret"));
        assert!(!rendered.contains("signing-secret"));
        assert!(!rendered.contains("access-secret"));
    }

    let unknown =
        ZephyrSquadClientError::fixture(ZephyrSquadClientErrorCode::UnknownOutcome, false);
    assert!(!unknown.retryable());
    assert!(!format!("{unknown:?} {unknown}").contains("provider-body-secret"));
}

struct CountingApi {
    calls: AtomicUsize,
}

impl CountingApi {
    const fn new() -> Self {
        Self {
            calls: AtomicUsize::new(0),
        }
    }

    fn unexpected(&self) -> Result<Value, ZephyrSquadClientError> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        Err(ZephyrSquadClientError::fixture(
            ZephyrSquadClientErrorCode::InvalidInput,
            false,
        ))
    }
}

#[async_trait]
impl ZephyrSquadApi for CountingApi {
    async fn get_test_step(
        &self,
        _: u64,
        _: &str,
        _: u64,
    ) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn update_test_step(
        &self,
        _: u64,
        _: &str,
        _: u64,
        _: &Value,
    ) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn delete_test_step(
        &self,
        _: u64,
        _: &str,
        _: u64,
    ) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn create_new_test_step(
        &self,
        _: u64,
        _: u64,
        _: &Value,
    ) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn get_all_test_steps(&self, _: u64, _: u64) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn get_all_test_step_statuses(&self) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn get_bdd_content(&self, _: u64) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn update_bdd_content(&self, _: u64, _: &str) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn delete_bdd_content(&self, _: u64) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn create_new_cycle(&self, _: &Value) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn create_folder(&self, _: &Value) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn add_test_to_cycle(&self, _: &str, _: &Value) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn add_test_to_folder(
        &self,
        _: &str,
        _: &Value,
    ) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn create_execution(&self, _: &Value) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }

    async fn get_execution(
        &self,
        _: &str,
        _: u64,
        _: u64,
    ) -> Result<Value, ZephyrSquadClientError> {
        self.unexpected()
    }
}

#[tokio::test]
async fn catalog_preserves_all_tools_order_groups_and_selection_quality_metadata() {
    let toolset = build_zephyr_squad_toolset("jira-tests", config(&[]), &policy(&[]))
        .expect("complete Zephyr Squad toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Zephyr Squad tools");
    let expected = [
        ("get_test_step", true),
        ("update_test_step", false),
        ("delete_test_step", false),
        ("create_new_test_step", false),
        ("get_all_test_steps", true),
        ("get_all_test_step_statuses", true),
        ("get_bdd_content", true),
        ("update_bdd_content", false),
        ("delete_bdd_content", false),
        ("create_new_cycle", false),
        ("create_folder", false),
        ("add_test_to_cycle", false),
        ("add_test_to_folder", false),
        ("create_execution", false),
        ("get_execution", true),
    ];
    assert_eq!(tools.len(), expected.len());
    for (tool, (name, read_only)) in tools.iter().zip(expected) {
        assert_eq!(tool.name(), name);
        assert_eq!(tool.is_read_only(), read_only);
        assert_eq!(tool.is_concurrency_safe(), read_only);
        assert!(tool.description().starts_with("Toolkit: jira-tests\n"));
        assert!(tool.description().len() <= 1_000);
        assert!(
            tool.description().contains("Read")
                || tool.description().contains("Create")
                || tool.description().contains("Replace")
                || tool.description().contains("Add")
                || tool.description().contains("Append")
                || tool.description().contains("delete")
                || tool.description().contains("remove")
        );
        let schema = tool.parameters_schema().expect("tool schema");
        assert_eq!(schema["type"], "object");
        assert_eq!(schema["additionalProperties"], false);
    }
    let update = tools[1].parameters_schema().expect("update schema");
    assert_eq!(
        update["required"],
        json!(["issue_id", "step_id", "project_id", "json"])
    );
    assert!(
        update["properties"]["json"]["description"]
            .as_str()
            .is_some_and(|value| {
                value.contains("customFieldValues")
                    && value.contains("256 KiB")
                    && value.contains("JSON-encoded object")
            })
    );
    let cycle = tools[9].parameters_schema().expect("cycle schema");
    assert!(
        cycle["properties"]["json"]["description"]
            .as_str()
            .is_some_and(|value| value.contains("unscheduled") && value.contains("projectId"))
    );
}

#[tokio::test]
async fn invalid_json_path_ids_and_conditional_payloads_fail_before_provider_use() {
    let api = Arc::new(CountingApi::new());
    let api_trait: Arc<dyn ZephyrSquadApi> = api.clone();
    let selected = [
        "update_test_step".to_owned(),
        "create_new_cycle".to_owned(),
        "add_test_to_cycle".to_owned(),
    ];
    let toolset = test_build_with_api("jira-tests", &selected, &policy(&[]), &api_trait)
        .expect("selected Zephyr Squad tools");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset
        .tools(readonly)
        .await
        .expect("selected Zephyr Squad tools");
    for invalid in [
        json!({"issue_id":1,"project_id":2,"step_id":"../step","json":"{\"id\":\"x\",\"step\":\"y\"}"}),
        json!({"issue_id":1,"project_id":2,"step_id":"step","json":"[]"}),
        json!({"issue_id":1,"project_id":2,"step_id":"step","json":"{\"step\":\"missing id\"}"}),
    ] {
        assert!(tools[0].execute(context(), invalid).await.is_err());
    }
    for invalid in [
        json!({"json":"{}"}),
        json!({"json":"{\"name\":\"cycle\",\"projectId\":0}"}),
    ] {
        assert!(tools[1].execute(context(), invalid).await.is_err());
    }
    for invalid in [
        json!({"cycle_id":"cycle-1","json":"{\"method\":\"1\",\"projectId\":1,\"versionId\":-1}"}),
        json!({"cycle_id":"cycle-1","json":"{\"method\":\"2\",\"projectId\":1,\"versionId\":-1}"}),
        json!({"cycle_id":"cycle-1","json":"{\"method\":\"4\",\"projectId\":1,\"versionId\":-1}"}),
    ] {
        assert!(tools[2].execute(context(), invalid).await.is_err());
    }
    assert_eq!(api.calls.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn gherkin_bound_is_utf8_byte_exact_before_provider_dispatch() {
    let api = Arc::new(CountingApi::new());
    let api_trait: Arc<dyn ZephyrSquadApi> = api.clone();
    let toolset = test_build_with_api(
        "jira-tests",
        &["update_bdd_content".to_owned()],
        &policy(&[]),
        &api_trait,
    )
    .expect("BDD update tool");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("BDD update tool");
    assert!(
        tools[0]
            .execute(
                context(),
                json!({"issue_id":1,"new_content":"😀".repeat(8_192)}),
            )
            .await
            .is_err()
    );
    assert_eq!(api.calls.load(Ordering::SeqCst), 1);
    assert!(
        tools[0]
            .execute(
                context(),
                json!({"issue_id":1,"new_content":"😀".repeat(8_193)}),
            )
            .await
            .is_err()
    );
    assert_eq!(api.calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn subset_order_block_policy_and_unknown_selection_are_fail_closed() {
    let selected = config(&["get_execution", "get_test_step", "get_execution"]);
    let toolset = build_zephyr_squad_toolset(
        "jira-tests",
        selected,
        &policy(&[("zephyr_squad", &["get_test_step"])]),
    )
    .expect("selected and policy-filtered toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset
        .tools(readonly)
        .await
        .expect("filtered Zephyr Squad tools");
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        ["get_execution"]
    );

    let unknown = ZephyrSquadToolkitConfig::parse(&settings(&["remove_everything"]))
        .expect("bounded unknown selected name parses");
    let Err(error) = build_zephyr_squad_toolset("jira-tests", unknown, &policy(&[])) else {
        panic!("unknown selected tool must fail");
    };
    assert_eq!(
        error.code(),
        ZephyrSquadToolsetErrorCode::UnsupportedSelection
    );
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let toolset = build_zephyr_squad_toolset("gate", config(&[]), &policy(&[]))
        .expect("complete zephyr_squad toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("zephyr_squad tools");
    super::sdk_conformance::assert_sdk_conformance("zephyr_squad", &tools);
}
