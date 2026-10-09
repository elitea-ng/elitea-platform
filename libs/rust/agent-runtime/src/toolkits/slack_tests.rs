use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};
use tokio::sync::{Notify, Semaphore};

use super::families::slack::client::{
    SlackApi, SlackClient, SlackClientError, SlackClientErrorCode, SlackHttpResponse,
    SlackOperation, SlackTransport, test_map_http_status, test_map_slack_error,
};
use super::families::slack::config::{SlackConfigErrorCode, SlackToolkitConfig};
use super::families::slack::tools::{
    SlackToolsetErrorCode, build_slack_toolset, test_build_with_api,
};
use super::policy::ToolAdmissionPolicy;

fn settings(selected_tools: &[&str]) -> Map<String, Value> {
    json!({
        "slack_configuration": {
            "name": null,
            "slack_token": "xoxb-super-secret-token",
            "channel_id": "C12345678"
        },
        "selected_tools": selected_tools
    })
    .as_object()
    .cloned()
    .expect("Slack fixture settings are an object")
}

fn config(selected_tools: &[&str]) -> SlackToolkitConfig {
    SlackToolkitConfig::parse(&settings(selected_tools)).expect("valid Slack configuration")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Slack policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("slack-test").with_function_call_id("slack-call"))
}

#[test]
fn materialized_configuration_is_bounded_deduplicated_and_secret_safe() {
    let parsed = config(&["send_message", "send_message", "read_messages"]);
    assert_eq!(parsed.test_default_channel_id(), Some("C12345678"));
    assert_eq!(
        parsed.selected_tools(),
        [
            Box::<str>::from("send_message"),
            Box::<str>::from("read_messages")
        ]
    );
    let rendered = format!("{:?}", SlackToolkitConfig::parse(&settings(&[])).err());
    assert!(!rendered.contains("xoxb-super-secret-token"));
}

#[test]
fn malformed_credentials_channels_and_bounds_fail_closed() {
    for invalid in [
        json!({"slack_configuration":{"slack_token":null,"channel_id":"C1"}}),
        json!({"slack_configuration":{"slack_token":"","channel_id":"C1"}}),
        json!({"slack_configuration":{"slack_token":"token\nvalue","channel_id":"C1"}}),
        json!({"slack_configuration":{"slack_token":"token","channel_id":"#general"}}),
    ] {
        let Err(error) = SlackToolkitConfig::parse(
            invalid
                .as_object()
                .expect("invalid Slack fixture is an object"),
        ) else {
            panic!("invalid Slack configuration must fail");
        };
        assert_eq!(error.code(), SlackConfigErrorCode::InvalidConfiguration);
    }

    let mut oversized = settings(&[]);
    oversized
        .get_mut("slack_configuration")
        .and_then(Value::as_object_mut)
        .expect("nested Slack configuration")
        .insert(
            "slack_token".to_owned(),
            Value::String("x".repeat(16 * 1_024 + 1)),
        );
    let Err(error) = SlackToolkitConfig::parse(&oversized) else {
        panic!("oversized Slack token must fail");
    };
    assert_eq!(error.code(), SlackConfigErrorCode::ResourceExhausted);
}

#[derive(Clone)]
struct CapturedRequest {
    path: String,
    query: Vec<(String, String)>,
    body: Option<Value>,
    authorization: Option<String>,
    authorization_sensitive: bool,
    effect: bool,
}

type Handler = dyn Fn(&Request, bool) -> Result<SlackHttpResponse, SlackClientError> + Send + Sync;

struct FixtureTransport {
    requests: Mutex<Vec<CapturedRequest>>,
    handler: Box<Handler>,
}

impl FixtureTransport {
    fn new(
        handler: impl Fn(&Request, bool) -> Result<SlackHttpResponse, SlackClientError>
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
            .expect("Slack request fixture lock")
            .clone()
    }
}

