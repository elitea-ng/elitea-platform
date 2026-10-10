//! Focused compatibility and safety tests for the Jira family. Fixtures are
//! shaped on Jira Cloud (REST v3) and Server/Data Center (REST v2) replies.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_core::{ErrorCategory, ReadonlyContext, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::header::{AUTHORIZATION, COOKIE};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::jira::client::{
    JiraApi, JiraClient, JiraClientError, JiraClientErrorCode, JiraHttpResponse, JiraOperation,
    JiraTransport,
};
use super::families::jira::config::{JiraApiVersion, JiraConfigErrorCode, JiraToolkitConfig};
use super::families::jira::tools::{
    JiraToolsetErrorCode, build_jira_toolset, test_build_with_api, test_catalog,
};
use super::policy::ToolAdmissionPolicy;

const TOKEN: &str = "jira-private-token";
const API_KEY: &str = "jira-private-api-key";

fn cloud_settings() -> Map<String, Value> {
    json!({
        "jira_configuration": {
            "base_url": "https://tenant.atlassian.net/",
            "hosting": "Auto",
            "username": "",
            "api_key": null,
            "token": TOKEN
        },
        "api_version": "Auto",
        "limit": 5,
        "labels": "elitea, automation;;",
        "additional_fields": "customfield_10045, customfield_10100",
        "custom_headers": {"X-Tenant-Id": "acme"},
        "verify_ssl": true,
        "selected_tools": []
    })
    .as_object()
    .cloned()
    .expect("Jira cloud fixture is an object")
}

fn server_settings() -> Map<String, Value> {
    json!({
        "jira_configuration": {
            "base_url": "https://jira.example.test/jira",
            "hosting": "Server",
            "username": "svc-elitea",
            "api_key": API_KEY
        },
        "limit": 5
    })
    .as_object()
    .cloned()
    .expect("Jira server fixture is an object")
}

fn with(mut settings: Map<String, Value>, name: &str, value: Value) -> Map<String, Value> {
    settings.insert(name.to_owned(), value);
    settings
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Jira policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("jira-test").with_function_call_id("jira-call"))
}

#[derive(Clone, Debug)]
struct Snapshot {
    method: Method,
    url: String,
    body: Option<Value>,
    authorization: Option<String>,
    cookie: Option<String>,
    tenant: Option<String>,
    sensitive: bool,
    effect: bool,
}

struct FixtureTransport {
    responses: Mutex<VecDeque<JiraHttpResponse>>,
    requests: Mutex<Vec<Snapshot>>,
}

impl FixtureTransport {
    fn new(responses: Vec<JiraHttpResponse>) -> Arc<Self> {
        Arc::new(Self {
            responses: Mutex::new(responses.into()),
            requests: Mutex::new(Vec::new()),
        })
    }

    fn requests(&self) -> Vec<Snapshot> {
        self.requests.lock().expect("requests").clone()
    }
}

#[async_trait]
impl JiraTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<JiraHttpResponse, JiraClientError> {
        let header = |name| {
            request
                .headers()
                .get(name)
                .and_then(|value: &reqwest::header::HeaderValue| value.to_str().ok())
                .map(ToOwned::to_owned)
        };
        self.requests.lock().expect("requests").push(Snapshot {
            method: request.method().clone(),
            url: request.url().to_string(),
            body: request
                .body()
                .and_then(reqwest::Body::as_bytes)
                .and_then(|bytes| serde_json::from_slice(bytes).ok()),
            authorization: header(AUTHORIZATION),
            cookie: header(COOKIE),
            tenant: header(reqwest::header::HeaderName::from_static("x-tenant-id")),
            sensitive: request
                .headers()
                .get(AUTHORIZATION)
                .or_else(|| request.headers().get(COOKIE))
                .is_some_and(reqwest::header::HeaderValue::is_sensitive),
            effect,
        });
        self.responses
            .lock()
            .expect("responses")
            .pop_front()
            .ok_or_else(|| {
                JiraClientError::fixture(JiraClientErrorCode::DependencyUnavailable, true)
            })
    }
}

fn ok(body: Value) -> JiraHttpResponse {
    JiraHttpResponse::fixture(StatusCode::OK, Some(body))
}

fn myself() -> JiraHttpResponse {
    ok(json!({"accountId": "5b10a2844c20165700ede21g", "displayName": "Elitea Bot"}))
}

fn client(
    settings: &Map<String, Value>,
    responses: Vec<JiraHttpResponse>,
) -> (JiraClient, Arc<FixtureTransport>) {
    let transport = FixtureTransport::new(responses);
    let config = JiraToolkitConfig::parse(settings).expect("valid Jira fixture");
    (
        JiraClient::with_transport(config, transport.clone()),
        transport,
    )
}

async fn run(client: &JiraClient, operation: JiraOperation<'_>) -> String {
    client
        .execute(operation)
        .await
        .expect("Jira operation")
        .as_str()
        .expect("Jira results are text")
        .to_owned()
}

