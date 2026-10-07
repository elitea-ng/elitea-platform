//! Focused compatibility and safety tests for the capability-disabled Azure family.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Toolset};
use async_trait::async_trait;
use reqwest::StatusCode;
use serde_json::{Map, Value, json};

use super::families::azure::client::{
    AzureApi, AzureClient, AzureClientError, AzureClientErrorCode, AzureHttpResponse, AzureRequest,
    AzureRequestBody, AzureTransport,
};
use super::families::azure::config::{AzureConfigErrorCode, AzureToolkitConfig};
use super::families::azure::tools::{
    AzureToolsetErrorCode, test_build_with_api, test_catalog, test_parse_optional_args,
    test_validate_arm_url,
};
use super::policy::ToolAdmissionPolicy;

const SUBSCRIPTION_ID: &str = "00000000-1111-2222-3333-444444444444";
const OTHER_SUBSCRIPTION_ID: &str = "99999999-1111-2222-3333-444444444444";
const CLIENT_ID: &str = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
const CLIENT_SECRET: &str = "claim-only-secret";
const ACCESS_TOKEN: &str = "fixture-access-token";

fn settings(selected_tools: &Value) -> Map<String, Value> {
    json!({
        "subscription_id": SUBSCRIPTION_ID,
        "tenant_id": "tenant.example",
        "client_id": CLIENT_ID,
        "client_secret": CLIENT_SECRET,
        "selected_tools": selected_tools
    })
    .as_object()
    .cloned()
    .expect("Azure settings fixture is an object")
}

fn config() -> AzureToolkitConfig {
    AzureToolkitConfig::parse(&settings(&json!([]))).expect("valid Azure fixture config")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Azure fixture policy"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("azure-test")
            .with_session_id("session-1")
            .with_function_call_id("azure-call-7"),
    )
}

#[derive(Clone, Debug, Eq, PartialEq)]
enum RecordedBody {
    Empty,
    Bytes(Vec<u8>),
    Multipart {
        fields: Vec<(String, String)>,
        files: Vec<(String, String, Vec<u8>, Option<String>)>,
    },
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct RecordedRequest {
    method: String,
    url: String,
    authorization: Option<String>,
    content_type: Option<String>,
    body: RecordedBody,
    effect: bool,
    response_limit: usize,
}

struct FixtureTransport {
    responses: Mutex<VecDeque<Result<AzureHttpResponse, AzureClientError>>>,
    requests: Mutex<Vec<RecordedRequest>>,
}

impl FixtureTransport {
    fn new(
        responses: impl IntoIterator<Item = Result<AzureHttpResponse, AzureClientError>>,
    ) -> Self {
        Self {
            responses: Mutex::new(responses.into_iter().collect()),
            requests: Mutex::new(Vec::new()),
        }
    }

