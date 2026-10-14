//! The Jira attachment tools against fakes: the artifact store and model are
//! `FakePlatform`; Jira is a scripted transport. Fixtures are shaped on Jira
//! Server/DC (REST v2) and Cloud (REST v3) replies.

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
use super::families::jira::client::{
    JiraApi, JiraClient, JiraClientError, JiraClientErrorCode, JiraHttpResponse, JiraOperation,
    JiraTransport,
};
use super::families::jira::config::JiraToolkitConfig;
use super::families::jira::tools::{
    JiraToolsetErrorCode, build_jira_toolset, test_build_with_api, test_build_without_attachments,
};
use super::families::office_host::OfficeFailure;
use super::policy::ToolAdmissionPolicy;

const TOKEN: &str = "jira-private-token";

fn server_settings() -> Map<String, Value> {
    json!({
        "jira_configuration": {
            "base_url": "https://jira.example.test/jira",
            "hosting": "Server",
            "username": "svc-elitea",
            "api_key": "jira-private-api-key"
        },
        "limit": 5
    })
    .as_object()
    .cloned()
    .expect("object")
}

fn cloud_settings() -> Map<String, Value> {
    json!({
        "jira_configuration": {
            "base_url": "https://tenant.atlassian.net/",
            "hosting": "Auto",
            "username": "",
            "api_key": null,
            "token": TOKEN
        },
        "api_version": "Auto"
    })
    .as_object()
    .cloned()
    .expect("object")
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("jira-att").with_function_call_id("call"))
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
    responses: Mutex<VecDeque<Result<JiraHttpResponse, JiraClientErrorCode>>>,
    seen: Mutex<Vec<Seen>>,
}

impl Script {
    fn new(responses: Vec<Result<JiraHttpResponse, JiraClientErrorCode>>) -> Arc<Self> {
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
impl JiraTransport for Script {
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
            Some(Err(code)) => Err(JiraClientError::fixture(code, false)),
            None => Err(JiraClientError::fixture(
                JiraClientErrorCode::DependencyUnavailable,
                true,
            )),
        }
    }
}

fn ok(body: Value) -> Result<JiraHttpResponse, JiraClientErrorCode> {
    Ok(JiraHttpResponse::fixture(StatusCode::OK, Some(body)))
}

fn status(code: StatusCode) -> Result<JiraHttpResponse, JiraClientErrorCode> {
    Ok(JiraHttpResponse::fixture(code, None))
}

fn refused(code: StatusCode, message: &str) -> Result<JiraHttpResponse, JiraClientErrorCode> {
    Ok(JiraHttpResponse::fixture(
        code,
        Some(json!({"errorMessages": [message], "errors": {}})),
    ))
}

fn myself() -> Result<JiraHttpResponse, JiraClientErrorCode> {
    ok(json!({"accountId": "1", "displayName": "Elitea Bot"}))
}

fn bytes(
    code: StatusCode,
    body: &[u8],
    media_type: &str,
) -> Result<JiraHttpResponse, JiraClientErrorCode> {
    Ok(JiraHttpResponse::raw_fixture(
        code,
        body,
        Some(media_type),
        None,
    ))
}

fn redirect(to: &str) -> Result<JiraHttpResponse, JiraClientErrorCode> {
    Ok(JiraHttpResponse::raw_fixture(
        StatusCode::SEE_OTHER,
        b"",
        None,
        Some(to),
    ))
}

/// A real PNG header, padded past the 100 bytes a picture must have.
fn picture() -> Vec<u8> {
    let mut data = PNG.to_vec();
    data.resize(300, 0);
    data
}

fn setup(
    settings: &Map<String, Value>,
    responses: Vec<Result<JiraHttpResponse, JiraClientErrorCode>>,
) -> (JiraClient, Arc<Script>, Arc<FakePlatform>) {
    let script = Script::new(responses);
    let platform = FakePlatform::new();
    let client = JiraClient::with_transport(
        JiraToolkitConfig::parse(settings).expect("config"),
        script.clone(),
    )
    .with_attachments(Some(platform.host()));
    (client, script, platform)
}

async fn run(client: &JiraClient, operation: JiraOperation<'_>) -> String {
    client
        .execute(operation)
        .await
        .expect("operation")
        .as_str()
        .expect("text")
        .to_owned()
}

/// What the model sees of a failed call: a tool error (infrastructure
/// failures, kept generic by the invocation layer) or, for a failure with a
/// sentence of its own, the text result.
async fn fail(client: &JiraClient, operation: JiraOperation<'_>) -> adk_core::AdkError {
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

fn add_description<'a>(
    filepath: &'a str,
    filename: Option<&'a str>,
    prepend: bool,
) -> JiraOperation<'a> {
    JiraOperation::AddFileToIssueDescription {
        issue_key: "PROJ-1",
        filepath,
        filename,
        prepend,
    }
}

fn attachment_info(id: &str, name: &str) -> Value {
    json!([{
        "id": id,
        "filename": name,
        "content": format!("https://jira.example.test/jira/secure/attachment/{id}/{name}")
    }])
}

fn contains(haystack: &[u8], needle: &str) -> bool {
    haystack
        .windows(needle.len())
        .any(|window| window == needle.as_bytes())
}

