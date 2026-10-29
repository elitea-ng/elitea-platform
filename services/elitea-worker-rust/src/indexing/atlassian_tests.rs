//! ADR-0030 Verification (2) for the I2 Atlassian families: a recorded
//! Confluence and a recorded Jira (a scripted HTTP transport, no network)
//! indexed through the real connector, pipeline and the in-process
//! `elitea-vector` fake. Each family indexes its source, then indexes it
//! again: an unchanged source writes nothing to the store; one changed
//! document (a page, a comment) rewrites only that document's chunks; a
//! document gone from the source has its chunks removed. Attachments and
//! comments are dependents with keys of their own.

use std::sync::atomic::Ordering;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_connectors::transport::{
    HeaderMap, HeaderValue, Request, Response, StatusCode, Transport, TransportError,
};
use serde_json::{Value, json};

use super::tests::{Harness, MODEL, NS, binding, command, harness, stored};
use super::{IndexRun, run_index};
use crate::execution::ClaimLeaseError;
use crate::protocol::elitea::runtime::v1::{IndexIngestStatusV1, IndexIngestSummaryV1};

struct Always;

impl super::pipeline::RunLiveness for Always {
    fn ensure_running(&self) -> Result<(), ClaimLeaseError> {
        Ok(())
    }
}

type Handler = dyn Fn(&Request) -> (StatusCode, Value) + Send + Sync;

/// A recorded provider: `handler` maps a request to a status and a JSON (or,
/// for a download, a string) body.
struct Recorded {
    handler: Box<Handler>,
    requests: Mutex<Vec<String>>,
}

impl Recorded {
    fn new(handler: impl Fn(&Request) -> (StatusCode, Value) + Send + Sync + 'static) -> Arc<Self> {
        Arc::new(Self {
            handler: Box::new(handler),
            requests: Mutex::new(Vec::new()),
        })
    }

    fn seen(&self) -> Vec<String> {
        self.requests.lock().expect("lock").clone()
    }
}

#[async_trait]
impl Transport for Recorded {
    async fn execute(&self, request: Request) -> Result<Response, TransportError> {
        self.requests.lock().expect("lock").push(format!(
            "{} {}",
            request.method(),
            request.url().path()
        ));
        let (status, body) = (self.handler)(&request);
        let mut headers = HeaderMap::new();
        let bytes = match body {
            Value::String(raw) => raw.into_bytes(),
            other => {
                headers.insert("content-type", HeaderValue::from_static("application/json"));
                other.to_string().into_bytes()
            }
        };
        Ok(Response::from_bytes(status, headers, bytes))
    }
}

fn query(request: &Request, name: &str) -> Option<String> {
    request
        .url()
        .query_pairs()
        .find(|(key, _)| key == name)
        .map(|(_, value)| value.into_owned())
}

async fn run(
    harness: &Harness,
    configuration: &Value,
    parameters: &Value,
    expected: Option<(&str, u32)>,
) -> IndexIngestSummaryV1 {
    let now = || 1_700_000_000_000_i64;
    run_index(
        &harness.runtime,
        IndexRun {
            command: &command(NS, expected),
            egress: elitea_agent_runtime::egress::RunEgress::open(),
            toolkit_configuration: configuration.to_string().as_bytes(),
            tool_parameters: parameters.to_string().as_bytes(),
            parsed: None,
            embedding_binding: Some(&binding()),
            llm_model: None,
            llm_configuration: None,
            chat_entries: None,
            vector_token: Some(&harness.token),
            context: &harness.context,
            lease: &Always,
            deadline_unix_millis: i64::MAX,
            now_unix_millis: &now,
        },
    )
    .await
    .expect("a summary")
}

async fn stored_versions(harness: &Harness) -> Vec<(String, String)> {
    let mut rows: Vec<(String, String)> = stored(harness)
        .await
        .into_iter()
        .map(|point| (point.document_key, point.document_version))
        .collect();
    rows.sort();
    rows
}

/// The store calls since `before`.
fn store_calls_since(harness: &Harness, before: usize) -> Vec<&'static str> {
    harness.fake.calls()[before..].to_vec()
}

// ---------------------------------------------------------------- Confluence

/// The recorded Confluence: pages by id, each with a version and a body; one
/// attachment and one comment on page 1 while present.
#[derive(Clone)]
struct Wiki {
    pages: Vec<(u64, &'static str, u64, &'static str)>,
    attachment: bool,
    comment_version: Option<u64>,
}