#[async_trait]
impl SlackTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<SlackHttpResponse, SlackClientError> {
        let body = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .and_then(|bytes| serde_json::from_slice(bytes).ok());
        let authorization = request
            .headers()
            .get(AUTHORIZATION)
            .and_then(|value| value.to_str().ok())
            .map(ToOwned::to_owned);
        let authorization_sensitive = request
            .headers()
            .get(AUTHORIZATION)
            .is_some_and(reqwest::header::HeaderValue::is_sensitive);
        self.requests
            .lock()
            .expect("Slack request fixture lock")
            .push(CapturedRequest {
                path: request.url().path().to_owned(),
                query: request
                    .url()
                    .query_pairs()
                    .map(|(key, value)| (key.into_owned(), value.into_owned()))
                    .collect(),
                body,
                authorization,
                authorization_sensitive,
                effect,
            });
        (self.handler)(&request, effect)
    }
}

fn ok(body: Value) -> SlackHttpResponse {
    SlackHttpResponse::fixture(StatusCode::OK, body)
}

#[test]
fn every_slack_request_is_fixed_origin_bearer_bound_and_bounded() {
    let client = SlackClient::new(config(&[])).expect("Slack client");
    let cases = [
        (SlackOperation::SendMessage, Method::POST),
        (SlackOperation::ReadMessages, Method::GET),
        (SlackOperation::CreateChannel, Method::POST),
        (SlackOperation::ListChannelMembers, Method::GET),
        (SlackOperation::UserInfo, Method::GET),
        (SlackOperation::ListWorkspaceUsers, Method::GET),
        (SlackOperation::Invite, Method::POST),
        (SlackOperation::ListWorkspaceConversations, Method::GET),
    ];
    for (operation, method) in cases {
        let request = client
            .test_request(
                operation,
                &Map::from_iter([("channel".to_owned(), Value::String("C1".to_owned()))]),
            )
            .expect("Slack request");
        assert_eq!(request.method(), method);
        assert_eq!(
            request.url().origin().ascii_serialization(),
            "https://slack.com"
        );
        let authorization = request
            .headers()
            .get(AUTHORIZATION)
            .expect("Slack Authorization header");
        assert_eq!(
            authorization.to_str().ok(),
            Some("Bearer xoxb-super-secret-token")
        );
        assert!(authorization.is_sensitive());
        if method == Method::POST {
            assert_eq!(
                request
                    .headers()
                    .get(CONTENT_TYPE)
                    .and_then(|value| value.to_str().ok()),
                Some("application/json")
            );
        }
    }
}

#[tokio::test]
async fn send_and_history_preserve_results_without_redundant_auth_test() {
    let transport = Arc::new(FixtureTransport::new(|request, _| {
        match request.url().path() {
            "/api/chat.postMessage" => Ok(ok(json!({
                "ok":true,"channel":"D555","ts":"1712345678.000100"
            }))),
            "/api/conversations.history" => Ok(ok(json!({
                "ok":true,
                "messages":[
                    {"ts":"2.0","user":"U2","text":"new","bot_profile":{"name":"deploy"},"thread_ts":"1.0"},
                    {"ts":"1.0"}
                ]
            }))),
            _ => Err(SlackClientError::fixture(
                SlackClientErrorCode::InvalidInput,
                false,
            )),
        }
    }));
    let client = SlackClient::with_transport(config(&[]), transport.clone());
    assert_eq!(
        client
            .send_message("D555", "hello", Some("1712000000.000001"))
            .await
            .expect("send Slack message"),
        json!({
            "success":true,"channel_id":"D555","ts":"1712345678.000100",
            "thread_ts":"1712000000.000001"
        })
    );
    assert_eq!(
        client
            .read_messages("C12345678", 10)
            .await
            .expect("read Slack messages"),
        json!([
            {"ts":"2.0","user":"U2","message":"new","app_name":"deploy","thread_ts":"1.0"},
            {"ts":"1.0","user":"Undefined User","message":"No message","app_name":"No App Name"}
        ])
    );
    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    assert!(
        requests
            .iter()
            .all(|request| request.path != "/api/auth.test")
    );
    assert_eq!(
        requests[0].body.as_ref().and_then(|body| body.get("text")),
        Some(&json!("hello"))
    );
    assert_eq!(
        requests[1].query,
        [
            ("channel".to_owned(), "C12345678".to_owned()),
            ("limit".to_owned(), "10".to_owned())
        ]
    );
}