#[tokio::test]
async fn a_pdf_is_uploaded_and_linked_in_a_v2_description() {
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"attachment": []}})),
            ok(attachment_info("501", "report.pdf")),
            ok(json!({"fields": {"description": "Old text"}})),
            status(StatusCode::NO_CONTENT),
        ],
    );
    platform.put("docs", "report.pdf", b"%PDF-1.7 body");
    let text = run(&client, add_description("/docs/report.pdf", None, false)).await;
    assert_eq!(
        text,
        "File 'report.pdf' uploaded and added to PROJ-1 description as file. View at: https://jira.example.test/jira/browse/PROJ-1."
    );
    let seen = script.seen();
    assert_eq!(seen.len(), 5);
    assert_eq!(
        seen[1].url,
        "https://jira.example.test/jira/rest/api/2/issue/PROJ-1?fields=attachment"
    );
    assert_eq!(seen[2].method, Method::POST);
    assert_eq!(
        seen[2].url,
        "https://jira.example.test/jira/rest/api/2/issue/PROJ-1/attachments"
    );
    assert!(seen[2].effect, "the upload is an effect");
    assert_eq!(seen[2].atlassian_token.as_deref(), Some("no-check"));
    assert!(
        seen[2]
            .content_type
            .as_deref()
            .is_some_and(|value| value.starts_with("multipart/form-data; boundary="))
    );
    assert!(contains(&seen[2].body, "filename=\"report.pdf\""));
    assert!(contains(&seen[2].body, "Content-Type: application/pdf"));
    assert!(contains(&seen[2].body, "%PDF-1.7 body"));
    assert!(
        seen[2]
            .authorization
            .as_deref()
            .is_some_and(|value| value.starts_with("Basic "))
    );
    assert_eq!(seen[4].method, Method::PUT);
    assert_eq!(
        seen[4].url,
        "https://jira.example.test/jira/rest/api/2/issue/PROJ-1?notifyUsers=true"
    );
    assert!(seen[4].effect);
    assert_eq!(
        seen[4].json(),
        json!({"fields": {"description": "Old text\n\n[^report.pdf]"}})
    );
}

#[tokio::test]
async fn a_picture_is_inline_and_prepended_with_a_chosen_name() {
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"attachment": []}})),
            ok(attachment_info("7", "shot.png")),
            ok(json!({"fields": {"description": null}})),
            status(StatusCode::NO_CONTENT),
        ],
    );
    platform.put("imgs", "generated-1.png", &picture());
    let text = run(
        &client,
        add_description("/imgs/generated-1.png", Some("shot.png"), true),
    )
    .await;
    assert!(
        text.starts_with("File 'shot.png' uploaded and added to PROJ-1 description as image."),
        "{text}"
    );
    let seen = script.seen();
    assert!(contains(&seen[2].body, "filename=\"shot.png\""));
    assert!(contains(&seen[2].body, "Content-Type: image/png"));
    assert_eq!(
        seen[4].json(),
        json!({"fields": {"description": "!shot.png!\n\n"}})
    );
}

#[tokio::test]
async fn a_v3_description_gets_an_adf_link_paragraph() {
    let (client, script, platform) = setup(
        &cloud_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"attachment": []}})),
            ok(attachment_info("9", "notes.txt")),
            ok(
                json!({"fields": {"description": {"type": "doc", "version": 1, "content": [
                    {"type": "paragraph", "content": [{"type": "text", "text": "Existing"}]}
                ]}}}),
            ),
            status(StatusCode::NO_CONTENT),
        ],
    );
    platform.put("docs", "notes.txt", b"hello");
    run(&client, add_description("/docs/notes.txt", None, true)).await;
    let seen = script.seen();
    assert_eq!(
        seen[1].url,
        "https://tenant.atlassian.net/rest/api/3/issue/PROJ-1?fields=attachment"
    );
    let description = &seen[4].json()["fields"]["description"];
    assert_eq!(description["content"].as_array().map(Vec::len), Some(2));
    assert_eq!(
        description["content"][0]["content"][0]["text"],
        "See attached file: "
    );
    assert_eq!(description["content"][0]["content"][1]["text"], "notes.txt");
    assert_eq!(
        description["content"][0]["content"][1]["marks"][0]["attrs"]["href"],
        "https://jira.example.test/jira/secure/attachment/9/notes.txt"
    );
    assert_eq!(description["content"][1]["content"][0]["text"], "Existing");
}

#[tokio::test]
async fn a_same_named_attachment_is_kept_and_reported() {
    let (client, _, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"attachment": [{"id": "1", "filename": "report.pdf"}]}})),
            ok(attachment_info("2", "report.pdf")),
            ok(json!({"fields": {"description": ""}})),
            status(StatusCode::NO_CONTENT),
        ],
    );
    platform.put("docs", "report.pdf", b"%PDF-1.7");
    let text = run(&client, add_description("/docs/report.pdf", None, false)).await;
    assert!(
        text.ends_with(" Note: the issue already had an attachment named 'report.pdf'; the new file was added beside it as attachment 2."),
        "{text}"
    );
}

#[tokio::test]
async fn nothing_is_sent_when_the_artifact_cannot_be_read() {
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
        let (client, script, platform) = setup(&server_settings(), vec![myself()]);
        platform.put("docs", "report.pdf", b"%PDF");
        platform.fail_reads(failure);
        let error = fail(&client, add_description("/docs/report.pdf", None, false)).await;
        assert!(error.message.contains(expected), "{}", error.message);
        // Only the credential probe went out.
        assert_eq!(script.seen().len(), 1);
    }
    let (client, script, _) = setup(&server_settings(), vec![myself()]);
    let error = fail(&client, add_description("/docs/missing.pdf", None, false)).await;
    assert!(
        error.message.contains("The artifact was not found."),
        "{}",
        error.message
    );
    let error = fail(&client, add_description("not-a-path", None, false)).await;
    assert!(
        error.message.contains("artifact path is not valid"),
        "{}",
        error.message
    );
    assert_eq!(script.seen().len(), 1);
}

