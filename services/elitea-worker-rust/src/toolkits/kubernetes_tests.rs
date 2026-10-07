//! Focused compatibility and safety tests for the capability-disabled Kubernetes family.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Toolset};
use async_trait::async_trait;
use reqwest::StatusCode;
use serde_json::{Map, Value, json};

use super::families::kubernetes::client::{
    KubernetesApi, KubernetesClient, KubernetesClientError, KubernetesClientErrorCode,
    KubernetesHttpResponse, KubernetesTransport, validate_suburl,
};
use super::families::kubernetes::config::{KubernetesConfigErrorCode, KubernetesToolkitConfig};
use super::families::kubernetes::tools::{
    KubernetesToolsetErrorCode, test_build_with_api, test_catalog, test_parse_object,
};
use super::policy::ToolAdmissionPolicy;

const CLUSTER_URL: &str = "https://cluster.example:6443";
const TOKEN: &str = "claim-only-kubernetes-token";

fn settings(selected_tools: &Value) -> Map<String, Value> {
    json!({
        "url": CLUSTER_URL,
        "token": TOKEN,
        "selected_tools": selected_tools
    })
    .as_object()
    .cloned()
    .expect("Kubernetes settings fixture is an object")
}

fn config() -> KubernetesToolkitConfig {
    KubernetesToolkitConfig::parse(&settings(&json!([]))).expect("valid Kubernetes fixture config")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Kubernetes fixture policy"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("kubernetes-test")
            .with_session_id("session-1")
            .with_function_call_id("kubernetes-call-7"),
    )
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct RecordedRequest {
    method: String,
    url: String,
    authorization: Option<String>,
    content_type: Option<String>,
    body: Vec<u8>,
    effect: bool,
    response_limit: usize,
}

struct FixtureTransport {
    responses: Mutex<VecDeque<Result<KubernetesHttpResponse, KubernetesClientError>>>,
    requests: Mutex<Vec<RecordedRequest>>,
}

impl FixtureTransport {
    fn new(
        responses: impl IntoIterator<Item = Result<KubernetesHttpResponse, KubernetesClientError>>,
    ) -> Self {
        Self {
            responses: Mutex::new(responses.into_iter().collect()),
            requests: Mutex::new(Vec::new()),
        }
    }

    fn requests(&self) -> Vec<RecordedRequest> {
        self.requests
            .lock()
            .expect("Kubernetes request fixture lock")
            .clone()
    }
}

#[async_trait]
impl KubernetesTransport for FixtureTransport {
    async fn execute(
        &self,
        request: reqwest::Request,
        effect: bool,
        response_limit: usize,
    ) -> Result<KubernetesHttpResponse, KubernetesClientError> {
        self.requests
            .lock()
            .expect("Kubernetes request fixture lock")
            .push(RecordedRequest {
                method: request.method().to_string(),
                url: request.url().to_string(),
                authorization: request
                    .headers()
                    .get(reqwest::header::AUTHORIZATION)
                    .and_then(|value| value.to_str().ok())
                    .map(str::to_owned),
                content_type: request
                    .headers()
                    .get(reqwest::header::CONTENT_TYPE)
                    .and_then(|value| value.to_str().ok())
                    .map(str::to_owned),
                body: request
                    .body()
                    .and_then(reqwest::Body::as_bytes)
                    .map_or_else(Vec::new, <[u8]>::to_vec),
                effect,
                response_limit,
            });
        self.responses
            .lock()
            .expect("Kubernetes response fixture lock")
            .pop_front()
            .unwrap_or_else(|| {
                Err(KubernetesClientError::fixture(
                    KubernetesClientErrorCode::InvalidResponse,
                    false,
                ))
            })
    }
}

struct FixtureApi {
    calls: Mutex<Vec<Value>>,
    health: Value,
}

impl FixtureApi {
    fn new() -> Self {
        Self {
            calls: Mutex::new(Vec::new()),
            health: json!([true, ""]),
        }
    }

    fn calls(&self) -> Vec<Value> {
        self.calls
            .lock()
            .expect("Kubernetes API fixture lock")
            .clone()
    }
}

#[async_trait]
impl KubernetesApi for FixtureApi {
    async fn execute(
        &self,
        method: &str,
        suburl: &str,
        body: Option<&Map<String, Value>>,
        headers: &Map<String, Value>,
    ) -> Result<Value, KubernetesClientError> {
        self.calls
            .lock()
            .expect("Kubernetes API fixture lock")
            .push(json!({
                "method": method,
                "suburl": suburl,
                "body": body,
                "headers": headers
            }));
        Ok(Value::String("fixture-result".to_owned()))
    }