#[tokio::test]
async fn create_invite_workspace_projections_and_effect_flags_are_exact() {
    let transport = Arc::new(FixtureTransport::new(|request, effect| {
        match request.url().path() {
            "/api/conversations.create" => {
                assert!(effect);
                Ok(ok(json!({"ok":true,"channel":{"id":"CNEW"}})))
            }
            "/api/conversations.invite" => {
                assert!(effect);
                Ok(ok(
                    json!({"ok":true,"channel":{"id":"C12345678","name":"general"}}),
                ))
            }
            "/api/users.list" => Ok(ok(json!({
                "ok":true,
                "members":[{"id":"U1","name":"ada","is_bot":false,"profile":{"email":"ada@example.com","team":"T1"}}]
            }))),
            "/api/conversations.list" => Ok(ok(json!({
                "ok":true,
                "channels":[{"id":"C1","name":"general","is_channel":true,"shared_team_ids":["T1"]}]
            }))),
            _ => Err(SlackClientError::fixture(
                SlackClientErrorCode::InvalidInput,
                false,
            )),
        }
    }));
    let client = SlackClient::with_transport(config(&[]), transport.clone());
    assert_eq!(
        client
            .create_channel("release-status", true)
            .await
            .expect("create channel"),
        json!({"success":true,"channel_id":"CNEW"})
    );
    assert_eq!(
        client
            .invite_to_conversation("", &["U1".to_owned(), "U2".to_owned()])
            .await
            .expect("invite Slack users"),
        json!({"ok":true,"channel":{"id":"C12345678","name":"general"}})
    );
    assert_eq!(
        client
            .list_workspace_users()
            .await
            .expect("workspace users"),
        json!([{"id":"U1","name":"ada","is_bot":false,"email":"ada@example.com","team":"T1"}])
    );
    assert_eq!(
        client
            .list_workspace_conversations()
            .await
            .expect("workspace channels"),
        json!([{"id":"C1","name":"general","is_channel":true,"shared_team_ids":["T1"]}])
    );
    let requests = transport.requests();
    assert_eq!(requests.iter().filter(|request| request.effect).count(), 2);
    assert!(
        requests
            .iter()
            .all(|request| request.authorization_sensitive)
    );
    assert!(
        requests
            .iter()
            .all(|request| request.authorization.as_deref()
                == Some("Bearer xoxb-super-secret-token"))
    );
}

struct GatedMembersTransport {
    active: std::sync::atomic::AtomicUsize,
    peak: std::sync::atomic::AtomicUsize,
    started_count: std::sync::atomic::AtomicUsize,
    started: Notify,
    gate: Semaphore,
}

impl GatedMembersTransport {
    fn new() -> Self {
        Self {
            active: std::sync::atomic::AtomicUsize::new(0),
            peak: std::sync::atomic::AtomicUsize::new(0),
            started_count: std::sync::atomic::AtomicUsize::new(0),
            started: Notify::new(),
            gate: Semaphore::new(0),
        }
    }
}

#[async_trait]
impl SlackTransport for GatedMembersTransport {
    async fn execute(
        &self,
        request: Request,
        _effect: bool,
    ) -> Result<SlackHttpResponse, SlackClientError> {
        if request.url().path() == "/api/conversations.members" {
            return Ok(ok(json!({
                "ok":true,
                "members":["U9","U8","U7","U6","U5","U4","U3","U2","U1"]
            })));
        }
        if request.url().path() != "/api/users.info" {
            return Err(SlackClientError::fixture(
                SlackClientErrorCode::InvalidInput,
                false,
            ));
        }
        let id = request
            .url()
            .query_pairs()
            .find(|(key, _)| key == "user")
            .map(|(_, value)| value.into_owned())
            .ok_or_else(|| SlackClientError::fixture(SlackClientErrorCode::InvalidInput, false))?;
        let active = self
            .active
            .fetch_add(1, std::sync::atomic::Ordering::SeqCst)
            + 1;
        self.peak
            .fetch_max(active, std::sync::atomic::Ordering::SeqCst);
        self.started_count
            .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        self.started.notify_one();
        let permit = self.gate.acquire().await.map_err(|_| {
            SlackClientError::fixture(SlackClientErrorCode::DependencyUnavailable, true)
        })?;
        permit.forget();
        self.active
            .fetch_sub(1, std::sync::atomic::Ordering::SeqCst);
        Ok(ok(
            json!({"ok":true,"user":{"id":id,"name":format!("name-{id}")}}),
        ))
    }
}