#[tokio::test]
async fn an_empty_oversized_or_badly_named_file_is_refused_before_the_upload() {
    let (client, script, platform) = setup(&server_settings(), vec![myself()]);
    platform.put("docs", "empty.txt", b"");
    let error = fail(&client, add_description("/docs/empty.txt", None, false)).await;
    assert!(error.message.contains("empty"), "{}", error.message);
    platform.put("docs", "big.bin", &vec![0_u8; 50 * 1024 * 1024 + 1]);
    let error = fail(&client, add_description("/docs/big.bin", None, false)).await;
    assert!(error.message.contains("50 MiB"), "{}", error.message);
    platform.put("docs", "ok.txt", b"x");
    for name in ["a/b.png", "..", "bad\nname", &"n".repeat(256)] {
        let error = fail(&client, add_description("/docs/ok.txt", Some(name), false)).await;
        assert!(
            error.message.contains("file name is not valid"),
            "{}",
            error.message
        );
    }
    assert_eq!(script.seen().len(), 1, "only the credential probe");
}

#[tokio::test]
async fn an_upload_is_never_retried_and_each_failure_has_its_sentence() {
    let cases: Vec<(Result<JiraHttpResponse, JiraClientErrorCode>, &str)> = vec![
        (
            status(StatusCode::UNAUTHORIZED),
            "Jira authentication failed",
        ),
        (
            status(StatusCode::TOO_MANY_REQUESTS),
            "Jira rate limited the request",
        ),
        (
            status(StatusCode::BAD_GATEWAY),
            "Jira may have applied the requested effect; reconcile it before retrying",
        ),
        (
            Err(JiraClientErrorCode::UnknownOutcome),
            "Jira may have applied the requested effect",
        ),
        (
            refused(StatusCode::PAYLOAD_TOO_LARGE, "File is too large"),
            "Failed to upload attachment to PROJ-1: HTTP 413 Payload Too Large: File is too large. Nothing was uploaded.",
        ),
        (
            refused(StatusCode::FORBIDDEN, "No permission"),
            "Nothing was uploaded.",
        ),
    ];
    for (reply, expected) in cases {
        let (client, script, platform) = setup(
            &server_settings(),
            vec![myself(), ok(json!({"fields": {"attachment": []}})), reply],
        );
        platform.put("docs", "report.pdf", b"%PDF");
        let error = fail(&client, add_description("/docs/report.pdf", None, false)).await;
        assert!(
            error.message.contains(expected),
            "{} / {expected}",
            error.message
        );
        assert_eq!(script.seen().len(), 3, "the upload is sent once");
    }
}

#[tokio::test]
async fn a_description_that_cannot_be_updated_says_the_file_was_uploaded() {
    let (client, _, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"attachment": []}})),
            ok(attachment_info("501", "report.pdf")),
            ok(json!({"fields": {"description": "x"}})),
            refused(StatusCode::BAD_REQUEST, "Field 'description' cannot be set"),
        ],
    );
    platform.put("docs", "report.pdf", b"%PDF");
    let error = fail(&client, add_description("/docs/report.pdf", None, false)).await;
    assert!(
        error.message.starts_with(
            "The file 'report.pdf' was uploaded (attachment 501), but the description was NOT updated: HTTP 400 Bad Request: Field 'description' cannot be set."
        ),
        "{}",
        error.message
    );
    assert!(error.message.contains("Do not upload the file again"));

    let (client, _, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"attachment": []}})),
            ok(attachment_info("501", "report.pdf")),
            ok(json!({"fields": {"description": "x"}})),
            status(StatusCode::INTERNAL_SERVER_ERROR),
        ],
    );
    platform.put("docs", "report.pdf", b"%PDF");
    let error = fail(&client, add_description("/docs/report.pdf", None, false)).await;
    assert!(
        error
            .message
            .contains("whether the description was updated is unknown"),
        "{}",
        error.message
    );
}

#[tokio::test]
async fn an_upload_with_no_reported_attachment_is_an_unknown_outcome() {
    let (client, _, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"attachment": []}})),
            ok(json!([])),
        ],
    );
    platform.put("docs", "report.pdf", b"%PDF");
    let error = fail(&client, add_description("/docs/report.pdf", None, false)).await;
    assert!(
        error.message.contains("may or may not have been attached"),
        "{}",
        error.message
    );
}

fn update_comment(filepath: &str) -> JiraOperation<'_> {
    JiraOperation::UpdateCommentWithFile {
        issue_key: "PROJ-1",
        comment_id: "10001",
        filepath,
        filename: None,
        prepend: false,
    }
}

#[tokio::test]
async fn a_comment_is_read_then_the_file_uploaded_then_the_comment_edited() {
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(json!({"id": "10001", "body": "See below"})),
            ok(json!({"fields": {"attachment": []}})),
            ok(attachment_info("12", "shot.png")),
            status(StatusCode::OK),
        ],
    );
    platform.put("imgs", "shot.png", &picture());
    let text = run(&client, update_comment("/imgs/shot.png")).await;
    assert_eq!(
        text,
        "File 'shot.png' uploaded and added to PROJ-1 comment 10001 as image. View at: https://jira.example.test/jira/browse/PROJ-1."
    );
    let seen = script.seen();
    assert_eq!(
        seen[1].url,
        "https://jira.example.test/jira/rest/api/2/issue/PROJ-1/comment/10001"
    );
    assert_eq!(seen[4].method, Method::PUT);
    assert_eq!(
        seen[4].url,
        "https://jira.example.test/jira/rest/api/2/issue/PROJ-1/comment/10001?notifyUsers=true"
    );
    assert_eq!(seen[4].json(), json!({"body": "See below\n\n!shot.png!"}));
}

