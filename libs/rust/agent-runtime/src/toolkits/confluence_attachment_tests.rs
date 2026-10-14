//! The Confluence attachment tools against fakes: the artifact store and
//! model are `FakePlatform`; Confluence is a scripted transport. Fixtures
//! follow Confluence Cloud (REST v1 under `/wiki`, v2 `pages`) and
//! Server/Data Center replies.

#![allow(
    clippy::unnecessary_wraps,
    clippy::needless_pass_by_value,
    clippy::needless_lifetimes
)]

use std::collections::VecDeque;
use std::sync::{Arc, Mutex};

use adk_core::{ErrorCategory, ReadonlyContext, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::attachment_test_support::{FakePlatform, PNG};
use super::families::confluence::client::{
    ConfluenceApi, ConfluenceClient, ConfluenceClientError, ConfluenceClientErrorCode,
    ConfluenceHttpResponse, ConfluenceOperation, ConfluenceTransport,
};
use super::families::confluence::config::ConfluenceToolkitConfig;
use super::families::confluence::tools::{
    ConfluenceToolsetErrorCode, build_confluence_toolset, test_build_with_api,
    test_build_without_attachments,
};
use super::families::office_host::OfficeFailure;
use super::policy::ToolAdmissionPolicy;

fn cloud_settings() -> Map<String, Value> {
    json!({
        "confluence_configuration": {
            "base_url": "https://tenant.atlassian.net/wiki/",
            "hosting": "Cloud",
            "username": "bot@example.test",
            "api_key": "confluence-api-key"
        },
        "space": "DOCS",
        "api_version": "1"
    })
    .as_object()
    .cloned()
    .expect("object")
}

fn server_settings() -> Map<String, Value> {
    json!({
        "confluence_configuration": {
            "base_url": "https://wiki.example.test/confluence",
            "hosting": "Server",
            "token": "confluence-private-token"
        },
        "space": "ENG"
    })
    .as_object()
    .cloned()
    .expect("object")
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("conf-att").with_function_call_id("call"))
}

#[derive(Clone, Debug)]
struct Seen {
    method: Method,
    url: String,
    body: Vec<u8>,
    authorization: Option<String>,
    content_type: Option<String>,
    atlassian_token: Option<String>,
    effect: bool,
}

impl Seen {
    fn json(&self) -> Value {
        serde_json::from_slice(&self.body).unwrap_or(Value::Null)
    }
}

struct Script {
    responses: Mutex<VecDeque<Result<ConfluenceHttpResponse, ConfluenceClientErrorCode>>>,
    seen: Mutex<Vec<Seen>>,
}

impl Script {
    fn new(responses: Vec<Result<ConfluenceHttpResponse, ConfluenceClientErrorCode>>) -> Arc<Self> {
        Arc::new(Self {
            responses: Mutex::new(responses.into()),
            seen: Mutex::new(Vec::new()),
        })
    }

    fn seen(&self) -> Vec<Seen> {
        self.seen.lock().expect("seen").clone()
    }
}

#[async_trait]
impl ConfluenceTransport for Script {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<ConfluenceHttpResponse, ConfluenceClientError> {
        let header = |name| {
            request
                .headers()
                .get(name)
                .and_then(|value: &reqwest::header::HeaderValue| value.to_str().ok())
                .map(ToOwned::to_owned)
        };
        self.seen.lock().expect("seen").push(Seen {
            method: request.method().clone(),
            url: request.url().to_string(),
            body: request
                .body()
                .and_then(reqwest::Body::as_bytes)
                .map(<[u8]>::to_vec)
                .unwrap_or_default(),
            authorization: header(AUTHORIZATION),
            content_type: header(CONTENT_TYPE),
            atlassian_token: header(reqwest::header::HeaderName::from_static(
                "x-atlassian-token",
            )),
            effect,
        });
        match self.responses.lock().expect("responses").pop_front() {
            Some(Ok(response)) => Ok(response),
            Some(Err(code)) => Err(ConfluenceClientError::fixture(code, false)),
            None => Err(ConfluenceClientError::fixture(
                ConfluenceClientErrorCode::DependencyUnavailable,
                true,
            )),
        }
    }
}

type Reply = Result<ConfluenceHttpResponse, ConfluenceClientErrorCode>;

fn ok(body: Value) -> Reply {
    Ok(ConfluenceHttpResponse::fixture(StatusCode::OK, Some(body)))
}

fn status(code: StatusCode) -> Reply {
    Ok(ConfluenceHttpResponse::fixture(code, None))
}

fn refused(code: StatusCode, message: &str) -> Reply {
    Ok(ConfluenceHttpResponse::fixture(
        code,
        Some(json!({"message": message})),
    ))
}

fn bytes(code: StatusCode, body: &[u8], media_type: &str) -> Reply {
    Ok(ConfluenceHttpResponse::raw_fixture(
        code,
        body,
        Some(media_type),
        None,
    ))
}

fn redirect(to: &str) -> Reply {
    Ok(ConfluenceHttpResponse::raw_fixture(
        StatusCode::FOUND,
        b"",
        None,
        Some(to),
    ))
}

fn picture() -> Vec<u8> {
    let mut data = PNG.to_vec();
    data.resize(300, 0);
    data
}

fn setup(
    settings: &Map<String, Value>,
    responses: Vec<Reply>,
) -> (ConfluenceClient, Arc<Script>, Arc<FakePlatform>) {
    let script = Script::new(responses);
    let platform = FakePlatform::new();
    let client = ConfluenceClient::with_transport(
        ConfluenceToolkitConfig::parse(settings).expect("config"),
        script.clone(),
    )
    .with_attachments(Some(platform.host()));
    (client, script, platform)
}

async fn run(client: &ConfluenceClient, operation: ConfluenceOperation<'_>) -> String {
    client
        .execute(operation)
        .await
        .expect("operation")
        .as_str()
        .expect("text")
        .to_owned()
}

