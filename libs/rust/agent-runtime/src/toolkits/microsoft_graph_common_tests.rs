//! The shared Microsoft Graph helper against a mocked Graph: token lookup,
//! the interrupt, 401 then one refresh then one retry, 429 and 503 with
//! `Retry-After`, paging on the same host only, `$batch`, error mapping, the
//! request rules, and the Dataverse origin.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE, HeaderMap, HeaderName, HeaderValue};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Map, Value, json};
use zeroize::Zeroizing;

use super::delegated_auth::delegated_authorization_requirement;
use super::families::microsoft_graph_common::{
    BatchItem, BatchOutcome, Endpoint, GraphBody, GraphClient, GraphClientOptions, GraphError,
    GraphErrorKind, GraphRequest, GraphTransport, RawResponse, RequestRules, RequirementError,
    TokenLookupError, TokenRefresher, authorization_requirement, build_graph_url, graph_token_keys,
    lookup_access_token, normalize_path, query_pairs, split_graph_url,
};

// ---------------------------------------------------------------------------
// A mocked Graph
// ---------------------------------------------------------------------------

struct Reply {
    status: u16,
    headers: Vec<(&'static str, String)>,
    body: Vec<u8>,
}

fn json_reply(status: u16, body: &Value) -> Reply {
    Reply {
        status,
        headers: vec![("content-type", "application/json".to_owned())],
        body: serde_json::to_vec(body).expect("a JSON body"),
    }
}

fn reply_with(status: u16, name: &'static str, value: &str) -> Reply {
    Reply {
        status,
        headers: vec![(name, value.to_owned())],
        body: Vec::new(),
    }
}

struct Seen {
    method: Method,
    url: String,
    authorization: Option<String>,
    headers: HeaderMap,
    body: Vec<u8>,
}

struct MockGraph {
    replies: Mutex<VecDeque<Reply>>,
    seen: Mutex<Vec<Seen>>,
}

impl MockGraph {
    fn new(replies: Vec<Reply>) -> Arc<Self> {
        Arc::new(Self {
            replies: Mutex::new(replies.into()),
            seen: Mutex::new(Vec::new()),
        })
    }

    fn count(&self) -> usize {
        self.seen.lock().expect("seen").len()
    }

    fn with_seen<T>(&self, index: usize, read: impl FnOnce(&Seen) -> T) -> T {
        read(&self.seen.lock().expect("seen")[index])
    }
}

#[async_trait]
impl GraphTransport for MockGraph {
    async fn execute(&self, request: Request, max_body: usize) -> Result<RawResponse, GraphError> {
        self.seen.lock().expect("seen").push(Seen {
            method: request.method().clone(),
            url: request.url().to_string(),
            authorization: request
                .headers()
                .get(AUTHORIZATION)
                .and_then(|value| value.to_str().ok())
                .map(str::to_owned),
            headers: request.headers().clone(),
            body: request
                .body()
                .and_then(|body| body.as_bytes())
                .map(<[u8]>::to_vec)
                .unwrap_or_default(),
        });
        let reply = self
            .replies
            .lock()
            .expect("replies")
            .pop_front()
            .expect("the mock Graph ran out of replies");
        let mut headers = HeaderMap::new();
        for (name, value) in reply.headers {
            headers.insert(
                HeaderName::from_static(name),
                HeaderValue::from_str(&value).expect("a header value"),
            );
        }
        let oversize = reply.body.len() > max_body;
        Ok(RawResponse {
            status: StatusCode::from_u16(reply.status).expect("a status"),
            headers,
            body: if oversize { Vec::new() } else { reply.body },
            oversize,
        })
    }
}

struct FreshToken {
    token: Option<&'static str>,
    calls: AtomicUsize,
}

impl FreshToken {
    fn giving(token: Option<&'static str>) -> Arc<Self> {
        Arc::new(Self {
            token,
            calls: AtomicUsize::new(0),
        })
    }

    fn calls(&self) -> usize {
        self.calls.load(Ordering::SeqCst)
    }
}

#[async_trait]
impl TokenRefresher for FreshToken {
    async fn refresh(&self) -> Option<Zeroizing<String>> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.token.map(|token| Zeroizing::new(token.to_owned()))
    }
}

fn client_with(
    mock: &Arc<MockGraph>,
    refresher: Option<Arc<dyn TokenRefresher>>,
    options: GraphClientOptions,
) -> GraphClient {
    GraphClient::with_transport(
        Endpoint::graph(),
        Zeroizing::new("tok-1".to_owned()),
        refresher,
        Arc::<MockGraph>::clone(mock),
        options,
    )
}

fn client(mock: &Arc<MockGraph>) -> GraphClient {
    client_with(mock, None, GraphClientOptions::default())
}

fn graph_url(path: &str) -> Url {
    build_graph_url(path, "v1.0", &[]).expect("a Graph URL")
}

fn get(path: &str) -> GraphRequest {
    GraphRequest::get(graph_url(path))
}

fn tokens(value: &Value) -> Map<String, Value> {
    value.as_object().expect("an object").clone()
}

fn kind_of<T>(result: Result<T, GraphError>) -> GraphErrorKind {
    match result {
        Ok(_) => panic!("expected an error"),
        Err(error) => error.kind(),
    }
}

// ---------------------------------------------------------------------------
// Token lookup and the interrupt
// ---------------------------------------------------------------------------