#[tokio::test]
async fn a_missing_comment_leaves_no_orphan_attachment() {
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            refused(StatusCode::NOT_FOUND, "Can not find comment"),
        ],
    );
    platform.put("imgs", "shot.png", &picture());
    let error = fail(&client, update_comment("/imgs/shot.png")).await;
    assert!(
        error.message.contains("Failed to update comment with file: HTTP 404 Not Found: Can not find comment. Nothing was uploaded."),
        "{}",
        error.message
    );
    assert_eq!(script.seen().len(), 2);
    assert_eq!(platform.read_count(), 0, "the artifact was not even read");
}

#[tokio::test]
async fn a_comment_that_cannot_be_edited_says_the_file_was_uploaded() {
    let (client, _, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(json!({"id": "10001", "body": "x"})),
            ok(json!({"fields": {"attachment": []}})),
            ok(attachment_info("12", "shot.png")),
            refused(
                StatusCode::FORBIDDEN,
                "You do not have permission to edit this comment",
            ),
        ],
    );
    platform.put("imgs", "shot.png", &picture());
    let error = fail(&client, update_comment("/imgs/shot.png")).await;
    assert!(
        error.message.starts_with(
            "The file 'shot.png' was uploaded (attachment 12), but comment 10001 was NOT updated:"
        ),
        "{}",
        error.message
    );
}

fn attachments_issue(list: Value) -> Result<JiraHttpResponse, JiraClientErrorCode> {
    ok(json!({"fields": {"attachment": list}}))
}

#[tokio::test]
async fn attachments_content_reads_text_csv_and_pictures_and_skips_the_rest() {
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            attachments_issue(json!([
                {"id": "1", "filename": "notes.txt", "size": 12},
                {"id": "2", "filename": "table.csv", "size": 20},
                {"id": "3", "filename": "shot.png", "size": 300},
                {"id": "4", "filename": "doc.pdf", "size": 900},
                {"id": "5", "filename": "blob.bin", "size": 3}
            ])),
            bytes(StatusCode::OK, b"line one\r\nline two", "text/plain"),
            bytes(StatusCode::OK, b"a,b\r\n1,2\r\n\r\n", "text/csv"),
            bytes(StatusCode::OK, &picture(), "image/png"),
            bytes(
                StatusCode::OK,
                &[0xff, 0xfe, 0x00],
                "application/octet-stream",
            ),
        ],
    );
    platform.reply("A login form.");
    let text = run(
        &client,
        JiraOperation::GetAttachmentsContent {
            issue_key: "PROJ-1",
            attachment_pattern: None,
        },
    )
    .await;
    assert_eq!(
        text,
        "filename: notes.txt\ncontent: ['line one\\nline two']\n\nfilename: table.csv\ncontent: ['a,b\\n1,2']\n\nfilename: shot.png\ncontent: ['A login form.']"
    );
    let seen = script.seen();
    assert_eq!(
        seen[2].url,
        "https://jira.example.test/jira/secure/attachment/1/notes.txt"
    );
    assert!(seen.iter().all(|request| !request.effect));
    let calls = platform.model_calls();
    assert_eq!(calls.len(), 1);
    assert!(
        calls[0]
            .prompt
            .contains("You are an AI model designed for analyzing images.")
    );
    assert_eq!(
        calls[0].image_media_types.as_ref(),
        ["image/png".to_owned()]
    );
}

#[tokio::test]
async fn attachments_content_filters_by_a_regular_expression() {
    let (client, script, _) = setup(
        &server_settings(),
        vec![
            myself(),
            attachments_issue(json!([
                {"id": "1", "filename": "a.txt"},
                {"id": "2", "filename": "b.yaml"}
            ])),
            bytes(StatusCode::OK, b"key: value", "text/plain"),
        ],
    );
    let text = run(
        &client,
        JiraOperation::GetAttachmentsContent {
            issue_key: "PROJ-1",
            attachment_pattern: Some(r"\.ya?ml$"),
        },
    )
    .await;
    assert_eq!(text, "filename: b.yaml\ncontent: ['key: value']");
    assert_eq!(script.seen().len(), 3);

    let (client, _, _) = setup(
        &server_settings(),
        vec![myself(), attachments_issue(json!([]))],
    );
    let error = fail(
        &client,
        JiraOperation::GetAttachmentsContent {
            issue_key: "PROJ-1",
            attachment_pattern: Some("("),
        },
    )
    .await;
    assert_eq!(
        error.message,
        "attachment_pattern is not a valid regular expression."
    );
}

#[tokio::test]
async fn cloud_attachments_follow_the_media_redirect_without_the_credential() {
    let (client, script, _) = setup(
        &cloud_settings(),
        vec![
            myself(),
            attachments_issue(json!([{"id": "10", "filename": "notes.txt", "size": 8}])),
            ok(
                json!({"id": "10", "content": "https://tenant.atlassian.net/rest/api/3/attachment/content/10"}),
            ),
            redirect("https://api.media.atlassian.com/file/abc/binary?token=secret"),
            bytes(StatusCode::OK, &[b'x'; 150], "text/plain"),
        ],
    );
    let text = run(
        &client,
        JiraOperation::GetAttachmentsContent {
            issue_key: "PROJ-1",
            attachment_pattern: None,
        },
    )
    .await;
    assert_eq!(
        text,
        format!("filename: notes.txt\ncontent: ['{}']", "x".repeat(150))
    );
    let seen = script.seen();
    assert_eq!(
        seen[2].url,
        "https://tenant.atlassian.net/rest/api/3/attachment/10"
    );
    assert_eq!(
        seen[3].url,
        "https://tenant.atlassian.net/rest/api/3/attachment/content/10"
    );
    assert!(seen[3].authorization.is_some());
    assert_eq!(
        seen[4].url,
        "https://api.media.atlassian.com/file/abc/binary?token=secret"
    );
    assert!(
        seen[4].authorization.is_none(),
        "no credential to the media host"
    );
}