    fn requests(&self) -> Vec<RecordedRequest> {
        self.requests
            .lock()
            .expect("Azure request fixture lock")
            .clone()
    }
}

#[async_trait]
impl AzureTransport for FixtureTransport {
    async fn execute(
        &self,
        request: AzureRequest,
        effect: bool,
        response_limit: usize,
    ) -> Result<AzureHttpResponse, AzureClientError> {
        let body = match request.body() {
            AzureRequestBody::Empty => RecordedBody::Empty,
            AzureRequestBody::Bytes(body) => RecordedBody::Bytes(body.clone()),
            AzureRequestBody::Multipart { fields, files } => RecordedBody::Multipart {
                fields: fields.clone(),
                files: files
                    .iter()
                    .map(|file| {
                        (
                            file.field.clone(),
                            file.filename.clone(),
                            file.content.clone(),
                            file.content_type.clone(),
                        )
                    })
                    .collect(),
            },
        };
        self.requests
            .lock()
            .expect("Azure request fixture lock")
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
                body,
                effect,
                response_limit,
            });
        self.responses
            .lock()
            .expect("Azure response fixture lock")
            .pop_front()
            .unwrap_or_else(|| {
                Err(AzureClientError::fixture(
                    AzureClientErrorCode::InvalidResponse,
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
        self.calls.lock().expect("Azure API fixture lock").clone()
    }
}

#[async_trait]
impl AzureApi for FixtureApi {
    async fn execute(
        &self,
        method: &str,
        url: &str,
        optional_args: &Map<String, Value>,
    ) -> Result<Value, AzureClientError> {
        self.calls
            .lock()
            .expect("Azure API fixture lock")
            .push(json!({
                "method": method,
                "url": url,
                "optional_args": optional_args
            }));
        Ok(Value::String("fixture-result".to_owned()))
    }

    async fn healthcheck(&self) -> Value {
        self.health.clone()
    }
}

fn client_with(transport: Arc<FixtureTransport>) -> AzureClient {
    let transport_trait: Arc<dyn AzureTransport> = transport;
    AzureClient::with_transport(config(), transport_trait)
}

fn token_response() -> AzureHttpResponse {
    AzureHttpResponse::fixture(
        StatusCode::OK,
        format!(r#"{{"access_token":"{ACCESS_TOKEN}","expires_in":60}}"#),
    )
}

fn arm_url(path: &str) -> String {
    format!("https://management.azure.com/subscriptions/{SUBSCRIPTION_ID}{path}")
}

#[test]
fn configuration_is_claim_scoped_bounded_and_redacted() {
    let client = AzureClient::with_transport(config(), Arc::new(FixtureTransport::new([])));
    let request = client
        .test_token_request()
        .expect("bounded token request fixture");
    assert_eq!(
        request.url().as_str(),
        "https://login.microsoftonline.com/tenant.example/oauth2/v2.0/token"
    );
    let AzureRequestBody::Bytes(body) = request.body() else {
        panic!("token request must use a bounded form body");
    };
    let body = String::from_utf8(body.clone()).expect("token fixture form is UTF-8");
    for cue in [
        "client_id=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
        "client_secret=claim-only-secret",
        "grant_type=client_credentials",
        "scope=https%3A%2F%2Fmanagement.azure.com%2F.default",
    ] {
        assert!(
            body.contains(cue),
            "missing token form cue: {cue}; body was {body}"
        );
    }
    assert!(
        request
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .is_none()
    );

    let invalid_settings = [
        json!({"subscription_id":"not-a-guid"}),
        json!({"tenant_id":"https://tenant.example"}),
        json!({"tenant_id":"tenant..example"}),
        json!({"tenant_id":"-tenant.example"}),
        json!({"client_id":"not-a-guid"}),
        json!({"client_secret":""}),
    ];
    for patch in invalid_settings {
        let mut values = settings(&json!([]));
        values.extend(patch.as_object().expect("settings patch").clone());
        let Err(error) = AzureToolkitConfig::parse(&values) else {
            panic!("invalid Azure configuration must fail");
        };
        assert_eq!(error.code(), AzureConfigErrorCode::InvalidConfiguration);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains(CLIENT_SECRET));
        assert!(!diagnostic.contains(SUBSCRIPTION_ID));
        assert!(!diagnostic.contains("tenant.example"));
    }
}

#[tokio::test]
async fn catalog_descriptions_schemas_selection_and_policy_are_truthful() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn AzureApi> = api.clone();
    let toolkit_name = "云".repeat(200);
    let toolset = test_build_with_api(
        &toolkit_name,
        SUBSCRIPTION_ID,
        &[],
        &policy(&[]),
        &api_trait,
    )
    .expect("complete Azure fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Azure tools");
    assert_eq!(
        test_catalog(),
        [
            ("execute", "execute"),
            ("azure_integration_healthcheck", "read")
        ]
    );
    assert_eq!(tools.len(), 2);
    assert_eq!(tools[0].name(), "execute");
    assert!(!tools[0].is_read_only());
    assert!(!tools[0].is_concurrency_safe());
    assert_eq!(tools[1].name(), "azure_integration_healthcheck");
    assert!(tools[1].is_read_only());
    assert!(tools[1].is_concurrency_safe());
    for tool in &tools {
        assert!(tool.description().len() <= 1_000);
        assert!(!tool.description().contains(CLIENT_SECRET));
        assert!(!tool.description().contains(SUBSCRIPTION_ID));
    }
    for cue in [
        "configured subscription",
        "api-version=2021-04-01",
        "inline multipart",
        "never reads a local path",
        "create, update, delete",
        "independently require approval",
        "without automatic retry",
        "reconcile Azure state",
        "512 KiB",
        "202",
    ] {
        assert!(tools[0].description().contains(cue), "missing cue: {cue}");
    }
    for cue in ["read-only", "[true,\"\"]", "does not prove access"] {
        assert!(tools[1].description().contains(cue), "missing cue: {cue}");
    }

    let schema = tools[0].parameters_schema().expect("Azure execute schema");
    assert_eq!(schema["required"], json!(["method", "url"]));
    assert_eq!(schema["additionalProperties"], false);
    assert_eq!(schema["properties"]["method"]["maxLength"], 32);
    assert_eq!(
        schema["properties"]["optional_args"]["default"],
        Value::Null
    );
    let option_prose = schema["properties"]["optional_args"]["description"]
        .as_str()
        .expect("optional args prose");
    for cue in [
        "headers", "params", "json", "data", "files", "240 KiB", "256 KiB",
    ] {
        assert!(option_prose.contains(cue), "missing option cue: {cue}");
    }

    let subset = test_build_with_api(
        "azure",
        SUBSCRIPTION_ID,
        &["azure_integration_healthcheck".to_owned()],
        &policy(&[]),
        &api_trait,
    )
    .expect("Azure subset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert_eq!(
        subset.tools(readonly).await.expect("Azure subset tools")[0].name(),
        "azure_integration_healthcheck"
    );

    let blocked = test_build_with_api(
        "azure-prod",
        SUBSCRIPTION_ID,
        &[],
        &policy(&[("azure", &["execute"])]),
        &api_trait,
    )
    .expect("blocked Azure definition is valid");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let blocked_tools = blocked.tools(readonly).await.expect("blocked Azure tools");
    assert_eq!(blocked_tools.len(), 1);
    assert_eq!(blocked_tools[0].name(), "azure_integration_healthcheck");

    let Err(error) = test_build_with_api(
        "azure",
        SUBSCRIPTION_ID,
        &["unknown".to_owned()],
        &policy(&[]),
        &api_trait,
    ) else {
        panic!("unknown Azure selection must fail closed");
    };
    assert_eq!(error.code(), AzureToolsetErrorCode::UnsupportedSelection);
}

#[tokio::test]
async fn tool_dispatch_preserves_object_or_string_options_and_health_shape() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn AzureApi> = api.clone();
    let toolset = test_build_with_api("azure", SUBSCRIPTION_ID, &[], &policy(&[]), &api_trait)
        .expect("Azure fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Azure tools");
    let url = arm_url("/resourcegroups/demo?api-version=2021-04-01");
    assert_eq!(
        tools[0]
            .execute(
                context(),
                json!({
                    "method":"patch",
                    "url":url,
                    "optional_args":"{\"json\":{\"tags\":{\"owner\":\"agent\"}}}"
                }),
            )
            .await
            .expect("valid Azure execution"),
        Value::String("fixture-result".to_owned())
    );
    assert_eq!(api.calls().len(), 1);
    assert_eq!(api.calls()[0]["method"], "patch");
    assert_eq!(
        api.calls()[0]["optional_args"]["json"]["tags"]["owner"],
        "agent"
    );
    assert_eq!(
        tools[1]
            .execute(context(), json!({}))
            .await
            .expect("Azure health fixture"),
        json!([true, ""])
    );
}

#[tokio::test]
async fn malformed_arguments_fail_before_api_or_network() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn AzureApi> = api.clone();
    let toolset = test_build_with_api("azure", SUBSCRIPTION_ID, &[], &policy(&[]), &api_trait)
        .expect("Azure fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("Azure tools")
        .remove(0);
    for arguments in [
        json!({"method":"GET bad","url":arm_url("?api-version=1")}),
        json!({"method":"GET","url":"https://evil.example/subscriptions/x"}),
        json!({"method":"GET","url":format!("https://management.azure.com/subscriptions/{OTHER_SUBSCRIPTION_ID}/resources?api-version=1")}),
        json!({"method":"GET","url":arm_url("/%2e%2e/resources?api-version=1")}),
        json!({"method":"GET","url":arm_url("/resources?api-version=1#fragment")}),
        json!({"method":"GET","url":arm_url("/resources?api-version=1"),"optional_args":"[]"}),
        json!({"method":"GET","url":arm_url("/resources?api-version=1"),"extra":true}),
    ] {
        assert!(tool.execute(context(), arguments).await.is_err());
    }
    assert!(api.calls().is_empty());

    let transport = Arc::new(FixtureTransport::new([]));
    let client = client_with(transport.clone());
    assert!(
        client
            .execute(
                "GET",
                &arm_url("/resources?api-version=1"),
                json!({"timeout":1})
                    .as_object()
                    .expect("invalid options fixture"),
            )
            .await
            .is_err()
    );
    assert!(transport.requests().is_empty());
}

#[test]
fn arm_authority_is_exact_subscription_scoped_and_traversal_safe() {
    let valid = arm_url("/resourcegroups/demo?api-version=2021-04-01");
    assert_eq!(
        test_validate_arm_url(&valid, SUBSCRIPTION_ID)
            .expect("valid ARM URL")
            .as_str(),
        valid
    );
    for invalid in [
        "http://management.azure.com/subscriptions/00000000-1111-2222-3333-444444444444/resources",
        "https://user@management.azure.com/subscriptions/00000000-1111-2222-3333-444444444444/resources",
        "https://management.azure.com:443/subscriptions/00000000-1111-2222-3333-444444444444/resources",
        "https://management.azure.com.evil/subscriptions/00000000-1111-2222-3333-444444444444/resources",
        "https://management.azure.com/providers/Microsoft.Resources",
        "https://management.azure.com/subscriptions/99999999-1111-2222-3333-444444444444/resources",
        "https://management.azure.com/subscriptions/00000000-1111-2222-3333-444444444444/%252e%252e/resources",
        "https://management.azure.com/subscriptions/00000000-1111-2222-3333-444444444444/a%2Fb",
        "https://management.azure.com/subscriptions/00000000-1111-2222-3333-444444444444/bad%zz",
    ] {
        assert!(
            test_validate_arm_url(invalid, SUBSCRIPTION_ID).is_err(),
            "invalid ARM authority must fail: {invalid}"
        );
    }
}

#[test]
fn request_builder_preserves_query_json_form_headers_and_inline_multipart() {
    let client = AzureClient::with_transport(config(), Arc::new(FixtureTransport::new([])));
    let request = client
        .test_arm_request(
            "POST",
            &arm_url("/providers/Microsoft.Test/widgets?api-version=2026-01-01"),
            json!({
                "headers":{"X-Correlation-ID":"safe-id"},
                "params":{"top":5,"tag":["one","two"]},
                "json":{"name":"demo"}
            })
            .as_object()
            .expect("JSON request options"),
            ACCESS_TOKEN,
        )
        .expect("bounded JSON ARM request");
    assert_eq!(request.method(), reqwest::Method::POST);
    assert!(
        request
            .url()
            .query()
            .expect("ARM query")
            .contains("api-version=2026-01-01")
    );
    assert!(request.url().query().expect("ARM query").contains("top=5"));
    assert_eq!(
        request
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .expect("Bearer header"),
        "Bearer fixture-access-token"
    );
    assert_eq!(
        request
            .headers()
            .get(reqwest::header::CONTENT_TYPE)
            .expect("JSON content type"),
        "application/json"
    );
    assert!(matches!(
        request.body(),
        AzureRequestBody::Bytes(body) if body == br#"{"name":"demo"}"#
    ));

    let request = client
        .test_arm_request(
            "PUT",
            &arm_url("/providers/Microsoft.Test/uploads/demo?api-version=2026-01-01"),
            json!({
                "data":{"metadata":"safe"},
                "files":{
                    "attachment":["report.txt","literal content","text/plain",{"X-Part":"one"}],
                    "plain":"not/a/local/path"
                }
            })
            .as_object()
            .expect("multipart options"),
            ACCESS_TOKEN,
        )
        .expect("bounded multipart ARM request");
    let AzureRequestBody::Multipart { fields, files } = request.body() else {
        panic!("files option must produce multipart body");
    };
    assert_eq!(fields, &[("metadata".to_owned(), "safe".to_owned())]);
    assert_eq!(files.len(), 2);
    assert!(files.iter().any(|file| {
        file.field == "attachment"
            && file.filename == "report.txt"
            && file.content == b"literal content"
            && file.content_type.as_deref() == Some("text/plain")
    }));
    assert!(files.iter().any(|file| {
        file.field == "plain" && file.filename == "plain" && file.content == b"not/a/local/path"
    }));
}

#[test]
fn optional_argument_shapes_and_bounds_are_strict() {
    assert_eq!(
        test_parse_optional_args(Some(&json!("{\"data\":{\"a\":1}}"))).expect("string options")["data"]
            ["a"],
        1
    );
    for invalid in [json!("[]"), json!([]), json!(7), json!("{'data':1}")] {
        assert!(test_parse_optional_args(Some(&invalid)).is_err());
    }

    let client = AzureClient::with_transport(config(), Arc::new(FixtureTransport::new([])));
    for invalid in [
        json!({"headers":{"Authorization":"secret"}}),
        json!({"headers":{"Host":"evil.example"}}),
        json!({"json":{},"data":"x"}),
        json!({"json":{},"files":{"f":"x"}}),
        json!({"files":{}}),
        json!({"files":{"f":["../name","x"]}}),
        json!({"files":{"f":"x"},"data":"raw"}),
        json!({"params":{"nested":{"x":1}}}),
    ] {
        assert!(
            client
                .test_arm_request(
                    "GET",
                    &arm_url("/resources?api-version=1"),
                    invalid.as_object().expect("invalid option fixture"),
                    ACCESS_TOKEN,
                )
                .is_err()
        );
    }
    let oversized = "x".repeat(240 * 1_024 + 1);
    assert!(
        client
            .test_arm_request(
                "POST",
                &arm_url("/resources?api-version=1"),
                json!({"files":{"f":oversized}})
                    .as_object()
                    .expect("oversized file fixture"),
                ACCESS_TOKEN,
            )
            .is_err()
    );
    let exact = "x".repeat(240 * 1_024);
    assert!(
        client
            .test_arm_request(
                "POST",
                &arm_url("/resources?api-version=1"),
                json!({"files":{"f":exact}})
                    .as_object()
                    .expect("exact file fixture"),
                ACCESS_TOKEN,
            )
            .is_ok()
    );
}

#[tokio::test]
async fn exact_token_then_arm_wire_and_success_projection_are_bounded() {
    let transport = Arc::new(FixtureTransport::new([
        Ok(token_response()),
        Ok(AzureHttpResponse::fixture(
            StatusCode::OK,
            br#"{"value":[]}"#,
        )),
    ]));
    let client = client_with(transport.clone());
    let result = client
        .execute(
            "GET",
            &arm_url("/resourcegroups?api-version=2021-04-01"),
            &Map::new(),
        )
        .await
        .expect("successful Azure read");
    assert_eq!(result, Value::String(r#"{"value":[]}"#.to_owned()));
    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[0].method, "POST");
    assert!(requests[0].url.contains("/oauth2/v2.0/token"));
    assert_eq!(requests[0].authorization, None);
    assert!(!requests[0].effect);
    assert_eq!(requests[1].method, "GET");
    assert_eq!(
        requests[1].authorization.as_deref(),
        Some("Bearer fixture-access-token")
    );
    assert!(!requests[1].effect);
    assert_eq!(requests[1].response_limit, 512 * 1_024);
}

#[tokio::test]
async fn token_failures_are_typed_and_never_dispatch_arm() {
    let cases = [
        (
            AzureHttpResponse::fixture(StatusCode::UNAUTHORIZED, b"private"),
            AzureClientErrorCode::Authentication,
            false,
        ),
        (
            AzureHttpResponse::fixture(StatusCode::TOO_MANY_REQUESTS, b"private"),
            AzureClientErrorCode::RateLimited,
            true,
        ),
        (
            AzureHttpResponse::fixture(StatusCode::SERVICE_UNAVAILABLE, b"private"),
            AzureClientErrorCode::DependencyUnavailable,
            true,
        ),
        (
            AzureHttpResponse::fixture(StatusCode::OK, br#"{"expires_in":60}"#),
            AzureClientErrorCode::InvalidResponse,
            false,
        ),
    ];
    for (response, expected, retryable) in cases {
        let transport = Arc::new(FixtureTransport::new([Ok(response)]));
        let client = client_with(transport.clone());
        let error = client
            .execute(
                "GET",
                &arm_url("/resourcegroups?api-version=2021-04-01"),
                &Map::new(),
            )
            .await
            .expect_err("token failure must stop before ARM");
        assert_eq!(error.code(), expected);
        assert_eq!(error.retryable(), retryable);
        assert_eq!(transport.requests().len(), 1);
        assert!(!transport.requests()[0].effect);
    }
}

#[tokio::test]
async fn accepted_async_effect_returns_provider_receipt_without_retry() {
    let transport = Arc::new(FixtureTransport::new([
        Ok(token_response()),
        Ok(AzureHttpResponse::fixture(
            StatusCode::ACCEPTED,
            br#"{"status":"Accepted"}"#,
        )),
    ]));
    let client = client_with(transport.clone());
    assert_eq!(
        client
            .execute(
                "PUT",
                &arm_url("/resourcegroups/demo?api-version=2021-04-01"),
                &Map::new(),
            )
            .await
            .expect("accepted Azure effect"),
        Value::String(r#"{"status":"Accepted"}"#.to_owned())
    );
    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    assert!(requests[1].effect);
}

#[tokio::test]
async fn effects_are_one_attempt_and_ambiguous_status_or_projection_is_unknown() {
    for response in [
        AzureHttpResponse::fixture(StatusCode::TOO_MANY_REQUESTS, b"rate"),
        AzureHttpResponse::fixture(StatusCode::INTERNAL_SERVER_ERROR, b"server"),
        AzureHttpResponse::fixture(StatusCode::FOUND, b"redirect"),
        AzureHttpResponse::fixture(StatusCode::OK, vec![0xff]),
    ] {
        let transport = Arc::new(FixtureTransport::new([Ok(token_response()), Ok(response)]));
        let client = client_with(transport.clone());
        let error = client
            .execute(
                "DELETE",
                &arm_url("/resourcegroups/demo?api-version=2021-04-01"),
                &Map::new(),
            )
            .await
            .expect_err("ambiguous Azure effect must fail");
        assert_eq!(error.code(), AzureClientErrorCode::UnknownOutcome);
        assert!(!error.retryable());
        assert_eq!(transport.requests().len(), 2);
        assert!(transport.requests()[1].effect);
    }
}

#[tokio::test]
async fn read_errors_are_typed_retryable_and_data_free() {
    for (status, expected, retryable) in [
        (
            StatusCode::UNAUTHORIZED,
            AzureClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            AzureClientErrorCode::Authorization,
            false,
        ),
        (StatusCode::NOT_FOUND, AzureClientErrorCode::NotFound, false),
        (
            StatusCode::TOO_MANY_REQUESTS,
            AzureClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::REQUEST_TIMEOUT,
            AzureClientErrorCode::Timeout,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            AzureClientErrorCode::DependencyUnavailable,
            true,
        ),
    ] {
        let transport = Arc::new(FixtureTransport::new([
            Ok(token_response()),
            Ok(AzureHttpResponse::fixture(status, b"provider-secret-body")),
        ]));
        let client = client_with(transport);
        let error = client
            .execute(
                "GET",
                &arm_url("/resourcegroups?api-version=2021-04-01"),
                &Map::new(),
            )
            .await
            .expect_err("Azure read error fixture");
        assert_eq!(error.code(), expected);
        assert_eq!(error.retryable(), retryable);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains("provider-secret-body"));
        assert!(!diagnostic.contains(CLIENT_SECRET));
        assert!(!diagnostic.contains(ACCESS_TOKEN));
        assert!(!diagnostic.contains(SUBSCRIPTION_ID));
    }
}

#[tokio::test]
async fn healthcheck_uses_exact_read_route_and_stable_tuple_projection() {
    let transport = Arc::new(FixtureTransport::new([
        Ok(token_response()),
        Ok(AzureHttpResponse::fixture(
            StatusCode::OK,
            br#"{"value":[]}"#,
        )),
    ]));
    let client = client_with(transport.clone());
    assert_eq!(client.healthcheck().await, json!([true, ""]));
    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[1].method, "GET");
    assert_eq!(
        requests[1].url,
        arm_url("/resourcegroups?api-version=2021-04-01")
    );
    assert!(!requests[1].effect);

    let transport = Arc::new(FixtureTransport::new([
        Ok(token_response()),
        Ok(AzureHttpResponse::fixture(
            StatusCode::FORBIDDEN,
            b"private",
        )),
    ]));
    let client = client_with(transport);
    assert_eq!(
        client.healthcheck().await,
        json!([false, "resource-group listing is not authorized"])
    );
}

#[test]
fn two_clients_keep_subscription_tenant_and_secret_authority_isolated() {
    let mut second = settings(&json!([]));
    second.insert("subscription_id".to_owned(), json!(OTHER_SUBSCRIPTION_ID));
    second.insert("tenant_id".to_owned(), json!("other.example"));
    second.insert(
        "client_id".to_owned(),
        json!("bbbbbbbb-cccc-dddd-eeee-ffffffffffff"),
    );
    second.insert("client_secret".to_owned(), json!("other-secret"));
    let first = AzureClient::with_transport(config(), Arc::new(FixtureTransport::new([])));
    let second = AzureClient::with_transport(
        AzureToolkitConfig::parse(&second).expect("second Azure config"),
        Arc::new(FixtureTransport::new([])),
    );
    let first_request = first.test_token_request().expect("first token request");
    let second_request = second.test_token_request().expect("second token request");
    assert!(first_request.url().as_str().contains("tenant.example"));
    assert!(second_request.url().as_str().contains("other.example"));
    let AzureRequestBody::Bytes(first_body) = first_request.body() else {
        panic!("first token form");
    };
    let AzureRequestBody::Bytes(second_body) = second_request.body() else {
        panic!("second token form");
    };
    assert!(
        first_body
            .windows("claim-only-secret".len())
            .any(|window| window == b"claim-only-secret")
    );
    assert!(
        !second_body
            .windows(CLIENT_SECRET.len())
            .any(|window| window == CLIENT_SECRET.as_bytes())
    );
    assert!(
        second_body
            .windows("other-secret".len())
            .any(|window| window == b"other-secret")
    );
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn AzureApi> = Arc::new(FixtureApi::new());
    let toolset = test_build_with_api("gate", SUBSCRIPTION_ID, &[], &policy(&[]), &api)
        .expect("complete azure toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("azure tools");
    super::sdk_conformance::assert_sdk_conformance("azure", &tools);
}
