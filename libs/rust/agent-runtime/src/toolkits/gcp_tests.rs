//! Focused compatibility and safety tests for the capability-disabled GCP family.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};
use std::time::{Duration, UNIX_EPOCH};

use adk_core::{ReadonlyContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use percent_encoding::percent_decode_str;
use reqwest::StatusCode;
use serde_json::{Map, Value, json};

use super::families::gcp::client::{
    GcpApi, GcpClient, GcpClientError, GcpClientErrorCode, GcpHttpResponse, GcpTransport,
    validate_api_url,
};
use super::families::gcp::config::{GcpConfigErrorCode, GcpToolkitConfig};
use super::families::gcp::tools::{GcpToolsetErrorCode, test_build_with_api, test_catalog};
use super::policy::ToolAdmissionPolicy;

const TEST_PKCS8_KEY_BODY: &str = "MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQDIp4UApaJQ247TbIW43Pg8S+GVMRT6qsdhbg6iSSL6a3qwH4VYLIFcw73rXtRnYrxTasyqi3JwWwDO8xay7FCPuWlyQbnjQjhBnMz3M57riwYhR69PWTL2E9m8CucL9tVtRDLoPhN2dYdTG/qd1WUxdBJEvnXovJImufpEtLihATWNfou3XQxySk8R7Od3diY/rv55YS6x1xZG536JgoZr4UAOr8NYDTE5tBqqc4AYc3LyLjW9VbKISWFlyIHtFU1YESRcUtVswJ1JFtTypQvPWuCiY39M+mv52q/BE9uoODtt19pt2Nsi2FEKjTEVmDMIkJoaAzJReqVeiW4VQkmzAgMBAAECggEAI6TukZDa5rY6BwDOOGq4hi2Moy4W5fiUdpBQdS+80PNq1gKjc2hkipATGs67uKnnfoIIXXtsFt1zpU+1ho9IOF/dhXh7hw1qZO1v07IN1xXZPuw3DkdwMBqSoT7mkE+G1mQ5DtyIJJD4OyFLQeJ4mXJfFGspEvD8nXiIJtBbw+3cMzbUJRYwTWfTxIHfkq7uuXUs1zn3hGm1Ku3WIQo/e3+y1eiecSTqJqrGGWLtZjB6689c59RI0leT6jM4tizOIQ3BkUXAetn/HRFbKZRcNFhh0e7+G6QIVTFX/wXHbLZsJWkPzHxNX2USoWqgpnmgiGZSGTbAt/CJ492NeX0K8QKBgQD4W6jcKVAjlu6SKrhVlhO8RdjYs4IC+Mi4/1eyhvCtgtPhrHxWb/5zHPrlYZrt3E5rdhvcshNkcOM9cS1MxwPCnJshs+eWnjXwkl+tWy3ceroc21xAhu9XHrPqNLuyX04YHV/B0Rg23aC+/C8aQmikq30yeLxFpTiz0jQdSDgnFwKBgQDO1BfuiMQBoDRDYfUx3NfwJXcw5AX81U625OU5aOZc5WBC3I/F4W5S5r3D3CbsiunD+JGxxEuR+xFjSinxQkT9hQ/Vjp5PX53wJ1WmGQmM/VyBlSN6htfCR/Y8ra9nuUiV1qphlTrckdy1wY2VreK/RG3QZcFRlrlv+mFWGXaTxQKBgQC/yYCHq3uQUDChPU4mAYPyAvomtdBzXQ0cF0rwuVXIl9vpTNqjoU6cNEfntM0AW/1O7OEtN3LUQHyq6Ogzfwf/VBJUH2p6nGhJA6/Q3jV3Kmrod9kwl0LiQvpqpRhA8WoMIzrcIA0T6WgFtBbnr1rBtxAyVpwFKEa2TmAiMK/0NwKBgDW7gCQmP9W0Sx+eWVcE6symzxpSgwO2XubA/JQ3nnFP3fxA1NExybmb3Hz/utUFGcohz6gBOSjJszC6Wb8l2kqKwRxYGuTAEIYNkgC+zG5mfBvmJPt2AKOmkmAdN06ZIjRbOpRzcoFPG6nUiPXz4M6T9nuHk/ugTri6sYLuxpGJAoGBALI0mlazlyncjdZYq8GNnN8HaQu6uMahky1cgJjnN5LSq8jC03gEhHwyPlFSmjKVXD0En2YyQC5dEZAtFde76EJMAqtU3ZbEDADY/0H1ajcguEPUXBtey/xQ2y5tWgsXtaF0PeIfamGlgC2pAnH72m5MbRKuM5IiUql/qXNlOreq";
const SCOPE: &str = "https://www.googleapis.com/auth/cloud-platform.read-only";
const API_URL: &str = "https://compute.googleapis.com/compute/v1/projects/example/zones";

fn settings(email: &str, selected_tools: &Value) -> Map<String, Value> {
    let credentials = json!({
        "type": "service_account",
        "client_email": email,
        "private_key_id": "0123456789abcdef",
        "private_key": format!(
            "-----BEGIN PRIVATE KEY-----\n{TEST_PKCS8_KEY_BODY}\n-----END PRIVATE KEY-----"
        ),
        "token_uri": "https://oauth2.googleapis.com/token"
    });
    json!({
        "api_key": credentials.to_string(),
        "selected_tools": selected_tools
    })
    .as_object()
    .cloned()
    .expect("GCP settings fixture is an object")
}

fn config() -> GcpToolkitConfig {
    GcpToolkitConfig::parse(&settings(
        "agent@project.iam.gserviceaccount.com",
        &json!([]),
    ))
    .expect("valid GCP fixture config")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("GCP fixture policy"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("gcp-test")
            .with_session_id("session-1")
            .with_function_call_id("gcp-call-7"),
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
    responses: Mutex<VecDeque<Result<GcpHttpResponse, GcpClientError>>>,
    requests: Mutex<Vec<RecordedRequest>>,
}

impl FixtureTransport {
    fn new(responses: impl IntoIterator<Item = Result<GcpHttpResponse, GcpClientError>>) -> Self {
        Self {
            responses: Mutex::new(responses.into_iter().collect()),
            requests: Mutex::new(Vec::new()),
        }
    }

    fn requests(&self) -> Vec<RecordedRequest> {
        self.requests
            .lock()
            .expect("GCP request fixture lock")
            .clone()
    }
}

#[async_trait]
impl GcpTransport for FixtureTransport {
    async fn execute(
        &self,
        request: reqwest::Request,
        effect: bool,
        response_limit: usize,
    ) -> Result<GcpHttpResponse, GcpClientError> {
        self.requests
            .lock()
            .expect("GCP request fixture lock")
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
            .expect("GCP response fixture lock")
            .pop_front()
            .unwrap_or_else(|| {
                Err(GcpClientError::fixture(
                    GcpClientErrorCode::InvalidResponse,
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
        self.calls.lock().expect("GCP API fixture lock").clone()
    }
}

#[async_trait]
impl GcpApi for FixtureApi {
    async fn execute(
        &self,
        method: &str,
        scopes: &[String],
        url: &str,
        optional_args: &Map<String, Value>,
    ) -> Result<Value, GcpClientError> {
        self.calls
            .lock()
            .expect("GCP API fixture lock")
            .push(json!({
                "method": method,
                "scopes": scopes,
                "url": url,
                "optional_args": optional_args
            }));
        Ok(json!({"fixture": true}))
    }
}

fn client_with(transport: Arc<FixtureTransport>) -> GcpClient {
    let transport_trait: Arc<dyn GcpTransport> = transport;
    GcpClient::with_transport(config(), transport_trait)
}

#[test]
fn configuration_is_claim_scoped_strict_and_redacted() {
    let selected = json!(["execute_request", "execute_request"]);
    let parsed = GcpToolkitConfig::parse(&settings(
        "agent@project.iam.gserviceaccount.com",
        &selected,
    ))
    .expect("valid GCP config");
    assert_eq!(
        parsed.selected_tools(),
        [Box::<str>::from("execute_request")]
    );

    for patch in [
        json!({"api_key": null}),
        json!({"api_key": ""}),
        json!({"api_key": "{}"}),
        json!({"api_key": json!({"type":"service_account"}).to_string()}),
    ] {
        let mut values = settings("agent@project.iam.gserviceaccount.com", &json!([]));
        values.extend(patch.as_object().expect("GCP settings patch").clone());
        let Err(error) = GcpToolkitConfig::parse(&values) else {
            panic!("invalid GCP configuration must fail");
        };
        assert_eq!(error.code(), GcpConfigErrorCode::InvalidConfiguration);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains("agent@"));
        assert!(!diagnostic.contains(TEST_PKCS8_KEY_BODY));
    }
}

#[test]
fn service_account_domain_and_private_key_are_validated_without_network() {
    for email in [
        "agent@evilgserviceaccount.com",
        "agent@example.com",
        "@project.iam.gserviceaccount.com",
    ] {
        let Err(error) = GcpToolkitConfig::parse(&settings(email, &json!([]))) else {
            panic!("invalid service-account email must fail");
        };
        assert_eq!(error.code(), GcpConfigErrorCode::InvalidConfiguration);
    }

    let mut values = settings("agent@project.iam.gserviceaccount.com", &json!([]));
    values.insert("api_key".to_owned(), json!("not-json"));
    let Err(error) = GcpToolkitConfig::parse(&values) else {
        panic!("invalid private key must fail");
    };
    assert_eq!(error.code(), GcpConfigErrorCode::InvalidConfiguration);
}

#[test]
fn jwt_bearer_grant_has_exact_claims_and_fixed_token_authority() {
    let client = GcpClient::with_transport(config(), Arc::new(FixtureTransport::new([])));
    let request = client
        .test_token_request(
            &[SCOPE.to_owned()],
            UNIX_EPOCH + Duration::from_secs(1_700_000_000),
        )
        .expect("GCP token request");
    assert_eq!(request.method(), reqwest::Method::POST);
    assert_eq!(
        request.url().as_str(),
        "https://oauth2.googleapis.com/token"
    );
    assert_eq!(
        request
            .headers()
            .get(reqwest::header::CONTENT_TYPE)
            .and_then(|value| value.to_str().ok()),
        Some("application/x-www-form-urlencoded")
    );
    let form = request
        .body()
        .and_then(reqwest::Body::as_bytes)
        .and_then(|body| std::str::from_utf8(body).ok())
        .expect("GCP token form body");
    let assertion = decode_form_value(form, "assertion");
    let parts = assertion.split('.').collect::<Vec<_>>();
    assert_eq!(parts.len(), 3);
    let header = decode_jwt_part(parts[0]);
    let claims = decode_jwt_part(parts[1]);
    assert_eq!(
        header,
        json!({"alg":"RS256","kid":"0123456789abcdef","typ":"JWT"})
    );
    assert_eq!(claims["iss"], "agent@project.iam.gserviceaccount.com");
    assert_eq!(claims["scope"], SCOPE);
    assert_eq!(claims["aud"], "https://oauth2.googleapis.com/token");
    assert_eq!(claims["iat"], 1_700_000_000_u64);
    assert_eq!(claims["exp"], 1_700_003_600_u64);
}

#[test]
fn two_service_accounts_never_cross_signing_identity() {
    let first = GcpToolkitConfig::parse(&settings(
        "first@project-a.iam.gserviceaccount.com",
        &json!([]),
    ))
    .expect("first GCP identity");
    let second = GcpToolkitConfig::parse(&settings(
        "second@project-b.iam.gserviceaccount.com",
        &json!([]),
    ))
    .expect("second GCP identity");
    let first = GcpClient::with_transport(first, Arc::new(FixtureTransport::new([])));
    let second = GcpClient::with_transport(second, Arc::new(FixtureTransport::new([])));
    let now = UNIX_EPOCH + Duration::from_secs(1_700_000_000);
    let first_claims = request_claims(&first, now);
    let second_claims = request_claims(&second, now);
    assert_eq!(
        first_claims["iss"],
        "first@project-a.iam.gserviceaccount.com"
    );
    assert_eq!(
        second_claims["iss"],
        "second@project-b.iam.gserviceaccount.com"
    );
}

fn request_claims(client: &GcpClient, now: std::time::SystemTime) -> Value {
    let request = client
        .test_token_request(&[SCOPE.to_owned()], now)
        .expect("GCP identity token request");
    let form = request
        .body()
        .and_then(reqwest::Body::as_bytes)
        .and_then(|body| std::str::from_utf8(body).ok())
        .expect("GCP identity form");
    let assertion = decode_form_value(form, "assertion");
    let claims = assertion
        .split('.')
        .nth(1)
        .expect("GCP identity JWT claims");
    decode_jwt_part(claims)
}

fn decode_form_value(form: &str, name: &str) -> String {
    form.split('&')
        .filter_map(|pair| pair.split_once('='))
        .find(|(key, _)| *key == name)
        .and_then(|(_, value)| percent_decode_str(value).decode_utf8().ok())
        .map(std::borrow::Cow::into_owned)
        .expect("form value")
}

fn decode_jwt_part(value: &str) -> Value {
    let bytes = URL_SAFE_NO_PAD.decode(value).expect("JWT fixture base64");
    serde_json::from_slice(&bytes).expect("JWT fixture JSON")
}

#[tokio::test]
async fn catalog_descriptions_schemas_selection_and_policy_are_truthful() {
    let api: Arc<dyn GcpApi> = Arc::new(FixtureApi::new());
    let toolset = test_build_with_api("gcp-prod", &[], &policy(&[]), &api)
        .expect("complete GCP fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("GCP tools");
    assert_eq!(test_catalog(), [("execute_request", "execute")]);
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name(), "execute_request");
    assert!(!tools[0].is_read_only());
    assert!(!tools[0].is_concurrency_safe());
    assert!(tools[0].description().len() <= 1_000);
    for cue in [
        "create, update, patch, delete",
        "1 to 32 exact unique",
        "googleapis.com",
        "one OAuth token exchange and one API request",
        "no automatic retry",
        "independently require approval",
        "reconcile Google Cloud state",
    ] {
        assert!(tools[0].description().contains(cue), "missing cue: {cue}");
    }
    let schema = tools[0].parameters_schema().expect("GCP schema");
    assert_eq!(schema["required"], json!(["method", "scopes", "url"]));
    assert_eq!(schema["additionalProperties"], false);
    assert_eq!(schema["properties"]["method"]["maxLength"], 32);
    assert_eq!(schema["properties"]["scopes"]["maxItems"], 32);
    assert_eq!(
        schema["properties"]["optional_args"]["default"],
        Value::Null
    );

    let blocked = test_build_with_api(
        "gcp-prod",
        &[],
        &policy(&[("gcp", &["execute_request"])]),
        &api,
    )
    .expect("blocked GCP definition");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        blocked
            .tools(readonly)
            .await
            .expect("blocked tools")
            .is_empty()
    );
    let Err(error) = test_build_with_api("gcp-prod", &["unknown".to_owned()], &policy(&[]), &api)
    else {
        panic!("unknown GCP selection must fail");
    };
    assert_eq!(error.code(), GcpToolsetErrorCode::UnsupportedSelection);
}

#[tokio::test]
async fn tool_dispatch_preserves_exact_arguments_and_rejects_bad_calls_before_api() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn GcpApi> = api.clone();
    let toolset =
        test_build_with_api("gcp", &[], &policy(&[]), &api_trait).expect("GCP fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = toolset.tools(readonly).await.expect("GCP tools").remove(0);
    let result = tool
        .execute(
            context(),
            json!({
                "method":"POST",
                "scopes":[SCOPE],
                "url":API_URL,
                "optional_args":{"json":{"name":"zone-a"}}
            }),
        )
        .await
        .expect("GCP tool dispatch");
    assert_eq!(result, json!({"fixture":true}));
    assert_eq!(api.calls().len(), 1);

    for arguments in [
        json!({"method":"GET","scopes":[],"url":API_URL}),
        json!({"method":"GET","scopes":[SCOPE,SCOPE],"url":API_URL}),
        json!({"method":"GET","scopes":[SCOPE],"url":"https://example.com/v1"}),
        json!({"method":"GET","scopes":[SCOPE],"url":API_URL,"unknown":true}),
        json!({"method":"GET","scopes":[SCOPE],"url":API_URL,"optional_args":[]}),
        json!({"method":"GET","scopes":[SCOPE],"url":API_URL,"optional_args":{"timeout":1}}),
        json!({"method":"POST","scopes":[SCOPE],"url":API_URL,"optional_args":{"json":{},"data":{}}}),
    ] {
        assert!(tool.execute(context(), arguments).await.is_err());
    }
    assert_eq!(api.calls().len(), 1);
}

#[test]
fn google_api_authority_rejects_non_google_ports_fragments_and_traversal() {
    assert!(validate_api_url(API_URL).is_ok());
    assert!(validate_api_url("https://storage.googleapis.com/bucket/object?alt=json").is_ok());
    for value in [
        "http://compute.googleapis.com/v1",
        "https://user@compute.googleapis.com/v1",
        "https://compute.googleapis.com:8443/v1",
        "https://compute.googleapis.com/v1#fragment",
        "https://compute.googleapis.com/v1/../secrets",
        "https://compute.googleapis.com/v1/%252fsecrets",
        "https://compute.googleapis.com//v1",
        "https://googleapis.com.evil.example/v1",
    ] {
        assert!(
            validate_api_url(value).is_err(),
            "accepted unsafe URL: {value}"
        );
    }
}

#[test]
fn api_request_builder_encodes_query_headers_json_and_form_without_auth_override() {
    let json_request = GcpClient::test_api_request(
        "PATCH",
        API_URL,
        json!({
            "params":{"filter":"name = a","label":["x","y"]},
            "headers":{"x-goog-user-project":"billing-project"},
            "json":{"enabled":true}
        })
        .as_object()
        .expect("request options"),
        "token-one",
    )
    .expect("GCP JSON request");
    assert_eq!(json_request.method(), reqwest::Method::PATCH);
    assert!(json_request.url().query().is_some_and(|query| {
        query.contains("filter=name+%3D+a")
            && query.contains("label=x")
            && query.contains("label=y")
    }));
    assert_eq!(
        json_request
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok()),
        Some("Bearer token-one")
    );
    assert_eq!(
        json_request.body().and_then(reqwest::Body::as_bytes),
        Some(br#"{"enabled":true}"#.as_slice())
    );

    let form_request = GcpClient::test_api_request(
        "POST",
        API_URL,
        json!({"data":{"name":"a b","tag":["x","y"]}})
            .as_object()
            .expect("form options"),
        "token-two",
    )
    .expect("GCP form request");
    let form = form_request
        .body()
        .and_then(reqwest::Body::as_bytes)
        .and_then(|body| std::str::from_utf8(body).ok())
        .expect("form body");
    assert!(form.contains("name=a+b"));
    assert!(form.contains("tag=x"));
    assert!(form.contains("tag=y"));

    let forbidden = json!({"headers":{"Authorization":"other"}});
    assert!(
        GcpClient::test_api_request(
            "GET",
            API_URL,
            forbidden.as_object().expect("forbidden headers"),
            "token",
        )
        .is_err()
    );
}

#[tokio::test]
async fn token_then_api_are_exactly_one_attempt_and_success_projection_is_stable() {
    let transport = Arc::new(FixtureTransport::new([
        Ok(GcpHttpResponse::fixture(
            StatusCode::OK,
            br#"{"access_token":"access-one","expires_in":3600}"#,
        )),
        Ok(GcpHttpResponse::fixture(
            StatusCode::OK,
            br#"{"items":[{"name":"zone-a"}]}"#,
        )),
    ]));
    let result = client_with(transport.clone())
        .execute("GET", &[SCOPE.to_owned()], API_URL, &Map::new())
        .await
        .expect("GCP API result");
    assert_eq!(result, json!({"items":[{"name":"zone-a"}]}));
    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[0].url, "https://oauth2.googleapis.com/token");
    assert!(!requests[0].effect);
    assert_eq!(requests[1].url, API_URL);
    assert!(!requests[1].effect);
    assert_eq!(
        requests[1].authorization.as_deref(),
        Some("Bearer access-one")
    );

    let empty_transport = Arc::new(FixtureTransport::new([
        Ok(GcpHttpResponse::fixture(
            StatusCode::OK,
            br#"{"access_token":"access-two"}"#,
        )),
        Ok(GcpHttpResponse::fixture(StatusCode::NO_CONTENT, [])),
    ]));
    let empty = client_with(empty_transport)
        .execute("DELETE", &[SCOPE.to_owned()], API_URL, &Map::new())
        .await
        .expect("empty GCP success");
    assert_eq!(
        empty,
        Value::String(
            "Success: The request has been fulfilled and resulted in a new resource being created."
                .to_owned()
        )
    );
}

#[tokio::test]
async fn token_failure_stops_before_api_and_effect_failures_require_reconciliation() {
    let auth_transport = Arc::new(FixtureTransport::new([Ok(GcpHttpResponse::fixture(
        StatusCode::UNAUTHORIZED,
        br#"{"error":"invalid_grant","secret":"do-not-leak"}"#,
    ))]));
    let error = client_with(auth_transport.clone())
        .execute("GET", &[SCOPE.to_owned()], API_URL, &Map::new())
        .await
        .expect_err("token failure");
    assert_eq!(error.code(), GcpClientErrorCode::Authentication);
    assert_eq!(auth_transport.requests().len(), 1);
    assert!(!format!("{error:?} {error}").contains("do-not-leak"));

    for status in [
        StatusCode::REQUEST_TIMEOUT,
        StatusCode::TOO_MANY_REQUESTS,
        StatusCode::INTERNAL_SERVER_ERROR,
        StatusCode::BAD_GATEWAY,
    ] {
        let transport = Arc::new(FixtureTransport::new([
            Ok(GcpHttpResponse::fixture(
                StatusCode::OK,
                br#"{"access_token":"effect-token"}"#,
            )),
            Ok(GcpHttpResponse::fixture(status, br#"{"message":"secret"}"#)),
        ]));
        let error = client_with(transport.clone())
            .execute("POST", &[SCOPE.to_owned()], API_URL, &Map::new())
            .await
            .expect_err("ambiguous GCP effect");
        assert_eq!(error.code(), GcpClientErrorCode::UnknownOutcome);
        assert!(!error.retryable());
        assert_eq!(transport.requests().len(), 2);
        assert!(transport.requests()[1].effect);
    }
}

#[tokio::test]
async fn read_statuses_remain_typed_retryable_and_data_free() {
    for (status, code, retryable) in [
        (
            StatusCode::UNAUTHORIZED,
            GcpClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            GcpClientErrorCode::Authorization,
            false,
        ),
        (StatusCode::NOT_FOUND, GcpClientErrorCode::NotFound, false),
        (
            StatusCode::REQUEST_TIMEOUT,
            GcpClientErrorCode::Timeout,
            true,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            GcpClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            GcpClientErrorCode::DependencyUnavailable,
            true,
        ),
    ] {
        let transport = Arc::new(FixtureTransport::new([
            Ok(GcpHttpResponse::fixture(
                StatusCode::OK,
                br#"{"access_token":"read-token"}"#,
            )),
            Ok(GcpHttpResponse::fixture(
                status,
                br#"{"secret":"provider"}"#,
            )),
        ]));
        let error = client_with(transport)
            .execute("GET", &[SCOPE.to_owned()], API_URL, &Map::new())
            .await
            .expect_err("typed GCP read failure");
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
        assert!(!format!("{error:?} {error}").contains("provider"));
    }
}

#[tokio::test]
async fn post_accept_invalid_and_oversized_results_are_effect_aware() {
    for (method, expected) in [
        ("GET", GcpClientErrorCode::InvalidResponse),
        ("POST", GcpClientErrorCode::UnknownOutcome),
    ] {
        let transport = Arc::new(FixtureTransport::new([
            Ok(GcpHttpResponse::fixture(
                StatusCode::OK,
                br#"{"access_token":"projection-token"}"#,
            )),
            Ok(GcpHttpResponse::fixture(StatusCode::OK, b"not-json")),
        ]));
        let error = client_with(transport)
            .execute(method, &[SCOPE.to_owned()], API_URL, &Map::new())
            .await
            .expect_err("invalid accepted GCP result");
        assert_eq!(error.code(), expected);
    }

    for (method, expected) in [
        ("GET", GcpClientErrorCode::ResourceExhausted),
        ("DELETE", GcpClientErrorCode::UnknownOutcome),
    ] {
        let oversized = json!({"value":"x".repeat(512 * 1_024)}).to_string();
        let transport = Arc::new(FixtureTransport::new([
            Ok(GcpHttpResponse::fixture(
                StatusCode::OK,
                br#"{"access_token":"projection-token"}"#,
            )),
            Ok(GcpHttpResponse::fixture(StatusCode::OK, oversized)),
        ]));
        let error = client_with(transport)
            .execute(method, &[SCOPE.to_owned()], API_URL, &Map::new())
            .await
            .expect_err("oversized accepted GCP result");
        assert_eq!(error.code(), expected);
        assert!(!error.retryable());
    }
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn GcpApi> = Arc::new(FixtureApi::new());
    let toolset =
        test_build_with_api("gate", &[], &policy(&[]), &api).expect("complete gcp toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("gcp tools");
    super::sdk_conformance::assert_sdk_conformance("gcp", &tools);
}