async fn wait_for_started(transport: &GatedMembersTransport, expected: usize) {
    while transport
        .started_count
        .load(std::sync::atomic::Ordering::SeqCst)
        < expected
    {
        let notified = transport.started.notified();
        if transport
            .started_count
            .load(std::sync::atomic::Ordering::SeqCst)
            >= expected
        {
            break;
        }
        notified.await;
    }
}

#[tokio::test]
async fn channel_member_fanout_is_bounded_concurrent_and_returned_in_member_order() {
    let transport = Arc::new(GatedMembersTransport::new());
    let client = Arc::new(SlackClient::with_transport(config(&[]), transport.clone()));
    let task = tokio::spawn({
        let client = client.clone();
        async move { client.list_channel_users("").await }
    });
    tokio::time::timeout(Duration::from_secs(2), wait_for_started(&transport, 8))
        .await
        .expect("eight member lookups are admitted concurrently");
    assert_eq!(
        transport.active.load(std::sync::atomic::Ordering::SeqCst),
        8
    );
    transport.gate.add_permits(8);
    tokio::time::timeout(Duration::from_secs(2), wait_for_started(&transport, 9))
        .await
        .expect("ninth lookup starts after capacity returns");
    transport.gate.add_permits(1);
    let result = tokio::time::timeout(Duration::from_secs(2), task)
        .await
        .expect("bounded member lookup completes")
        .expect("member lookup task remains owned")
        .expect("channel members are projected");
    assert_eq!(
        result,
        json!([
            {"id":"U9","name":"name-U9"}, {"id":"U8","name":"name-U8"},
            {"id":"U7","name":"name-U7"}, {"id":"U6","name":"name-U6"},
            {"id":"U5","name":"name-U5"}, {"id":"U4","name":"name-U4"},
            {"id":"U3","name":"name-U3"}, {"id":"U2","name":"name-U2"},
            {"id":"U1","name":"name-U1"}
        ])
    );
    assert_eq!(transport.peak.load(std::sync::atomic::Ordering::SeqCst), 8);
}

#[test]
fn provider_and_http_taxonomy_is_stable_redacted_and_effect_aware() {
    for (provider, effect, code, retryable) in [
        (
            Some("invalid_auth"),
            false,
            SlackClientErrorCode::Authentication,
            false,
        ),
        (
            Some("missing_scope"),
            false,
            SlackClientErrorCode::Authorization,
            false,
        ),
        (
            Some("channel_not_found"),
            false,
            SlackClientErrorCode::NotFound,
            false,
        ),
        (
            Some("ratelimited"),
            false,
            SlackClientErrorCode::RateLimited,
            true,
        ),
        (
            Some("internal_error"),
            false,
            SlackClientErrorCode::DependencyUnavailable,
            true,
        ),
        (
            Some("internal_error"),
            true,
            SlackClientErrorCode::UnknownOutcome,
            false,
        ),
    ] {
        let error = test_map_slack_error(provider, effect);
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
    }
    for (status, effect, code, retryable) in [
        (
            StatusCode::UNAUTHORIZED,
            false,
            SlackClientErrorCode::Authentication,
            false,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            false,
            SlackClientErrorCode::RateLimited,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            false,
            SlackClientErrorCode::DependencyUnavailable,
            true,
        ),
        (
            StatusCode::SERVICE_UNAVAILABLE,
            true,
            SlackClientErrorCode::UnknownOutcome,
            false,
        ),
    ] {
        let error = test_map_http_status(status, effect);
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
    }
    let error = SlackClientError::fixture(SlackClientErrorCode::UnknownOutcome, false);
    let rendered = format!("{error:?} {error}");
    assert!(!rendered.contains("xoxb-super-secret-token"));
    assert!(!rendered.contains("provider_body"));
}

