//! Focused compatibility and safety tests for the capability-disabled Keycloak family.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Toolset};
use async_trait::async_trait;
use reqwest::{Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::keycloak::client::{
    KeycloakApi, KeycloakClient, KeycloakClientError, KeycloakClientErrorCode,
    KeycloakHttpResponse, KeycloakTransport, test_map_admin_status, validate_relative_url,
};
use super::families::keycloak::config::{KeycloakConfigErrorCode, KeycloakToolkitConfig};
use super::families::keycloak::tools::{
    KeycloakToolsetErrorCode, test_build_with_api, test_catalog, test_parse_params,
};
use super::policy::ToolAdmissionPolicy;

const CLIENT_SECRET: &str = "  claim-secret  ";
const ACCESS_TOKEN: &str = "fixture-access-token";

fn settings(base_url: &str) -> Map<String, Value> {
    json!({
        "base_url": base_url,
        "realm": "Tenant Realm",
        "client_id": "worker-client",
        "client_secret": CLIENT_SECRET,
        "selected_tools": []
    })
    .as_object()
    .cloned()
    .expect("Keycloak settings fixture is an object")
}

fn config(base_url: &str) -> KeycloakToolkitConfig {
    KeycloakToolkitConfig::parse(&settings(base_url)).expect("valid Keycloak fixture config")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Keycloak fixture policy"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("keycloak-test")
            .with_session_id("session-1")
            .with_function_call_id("keycloak-call-7"),
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
    responses: Mutex<VecDeque<Result<KeycloakHttpResponse, KeycloakClientError>>>,
    requests: Mutex<Vec<RecordedRequest>>,
}

impl FixtureTransport {
    fn new(
        responses: impl IntoIterator<Item = Result<KeycloakHttpResponse, KeycloakClientError>>,
    ) -> Self {
        Self {
            responses: Mutex::new(responses.into_iter().collect()),
            requests: Mutex::new(Vec::new()),
        }
    }

    fn requests(&self) -> Vec<RecordedRequest> {
        self.requests
            .lock()
            .expect("Keycloak request fixture lock")
            .clone()
    }
}

#[async_trait]
impl KeycloakTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
        response_limit: usize,
    ) -> Result<KeycloakHttpResponse, KeycloakClientError> {
        let body = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .unwrap_or_default()
            .to_vec();
        self.requests
            .lock()
            .expect("Keycloak request fixture lock")
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
            .expect("Keycloak response fixture lock")
            .pop_front()
            .unwrap_or_else(|| {
                Err(KeycloakClientError::fixture(
                    KeycloakClientErrorCode::InvalidResponse,
                    false,
                ))
            })
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
            .expect("Keycloak API fixture lock")
            .clone()
    }
}

#[async_trait]
impl KeycloakApi for FixtureApi {
    async fn execute(
        &self,
        method: &str,
        relative_url: &str,
        params: &Map<String, Value>,
    ) -> Result<Value, KeycloakClientError> {
        self.calls
            .lock()
            .expect("Keycloak API fixture lock")
            .push(json!({
                "method":method,
                "relative_url":relative_url,
                "params":params
            }));
        Ok(Value::String("fixture-result".to_owned()))
    }
}

fn client_with(transport: Arc<FixtureTransport>) -> KeycloakClient {
    let transport_trait: Arc<dyn KeycloakTransport> = transport;
    KeycloakClient::with_transport(config("https://id.example.test/auth/"), transport_trait)
}

fn token_response() -> KeycloakHttpResponse {
    KeycloakHttpResponse::fixture(
        StatusCode::OK,
        format!(r#"{{"access_token":"{ACCESS_TOKEN}","expires_in":60}}"#),
    )
}

#[test]
fn configuration_is_claim_scoped_normalized_and_redacted() {
    let parsed = config(" https://id.example.test/auth/ ");
    let client = KeycloakClient::with_transport(parsed, Arc::new(FixtureTransport::new([])));
    let request = client
        .test_token_request()
        .expect("bounded token request fixture");
    assert_eq!(
        request.url().as_str(),
        "https://id.example.test/auth/realms/Tenant%20Realm/protocol/openid-connect/token"
    );
    let body = request
        .body()
        .and_then(reqwest::Body::as_bytes)
        .expect("token form body");
    assert_eq!(
        body,
        b"client_id=worker%2Dclient&client_secret=%20%20claim%2Dsecret%20%20&grant_type=client_credentials"
    );
    assert!(
        request
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .is_none()
    );

    for invalid in [
        "http://id.example.test",
        "https://user@id.example.test",
        "https://id.example.test/auth?x=1",
        "https://id.example.test/auth#fragment",
        "https://id.example.test/%2e%2e/escape",
        "https://id.example.test/%252e%252e/escape",
        "https://id.example.test/a%2Fb",
        "https://id.example.test/bad%zz",
    ] {
        let parsed = KeycloakToolkitConfig::parse(&settings(invalid));
        let Err(error) = parsed else {
            panic!("invalid Keycloak authority must fail: {invalid:?}");
        };
        assert_eq!(error.code(), KeycloakConfigErrorCode::InvalidConfiguration);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains(CLIENT_SECRET));
        assert!(!diagnostic.contains("Tenant Realm"));
        assert!(!diagnostic.contains("id.example.test"));
    }
}