async fn run_value(client: &ConfluenceClient, operation: ConfluenceOperation<'_>) -> Value {
    client.execute(operation).await.expect("operation")
}

/// What the model sees of a failed call: a tool error (infrastructure
/// failures, kept generic by the invocation layer) or, for a failure with a
/// sentence of its own, the text result.
async fn fail(client: &ConfluenceClient, operation: ConfluenceOperation<'_>) -> adk_core::AdkError {
    match client.execute(operation).await {
        Err(error) => error.into_adk(),
        Ok(Value::String(sentence)) => adk_core::AdkError::new(
            adk_core::ErrorComponent::Tool,
            ErrorCategory::Internal,
            "tool.text_result",
            sentence,
        ),
        Ok(other) => panic!("must fail, got {other}"),
    }
}

fn contains(haystack: &[u8], needle: &str) -> bool {
    haystack
        .windows(needle.len())
        .any(|window| window == needle.as_bytes())
}

fn page_images(prompt: Option<&str>) -> ConfluenceOperation<'_> {
    ConfluenceOperation::GetPageWithImageDescriptions {
        page_id: "42",
        prompt,
        context_radius: 500,
    }
}

fn storage_page(html: &str) -> Reply {
    ok(json!({"id": "42", "title": "Design", "body": {"storage": {"value": html}}}))
}

fn attachment_listing(entries: Value) -> Reply {
    ok(json!({"results": entries, "size": 1}))
}

fn shot_entry() -> Value {
    json!({"id": "att1", "title": "shot.png", "_links": {"download": "/download/attachments/42/shot.png?version=1"}})
}

#[tokio::test]
async fn attached_linked_and_embedded_pictures_are_described() {
    let embedded = base64_of(&picture());
    let html = format!(
        "<p>Intro</p><ac:image ac:width=\"300\"><ri:attachment ri:filename=\"shot.png\" /></ac:image><p>Middle</p><ac:image><ri:url ri:value=\"https://wiki.example.test/confluence/download/attachments/42/linked.png?version=2&amp;api=v2\" /></ac:image><ac:image><ac:resource><ac:media-type>image/png</ac:media-type><ac:data>{embedded}</ac:data></ac:resource></ac:image>"
    );
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            storage_page(&html),
            attachment_listing(json!([shot_entry()])),
            bytes(StatusCode::OK, &picture(), "image/png"),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    platform.reply("First description.");
    platform.reply("Second description.");
    platform.reply("Third description.");
    let text = run(&client, page_images(None)).await;
    assert!(
        text.starts_with("Page 'Design' (ID: 42) with image descriptions:\n\n"),
        "{text}"
    );
    assert!(
        text.contains("Image shot.png Description: First description."),
        "{text}"
    );
    // The SDK names a linked picture by the last path segment, query included.
    assert!(
        text.contains("Image linked.png?version=2&api=v2 Description: Second description."),
        "{text}"
    );
    assert!(
        text.contains("Image embedded-image.png Description: Third description."),
        "{text}"
    );
    assert!(!text.contains("ac:image"));
    let seen = script.seen();
    assert_eq!(
        seen[0].url,
        "https://wiki.example.test/confluence/rest/api/content/42?expand=body.storage"
    );
    assert_eq!(
        seen[1].url,
        "https://wiki.example.test/confluence/rest/api/content/42/child/attachment?limit=50"
    );
    assert_eq!(
        seen[2].url,
        "https://wiki.example.test/confluence/download/attachments/42/shot.png"
    );
    // The SDK drops the query of a link that carries `version=`.
    assert_eq!(
        seen[3].url,
        "https://wiki.example.test/confluence/download/attachments/42/linked.png"
    );
    assert!(seen.iter().all(|request| !request.effect));
    let calls = platform.model_calls();
    assert_eq!(calls.len(), 3);
    assert!(
        calls[0]
            .prompt
            .starts_with("\n        ## Image Analysis Task:\n        Analyze this image in detail")
    );
    assert!(
        calls[0]
            .prompt
            .contains("- Image Name/Reference: shot.png\n")
    );
    assert!(
        calls[0]
            .prompt
            .contains("- Surrounding Content: <p>Intro</p><ac:image ac:width=\"300\">")
    );
    assert!(calls[0].prompt.ends_with("when relevant."));
}

fn base64_of(data: &[u8]) -> String {
    use base64::Engine as _;
    base64::engine::general_purpose::STANDARD.encode(data)
}

#[tokio::test]
async fn a_custom_prompt_replaces_the_default() {
    let (client, _, platform) = setup(
        &server_settings(),
        vec![
            storage_page("<ac:image><ri:attachment ri:filename=\"shot.png\"/></ac:image>"),
            attachment_listing(json!([shot_entry()])),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    platform.reply("ok");
    run(&client, page_images(Some("Be brief."))).await;
    assert!(
        platform.model_calls()[0]
            .prompt
            .starts_with("Be brief.\n\n## Additional Context Information:")
    );
}

#[tokio::test]
async fn unusable_picture_references_become_placeholders() {
    let html = "<ac:image><ri:attachment ri:filename=\"missing.png\"/></ac:image> <ac:image><ri:attachment ri:filename=\"shot.png\"/></ac:image> <ac:image><ri:url ri:value=\"https://evil.example.net/x.png\"/></ac:image> <ac:image><ac:resource><ac:media-type>image/png</ac:media-type><ac:data>!!</ac:data></ac:resource></ac:image>";
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            storage_page(html),
            attachment_listing(json!([shot_entry()])),
            bytes(StatusCode::OK, b"<html>login</html>", "text/html"),
        ],
    );
    let text = run(&client, page_images(None)).await;
    assert!(
        text.contains("[Image: missing.png - attachment not found]")
            || text.contains("Image: missing.png - attachment not found"),
        "{text}"
    );
    assert!(text.contains("Image: shot.png - download failed"), "{text}");
    assert!(text.contains("Image: x.png - download failed"), "{text}");
    assert!(
        text.contains("Image: embedded image/png - decode failed"),
        "{text}"
    );
    assert!(platform.model_calls().is_empty());
    assert!(
        script
            .seen()
            .iter()
            .all(|request| !request.url.contains("evil.example.net")),
        "a foreign link is never fetched"
    );
}

