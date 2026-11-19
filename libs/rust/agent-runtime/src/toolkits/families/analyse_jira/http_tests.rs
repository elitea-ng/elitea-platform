//! The analysis reads through the real Jira client over a scripted transport:
//! the URLs they ask for, and what a 401, a 403, a 429, a 4xx refusal and a
//! hostile body come back as.

#![allow(clippy::needless_pass_by_value, clippy::unnecessary_wraps)]

use std::collections::VecDeque;
use std::sync::{Arc, Mutex};

use adk_core::{Tool, ToolContext};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::{Request, StatusCode};
use serde_json::{Map, Value, json};

use crate::toolkits::attachment_test_support::FakePlatform;
use crate::toolkits::families::jira::client::{
    JiraClient, JiraClientError, JiraClientErrorCode, JiraHttpResponse, JiraRead, JiraRoot,
    JiraTransport,
};
use crate::toolkits::families::jira::config::JiraToolkitConfig;

use super::config::AnalysisSettings;
use super::extract::JiraReader;
use super::tools::{AnalyseTool, Kind, Runner};

/// Answers in order and remembers the URLs it was asked for.
struct Scripted {
    responses: Mutex<VecDeque<JiraHttpResponse>>,
    urls: Mutex<Vec<String>>,
}

impl Scripted {
    fn new(responses: Vec<JiraHttpResponse>) -> Arc<Self> {
        Arc::new(Self {
            responses: Mutex::new(responses.into()),
            urls: Mutex::new(Vec::new()),
        })
    }

    fn urls(&self) -> Vec<String> {
        self.urls.lock().expect("urls").clone()
    }
}

#[async_trait]
impl JiraTransport for Scripted {
    async fn execute(
        &self,
        request: Request,
        _effect: bool,
    ) -> Result<JiraHttpResponse, JiraClientError> {
        self.urls
            .lock()
            .expect("urls")
            .push(request.url().to_string());
        self.responses
            .lock()
            .expect("responses")
            .pop_front()
            .ok_or_else(|| {
                JiraClientError::fixture(JiraClientErrorCode::DependencyUnavailable, true)
            })
    }
}

fn cloud_settings() -> Map<String, Value> {
    json!({
        "jira_configuration": {
            "base_url": "https://tenant.atlassian.net/",
            "hosting": "Cloud",
            "token": "jira-private-token"
        }
    })
    .as_object()
    .cloned()
    .expect("object")
}

fn client(responses: Vec<JiraHttpResponse>) -> (JiraClient, Arc<Scripted>) {
    let transport = Scripted::new(responses);
    let client = JiraClient::with_transport(
        JiraToolkitConfig::parse(&cloud_settings()).expect("config"),
        transport.clone(),
    );
    (client, transport)
}

fn ok(body: Value) -> JiraHttpResponse {
    JiraHttpResponse::fixture(StatusCode::OK, Some(body))
}

fn myself() -> JiraHttpResponse {
    ok(json!({"accountId": "1", "displayName": "Elitea Bot"}))
}

#[tokio::test]
async fn a_read_asks_for_the_rest_root_it_names_with_its_query_encoded() {
    let (client, transport) = client(vec![ok(json!({"issues": []})), ok(json!({"values": []}))]);
    client
        .read(
            JiraRoot::Api,
            &["search", "jql"],
            &[("jql", "project = \"A\""), ("nextPageToken", "t 1")],
        )
        .await
        .expect("read");
    client
        .read(
            JiraRoot::Agile,
            &["board", "7", "sprint"],
            &[("startAt", "0")],
        )
        .await
        .expect("read");
    assert_eq!(
        transport.urls(),
        [
            "https://tenant.atlassian.net/rest/api/3/search/jql?jql=project+%3D+%22A%22&nextPageToken=t+1",
            "https://tenant.atlassian.net/rest/agile/1.0/board/7/sprint?startAt=0",
        ]
    );
}

#[tokio::test]
async fn a_path_segment_that_could_escape_the_root_is_refused_before_any_request() {
    let (client, transport) = client(vec![]);
    for segment in ["..", ".", "", "a\nb"] {
        let error = client
            .read(JiraRoot::Api, &["issue", segment], &[])
            .await
            .err()
            .expect("refused");
        assert_eq!(
            error.code(),
            JiraClientErrorCode::InvalidInput,
            "{segment:?}"
        );
    }
    assert!(transport.urls().is_empty());
}

#[tokio::test]
async fn a_credential_jira_refuses_is_told_in_the_sdks_words() {
    let (client, _) = client(vec![JiraHttpResponse::fixture(
        StatusCode::UNAUTHORIZED,
        None,
    )]);
    assert_eq!(
        client.check_access().await.expect("checked"),
        Some("Authentication failed: Invalid username or API key.".to_owned())
    );
    let (client, _) = client(vec![JiraHttpResponse::fixture(StatusCode::FORBIDDEN, None)]);
    assert_eq!(
        client.check_access().await.expect("checked"),
        Some("Authentication failed: Access forbidden.".to_owned())
    );
    let (client, _) = client(vec![myself()]);
    assert_eq!(client.check_access().await.expect("checked"), None);
}