#[test]
fn token_keys_are_most_specific_first_and_deduplicated() {
    let keys = graph_token_keys(
        Some("uuid-1"),
        "https://login.microsoftonline.com/tenant/",
        "https://login.microsoftonline.com/tenant",
    );
    assert_eq!(
        keys,
        [
            "uuid-1:https://login.microsoftonline.com/tenant/",
            "https://login.microsoftonline.com/tenant/",
            "uuid-1:https://login.microsoftonline.com/tenant",
            "https://login.microsoftonline.com/tenant",
        ]
    );
    let same = graph_token_keys(None, "https://a", "https://a");
    assert_eq!(same, ["https://a"]);
}

#[test]
fn lookup_finds_the_first_key_and_reads_an_access_token_only() {
    let keys = graph_token_keys(Some("u"), "https://a/t", "https://a/t");
    let map = tokens(&json!({
        "https://a/t": "plain-token",
        "u:https://a/t": {"access_token": "wins", "refresh_token": "never-read", "token": "other"},
    }));
    let found = lookup_access_token(&map, &keys)
        .expect("lookup")
        .expect("a token");
    assert_eq!(found.as_str(), "wins");

    let object_token = tokens(&json!({"https://a/t": {"token": "via-token"}}));
    let found = lookup_access_token(&object_token, &keys)
        .expect("lookup")
        .expect("a token");
    assert_eq!(found.as_str(), "via-token");

    assert!(
        lookup_access_token(&tokens(&json!({"https://other": "x"})), &keys)
            .expect("lookup")
            .is_none()
    );
}

#[test]
fn lookup_refuses_a_malformed_or_oversized_entry() {
    let keys = ["k".to_owned()];
    for bad in [
        json!(5),
        json!({"nope": "x"}),
        json!(""),
        json!("   "),
        json!("a\nb"),
        json!({"access_token": 3}),
    ] {
        let map = tokens(&json!({ "k": bad }));
        assert_eq!(
            lookup_access_token(&map, &keys).err(),
            Some(TokenLookupError::Invalid),
            "{map:?}"
        );
    }
    let huge = tokens(&json!({"k": "x".repeat(64 * 1_024 + 1)}));
    assert_eq!(
        lookup_access_token(&huge, &keys).err(),
        Some(TokenLookupError::TooLarge)
    );
}

fn configuration() -> Map<String, Value> {
    tokens(&json!({
        "client_id": "app-id",
        "client_secret": "S3CRET-VALUE",
        "refresh_token": "R3FRESH-VALUE",
        "configuration_uuid": "00000000-0000-4000-8000-000000000123",
        "scopes": ["Mail.Read", "offline_access"],
    }))
}

#[test]
fn the_requirement_names_the_authority_and_never_carries_a_secret() {
    let requirement = authorization_requirement(
        "mail",
        "outlook",
        "https://graph.microsoft.com".to_owned(),
        "https://login.microsoftonline.com/tenant",
        &configuration(),
    )
    .expect("a requirement");
    assert_eq!(requirement.toolkit_name(), "mail");
    assert_eq!(requirement.toolkit_type(), "outlook");
    assert_eq!(requirement.server_url(), "https://graph.microsoft.com");
    let servers = requirement
        .authorization_servers()
        .expect("authorization servers");
    assert!(servers.contains(&json!("https://login.microsoftonline.com/tenant")));
    let text = serde_json::to_string(&requirement).expect("serializes");
    for secret in ["S3CRET-VALUE", "R3FRESH-VALUE"] {
        assert!(!text.contains(secret), "{secret} leaked into {text}");
    }
}

#[test]
fn a_requirement_without_a_client_id_or_with_an_oversized_authority_is_refused() {
    let mut no_client = configuration();
    no_client.remove("client_id");
    assert_eq!(
        authorization_requirement("m", "outlook", "k".to_owned(), "https://a/t", &no_client).err(),
        Some(RequirementError::Invalid)
    );
    let long = format!("https://login.microsoftonline.com/{}", "t".repeat(9_000));
    assert_eq!(
        authorization_requirement("m", "outlook", "k".to_owned(), &long, &configuration()).err(),
        Some(RequirementError::TooLarge)
    );
}

#[tokio::test]
async fn a_rejected_token_with_no_way_to_refresh_is_the_interrupt() {
    let mock = MockGraph::new(vec![json_reply(
        401,
        &json!({"error": {"code": "InvalidAuthenticationToken", "message": "Access token has expired"}}),
    )]);
    let error = client(&mock)
        .request(get("/me"))
        .await
        .err()
        .expect("an error");
    assert_eq!(error.kind(), GraphErrorKind::AuthorizationRequired);
    assert_eq!(mock.count(), 1);
    let rendered = format!("{error} {error:?}");
    assert!(!rendered.contains("tok-1"), "the token is in {rendered}");

    let requirement = authorization_requirement(
        "mail",
        "outlook",
        "https://graph.microsoft.com".to_owned(),
        "https://login.microsoftonline.com/tenant",
        &configuration(),
    )
    .expect("a requirement");
    let adk = error.clone().into_adk(Some(&requirement));
    let decoded = delegated_authorization_requirement(&adk).expect("the interrupt");
    assert!(decoded.same_authority(&requirement));
    // Without a requirement it is a plain unauthorized tool failure.
    let plain = error.into_adk(None);
    assert!(delegated_authorization_requirement(&plain).is_none());
}

// ---------------------------------------------------------------------------
// 401: one refresh request to Main, then one retry
// ---------------------------------------------------------------------------