    async fn healthcheck(&self) -> Value {
        self.health.clone()
    }
}

fn client_with(transport: Arc<FixtureTransport>) -> KubernetesClient {
    let transport_trait: Arc<dyn KubernetesTransport> = transport;
    KubernetesClient::with_transport(config(), transport_trait)
}

#[test]
fn configuration_is_exact_claim_scoped_and_redacted() {
    let client = KubernetesClient::with_transport(config(), Arc::new(FixtureTransport::new([])));
    let request = client
        .test_request("GET", "/version", None, &Map::new())
        .expect("bounded Kubernetes request fixture");
    assert_eq!(
        request.url().as_str(),
        "https://cluster.example:6443/version"
    );
    assert_eq!(
        request
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok()),
        Some("Bearer claim-only-kubernetes-token")
    );

    for patch in [
        json!({"url":""}),
        json!({"url":"http://cluster.example:6443"}),
        json!({"url":"https://user@cluster.example:6443"}),
        json!({"url":"https://cluster.example:6443/base"}),
        json!({"url":"https://cluster.example:6443?query=1"}),
        json!({"token":null}),
        json!({"token":""}),
    ] {
        let mut values = settings(&json!([]));
        values.extend(patch.as_object().expect("settings patch").clone());
        let Err(error) = KubernetesToolkitConfig::parse(&values) else {
            panic!("invalid Kubernetes configuration must fail");
        };
        assert_eq!(
            error.code(),
            KubernetesConfigErrorCode::InvalidConfiguration
        );
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains(TOKEN));
        assert!(!diagnostic.contains("cluster.example"));
    }
}

#[tokio::test]
async fn catalog_descriptions_and_schemas_are_truthful() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn KubernetesApi> = api;
    let toolkit_name = "集群".repeat(100);
    let toolset = test_build_with_api(&toolkit_name, CLUSTER_URL, &[], &policy(&[]), &api_trait)
        .expect("complete Kubernetes fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Kubernetes tools");
    assert_eq!(
        test_catalog(),
        [
            ("execute_kubernetes", "execute"),
            ("kubernetes_integration_healthcheck", "read")
        ]
    );
    assert_eq!(tools.len(), 2);
    assert_eq!(tools[0].name(), "execute_kubernetes");
    assert!(!tools[0].is_read_only());
    assert!(!tools[0].is_concurrency_safe());
    assert_eq!(tools[1].name(), "kubernetes_integration_healthcheck");
    assert!(tools[1].is_read_only());
    assert!(tools[1].is_concurrency_safe());
    for tool in &tools {
        assert!(tool.description().len() <= 1_000);
        assert!(!tool.description().contains(TOKEN));
        assert!(!tool.description().contains("cluster.example"));
    }
    for cue in [
        "configured cluster",
        "/api/v1/namespaces/default/pods?limit=50",
        "create, update, patch, delete",
        "cannot replace Authorization",
        "512 KiB",
        "202",
        "without automatic retry",
        "independently require approval",
        "reconcile cluster state",
    ] {
        assert!(tools[0].description().contains(cue), "missing cue: {cue}");
    }
    for cue in ["read-only", "GET /version", "does not prove authorization"] {
        assert!(tools[1].description().contains(cue), "missing cue: {cue}");
    }
    let schema = tools[0]
        .parameters_schema()
        .expect("Kubernetes execute schema");
    assert_eq!(schema["required"], json!(["method", "suburl"]));
    assert_eq!(schema["additionalProperties"], false);
    assert_eq!(schema["properties"]["method"]["maxLength"], 32);
    assert_eq!(schema["properties"]["suburl"]["maxLength"], 2048);
    assert_eq!(schema["properties"]["body"]["default"], Value::Null);
    assert_eq!(schema["properties"]["headers"]["default"], Value::Null);
    assert_eq!(
        schema["properties"]["headers"]["anyOf"][0]["maxLength"],
        16_384
    );
    assert_eq!(
        schema["properties"]["headers"]["anyOf"][1]["additionalProperties"]["maxLength"],
        2_048
    );
    for cue in ["240 KiB", "object", "null"] {
        assert!(
            schema["properties"]["body"]["description"]
                .as_str()
                .expect("Kubernetes body prose")
                .contains(cue)
        );
    }
}

