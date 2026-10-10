//! Focused compatibility and safety tests for the Confluence family.
//! Fixtures follow Confluence Cloud (REST v1 under `/wiki`, v2 `pages`) and
//! Server/Data Center replies.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_core::{ErrorCategory, ReadonlyContext, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::header::AUTHORIZATION;
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::confluence::client::{
    ConfluenceApi, ConfluenceClient, ConfluenceClientError, ConfluenceClientErrorCode,
    ConfluenceHttpResponse, ConfluenceOperation, ConfluenceTransport,
};
use super::families::confluence::config::{
    ConfluenceApiVersion, ConfluenceConfigErrorCode, ConfluenceToolkitConfig,
};
use super::families::confluence::tools::{
    ConfluenceToolsetErrorCode, build_confluence_toolset, test_build_with_api, test_catalog,
};
use super::policy::ToolAdmissionPolicy;

const TOKEN: &str = "confluence-private-token";

fn cloud_settings() -> Map<String, Value> {
    json!({
        "confluence_configuration": {
            "base_url": "https://tenant.atlassian.net/wiki/",
            "hosting": "Cloud",
            "username": "bot@example.test",
            "api_key": "confluence-api-key"
        },
        "space": "'DOCS'",
        "api_version": "1",
        "limit": 2,
        "max_pages": 3,
        "labels": "elitea",
        "selected_tools": []
    })
    .as_object()
    .cloned()
    .expect("Confluence cloud fixture is an object")
}

fn server_settings() -> Map<String, Value> {
    json!({
        "confluence_configuration": {
            "base_url": "https://wiki.example.test/confluence",
            "hosting": "Server",
            "token": TOKEN
        },
        "space": "ENG"
    })
    .as_object()
    .cloned()
    .expect("Confluence server fixture is an object")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Confluence policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("confluence-test").with_function_call_id("confluence-call"))
}

#[derive(Clone, Debug)]
struct Snapshot {
    method: Method,
    url: String,
    body: Option<Value>,
    authorization: Option<String>,
    sensitive: bool,
    effect: bool,
}

struct FixtureTransport {
    responses: Mutex<VecDeque<ConfluenceHttpResponse>>,
    requests: Mutex<Vec<Snapshot>>,
}