#[tokio::test]
async fn a_missing_page_and_a_refused_read_are_reported_as_text() {
    let (client, _, _) = setup(
        &server_settings(),
        vec![refused(StatusCode::NOT_FOUND, "no such page")],
    );
    assert_eq!(
        run(&client, page_images(None)).await,
        "Error processing page with images: There is no content with the given id, or the calling user does not have permission to view the content"
    );
    let (client, _, _) = setup(
        &server_settings(),
        vec![refused(StatusCode::FORBIDDEN, "denied")],
    );
    assert_eq!(
        run(&client, page_images(None)).await,
        "Error processing page with images: HTTP 403 Forbidden: denied"
    );
    let (client, _, _) = setup(&server_settings(), vec![status(StatusCode::UNAUTHORIZED)]);
    assert_eq!(
        fail(&client, page_images(None)).await.category,
        ErrorCategory::Unauthorized
    );
}

#[tokio::test]
async fn a_budget_refusal_stops_the_model_calls_and_no_model_says_so_in_the_sdk_words() {
    let html = "<ac:image><ri:attachment ri:filename=\"shot.png\"/></ac:image><ac:image><ri:url ri:value=\"/download/attachments/42/b.png\"/></ac:image>";
    let (client, _, platform) = setup(
        &server_settings(),
        vec![
            storage_page(html),
            attachment_listing(json!([shot_entry()])),
            bytes(StatusCode::OK, &picture(), "image/png"),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    platform.fail_model(OfficeFailure::BudgetRefused);
    let text = run(&client, page_images(None)).await;
    assert!(
        text.contains("not described: The model budget is exhausted."),
        "{text}"
    );
    assert_eq!(platform.model_calls().len(), 1);

    let (client, _, platform) = setup(
        &server_settings(),
        vec![
            storage_page(html),
            attachment_listing(json!([shot_entry()])),
            bytes(StatusCode::OK, &picture(), "image/png"),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    platform.fail_model(OfficeFailure::NoModel);
    let text = run(&client, page_images(None)).await;
    assert_eq!(
        text.matches("[LLM not available for image processing]")
            .count(),
        2,
        "{text}"
    );
    assert_eq!(platform.model_calls().len(), 1);
}

#[tokio::test]
#[allow(clippy::format_collect)]
async fn only_twenty_pictures_are_described_per_call() {
    let html = (0..22)
        .map(|n| {
            format!("<ac:image><ri:url ri:value=\"/download/attachments/42/p{n}.png\"/></ac:image>")
        })
        .collect::<String>();
    let mut script = vec![storage_page(&html)];
    for _ in 0..20 {
        script.push(bytes(StatusCode::OK, &picture(), "image/png"));
    }
    let (client, _, platform) = setup(&server_settings(), script);
    for _ in 0..20 {
        platform.reply("d");
    }
    let text = run(&client, page_images(None)).await;
    assert_eq!(platform.model_calls().len(), 20);
    assert!(
        text.contains("p20.png - not described: at most 20 pictures are described per call"),
        "{text}"
    );
}

fn page_attachments<'a>() -> ConfluenceOperation<'a> {
    ConfluenceOperation::GetPageAttachments {
        page_id: "42",
        max_content_length: 10_000,
        custom_prompt: None,
        allowed_extensions: None,
        name_pattern: None,
        max_attachments: 50,
    }
}

fn entry(id: &str, title: &str, media: &str, file_id: &str) -> Value {
    json!({
        "id": id,
        "title": title,
        "metadata": {"mediaType": media},
        "extensions": {"fileSize": 12, "fileId": file_id},
        "_links": {"download": format!("/download/attachments/42/{title}?version=1")}
    })
}

fn history(by: &str) -> Reply {
    ok(
        json!({"createdBy": {"displayName": by}, "createdDate": "2026-10-01T09:00:00.000Z", "lastUpdated": {"when": "2026-10-02T09:00:00.000Z", "number": 1}}),
    )
}

#[tokio::test]
async fn text_attachments_are_read_pictures_described_and_unparsable_ones_say_so() {
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            attachment_listing(json!([
                entry("a1", "notes.txt", "text/plain", ""),
                entry("a2", "table.csv", "text/csv", ""),
                entry("a3", "page.html", "text/html", ""),
                entry("a4", "shot.png", "image/png", ""),
                entry("a5", "spec.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", ""),
                entry("a6", "plan.pdf", "application/pdf", ""),
                entry("a7", "data.bin", "application/x-foo", "")
            ])),
            history("Ann"),
            history("Ann"),
            history("Ann"),
            history("Ann"),
            history("Ann"),
            history("Ann"),
            history("Ann"),
            bytes(StatusCode::OK, "héllo\nworld".as_bytes(), "text/plain"),
            bytes(StatusCode::OK, b"a,\"b,c\"\r\n1,2\r\n", "text/csv"),
            bytes(StatusCode::OK, b"<html><head><style>p{}</style></head><body><p>Hello &amp; <b>bye</b></p><script>x()</script></body></html>", "text/html"),
            bytes(StatusCode::OK, &picture(), "image/png"),
            bytes(StatusCode::OK, b"PK\x03\x04junk", "application/octet-stream"),
        ],
    );
    platform.reply("A diagram.");
    let result = run_value(&client, page_attachments()).await;
    let items = result.as_array().expect("a list");
    assert_eq!(items.len(), 7);
    assert_eq!(items[0]["metadata"]["name"], "notes.txt");
    assert_eq!(items[0]["metadata"]["creator"], "Ann");
    assert_eq!(items[0]["metadata"]["created"], "2026-10-01T09:00:00.000Z");
    assert_eq!(items[0]["metadata"]["updated"], "2026-10-02T09:00:00.000Z");
    assert_eq!(items[0]["metadata"]["size"], 12);
    assert_eq!(
        items[0]["metadata"]["download_url"],
        "https://wiki.example.test/confluence/download/attachments/42/notes.txt?version=1"
    );
    assert_eq!(items[0]["comments"], json!([]));
    assert_eq!(items[0]["content"], "héllo\nworld");
    assert_eq!(items[0]["llm_analysis"], Value::Null);
    assert_eq!(items[1]["content"], "a, b,c\n1, 2");
    assert_eq!(items[2]["content"], "Hello & bye");
    assert_eq!(items[3]["content"], Value::Null);
    assert_eq!(items[3]["llm_analysis"], "A diagram.");
    assert_eq!(
        items[4]["content"],
        "[Text extraction from .docx files is not available in this runtime]"
    );
    assert_eq!(
        items[5]["llm_analysis"],
        "[PDF page analysis is not available in this runtime]"
    );
    assert_eq!(items[6]["content"], Value::Null);
    let calls = platform.model_calls();
    assert_eq!(calls.len(), 1);
    assert!(
        calls[0]
            .prompt
            .contains("- Surrounding Content: Attachment: shot.png (type: image/png)\n")
    );
    assert!(script.seen().iter().all(|request| !request.effect));
    // The listing asks for 50 and Server never inspects the body.
    assert_eq!(
        script.seen()[0].url,
        "https://wiki.example.test/confluence/rest/api/content/42/child/attachment?limit=50"
    );
}

#[tokio::test]
async fn content_and_analysis_are_cut_at_max_content_length() {
    let (client, _, _) = setup(
        &server_settings(),
        vec![
            attachment_listing(json!([entry("a1", "notes.txt", "text/plain", "")])),
            history("Ann"),
            bytes(StatusCode::OK, "ä".repeat(30).as_bytes(), "text/plain"),
        ],
    );
    let result = run_value(
        &client,
        ConfluenceOperation::GetPageAttachments {
            page_id: "42",
            max_content_length: 10,
            custom_prompt: None,
            allowed_extensions: None,
            name_pattern: None,
            max_attachments: 50,
        },
    )
    .await;
    assert_eq!(
        result[0]["content"],
        format!(
            "{}\n...[truncated, showing first 10 characters]",
            "ä".repeat(10)
        )
    );
}

#[tokio::test]
async fn extension_and_name_filters_follow_the_sdk() {
    let listing = || {
        attachment_listing(json!([
            entry("a1", "report_q1.pdf", "application/pdf", ""),
            entry("a2", "report_q1.TXT", "text/plain", ""),
            entry("a3", "other.txt", "text/plain", "")
        ]))
    };
    let (client, _, _) = setup(&server_settings(), vec![listing(), history("A")]);
    let result = run_value(
        &client,
        ConfluenceOperation::GetPageAttachments {
            page_id: "42",
            max_content_length: 10_000,
            custom_prompt: None,
            allowed_extensions: Some(vec!["pdf".to_owned()]),
            name_pattern: Some("^report_.*"),
            max_attachments: 50,
        },
    )
    .await;
    let names = result
        .as_array()
        .expect("list")
        .iter()
        .map(|item| item["metadata"]["name"].as_str().expect("name").to_owned())
        .collect::<Vec<_>>();
    assert_eq!(names, ["report_q1.pdf"]);

    // `re.match` anchors at the start.
    let (client, _, _) = setup(
        &server_settings(),
        vec![
            listing(),
            history("A"),
            bytes(StatusCode::OK, b"x", "text/plain"),
        ],
    );
    let result = run_value(
        &client,
        ConfluenceOperation::GetPageAttachments {
            page_id: "42",
            max_content_length: 10_000,
            custom_prompt: None,
            allowed_extensions: None,
            name_pattern: Some("other"),
            max_attachments: 50,
        },
    )
    .await;
    assert_eq!(result.as_array().map(Vec::len), Some(1));

    let (client, _, _) = setup(&server_settings(), vec![listing()]);
    let error = fail(
        &client,
        ConfluenceOperation::GetPageAttachments {
            page_id: "42",
            max_content_length: 10_000,
            custom_prompt: None,
            allowed_extensions: None,
            name_pattern: Some("("),
            max_attachments: 50,
        },
    )
    .await;
    assert_eq!(
        error.message,
        "name_pattern is not a valid regular expression."
    );
}

#[tokio::test]
async fn more_matches_than_the_limit_end_with_a_notice() {
    let (client, _, _) = setup(
        &server_settings(),
        vec![
            attachment_listing(json!([
                entry("a1", "one.txt", "text/plain", ""),
                entry("a2", "two.txt", "text/plain", "")
            ])),
            history("A"),
            bytes(StatusCode::OK, b"1", "text/plain"),
        ],
    );
    let result = run_value(
        &client,
        ConfluenceOperation::GetPageAttachments {
            page_id: "42",
            max_content_length: 10_000,
            custom_prompt: None,
            allowed_extensions: None,
            name_pattern: None,
            max_attachments: 1,
        },
    )
    .await;
    let items = result.as_array().expect("list");
    assert_eq!(items.len(), 2);
    assert_eq!(
        items[1]["notice"],
        "Showing the first 1 matching visible attachments; more exist — narrow with allowed_extensions/name_pattern or raise max_attachments."
    );
}

#[tokio::test]
async fn cloud_hides_files_removed_from_the_page_body_and_pages_through_the_listing() {
    let body = json!({"body": {"atlas_doc_format": {"value": serde_json::to_string(&json!({
        "type": "doc",
        "content": [{"type": "mediaSingle", "content": [{"type": "media", "attrs": {"id": "file-keep", "type": "file"}}]}]
    })).expect("adf")}}});
    let first = json!({
        "results": [entry("a1", "keep.txt", "text/plain", "file-keep")],
        "_links": {"next": "/rest/api/content/42/child/attachment?limit=50&start=50"}
    });
    let second = json!({"results": [entry("a2", "removed.txt", "text/plain", "file-gone")]});
    let (client, script, _) = setup(
        &cloud_settings(),
        vec![
            ok(first),
            ok(body),
            ok(second),
            history("Ann"),
            bytes(StatusCode::OK, b"kept", "text/plain"),
        ],
    );
    let result = run_value(&client, page_attachments()).await;
    let items = result.as_array().expect("list");
    assert_eq!(items.len(), 1);
    assert_eq!(items[0]["content"], "kept");
    let seen = script.seen();
    assert_eq!(
        seen[1].url,
        "https://tenant.atlassian.net/wiki/api/v2/pages/42?body-format=atlas_doc_format"
    );
    assert_eq!(
        seen[2].url,
        "https://tenant.atlassian.net/wiki/rest/api/content/42/child/attachment?limit=50&start=50"
    );
    assert_eq!(
        seen[4].url,
        "https://tenant.atlassian.net/wiki/download/attachments/42/keep.txt?version=1"
    );
    assert_eq!(
        items[0]["metadata"]["download_url"],
        "https://tenant.atlassian.net/wiki/download/attachments/42/keep.txt?version=1"
    );
}

#[tokio::test]
async fn attachments_that_the_body_does_not_show_are_counted_not_returned() {
    let body = json!({"body": {"atlas_doc_format": {"value": serde_json::to_string(&json!({
        "type": "doc",
        "content": [{"type": "paragraph", "content": [{"type": "text", "text": "no files"}]}]
    })).expect("adf")}}});
    let (client, _, _) = setup(
        &cloud_settings(),
        vec![
            attachment_listing(json!([entry("a1", "gone.txt", "text/plain", "file-gone")])),
            ok(body),
        ],
    );
    assert_eq!(
        run(&client, page_attachments()).await,
        "No attachments are visible on page 42: 1 attachment(s) exist but are not referenced in the page body."
    );
}

#[tokio::test]
async fn no_attachments_and_a_missing_page_are_answered_as_text() {
    let (client, _, _) = setup(&server_settings(), vec![ok(json!({"results": []}))]);
    assert_eq!(
        run(&client, page_attachments()).await,
        "No attachments found for page ID 42."
    );
    let (client, _, _) = setup(
        &server_settings(),
        vec![refused(StatusCode::NOT_FOUND, "x")],
    );
    assert_eq!(
        run(&client, page_attachments()).await,
        "Error retrieving attachments: There is no content with the given id, or the calling user does not have permission to view the content"
    );
}

#[tokio::test]
async fn a_failed_download_is_reported_inside_the_entry() {
    let (client, _, _) = setup(
        &server_settings(),
        vec![
            attachment_listing(json!([
                entry("a1", "gone.txt", "text/plain", ""),
                entry("a2", "big.txt", "text/plain", "")
            ])),
            history("A"),
            history("A"),
            status(StatusCode::NOT_FOUND),
            Err(ConfluenceClientErrorCode::ResourceExhausted),
        ],
    );
    let result = run_value(&client, page_attachments()).await;
    assert_eq!(result[0]["content"], "[Failed to download: HTTP 404]");
    assert_eq!(
        result[1]["content"],
        "[Failed to download: larger than 10 MiB]"
    );
}

#[tokio::test]
async fn cloud_downloads_follow_the_media_redirect_without_the_credential() {
    let (client, script, _) = setup(
        &server_settings(),
        vec![
            attachment_listing(json!([entry("a1", "notes.txt", "text/plain", "")])),
            history("A"),
            redirect("https://api.media.atlassian.com/file/x/binary?token=t"),
            bytes(StatusCode::OK, b"via media", "text/plain"),
        ],
    );
    let result = run_value(&client, page_attachments()).await;
    assert_eq!(result[0]["content"], "via media");
    let seen = script.seen();
    assert!(seen[2].authorization.is_some());
    assert_eq!(
        seen[3].url,
        "https://api.media.atlassian.com/file/x/binary?token=t"
    );
    assert!(seen[3].authorization.is_none());

    for target in [
        "https://evil.example.net/x",
        "http://api.media.atlassian.com/x",
    ] {
        let (client, script, _) = setup(
            &server_settings(),
            vec![
                attachment_listing(json!([entry("a1", "notes.txt", "text/plain", "")])),
                history("A"),
                redirect(target),
            ],
        );
        let result = run_value(&client, page_attachments()).await;
        assert_eq!(
            result[0]["content"],
            "[Error downloading content: the download was refused or failed]"
        );
        assert_eq!(script.seen().len(), 3);
    }
}

fn add_file<'a>(
    filepath: &'a str,
    caption: Option<&'a str>,
    prepend: bool,
) -> ConfluenceOperation<'a> {
    ConfluenceOperation::AddFileToPage {
        page_id: "42",
        filepath,
        filename: None,
        caption,
        prepend,
    }
}

