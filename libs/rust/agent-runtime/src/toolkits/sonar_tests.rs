use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_core::{ReadonlyContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use reqwest::header::{ACCEPT, AUTHORIZATION};
use reqwest::{Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::sonar::client::{
    SonarApi, SonarClient, SonarClientError, SonarClientErrorCode, SonarTransport, test_http_status,
};
use super::families::sonar::config::{SonarConfigErrorCode, SonarToolkitConfig};
use super::families::sonar::tools::{
    SonarToolsetErrorCode, build_sonar_read_only_toolset, test_build_with_api,
};
use super::policy::ToolAdmissionPolicy;

fn settings_with_token(selected_tools: &[&str], token: &str, url: &str) -> Map<String, Value> {
    json!({
        "sonar_configuration": {
            "configuration_type": "sonar",
            "url": url,
            "sonar_token": token
        },
        "sonar_project_name": "elitea:runtime",
        "selected_tools": selected_tools
    })
    .as_object()
    .expect("Sonar settings fixture")
    .clone()
}

fn settings(selected_tools: &[&str]) -> Map<String, Value> {
    settings_with_token(
        selected_tools,
        "fixture-sonar-token",
        "https://sonar.example.test/sonarqube/",
    )
}

fn config(selected_tools: &[&str]) -> SonarToolkitConfig {
    SonarToolkitConfig::parse(&settings(selected_tools)).expect("valid Sonar configuration")
}

fn policy(blocked: &[(&str, &[&str])]) -> Arc<ToolAdmissionPolicy> {
    let blocked = blocked
        .iter()
        .map(|(toolkit, tools)| {
            (
                (*toolkit).to_owned(),
                tools.iter().map(ToString::to_string).collect::<Vec<_>>(),
            )
        })
        .collect::<BTreeMap<_, _>>();
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Sonar policy fixture"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("sonar-tool-test")
            .with_session_id("session-1")
            .with_function_call_id("call-1"),
    )
}

#[test]
fn materialized_configuration_is_bounded_deduplicated_and_secret_safe() {
    let parsed = config(&["get_sonar_data", "get_sonar_data"]);
    assert_eq!(
        parsed.selected_tools(),
        [Box::<str>::from("get_sonar_data")]
    );

    for invalid in [
        json!({"selected_tools": []}),
        json!({
            "sonar_configuration": {"url": "http://sonar.test", "sonar_token": "token"},
            "sonar_project_name": "project",
            "selected_tools": []
        }),
        json!({
            "sonar_configuration": {"url": "https://user@sonar.test", "sonar_token": "token"},
            "sonar_project_name": "project",
            "selected_tools": []
        }),
        json!({
            "sonar_configuration": {"url": "https://sonar.test?leak=yes", "sonar_token": "token"},
            "sonar_project_name": "project",
            "selected_tools": []
        }),
        json!({
            "sonar_configuration": {"url": "https://sonar.test/%2e%2e/private", "sonar_token": "token"},
            "sonar_project_name": "project",
            "selected_tools": []
        }),
        json!({
            "sonar_configuration": {"url": "https://sonar.test/../private", "sonar_token": "token"},
            "sonar_project_name": "project",
            "selected_tools": []
        }),
        json!({
            "sonar_configuration": {"url": "https://sonar.test", "sonar_token": "fixture\nsecret"},
            "sonar_project_name": "project",
            "selected_tools": []
        }),
    ] {
        let object = invalid.as_object().expect("invalid Sonar fixture object");
        let Err(error) = SonarToolkitConfig::parse(object) else {
            panic!("malformed configuration must fail before client construction");
        };
        assert_eq!(error.code(), SonarConfigErrorCode::InvalidConfiguration);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains("fixture"));
        assert!(!diagnostic.contains("sonar.test"));
    }

    let oversized = json!({
        "sonar_configuration": {
            "url": "https://sonar.test",
            "sonar_token": "x".repeat(8 * 1_024 + 1)
        },
        "sonar_project_name": "project",
        "selected_tools": []
    });
    let Err(error) = SonarToolkitConfig::parse(
        oversized
            .as_object()
            .expect("oversized Sonar settings object"),
    ) else {
        panic!("oversized Sonar token must fail");
    };
    assert_eq!(error.code(), SonarConfigErrorCode::ResourceExhausted);
}