#[tokio::test]
async fn a_401_asks_main_once_and_retries_once_with_the_new_token() {
    let mock = MockGraph::new(vec![
        json_reply(401, &json!({})),
        json_reply(200, &json!({"id": "me"})),
    ]);
    let refresher = FreshToken::giving(Some("tok-2"));
    let client = client_with(
        &mock,
        Some(refresher.clone()),
        GraphClientOptions::default(),
    );
    let response = client
        .request(get("/me"))
        .await
        .expect("the retry succeeds");
    assert_eq!(response.status, 200);
    assert_eq!(refresher.calls(), 1);
    assert_eq!(mock.count(), 2);
    assert_eq!(
        mock.with_seen(0, |s| s.authorization.clone()).as_deref(),
        Some("Bearer tok-1")
    );
    assert_eq!(
        mock.with_seen(1, |s| s.authorization.clone()).as_deref(),
        Some("Bearer tok-2")
    );
}

#[tokio::test]
async fn a_second_401_is_the_interrupt_and_asks_main_no_more() {
    let mock = MockGraph::new(vec![
        json_reply(401, &json!({})),
        json_reply(401, &json!({})),
    ]);
    let refresher = FreshToken::giving(Some("tok-2"));
    let client = client_with(
        &mock,
        Some(refresher.clone()),
        GraphClientOptions::default(),
    );
    assert_eq!(
        kind_of(client.request(get("/me")).await),
        GraphErrorKind::AuthorizationRequired
    );
    assert_eq!(refresher.calls(), 1);
    assert_eq!(mock.count(), 2);
}

#[tokio::test]
async fn main_having_no_token_ends_in_the_interrupt_without_a_retry() {
    let mock = MockGraph::new(vec![json_reply(401, &json!({}))]);
    let refresher = FreshToken::giving(None);
    let client = client_with(
        &mock,
        Some(refresher.clone()),
        GraphClientOptions::default(),
    );
    assert_eq!(
        kind_of(client.request(get("/me")).await),
        GraphErrorKind::AuthorizationRequired
    );
    assert_eq!(refresher.calls(), 1);
    assert_eq!(mock.count(), 1);
}

#[tokio::test]
async fn the_new_token_is_used_for_later_requests() {
    let mock = MockGraph::new(vec![
        json_reply(401, &json!({})),
        json_reply(200, &json!({})),
        json_reply(200, &json!({})),
    ]);
    let client = client_with(
        &mock,
        Some(FreshToken::giving(Some("tok-2"))),
        GraphClientOptions::default(),
    );
    client.request(get("/me")).await.expect("first");
    client.request(get("/me/messages")).await.expect("second");
    assert_eq!(
        mock.with_seen(2, |s| s.authorization.clone()).as_deref(),
        Some("Bearer tok-2")
    );
}

// ---------------------------------------------------------------------------
// Throttling
// ---------------------------------------------------------------------------

#[tokio::test(start_paused = true)]
async fn a_429_waits_for_retry_after_then_succeeds() {
    let mock = MockGraph::new(vec![
        reply_with(429, "retry-after", "3"),
        json_reply(200, &json!({"ok": true})),
    ]);
    let start = tokio::time::Instant::now();
    let response = client(&mock).request(get("/me")).await.expect("succeeds");
    assert_eq!(response.status, 200);
    assert_eq!(mock.count(), 2);
    let waited = start.elapsed();
    assert!(
        waited >= Duration::from_secs(3) && waited < Duration::from_secs(4),
        "{waited:?}"
    );
}

#[tokio::test(start_paused = true)]
async fn retry_after_is_capped_at_thirty_seconds() {
    let mock = MockGraph::new(vec![
        reply_with(429, "retry-after", "120"),
        json_reply(200, &json!({})),
    ]);
    let start = tokio::time::Instant::now();
    client(&mock).request(get("/me")).await.expect("succeeds");
    let waited = start.elapsed();
    assert!(
        waited >= Duration::from_secs(30) && waited < Duration::from_secs(31),
        "{waited:?}"
    );
}

#[tokio::test(start_paused = true)]
async fn a_503_without_retry_after_backs_off_one_two_four_then_gives_up() {
    let mock = MockGraph::new(vec![
        json_reply(503, &json!({})),
        json_reply(503, &json!({})),
        json_reply(503, &json!({})),
        json_reply(503, &json!({})),
    ]);
    let start = tokio::time::Instant::now();
    let error = client(&mock)
        .request(get("/me"))
        .await
        .err()
        .expect("an error");
    assert_eq!(error.kind(), GraphErrorKind::Unavailable);
    assert!(error.retryable());
    assert_eq!(mock.count(), 4);
    let waited = start.elapsed();
    assert!(
        waited >= Duration::from_secs(7) && waited < Duration::from_secs(8),
        "{waited:?}"
    );
}

#[tokio::test(start_paused = true)]
async fn four_429s_end_throttled_and_say_how_long_to_wait() {
    let replies = (0..4)
        .map(|_| reply_with(429, "retry-after", "2"))
        .collect();
    let mock = MockGraph::new(replies);
    let error = client(&mock)
        .request(get("/me"))
        .await
        .err()
        .expect("an error");
    assert_eq!(error.kind(), GraphErrorKind::Throttled);
    assert_eq!(error.status(), Some(429));
    assert_eq!(error.retry_after(), Some(Duration::from_secs(2)));
    assert_eq!(mock.count(), 4);
    let adk = error.into_adk(None);
    assert!(adk.retry.should_retry);
    assert_eq!(adk.retry.retry_after_ms, Some(2_000));
}