#[tokio::test]
async fn selection_and_policy_preserve_complete_source_order() {
    let api: Arc<dyn KubernetesApi> = Arc::new(FixtureApi::new());
    let subset = test_build_with_api(
        "cluster",
        CLUSTER_URL,
        &["kubernetes_integration_healthcheck".to_owned()],
        &policy(&[]),
        &api,
    )
    .expect("Kubernetes subset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert_eq!(
        subset
            .tools(readonly)
            .await
            .expect("Kubernetes subset tools")[0]
            .name(),
        "kubernetes_integration_healthcheck"
    );
    let blocked = test_build_with_api(
        "cluster-prod",
        CLUSTER_URL,
        &[],
        &policy(&[("kubernetes", &["execute_kubernetes"])]),
        &api,
    )
    .expect("blocked Kubernetes definition is valid");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let blocked_tools = blocked
        .tools(readonly)
        .await
        .expect("blocked Kubernetes tools");
    assert_eq!(blocked_tools.len(), 1);
    assert_eq!(
        blocked_tools[0].name(),
        "kubernetes_integration_healthcheck"
    );
    let Err(error) = test_build_with_api(
        "cluster",
        CLUSTER_URL,
        &["unknown".to_owned()],
        &policy(&[]),
        &api,
    ) else {
        panic!("unknown Kubernetes selection must fail closed");
    };
    assert_eq!(
        error.code(),
        KubernetesToolsetErrorCode::UnsupportedSelection
    );
}

#[tokio::test]
async fn tool_dispatch_preserves_object_or_string_inputs_and_health_shape() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn KubernetesApi> = api.clone();
    let toolset = test_build_with_api("cluster", CLUSTER_URL, &[], &policy(&[]), &api_trait)
        .expect("Kubernetes fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Kubernetes tools");
    assert_eq!(
        tools[0]
            .execute(
                context(),
                json!({
                    "method":"patch",
                    "suburl":"/apis/apps/v1/namespaces/default/deployments/demo",
                    "body":"{\"spec\":{\"replicas\":2}}",
                    "headers":{"Content-Type":"application/merge-patch+json"}
                }),
            )
            .await
            .expect("valid Kubernetes execution"),
        Value::String("fixture-result".to_owned())
    );
    let calls = api.calls();
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0]["method"], "patch");
    assert_eq!(calls[0]["body"]["spec"]["replicas"], 2);
    assert_eq!(
        calls[0]["headers"]["Content-Type"],
        "application/merge-patch+json"
    );
    assert_eq!(
        tools[1]
            .execute(context(), json!({}))
            .await
            .expect("Kubernetes health fixture"),
        json!([true, ""])
    );
}

#[tokio::test]
async fn malformed_arguments_fail_before_api_dispatch() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn KubernetesApi> = api.clone();
    let toolset = test_build_with_api("cluster", CLUSTER_URL, &[], &policy(&[]), &api_trait)
        .expect("Kubernetes fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("Kubernetes tools")
        .remove(0);
    for arguments in [
        json!({"method":"GET bad","suburl":"/version"}),
        json!({"method":"GET","suburl":"https://evil.example/version"}),
        json!({"method":"GET","suburl":"//evil.example/version"}),
        json!({"method":"GET","suburl":"/api/%2e%2e/version"}),
        json!({"method":"GET","suburl":"/api/a%2Fb"}),
        json!({"method":"GET","suburl":"/api/%zz"}),
        json!({"method":"POST","suburl":"/api/v1/pods","body":"[]"}),
        json!({"method":"POST","suburl":"/api/v1/pods","headers":{"X-Test":1}}),
        json!({"method":"GET","suburl":"/version","extra":true}),
    ] {
        assert!(tool.execute(context(), arguments).await.is_err());
    }
    assert!(api.calls().is_empty());
}

#[test]
fn suburl_authority_is_exact_origin_and_traversal_safe() {
    let valid = "/api/v1/namespaces/default/pods?limit=50&labelSelector=app%3Ddemo";
    assert_eq!(
        validate_suburl(CLUSTER_URL, valid)
            .expect("valid Kubernetes suburl")
            .as_str(),
        format!("{CLUSTER_URL}{valid}")
    );
    for invalid in [
        "",
        "version",
        "//evil.example/version",
        "/api//v1",
        "/api/../version",
        "/api/%2E%2E/version",
        "/api/%252e%252e/version",
        "/api/a%2fb",
        "/api/a\\b",
        "/api/v1#fragment",
        "/api/%",
    ] {
        assert!(
            validate_suburl(CLUSTER_URL, invalid).is_err(),
            "invalid Kubernetes suburl must fail: {invalid}"
        );
    }
}