#[tokio::test]
async fn metadata_schema_descriptions_selection_and_policy_are_truthful() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn KeycloakApi> = api.clone();
    let toolkit_name = "身份".repeat(150);
    let toolset = test_build_with_api(&toolkit_name, &[], &policy(&[]), &api_trait)
        .expect("complete Keycloak fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Keycloak tools");
    assert_eq!(test_catalog(), [("execute", "execute")]);
    assert_eq!(tools.len(), 1);
    let tool = &tools[0];
    assert_eq!(tool.name(), "execute");
    assert!(!tool.is_read_only());
    assert!(!tool.is_concurrency_safe());
    assert!(tool.description().len() <= 1_000);
    for cue in [
        "configured realm",
        "/users?first=0&max=20",
        "strict JSON object",
        "512 KiB",
        "create, update, delete",
        "independently require approval",
        "no automatic retry",
        "reconcile Keycloak state",
    ] {
        assert!(tool.description().contains(cue), "missing cue: {cue}");
    }
    assert!(!tool.description().contains("id.example.test"));
    assert!(!tool.description().contains(CLIENT_SECRET));

    let schema = tool.parameters_schema().expect("Keycloak schema");
    assert_eq!(schema["required"], json!(["method", "relative_url"]));
    assert_eq!(schema["title"], "ExecuteModel");
    assert_eq!(schema["additionalProperties"], false);
    assert_eq!(schema["properties"]["method"]["maxLength"], 32);
    assert_eq!(schema["properties"]["relative_url"]["maxLength"], 4096);
    assert_eq!(schema["properties"]["params"]["default"], "");
    assert!(
        schema["properties"]["relative_url"]["description"]
            .as_str()
            .expect("relative URL prose")
            .contains("configured_realm")
    );

    assert_eq!(
        tool.execute(
            context(),
            json!({
                "method":"patch",
                "relative_url":"/users/user-7",
                "params":"{\"enabled\":false}"
            }),
        )
        .await
        .expect("valid Keycloak tool invocation"),
        Value::String("fixture-result".to_owned())
    );
    assert_eq!(
        api.calls(),
        [json!({
            "method":"patch",
            "relative_url":"/users/user-7",
            "params":{"enabled":false}
        })]
    );

    let blocked = test_build_with_api(
        "identity",
        &[],
        &policy(&[("keycloak", &["execute"])]),
        &api_trait,
    )
    .expect("blocked Keycloak definition is valid");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        blocked
            .tools(readonly)
            .await
            .expect("blocked tools")
            .is_empty()
    );

    let Err(error) = test_build_with_api(
        "identity",
        &["unknown".to_owned()],
        &policy(&[]),
        &api_trait,
    ) else {
        panic!("unknown Keycloak selection must fail closed");
    };
    assert_eq!(error.code(), KeycloakToolsetErrorCode::UnsupportedSelection);
}

#[tokio::test]
async fn strict_parameter_and_path_admission_fails_before_the_api() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn KeycloakApi> = api.clone();
    let toolset = test_build_with_api("identity", &[], &policy(&[]), &api_trait)
        .expect("Keycloak fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("Keycloak tools")
        .remove(0);

    for arguments in [
        json!({"method":"GET","relative_url":"//other.example/users"}),
        json!({"method":"GET","relative_url":"/../users"}),
        json!({"method":"GET","relative_url":"/%2e%2e/users"}),
        json!({"method":"GET","relative_url":"/users#fragment"}),
        json!({"method":"GET bad","relative_url":"/users"}),
        json!({"method":"GET","relative_url":"/users","params":"{'first':0}"}),
        json!({"method":"GET","relative_url":"/users","params":"[]"}),
        json!({"method":"GET","relative_url":"/users","extra":true}),
    ] {
        assert!(tool.execute(context(), arguments).await.is_err());
    }
    assert!(api.calls().is_empty());
    assert_eq!(
        test_parse_params(Some(&Value::Null)).expect("null params"),
        Map::new()
    );
}