fn upload_replies(page_body: &str) -> Vec<Reply> {
    vec![
        ok(json!({"results": [], "size": 0})),
        ok(json!({"results": [{"title": "shot.png"}], "size": 1})),
        ok(
            json!({"id": "42", "title": "Design", "body": {"storage": {"value": page_body}}, "version": {"number": 3}, "_links": {"webui": "/spaces/ENG/pages/42/Design"}}),
        ),
        ok(json!({"id": "42"})),
    ]
}

#[tokio::test]
async fn a_picture_is_uploaded_and_placed_in_the_page_with_its_caption() {
    let (client, script, platform) = setup(&server_settings(), upload_replies("<p>Body</p>"));
    platform.put("imgs", "shot.png", &picture());
    let text = run(
        &client,
        add_file("/imgs/shot.png", Some("A <b> & caption"), false),
    )
    .await;
    assert_eq!(
        text,
        "File 'shot.png' uploaded and added to page 42 as image. View at: https://wiki.example.test/confluence/spaces/ENG/pages/42/Design"
    );
    let seen = script.seen();
    assert_eq!(seen.len(), 4);
    assert_eq!(
        seen[0].url,
        "https://wiki.example.test/confluence/rest/api/content/42/child/attachment?filename=shot.png"
    );
    assert_eq!(seen[1].method, Method::POST);
    assert_eq!(
        seen[1].url,
        "https://wiki.example.test/confluence/rest/api/content/42/child/attachment"
    );
    assert!(seen[1].effect);
    assert_eq!(seen[1].atlassian_token.as_deref(), Some("no-check"));
    assert!(
        seen[1]
            .content_type
            .as_deref()
            .is_some_and(|value| value.starts_with("multipart/form-data; boundary="))
    );
    for part in [
        "name=\"type\"\r\n\r\nattachment",
        "name=\"fileName\"\r\n\r\nshot.png",
        "name=\"contentType\"\r\n\r\nimage/png",
        "name=\"comment\"\r\n\r\nUploaded shot.png.",
        "name=\"minorEdit\"\r\n\r\ntrue",
        "name=\"file\"; filename=\"shot.png\"",
    ] {
        assert!(contains(&seen[1].body, part), "{part}");
    }
    assert_eq!(seen[3].method, Method::PUT);
    assert!(seen[3].effect);
    assert_eq!(
        seen[3].url,
        "https://wiki.example.test/confluence/rest/api/content/42?status=current"
    );
    let payload = seen[3].json();
    assert_eq!(payload["version"]["number"], 4);
    assert_eq!(payload["version"]["message"], "Added file: shot.png");
    assert_eq!(
        payload["body"]["storage"]["value"],
        "<p>Body</p><p></p><ac:image><ri:attachment ri:filename=\"shot.png\"/><ac:caption><p>A &lt;b&gt; &amp; caption</p></ac:caption></ac:image>"
    );
}