#[tokio::test]
async fn a_redirect_to_any_other_host_or_back_to_the_origin_is_refused() {
    for target in [
        "https://evil.example.net/steal",
        "http://api.media.atlassian.com/plain",
        "https://tenant.atlassian.net/login?next=1",
    ] {
        let (client, script, _) = setup(
            &cloud_settings(),
            vec![
                myself(),
                attachments_issue(json!([{"id": "10", "filename": "notes.txt"}])),
                ok(
                    json!({"id": "10", "content": "https://tenant.atlassian.net/rest/api/3/attachment/content/10"}),
                ),
                redirect(target),
                // The fallback route answers with another redirect.
                redirect(target),
            ],
        );
        let text = run(
            &client,
            JiraOperation::GetAttachmentsContent {
                issue_key: "PROJ-1",
                attachment_pattern: None,
            },
        )
        .await;
        // The login-page redirect stays on the origin and is followed once;
        // the next answer is not a file either, so nothing is read.
        assert_eq!(text, "", "{target}");
        assert!(
            script
                .seen()
                .iter()
                .all(|request| !request.url.contains("evil.example.net")),
            "{target}"
        );
    }
}

#[tokio::test]
async fn attachment_pictures_stop_calling_the_model_after_a_budget_refusal() {
    let (client, _, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            attachments_issue(json!([
                {"id": "1", "filename": "a.png", "size": 300},
                {"id": "2", "filename": "b.png", "size": 300}
            ])),
            bytes(StatusCode::OK, &picture(), "image/png"),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    platform.fail_model(OfficeFailure::BudgetRefused);
    let text = run(
        &client,
        JiraOperation::GetAttachmentsContent {
            issue_key: "PROJ-1",
            attachment_pattern: None,
        },
    )
    .await;
    assert!(
        text.contains("filename: a.png\ncontent: ['[Image: a.png - not described: The model budget is exhausted. Do not retry; tell the user.]']"),
        "{text}"
    );
    assert!(
        text.contains("filename: b.png\ncontent: ['[Image: b.png - not described: The model budget is exhausted."),
        "{text}"
    );
    assert_eq!(platform.model_calls().len(), 1, "the loop stopped");
}

fn field_op<'a>(field: &'a str, prompt: Option<&'a str>, process: bool) -> JiraOperation<'a> {
    JiraOperation::GetFieldWithImageDescriptions {
        issue_key: "PROJ-1",
        field_name: field,
        prompt,
        context_radius: 500,
        process_images: process,
    }
}

fn adf_description() -> Value {
    json!({"type": "doc", "version": 1, "content": [
        {"type": "paragraph", "content": [{"type": "text", "text": "Login screen:"}]},
        {"type": "mediaSingle", "content": [{"type": "media", "attrs": {"type": "file", "id": "abc", "alt": "shot.png"}}]},
        {"type": "paragraph", "content": [{"type": "text", "text": "Expected: success"}]}
    ]})
}

#[tokio::test]
async fn a_field_picture_is_described_with_its_context() {
    let (client, script, platform) = setup(
        &cloud_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"description": adf_description()}})),
            ok(json!({"fields": {"attachment": [
                {"id": "77", "filename": "shot.png", "size": 300, "content": "https://tenant.atlassian.net/rest/api/3/attachment/content/77"}
            ]}})),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    platform.reply("A login form with two fields.");
    let text = run(&client, field_op("description", None, true)).await;
    assert_eq!(
        text,
        "Field 'description' from issue 'PROJ-1' with image descriptions:\n\nLogin screen:\n\n[Image shot.png Description: A login form with two fields.]Expected: success"
    );
    let calls = platform.model_calls();
    assert_eq!(calls.len(), 1);
    let prompt = &calls[0].prompt;
    assert!(prompt.starts_with(
        "\n        ## Image Analysis Task:\n        Analyze this image and describe it in detail"
    ));
    assert!(prompt.contains("- Image Name/Reference: shot.png\n"));
    assert!(prompt.contains(
        "- Surrounding Content: Login screen:\n\n!shot.png|alt=\"shot.png\"!Expected: success\n"
    ));
    assert!(prompt.ends_with(
        "\nPlease incorporate this contextual information in your description when relevant."
    ));
    assert_eq!(
        script.seen()[1].url,
        "https://tenant.atlassian.net/rest/api/3/issue/PROJ-1?fields=description"
    );
}

#[tokio::test]
async fn a_custom_prompt_replaces_the_default_one() {
    let (client, _, platform) = setup(
        &cloud_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"description": adf_description()}})),
            ok(json!({"fields": {"attachment": [
                {"id": "77", "filename": "shot.png", "content": "https://tenant.atlassian.net/rest/api/3/attachment/content/77"}
            ]}})),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    platform.reply("ok");
    run(
        &client,
        field_op("description", Some("Describe briefly."), true),
    )
    .await;
    assert!(
        platform.model_calls()[0]
            .prompt
            .starts_with("Describe briefly.\n\n## Additional Context Information:")
    );
}

#[tokio::test]
async fn raw_field_text_needs_no_model_and_no_picture_fetch() {
    let (client, script, platform) = setup(
        &cloud_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"description": adf_description()}})),
        ],
    );
    let text = run(&client, field_op("description", None, false)).await;
    assert_eq!(
        text,
        "Field 'description' from issue 'PROJ-1':\n\nLogin screen:\n\n!shot.png|alt=\"shot.png\"!Expected: success"
    );
    assert!(platform.model_calls().is_empty());
    assert_eq!(script.seen().len(), 2);
}