#[test]
fn request_builder_preserves_body_headers_query_and_fixed_credential() {
    let client = KubernetesClient::with_transport(config(), Arc::new(FixtureTransport::new([])));
    let body = json!({"spec":{"replicas":3}})
        .as_object()
        .cloned()
        .expect("Kubernetes body fixture");
    let headers = json!({
        "Content-Type":"application/merge-patch+json",
        "If-Match":"resource-version"
    })
    .as_object()
    .cloned()
    .expect("Kubernetes headers fixture");
    let request = client
        .test_request(
            "PATCH",
            "/apis/apps/v1/namespaces/default/deployments/demo?dryRun=All",
            Some(&body),
            &headers,
        )
        .expect("bounded Kubernetes request");
    assert_eq!(request.method(), reqwest::Method::PATCH);
    assert_eq!(
        request.url().as_str(),
        "https://cluster.example:6443/apis/apps/v1/namespaces/default/deployments/demo?dryRun=All"
    );
    assert_eq!(
        request
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok()),
        Some("Bearer claim-only-kubernetes-token")
    );
    assert_eq!(
        request
            .headers()
            .get(reqwest::header::CONTENT_TYPE)
            .and_then(|value| value.to_str().ok()),
        Some("application/merge-patch+json")
    );
    assert_eq!(
        request.body().and_then(reqwest::Body::as_bytes),
        Some(br#"{"spec":{"replicas":3}}"#.as_slice())
    );
    for forbidden in [
        "Authorization",
        "Host",
        "Content-Length",
        "Transfer-Encoding",
        "Proxy-Authorization",
    ] {
        let headers = json!({forbidden:"override"})
            .as_object()
            .cloned()
            .expect("forbidden header fixture");
        assert!(
            client
                .test_request("GET", "/version", None, &headers)
                .is_err()
        );
    }
}

#[tokio::test]
async fn effect_success_and_ambiguity_are_one_attempt_and_post_accept_safe() {
    let transport = Arc::new(FixtureTransport::new([
        Ok(KubernetesHttpResponse::fixture(
            StatusCode::ACCEPTED,
            r#"{"kind":"Status","status":"Success"}"#,
        )),
        Ok(KubernetesHttpResponse::fixture(
            StatusCode::INTERNAL_SERVER_ERROR,
            "provider body must not leak",
        )),
        Ok(KubernetesHttpResponse::fixture(
            StatusCode::TOO_MANY_REQUESTS,
            "provider body must not leak",
        )),
        Ok(KubernetesHttpResponse::fixture(
            StatusCode::REQUEST_TIMEOUT,
            "provider body must not leak",
        )),
        Ok(KubernetesHttpResponse::fixture(StatusCode::OK, vec![0xff])),
        Ok(KubernetesHttpResponse::fixture(
            StatusCode::OK,
            vec![b'x'; 512 * 1_024],
        )),
    ]));
    let client = client_with(transport.clone());
    assert_eq!(
        client
            .execute("POST", "/api/v1/namespaces", None, &Map::new())
            .await
            .expect("accepted Kubernetes effect"),
        Value::String(r#"{"kind":"Status","status":"Success"}"#.to_owned())
    );
    let error = client
        .execute("DELETE", "/api/v1/namespaces/demo", None, &Map::new())
        .await
        .expect_err("effect 5xx is ambiguous");
    assert_eq!(error.code(), KubernetesClientErrorCode::UnknownOutcome);
    assert!(!error.retryable());
    for method in ["POST", "PUT"] {
        let error = client
            .execute(method, "/api/v1/namespaces/demo", None, &Map::new())
            .await
            .expect_err("effect transient status is ambiguous");
        assert_eq!(error.code(), KubernetesClientErrorCode::UnknownOutcome);
        assert!(!error.retryable());
    }
    let error = client
        .execute("PATCH", "/api/v1/namespaces/demo", None, &Map::new())
        .await
        .expect_err("accepted invalid effect response is ambiguous");
    assert_eq!(error.code(), KubernetesClientErrorCode::UnknownOutcome);
    let error = client
        .execute("DELETE", "/api/v1/namespaces/demo", None, &Map::new())
        .await
        .expect_err("accepted oversized effect response is ambiguous");
    assert_eq!(error.code(), KubernetesClientErrorCode::UnknownOutcome);
    assert_eq!(transport.requests().len(), 6);
    assert!(transport.requests().iter().all(|request| request.effect));
}

#[tokio::test]
async fn reads_map_statuses_without_leaking_provider_or_authority_data() {
    let cases = [
        (
            StatusCode::UNAUTHORIZED,
            KubernetesClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            KubernetesClientErrorCode::Authorization,
            false,
        ),
        (
            StatusCode::NOT_FOUND,
            KubernetesClientErrorCode::NotFound,
            false,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            KubernetesClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::REQUEST_TIMEOUT,
            KubernetesClientErrorCode::Timeout,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            KubernetesClientErrorCode::DependencyUnavailable,
            true,
        ),
    ];
    for (status, code, retryable) in cases {
        let transport = Arc::new(FixtureTransport::new([Ok(
            KubernetesHttpResponse::fixture(status, "provider secret body"),
        )]));
        let error = client_with(transport)
            .execute("GET", "/api/v1/nodes", None, &Map::new())
            .await
            .expect_err("Kubernetes read status must fail");
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains("provider secret body"));
        assert!(!diagnostic.contains(TOKEN));
        assert!(!diagnostic.contains("cluster.example"));
    }

    let oversized = Arc::new(FixtureTransport::new([Ok(
        KubernetesHttpResponse::fixture(StatusCode::OK, vec![b'x'; 512 * 1_024]),
    )]));
    let error = client_with(oversized)
        .execute("GET", "/api/v1/nodes", None, &Map::new())
        .await
        .expect_err("serialized read output must stay bounded");
    assert_eq!(error.code(), KubernetesClientErrorCode::ResourceExhausted);
    assert!(!error.retryable());
}