#[tokio::test]
async fn a_file_is_linked_and_prepended_and_a_same_named_one_gets_a_new_version() {
    let mut replies = upload_replies("<p>Body</p>");
    replies[0] = ok(json!({"results": [{"id": "att9", "title": "spec.pdf"}], "size": 1}));
    replies[1] = ok(json!({"id": "att9", "title": "spec.pdf"}));
    let (client, script, platform) = setup(&server_settings(), replies);
    platform.put("docs", "spec.pdf", b"%PDF-1.7");
    let text = run(&client, add_file("/docs/spec.pdf", Some("ignored"), true)).await;
    assert!(
        text.starts_with("File 'spec.pdf' uploaded and added to page 42 as file. View at: https://wiki.example.test/confluence/spaces/ENG/pages/42/Design Note: the page already had an attachment named 'spec.pdf'; a new version of it was uploaded"),
        "{text}"
    );
    let seen = script.seen();
    assert_eq!(
        seen[1].url,
        "https://wiki.example.test/confluence/rest/api/content/42/child/attachment/att9/data"
    );
    assert_eq!(
        seen[3].json()["body"]["storage"]["value"],
        "<ac:link><ri:attachment ri:filename=\"spec.pdf\"/></ac:link><p></p><p>Body</p>"
    );
}

