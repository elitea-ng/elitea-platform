//! Shared fixtures and transport tests for the Zephyr REST families
//! (`zephyr_enterprise`, `zephyr_essential`, `zephyr_scale`).

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_core::ReadonlyContext;
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Value, json};

use super::families::zephyr_rest::client::{
    ZephyrReply, ZephyrRestClient, ZephyrRestError, ZephyrRestErrorCode, ZephyrRestResponse,
    ZephyrRestTransport,
};
use super::families::zephyr_rest::config::{
    ZephyrRestConfigErrorCode, bearer_token, parse_https_base, selected_tools,
};
use super::policy::ToolAdmissionPolicy;

pub(super) const TOKEN: &str = "zephyr-test-bearer-token";

#[derive(Clone, Debug)]
pub(super) struct RecordedRequest {
    pub(super) method: Method,
    pub(super) url: String,
    pub(super) authorization: Option<String>,
    pub(super) body: Option<Value>,
}

/// Replays canned provider responses and records every request it is sent.
pub(super) struct FixtureTransport {
    responses: Mutex<VecDeque<Result<ZephyrRestResponse, ZephyrRestError>>>,
    requests: Mutex<Vec<RecordedRequest>>,
}

impl FixtureTransport {
    pub(super) fn new(
        responses: impl IntoIterator<Item = Result<ZephyrRestResponse, ZephyrRestError>>,
    ) -> Arc<Self> {
        Arc::new(Self {
            responses: Mutex::new(responses.into_iter().collect()),
            requests: Mutex::new(Vec::new()),
        })
    }

    /// JSON 200 responses, in order.
    pub(super) fn json(values: impl IntoIterator<Item = Value>) -> Arc<Self> {
        Self::new(
            values
                .into_iter()
                .map(|value| Ok(ZephyrRestResponse::json(StatusCode::OK, &value))),
        )
    }

    pub(super) fn requests(&self) -> Vec<RecordedRequest> {
        self.requests.lock().expect("Zephyr request log").clone()
    }

    pub(super) fn urls(&self) -> Vec<String> {
        self.requests()
            .into_iter()
            .map(|request| format!("{} {}", request.method, request.url))
            .collect()
    }
}

#[async_trait]
impl ZephyrRestTransport for FixtureTransport {
    async fn execute(&self, request: Request) -> Result<ZephyrRestResponse, ZephyrRestError> {
        self.requests
            .lock()
            .expect("Zephyr request log")
            .push(RecordedRequest {
                method: request.method().clone(),
                url: request.url().to_string(),
                authorization: request
                    .headers()
                    .get(reqwest::header::AUTHORIZATION)
                    .and_then(|value| value.to_str().ok())
                    .map(str::to_owned),
                body: request
                    .body()
                    .and_then(reqwest::Body::as_bytes)
                    .map(|bytes| serde_json::from_slice(bytes).expect("JSON request body")),
            });
        self.responses
            .lock()
            .expect("Zephyr response queue")
            .pop_front()
            .unwrap_or_else(|| {
                Err(ZephyrRestError::fixture(
                    ZephyrRestErrorCode::InvalidResponse,
                    false,
                ))
            })
    }
}

pub(super) fn rest_client(base: &str, transport: &Arc<FixtureTransport>) -> ZephyrRestClient {
    let transport: Arc<dyn ZephyrRestTransport> = Arc::clone(transport) as _;
    ZephyrRestClient::with_transport(
        parse_https_base(base).expect("fixture base URL"),
        TOKEN,
        transport,
    )
}

pub(super) fn policy(blocked: &[(&str, &[&str])]) -> Arc<ToolAdmissionPolicy> {
    let blocked = blocked
        .iter()
        .map(|(toolkit, tools)| {
            (
                (*toolkit).to_owned(),
                tools.iter().map(|tool| (*tool).to_owned()).collect(),
            )
        })
        .collect::<BTreeMap<_, _>>();
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Zephyr test policy"))
}

pub(super) fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("zephyr-rest-test")
            .with_session_id("session-1")
            .with_function_call_id("zephyr-call-1"),
    )
}

pub(super) fn readonly() -> Arc<dyn ReadonlyContext> {
    context()
}

pub(super) fn tool_context() -> Arc<dyn adk_core::ToolContext> {
    context()
}