fn issue(key: &str, summary: &str) -> Value {
    json!({
        "id": "10001",
        "key": key,
        "fields": {
            "summary": summary,
            "description": {"type": "doc", "version": 1, "content": []},
            "created": "2026-10-01T09:30:00.000+0000",
            "updated": "2026-10-02T10:00:00.000+0000",
            "duedate": null,
            "priority": {"name": "High"},
            "status": {"name": "In Progress"},
            "project": {"id": "10000", "key": "PROJ"},
            "assignee": null,
            "issuetype": {"name": "Task"},
            "customfield_10045": "team-a",
            "issuelinks": [
                {"type": {"inward": "is blocked by", "outward": "blocks"}, "outwardIssue": {"key": "PROJ-9"}}
            ]
        }
    })
}

#[test]
fn configuration_resolves_hosting_version_credentials_and_lists_like_the_sdk() {
    let cloud = JiraToolkitConfig::parse(&cloud_settings()).expect("cloud");
    assert!(cloud.test_cloud());
    assert_eq!(cloud.api_version(), JiraApiVersion::V3);
    assert_eq!(cloud.test_base_url(), "https://tenant.atlassian.net/");
    assert_eq!(cloud.test_limit(), 5);
    assert_eq!(cloud.test_labels(), ["elitea", "automation"]);
    assert_eq!(
        cloud.test_additional_fields(),
        ["customfield_10045", "customfield_10100"]
    );

    let server = JiraToolkitConfig::parse(&server_settings()).expect("server");
    assert!(!server.test_cloud());
    assert_eq!(server.api_version(), JiraApiVersion::V2);
    assert_eq!(server.test_base_url(), "https://jira.example.test/jira");
    assert_eq!(server.test_limit(), 5);

    // Toolkit-level cloud wins; an explicit version wins over hosting; an
    // *.atlassian.net host resolves Auto to v3 even when hosting says Server.
    let explicit = with(
        with(server_settings(), "cloud", json!(true)),
        "api_version",
        json!("2"),
    );
    let explicit = JiraToolkitConfig::parse(&explicit).expect("explicit");
    assert!(explicit.test_cloud());
    assert_eq!(explicit.api_version(), JiraApiVersion::V2);
    let mut server_on_cloud_host = cloud_settings();
    server_on_cloud_host["jira_configuration"]["hosting"] = json!("Server");
    let parsed = JiraToolkitConfig::parse(&server_on_cloud_host).expect("server on cloud host");
    assert!(!parsed.test_cloud());
    assert_eq!(parsed.api_version(), JiraApiVersion::V3);
    // A spoofed host is not Atlassian Cloud.
    let mut spoof = server_settings();
    spoof["jira_configuration"]["base_url"] = json!("https://tenant.atlassian.net.evil.test");
    spoof["jira_configuration"]["hosting"] = json!("Auto");
    let parsed = JiraToolkitConfig::parse(&spoof).expect("spoofed host");
    assert!(!parsed.test_cloud());
    assert_eq!(parsed.api_version(), JiraApiVersion::V2);

    // Limit: default 5, clamped to the 1000 search maximum, never zero.
    assert_eq!(
        JiraToolkitConfig::parse(&with(server_settings(), "limit", json!(50_000)))
            .expect("clamped")
            .test_limit(),
        1_000
    );
    assert_eq!(
        JiraToolkitConfig::parse(&with(server_settings(), "limit", Value::Null))
            .expect("default")
            .test_limit(),
        5
    );

    let rendered = format!("{:?}", JiraToolkitConfig::parse(&Map::new()).err());
    assert!(!rendered.contains(TOKEN) && !rendered.contains(API_KEY));
}

#[test]
fn unsupported_and_invalid_configuration_fail_closed_with_distinct_codes() {
    let code = |settings: Map<String, Value>| {
        JiraToolkitConfig::parse(&settings)
            .err()
            .expect("configuration must fail")
            .code()
    };
    let mut http = server_settings();
    http["jira_configuration"]["base_url"] = json!("http://jira.example.test");
    assert_eq!(code(http), JiraConfigErrorCode::UnsupportedCapability);
    assert_eq!(
        code(with(server_settings(), "verify_ssl", json!(false))),
        JiraConfigErrorCode::UnsupportedCapability
    );
    assert_eq!(
        code(with(
            server_settings(),
            "custom_headers",
            json!({"Authorization": "Basic other"})
        )),
        JiraConfigErrorCode::UnsupportedCapability
    );
    assert_eq!(
        code(with(
            server_settings(),
            "custom_headers",
            json!({"X-Elitea-Project-Id": "7"})
        )),
        JiraConfigErrorCode::UnsupportedCapability
    );
    let mut no_credential = server_settings();
    no_credential["jira_configuration"]["api_key"] = json!("");
    assert_eq!(
        code(no_credential),
        JiraConfigErrorCode::InvalidConfiguration
    );
    let mut user_info = server_settings();
    user_info["jira_configuration"]["base_url"] = json!("https://user@jira.example.test");
    assert_eq!(code(user_info), JiraConfigErrorCode::InvalidConfiguration);
    assert_eq!(
        code(with(server_settings(), "limit", json!(0))),
        JiraConfigErrorCode::InvalidConfiguration
    );
    let mut oversized = server_settings();
    oversized["jira_configuration"]["api_key"] = json!("x".repeat(16 * 1_024 + 1));
    assert_eq!(code(oversized), JiraConfigErrorCode::ResourceExhausted);
}