#[tokio::test]
async fn a_jira_refusal_a_rate_limit_and_a_hostile_body_read_as_themselves() {
    let (client, _) = client(vec![JiraHttpResponse::fixture(
        StatusCode::BAD_REQUEST,
        Some(json!({"errorMessages": ["The board does not support sprints"], "errors": {}})),
    )]);
    match client
        .read(JiraRoot::Agile, &["board"], &[])
        .await
        .expect("read")
    {
        JiraRead::Refused(message) => {
            assert!(
                message.contains("The board does not support sprints"),
                "{message}"
            );
        }
        JiraRead::Accepted(_) => panic!("a 400 was accepted"),
    }

    let (client, _) = client(vec![JiraHttpResponse::fixture(
        StatusCode::TOO_MANY_REQUESTS,
        None,
    )]);
    let limited = client
        .read(JiraRoot::Api, &["field"], &[])
        .await
        .err()
        .expect("rate limited");
    assert_eq!(limited.code(), JiraClientErrorCode::RateLimited);
    assert!(limited.retryable());

    let (client, _) = client(vec![JiraHttpResponse::fixture(
        StatusCode::SERVICE_UNAVAILABLE,
        None,
    )]);
    let down = client
        .read(JiraRoot::Api, &["field"], &[])
        .await
        .err()
        .expect("unavailable");
    assert!(down.retryable());

    // A 200 that is not JSON (a proxy's page) is no data, not a crash.
    let (client, _) = client(vec![JiraHttpResponse::text_fixture(
        StatusCode::OK,
        "<html>maintenance</html>",
    )]);
    match client
        .read(JiraRoot::Api, &["field"], &[])
        .await
        .expect("read")
    {
        JiraRead::Accepted(value) => assert!(value.is_null()),
        JiraRead::Refused(_) => panic!("a 200 was refused"),
    }
}

fn settings() -> AnalysisSettings {
    AnalysisSettings {
        bucket: "analyse-jira".to_owned(),
        closed_status: "Done".to_owned(),
        defect_types: vec!["Defect".to_owned()],
        custom_fields: vec![("team".to_owned(), Vec::new())],
        project_keys: vec!["ALP".to_owned()],
        max_issues: 5_000,
    }
}

fn tool(runner: &Arc<Runner>, name: &str) -> AnalyseTool {
    let kind = Kind::ALL
        .into_iter()
        .find(|kind| kind.name() == name)
        .expect("a tool");
    AnalyseTool::new(kind, "kit", Arc::clone(runner))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("analyse-jira").with_function_call_id("call"))
}

fn arguments() -> Value {
    json!({
        "closed_issues_based_on": 2,
        "resolved_after": "2026-01-01",
        "updated_after": "2026-01-01",
        "created_after": "2026-01-01"
    })
}

#[tokio::test]
async fn an_extraction_through_the_real_client_checks_the_credential_then_searches_twice() {
    let (client, transport) = client(vec![
        myself(),
        ok(json!({"issues": [], "isLast": true})),
        ok(json!({"issues": [], "isLast": true})),
    ]);
    let platform = FakePlatform::new();
    let runner = Arc::new(Runner::new(
        Arc::new(client),
        settings(),
        Some(platform.host()),
    ));
    let said = tool(&runner, "get_jira_issues")
        .execute(context(), arguments())
        .await
        .expect("told");
    assert!(
        said.as_str().expect("text").contains("nothing was written"),
        "{said}"
    );
    let urls = transport.urls();
    assert_eq!(urls.len(), 3, "{urls:?}");
    assert_eq!(urls[0], "https://tenant.atlassian.net/rest/api/3/myself");
    for url in &urls[1..] {
        assert!(
            url.starts_with("https://tenant.atlassian.net/rest/api/3/search/jql?jql=project+IN+"),
            "{url}"
        );
        assert!(url.contains("&maxResults=50&expand=changelog"), "{url}");
    }
    assert!(urls[1].contains("resolved+is+not+EMPTY"), "{}", urls[1]);
    assert!(urls[2].contains("resolved+is+EMPTY"), "{}", urls[2]);
}

#[tokio::test]
async fn a_rate_limit_that_does_not_lift_ends_the_tool_with_its_error() {
    let (client, transport) = client(vec![
        myself(),
        JiraHttpResponse::fixture(StatusCode::TOO_MANY_REQUESTS, None),
        JiraHttpResponse::fixture(StatusCode::TOO_MANY_REQUESTS, None),
        JiraHttpResponse::fixture(StatusCode::TOO_MANY_REQUESTS, None),
        JiraHttpResponse::fixture(StatusCode::TOO_MANY_REQUESTS, None),
    ]);
    let mut runner = Runner::new(Arc::new(client), settings(), None);
    runner.retry_delay = std::time::Duration::ZERO;
    let runner = Arc::new(runner);
    let error = tool(&runner, "get_number_off_all_issues")
        .execute(context(), json!({"after_date": "2026-01-01"}))
        .await
        .expect_err("rate limited");
    assert!(format!("{error:?}").contains("rate"), "{error:?}");
    // The credential check, the project listing and its three retries.
    assert_eq!(transport.urls().len(), 5);
}