// ---------------------------------------------------------------------------
// Paging on the same host only
// ---------------------------------------------------------------------------

fn page(values: &Value, next: Option<&str>) -> Reply {
    let mut body = json!({ "value": values });
    if let Some(next) = next {
        body["@odata.nextLink"] = json!(next);
    }
    json_reply(200, &body)
}

#[tokio::test]
async fn collect_follows_next_links_with_the_token_and_stops_at_the_end() {
    let next = "https://graph.microsoft.com/v1.0/me/messages?$skiptoken=abc";
    let mock = MockGraph::new(vec![
        page(&json!([1, 2]), Some(next)),
        page(&json!([3]), None),
    ]);
    let collected = client(&mock)
        .collect(graph_url("/me/messages"), 10, 100, None)
        .await
        .expect("collected");
    assert_eq!(collected.items, [json!(1), json!(2), json!(3)]);
    assert!(!collected.has_more);
    assert_eq!(mock.with_seen(1, |s| s.url.clone()), next);
    assert_eq!(
        mock.with_seen(1, |s| s.authorization.clone()).as_deref(),
        Some("Bearer tok-1")
    );
}

#[tokio::test]
async fn collect_filters_and_reports_when_it_stopped_early() {
    let even = |value: &Value| value.as_i64().is_some_and(|n| n % 2 == 0);
    let mock = MockGraph::new(vec![page(&json!([1, 2, 3, 4, 5]), None)]);
    let collected = client(&mock)
        .collect(graph_url("/me/messages"), 1, 100, Some(&even))
        .await
        .expect("collected");
    assert_eq!(collected.items, [json!(2)]);
    assert!(collected.has_more);

    let mock = MockGraph::new(vec![page(&json!([1, 2, 3, 4, 5]), None)]);
    let never = |_: &Value| false;
    let collected = client(&mock)
        .collect(graph_url("/me/messages"), 10, 3, Some(&never))
        .await
        .expect("collected");
    assert!(collected.items.is_empty());
    assert!(
        collected.has_more,
        "the scan cap stopped it with items unread"
    );
}

#[tokio::test]
async fn a_next_link_on_another_host_scheme_port_or_path_is_refused_without_a_request() {
    for link in [
        "https://evil.example/v1.0/me/messages?$skiptoken=x",
        "http://graph.microsoft.com/v1.0/me/messages",
        "https://graph.microsoft.com:8443/v1.0/me/messages",
        "https://user@graph.microsoft.com/v1.0/me/messages",
        "https://graph.microsoft.com.evil.example/v1.0/me",
        "https://graph.microsoft.com/other/me",
    ] {
        let mock = MockGraph::new(vec![page(&json!([1]), Some(link))]);
        let result = client(&mock)
            .collect(graph_url("/me/messages"), 10, 100, None)
            .await;
        assert_eq!(kind_of(result), GraphErrorKind::Refused, "{link}");
        assert_eq!(mock.count(), 1, "{link}: the link was followed");
    }
}

#[tokio::test]
async fn follow_pages_merges_up_to_the_page_count_and_keeps_the_rest_link() {
    let second = "https://graph.microsoft.com/v1.0/me/messages?$skiptoken=2";
    let third = "https://graph.microsoft.com/v1.0/me/messages?$skiptoken=3";
    let mock = MockGraph::new(vec![page(&json!([2]), Some(third))]);
    let mut first = Map::new();
    first.insert("@odata.context".to_owned(), json!("ctx"));
    first.insert("value".to_owned(), json!([1]));
    first.insert("@odata.nextLink".to_owned(), json!(second));
    let merged = client(&mock)
        .follow_pages(first, 1, 100_000, None)
        .await
        .expect("merged");
    assert_eq!(merged["value"], json!([1, 2]));
    assert_eq!(merged["@odata.context"], json!("ctx"));
    assert_eq!(merged["@odata.nextLink"], json!(third));
    assert_eq!(mock.with_seen(0, |s| s.url.clone()), second);
}

#[tokio::test]
async fn follow_pages_lets_the_toolkit_check_each_link() {
    let mock = MockGraph::new(vec![]);
    let mut first = Map::new();
    first.insert("value".to_owned(), json!([1]));
    first.insert(
        "@odata.nextLink".to_owned(),
        json!("https://graph.microsoft.com/v1.0/users"),
    );
    let deny = |_: &Url| -> Result<(), GraphError> {
        Err(GraphError::new(
            GraphErrorKind::Refused,
            "Path /users is denied by this toolkit",
        ))
    };
    let result = client(&mock)
        .follow_pages(first, 3, 100_000, Some(&deny))
        .await;
    assert_eq!(kind_of(result), GraphErrorKind::Refused);
    assert_eq!(mock.count(), 0);
}

// ---------------------------------------------------------------------------
// Redirects, hosts, headers and bodies
// ---------------------------------------------------------------------------

#[tokio::test]
async fn a_redirect_on_graph_is_followed_with_the_token() {
    let mock = MockGraph::new(vec![
        reply_with(302, "location", "/v1.0/me/photo/$value"),
        json_reply(200, &json!({})),
    ]);
    client(&mock)
        .request(get("/me/photo"))
        .await
        .expect("followed");
    assert_eq!(
        mock.with_seen(1, |s| s.url.clone()),
        "https://graph.microsoft.com/v1.0/me/photo/$value"
    );
    assert_eq!(
        mock.with_seen(1, |s| s.authorization.clone()).as_deref(),
        Some("Bearer tok-1")
    );
}