#[tokio::test]
async fn catalog_selection_policy_and_the_sdk_contract() {
    assert_eq!(
        test_catalog(),
        vec![
            ("search_using_jql", true),
            ("create_issue", false),
            ("update_issue", false),
            ("modify_labels", false),
            ("list_comments", true),
            ("add_comments", false),
            ("list_projects", true),
            ("set_issue_status", false),
            ("get_specific_field_info", true),
            ("get_remote_links", true),
            ("link_issues", false),
            ("execute_generic_rq", false),
        ]
    );
    let (client, _) = client(&server_settings(), Vec::new());
    let api: Arc<dyn JiraApi> = Arc::new(client);
    let readonly: Arc<dyn ReadonlyContext> = context();

    // Every SDK name selected: the family keeps the twelve it serves.
    let names = super::sdk_conformance::sdk_tool_names("jira");
    let names = names.iter().map(String::as_str).collect::<Vec<_>>();
    let toolset = test_build_with_api("tracker", &names, &policy(&[]), &api).expect("all");
    let tools = toolset.tools(readonly.clone()).await.expect("tools");
    assert_eq!(tools.len(), 12);
    super::sdk_conformance::assert_sdk_conformance("jira", &tools);
    for tool in &tools {
        assert!(tool.description().starts_with("Toolkit: tracker\n"));
        assert!(!tool.description().contains("jira.example.test"));
        let schema = tool.parameters_schema().expect("schema");
        assert_eq!(schema["additionalProperties"], json!(false));
        assert_eq!(tool.is_concurrency_safe(), tool.is_read_only());
        if !tool.is_read_only() {
            assert!(tool.description().contains("reconciled"));
        }
    }

    // Empty selection is every served tool; a mixed one keeps the served.
    let all = test_build_with_api("tracker", &[], &policy(&[]), &api).expect("empty");
    assert_eq!(all.tools(readonly.clone()).await.expect("tools").len(), 12);
    let mixed = test_build_with_api(
        "tracker",
        &["get_attachments_content", "list_projects", "index_data"],
        &policy(&[]),
        &api,
    )
    .expect("mixed");
    let mixed = mixed.tools(readonly.clone()).await.expect("tools");
    assert_eq!(
        mixed.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        ["list_projects"]
    );
    for refused in [
        &["update_comment_with_file"][..],
        &["not_a_jira_tool", "list_projects"][..],
    ] {
        let Err(error) = test_build_with_api("tracker", refused, &policy(&[]), &api) else {
            panic!("{refused:?} must be an unsupported selection");
        };
        assert_eq!(error.code(), JiraToolsetErrorCode::UnsupportedSelection);
    }
    let blocked = test_build_with_api(
        "tracker",
        &[],
        &policy(&[("jira", &["execute_generic_rq"])]),
        &api,
    )
    .expect("blocked");
    assert_eq!(blocked.tools(readonly).await.expect("tools").len(), 11);

    // The production builder applies the same selection rules.
    let config = JiraToolkitConfig::parse(&with(
        server_settings(),
        "selected_tools",
        json!(["get_field_with_image_descriptions"]),
    ))
    .expect("config");
    let Err(error) = build_jira_toolset("tracker", config, &policy(&[])) else {
        panic!("an unserved-only selection has nothing to serve");
    };
    assert_eq!(error.code(), JiraToolsetErrorCode::UnsupportedSelection);
}

#[tokio::test]
async fn arguments_are_closed_and_typed_before_any_request() {
    let transport = FixtureTransport::new(Vec::new());
    let api: Arc<dyn JiraApi> = Arc::new(JiraClient::with_transport(
        JiraToolkitConfig::parse(&server_settings()).expect("config"),
        transport.clone(),
    ));
    let toolset = test_build_with_api("tracker", &[], &policy(&[]), &api).expect("toolset");
    let tools = toolset.tools(context()).await.expect("tools");
    let find = |name: &str| {
        tools
            .iter()
            .find(|tool| tool.name() == name)
            .expect("tool")
            .clone()
    };
    for (tool, arguments) in [
        (
            "search_using_jql",
            json!({"jql": "project = PROJ", "unknown": 1}),
        ),
        (
            "search_using_jql",
            json!({"jql": "project = PROJ", "limit": "ten"}),
        ),
        ("search_using_jql", json!({})),
        (
            "modify_labels",
            json!({"issue_key": "PROJ-1", "add_labels": [1]}),
        ),
        ("execute_generic_rq", json!({"method": "GET"})),
    ] {
        let error = find(tool)
            .execute(context(), arguments)
            .await
            .expect_err("invalid arguments");
        assert_eq!(error.category, ErrorCategory::InvalidInput);
    }
    assert!(transport.requests().is_empty());
}