#[tokio::test]
async fn token_and_read_request_wire_are_exact_and_one_attempt_each() {
    let transport = Arc::new(FixtureTransport::new([
        Ok(token_response()),
        Ok(KeycloakHttpResponse::fixture(
            StatusCode::OK,
            r#"[{"id":"user-1"}]"#,
        )),
    ]));
    let client = client_with(transport.clone());
    let result = client
        .execute("get", "/users?first=0&max=20", &Map::new())
        .await
        .expect("Keycloak read fixture");
    assert_eq!(result, Value::String(r#"[{"id":"user-1"}]"#.to_owned()));

    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[0].method, "POST");
    assert_eq!(
        requests[0].url,
        "https://id.example.test/auth/realms/Tenant%20Realm/protocol/openid-connect/token"
    );
    assert_eq!(
        requests[0].content_type.as_deref(),
        Some("application/x-www-form-urlencoded")
    );
    assert_eq!(requests[0].authorization, None);
    assert!(!requests[0].effect);
    assert_eq!(requests[0].response_limit, 64 * 1_024);

    assert_eq!(requests[1].method, "GET");
    assert_eq!(
        requests[1].url,
        "https://id.example.test/auth/admin/realms/Tenant%20Realm/users?first=0&max=20"
    );
    assert_eq!(
        requests[1].authorization.as_deref(),
        Some("Bearer fixture-access-token")
    );
    assert_eq!(
        requests[1].content_type.as_deref(),
        Some("application/json")
    );
    assert_eq!(requests[1].body, b"{}");
    assert!(!requests[1].effect);
    assert_eq!(requests[1].response_limit, 512 * 1_024);
}

#[tokio::test]
async fn custom_effect_method_has_bearer_only_body_and_empty_success_text() {
    let transport = Arc::new(FixtureTransport::new([
        Ok(token_response()),
        Ok(KeycloakHttpResponse::fixture(
            StatusCode::NO_CONTENT,
            Vec::new(),
        )),
    ]));
    let client = client_with(transport.clone());
    let params = json!({"action":"rotate"})
        .as_object()
        .cloned()
        .expect("effect params object");
    assert_eq!(
        client
            .execute("PROPPATCH", "/users/user%2D1/action", &params)
            .await
            .expect("custom Keycloak effect fixture"),
        Value::String(String::new())
    );
    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[1].method, "PROPPATCH");
    assert!(requests[1].effect);
    assert_eq!(requests[1].body, br#"{"action":"rotate"}"#);
    assert_eq!(
        requests[1].authorization.as_deref(),
        Some("Bearer fixture-access-token")
    );
}

#[tokio::test]
async fn authentication_and_admin_failures_are_typed_redacted_and_not_retried() {
    let transport = Arc::new(FixtureTransport::new([Ok(KeycloakHttpResponse::fixture(
        StatusCode::UNAUTHORIZED,
        "provider-secret-body",
    ))]));
    let client = client_with(transport.clone());
    let error = client
        .execute("GET", "/users", &Map::new())
        .await
        .expect_err("token authentication rejection");
    assert_eq!(error.code(), KeycloakClientErrorCode::Authentication);
    assert!(!error.retryable());
    let diagnostic = format!("{error:?} {error}");
    assert!(!diagnostic.contains("provider-secret-body"));
    assert!(!diagnostic.contains(CLIENT_SECRET));
    assert!(!diagnostic.contains("id.example.test"));
    assert_eq!(transport.requests().len(), 1);

    let transport = Arc::new(FixtureTransport::new([
        Ok(token_response()),
        Ok(KeycloakHttpResponse::fixture(
            StatusCode::INTERNAL_SERVER_ERROR,
            "private provider failure",
        )),
    ]));
    let client = client_with(transport.clone());
    let error = client
        .execute("DELETE", "/users/user-1", &Map::new())
        .await
        .expect_err("effect 500 is ambiguous");
    assert_eq!(error.code(), KeycloakClientErrorCode::UnknownOutcome);
    assert!(!error.retryable());
    assert_eq!(transport.requests().len(), 2);
}

#[tokio::test]
async fn token_shape_and_access_token_bounds_fail_before_admin_dispatch() {
    let cases = [
        ("{}".to_owned(), KeycloakClientErrorCode::InvalidResponse),
        (
            r#"{"access_token":null}"#.to_owned(),
            KeycloakClientErrorCode::InvalidResponse,
        ),
        (
            r#"{"access_token":""}"#.to_owned(),
            KeycloakClientErrorCode::InvalidResponse,
        ),
        (
            format!(r#"{{"access_token":"{}"}}"#, "t".repeat(32 * 1_024 + 1)),
            KeycloakClientErrorCode::ResourceExhausted,
        ),
    ];
    for (body, expected) in cases {
        let transport = Arc::new(FixtureTransport::new([Ok(KeycloakHttpResponse::fixture(
            StatusCode::OK,
            body,
        ))]));
        let client = client_with(transport.clone());
        let error = client
            .execute("GET", "/users", &Map::new())
            .await
            .expect_err("invalid token response must fail");
        assert_eq!(error.code(), expected);
        assert_eq!(transport.requests().len(), 1);
    }
}

#[test]
fn read_and_effect_status_classes_preserve_reconciliation_semantics() {
    for status in [
        StatusCode::REQUEST_TIMEOUT,
        StatusCode::TOO_MANY_REQUESTS,
        StatusCode::BAD_GATEWAY,
    ] {
        let read = test_map_admin_status(status, false);
        assert!(matches!(
            read.code(),
            KeycloakClientErrorCode::Timeout
                | KeycloakClientErrorCode::RateLimited
                | KeycloakClientErrorCode::DependencyUnavailable
        ));
        assert!(read.retryable());

        let effect = test_map_admin_status(status, true);
        assert_eq!(effect.code(), KeycloakClientErrorCode::UnknownOutcome);
        assert!(!effect.retryable());
    }
    assert_eq!(
        test_map_admin_status(StatusCode::NOT_FOUND, true).code(),
        KeycloakClientErrorCode::NotFound
    );
    assert_eq!(
        test_map_admin_status(StatusCode::FORBIDDEN, false).code(),
        KeycloakClientErrorCode::Authorization
    );
}

#[tokio::test]
async fn post_accept_invalid_text_is_effect_aware() {
    for (method, expected) in [
        ("GET", KeycloakClientErrorCode::InvalidResponse),
        ("POST", KeycloakClientErrorCode::UnknownOutcome),
    ] {
        let transport = Arc::new(FixtureTransport::new([
            Ok(token_response()),
            Ok(KeycloakHttpResponse::fixture(
                StatusCode::OK,
                vec![0xff, 0xfe],
            )),
        ]));
        let client = client_with(transport);
        let error = client
            .execute(method, "/users", &Map::new())
            .await
            .expect_err("invalid accepted response text");
        assert_eq!(error.code(), expected);
        assert!(!error.retryable());
    }
}

#[tokio::test]
async fn post_accept_serialized_output_bound_is_effect_aware() {
    let expansion = "\"".repeat(300 * 1_024);
    for (method, expected) in [
        ("GET", KeycloakClientErrorCode::ResourceExhausted),
        ("POST", KeycloakClientErrorCode::UnknownOutcome),
    ] {
        let transport = Arc::new(FixtureTransport::new([
            Ok(token_response()),
            Ok(KeycloakHttpResponse::fixture(
                StatusCode::OK,
                expansion.clone(),
            )),
        ]));
        let client = client_with(transport);
        let error = client
            .execute(method, "/users", &Map::new())
            .await
            .expect_err("serialized output above 512 KiB");
        assert_eq!(error.code(), expected);
        assert!(!error.retryable());
    }
}

#[test]
fn relative_url_validation_covers_encoded_authority_escape_and_controls() {
    for valid in [
        "/",
        "/users",
        "/users/user%2D1?briefRepresentation=true",
        "/groups/group-1/role-mappings/realm",
    ] {
        assert!(validate_relative_url(valid).is_ok(), "valid path: {valid}");
    }
    for invalid in [
        "users",
        "//evil.example/users",
        "/./users",
        "/%2E/users",
        "/%252E%252E/users",
        "/%2fadmin",
        "/bad%zz",
        "/users\\escape",
        "/users\nother",
        "/users#fragment",
    ] {
        assert!(
            validate_relative_url(invalid).is_err(),
            "invalid path: {invalid:?}"
        );
    }
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn KeycloakApi> = Arc::new(FixtureApi::new());
    let toolset =
        test_build_with_api("gate", &[], &policy(&[]), &api).expect("complete keycloak toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("keycloak tools");
    super::sdk_conformance::assert_sdk_conformance("keycloak", &tools);
}