fn page_json(id: u64, title: &str, version: u64, body: &str) -> Value {
    json!({
        "id": id.to_string(), "type": "page", "status": "current", "title": title,
        "version": {"number": version, "when": format!("2026-10-0{version}T00:00:00.000Z")},
        "ancestors": [],
        // As Confluence answers `expand=restrictions.read.restrictions.user,…group`:
        // the read level is present, empty when the page is open.
        "restrictions": {"read": {"operation": "read", "restrictions": {
            "user": {"results": [], "start": 0, "limit": 100, "size": 0},
            "group": {"results": [], "start": 0, "limit": 100, "size": 0},
        }}},
        "_links": {"webui": format!("/spaces/DOCS/pages/{id}/{title}")},
        "space": {"key": "DOCS"},
        "body": {"view": {"value": body}},
    })
}

fn wiki_transport(state: Arc<Mutex<Wiki>>) -> Arc<Recorded> {
    Recorded::new(move |request| {
        let wiki = state.lock().expect("lock").clone();
        let path = request.url().path();
        if path == "/wiki/rest/api/content" {
            assert_eq!(query(request, "spaceKey").as_deref(), Some("DOCS"));
            let results: Vec<Value> = wiki
                .pages
                .iter()
                .map(|(id, title, version, body)| page_json(*id, title, *version, body))
                .collect();
            return (
                StatusCode::OK,
                json!({"results": results, "size": results.len()}),
            );
        }
        if path == "/wiki/rest/api/content/1/child/attachment" {
            let results = if wiki.attachment {
                vec![json!({
                    "id": "att-1", "title": "notes.txt", "extensions": {"fileSize": 27},
                    "metadata": {"mediaType": "text/plain"}, "version": {"number": 1},
                    "_links": {"download": "/download/attachments/1/notes.txt?version=1&api=v2"},
                })]
            } else {
                Vec::new()
            };
            return (
                StatusCode::OK,
                json!({"results": results, "size": results.len()}),
            );
        }
        if path.ends_with("/child/attachment") {
            return (StatusCode::OK, json!({"results": [], "size": 0}));
        }
        if path == "/wiki/rest/api/content/1/child/comment" {
            let results: Vec<Value> = wiki
                .comment_version
                .map(|version| json!({"id": "500", "type": "comment", "version": {"number": version}}))
                .into_iter()
                .collect();
            return (
                StatusCode::OK,
                json!({"results": results, "size": results.len()}),
            );
        }
        if path.ends_with("/child/comment") {
            return (StatusCode::OK, json!({"results": [], "size": 0}));
        }
        if path == "/wiki/download/attachments/1/notes.txt" {
            return (
                StatusCode::OK,
                Value::String("Attachment notes, plain text.".to_owned()),
            );
        }
        if path == "/wiki/rest/api/content/500" {
            let version = wiki.comment_version.expect("the comment exists");
            return (
                StatusCode::OK,
                json!({
                    "id": "500", "version": {"number": version, "when": "2026-10-05T00:00:00Z"},
                    "container": {"id": "1", "title": "Alpha"},
                    "body": {"view": {"value": format!("<p>Comment text, revision {version}.</p>")}},
                }),
            );
        }
        if let Some(id) = path.strip_prefix("/wiki/rest/api/content/") {
            let id: u64 = id.parse().expect("a page id");
            let (id, title, version, body) = wiki
                .pages
                .iter()
                .find(|(page, _, _, _)| *page == id)
                .copied()
                .expect("a listed page");
            return (StatusCode::OK, page_json(id, title, version, body));
        }
        panic!("unexpected request {path}");
    })
}

fn confluence_configuration() -> Value {
    json!({
        "id": 7, "type": "confluence", "toolkit_name": "wiki",
        "settings": {
            "space": "DOCS",
            "confluence_configuration": {"base_url": "https://acme.atlassian.net/wiki", "token": "fixture"},
        }
    })
}

fn confluence_parameters() -> Value {
    json!({"index_name": "wiki", "include_attachments": true, "include_comments": true})
}