#[test]
fn base_urls_are_https_authorities_with_an_optional_prefix() {
    let base = parse_https_base(" https://api.zephyrscale.smartbear.com/v2/ ").expect("base");
    assert_eq!(base.as_str(), "https://api.zephyrscale.smartbear.com/v2");
    let root = parse_https_base("https://zephyr.example.test").expect("root base");
    assert_eq!(root.host_str(), Some("zephyr.example.test"));
    for invalid in [
        "http://zephyr.example.test",
        "https://user:pass@zephyr.example.test",
        "https://zephyr.example.test/?x=1",
        "https://zephyr.example.test/#f",
        "https://zephyr.example.test/%2e%2e",
        "zephyr.example.test",
        "",
    ] {
        let error = parse_https_base(invalid).expect_err("invalid base must fail");
        assert_eq!(
            error.code(),
            ZephyrRestConfigErrorCode::InvalidConfiguration
        );
        assert!(!format!("{error:?} {error}").contains("zephyr.example"));
    }
}

#[test]
fn tokens_are_header_safe_and_selections_deduplicate() {
    let object = json!({"token":"abc def"});
    let error = bearer_token(object.as_object().expect("object"), "token")
        .expect_err("a token with a space is not a header-safe bearer");
    assert_eq!(
        error.code(),
        ZephyrRestConfigErrorCode::InvalidConfiguration
    );
    let object = json!({"token":"t".repeat(16 * 1_024 + 1)});
    let error =
        bearer_token(object.as_object().expect("object"), "token").expect_err("oversized token");
    assert_eq!(error.code(), ZephyrRestConfigErrorCode::ResourceExhausted);

    let settings = json!({"selected_tools":["a","b","a"]});
    assert_eq!(
        selected_tools(settings.as_object().expect("object")).expect("selection"),
        vec![Box::<str>::from("a"), Box::<str>::from("b")]
    );
    let settings = json!({"selected_tools":null});
    assert!(
        selected_tools(settings.as_object().expect("object"))
            .expect("null selection")
            .is_empty()
    );
}

#[test]
fn identifiers_are_encoded_segments_and_cannot_escape_the_prefix() {
    let transport = FixtureTransport::new([]);
    let client = rest_client("https://zephyr.example.test/v2", &transport);
    let request = client
        .test_request(
            Method::GET,
            &["testcases", "PROJ-T1/../../admin?x=1#f"],
            &[("projectKey", "A&B".to_owned())],
            None,
        )
        .expect("encoded request");
    assert_eq!(
        request.url().as_str(),
        "https://zephyr.example.test/v2/testcases/PROJ-T1%2F..%2F..%2Fadmin%3Fx=1%23f?projectKey=A%26B"
    );
    assert_eq!(
        request
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok()),
        Some("Bearer zephyr-test-bearer-token")
    );
    assert!(
        request
            .headers()
            .get(reqwest::header::AUTHORIZATION)
            .is_some_and(reqwest::header::HeaderValue::is_sensitive)
    );
    assert!(!format!("{request:?}").contains(TOKEN));
    for segment in ["..", ".", "a\nb"] {
        let error = client
            .test_request(Method::GET, &["testcases", segment], &[], None)
            .expect_err("dot or control segment");
        assert_eq!(error.code(), ZephyrRestErrorCode::InvalidInput);
    }
    let trailing = client
        .test_request(Method::POST, &["testcase", ""], &[], Some(&json!({})))
        .expect("trailing slash route");
    assert_eq!(
        trailing.url().as_str(),
        "https://zephyr.example.test/v2/testcase/"
    );
}

#[test]
fn next_page_links_contribute_only_their_query() {
    let transport = FixtureTransport::new([]);
    let client = rest_client("https://zephyr.example.test/v2", &transport);
    let query = client
        .next_page_query(
            "https://evil.example.test/v2/testcases?maxResults=10&startAt=10&projectKey=",
        )
        .expect("next query");
    assert_eq!(
        query,
        vec![
            ("maxResults".to_owned(), "10".to_owned()),
            ("startAt".to_owned(), "10".to_owned())
        ]
    );
}

async fn get(client: &ZephyrRestClient) -> Result<ZephyrReply, ZephyrRestError> {
    client.call(Method::GET, &["healthcheck"], &[], None).await
}