#[tokio::test]
async fn cloud_search_probes_once_paginates_by_token_and_projects_issues() {
    let first_page = (0..100)
        .map(|index| issue(&format!("PROJ-{index}"), "First page"))
        .collect::<Vec<_>>();
    let (client, transport) = client(
        &cloud_settings(),
        vec![
            myself(),
            ok(json!({"issues": first_page, "nextPageToken": "page-2", "isLast": false})),
            ok(
                json!({"issues": [issue("PROJ-100", "Second page"), issue("PROJ-101", "Extra")], "isLast": true}),
            ),
            ok(json!({"issues": [], "isLast": true})),
        ],
    );
    let output = run(
        &client,
        JiraOperation::SearchUsingJql {
            jql: "project = PROJ ORDER BY created",
            limit: Some(101),
        },
    )
    .await;
    assert!(output.starts_with("Found 101 Jira issues:\n["), "{output}");
    let parsed: Vec<Value> =
        serde_json::from_str(output.split_once('\n').expect("list").1).expect("JSON list");
    let last = &parsed[100];
    assert_eq!(last["key"], "PROJ-100");
    assert_eq!(last["url"], "https://tenant.atlassian.net/browse/PROJ-100");
    assert_eq!(last["created"], "2026-10-01");
    assert_eq!(last["assignee"], "None");
    assert_eq!(last["priority"], "High");
    assert_eq!(last["status"], "In Progress");
    assert_eq!(last["projectId"], "10000");
    assert_eq!(last["customfield_10045"], "team-a");
    assert_eq!(last["customfield_10100"], Value::Null);
    assert_eq!(
        last["related_issues"],
        json!({"type": "blocks", "key": "PROJ-9", "url": "https://tenant.atlassian.net/browse/PROJ-9"})
    );

    let requests = transport.requests();
    assert_eq!(requests.len(), 3);
    assert_eq!(
        requests[0].url,
        "https://tenant.atlassian.net/rest/api/3/myself"
    );
    assert_eq!(
        requests[1].url,
        "https://tenant.atlassian.net/rest/api/3/search/jql?maxResults=100&fields=*all&jql=project+%3D+PROJ+ORDER+BY+created"
    );
    assert!(requests[2].url.ends_with("&nextPageToken=page-2"));
    for request in &requests {
        assert_eq!(
            request.authorization.as_deref(),
            Some("Bearer jira-private-token")
        );
        assert!(request.sensitive);
        assert_eq!(request.tenant.as_deref(), Some("acme"));
        assert!(!request.effect);
    }

    // The probe is not repeated; an empty result has the SDK's sentence.
    let transport_before = transport.requests().len();
    let (client, transport) =
        client_after_probe(&cloud_settings(), vec![ok(json!({"issues": []}))]);
    let output = run(
        &client,
        JiraOperation::SearchUsingJql {
            jql: "project = NONE",
            limit: None,
        },
    )
    .await;
    assert_eq!(output, "No Jira issues found");
    assert!(transport.requests()[1].url.contains("maxResults=5&"));
    assert_eq!(transport_before, 3);
}

/// A client whose first queued reply is the credential probe.
fn client_after_probe(
    settings: &Map<String, Value>,
    mut responses: Vec<JiraHttpResponse>,
) -> (JiraClient, Arc<FixtureTransport>) {
    responses.insert(0, myself());
    client(settings, responses)
}

#[tokio::test]
async fn server_search_paginates_by_start_at_under_the_context_path_with_basic_auth() {
    let page = |start: usize, count: usize| {
        ok(json!({
            "startAt": start,
            "issues": (start..start + count).map(|index| issue(&format!("OPS-{index}"), "s")).collect::<Vec<_>>()
        }))
    };
    let (client, transport) =
        client_after_probe(&server_settings(), vec![page(0, 100), page(100, 20)]);
    let output = run(
        &client,
        JiraOperation::SearchUsingJql {
            jql: "project = OPS",
            limit: Some(0),
        },
    )
    .await;
    assert!(output.starts_with("Found 120 Jira issues:"));
    let requests = transport.requests();
    assert_eq!(
        requests[0].url,
        "https://jira.example.test/jira/rest/api/2/myself"
    );
    assert!(requests[1]
        .url
        .starts_with("https://jira.example.test/jira/rest/api/2/search?maxResults=100&fields=*all&jql=project+%3D+OPS&startAt=0"));
    assert!(requests[2].url.ends_with("&startAt=100"));
    let expected = format!(
        "Basic {}",
        base64::Engine::encode(
            &base64::engine::general_purpose::STANDARD,
            format!("svc-elitea:{API_KEY}")
        )
    );
    assert_eq!(
        requests[1].authorization.as_deref(),
        Some(expected.as_str())
    );
    assert!(output.contains("https://jira.example.test/jira/browse/OPS-0"));
}