#[tokio::test]
async fn a_redirect_to_another_host_is_refused_and_not_followed() {
    let mock = MockGraph::new(vec![reply_with(
        302,
        "location",
        "https://contoso.sharepoint.com/_layouts/download.aspx?id=1",
    )]);
    let error = client(&mock)
        .request(get("/me/drive/root/content"))
        .await
        .err()
        .expect("an error");
    assert_eq!(error.kind(), GraphErrorKind::Refused);
    assert_eq!(mock.count(), 1);
    assert!(!error.to_string().contains("sharepoint"), "{error}");
}

#[tokio::test]
async fn a_303_turns_a_post_into_a_get_without_its_body() {
    let mock = MockGraph::new(vec![
        reply_with(303, "location", "https://graph.microsoft.com/v1.0/done"),
        json_reply(200, &json!({})),
    ]);
    let request = GraphRequest {
        method: Method::POST,
        url: graph_url("/me/sendMail"),
        headers: Vec::new(),
        body: GraphBody::Json(json!({"a": 1})),
    };
    client(&mock).request(request).await.expect("followed");
    assert_eq!(mock.with_seen(1, |s| s.method.clone()), Method::GET);
    assert!(mock.with_seen(1, |s| s.body.is_empty()));
}

#[tokio::test]
async fn six_redirects_are_too_many() {
    let replies = (0..6)
        .map(|_| reply_with(302, "location", "https://graph.microsoft.com/v1.0/loop"))
        .collect();
    let mock = MockGraph::new(replies);
    assert_eq!(
        kind_of(client(&mock).request(get("/me")).await),
        GraphErrorKind::Refused
    );
    assert_eq!(mock.count(), 6);
}

#[tokio::test]
async fn the_token_goes_only_to_the_endpoint() {
    let mock = MockGraph::new(vec![]);
    for url in [
        "https://evil.example/v1.0/me",
        "http://graph.microsoft.com/v1.0/me",
        "https://graph.microsoft.com/me",
        "https://contoso.crm.dynamics.com/api/data/v9.2/WhoAmI",
    ] {
        let request = GraphRequest::get(Url::parse(url).expect("a URL"));
        assert_eq!(
            kind_of(client(&mock).request(request).await),
            GraphErrorKind::Refused,
            "{url}"
        );
    }
    assert_eq!(mock.count(), 0);
}

#[tokio::test]
async fn reserved_headers_are_ignored_and_default_headers_apply() {
    let mock = MockGraph::new(vec![json_reply(200, &json!({}))]);
    let options = GraphClientOptions {
        default_headers: vec![("Prefer".to_owned(), "IdType=\"ImmutableId\"".to_owned())],
        ..GraphClientOptions::default()
    };
    let request = GraphRequest {
        method: Method::GET,
        url: graph_url("/me/messages"),
        headers: vec![
            ("Authorization".to_owned(), "Bearer evil".to_owned()),
            ("Cookie".to_owned(), "a=b".to_owned()),
            ("X-Test".to_owned(), "1".to_owned()),
        ],
        body: GraphBody::Empty,
    };
    client_with(&mock, None, options)
        .request(request)
        .await
        .expect("ok");
    assert_eq!(
        mock.with_seen(0, |s| s.authorization.clone()).as_deref(),
        Some("Bearer tok-1")
    );
    mock.with_seen(0, |s| {
        assert!(s.headers.get("cookie").is_none());
        assert_eq!(
            s.headers.get("x-test").and_then(|v| v.to_str().ok()),
            Some("1")
        );
        assert_eq!(
            s.headers.get("prefer").and_then(|v| v.to_str().ok()),
            Some("IdType=\"ImmutableId\"")
        );
    });
}

#[tokio::test]
async fn json_and_byte_bodies_carry_their_content_type() {
    let mock = MockGraph::new(vec![
        json_reply(200, &json!({})),
        json_reply(200, &json!({})),
    ]);
    let client = client(&mock);
    let json_request = GraphRequest {
        method: Method::POST,
        url: graph_url("/me/sendMail"),
        headers: Vec::new(),
        body: GraphBody::Json(json!({"message": {"subject": "hi"}})),
    };
    client.request(json_request).await.expect("json");
    let bytes_request = GraphRequest {
        method: Method::PUT,
        url: graph_url("/me/drive/root:/a.bin:/content"),
        headers: Vec::new(),
        body: GraphBody::Bytes {
            content: vec![1, 2, 3],
            content_type: "application/octet-stream".to_owned(),
        },
    };
    client.request(bytes_request).await.expect("bytes");
    mock.with_seen(0, |s| {
        assert_eq!(
            s.headers.get(CONTENT_TYPE).and_then(|v| v.to_str().ok()),
            Some("application/json")
        );
        let body: Value = serde_json::from_slice(&s.body).expect("JSON");
        assert_eq!(body["message"]["subject"], "hi");
    });
    mock.with_seen(1, |s| {
        assert_eq!(
            s.headers.get(CONTENT_TYPE).and_then(|v| v.to_str().ok()),
            Some("application/octet-stream")
        );
        assert_eq!(s.body, [1, 2, 3]);
    });
}