#[tokio::test]
async fn replies_decode_like_the_sdk_clients() {
    let transport = FixtureTransport::new([
        Ok(ZephyrRestResponse::json(StatusCode::OK, &json!({"id":1}))),
        Ok(ZephyrRestResponse::fixture(
            StatusCode::OK,
            Some("text/plain"),
            b"plain text",
        )),
        Ok(ZephyrRestResponse::fixture(
            StatusCode::NO_CONTENT,
            None,
            b"",
        )),
        Ok(ZephyrRestResponse::fixture(
            StatusCode::OK,
            Some("application/json"),
            b"{not json",
        )),
    ]);
    let client = rest_client("https://zephyr.example.test/v2", &transport);
    assert_eq!(
        get(&client).await.expect("json"),
        ZephyrReply::Json(json!({"id":1}))
    );
    assert_eq!(
        get(&client).await.expect("text"),
        ZephyrReply::Text("plain text".to_owned())
    );
    assert_eq!(get(&client).await.expect("empty"), ZephyrReply::Empty);
    assert_eq!(
        get(&client).await.expect_err("bad json").code(),
        ZephyrRestErrorCode::InvalidResponse
    );
}

#[tokio::test]
async fn statuses_map_safely_and_effects_stay_ambiguous() {
    for (status, code, retryable) in [
        (
            StatusCode::BAD_REQUEST,
            ZephyrRestErrorCode::InvalidInput,
            false,
        ),
        (
            StatusCode::UNAUTHORIZED,
            ZephyrRestErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::FORBIDDEN,
            ZephyrRestErrorCode::Authorization,
            false,
        ),
        (StatusCode::NOT_FOUND, ZephyrRestErrorCode::NotFound, false),
        (
            StatusCode::TOO_MANY_REQUESTS,
            ZephyrRestErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            ZephyrRestErrorCode::DependencyUnavailable,
            true,
        ),
    ] {
        let transport = FixtureTransport::new([Ok(ZephyrRestResponse::fixture(
            status,
            Some("application/json"),
            br#"{"message":"provider-body"}"#,
        ))]);
        let client = rest_client("https://zephyr.example.test/v2", &transport);
        let error = client
            .call(Method::GET, &["projects"], &[], None)
            .await
            .expect_err("status must fail");
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
        assert!(!format!("{error:?} {error}").contains("provider-body"));
    }
    for status in [
        StatusCode::SERVICE_UNAVAILABLE,
        StatusCode::TOO_MANY_REQUESTS,
    ] {
        let transport = FixtureTransport::new([Ok(ZephyrRestResponse::fixture(status, None, b""))]);
        let client = rest_client("https://zephyr.example.test/v2", &transport);
        let error = client
            .call(Method::POST, &["testcases"], &[], Some(&json!({})))
            .await
            .expect_err("effect status");
        assert_eq!(error.code(), ZephyrRestErrorCode::UnknownOutcome);
        assert!(!error.retryable());
        // A read-only POST (a search) is an ordinary read failure.
        let transport = FixtureTransport::new([Ok(ZephyrRestResponse::fixture(status, None, b""))]);
        let client = rest_client("https://zephyr.example.test/v2", &transport);
        let error = client
            .post_read(&["advancesearch", "zql"], &json!({}))
            .await
            .expect_err("search status");
        assert_ne!(error.code(), ZephyrRestErrorCode::UnknownOutcome);
    }
}

#[test]
fn two_clients_never_share_origin_or_token() {
    let transport = FixtureTransport::new([]);
    let first = rest_client("https://first.example.test/v2", &transport);
    let second = ZephyrRestClient::with_transport(
        Url::parse("https://second.example.test/api").expect("second base"),
        "second-token",
        Arc::clone(&transport) as Arc<dyn ZephyrRestTransport>,
    );
    let first = first
        .test_request(Method::GET, &["projects"], &[], None)
        .expect("first request");
    let second = second
        .test_request(Method::GET, &["projects"], &[], None)
        .expect("second request");
    assert_eq!(first.url().host_str(), Some("first.example.test"));
    assert_eq!(second.url().host_str(), Some("second.example.test"));
    assert_ne!(
        first.headers().get(reqwest::header::AUTHORIZATION),
        second.headers().get(reqwest::header::AUTHORIZATION)
    );
}