#[tokio::test]
async fn successful_effect_with_unprojectable_result_remains_unknown_outcome() {
    let transport = Arc::new(FixtureTransport::new(|request, _| {
        match request.url().path() {
            "/api/chat.postMessage" => Ok(ok(json!({"ok":true,"channel":"C1"}))),
            "/api/conversations.create" => Ok(ok(json!({"ok":true,"channel":{}}))),
            "/api/conversations.invite" => Ok(ok(json!({
                "ok":true,"oversized":"x".repeat(512 * 1_024)
            }))),
            _ => Err(SlackClientError::fixture(
                SlackClientErrorCode::InvalidInput,
                false,
            )),
        }
    }));
    let client = SlackClient::with_transport(config(&[]), transport);
    for error in [
        client
            .send_message("C1", "hello", None)
            .await
            .expect_err("missing post timestamp is ambiguous"),
        client
            .create_channel("release-status", false)
            .await
            .expect_err("missing created channel ID is ambiguous"),
        client
            .invite_to_conversation("C1", &["U1".to_owned()])
            .await
            .expect_err("oversized successful invite result is ambiguous"),
    ] {
        assert_eq!(error.code(), SlackClientErrorCode::UnknownOutcome);
        assert!(!error.retryable());
    }
}

#[derive(Default)]
struct FixtureSlackApi {
    calls: Mutex<Vec<Value>>,
}

impl FixtureSlackApi {
    fn record(&self, value: Value) -> Value {
        self.calls
            .lock()
            .expect("Slack API fixture lock")
            .push(value.clone());
        value
    }
}

#[async_trait]
impl SlackApi for FixtureSlackApi {
    async fn send_message(
        &self,
        channel_id: &str,
        message: &str,
        thread_ts: Option<&str>,
    ) -> Result<Value, SlackClientError> {
        Ok(self.record(json!({"tool":"send_message","channel_id":channel_id,"message":message,"thread_ts":thread_ts})))
    }
    async fn read_messages(
        &self,
        channel_id: &str,
        limit: usize,
    ) -> Result<Value, SlackClientError> {
        Ok(self.record(json!({"tool":"read_messages","channel_id":channel_id,"limit":limit})))
    }
    async fn create_channel(
        &self,
        channel_name: &str,
        is_private: bool,
    ) -> Result<Value, SlackClientError> {
        Ok(self.record(json!({"tool":"create_slack_channel","channel_name":channel_name,"is_private":is_private})))
    }
    async fn list_channel_users(&self, channel_id: &str) -> Result<Value, SlackClientError> {
        Ok(self.record(json!({"tool":"list_channel_users","channel_id":channel_id})))
    }
    async fn list_workspace_users(&self) -> Result<Value, SlackClientError> {
        Ok(self.record(json!({"tool":"list_workspace_users"})))
    }
    async fn invite_to_conversation(
        &self,
        channel_id: &str,
        user_ids: &[String],
    ) -> Result<Value, SlackClientError> {
        Ok(self.record(
            json!({"tool":"invite_to_conversation","channel_id":channel_id,"user_ids":user_ids}),
        ))
    }
    async fn list_workspace_conversations(&self) -> Result<Value, SlackClientError> {
        Ok(self.record(json!({"tool":"list_workspace_conversations"})))
    }
}