impl FixtureTransport {
    fn new(responses: Vec<ConfluenceHttpResponse>) -> Arc<Self> {
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
impl ConfluenceTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<ConfluenceHttpResponse, ConfluenceClientError> {
        self.requests.lock().expect("requests").push(Snapshot {
            method: request.method().clone(),
            url: request.url().to_string(),
            body: request
                .body()
                .and_then(reqwest::Body::as_bytes)
                .and_then(|bytes| serde_json::from_slice(bytes).ok()),
            authorization: request
                .headers()
                .get(AUTHORIZATION)
                .and_then(|value| value.to_str().ok())
                .map(ToOwned::to_owned),
            sensitive: request
                .headers()
                .get(AUTHORIZATION)
                .is_some_and(reqwest::header::HeaderValue::is_sensitive),
            effect,
        });
        self.responses
            .lock()
            .expect("responses")
            .pop_front()
            .ok_or_else(|| {
                ConfluenceClientError::fixture(
                    ConfluenceClientErrorCode::DependencyUnavailable,
                    true,
                )
            })
    }
}

fn ok(body: Value) -> ConfluenceHttpResponse {
    ConfluenceHttpResponse::fixture(StatusCode::OK, Some(body))
}

fn status(code: StatusCode, body: Option<Value>) -> ConfluenceHttpResponse {
    ConfluenceHttpResponse::fixture(code, body)
}

fn fixture_client(
    settings: &Map<String, Value>,
    responses: Vec<ConfluenceHttpResponse>,
) -> (ConfluenceClient, Arc<FixtureTransport>) {
    let transport = FixtureTransport::new(responses);
    let config = ConfluenceToolkitConfig::parse(settings).expect("valid Confluence fixture");
    (
        ConfluenceClient::with_transport(config, transport.clone()),
        transport,
    )
}

async fn run(client: &ConfluenceClient, operation: ConfluenceOperation<'_>) -> String {
    client
        .execute(operation)
        .await
        .expect("Confluence operation")
        .as_str()
        .expect("Confluence results are text")
        .to_owned()
}

fn page(id: &str, title: &str, html: &str) -> Value {
    json!({
        "id": id,
        "type": "page",
        "title": title,
        "body": {"view": {"value": html, "representation": "view"}},
        "version": {"number": 4, "when": "2026-10-01T09:00:00.000Z"},
        "_links": {"webui": format!("/spaces/DOCS/pages/{id}/{title}"), "base": "https://tenant.atlassian.net/wiki"}
    })
}

#[test]
fn configuration_normalises_urls_hosting_versions_and_bounds_like_the_sdk() {
    let cloud = ConfluenceToolkitConfig::parse(&cloud_settings()).expect("cloud");
    assert!(cloud.test_cloud());
    assert_eq!(cloud.api_version(), ConfluenceApiVersion::V1);
    // `/wiki` stripped from the base, re-added for the REST root.
    assert_eq!(
        cloud.test_urls(),
        (
            "https://tenant.atlassian.net/",
            "https://tenant.atlassian.net/wiki"
        )
    );
    assert_eq!(cloud.test_paging(), (2, 3));
    assert_eq!(cloud.test_labels(), ["elitea"]);

    let server = ConfluenceToolkitConfig::parse(&server_settings()).expect("server");
    assert!(!server.test_cloud());
    assert_eq!(server.api_version(), ConfluenceApiVersion::V1);
    assert_eq!(
        server.test_urls(),
        (
            "https://wiki.example.test/confluence",
            "https://wiki.example.test/confluence"
        )
    );
    assert_eq!(server.test_paging(), (5, 10));

    // Auto resolves Cloud to v2; caps hold.
    let auto = ConfluenceToolkitConfig::parse(&with(
        with(
            with(cloud_settings(), "api_version", json!("Auto")),
            "limit",
            json!(1_000),
        ),
        "max_pages",
        json!(1_000),
    ))
    .expect("auto");
    assert_eq!(auto.api_version(), ConfluenceApiVersion::V2);
    assert_eq!(auto.test_paging(), (100, 50));

    let code = |settings: Map<String, Value>| {
        ConfluenceToolkitConfig::parse(&settings)
            .err()
            .expect("configuration must fail")
            .code()
    };
    let mut http = server_settings();
    http["confluence_configuration"]["base_url"] = json!("http://wiki.example.test");
    assert_eq!(code(http), ConfluenceConfigErrorCode::UnsupportedCapability);
    assert_eq!(
        code(with(server_settings(), "verify_ssl", json!(false))),
        ConfluenceConfigErrorCode::UnsupportedCapability
    );
    let mut no_credential = server_settings();
    no_credential["confluence_configuration"]["token"] = json!("");
    assert_eq!(
        code(no_credential),
        ConfluenceConfigErrorCode::InvalidConfiguration
    );
    let rendered = format!("{:?}", ConfluenceToolkitConfig::parse(&Map::new()).err());
    assert!(!rendered.contains(TOKEN));
}

#[tokio::test]
async fn catalog_selection_policy_and_the_sdk_contract() {
    assert_eq!(test_catalog().len(), 16);
    assert_eq!(
        test_catalog()
            .iter()
            .filter(|(_, read_only)| *read_only)
            .count(),
        8
    );
    let (client, _) = fixture_client(&server_settings(), Vec::new());
    let api: Arc<dyn ConfluenceApi> = Arc::new(client);
    let readonly: Arc<dyn ReadonlyContext> = context();
    let names = super::sdk_conformance::sdk_tool_names("confluence");
    let names = names.iter().map(String::as_str).collect::<Vec<_>>();
    let toolset = test_build_with_api("wiki", &names, &policy(&[]), &api).expect("all");
    let tools = toolset.tools(readonly.clone()).await.expect("tools");
    assert_eq!(tools.len(), 16);
    super::sdk_conformance::assert_sdk_conformance("confluence", &tools);
    for tool in &tools {
        assert!(tool.description().starts_with("Toolkit: wiki\n"));
        assert_eq!(
            tool.parameters_schema().expect("schema")["additionalProperties"],
            json!(false)
        );
        assert_eq!(tool.is_concurrency_safe(), tool.is_read_only());
    }
    let mixed = test_build_with_api(
        "wiki",
        &["add_file_to_page", "site_search"],
        &policy(&[]),
        &api,
    )
    .expect("mixed");
    assert_eq!(
        mixed
            .tools(readonly.clone())
            .await
            .expect("tools")
            .iter()
            .map(|tool| tool.name())
            .collect::<Vec<_>>(),
        ["site_search"]
    );
    let Err(error) = test_build_with_api("wiki", &["get_page_attachments"], &policy(&[]), &api)
    else {
        panic!("unserved-only selection");
    };
    assert_eq!(
        error.code(),
        ConfluenceToolsetErrorCode::UnsupportedSelection
    );
    let blocked = test_build_with_api(
        "wiki",
        &[],
        &policy(&[("confluence", &["delete_page"])]),
        &api,
    )
    .expect("blocked");
    assert_eq!(blocked.tools(readonly).await.expect("tools").len(), 15);

    let config = ConfluenceToolkitConfig::parse(&with(
        server_settings(),
        "selected_tools",
        json!(["no_such_tool"]),
    ))
    .expect("config");
    let Err(error) = build_confluence_toolset("wiki", config, &policy(&[])) else {
        panic!("unknown tool");
    };
    assert_eq!(
        error.code(),
        ConfluenceToolsetErrorCode::UnsupportedSelection
    );

    // Arguments are closed and typed before any request.
    let transport = FixtureTransport::new(Vec::new());
    let api: Arc<dyn ConfluenceApi> = Arc::new(ConfluenceClient::with_transport(
        ConfluenceToolkitConfig::parse(&server_settings()).expect("config"),
        transport.clone(),
    ));
    let toolset = test_build_with_api("wiki", &[], &policy(&[]), &api).expect("toolset");
    let tools = toolset.tools(context()).await.expect("tools");
    for (name, arguments) in [
        (
            "read_page_by_id",
            json!({"page_id": "1", "skip_images": "yes"}),
        ),
        ("update_labels", json!({"page_ids": [{"id": 1}]})),
        ("site_search", json!({"query": "x", "extra": 1})),
    ] {
        let tool = tools.iter().find(|tool| tool.name() == name).expect("tool");
        let error = tool
            .execute(context(), arguments)
            .await
            .expect_err("invalid arguments");
        assert_eq!(error.category, ErrorCategory::InvalidInput);
    }
    assert!(transport.requests().is_empty());
}

#[tokio::test]
async fn read_page_renders_markdown_or_raw_adf_and_reports_missing_pages() {
    let (client, transport) = fixture_client(
        &cloud_settings(),
        vec![ok(page(
            "101",
            "Runbook",
            "<h2>Deploy</h2><p>Run <strong>make</strong> then <img src=\"data:image/png;base64,iVBORw0KGgo=\"/></p>",
        ))],
    );
    let output = run(
        &client,
        ConfluenceOperation::ReadPageById {
            page_id: "101",
            skip_images: true,
            content_format: None,
        },
    )
    .await;
    assert!(output.contains("## Deploy"), "{output}");
    assert!(output.contains("**make**"), "{output}");
    assert!(output.contains("[Image Removed]"), "{output}");
    assert!(!output.contains("base64"), "{output}");
    let request = &transport.requests()[0];
    assert_eq!(
        request.url,
        "https://tenant.atlassian.net/wiki/rest/api/content/101?expand=body.view%2Cversion"
    );
    let expected = format!(
        "Basic {}",
        base64::Engine::encode(
            &base64::engine::general_purpose::STANDARD,
            "bot@example.test:confluence-api-key"
        )
    );
    assert_eq!(request.authorization.as_deref(), Some(expected.as_str()));
    assert!(request.sensitive);

    let adf = r#"{"type":"doc","content":[]}"#;
    let (client, transport) = fixture_client(
        &cloud_settings(),
        vec![ok(json!({
            "id": "102", "title": "ADF",
            "body": {"atlas_doc_format": {"value": adf}},
            "_links": {"webui": "/spaces/DOCS/pages/102"}
        }))],
    );
    assert_eq!(
        run(
            &client,
            ConfluenceOperation::ReadPageById {
                page_id: "102",
                skip_images: false,
                content_format: Some("atlas_doc_format"),
            }
        )
        .await,
        adf
    );
    assert!(
        transport.requests()[0]
            .url
            .ends_with("expand=body.atlas_doc_format%2Cversion")
    );

    let (client, _) = fixture_client(
        &server_settings(),
        vec![status(
            StatusCode::NOT_FOUND,
            Some(
                json!({"statusCode": 404, "message": "No content found with id: ContentId{id=9}"}),
            ),
        )],
    );
    assert_eq!(
        run(
            &client,
            ConfluenceOperation::ReadPageById {
                page_id: "9",
                skip_images: false,
                content_format: None,
            }
        )
        .await,
        "Pages not found. Errors: [\"Confluence API Error: cannot fetch the page with ID 9: HTTP 404 Not Found: No content found with id: ContentId{id=9}\"]"
    );
}

#[tokio::test]
async fn searches_build_the_sdk_cql_page_through_results_and_read_each_page() {
    let (client, transport) = fixture_client(
        &cloud_settings(),
        vec![
            ok(json!({"results": [{"content": {"id": "1"}}, {"content": {"id": "2"}}]})),
            ok(page("1", "One", "<p>first</p>")),
            ok(page("2", "Two", "<p>second</p>")),
            ok(json!({"results": [{"content": {"id": "2"}}]})),
        ],
    );
    let output = run(
        &client,
        ConfluenceOperation::SearchPages {
            query: "deploy \"prod\"",
            skip_images: false,
        },
    )
    .await;
    let pages: Vec<Value> = serde_json::from_str(&output).expect("JSON list");
    assert_eq!(pages.len(), 2);
    assert_eq!(pages[0]["page_id"], "1");
    assert_eq!(pages[0]["content"].as_str().map(str::trim), Some("first"));
    assert_eq!(
        pages[0]["page_url"],
        "https://tenant.atlassian.net/wiki/spaces/DOCS/pages/1/One"
    );
    let requests = transport.requests();
    // ceil(max_pages 3 / limit 2) = 2 search pages; the second only repeats.
    assert_eq!(requests.len(), 4);
    let cql = reqwest::Url::parse(&requests[0].url)
        .expect("search URL")
        .query_pairs()
        .map(|(name, value)| (name.into_owned(), value.into_owned()))
        .collect::<Vec<(String, String)>>();
    assert_eq!(cql[0], ("start".to_owned(), "0".to_owned()));
    assert_eq!(cql[1], ("limit".to_owned(), "2".to_owned()));
    assert_eq!(
        cql[2].1,
        "(type=page and space=\"DOCS\") and (title~\"deploy \\\"prod\\\"\" or text~\"deploy \\\"prod\\\"\")"
    );
    assert!(requests[3].url.contains("start=2"));

    let (client, _) = fixture_client(&server_settings(), vec![ok(json!({"results": []}))]);
    assert_eq!(
        run(
            &client,
            ConfluenceOperation::SearchByTitle {
                query: "Nothing",
                skip_images: false
            }
        )
        .await,
        "Unable to find anything using query (type=page and space=\"ENG\") and (title~\"Nothing\"). Check space or query."
    );

    let (client, transport) = fixture_client(
        &server_settings(),
        vec![ok(json!({"results": [
            {"content": {"id": "7", "title": "Seven", "_links": {"self": "https://wiki.example.test/confluence/rest/api/content/7"}}, "excerpt": "seven @@@hl@@@"},
            {"content": {"id": "8", "title": "Eight", "_links": {"self": "https://wiki.example.test/confluence/rest/api/content/8"}}, "excerpt": ""}
        ]}))],
    );
    let output = run(&client, ConfluenceOperation::SiteSearch { query: "seven" }).await;
    assert_eq!(output.split("---").count(), 2);
    assert!(output.starts_with("{\"page_id\":\"7\""));
    assert!(
        transport.requests()[0]
            .url
            .contains("start=0&limit=10&cql=")
    );
    assert_eq!(
        transport.requests()[0].authorization.as_deref(),
        Some("Bearer confluence-private-token")
    );
}

#[tokio::test]
async fn label_reads_page_with_dedupe_and_list_reads_no_bodies() {
    let (client, transport) = fixture_client(
        &cloud_settings(),
        vec![
            ok(json!({"results": [{"id": "1", "title": "One"}, {"id": "2", "title": "Two"}]})),
            ok(json!({"results": [{"id": "2", "title": "Two"}, {"id": "3", "title": "Three"}]})),
        ],
    );
    assert_eq!(
        run(
            &client,
            ConfluenceOperation::ListPagesWithLabel { label: "runbook" }
        )
        .await,
        "[{\"id\":\"1\",\"title\":\"One\"},{\"id\":\"2\",\"title\":\"Two\"},{\"id\":\"3\",\"title\":\"Three\"}]"
    );
    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    assert_eq!(
        requests[0].url,
        "https://tenant.atlassian.net/wiki/rest/api/content/search?cql=type%3Dpage+AND+label%3D%22runbook%22&limit=2"
    );
    assert!(requests[1].url.ends_with("&start=2&limit=2"));

    let (client, _) = fixture_client(
        &cloud_settings(),
        vec![
            ok(json!({"results": [{"id": "1", "title": "One"}]})),
            ok(json!({"results": []})),
            ok(page("1", "One", "<p>Body</p>")),
        ],
    );
    let output = run(
        &client,
        ConfluenceOperation::GetPagesWithLabel { label: "runbook" },
    )
    .await;
    let pages: Vec<Value> = serde_json::from_str(&output).expect("JSON");
    assert_eq!(pages[0]["content"].as_str().map(str::trim), Some("Body"));
    assert_eq!(pages[0]["page_title"], "One");
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // The v1 and v2 create flows side by side.
async fn create_page_checks_duplicates_resolves_the_home_page_and_labels() {
    let (client, transport) = fixture_client(
        &cloud_settings(),
        vec![
            ok(json!({"results": []})),
            ok(json!({"key": "DOCS", "homepage": {"id": "65537"}})),
            status(
                StatusCode::OK,
                Some(json!({
                    "id": "200", "title": "Plan", "space": {"key": "DOCS"},
                    "version": {"by": {"displayName": "Elitea Bot"}, "number": 1},
                    "_links": {"webui": "/spaces/DOCS/pages/200/Plan", "edit": "/pages/resumedraft.action?draftId=200"}
                })),
            ),
            ok(json!({"results": [{"name": "plan"}]})),
            ok(json!({"results": [{"name": "elitea"}]})),
        ],
    );
    let output = run(
        &client,
        ConfluenceOperation::CreatePage {
            title: "Plan",
            body: "<p>Hello</p>",
            status: None,
            space: None,
            parent_id: None,
            representation: None,
            label: Some("plan"),
        },
    )
    .await;
    assert!(
        output.starts_with(
            "The page 'Plan' was created under parent page '65537': 'https://tenant.atlassian.net/wiki/spaces/DOCS/pages/200/Plan'. \nDetails: {"
        ),
        "{output}"
    );
    assert!(output.contains("\"label\":\"plan\""));
    let requests = transport.requests();
    assert!(
        requests[0]
            .url
            .contains("content?type=page&start=0&limit=1&spaceKey=%27DOCS%27&title=Plan")
    );
    assert_eq!(
        requests[1].url,
        "https://tenant.atlassian.net/wiki/rest/api/space/'DOCS'?expand=description.plain%2Chomepage"
    );
    assert_eq!(requests[2].method, Method::POST);
    assert_eq!(
        requests[2].url,
        "https://tenant.atlassian.net/wiki/rest/api/content"
    );
    assert_eq!(
        requests[2].body,
        Some(json!({
            "type": "page", "title": "Plan", "status": "current",
            "space": {"key": "'DOCS'"},
            "body": {"storage": {"value": "<p>Hello</p>", "representation": "storage"}},
            "metadata": {"properties": {
                "content-appearance-draft": {"value": "fixed-width"},
                "content-appearance-published": {"value": "fixed-width"}
            }},
            "ancestors": [{"type": "page", "id": "65537"}]
        }))
    );
    assert!(requests[2].effect);
    assert_eq!(
        requests[3].body,
        Some(json!({"prefix": "global", "name": "plan"}))
    );
    assert_eq!(
        requests[4].body,
        Some(json!({"prefix": "global", "name": "elitea"}))
    );

    let (client, transport) = fixture_client(
        &server_settings(),
        vec![ok(json!({"results": [{"id": "5", "title": "Plan"}]}))],
    );
    assert_eq!(
        run(
            &client,
            ConfluenceOperation::CreatePage {
                title: "Plan",
                body: "x",
                status: None,
                space: None,
                parent_id: None,
                representation: None,
                label: None,
            }
        )
        .await,
        "Page with title Plan already exists, please use other title."
    );
    assert_eq!(transport.requests().len(), 1);

    // v2: spaceId lookup, then POST /api/v2/pages, reshaped like v1.
    let (client, transport) = fixture_client(
        &with(cloud_settings(), "api_version", json!("2")),
        vec![
            ok(json!({"results": []})),
            ok(json!({"results": [{"id": "98304", "key": "DOCS"}]})),
            status(
                StatusCode::OK,
                Some(
                    json!({"id": "300", "title": "V2", "status": "current", "spaceId": "98304",
                            "version": {"number": 1, "authorId": "557058:abc"}, "_links": {}}),
                ),
            ),
            ok(json!({"results": []})),
        ],
    );
    let output = run(
        &client,
        ConfluenceOperation::CreatePage {
            title: "V2",
            body: "{}",
            status: None,
            space: Some("DOCS"),
            parent_id: Some("1"),
            representation: Some("adf"),
            label: None,
        },
    )
    .await;
    assert!(
        output.contains("'https://tenant.atlassian.net/wiki/spaces/DOCS/pages/300'"),
        "{output}"
    );
    assert!(output.contains("\"author\":\"557058:abc\""));
    let requests = transport.requests();
    assert_eq!(
        requests[1].url,
        "https://tenant.atlassian.net/wiki/api/v2/spaces?keys=DOCS&limit=1"
    );
    assert_eq!(
        requests[2].url,
        "https://tenant.atlassian.net/wiki/api/v2/pages"
    );
    assert_eq!(
        requests[2].body,
        Some(
            json!({"spaceId": "98304", "status": "current", "title": "V2",
                    "body": {"representation": "atlas_doc_format", "value": "{}"},
                    "parentId": "1"})
        )
    );
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Version, labels and the unchanged short-circuit together.
async fn update_keeps_storage_bodies_bumps_the_version_and_replaces_labels() {
    let current = json!({
        "id": "42", "title": "Guide", "space": {"key": "ENG"},
        "version": {"number": 7, "by": {"displayName": "Ann"}},
        "body": {"storage": {"value": "<p>Old</p><ac:structured-macro ac:name=\"toc\"/>"}},
        "_links": {"webui": "/display/ENG/Guide", "base": "https://wiki.example.test/confluence"}
    });
    let (client, transport) = fixture_client(
        &server_settings(),
        vec![
            ok(current.clone()),
            ok(json!({"results": []})),
            ok(json!({"lastUpdated": {"number": 7}})),
            ok(json!({
                "id": "42", "title": "Guide v2", "space": {"key": "ENG"},
                "version": {"number": 8, "by": {"displayName": "Elitea Bot"}},
                "_links": {"webui": "/display/ENG/Guide+v2", "base": "https://wiki.example.test/confluence"}
            })),
            ok(json!({"results": [{"name": "old"}]})),
            status(StatusCode::NO_CONTENT, None),
            ok(json!({"results": [{"name": "fresh"}]})),
        ],
    );
    let output = run(
        &client,
        ConfluenceOperation::UpdatePageById {
            page_id: "42",
            representation: None,
            new_title: Some("Guide v2"),
            new_body: None,
            new_labels: Some(vec!["fresh".to_owned()]),
        },
    )
    .await;
    assert!(output.starts_with(
        "The page '42' was updated successfully: 'https://wiki.example.test/confluence/display/ENG/Guide+v2'. \nDetails: {"
    ));
    assert!(output.contains("\"version\":8"));
    assert!(output.contains(
        "pages/diffpagesbyversion.action?pageId=42&selectedPageVersions=7&selectedPageVersions=8"
    ));
    assert!(output.contains("\"labels\":[\"fresh\"]"));
    let requests = transport.requests();
    assert_eq!(
        requests[0].url,
        "https://wiki.example.test/confluence/rest/api/content/42?expand=version%2Cbody.storage%2Cspace"
    );
    assert_eq!(requests[3].method, Method::PUT);
    assert_eq!(
        requests[3].url,
        "https://wiki.example.test/confluence/rest/api/content/42?status=current"
    );
    // The page keeps its STORAGE body (macros intact), at version 8.
    assert_eq!(
        requests[3].body.as_ref().expect("body")["body"],
        json!({"storage": {"value": "<p>Old</p><ac:structured-macro ac:name=\"toc\"/>", "representation": "storage"}})
    );
    assert_eq!(
        requests[3].body.as_ref().expect("body")["version"],
        json!({"number": 8, "minorEdit": false})
    );
    assert_eq!(requests[5].method, Method::DELETE);
    assert!(
        requests[5]
            .url
            .ends_with("/content/42/label?id=42&name=old")
    );
    assert_eq!(
        requests[6].body,
        Some(json!({"prefix": "global", "name": "fresh"}))
    );

    // Labels only: same title, same body, so no new version is written.
    let (client, transport) = fixture_client(
        &server_settings(),
        vec![
            ok(current.clone()),
            ok(json!({"results": []})),
            ok(json!({"results": [{"name": "x"}]})),
        ],
    );
    let output = run(
        &client,
        ConfluenceOperation::UpdateLabels {
            page_ids: Some(vec!["42".to_owned()]),
            new_labels: Some(vec!["x".to_owned()]),
        },
    )
    .await;
    assert!(output.starts_with("[\"The page '42' was updated successfully"));
    assert!(
        transport
            .requests()
            .iter()
            .all(|request| request.method != Method::PUT)
    );

    let (client, _) = fixture_client(
        &server_settings(),
        vec![status(
            StatusCode::NOT_FOUND,
            Some(json!({"message": "gone"})),
        )],
    );
    assert_eq!(
        run(
            &client,
            ConfluenceOperation::UpdatePageById {
                page_id: "404",
                representation: None,
                new_title: None,
                new_body: Some("x"),
                new_labels: None,
            }
        )
        .await,
        "Page with ID 404 not found."
    );
    let (client, _) = fixture_client(&server_settings(), Vec::new());
    assert_eq!(
        run(
            &client,
            ConfluenceOperation::UpdatePages {
                page_ids: Some(vec!["1".into(), "2".into(), "3".into()]),
                new_contents: Some(vec!["a".into(), "b".into()]),
                new_labels: None,
            }
        )
        .await,
        "New content should be provided for all the pages or it should contain only 1 new body for bulk update"
    );
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Delete, tree, lookup and generic cases together.
async fn delete_tree_title_lookup_and_generic_requests() {
    let (client, transport) = fixture_client(
        &server_settings(),
        vec![
            ok(json!({"results": [{"id": "77", "title": "Old"}]})),
            status(StatusCode::NO_CONTENT, None),
        ],
    );
    assert_eq!(
        run(
            &client,
            ConfluenceOperation::DeletePage {
                page_id: None,
                page_title: Some("Old"),
            }
        )
        .await,
        "Page with ID '77' has been successfully deleted."
    );
    assert_eq!(transport.requests()[1].method, Method::DELETE);
    assert!(transport.requests()[1].effect);
    let (client, _) = fixture_client(&server_settings(), Vec::new());
    assert_eq!(
        run(
            &client,
            ConfluenceOperation::DeletePage {
                page_id: None,
                page_title: None,
            }
        )
        .await,
        "Either page_id or page_title is required to delete the page"
    );

    let (client, transport) = fixture_client(
        &server_settings(),
        vec![
            ok(json!({"results": [{"id": "2", "title": "Child"}]})),
            ok(json!({"results": [{"id": "3", "title": "Grandchild"}]})),
            ok(json!({"results": []})),
        ],
    );
    assert_eq!(
        run(&client, ConfluenceOperation::GetPageTree { page_id: "1" }).await,
        "The list of pages under the '1' was extracted: {\"2\":[\"Child\",\"1\"],\"3\":[\"Grandchild\",\"2\"]}"
    );
    assert!(
        transport.requests()[0]
            .url
            .ends_with("/rest/api/content/1/child/page?start=0&limit=100")
    );

    let (client, transport) = fixture_client(
        &server_settings(),
        vec![ok(json!({"results": [{"id": "55", "title": "Spec"}]}))],
    );
    assert_eq!(
        run(
            &client,
            ConfluenceOperation::GetPageIdByTitle {
                title: "Spec",
                page_type: Some("blogpost"),
            }
        )
        .await,
        "55"
    );
    assert!(
        transport.requests()[0]
            .url
            .contains("content?type=blogpost&start=0&limit=1&spaceKey=ENG&title=Spec")
    );

    // Generic: SDK output shape (no separators), every status as text, the
    // path confined to the instance.
    let (client, transport) = fixture_client(
        &cloud_settings(),
        vec![status(
            StatusCode::BAD_REQUEST,
            Some(json!({"statusCode": 400, "message": "bad cql"})),
        )],
    );
    let output = run(
        &client,
        ConfluenceOperation::ExecuteGenericConfluence {
            method: "GET",
            relative_url: "/rest/api/search",
            params: Some(r#"{"cql": "type=page", "limit": 5}"#),
        },
    )
    .await;
    let body = output
        .strip_prefix("HTTP: GET/rest/api/search -> 400Bad Request")
        .expect("the SDK's unseparated status line");
    assert_eq!(
        serde_json::from_str::<Value>(body).expect("provider body"),
        json!({"statusCode": 400, "message": "bad cql"})
    );
    assert_eq!(
        transport.requests()[0].url,
        "https://tenant.atlassian.net/wiki/rest/api/search?cql=type%3Dpage&limit=5"
    );
    let (client, transport) = fixture_client(&cloud_settings(), Vec::new());
    for relative_url in ["https://evil.test/rest", "/rest/../../x", "/rest/api?x=1"] {
        assert!(
            run(
                &client,
                ConfluenceOperation::ExecuteGenericConfluence {
                    method: "GET",
                    relative_url,
                    params: None,
                }
            )
            .await
            .starts_with("Confluence tool exception. relative_url must be")
        );
    }
    assert!(transport.requests().is_empty());

    let (client, _) = fixture_client(
        &cloud_settings(),
        vec![status(StatusCode::BAD_GATEWAY, None)],
    );
    let error = client
        .execute(ConfluenceOperation::ExecuteGenericConfluence {
            method: "PUT",
            relative_url: "/rest/api/content/1",
            params: Some("{}"),
        })
        .await
        .expect_err("unknown outcome");
    assert_eq!(error.code(), ConfluenceClientErrorCode::UnknownOutcome);
    assert!(!error.retryable());
}