#[tokio::test]
async fn nothing_is_sent_when_the_artifact_cannot_be_used() {
    for (failure, expected) in [
        (OfficeFailure::NotFound, "The artifact was not found."),
        (
            OfficeFailure::AccessRefused,
            "refused access to the artifact store",
        ),
        (
            OfficeFailure::Unreachable,
            "could not be reached; nothing was sent",
        ),
    ] {
        let (client, script, platform) = setup(&server_settings(), Vec::new());
        platform.put("docs", "spec.pdf", b"%PDF");
        platform.fail_reads(failure);
        let error = fail(&client, add_file("/docs/spec.pdf", None, false)).await;
        assert!(error.message.contains(expected), "{}", error.message);
        assert!(script.seen().is_empty());
    }
    let (client, script, platform) = setup(&server_settings(), Vec::new());
    platform.put("docs", "empty.txt", b"");
    platform.put("docs", "ok.txt", b"x");
    platform.put("docs", "big.bin", &vec![0_u8; 50 * 1024 * 1024 + 1]);
    for (path, expected) in [
        ("/docs/empty.txt", "empty"),
        ("/docs/big.bin", "50 MiB"),
        ("bad", "artifact path is not valid"),
    ] {
        let error = fail(&client, add_file(path, None, false)).await;
        assert!(error.message.contains(expected), "{}", error.message);
    }
    let error = fail(
        &client,
        ConfluenceOperation::AddFileToPage {
            page_id: "42",
            filepath: "/docs/ok.txt",
            filename: Some("a/b.txt"),
            caption: None,
            prepend: false,
        },
    )
    .await;
    assert!(
        error.message.contains("file name is not valid"),
        "{}",
        error.message
    );
    assert!(script.seen().is_empty());
}