#[tokio::test]
async fn healthcheck_uses_exact_version_route_and_valid_json() {
    let transport = Arc::new(FixtureTransport::new([Ok(
        KubernetesHttpResponse::fixture(StatusCode::OK, r#"{"gitVersion":"v1.31.0"}"#),
    )]));
    assert_eq!(
        client_with(transport.clone()).healthcheck().await,
        json!([true, ""])
    );
    let requests = transport.requests();
    assert_eq!(requests.len(), 1);
    assert_eq!(requests[0].method, "GET");
    assert_eq!(requests[0].url, "https://cluster.example:6443/version");
    assert!(!requests[0].effect);

    let invalid = Arc::new(FixtureTransport::new([Ok(
        KubernetesHttpResponse::fixture(StatusCode::OK, "not json"),
    )]));
    assert_eq!(
        client_with(invalid).healthcheck().await,
        json!([false, "invalid cluster response"])
    );
}

#[test]
fn object_and_argument_bounds_are_enforced_before_dispatch() {
    assert!(test_parse_object(Some(&json!({"ok":true})), 32).is_ok());
    assert!(test_parse_object(Some(&json!([])), 32).is_err());
    assert!(test_parse_object(Some(&json!("[]")), 32).is_err());
    assert!(test_parse_object(Some(&json!({"x":"a".repeat(128)})), 32).is_err());
    assert!(
        test_parse_object(
            Some(&json!(format!("{{\"x\":\"{}\"}}", "a".repeat(65_537)))),
            240 * 1_024
        )
        .is_err()
    );
}

#[test]
fn two_clients_never_cross_cluster_credentials_or_authority() {
    let first = KubernetesClient::with_transport(config(), Arc::new(FixtureTransport::new([])));
    let second_settings = json!({
        "url":"https://second.example",
        "token":"second-token",
        "selected_tools":[]
    })
    .as_object()
    .cloned()
    .expect("second Kubernetes settings fixture");
    let second = KubernetesClient::with_transport(
        KubernetesToolkitConfig::parse(&second_settings).expect("second Kubernetes config"),
        Arc::new(FixtureTransport::new([])),
    );
    let first_request = first
        .test_request("GET", "/version", None, &Map::new())
        .expect("first Kubernetes request");
    let second_request = second
        .test_request("GET", "/version", None, &Map::new())
        .expect("second Kubernetes request");
    assert_eq!(first_request.url().host_str(), Some("cluster.example"));
    assert_eq!(second_request.url().host_str(), Some("second.example"));
    assert_eq!(
        first_request
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok()),
        Some("Bearer claim-only-kubernetes-token")
    );
    assert_eq!(
        second_request
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok()),
        Some("Bearer second-token")
    );
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn KubernetesApi> = Arc::new(FixtureApi::new());
    let toolset = test_build_with_api("gate", CLUSTER_URL, &[], &policy(&[]), &api)
        .expect("complete kubernetes toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("kubernetes tools");
    super::sdk_conformance::assert_sdk_conformance("kubernetes", &tools);
}
