//! ADR-0030 Verification (2) for the I3 Figma family: a recorded Figma file
//! (a scripted HTTP transport answering in the REST API's documented shapes,
//! no network) indexed through the real connector, pipeline and the
//! in-process `elitea-vector` fake. The file is indexed, then indexed again:
//! an unchanged file (even one saved again, with a new `version`) writes
//! nothing; one frame whose text changed rewrites only that frame's chunks;
//! a resolved comment rewrites only the comment; a deleted frame has its
//! chunks removed. The credential is refused with an error summary, never an
//! empty index.

use std::collections::BTreeMap;
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

/// A recorded provider: `handler` maps a request to a status and a JSON body.
struct Recorded {
    handler: Box<Handler>,
    requests: Mutex<Vec<Request>>,
}

impl Recorded {
    fn new(handler: impl Fn(&Request) -> (StatusCode, Value) + Send + Sync + 'static) -> Arc<Self> {
        Arc::new(Self {
            handler: Box::new(handler),
            requests: Mutex::new(Vec::new()),
        })
    }

    fn requests(&self) -> Vec<Request> {
        self.requests.lock().expect("lock").clone()
    }
}

#[async_trait]
impl Transport for Recorded {
    async fn execute(&self, request: Request) -> Result<Response, TransportError> {
        self.requests.lock().expect("lock").push(request.clone());
        let (status, body) = (self.handler)(&request);
        let mut headers = HeaderMap::new();
        headers.insert("content-type", HeaderValue::from_static("application/json"));
        Ok(Response::from_bytes(
            status,
            headers,
            body.to_string().into_bytes(),
        ))
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

async fn stored_versions(harness: &Harness) -> BTreeMap<String, String> {
    stored(harness)
        .await
        .into_iter()
        .map(|point| (point.document_key, point.document_version))
        .collect()
}

/// The store calls since `before`.
fn store_calls_since(harness: &Harness, before: usize) -> Vec<&'static str> {
    harness.fake.calls()[before..].to_vec()
}

fn text(characters: &str, size: f64, weight: f64) -> Value {
    json!({
        "id": "t:0", "name": "Text", "type": "TEXT", "characters": characters,
        "style": {"fontSize": size, "fontWeight": weight},
        "fills": [{"type": "SOLID", "color": {"r": 0.1, "g": 0.1, "b": 0.1, "a": 1.0}}],
        "absoluteBoundingBox": {"x": 0.0, "y": 0.0, "width": 90.0, "height": 20.0},
    })
}

fn frame(id: &str, name: &str, texts: &[(&str, f64, f64)]) -> Value {
    json!({
        "id": id, "name": name, "type": "FRAME",
        "absoluteBoundingBox": {"x": 0.0, "y": 0.0, "width": 375.0, "height": 812.0},
        "children": texts.iter().map(|(t, size, weight)| text(t, *size, *weight)).collect::<Vec<_>>(),
    })
}

/// The recorded file `K1`: two pages of frames and one comment.
#[derive(Clone)]
struct Design {
    /// Figma's file `version`: it moves on every save, text changed or not.
    saved: u32,
    login: Vec<(&'static str, f64, f64)>,
    show_dashboard: bool,
    comment_resolved: bool,
}

fn design_transport(state: Arc<Mutex<Design>>) -> Arc<Recorded> {
    Recorded::new(move |request| {
        let design = state.lock().expect("lock").clone();
        let path = request.url().path();
        let mut pages = vec![("0:1", "Auth", vec![frame("1:2", "Login", &design.login)])];
        if design.show_dashboard {
            pages.push((
                "0:2",
                "Home",
                vec![frame(
                    "1:3",
                    "Dashboard",
                    &[("Usage this month", 20.0, 700.0)],
                )],
            ));
        }
        let page_json = |id: &str, name: &str, frames: &[Value]| json!({"id": id, "name": name, "type": "CANVAS", "children": frames});
        if path == "/v1/files/K1" {
            assert_eq!(query(request, "depth").as_deref(), Some("1"));
            return (
                StatusCode::OK,
                json!({
                    "name": "Mobile App", "role": "owner", "lastModified": "2026-10-01T09:00:00Z",
                    "editorType": "figma", "version": design.saved.to_string(),
                    "document": {"id": "0:0", "name": "Document", "type": "DOCUMENT",
                        "children": pages.iter().map(|(id, name, _)| page_json(id, name, &[])).collect::<Vec<_>>()},
                }),
            );
        }
        if path == "/v1/files/K1/nodes" {
            let mut nodes = serde_json::Map::new();
            for id in query(request, "ids").expect("ids").split(',') {
                let page = pages.iter().find(|page| page.0 == id).expect("a page");
                nodes.insert(
                    id.to_owned(),
                    json!({"document": page_json(page.0, page.1, &page.2), "components": {}}),
                );
            }
            return (
                StatusCode::OK,
                json!({"name": "Mobile App", "nodes": nodes}),
            );
        }
        if path == "/v1/files/K1/comments" {
            return (
                StatusCode::OK,
                json!({"comments": [{
                    "id": "100", "user": {"handle": "Ada"}, "created_at": "2026-10-02T10:00:00Z",
                    "resolved_at": if design.comment_resolved { json!("2026-10-03T10:00:00Z") } else { Value::Null },
                    "message": "Make the sign in button larger",
                    "client_meta": {"node_id": "1:2", "node_offset": {"x": 1, "y": 2}},
                }]}),
            );
        }
        panic!("unexpected request {path}");
    })
}

fn configuration() -> Value {
    json!({
        "id": 9, "type": "figma", "toolkit_name": "designs",
        "settings": {"figma_configuration": {"token": "figd_fixture"}}
    })
}

fn parameters() -> Value {
    json!({"index_name": "designs", "urls_or_file_keys": "K1", "include_comments": true})
}

fn original() -> Design {
    Design {
        saved: 1,
        login: vec![
            ("Welcome back", 24.0, 700.0),
            ("Sign in to continue", 14.0, 400.0),
        ],
        show_dashboard: true,
        comment_resolved: false,
    }
}

#[tokio::test(flavor = "multi_thread")]
#[allow(clippy::too_many_lines, reason = "six runs over one recorded file")]
async fn figma_indexes_incrementally_with_comments() {
    let design = Arc::new(Mutex::new(original()));
    let mut harness = harness(4, None).await;
    let transport = design_transport(Arc::clone(&design));
    harness.runtime.transport = transport.clone();
    let configuration = configuration();
    let parameters = parameters();
    let login = "file/K1/frame/1-2.txt";
    let dashboard = "file/K1/frame/1-3.txt";
    let comment = "file/K1/comment/100.txt";

    // First run: two frames and a comment.
    let first = run(&harness, &configuration, &parameters, None).await;
    assert_eq!(first.status, IndexIngestStatusV1::Ok as i32, "{first:?}");
    assert_eq!(first.indexed_documents, 3);
    assert_eq!(first.total_documents, Some(3));
    assert!(first.whole_index_record);
    let before = stored_versions(&harness).await;
    assert_eq!(
        before.keys().map(String::as_str).collect::<Vec<_>>(),
        vec![comment, login, dashboard]
    );
    assert!(before.values().all(|version| version.len() == 64));
    for request in transport.requests() {
        assert_eq!(request.url().host_str(), Some("api.figma.com"));
        assert_eq!(
            request
                .headers()
                .get("x-figma-token")
                .map(|t| t.to_str().unwrap()),
            Some("figd_fixture")
        );
    }

    // Unchanged source: nothing is written or deleted, and the file saved
    // again without a text change does not count as a change.
    design.lock().expect("lock").saved = 2;
    let calls_before = harness.fake.calls().len();
    let embeddings_before = harness.embeddings.calls.load(Ordering::SeqCst);
    let second = run(&harness, &configuration, &parameters, Some((MODEL, 4))).await;
    assert_eq!(second.status, IndexIngestStatusV1::Ok as i32, "{second:?}");
    assert_eq!(second.indexed_documents, 0);
    assert_eq!(second.total_documents, Some(3));
    assert_eq!(second.removed_document_keys, [] as [String; 0]);
    assert_eq!(
        store_calls_since(&harness, calls_before),
        vec!["list_documents"]
    );
    assert_eq!(
        harness.embeddings.calls.load(Ordering::SeqCst),
        embeddings_before
    );

    // One frame's text changes: only its chunks are rewritten.
    design.lock().expect("lock").login = vec![
        ("Welcome back", 24.0, 700.0),
        ("Sign in with your work email to continue", 14.0, 400.0),
    ];
    let calls_before = harness.fake.calls().len();
    let third = run(&harness, &configuration, &parameters, Some((MODEL, 4))).await;
    assert_eq!(third.status, IndexIngestStatusV1::Ok as i32, "{third:?}");
    assert_eq!(third.indexed_documents, 1);
    assert_eq!(third.updated, 1);
    assert_eq!(third.documents.len(), 1);
    assert_eq!(third.documents[0].document_key, login);
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
    let after = stored_versions(&harness).await;
    assert_ne!(after[login], before[login]);
    assert_eq!(after[dashboard], before[dashboard]);
    assert_eq!(after[comment], before[comment]);

    // The comment is resolved: only the comment is rewritten.
    design.lock().expect("lock").comment_resolved = true;
    let fourth = run(&harness, &configuration, &parameters, Some((MODEL, 4))).await;
    assert_eq!(fourth.indexed_documents, 1);
    assert_eq!(fourth.documents[0].document_key, comment);
    let resolved = stored_versions(&harness).await;
    assert_ne!(resolved[comment], after[comment]);
    assert_eq!(resolved[login], after[login]);

    // A frame disappears: its chunks are removed, nothing else is touched.
    design.lock().expect("lock").show_dashboard = false;
    let fifth = run(&harness, &configuration, &parameters, Some((MODEL, 4))).await;
    assert_eq!(fifth.status, IndexIngestStatusV1::Ok as i32, "{fifth:?}");
    assert_eq!(fifth.indexed_documents, 0);
    assert_eq!(fifth.removed_document_keys, vec![dashboard]);
    assert_eq!(fifth.total_documents, Some(2));
    let last = stored_versions(&harness).await;
    assert_eq!(
        last.keys().map(String::as_str).collect::<Vec<_>>(),
        vec![comment, login]
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn figma_credential_refusal_is_an_error_summary_not_an_empty_index() {
    for status in [StatusCode::FORBIDDEN, StatusCode::UNAUTHORIZED] {
        let mut harness = harness(4, None).await;
        harness.runtime.transport = Recorded::new(move |_| {
            (
                status,
                json!({"status": status.as_u16(), "err": "Invalid token"}),
            )
        });
        let summary = run(&harness, &configuration(), &parameters(), None).await;
        assert_eq!(
            summary.status,
            IndexIngestStatusV1::Error as i32,
            "{summary:?}"
        );
        assert!(
            summary.message.contains("refused the credential"),
            "{}",
            summary.message
        );
        assert!(!summary.message.contains("figd_fixture"));
        assert_eq!(summary.total_documents, None, "the store was never touched");
        assert_eq!(harness.fake.point_count(), 0);
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn figma_without_files_to_index_is_refused_before_any_request() {
    let mut harness = harness(4, None).await;
    let transport = Recorded::new(|_| panic!("no request may be sent"));
    harness.runtime.transport = transport.clone();
    let summary = run(
        &harness,
        &configuration(),
        &json!({"index_name": "designs"}),
        None,
    )
    .await;
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Error as i32,
        "{summary:?}"
    );
    assert!(
        summary.message.contains("urls_or_file_keys"),
        "{}",
        summary.message
    );
    assert!(transport.requests().is_empty());
}