#[tokio::test(flavor = "multi_thread")]
#[allow(clippy::too_many_lines, reason = "five runs over one recorded wiki")]
async fn confluence_indexes_incrementally_with_dependents() {
    let wiki = Arc::new(Mutex::new(Wiki {
        pages: vec![
            (
                1,
                "Alpha",
                1,
                "<h1>Alpha</h1><p>First page, first revision.</p>",
            ),
            (
                2,
                "Bravo",
                1,
                "<h1>Bravo</h1><p>Second page, first revision.</p>",
            ),
        ],
        attachment: true,
        comment_version: Some(1),
    }));
    let mut harness = harness(4, None).await;
    let transport = wiki_transport(Arc::clone(&wiki));
    harness.runtime.transport = transport.clone();
    let configuration = confluence_configuration();
    let parameters = confluence_parameters();

    // First run: two pages, one attachment, one comment.
    let first = run(&harness, &configuration, &parameters, None).await;
    assert_eq!(first.status, IndexIngestStatusV1::Ok as i32, "{first:?}");
    assert_eq!(first.indexed_documents, 4);
    assert_eq!(first.total_documents, Some(4));
    assert!(first.whole_index_record);
    assert_eq!(
        stored_versions(&harness).await,
        vec![
            ("page/1.html".to_owned(), "1".to_owned()),
            (
                "page/1/attachment/att-1/notes.txt".to_owned(),
                "1".to_owned()
            ),
            ("page/1/comment/500.html".to_owned(), "1".to_owned()),
            ("page/2.html".to_owned(), "1".to_owned()),
        ]
    );
    let seen = transport.seen();
    assert!(
        seen.contains(&"GET /wiki/download/attachments/1/notes.txt".to_owned()),
        "{seen:?}"
    );
    assert!(
        seen.iter()
            .all(|request| !request.contains("rest/api/content/search")),
        "no CQL: a space listing"
    );

    // Unchanged source: nothing is written or deleted.
    let calls_before = harness.fake.calls().len();
    let embeddings_before = harness.embeddings.calls.load(Ordering::SeqCst);
    let second = run(&harness, &configuration, &parameters, Some((MODEL, 4))).await;
    assert_eq!(second.status, IndexIngestStatusV1::Ok as i32, "{second:?}");
    assert_eq!(second.indexed_documents, 0);
    assert_eq!(second.total_documents, Some(4));
    assert_eq!(second.removed_document_keys, [] as [String; 0]);
    assert_eq!(second.reindex, Some(true));
    assert_eq!(
        store_calls_since(&harness, calls_before),
        vec!["list_documents"]
    );
    assert_eq!(
        harness.embeddings.calls.load(Ordering::SeqCst),
        embeddings_before
    );

    // One page changes: only its chunks are rewritten.
    wiki.lock().expect("lock").pages[1] = (
        2,
        "Bravo",
        2,
        "<h1>Bravo</h1><p>Second page, second revision, longer than before.</p>",
    );
    let calls_before = harness.fake.calls().len();
    let third = run(&harness, &configuration, &parameters, Some((MODEL, 4))).await;
    assert_eq!(third.status, IndexIngestStatusV1::Ok as i32, "{third:?}");
    assert_eq!(third.indexed_documents, 1);
    assert_eq!(third.updated, 1);
    assert_eq!(third.documents.len(), 1);
    assert_eq!(third.documents[0].document_key, "page/2.html");
    assert_eq!(third.documents[0].version, "2");
    assert_eq!(third.total_documents, Some(4));
    let calls = store_calls_since(&harness, calls_before);
    assert_eq!(
        calls.iter().filter(|call| **call == "delete").count(),
        1,
        "{calls:?}"
    );
    assert_eq!(
        calls.iter().filter(|call| **call == "upsert").count(),
        1,
        "{calls:?}"
    );
    assert_eq!(
        stored_versions(&harness).await,
        vec![
            ("page/1.html".to_owned(), "1".to_owned()),
            (
                "page/1/attachment/att-1/notes.txt".to_owned(),
                "1".to_owned()
            ),
            ("page/1/comment/500.html".to_owned(), "1".to_owned()),
            ("page/2.html".to_owned(), "2".to_owned()),
        ]
    );

    // A comment is edited: only the comment's chunks are rewritten.
    wiki.lock().expect("lock").comment_version = Some(2);
    let fourth = run(&harness, &configuration, &parameters, Some((MODEL, 4))).await;
    assert_eq!(fourth.indexed_documents, 1);
    assert_eq!(fourth.documents[0].document_key, "page/1/comment/500.html");
    assert_eq!(fourth.documents[0].version, "2");

    // A page and the attachment disappear: their chunks are removed.
    {
        let mut wiki = wiki.lock().expect("lock");
        wiki.pages.truncate(1);
        wiki.attachment = false;
    }
    let fifth = run(&harness, &configuration, &parameters, Some((MODEL, 4))).await;
    assert_eq!(fifth.status, IndexIngestStatusV1::Ok as i32, "{fifth:?}");
    assert_eq!(fifth.indexed_documents, 0);
    assert_eq!(
        fifth.removed_document_keys,
        vec!["page/1/attachment/att-1/notes.txt", "page/2.html"]
    );
    assert_eq!(fifth.total_documents, Some(2));
    assert_eq!(
        stored_versions(&harness).await,
        vec![
            ("page/1.html".to_owned(), "1".to_owned()),
            ("page/1/comment/500.html".to_owned(), "2".to_owned()),
        ]
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn confluence_credential_refusal_is_an_error_summary_not_an_empty_index() {
    let mut harness = harness(4, None).await;
    harness.runtime.transport =
        Recorded::new(|_| (StatusCode::UNAUTHORIZED, json!({"message": "no"})));
    let summary = run(
        &harness,
        &confluence_configuration(),
        &confluence_parameters(),
        None,
    )
    .await;
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Error as i32,
        "{summary:?}"
    );
    assert!(
        summary
            .message
            .contains("refused the credential (HTTP 401)"),
        "{}",
        summary.message
    );
    assert_eq!(summary.total_documents, None, "the store was never touched");
    assert_eq!(harness.fake.point_count(), 0);
}

// ---------------------------------------------------------------------- Jira

#[derive(Clone)]
struct Tracker {
    issues: Vec<(u64, &'static str, &'static str, &'static str)>,
    comment_updated: &'static str,
}

fn issue_json(id: u64, key: &str, updated: &str, body: &str) -> Value {
    json!({
        "id": id.to_string(), "key": key,
        "fields": {
            "summary": format!("Summary of {key}"),
            "description": {"type": "doc", "version": 1, "content": [
                {"type": "paragraph", "content": [{"type": "text", "text": body}]}
            ]},
            "status": {"name": "Open"}, "reporter": {"emailAddress": "ann@example.com"},
            "created": "2026-09-01T00:00:00.000+0000", "updated": updated,
            "project": {"key": "PROJ"}, "issuetype": {"name": "Task"},
            // Jira answers a requested, empty `security` as null.
            "security": null,
        }
    })
}

fn tracker_transport(state: Arc<Mutex<Tracker>>) -> Arc<Recorded> {
    Recorded::new(move |request| {
        let tracker = state.lock().expect("lock").clone();
        let path = request.url().path();
        if path == "/rest/api/3/search/jql" {
            assert_eq!(query(request, "jql").as_deref(), Some("project = PROJ"));
            let issues: Vec<Value> = tracker
                .issues
                .iter()
                .map(|(id, key, updated, body)| issue_json(*id, key, updated, body))
                .collect();
            return (StatusCode::OK, json!({"issues": issues, "isLast": true}));
        }
        if path == "/rest/api/3/issue/1/comment" {
            return (
                StatusCode::OK,
                json!({"comments": [{
                    "id": "77", "created": "c0", "updated": tracker.comment_updated,
                    "author": {"displayName": "Cy"},
                    "body": {"type": "doc", "content": [{"type": "paragraph", "content": [
                        {"type": "text", "text": format!("Comment as of {}.", tracker.comment_updated)}
                    ]}]},
                }], "maxResults": 100, "startAt": 0, "total": 1}),
            );
        }
        if path.ends_with("/comment") {
            return (StatusCode::OK, json!({"comments": [], "total": 0}));
        }
        if let Some(id) = path.strip_prefix("/rest/api/3/issue/") {
            let id: u64 = id.parse().expect("an issue id");
            let (id, key, updated, body) = tracker
                .issues
                .iter()
                .find(|(issue, _, _, _)| *issue == id)
                .copied()
                .expect("a listed issue");
            return (StatusCode::OK, issue_json(id, key, updated, body));
        }
        panic!("unexpected request {path}");
    })
}

fn jira_configuration() -> Value {
    json!({
        "id": 8, "type": "jira", "toolkit_name": "tracker",
        "settings": {
            "jira_configuration": {"base_url": "https://acme.atlassian.net", "username": "bot", "api_key": "fixture"},
        }
    })
}

fn jira_parameters() -> Value {
    json!({"index_name": "issues", "jql": "project = PROJ", "include_comments": true, "include_attachments": true})
}

#[tokio::test(flavor = "multi_thread")]
#[allow(clippy::too_many_lines, reason = "five runs over one recorded tracker")]
async fn jira_indexes_incrementally_with_comments() {
    let tracker = Arc::new(Mutex::new(Tracker {
        issues: vec![
            (
                1,
                "PROJ-1",
                "2026-10-01T00:00:00.000+0000",
                "First issue body.",
            ),
            (
                2,
                "PROJ-2",
                "2026-10-01T00:00:00.000+0000",
                "Second issue body.",
            ),
        ],
        comment_updated: "2026-10-01T01:00:00.000+0000",
    }));
    let mut harness = harness(4, None).await;
    harness.runtime.transport = tracker_transport(Arc::clone(&tracker));
    let configuration = jira_configuration();
    let parameters = jira_parameters();

    let first = run(&harness, &configuration, &parameters, None).await;
    assert_eq!(first.status, IndexIngestStatusV1::Ok as i32, "{first:?}");
    assert_eq!(first.indexed_documents, 3);
    assert_eq!(first.total_documents, Some(3));
    assert_eq!(
        stored_versions(&harness).await,
        vec![
            (
                "issue/1.md".to_owned(),
                "2026-10-01T00:00:00.000+0000".to_owned()
            ),
            (
                "issue/1/comment/77.md".to_owned(),
                "2026-10-01T01:00:00.000+0000".to_owned()
            ),
            (
                "issue/2.md".to_owned(),
                "2026-10-01T00:00:00.000+0000".to_owned()
            ),
        ]
    );

    // Unchanged: nothing written.
    let calls_before = harness.fake.calls().len();
    let embeddings_before = harness.embeddings.calls.load(Ordering::SeqCst);
    let second = run(&harness, &configuration, &parameters, Some((MODEL, 4))).await;
    assert_eq!(second.indexed_documents, 0);
    assert_eq!(second.total_documents, Some(3));
    assert_eq!(
        store_calls_since(&harness, calls_before),
        vec!["list_documents"]
    );
    assert_eq!(
        harness.embeddings.calls.load(Ordering::SeqCst),
        embeddings_before
    );

    // The comment is edited: only the comment is rewritten, the issue stays.
    tracker.lock().expect("lock").comment_updated = "2026-10-02T01:00:00.000+0000";
    let calls_before = harness.fake.calls().len();
    let third = run(&harness, &configuration, &parameters, Some((MODEL, 4))).await;
    assert_eq!(third.status, IndexIngestStatusV1::Ok as i32, "{third:?}");
    assert_eq!(third.indexed_documents, 1);
    assert_eq!(third.updated, 1);
    assert_eq!(third.documents[0].document_key, "issue/1/comment/77.md");
    let calls = store_calls_since(&harness, calls_before);
    assert_eq!(
        calls.iter().filter(|call| **call == "upsert").count(),
        1,
        "{calls:?}"
    );
    assert_eq!(
        stored_versions(&harness).await,
        vec![
            (
                "issue/1.md".to_owned(),
                "2026-10-01T00:00:00.000+0000".to_owned()
            ),
            (
                "issue/1/comment/77.md".to_owned(),
                "2026-10-02T01:00:00.000+0000".to_owned()
            ),
            (
                "issue/2.md".to_owned(),
                "2026-10-01T00:00:00.000+0000".to_owned()
            ),
        ]
    );

    // An issue changes: only the issue is rewritten.
    tracker.lock().expect("lock").issues[1] = (
        2,
        "PROJ-2",
        "2026-10-03T00:00:00.000+0000",
        "Second issue body, edited.",
    );
    let fourth = run(&harness, &configuration, &parameters, Some((MODEL, 4))).await;
    assert_eq!(fourth.indexed_documents, 1);
    assert_eq!(fourth.documents[0].document_key, "issue/2.md");
    assert_eq!(fourth.documents[0].version, "2026-10-03T00:00:00.000+0000");

    // An issue leaves the JQL: its chunks are removed.
    tracker.lock().expect("lock").issues.truncate(1);
    let fifth = run(&harness, &configuration, &parameters, Some((MODEL, 4))).await;
    assert_eq!(fifth.indexed_documents, 0);
    assert_eq!(fifth.removed_document_keys, vec!["issue/2.md"]);
    assert_eq!(fifth.total_documents, Some(2));
}

#[tokio::test(flavor = "multi_thread")]
async fn jira_credential_refusal_is_an_error_summary_not_an_empty_index() {
    let mut harness = harness(4, None).await;
    harness.runtime.transport = Recorded::new(|_| (StatusCode::FORBIDDEN, json!({})));
    let summary = run(&harness, &jira_configuration(), &jira_parameters(), None).await;
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Error as i32,
        "{summary:?}"
    );
    assert!(
        summary
            .message
            .contains("refused the credential (HTTP 403)"),
        "{}",
        summary.message
    );
    assert_eq!(harness.fake.point_count(), 0);
}