#[tokio::test]
async fn a_missing_or_empty_field_lists_the_fields_the_issue_has() {
    let (client, _, _) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"description": null}})),
            ok(json!({"fields": {"summary": "S", "description": null}})),
        ],
    );
    let text = run(&client, field_op("description", None, true)).await;
    assert_eq!(
        text,
        "Unable to find field 'description' or it's empty. Available fields are: summary"
    );
}

#[tokio::test]
async fn picture_references_that_cannot_be_used_become_placeholders() {
    let description = "!missing.png! !nourl.png! !big.png! !html.png! !gone.png! !tiny.png!";
    let (client, _, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"description": description}})),
            ok(json!({"fields": {"attachment": [
                {"id": "1", "filename": "nourl.png"},
                {"id": "2", "filename": "big.png", "size": 6_291_456, "content": "https://jira.example.test/jira/secure/attachment/2/big.png"},
                {"id": "3", "filename": "html.png", "size": 500, "content": "https://jira.example.test/jira/secure/attachment/3/html.png"},
                {"id": "4", "filename": "gone.png", "size": 500, "content": "https://jira.example.test/jira/secure/attachment/4/gone.png"},
                {"id": "5", "filename": "tiny.png", "size": 20, "content": "https://jira.example.test/jira/secure/attachment/5/tiny.png"}
            ]}})),
            bytes(StatusCode::OK, b"<html>login</html>", "text/html"),
            status(StatusCode::NOT_FOUND),
            bytes(StatusCode::OK, b"short", "image/png"),
        ],
    );
    let text = run(&client, field_op("description", None, true)).await;
    assert_eq!(
        text,
        "Field 'description' from issue 'PROJ-1' with image descriptions:\n\n[Image: missing.png - attachment not found] [Image: nourl.png - no content URL] [Image: big.png - not described: larger than 5 MiB] [Image: html.png - download failed] [Image: gone.png - download failed] [Image: tiny.png - download failed]"
    );
    assert!(platform.model_calls().is_empty());
}

#[tokio::test]
async fn at_most_twenty_pictures_are_described_per_call() {
    let description = (0..22)
        .map(|n| format!("!p{n}.png!"))
        .collect::<Vec<_>>()
        .join(" ");
    let attachments = (0..22)
        .map(|n| json!({"id": format!("{}", 100 + n), "filename": format!("p{n}.png"), "size": 300,
            "content": format!("https://jira.example.test/jira/secure/attachment/{}/p{n}.png", 100 + n)}))
        .collect::<Vec<_>>();
    let mut script = vec![
        myself(),
        ok(json!({"fields": {"description": description}})),
        ok(json!({"fields": {"attachment": attachments}})),
    ];
    for _ in 0..20 {
        script.push(bytes(StatusCode::OK, &picture(), "image/png"));
    }
    let (client, _, platform) = setup(&server_settings(), script);
    for _ in 0..20 {
        platform.reply("d");
    }
    let text = run(&client, field_op("description", None, true)).await;
    assert_eq!(platform.model_calls().len(), 20);
    assert!(
        text.contains(
            "[Image: p20.png - not described: at most 20 pictures are described per call]"
        ),
        "{text}"
    );
    assert!(
        text.contains(
            "[Image: p21.png - not described: at most 20 pictures are described per call]"
        )
    );
}

#[tokio::test]
async fn the_same_picture_is_described_once() {
    let (client, _, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"description": "!a.png! and !a.png|thumbnail!"}})),
            ok(json!({"fields": {"attachment": [
                {"id": "1", "filename": "a.png", "size": 300, "content": "https://jira.example.test/jira/secure/attachment/1/a.png"}
            ]}})),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    platform.reply("one");
    let text = run(&client, field_op("description", None, true)).await;
    assert!(
        text.ends_with("[Image a.png Description: one] and [Image a.png Description: one]"),
        "{text}"
    );
    assert_eq!(platform.model_calls().len(), 1);
}

#[tokio::test]
async fn a_run_without_a_model_says_so_in_the_result() {
    let (client, _, platform) = setup(
        &cloud_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"description": adf_description()}})),
            ok(json!({"fields": {"attachment": [
                {"id": "77", "filename": "shot.png", "content": "https://tenant.atlassian.net/rest/api/3/attachment/content/77"}
            ]}})),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    platform.fail_model(OfficeFailure::NoModel);
    let text = run(&client, field_op("description", None, true)).await;
    assert!(
        text.starts_with("Error processing field with images: This tool needs a chat model and none is bound in this run"),
        "{text}"
    );
}

#[tokio::test]
async fn a_picture_the_model_cannot_take_is_not_sent() {
    let (client, _, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"description": "!logo.svg!"}})),
            ok(json!({"fields": {"attachment": [
                {"id": "1", "filename": "logo.svg", "size": 300, "content": "https://jira.example.test/jira/secure/attachment/1/logo.svg"}
            ]}})),
            bytes(StatusCode::OK, &[b'<'; 300], "image/svg+xml"),
        ],
    );
    let text = run(&client, field_op("description", None, true)).await;
    assert!(
        text.ends_with(
            "[Image logo.svg Description: [Could not identify image format for logo.svg]]"
        ),
        "{text}"
    );
    assert!(platform.model_calls().is_empty());
}