#[test]
fn request_is_origin_bound_project_bound_and_uses_sensitive_basic_auth() {
    let client = SonarClient::new(config(&["get_sonar_data"])).expect("Sonar client");
    let request = client
        .test_request(
            "/api/issues/search",
            Some(r#"{"componentKeys":"attacker","types":["BUG","VULNERABILITY"],"p":2,"ps":25}"#),
        )
        .expect("bounded Sonar request");
    assert_eq!(request.method(), reqwest::Method::GET);
    assert_eq!(request.url().scheme(), "https");
    assert_eq!(request.url().host_str(), Some("sonar.example.test"));
    assert_eq!(request.url().path(), "/sonarqube/api/issues/search");
    let pairs = request
        .url()
        .query_pairs()
        .map(|(key, value)| (key.into_owned(), value.into_owned()))
        .collect::<Vec<_>>();
    assert_eq!(
        pairs
            .iter()
            .filter(|(key, _)| key == "componentKeys")
            .collect::<Vec<_>>(),
        vec![&("componentKeys".to_owned(), "elitea:runtime".to_owned())]
    );
    assert!(pairs.contains(&("types".to_owned(), "BUG".to_owned())));
    assert!(pairs.contains(&("types".to_owned(), "VULNERABILITY".to_owned())));
    assert_eq!(
        request.headers().get(ACCEPT).expect("JSON accept header"),
        "application/json"
    );
    let authorization = request
        .headers()
        .get(AUTHORIZATION)
        .expect("Basic authorization");
    assert!(authorization.is_sensitive());
    assert_eq!(
        authorization.to_str().expect("ASCII Basic authorization"),
        format!("Basic {}", STANDARD.encode("fixture-sonar-token:"))
    );
    assert_eq!(request.timeout(), Some(&Duration::from_secs(20)));
}

#[test]
fn endpoint_and_query_escape_fail_before_transport() {
    let client = SonarClient::new(config(&[])).expect("Sonar client");
    for relative_url in [
        "https://attacker.test/api/issues/search",
        "/api/issues/search?componentKeys=attacker",
        "/api/issues/search/../admin",
        "/api/system/status",
    ] {
        let error = client
            .test_request(relative_url, None)
            .expect_err("endpoint escape must fail");
        assert_eq!(error.code(), SonarClientErrorCode::InvalidInput);
    }

    for params in [
        "[]",
        r#"{"nested":{"value":"no"}}"#,
        r#"{"bad key":"value"}"#,
        r#"{"ps":0}"#,
        r#"{"ps":501}"#,
        r#"{"p":0}"#,
        r#"{"p":101,"ps":100}"#,
        r#"{"p":[1,2]}"#,
        r#"{"projectKeys":"attacker"}"#,
        r#"{"projects":"attacker"}"#,
        r#"{"componentUuids":"attacker"}"#,
    ] {
        let error = client
            .test_request("/api/issues/search", Some(params))
            .expect_err("invalid query must fail");
        assert!(matches!(
            error.code(),
            SonarClientErrorCode::InvalidInput | SonarClientErrorCode::ResourceExhausted
        ));
        assert!(!format!("{error:?} {error}").contains(params));
    }

    let null_page = client
        .test_request("/api/issues/search", Some(r#"{"p":null,"ps":null}"#))
        .expect("null query values have current requests omission semantics");
    assert!(
        null_page
            .url()
            .query_pairs()
            .any(|(key, value)| key == "ps" && value == "100")
    );
}

#[derive(Default)]
struct FixtureTransport {
    requests: Mutex<Vec<Request>>,
    response: Mutex<Option<Result<Value, SonarClientErrorCode>>>,
}

impl FixtureTransport {
    fn with_response(response: Result<Value, SonarClientErrorCode>) -> Self {
        Self {
            requests: Mutex::new(Vec::new()),
            response: Mutex::new(Some(response)),
        }
    }

    fn request_count(&self) -> usize {
        self.requests
            .lock()
            .expect("Sonar request fixture lock")
            .len()
    }
}

#[async_trait]
impl SonarTransport for FixtureTransport {
    async fn execute_json(&self, request: Request) -> Result<Value, SonarClientError> {
        self.requests
            .lock()
            .expect("Sonar request fixture lock")
            .push(request);
        match self
            .response
            .lock()
            .expect("Sonar response fixture lock")
            .take()
            .expect("one Sonar response")
        {
            Ok(value) => Ok(value),
            Err(code) => Err(test_error(code)),
        }
    }
}

fn test_error(code: SonarClientErrorCode) -> SonarClientError {
    test_http_status(match code {
        SonarClientErrorCode::Authentication => StatusCode::UNAUTHORIZED,
        SonarClientErrorCode::Authorization => StatusCode::FORBIDDEN,
        SonarClientErrorCode::NotFound => StatusCode::NOT_FOUND,
        SonarClientErrorCode::RateLimited => StatusCode::TOO_MANY_REQUESTS,
        SonarClientErrorCode::Timeout => StatusCode::GATEWAY_TIMEOUT,
        SonarClientErrorCode::DependencyUnavailable => StatusCode::SERVICE_UNAVAILABLE,
        _ => StatusCode::IM_A_TEAPOT,
    })
    .expect_err("fixture status must fail")
}

#[tokio::test]
async fn one_request_returns_bounded_current_raw_json_shape() {
    let response = json!({
        "total": 1,
        "p": 1,
        "ps": 100,
        "paging": {"pageIndex": 1, "pageSize": 100, "total": 1},
        "issues": [{"key": "issue-1", "message": "bounded"}],
        "components": [{"key": "elitea:runtime"}]
    });
    let transport = Arc::new(FixtureTransport::with_response(Ok(response.clone())));
    let client =
        SonarClient::test_with_transport(config(&[]), transport.clone() as Arc<dyn SonarTransport>);
    let output = client
        .get_sonar_data("/api/issues/search", None)
        .await
        .expect("bounded Sonar response");
    assert_eq!(output, response);
    assert_eq!(transport.request_count(), 1);
}

#[tokio::test]
async fn response_issue_and_output_limits_fail_closed() {
    let too_many = Arc::new(FixtureTransport::with_response(Ok(json!({
        "issues": (0..2).map(|index| json!({"key": index})).collect::<Vec<_>>()
    }))));
    let client =
        SonarClient::test_with_transport(config(&[]), too_many.clone() as Arc<dyn SonarTransport>);
    let error = client
        .get_sonar_data("/api/issues/search", Some(r#"{"ps":1}"#))
        .await
        .expect_err("response larger than requested page must fail");
    assert_eq!(error.code(), SonarClientErrorCode::ResourceExhausted);
    assert_eq!(too_many.request_count(), 1);

    let oversized = Arc::new(FixtureTransport::with_response(Ok(json!({
        "issues": [],
        "payload": "x".repeat(512 * 1_024)
    }))));
    let client =
        SonarClient::test_with_transport(config(&[]), oversized as Arc<dyn SonarTransport>);
    let error = client
        .get_sonar_data("/api/issues/search", None)
        .await
        .expect_err("oversized projected response must fail");
    assert_eq!(error.code(), SonarClientErrorCode::ResourceExhausted);
    assert!(!error.retryable());
}

#[test]
fn status_taxonomy_is_stable_and_retryable_only_for_transient_failures() {
    for (status, code, retryable) in [
        (
            StatusCode::BAD_REQUEST,
            SonarClientErrorCode::InvalidInput,
            false,
        ),
        (
            StatusCode::UNAUTHORIZED,
            SonarClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            SonarClientErrorCode::Authorization,
            false,
        ),
        (StatusCode::NOT_FOUND, SonarClientErrorCode::NotFound, false),
        (
            StatusCode::TOO_MANY_REQUESTS,
            SonarClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::GATEWAY_TIMEOUT,
            SonarClientErrorCode::Timeout,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            SonarClientErrorCode::DependencyUnavailable,
            true,
        ),
    ] {
        let error = test_http_status(status).expect_err("Sonar HTTP failure");
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
    }
}

#[test]
fn invocation_scoped_clients_do_not_overwrite_each_others_credentials() {
    let first = SonarClient::new(
        SonarToolkitConfig::parse(&settings_with_token(
            &[],
            "first-token",
            "https://sonar.example.test",
        ))
        .expect("first Sonar config"),
    )
    .expect("first Sonar client");
    let second = SonarClient::new(
        SonarToolkitConfig::parse(&settings_with_token(
            &[],
            "second-token",
            "https://sonar.example.test",
        ))
        .expect("second Sonar config"),
    )
    .expect("second Sonar client");
    let first = first
        .test_request("/api/issues/search", None)
        .expect("first request");
    let second = second
        .test_request("/api/issues/search", None)
        .expect("second request");
    assert_ne!(
        first.headers().get(AUTHORIZATION),
        second.headers().get(AUTHORIZATION)
    );
}

#[derive(Default)]
struct FixtureSonarApi {
    calls: Mutex<Vec<(String, Option<String>)>>,
}

#[async_trait]
impl SonarApi for FixtureSonarApi {
    async fn get_sonar_data(
        &self,
        relative_url: &str,
        params: Option<&str>,
    ) -> Result<Value, SonarClientError> {
        self.calls
            .lock()
            .expect("Sonar API fixture lock")
            .push((relative_url.to_owned(), params.map(ToOwned::to_owned)));
        Ok(json!({"issues": []}))
    }
}

#[tokio::test]
async fn native_tool_preserves_schema_empty_selection_and_policy() {
    let api = Arc::new(FixtureSonarApi::default());
    let toolset = test_build_with_api(
        "quality",
        &[],
        &policy(&[]),
        &(api.clone() as Arc<dyn SonarApi>),
    )
    .expect("native Sonar toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Sonar tools");
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name(), "get_sonar_data");
    assert!(tools[0].is_read_only());
    assert!(tools[0].is_concurrency_safe());
    assert_eq!(
        tools[0]
            .parameters_schema()
            .and_then(|schema| schema.pointer("/properties/relative_url/enum/0").cloned()),
        Some(json!("/api/issues/search"))
    );
    let schema = tools[0]
        .parameters_schema()
        .expect("Sonar tool has an argument schema");
    let params_description = schema["properties"]["params"]["description"]
        .as_str()
        .expect("Sonar params description is text");
    assert!(params_description.contains("encoded as a JSON object string"));
    assert!(params_description.contains("severities"));
    assert!(params_description.contains("\"p\":1"));
    assert!(params_description.contains("bounded raw Sonar issue-search object"));
    tools[0]
        .execute(
            context(),
            json!({
                "relative_url": "/api/issues/search",
                "params": "{\"severities\":[\"CRITICAL\"]}"
            }),
        )
        .await
        .expect("Sonar tool call");
    assert_eq!(
        api.calls.lock().expect("Sonar API fixture lock").as_slice(),
        &[(
            "/api/issues/search".to_owned(),
            Some("{\"severities\":[\"CRITICAL\"]}".to_owned())
        )]
    );

    let blocked = test_build_with_api(
        "quality",
        &[],
        &policy(&[("sonar", &["get_sonar_data"])]),
        &(api.clone() as Arc<dyn SonarApi>),
    )
    .expect("policy-filtered Sonar toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        blocked
            .tools(readonly)
            .await
            .expect("blocked tools")
            .is_empty()
    );

    let calls = api.calls.lock().expect("Sonar API fixture lock").len();
    for invalid in [
        json!({}),
        json!({"relative_url": "/api/system/status"}),
        json!({"relative_url": "/api/issues/search", "params": {}}),
        json!({"relative_url": "/api/issues/search", "unknown": true}),
    ] {
        assert!(tools[0].execute(context(), invalid).await.is_err());
    }
    assert_eq!(
        api.calls.lock().expect("Sonar API fixture lock").len(),
        calls
    );
}

#[test]
fn unsupported_selection_fails_before_provider_use() {
    let Err(error) = build_sonar_read_only_toolset("quality", config(&["unknown"]), &policy(&[]))
    else {
        panic!("unknown Sonar tool must fail closed");
    };
    assert_eq!(error.code(), SonarToolsetErrorCode::UnsupportedSelection);
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn SonarApi> = Arc::new(FixtureSonarApi::default());
    let toolset =
        test_build_with_api("gate", &[], &policy(&[]), &api).expect("complete sonar toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("sonar tools");
    super::sdk_conformance::assert_sdk_conformance("sonar", &tools);
}