#[tokio::test]
async fn a_response_over_the_limit_is_refused() {
    let mock = MockGraph::new(vec![json_reply(200, &json!({"a": "0123456789"}))]);
    let options = GraphClientOptions {
        max_response_bytes: 8,
        ..GraphClientOptions::default()
    };
    let result = client_with(&mock, None, options).request(get("/me")).await;
    assert_eq!(kind_of(result), GraphErrorKind::ResourceExhausted);
}

#[tokio::test]
async fn a_success_exposes_the_selected_headers_and_the_body() {
    let mut reply = json_reply(200, &json!({"id": "1"}));
    reply.headers.push(("etag", "W/\"x\"".to_owned()));
    reply.headers.push(("set-cookie", "secret".to_owned()));
    let mock = MockGraph::new(vec![reply]);
    let response = client(&mock).request(get("/me")).await.expect("ok");
    assert!(response.is_json() && !response.is_text());
    assert_eq!(response.json().expect("json"), Some(json!({"id": "1"})));
    assert_eq!(
        response.headers.get("etag").map(String::as_str),
        Some("W/\"x\"")
    );
    assert!(!response.headers.contains_key("set-cookie"));
}

// ---------------------------------------------------------------------------
// Error mapping
// ---------------------------------------------------------------------------

async fn failure(status: u16, body: &Value) -> GraphError {
    let mock = MockGraph::new(vec![json_reply(status, body)]);
    client(&mock)
        .request(get("/me"))
        .await
        .err()
        .expect("an error")
}

#[tokio::test]
async fn a_403_names_the_missing_permission() {
    let body = json!({"error": {"code": "ErrorAccessDenied", "message": "Access is denied."}});
    let error = failure(403, &body).await;
    assert_eq!(error.kind(), GraphErrorKind::Forbidden);
    assert_eq!(error.status(), Some(403));
    let text = error.to_string();
    assert!(
        text.contains("HTTP 403: ErrorAccessDenied: Access is denied"),
        "{text}"
    );
    assert!(
        text.contains("lacks the Microsoft Graph permission"),
        "{text}"
    );
    let named = error.with_permission(Some("Mail.Read")).to_string();
    assert!(
        named.contains("needs the Microsoft Graph permission Mail.Read"),
        "{named}"
    );
    assert!(!named.contains("lacks the"), "{named}");
}

#[tokio::test]
async fn the_other_statuses_map_to_their_kinds() {
    let body = json!({"error": {"code": "X", "message": "m"}});
    for (status, kind) in [
        (400, GraphErrorKind::InvalidInput),
        (404, GraphErrorKind::NotFound),
        (409, GraphErrorKind::Conflict),
        (412, GraphErrorKind::Conflict),
        (500, GraphErrorKind::Unavailable),
        (502, GraphErrorKind::Unavailable),
        (418, GraphErrorKind::InvalidInput),
    ] {
        let error = failure(status, &body).await;
        assert_eq!(error.kind(), kind, "{status}");
        assert_eq!(error.status(), Some(status));
        assert!(error.to_string().contains("HTTP"), "{error}");
    }
    let not_found = failure(404, &body).await;
    assert!(
        not_found
            .to_string()
            .contains("do not retry with the same ID")
    );
    let hinted = not_found.with_not_found_hint(" Look it up with list_teams.");
    assert!(hinted.to_string().contains("list_teams"));
}

#[tokio::test]
async fn provider_text_is_cleaned_and_bounded() {
    let noisy =
        json!({"error": {"code": "C", "message": format!("a\u{7}b\nc{}", "z".repeat(2_000))}});
    let text = failure(400, &noisy).await.to_string();
    assert!(text.contains("C: abc"), "{text}");
    assert!(text.len() < 700, "{} bytes", text.len());
    // A body that is not Graph's error shape.
    let mock = MockGraph::new(vec![Reply {
        status: 400,
        headers: Vec::new(),
        body: b"<html>gateway</html>".to_vec(),
    }]);
    let error = client(&mock)
        .request(get("/me"))
        .await
        .err()
        .expect("an error");
    assert!(error.to_string().ends_with("no details"), "{error}");
}

// ---------------------------------------------------------------------------
// URL and path rules
// ---------------------------------------------------------------------------

#[test]
fn graph_urls_split_into_version_and_path() {
    let cases = [
        ("/me/messages", None, "/me/messages"),
        ("me/messages", None, "/me/messages"),
        ("v1.0/me", Some("v1.0"), "/me"),
        ("/BETA/users?x=1", Some("beta"), "/users"),
        (
            "https://graph.microsoft.com/v1.0/me/messages?$top=1",
            Some("v1.0"),
            "/me/messages",
        ),
        (
            "https://graph.microsoft.com:443/beta/me#frag",
            Some("beta"),
            "/me",
        ),
        ("/me//messages", None, "/me/messages"),
    ];
    for (input, version, path) in cases {
        let (got_version, got_path) = split_graph_url(input).expect(input);
        assert_eq!((got_version, got_path.as_str()), (version, path), "{input}");
    }
}

#[test]
fn only_graph_urls_are_accepted() {
    for bad in [
        "",
        "   ",
        "https://evil.example/v1.0/me",
        "http://graph.microsoft.com/v1.0/me",
        "https://user@graph.microsoft.com/v1.0/me",
        "https://graph.microsoft.com:8443/v1.0/me",
        "//graph.microsoft.com/v1.0/me",
        "ftp://graph.microsoft.com/v1.0/me",
    ] {
        assert!(split_graph_url(bad).is_err(), "{bad:?}");
    }
    assert_eq!(kind_of(split_graph_url("")), GraphErrorKind::InvalidInput);
}