#[tokio::test]
async fn a_rate_limited_model_stops_the_loop_and_a_timeout_does_not() {
    let (client, _, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(json!({"fields": {"description": "!a.png! !b.png! !c.png!"}})),
            ok(json!({"fields": {"attachment": [
                {"id": "1", "filename": "a.png", "size": 300, "content": "https://jira.example.test/jira/secure/attachment/1/a.png"},
                {"id": "2", "filename": "b.png", "size": 300, "content": "https://jira.example.test/jira/secure/attachment/2/b.png"},
                {"id": "3", "filename": "c.png", "size": 300, "content": "https://jira.example.test/jira/secure/attachment/3/c.png"}
            ]}})),
            bytes(StatusCode::OK, &picture(), "image/png"),
            bytes(StatusCode::OK, &picture(), "image/png"),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    platform.fail_model(OfficeFailure::Timeout);
    platform.fail_model(OfficeFailure::RateLimited);
    let text = run(&client, field_op("description", None, true)).await;
    assert!(
        text.contains("[Image a.png Description: [Image processing error: The model call timed out. It may work if tried again.]]"),
        "{text}"
    );
    assert!(
        text.contains(
            "[Image: b.png - not described: The model gateway is rate-limiting this call."
        ),
        "{text}"
    );
    assert!(
        text.contains(
            "[Image: c.png - not described: The model gateway is rate-limiting this call."
        ),
        "{text}"
    );
    assert_eq!(platform.model_calls().len(), 2);
}

#[tokio::test]
async fn a_field_read_that_jira_refuses_is_reported_as_text() {
    let (client, _, _) = setup(
        &server_settings(),
        vec![
            myself(),
            refused(StatusCode::NOT_FOUND, "Issue Does Not Exist"),
        ],
    );
    let text = run(&client, field_op("description", None, true)).await;
    assert_eq!(
        text,
        "Error processing field with images: HTTP 404 Not Found: Issue Does Not Exist"
    );
}

fn comments_op<'a>(process: bool) -> JiraOperation<'a> {
    JiraOperation::GetCommentsWithImageDescriptions {
        issue_key: "PROJ-1",
        prompt: None,
        context_radius: 500,
        process_images: process,
    }
}

fn comments_body() -> Value {
    json!({"comments": [
        {"id": "1", "author": {"displayName": "Ann"}, "created": "2026-10-01T10:00:00.000+0000", "body": "Look: !shot.png|thumbnail! done"},
        {"id": "2", "author": {"displayName": "Bob"}, "created": "2026-10-02T10:00:00.000+0000", "body": ""},
        {"id": "3", "created": "2026-10-03T10:00:00.000+0000", "body": "Plain"}
    ]})
}

#[tokio::test]
async fn comments_get_their_pictures_described_and_empty_ones_skipped() {
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(comments_body()),
            ok(json!({"fields": {"attachment": [
                {"id": "1", "filename": "shot.png", "size": 300, "content": "https://jira.example.test/jira/secure/attachment/1/shot.png"}
            ]}})),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    platform.reply("A screenshot.");
    let text = run(&client, comments_op(true)).await;
    assert_eq!(
        text,
        "Comments from issue 'PROJ-1' with image descriptions:\n\nComment #1 by Ann on 2026-10-01T10:00:00.000+0000:\nLook: [Image shot.png Description: A screenshot.] done\n\nComment #2 by Unknown on 2026-10-03T10:00:00.000+0000:\nPlain\n\n"
    );
    assert_eq!(
        script.seen()[1].url,
        "https://jira.example.test/jira/rest/api/2/issue/PROJ-1/comment"
    );
}

#[tokio::test]
async fn raw_comments_need_no_model_and_no_attachment_lookup() {
    let (client, script, platform) = setup(&server_settings(), vec![myself(), ok(comments_body())]);
    let text = run(&client, comments_op(false)).await;
    assert!(text.contains("Look: !shot.png|thumbnail! done"));
    assert!(platform.model_calls().is_empty());
    assert_eq!(script.seen().len(), 2);

    let (client, _, _) = setup(
        &server_settings(),
        vec![myself(), ok(json!({"comments": []}))],
    );
    assert_eq!(
        run(&client, comments_op(true)).await,
        "No comments found for issue 'PROJ-1'"
    );
}

#[tokio::test]
async fn an_absolute_picture_link_is_fetched_only_from_the_jira_origin() {
    let body = json!({"comments": [
        {"id": "1", "author": {"displayName": "Ann"}, "created": "c", "body": "!https://jira.example.test/jira/secure/attachment/9/own.png! !https://evil.example.net/x.png!"}
    ]});
    let (client, script, platform) = setup(
        &server_settings(),
        vec![
            myself(),
            ok(body),
            ok(json!({"fields": {"attachment": []}})),
            bytes(StatusCode::OK, &picture(), "image/png"),
        ],
    );
    platform.reply("own picture");
    let text = run(&client, comments_op(true)).await;
    assert!(
        text.contains("[Image own.png Description: own picture]"),
        "{text}"
    );
    assert!(
        text.contains("[Image: https://evil.example.net/x.png - attachment not found]"),
        "{text}"
    );
    assert!(
        script
            .seen()
            .iter()
            .all(|request| !request.url.contains("evil.example.net"))
    );
}

#[tokio::test]
async fn jira_failures_keep_their_own_error_kinds() {
    let (client, _, _) = setup(
        &server_settings(),
        vec![myself(), status(StatusCode::UNAUTHORIZED)],
    );
    let error = fail(&client, comments_op(false)).await;
    assert_eq!(error.category, ErrorCategory::Unauthorized);
    let (client, _, _) = setup(
        &server_settings(),
        vec![myself(), status(StatusCode::TOO_MANY_REQUESTS)],
    );
    let error = fail(&client, comments_op(false)).await;
    assert_eq!(error.category, ErrorCategory::RateLimited);
    let (client, _, _) = setup(
        &server_settings(),
        vec![myself(), status(StatusCode::BAD_GATEWAY)],
    );
    let error = fail(&client, comments_op(false)).await;
    assert_eq!(error.category, ErrorCategory::Unavailable);
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new()).expect("policy"))
}