#[tokio::test]
async fn create_issue_posts_fields_then_applies_default_labels() {
    let (client, transport) = client_after_probe(
        &cloud_settings(),
        vec![
            JiraHttpResponse::fixture(
                StatusCode::CREATED,
                Some(
                    json!({"id": "10050", "key": "PROJ-50", "self": "https://tenant.atlassian.net/rest/api/3/issue/10050"}),
                ),
            ),
            JiraHttpResponse::fixture(StatusCode::NO_CONTENT, None),
        ],
    );
    let output = run(
        &client,
        JiraOperation::CreateIssue {
            issue_json: r#"{"fields": {"project": {"key": "PROJ"}, "summary": "New", "issuetype": {"name": "Task"}}, "update": {"issuelinks": []}}"#,
        },
    )
    .await;
    assert!(output.starts_with(
        "Done. Issue PROJ-50 is created successfully. You can view it at https://tenant.atlassian.net/browse/PROJ-50. Details: "
    ));
    let requests = transport.requests();
    assert_eq!(requests[1].method, Method::POST);
    assert_eq!(
        requests[1].url,
        "https://tenant.atlassian.net/rest/api/3/issue?updateHistory=false"
    );
    assert_eq!(
        requests[1].body,
        Some(
            json!({"fields": {"project": {"key": "PROJ"}, "summary": "New", "issuetype": {"name": "Task"}}, "update": {"issuelinks": []}})
        )
    );
    assert!(requests[1].effect);
    // `_add_default_labels` goes through the SDK client's v2 update route.
    assert_eq!(requests[2].method, Method::PUT);
    assert_eq!(
        requests[2].url,
        "https://tenant.atlassian.net/rest/api/2/issue/PROJ-50"
    );
    assert_eq!(
        requests[2].body,
        Some(json!({"update": {"labels": [{"add": "elitea"}, {"add": "automation"}]}}))
    );

    // Validation answers are the SDK's sentences, sent before any request.
    let (client, transport) = client_after_probe(&cloud_settings(), Vec::new());
    let output = run(
        &client,
        JiraOperation::CreateIssue {
            issue_json: r#"{"fields": {"summary": "No project"}}"#,
        },
    )
    .await;
    assert_eq!(
        output,
        "Jira project key is required to create an issue. Ask user to provide it."
    );
    let output = run(&client, JiraOperation::CreateIssue { issue_json: "{}" }).await;
    assert!(output.starts_with("Jira fields are provided in a wrong way."));
    let output = run(
        &client,
        JiraOperation::CreateIssue {
            issue_json: "not json",
        },
    )
    .await;
    assert!(output.starts_with("Error creating Jira issue: the JSON is not valid"));
    assert_eq!(transport.requests().len(), 1);
}

#[tokio::test]
async fn provider_refusals_are_model_text_and_infrastructure_failures_are_typed() {
    let (client, _) = client_after_probe(
        &server_settings(),
        vec![JiraHttpResponse::fixture(
            StatusCode::BAD_REQUEST,
            Some(
                json!({"errorMessages": [], "errors": {"summary": "You must specify a summary of the issue."}}),
            ),
        )],
    );
    let output = run(
        &client,
        JiraOperation::CreateIssue {
            issue_json: r#"{"fields": {"project": {"key": "OPS"}}}"#,
        },
    )
    .await;
    assert_eq!(
        output,
        "Error creating Jira issue: HTTP 400 Bad Request: summary: You must specify a summary of the issue."
    );

    let (client, _) = client_after_probe(
        &server_settings(),
        vec![JiraHttpResponse::fixture(
            StatusCode::NOT_FOUND,
            Some(
                json!({"errorMessages": ["Issue does not exist or you do not have permission to see it."], "errors": {}}),
            ),
        )],
    );
    let output = run(
        &client,
        JiraOperation::ListComments {
            issue_key: "OPS-404",
        },
    )
    .await;
    assert_eq!(
        output,
        "Error during the attempt to extract available comments: HTTP 404 Not Found: Issue does not exist or you do not have permission to see it."
    );

    // A 5xx after an effect was sent is an unknown outcome; on a read it is
    // a retryable outage; a 401 mid-session is an authentication failure.
    let (client, _) = client_after_probe(
        &server_settings(),
        vec![JiraHttpResponse::fixture(StatusCode::BAD_GATEWAY, None)],
    );
    let error = client
        .execute(JiraOperation::AddComments {
            issue_key: "OPS-1",
            comment: "hello",
        })
        .await
        .expect_err("unknown outcome");
    assert_eq!(error.code(), JiraClientErrorCode::UnknownOutcome);
    assert!(!error.retryable());
    let (client, _) = client_after_probe(
        &server_settings(),
        vec![JiraHttpResponse::fixture(
            StatusCode::SERVICE_UNAVAILABLE,
            None,
        )],
    );
    let error = client
        .execute(JiraOperation::ListProjects)
        .await
        .expect_err("outage");
    assert_eq!(error.code(), JiraClientErrorCode::DependencyUnavailable);
    assert!(error.retryable());
    let (client, _) = client_after_probe(
        &server_settings(),
        vec![JiraHttpResponse::fixture(StatusCode::UNAUTHORIZED, None)],
    );
    let error = client
        .execute(JiraOperation::GetRemoteLinks { issue_key: "OPS-1" })
        .await
        .expect_err("401");
    assert_eq!(error.code(), JiraClientErrorCode::Authentication);
}