#[tokio::test]
async fn each_upload_failure_has_its_sentence_and_the_upload_is_never_retried() {
    let cases: Vec<(Reply, &str)> = vec![
        (
            status(StatusCode::UNAUTHORIZED),
            "Confluence authentication failed",
        ),
        (
            status(StatusCode::TOO_MANY_REQUESTS),
            "Confluence rate limited the request",
        ),
        (
            status(StatusCode::BAD_GATEWAY),
            "Confluence may have applied the requested effect; reconcile it before retrying",
        ),
        (
            Err(ConfluenceClientErrorCode::UnknownOutcome),
            "Confluence may have applied the requested effect",
        ),
        (
            refused(StatusCode::FORBIDDEN, "denied"),
            "Failed to upload attachment 'shot.png' to page 42: Attachments are disabled or the calling user does not have permission to add attachments to this content. Nothing was uploaded.",
        ),
        (
            refused(StatusCode::NOT_FOUND, "missing"),
            "the attachment exceeds the maximum configured attachment size. Nothing was uploaded.",
        ),
        (
            refused(StatusCode::BAD_REQUEST, "bad"),
            "Failed to upload attachment 'shot.png' to page 42: HTTP 400 Bad Request: bad. Nothing was uploaded.",
        ),
    ];
    for (reply, expected) in cases {
        let (client, script, platform) = setup(
            &server_settings(),
            vec![ok(json!({"results": [], "size": 0})), reply],
        );
        platform.put("imgs", "shot.png", &picture());
        let error = fail(&client, add_file("/imgs/shot.png", None, false)).await;
        assert!(
            error.message.contains(expected),
            "{} / {expected}",
            error.message
        );
        assert_eq!(script.seen().len(), 2, "the upload is sent once");
    }
    // The pre-check refusing (page not found) sends nothing.
    let (client, script, platform) = setup(
        &server_settings(),
        vec![refused(StatusCode::NOT_FOUND, "no page")],
    );
    platform.put("imgs", "shot.png", &picture());
    let error = fail(&client, add_file("/imgs/shot.png", None, false)).await;
    assert!(
        error.message.contains("Nothing was uploaded."),
        "{}",
        error.message
    );
    assert_eq!(script.seen().len(), 1);
}

#[tokio::test]
async fn a_page_that_cannot_be_updated_says_the_file_was_uploaded() {
    let mut replies = upload_replies("<p>x</p>");
    replies[3] = refused(StatusCode::BAD_REQUEST, "Invalid body");
    let (client, _, platform) = setup(&server_settings(), replies);
    platform.put("imgs", "shot.png", &picture());
    let error = fail(&client, add_file("/imgs/shot.png", None, false)).await;
    assert_eq!(
        error.message,
        "The file 'shot.png' was uploaded to page 42, but the page content was NOT updated: HTTP 400 Bad Request: Invalid body. Do not upload the file again."
    );

    let mut replies = upload_replies("<p>x</p>");
    replies[3] = status(StatusCode::INTERNAL_SERVER_ERROR);
    let (client, _, platform) = setup(&server_settings(), replies);
    platform.put("imgs", "shot.png", &picture());
    let error = fail(&client, add_file("/imgs/shot.png", None, false)).await;
    assert!(
        error
            .message
            .contains("whether the page content was updated is unknown"),
        "{}",
        error.message
    );
}

#[tokio::test]
async fn a_version_conflict_re_reads_the_page_and_tries_again_up_to_three_times() {
    let conflict = || refused(StatusCode::CONFLICT, "stale");
    let page = || {
        ok(
            json!({"id": "42", "title": "Design", "body": {"storage": {"value": "<p>x</p>"}}, "version": {"number": 3}, "_links": {"webui": "/w"}}),
        )
    };
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            ok(json!({"results": [], "size": 0})),
            ok(json!({"results": [{"title": "shot.png"}]})),
            page(),
            conflict(),
            page(),
            ok(json!({"id": "42"})),
        ],
    );
    platform.put("imgs", "shot.png", &picture());
    let text = run(&client, add_file("/imgs/shot.png", None, false)).await;
    assert!(
        text.starts_with("File 'shot.png' uploaded and added to page 42 as image."),
        "{text}"
    );
    assert_eq!(
        script
            .seen()
            .iter()
            .filter(|r| r.method == Method::POST)
            .count(),
        1
    );

    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            ok(json!({"results": [], "size": 0})),
            ok(json!({"results": [{"title": "shot.png"}]})),
            page(),
            conflict(),
            page(),
            conflict(),
            page(),
            conflict(),
        ],
    );
    platform.put("imgs", "shot.png", &picture());
    let error = fail(&client, add_file("/imgs/shot.png", None, false)).await;
    assert!(
        error
            .message
            .contains("was NOT updated: HTTP 409 Conflict: stale"),
        "{}",
        error.message
    );
    assert_eq!(
        script
            .seen()
            .iter()
            .filter(|r| r.method == Method::PUT)
            .count(),
        3
    );
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new()).expect("policy"))
}

#[tokio::test]
async fn the_tools_have_the_sdk_names_flags_and_closed_schemas() {
    let (client, script, platform) = setup(&server_settings(), Vec::new());
    let api: Arc<dyn ConfluenceApi> = Arc::new(client);
    let names = [
        "get_page_with_image_descriptions",
        "get_page_attachments",
        "add_file_to_page",
    ];
    let toolset = test_build_with_api("wiki", &names, &policy(), &api).expect("toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("tools");
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        names
    );
    for tool in &tools {
        let read = tool.name() != "add_file_to_page";
        assert_eq!(tool.is_read_only(), read, "{}", tool.name());
        assert_eq!(tool.is_concurrency_safe(), read, "{}", tool.name());
        assert_eq!(
            tool.parameters_schema().expect("schema")["additionalProperties"],
            json!(false)
        );
        if !read {
            assert!(tool.description().contains("reconciled"));
        }
    }
    let find = |name: &str| {
        tools
            .iter()
            .find(|tool| tool.name() == name)
            .expect("tool")
            .clone()
    };
    for (tool, arguments) in [
        ("add_file_to_page", json!({"page_id": "1"})),
        (
            "add_file_to_page",
            json!({"page_id": "1", "filepath": "/b/f", "extra": true}),
        ),
        ("get_page_attachments", json!({})),
        (
            "get_page_attachments",
            json!({"page_id": "1", "max_content_length": "lots"}),
        ),
        (
            "get_page_attachments",
            json!({"page_id": "1", "allowed_extensions": "pdf"}),
        ),
        (
            "get_page_with_image_descriptions",
            json!({"page_id": "1", "context_radius": 1.5}),
        ),
    ] {
        let error = find(tool)
            .execute(context(), arguments)
            .await
            .expect_err("invalid arguments");
        assert_eq!(error.category, ErrorCategory::InvalidInput, "{tool}");
    }
    assert!(script.seen().is_empty());
    assert!(platform.model_calls().is_empty());
}