#[tokio::test]
async fn the_tools_have_the_sdk_names_flags_and_closed_schemas() {
    let (client, script, platform) = setup(&server_settings(), Vec::new());
    let api: Arc<dyn JiraApi> = Arc::new(client);
    let names = [
        "get_field_with_image_descriptions",
        "get_comments_with_image_descriptions",
        "get_attachments_content",
        "add_file_to_issue_description",
        "update_comment_with_file",
    ];
    let toolset = test_build_with_api("tracker", &names, &policy(), &api).expect("toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("tools");
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        names
    );
    for tool in &tools {
        let read = !matches!(
            tool.name(),
            "add_file_to_issue_description" | "update_comment_with_file"
        );
        assert_eq!(tool.is_read_only(), read, "{}", tool.name());
        assert_eq!(tool.is_concurrency_safe(), read, "{}", tool.name());
        let schema = tool.parameters_schema().expect("schema");
        assert_eq!(schema["additionalProperties"], json!(false));
        if !read {
            assert!(tool.description().contains("reconciled"), "{}", tool.name());
        }
    }
    let find = |name: &str| {
        tools
            .iter()
            .find(|tool| tool.name() == name)
            .expect("tool")
            .clone()
    };
    // Arguments are checked before anything is sent.
    for (tool, arguments) in [
        ("add_file_to_issue_description", json!({"issue_key": "P-1"})),
        (
            "add_file_to_issue_description",
            json!({"issue_key": "P-1", "filepath": "/b/f", "position": "middle"}),
        ),
        (
            "add_file_to_issue_description",
            json!({"issue_key": "P-1", "filepath": "/b/f", "extra": 1}),
        ),
        (
            "update_comment_with_file",
            json!({"issue_key": "P-1", "filepath": "/b/f"}),
        ),
        (
            "get_field_with_image_descriptions",
            json!({"jira_issue_key": "P-1"}),
        ),
        (
            "get_field_with_image_descriptions",
            json!({"jira_issue_key": "P-1", "field_name": "d", "context_radius": "wide"}),
        ),
        (
            "get_field_with_image_descriptions",
            json!({"jira_issue_key": "P-1", "field_name": "d", "process_images": "yes"}),
        ),
        (
            "get_comments_with_image_descriptions",
            json!({"jira_issue_key": "P-1", "context_radius": 1.5}),
        ),
        (
            "get_attachments_content",
            json!({"attachment_pattern": "x"}),
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
async fn integer_arguments_are_coerced_like_pydantic() {
    let (client, script, _) = setup(&server_settings(), vec![myself(), ok(comments_body())]);
    let api: Arc<dyn JiraApi> = Arc::new(client);
    let toolset = test_build_with_api(
        "tracker",
        &["get_comments_with_image_descriptions"],
        &policy(),
        &api,
    )
    .expect("toolset");
    let tools = toolset.tools(context()).await.expect("tools");
    let result = tools[0]
        .execute(
            context(),
            json!({"jira_issue_key": "PROJ-1", "context_radius": "250", "process_images": false, "prompt": null}),
        )
        .await
        .expect("coerced");
    assert!(
        result
            .as_str()
            .is_some_and(|text| text.starts_with("Comments from issue 'PROJ-1'"))
    );
    assert_eq!(script.seen().len(), 2);
}

#[tokio::test]
async fn without_attachment_authority_the_tools_are_omitted() {
    let script = Script::new(Vec::new());
    let client: Arc<dyn JiraApi> = Arc::new(JiraClient::with_transport(
        JiraToolkitConfig::parse(&server_settings()).expect("config"),
        script,
    ));
    let toolset =
        test_build_without_attachments("tracker", &[], &policy(), &client).expect("toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let names = toolset
        .tools(readonly)
        .await
        .expect("tools")
        .iter()
        .map(|tool| tool.name().to_owned())
        .collect::<Vec<_>>();
    assert_eq!(names.len(), 12);
    assert!(!names.iter().any(|name| name.contains("image_descriptions")
        || name == "get_attachments_content"
        || name.ends_with("_with_file")
        || name == "add_file_to_issue_description"));
    // A selection naming only omitted tools has nothing to serve.
    let Err(error) = test_build_without_attachments(
        "tracker",
        &["add_file_to_issue_description", "update_comment_with_file"],
        &policy(),
        &client,
    ) else {
        panic!("only omitted tools selected");
    };
    assert_eq!(error.code(), JiraToolsetErrorCode::UnsupportedSelection);
    // The production builder omits them the same way.
    let config = JiraToolkitConfig::parse(&{
        let mut settings = server_settings();
        settings.insert(
            "selected_tools".to_owned(),
            json!(["get_attachments_content"]),
        );
        settings
    })
    .expect("config");
    let Err(error) = build_jira_toolset("tracker", config, &policy(), None) else {
        panic!("nothing to serve without a host");
    };
    assert_eq!(error.code(), JiraToolsetErrorCode::UnsupportedSelection);
    let config = JiraToolkitConfig::parse(&{
        let mut settings = server_settings();
        settings.insert(
            "selected_tools".to_owned(),
            json!(["get_attachments_content", "list_projects"]),
        );
        settings
    })
    .expect("config");
    let toolset = build_jira_toolset("tracker", config, &policy(), None).expect("partial");
    let tools = toolset.tools(context()).await.expect("tools");
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        ["list_projects"]
    );
}