#[test]
fn paths_are_normalized_for_matching_and_tricks_are_refused() {
    assert_eq!(normalize_path("/Me/Messages/").expect("ok"), "/me/messages");
    assert_eq!(normalize_path("/me/a%20b").expect("ok"), "/me/a b");
    assert_eq!(normalize_path("v1.0/ME").expect("ok"), "/me");
    for bad in ["/me/%2e%2e/x", "/me/../x", "/me/./x", "/me/%252e"] {
        assert!(normalize_path(bad).is_err(), "{bad}");
    }
}

#[test]
fn a_graph_url_is_built_with_the_version_and_an_encoded_query() {
    let url = build_graph_url(
        "/me/messages",
        "v1.0",
        &[
            ("$top".to_owned(), "5".to_owned()),
            ("$filter".to_owned(), "isRead eq false".to_owned()),
        ],
    )
    .expect("a URL");
    assert_eq!(url.host_str(), Some("graph.microsoft.com"));
    assert_eq!(url.path(), "/v1.0/me/messages");
    assert_eq!(url.query(), Some("%24top=5&%24filter=isRead+eq+false"));
    // A URL's own version wins over the argument.
    let beta = build_graph_url("https://graph.microsoft.com/beta/me", "v1.0", &[]).expect("a URL");
    assert_eq!(beta.path(), "/beta/me");
    assert!(build_graph_url("/me", "v2", &[]).is_err());

    let pairs =
        query_pairs(&tokens(&json!({"$top": 5, "$select": "a,b", "flag": true}))).expect("pairs");
    assert!(pairs.contains(&("$top".to_owned(), "5".to_owned())));
    assert!(pairs.contains(&("flag".to_owned(), "true".to_owned())));
    assert!(query_pairs(&tokens(&json!({"x": [1]}))).is_err());
}

fn strings(values: &[&str]) -> Vec<String> {
    values.iter().map(|value| (*value).to_owned()).collect()
}

#[test]
fn the_rules_allow_get_by_default_and_narrow_by_method_and_path() {
    let default = RequestRules::new(None, &[], &[]).expect("rules");
    default
        .check("GET", "/me/messages")
        .expect("GET is allowed");
    default.check("", "/me").expect("an empty method is GET");
    let post = default
        .check("POST", "/me/sendMail")
        .err()
        .expect("POST is refused");
    assert!(
        post.to_string()
            .contains("not allowed by this toolkit (allowed: GET)"),
        "{post}"
    );
    let trace = default
        .check("TRACE", "/me")
        .err()
        .expect("TRACE is refused");
    assert!(
        trace.to_string().contains("Unsupported HTTP method TRACE"),
        "{trace}"
    );
    assert!(default.describe().contains("Allowed methods: GET."));

    let rules = RequestRules::new(
        Some(&strings(&["get", "post"])),
        &strings(&["/me/messages", "/users/*/events"]),
        &strings(&["/me/messages/delete"]),
    )
    .expect("rules");
    for ok in [
        "/me/messages",
        "/me/messages/123",
        "ME/Messages/123",
        "/v1.0/users/42/events",
        "/users/a b/events/9",
    ] {
        rules
            .check("GET", ok)
            .unwrap_or_else(|error| panic!("{ok}: {error}"));
    }
    for (method, path) in [
        ("GET", "/me/messagesX"),
        ("GET", "/me/calendar"),
        ("GET", "/users/42/calendar"),
        ("GET", "/users/events"),
        ("POST", "/me/messages/delete"),
        ("PUT", "/me/messages"),
    ] {
        assert!(rules.check(method, path).is_err(), "{method} {path}");
    }
    let denied = rules
        .check("POST", "/me/messages/delete")
        .err()
        .expect("denied");
    assert!(
        denied
            .to_string()
            .contains("denied by this toolkit (rule: /me/messages/delete)"),
        "{denied}"
    );
    let outside = rules.check("GET", "/me/calendar").err().expect("outside");
    assert!(
        outside
            .to_string()
            .contains("allowed paths: /me/messages, /users/*/events"),
        "{outside}"
    );
    assert!(
        rules
            .describe()
            .contains("Denied paths: /me/messages/delete.")
    );
    // A path that escapes by encoding never matches an allowed prefix.
    assert!(rules.check("GET", "/me/messages/%2e%2e/calendar").is_err());
}

// ---------------------------------------------------------------------------
// $batch
// ---------------------------------------------------------------------------

fn item(id: &str, method: &str, url: &str) -> BatchItem {
    BatchItem {
        id: id.to_owned(),
        method: method.to_owned(),
        url: url.to_owned(),
        body: None,
        headers: std::collections::BTreeMap::new(),
        depends_on: Vec::new(),
    }
}