#[tokio::test]
async fn without_attachment_authority_the_tools_are_omitted() {
    let script = Script::new(Vec::new());
    let client: Arc<dyn ConfluenceApi> = Arc::new(ConfluenceClient::with_transport(
        ConfluenceToolkitConfig::parse(&server_settings()).expect("config"),
        script,
    ));
    let toolset = test_build_without_attachments("wiki", &[], &policy(), &client).expect("toolset");
    let names = toolset
        .tools(context())
        .await
        .expect("tools")
        .iter()
        .map(|tool| tool.name().to_owned())
        .collect::<Vec<_>>();
    assert_eq!(names.len(), 16);
    assert!(!names.iter().any(|name| matches!(
        name.as_str(),
        "get_page_with_image_descriptions" | "get_page_attachments" | "add_file_to_page"
    )));
    let Err(error) =
        test_build_without_attachments("wiki", &["add_file_to_page"], &policy(), &client)
    else {
        panic!("only an omitted tool selected");
    };
    assert_eq!(
        error.code(),
        ConfluenceToolsetErrorCode::UnsupportedSelection
    );
    let mut settings = server_settings();
    settings.insert("selected_tools".to_owned(), json!(["get_page_attachments"]));
    let config = ConfluenceToolkitConfig::parse(&settings).expect("config");
    let Err(error) = build_confluence_toolset("wiki", config, &policy(), None) else {
        panic!("nothing to serve without a host");
    };
    assert_eq!(
        error.code(),
        ConfluenceToolsetErrorCode::UnsupportedSelection
    );
}

#[tokio::test]
async fn the_next_version_comes_from_the_same_read_as_the_body() {
    // The page read says version 7; only that read is used (no second read of
    // the history that a concurrent edit could slip between).
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            ok(json!({"results": [], "size": 0})),
            ok(json!({"results": [{"title": "shot.png"}]})),
            ok(
                json!({"id": "42", "title": "T", "body": {"storage": {"value": "<p>x</p>"}}, "version": {"number": 7}, "_links": {"webui": "/w"}}),
            ),
            ok(json!({"id": "42"})),
        ],
    );
    platform.put("imgs", "shot.png", &picture());
    run(&client, add_file("/imgs/shot.png", None, false)).await;
    let seen = script.seen();
    assert_eq!(seen.len(), 4);
    assert!(seen.iter().all(|r| !r.url.contains("/history")));
    assert_eq!(seen[3].json()["version"]["number"], 8);
    assert_eq!(seen[3].json()["type"], "page");
}

#[tokio::test]
async fn a_concurrent_edit_is_a_conflict_that_re_reads_and_a_blog_post_keeps_its_type() {
    let blog = |version: u64| {
        ok(
            json!({"id": "42", "type": "blogpost", "title": "News", "body": {"storage": {"value": "<p>x</p>"}}, "version": {"number": version}, "_links": {"webui": "/w"}}),
        )
    };
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            ok(json!({"results": [], "size": 0})),
            ok(json!({"results": [{"title": "shot.png"}]})),
            blog(3),
            refused(StatusCode::CONFLICT, "stale"),
            blog(4),
            ok(json!({"id": "42"})),
        ],
    );
    platform.put("imgs", "shot.png", &picture());
    let text = run(&client, add_file("/imgs/shot.png", None, false)).await;
    assert!(
        text.starts_with("File 'shot.png' uploaded and added to page 42"),
        "{text}"
    );
    let puts: Vec<_> = script
        .seen()
        .into_iter()
        .filter(|r| r.method == Method::PUT)
        .collect();
    assert_eq!(puts.len(), 2);
    assert_eq!(puts[0].json()["version"]["number"], 4);
    assert_eq!(puts[1].json()["version"]["number"], 5);
    assert!(puts.iter().all(|r| r.json()["type"] == "blogpost"));
}

#[tokio::test]
async fn an_absolute_cloud_link_keeps_the_wiki_prefix_once() {
    let html = "<ac:image><ri:url ri:value=\"https://tenant.atlassian.net/wiki/download/attachments/42/a.png?version=1\"/></ac:image><ac:image><ri:url ri:value=\"https://tenant.atlassian.net/download/attachments/42/b.png\"/></ac:image><ac:image><ri:url ri:value=\"/wiki/download/attachments/42/c.png\"/></ac:image>";
    let (client, script, platform) = setup(
        &cloud_settings(),
        vec![
            storage_page(html),
            bytes(StatusCode::OK, &picture(), "image/png"),
            bytes(StatusCode::OK, &picture(), "image/png"),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    for _ in 0..3 {
        platform.reply("a picture");
    }
    let text = run(&client, page_images(None)).await;
    assert_eq!(text.matches("Description: a picture").count(), 3, "{text}");
    let urls: Vec<String> = script.seen().into_iter().skip(1).map(|r| r.url).collect();
    assert_eq!(
        urls,
        [
            "https://tenant.atlassian.net/wiki/download/attachments/42/a.png",
            "https://tenant.atlassian.net/wiki/download/attachments/42/b.png",
            "https://tenant.atlassian.net/wiki/download/attachments/42/c.png",
        ]
    );
}

#[tokio::test]
async fn once_the_run_has_no_model_no_further_picture_is_downloaded() {
    let html = "<ac:image><ri:url ri:value=\"/download/attachments/42/a.png\"/></ac:image><ac:image><ri:url ri:value=\"/download/attachments/42/b.png\"/></ac:image><ac:image><ri:url ri:value=\"/download/attachments/42/c.png\"/></ac:image>";
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            storage_page(html),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    platform.fail_model(OfficeFailure::NoModel);
    let text = run(&client, page_images(None)).await;
    assert_eq!(
        text.matches("[LLM not available for image processing]")
            .count(),
        3,
        "{text}"
    );
    // The page read and ONE picture download; the other two were not fetched.
    assert_eq!(script.seen().len(), 2);
    assert_eq!(platform.model_calls().len(), 1);
}