fn assert_model_contract(tools: &[Arc<dyn Tool>]) {
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        [
            "send_message",
            "read_messages",
            "create_slack_channel",
            "list_channel_users",
            "list_workspace_users",
            "invite_to_conversation",
            "list_workspace_conversations"
        ]
    );
    for (index, tool) in tools.iter().enumerate() {
        let read_only = matches!(index, 1 | 3 | 4 | 6);
        assert_eq!(tool.is_read_only(), read_only);
        assert_eq!(tool.is_concurrency_safe(), read_only);
        assert!(tool.description().contains("Toolkit: collaboration"));
        let schema = tool.parameters_schema().expect("Slack parameters schema");
        for property in schema["properties"]
            .as_object()
            .expect("Slack properties")
            .values()
        {
            assert!(
                property["description"]
                    .as_str()
                    .is_some_and(|value| !value.trim().is_empty())
            );
        }
    }
    assert!(tools[0].description().contains("not safe to retry"));
    assert!(tools[0].description().contains("mrkdwn"));
    assert!(tools[1].description().contains("1 through 15"));
    assert!(tools[2].description().contains("public or private"));
    assert!(tools[3].description().contains("bounded user lookup"));
    assert!(tools[4].description().contains("users:read.email"));
    assert!(
        tools[5]
            .description()
            .contains("changes channel membership")
    );
    assert!(tools[6].description().contains("public Slack channels"));

    let schemas = tools
        .iter()
        .map(|tool| tool.parameters_schema().expect("Slack schema"))
        .collect::<Vec<_>>();
    assert_eq!(schemas[0]["required"], json!(["message"]));
    assert_eq!(
        schemas[0]["properties"]["message"]["maxLength"],
        json!(40_000)
    );
    assert!(
        schemas[0]["properties"]["message"]["description"]
            .as_str()
            .is_some_and(|value| value.contains("unfurl"))
    );
    assert!(
        schemas[0]["properties"]["thread_ts"]["description"]
            .as_str()
            .is_some_and(|value| value.contains("1712345678.000100"))
    );
    assert_eq!(schemas[1]["properties"]["limit"]["default"], json!(10));
    assert_eq!(schemas[1]["properties"]["limit"]["maximum"], json!(15));
    assert_eq!(
        schemas[2]["properties"]["is_private"]["default"],
        json!(false)
    );
    assert!(
        schemas[2]["properties"]["channel_name"]["description"]
            .as_str()
            .is_some_and(|value| value.contains("release-status"))
    );
    assert_eq!(schemas[5]["properties"]["user_ids"]["minItems"], json!(1));
    assert_eq!(schemas[5]["properties"]["user_ids"]["maxItems"], json!(100));
}

#[tokio::test]
async fn all_seven_tools_preserve_order_metadata_arguments_and_policy() {
    let api = Arc::new(FixtureSlackApi::default());
    let api_trait: Arc<dyn SlackApi> = api.clone();
    let toolset = test_build_with_api("collaboration", &[], &policy(&[]), &api_trait)
        .expect("complete Slack toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Slack tools");
    assert_model_contract(&tools);
    tools[0]
        .execute(
            context(),
            json!({"message":"release shipped","channel_id":"C1"}),
        )
        .await
        .expect("send tool invocation");
    tools[5]
        .execute(context(), json!({"channel_id":"C1","user_ids":["U1","U2"]}))
        .await
        .expect("invite tool invocation");
    assert_eq!(api.calls.lock().expect("Slack calls").len(), 2);

    let blocked = test_build_with_api(
        "collaboration",
        &[],
        &policy(&[("slack", &["send_message"])]),
        &api_trait,
    )
    .expect("policy-filtered Slack toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        blocked
            .tools(readonly)
            .await
            .expect("filtered Slack tools")
            .iter()
            .all(|tool| tool.name() != "send_message")
    );
}

#[tokio::test]
async fn invalid_selection_and_arguments_fail_before_provider_use() {
    let api = Arc::new(FixtureSlackApi::default());
    let api_trait: Arc<dyn SlackApi> = api.clone();
    let selected = vec![
        "send_message".to_owned(),
        "invite_to_conversation".to_owned(),
    ];
    let toolset = test_build_with_api("collaboration", &selected, &policy(&[]), &api_trait)
        .expect("selected Slack tools");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("selected tools");
    for invalid in [
        json!({}),
        json!({"message":""}),
        json!({"message":"hello","thread_ts":"not-a-timestamp"}),
        json!({"message":"hello","extra":true}),
    ] {
        assert!(tools[0].execute(context(), invalid).await.is_err());
    }
    for invalid in [
        json!({"user_ids":[]}),
        json!({"user_ids":["U1","U1"]}),
        json!({"user_ids":["bad-user"]}),
    ] {
        assert!(tools[1].execute(context(), invalid).await.is_err());
    }
    assert!(api.calls.lock().expect("Slack calls").is_empty());

    let unknown = SlackToolkitConfig::parse(&settings(&["delete_workspace"]))
        .expect("bounded unknown Slack selection parses");
    let Err(error) = build_slack_toolset("collaboration", unknown, &policy(&[])) else {
        panic!("unknown Slack tool must fail");
    };
    assert_eq!(error.code(), SlackToolsetErrorCode::UnsupportedSelection);
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn SlackApi> = Arc::new(FixtureSlackApi::default());
    let toolset =
        test_build_with_api("gate", &[], &policy(&[]), &api).expect("complete slack toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("slack tools");
    super::sdk_conformance::assert_sdk_conformance("slack", &tools);
}