#[tokio::test]
async fn a_batch_is_one_post_and_each_answer_comes_back_by_id() {
    let mock = MockGraph::new(vec![json_reply(
        200,
        &json!({"responses": [
            {"id": "2", "status": 404, "body": {"error": {"code": "NotFound"}}},
            {"id": "1", "status": 200, "body": {"subject": "hi"}},
        ]}),
    )]);
    let mut second = item("2", "post", "me/messages");
    second.body = Some(json!({"subject": "x"}));
    second.depends_on = vec!["1".to_owned()];
    let outcomes = client(&mock)
        .batch(&[item("1", "", "/me/messages/abc"), second], "v1.0", None)
        .await
        .expect("batch");
    assert_eq!(
        outcomes,
        [
            BatchOutcome {
                id: "2".to_owned(),
                status: 404,
                body: json!({"error": {"code": "NotFound"}})
            },
            BatchOutcome {
                id: "1".to_owned(),
                status: 200,
                body: json!({"subject": "hi"})
            },
        ]
    );
    assert_eq!(mock.count(), 1);
    assert_eq!(mock.with_seen(0, |s| s.method.clone()), Method::POST);
    assert_eq!(
        mock.with_seen(0, |s| s.url.clone()),
        "https://graph.microsoft.com/v1.0/$batch"
    );
    let sent: Value = mock.with_seen(0, |s| serde_json::from_slice(&s.body).expect("JSON"));
    assert_eq!(
        sent["requests"][0],
        json!({"id": "1", "method": "GET", "url": "/me/messages/abc"})
    );
    assert_eq!(
        sent["requests"][1],
        json!({
            "id": "2", "method": "POST", "url": "/me/messages", "body": {"subject": "x"},
            "headers": {"Content-Type": "application/json"}, "dependsOn": ["1"],
        })
    );
}

#[tokio::test]
async fn a_bad_batch_is_refused_before_any_request() {
    let mock = MockGraph::new(vec![]);
    let client = client(&mock);
    let rules = RequestRules::new(None, &strings(&["/me"]), &[]).expect("rules");
    let too_many: Vec<BatchItem> = (0..21)
        .map(|n| item(&n.to_string(), "GET", "/me"))
        .collect();
    let cases: Vec<(Vec<BatchItem>, Option<&RequestRules>)> = vec![
        (vec![], None),
        (too_many, None),
        (vec![item("1", "GET", "/me"), item("1", "GET", "/me")], None),
        (
            vec![item("1", "GET", "https://graph.microsoft.com/v1.0/me")],
            None,
        ),
        (vec![item("1", "POST", "/$batch")], None),
        (vec![item("1", "POST", "/me/sendMail")], Some(&rules)),
        (vec![item("1", "GET", "/users/42")], Some(&rules)),
    ];
    for (items, rules) in cases {
        let result = client.batch(&items, "v1.0", rules).await;
        assert!(result.is_err(), "{items:?}");
    }
    assert_eq!(mock.count(), 0);
}

#[tokio::test]
async fn a_batch_answer_without_responses_is_an_invalid_response() {
    let mock = MockGraph::new(vec![json_reply(200, &json!({"value": []}))]);
    let result = client(&mock)
        .batch(&[item("1", "GET", "/me")], "v1.0", None)
        .await;
    assert_eq!(kind_of(result), GraphErrorKind::InvalidResponse);
}

// ---------------------------------------------------------------------------
// The Dataverse origin
// ---------------------------------------------------------------------------

#[test]
fn a_dataverse_endpoint_is_the_credentials_own_dynamics_origin() {
    for ok in [
        "https://contoso.crm.dynamics.com",
        "https://contoso.crm4.dynamics.com/",
        "  https://Contoso.crm.dynamics.com  ",
    ] {
        let endpoint = Endpoint::dataverse(ok).expect(ok);
        assert!(endpoint.host().ends_with(".dynamics.com"));
    }
    for bad in [
        "https://contoso.crm.dynamics.cn",
        "https://contoso.crm.microsoftdynamics.us",
        "https://evil.example",
        "https://dynamics.com",
        "https://.dynamics.com",
        "http://contoso.crm.dynamics.com",
        "https://contoso.crm.dynamics.com:8443",
        "https://contoso.crm.dynamics.com/api/data",
        "https://user@contoso.crm.dynamics.com",
        "https://contoso.crm.dynamics.com?x=1",
        "contoso.crm.dynamics.com",
        "",
    ] {
        assert_eq!(
            kind_of(Endpoint::dataverse(bad)),
            GraphErrorKind::Refused,
            "{bad:?}"
        );
    }
}

#[tokio::test]
async fn the_dataverse_client_sends_its_token_to_the_org_and_nowhere_else() {
    let mock = MockGraph::new(vec![json_reply(200, &json!({"UserId": "u"}))]);
    let client = GraphClient::with_transport(
        Endpoint::dataverse("https://contoso.crm.dynamics.com").expect("an org"),
        Zeroizing::new("dv-token".to_owned()),
        None,
        Arc::<MockGraph>::clone(&mock),
        GraphClientOptions::default(),
    );
    let org = Url::parse("https://contoso.crm.dynamics.com/api/data/v9.2/WhoAmI").expect("a URL");
    client.request(GraphRequest::get(org)).await.expect("ok");
    assert_eq!(
        mock.with_seen(0, |s| s.authorization.clone()).as_deref(),
        Some("Bearer dv-token")
    );
    for other in [
        "https://graph.microsoft.com/v1.0/me",
        "https://other.crm.dynamics.com/api/data/v9.2/WhoAmI",
        "https://contoso.crm.dynamics.com/other",
    ] {
        let request = GraphRequest::get(Url::parse(other).expect("a URL"));
        assert_eq!(
            kind_of(client.request(request).await),
            GraphErrorKind::Refused,
            "{other}"
        );
    }
    assert_eq!(mock.count(), 1);
}