#[tokio::test]
async fn the_credential_probe_answers_with_the_sdk_sentences() {
    for (status, expected) in [
        (
            StatusCode::UNAUTHORIZED,
            "Authentication failed: Invalid username or API key.".to_owned(),
        ),
        (
            StatusCode::FORBIDDEN,
            "Authentication failed: Access forbidden.".to_owned(),
        ),
        (
            StatusCode::NOT_FOUND,
            "Jira REST API v2 not found at https://jira.example.test/jira. Check the Hosting setting on the linked credential — Cloud uses v3, Server uses v2.".to_owned(),
        ),
        (
            StatusCode::METHOD_NOT_ALLOWED,
            "Authentication failed: Unable to connect to Jira (HTTP 405).".to_owned(),
        ),
    ] {
        let (client, transport) = client(
            &server_settings(),
            vec![JiraHttpResponse::fixture(status, None)],
        );
        let output = run(&client, JiraOperation::ListProjects).await;
        assert_eq!(output, expected);
        assert_eq!(transport.requests().len(), 1, "no operation after a failed probe");
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One route contract per SDK effect.
async fn update_modify_labels_and_status_transitions_follow_the_sdk_routes() {
    let (client, transport) = client_after_probe(
        &server_settings(),
        vec![JiraHttpResponse::fixture(StatusCode::NO_CONTENT, None)],
    );
    let output = run(
        &client,
        JiraOperation::UpdateIssue {
            issue_json: r#"{"key": "OPS-7", "fields": {"summary": "Renamed"}}"#,
        },
    )
    .await;
    assert_eq!(
        output,
        "Done. Issue OPS-7 has been updated successfully. You can view it at https://jira.example.test/jira/browse/OPS-7. Details: null"
    );
    let requests = transport.requests();
    assert_eq!(
        requests[1].url,
        "https://jira.example.test/jira/rest/api/2/issue/OPS-7"
    );
    assert_eq!(
        requests[1].body,
        Some(json!({"fields": {"summary": "Renamed"}}))
    );

    let (client, _) = client_after_probe(&server_settings(), Vec::new());
    assert_eq!(
        run(
            &client,
            JiraOperation::UpdateIssue {
                issue_json: r#"{"fields": {}}"#
            }
        )
        .await,
        "Jira issue key is required to update an issue. Ask user to provide it."
    );
    assert!(
        run(&client, JiraOperation::UpdateIssue { issue_json: r#"{"key": "OPS-1"}"# })
            .await
            .starts_with("Jira fields are provided in a wrong way. It should have at least any of nodes `fields` or `update`")
    );
    assert_eq!(
        run(
            &client,
            JiraOperation::ModifyLabels {
                issue_key: "OPS-1",
                add_labels: None,
                remove_labels: None
            }
        )
        .await,
        "You must provide at least 1 label to be added or removed"
    );

    let (client, transport) = client_after_probe(
        &server_settings(),
        vec![JiraHttpResponse::fixture(StatusCode::NO_CONTENT, None)],
    );
    run(
        &client,
        JiraOperation::ModifyLabels {
            issue_key: "OPS-1",
            add_labels: Some(vec!["triaged"]),
            remove_labels: Some(vec!["new"]),
        },
    )
    .await;
    assert_eq!(
        transport.requests()[1].body,
        Some(json!({"update": {"labels": [{"add": "triaged"}, {"remove": "new"}]}}))
    );

    let (client, transport) = client_after_probe(
        &server_settings(),
        vec![
            ok(json!({"transitions": [
                {"id": "11", "name": "Start", "to": {"name": "In Progress"}},
                {"id": "31", "name": "Finish", "to": {"name": "Done"}}
            ]})),
            JiraHttpResponse::fixture(StatusCode::NO_CONTENT, None),
        ],
    );
    let output = run(
        &client,
        JiraOperation::SetIssueStatus {
            issue_key: "OPS-1",
            status_name: "done",
            mandatory_fields_json: r#"{"fields": {"resolution": {"name": "Fixed"}}, "update": {"comment": [{"add": {"body": "closing"}}]}}"#,
        },
    )
    .await;
    assert_eq!(
        output,
        "Done. Status for issue OPS-1 was updated successfully. You can view it at https://jira.example.test/jira/browse/OPS-1."
    );
    let requests = transport.requests();
    assert_eq!(
        requests[2].url,
        "https://jira.example.test/jira/rest/api/2/issue/OPS-1/transitions"
    );
    assert_eq!(
        requests[2].body,
        Some(json!({
            "transition": {"id": 31},
            "fields": {"resolution": {"name": "Fixed"}},
            "update": {"comment": [{"add": {"body": "closing"}}]}
        }))
    );

    let (client, _) = client_after_probe(
        &server_settings(),
        vec![ok(
            json!({"transitions": [{"id": "11", "name": "Start", "to": {"name": "In Progress"}}]}),
        )],
    );
    let output = run(
        &client,
        JiraOperation::SetIssueStatus {
            issue_key: "OPS-1",
            status_name: "Closed",
            mandatory_fields_json: "{}",
        },
    )
    .await;
    assert_eq!(
        output,
        "Error creating Jira issue: no transition to status 'Closed' is available for OPS-1; available target statuses: In Progress"
    );
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One text contract per SDK read.
async fn comments_links_fields_projects_and_remote_links_render_sdk_text() {
    let (client, transport) = client_after_probe(
        &cloud_settings(),
        vec![
            JiraHttpResponse::fixture(StatusCode::CREATED, Some(json!({"id": "10200"}))),
            JiraHttpResponse::fixture(StatusCode::NO_CONTENT, None),
        ],
    );
    let output = run(
        &client,
        JiraOperation::AddComments {
            issue_key: "PROJ-1",
            comment: "Looks good",
        },
    )
    .await;
    assert_eq!(
        output,
        "Done. Comment 10200 is added for issue PROJ-1. You can view it at https://tenant.atlassian.net/browse/PROJ-1"
    );
    assert_eq!(
        transport.requests()[1].body,
        Some(
            json!({"body": {"type": "doc", "version": 1, "content": [{"type": "paragraph", "content": [{"type": "text", "text": "Looks good"}]}]}})
        )
    );

    let (client, transport) = client_after_probe(
        &server_settings(),
        vec![JiraHttpResponse::fixture(StatusCode::CREATED, None)],
    );
    let output = run(
        &client,
        JiraOperation::LinkIssues {
            inward_issue_key: "OPS-2",
            outward_issue_key: "OPS-3",
            linktype: "Blocks",
        },
    )
    .await;
    assert!(output.starts_with("Link created using following data: {"));
    assert_eq!(
        transport.requests()[1].body,
        Some(json!({
            "type": {"name": "Blocks"},
            "inwardIssue": {"key": "OPS-2"},
            "outwardIssue": {"key": "OPS-3"},
            "comment": {"body": "Issue OPS-2 was linked to OPS-3."}
        }))
    );
    assert_eq!(
        transport.requests()[1].url,
        "https://jira.example.test/jira/rest/api/2/issueLink"
    );

    let (client, transport) = client_after_probe(
        &server_settings(),
        vec![
            ok(json!({"fields": {"customfield_1": null}})),
            ok(
                json!({"fields": {"summary": "S", "status": {"name": "Open"}, "customfield_1": null}}),
            ),
        ],
    );
    let output = run(
        &client,
        JiraOperation::GetSpecificFieldInfo {
            issue_key: "OPS-1",
            field_name: "customfield_1",
        },
    )
    .await;
    let listed = output
        .strip_prefix("Unable to find field 'customfield_1'. All available fields are '")
        .and_then(|rest| rest.strip_suffix('\''))
        .expect("the SDK's sentence");
    let mut listed = listed.split(", ").collect::<Vec<_>>();
    listed.sort_unstable();
    assert_eq!(listed, ["status", "summary"]);
    assert!(
        transport.requests()[1]
            .url
            .ends_with("/issue/OPS-1?fields=customfield_1")
    );

    let (client, _) = client_after_probe(
        &server_settings(),
        vec![ok(json!({"fields": {"summary": "Plain summary"}}))],
    );
    assert_eq!(
        run(
            &client,
            JiraOperation::GetSpecificFieldInfo {
                issue_key: "OPS-1",
                field_name: "summary"
            }
        )
        .await,
        "Got the data from following Jira issue - OPS-1 and field - summary. The data is:\nPlain summary"
    );

    let (client, _) = client_after_probe(
        &server_settings(),
        vec![ok(
            json!([{"id": 1, "object": {"url": "https://docs.example.test", "title": "Spec"}}]),
        )],
    );
    assert!(
        run(
            &client,
            JiraOperation::GetRemoteLinks { issue_key: "OPS-1" }
        )
        .await
        .starts_with("Jira issue - OPS-1 has the following remote links:\n[{")
    );

    // Server lists projects in one call; Cloud pages through project/search
    // and never follows a nextPage off its own REST root.
    let (client, _) = client_after_probe(
        &server_settings(),
        vec![ok(
            json!([{"id": "1", "key": "OPS", "name": "Operations", "projectTypeKey": "software"}]),
        )],
    );
    let output = run(&client, JiraOperation::ListProjects).await;
    let projects = output
        .strip_prefix("Found 1 projects:\n")
        .expect("the SDK's sentence");
    assert_eq!(
        serde_json::from_str::<Value>(projects).expect("project list"),
        json!([{"id": "1", "key": "OPS", "name": "Operations", "type": "software", "style": ""}])
    );
    let (client, transport) = client_after_probe(
        &cloud_settings(),
        vec![
            ok(
                json!({"values": [{"id": "1", "key": "A", "name": "A", "projectTypeKey": "software"}],
                      "isLast": false,
                      "nextPage": "https://tenant.atlassian.net/rest/api/3/project/search?startAt=1"}),
            ),
            ok(
                json!({"values": [{"id": "2", "key": "B", "name": "B", "projectTypeKey": "business"}],
                      "isLast": false,
                      "nextPage": "https://elsewhere.example.test/rest/api/3/project/search?startAt=2"}),
            ),
        ],
    );
    assert!(
        run(&client, JiraOperation::ListProjects)
            .await
            .starts_with("Found 2 projects:")
    );
    let requests = transport.requests();
    assert_eq!(requests.len(), 3);
    assert_eq!(
        requests[2].url,
        "https://tenant.atlassian.net/rest/api/3/project/search?startAt=1"
    );
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Read, effect, refusal and path-escape cases together.
async fn generic_requests_stay_on_the_instance_and_keep_the_sdk_shapes() {
    let (client, transport) = client_after_probe(
        &cloud_settings(),
        vec![ok(json!({"issues": [issue("PROJ-1", "Login fails")]}))],
    );
    let output = run(
        &client,
        JiraOperation::ExecuteGenericRq {
            method: "GET",
            relative_url: "/rest/api/3/search/jql",
            params: Some(r#"Here: {"jql": "project = PROJ", "maxResults": 5, "fields": ["summary", "customfield_10045"]}"#),
        },
    )
    .await;
    let issues = output
        .strip_prefix("HTTP: GET /rest/api/3/search/jql -> 200 OK ")
        .expect("the SDK's status line");
    assert_eq!(
        serde_json::from_str::<Value>(issues).expect("projected issues"),
        json!([{
            "key": "PROJ-1",
            "url": "https://tenant.atlassian.net/browse/PROJ-1",
            "summary": "Login fails",
            "assignee": "None",
            "status": "In Progress",
            "issuetype": "Task",
            "customfield_10045": "team-a"
        }])
    );
    let request_url = reqwest::Url::parse(&transport.requests()[1].url).expect("generic URL");
    assert_eq!(request_url.path(), "/rest/api/3/search/jql");
    let mut query = request_url
        .query_pairs()
        .map(|(name, value)| (name.into_owned(), value.into_owned()))
        .collect::<Vec<_>>();
    query.sort();
    assert_eq!(
        query,
        [
            ("fields".to_owned(), "customfield_10045".to_owned()),
            ("fields".to_owned(), "summary".to_owned()),
            ("jql".to_owned(), "project = PROJ".to_owned()),
            ("maxResults".to_owned(), "5".to_owned()),
        ]
    );

    let (client, transport) = client_after_probe(
        &server_settings(),
        vec![JiraHttpResponse::fixture(
            StatusCode::CREATED,
            Some(json!({"id": "9"})),
        )],
    );
    let output = run(
        &client,
        JiraOperation::ExecuteGenericRq {
            method: "post",
            relative_url: "/rest/api/2/issue/OPS-1/watchers",
            params: Some(r#"{"accountId": "abc"}"#),
        },
    )
    .await;
    assert_eq!(
        output,
        "HTTP: post /rest/api/2/issue/OPS-1/watchers -> 201 Created {\"id\":\"9\"}"
    );
    let request = &transport.requests()[1];
    assert_eq!(request.method, Method::POST);
    assert!(request.effect);
    assert_eq!(request.body, Some(json!({"accountId": "abc"})));
    assert_eq!(
        request.url,
        "https://jira.example.test/jira/rest/api/2/issue/OPS-1/watchers"
    );

    let (client, transport) = client_after_probe(&server_settings(), Vec::new());
    for relative_url in [
        "https://evil.example.test/rest/api/2/myself",
        "//evil.example.test/rest",
        "/rest/api/2/../../admin",
        "/rest/api/2/%2e%2e/admin",
        "/rest/api/2/search?jql=x",
        "rest/api/2/myself",
    ] {
        let output = run(
            &client,
            JiraOperation::ExecuteGenericRq {
                method: "GET",
                relative_url,
                params: None,
            },
        )
        .await;
        assert!(
            output.starts_with("JIRA tool exception. relative_url must be"),
            "{relative_url}"
        );
    }
    let output = run(
        &client,
        JiraOperation::ExecuteGenericRq {
            method: "GET",
            relative_url: "/rest/api/2/myself",
            params: Some("{not json"),
        },
    )
    .await;
    assert!(output.starts_with("JIRA tool exception. Passed params are not valid JSON."));
    assert_eq!(transport.requests().len(), 1, "only the probe was sent");

    let (client, _) = client_after_probe(
        &server_settings(),
        vec![JiraHttpResponse::fixture(
            StatusCode::BAD_REQUEST,
            Some(json!({"errorMessages": ["Field 'x' does not exist."]})),
        )],
    );
    assert_eq!(
        run(
            &client,
            JiraOperation::ExecuteGenericRq {
                method: "GET",
                relative_url: "/rest/api/2/search",
                params: Some(r#"{"jql": "x = 1"}"#),
            }
        )
        .await,
        "Jira API error: HTTP 400 Bad Request: Field 'x' does not exist."
    );
}

#[tokio::test]
async fn cookie_tokens_are_sent_as_cookies_and_oversized_reads_are_refused() {
    let mut settings = server_settings();
    settings["jira_configuration"]["token"] = json!("JSESSIONID=abc123; atlassian.xsrf.token=t1");
    let (client, transport) = client_after_probe(&settings, vec![ok(json!([]))]);
    run(&client, JiraOperation::ListProjects).await;
    let request = &transport.requests()[1];
    assert_eq!(
        request.cookie.as_deref(),
        Some("JSESSIONID=abc123; atlassian.xsrf.token=t1")
    );
    assert!(request.authorization.is_none());
    assert!(request.sensitive);

    let huge = "x".repeat(600 * 1_024);
    let (client, _) = client_after_probe(&server_settings(), vec![ok(json!([{"id": huge}]))]);
    let error = client
        .execute(JiraOperation::GetRemoteLinks { issue_key: "OPS-1" })
        .await
        .expect_err("over the output bound");
    assert_eq!(error.code(), JiraClientErrorCode::ResourceExhausted);
}

#[tokio::test]
async fn generic_non_json_replies_are_returned_as_text() {
    let (client, _) = client_after_probe(
        &server_settings(),
        vec![JiraHttpResponse::text_fixture(
            StatusCode::OK,
            "<html><body>Server info</body></html>",
        )],
    );
    assert_eq!(
        run(
            &client,
            JiraOperation::ExecuteGenericRq {
                method: "GET",
                relative_url: "/rest/api/2/serverInfo",
                params: None,
            }
        )
        .await,
        "HTTP: GET /rest/api/2/serverInfo -> 200 OK <html><body>Server info</body></html>"
    );
}
