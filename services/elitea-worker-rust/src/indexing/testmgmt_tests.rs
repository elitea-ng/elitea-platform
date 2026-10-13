//! ADR-0030 Verification (2) for the I3 test-management families:
//! `testrail`, `qtest`, `xray_cloud`, `zephyr_scale`, `zephyr_essential` and
//! `zephyr_enterprise`, end to end through the index run. Each family's
//! connector is built from a real toolkit configuration and `index_data`
//! arguments, lists and fetches against a recorded HTTP fixture shaped as the
//! vendor documents its API (a scripted transport, no network), and writes to
//! the in-process fake of `elitea-vector`. Each family indexes its fixture,
//! indexes it again unchanged (the store sees no write and no delete, and
//! nothing is embedded), then with one document changed (only its chunks are
//! rewritten) and one removed (its chunks go).

use std::collections::BTreeMap;
use std::sync::atomic::Ordering;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_connectors::transport::header::{CONTENT_TYPE, HeaderValue};
use elitea_connectors::transport::{
    HeaderMap, Request, Response, StatusCode, Transport, TransportError,
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

/// A canned HTTP answer.
struct Reply {
    status: StatusCode,
    headers: HeaderMap,
    body: Vec<u8>,
}

impl Reply {
    fn json(value: &Value) -> Self {
        let mut headers = HeaderMap::new();
        headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
        Self {
            status: StatusCode::OK,
            headers,
            body: value.to_string().into_bytes(),
        }
    }

    fn bytes(body: &[u8]) -> Self {
        Self {
            status: StatusCode::OK,
            headers: HeaderMap::new(),
            body: body.to_vec(),
        }
    }

    fn status(status: StatusCode) -> Self {
        Self {
            status,
            headers: HeaderMap::new(),
            body: Vec::new(),
        }
    }
}

type Handler = dyn Fn(&Request) -> Reply + Send + Sync;

/// The recorded provider: every request answered by `handler`, and kept.
struct Recorded {
    handler: Box<Handler>,
    seen: Mutex<Vec<String>>,
}

impl Recorded {
    fn new(handler: impl Fn(&Request) -> Reply + Send + Sync + 'static) -> Arc<Self> {
        Arc::new(Self {
            handler: Box::new(handler),
            seen: Mutex::new(Vec::new()),
        })
    }

    fn seen(&self) -> Vec<String> {
        self.seen.lock().expect("lock").clone()
    }
}

#[async_trait]
impl Transport for Recorded {
    async fn execute(&self, request: Request) -> Result<Response, TransportError> {
        self.seen.lock().expect("lock").push(format!(
            "{} {}",
            request.method(),
            request.url().path()
        ));
        let reply = (self.handler)(&request);
        Ok(Response::from_bytes(
            reply.status,
            reply.headers,
            reply.body,
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

/// The route of a `TestRail` request (`/api/v2/get_cases/5&limit=…`).
fn route(request: &Request) -> String {
    request.url().query().unwrap_or_default().to_owned()
}

fn body_json(request: &Request) -> Value {
    serde_json::from_slice(request.body().and_then(|b| b.as_bytes()).expect("body"))
        .expect("json body")
}

async fn run(
    harness: &Harness,
    toolkit_type: &str,
    settings: &Value,
    arguments: &Value,
    expected: Option<(&str, u32)>,
) -> IndexIngestSummaryV1 {
    let configuration =
        json!({"id": 7, "type": toolkit_type, "toolkit_name": "kit", "settings": settings});
    let mut parameters = json!({"index_name": "tests", "clean_index": false});
    if let Value::Object(extra) = arguments {
        for (key, value) in extra {
            parameters[key] = value.clone();
        }
    }
    let now = || 1_700_000_000_000_i64;
    run_index(
        &harness.runtime,
        IndexRun {
            command: &command(NS, expected),
            toolkit_configuration: configuration.to_string().as_bytes(),
            tool_parameters: parameters.to_string().as_bytes(),
            embedding_binding: Some(&binding()),
            llm_model: None,
            llm_configuration: None,
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

fn store_calls_since(harness: &Harness, before: usize) -> Vec<&'static str> {
    harness.fake.calls()[before..].to_vec()
}

/// The Verification (2) drill every family runs: index the fixture, index it
/// again unchanged, change one document, remove another.
#[allow(clippy::too_many_arguments)] // The drill's inputs, spelled out per family.
async fn drill<S: Send + 'static>(
    toolkit_type: &str,
    transport: Arc<Recorded>,
    state: &Arc<Mutex<S>>,
    settings: Value,
    arguments: Value,
    expected_keys: &[&str],
    change: impl FnOnce(&mut S),
    changed: &str,
    remove: impl FnOnce(&mut S),
    removed: &str,
) -> Arc<Recorded> {
    let mut harness = harness(4, None).await;
    harness.runtime.transport = transport.clone();

    // First run: the whole fixture.
    let first = run(&harness, toolkit_type, &settings, &arguments, None).await;
    assert_eq!(first.status, IndexIngestStatusV1::Ok as i32, "{first:?}");
    assert_eq!(
        first.indexed_documents,
        expected_keys.len() as u64,
        "{first:?}"
    );
    assert!(first.whole_index_record);
    let before: BTreeMap<String, String> = stored_versions(&harness).await;
    assert_eq!(
        before.keys().map(String::as_str).collect::<Vec<_>>(),
        expected_keys,
        "{toolkit_type}: the first run indexed the fixture"
    );

    // Unchanged source: nothing is written, deleted or embedded.
    let calls_before = harness.fake.calls().len();
    let embeddings_before = harness.embeddings.calls.load(Ordering::SeqCst);
    let second = run(
        &harness,
        toolkit_type,
        &settings,
        &arguments,
        Some((MODEL, 4)),
    )
    .await;
    assert_eq!(second.status, IndexIngestStatusV1::Ok as i32, "{second:?}");
    assert_eq!(second.indexed_documents, 0, "{toolkit_type}: {second:?}");
    assert_eq!(second.total_documents, Some(expected_keys.len() as u64));
    assert!(second.removed_document_keys.is_empty());
    assert_eq!(
        store_calls_since(&harness, calls_before),
        vec!["list_documents"],
        "{toolkit_type}: an unchanged source writes nothing"
    );
    assert_eq!(
        harness.embeddings.calls.load(Ordering::SeqCst),
        embeddings_before
    );
    assert_eq!(stored_versions(&harness).await, before);

    // One document changes: only its chunks are rewritten.
    change(&mut state.lock().expect("state"));
    let calls_before = harness.fake.calls().len();
    let third = run(
        &harness,
        toolkit_type,
        &settings,
        &arguments,
        Some((MODEL, 4)),
    )
    .await;
    assert_eq!(third.status, IndexIngestStatusV1::Ok as i32, "{third:?}");
    assert_eq!(
        third.indexed_documents, 1,
        "{toolkit_type}: only the changed document was indexed: {third:?}"
    );
    assert_eq!(third.updated, 1);
    assert_eq!(third.documents[0].document_key, changed);
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
    assert_ne!(after[changed], before[changed], "the version moved");
    for (key, version) in &before {
        if key != changed {
            assert_eq!(&after[key], version, "{toolkit_type}: {key} untouched");
        }
    }

    // One document disappears: its chunks are removed.
    remove(&mut state.lock().expect("state"));
    let fourth = run(
        &harness,
        toolkit_type,
        &settings,
        &arguments,
        Some((MODEL, 4)),
    )
    .await;
    assert_eq!(fourth.status, IndexIngestStatusV1::Ok as i32, "{fourth:?}");
    assert_eq!(fourth.indexed_documents, 0, "{toolkit_type}: {fourth:?}");
    assert_eq!(fourth.removed_document_keys, vec![removed.to_owned()]);
    assert!(!stored_versions(&harness).await.contains_key(removed));
    assert_eq!(
        stored_versions(&harness).await.len(),
        expected_keys.len() - 1,
        "{toolkit_type}: only the removed document went"
    );
    transport
}

/// A rejected credential is an error summary naming it, with the store never
/// touched.
async fn refusal(toolkit_type: &str, settings: &Value, arguments: &Value, needle: &str) {
    for status in [StatusCode::UNAUTHORIZED, StatusCode::FORBIDDEN] {
        let mut harness = harness(4, None).await;
        harness.runtime.transport = Recorded::new(move |_| Reply::status(status));
        let summary = run(&harness, toolkit_type, settings, arguments, None).await;
        assert_eq!(
            summary.status,
            IndexIngestStatusV1::Error as i32,
            "{summary:?}"
        );
        assert!(summary.message.contains(needle), "{}", summary.message);
        assert_eq!(summary.total_documents, None, "the store was never touched");
        assert_eq!(harness.fake.point_count(), 0);
    }
}

// ------------------------------------------------------------------ TestRail

/// Cases: id → (title, `updated_on`); one attachment on case 1 while present.
#[derive(Clone)]
struct Rail {
    cases: BTreeMap<u64, (&'static str, u64)>,
    attachment: bool,
}

fn rail_case(id: u64, title: &str, updated: u64) -> Value {
    json!({
        "id": id, "title": title, "section_id": 1, "template_id": 1, "type_id": 7,
        "priority_id": 2, "milestone_id": null, "refs": null, "created_by": 1,
        "created_on": 1_700_000_000, "updated_by": 1, "updated_on": updated,
        "estimate": null, "estimate_forecast": null, "suite_id": 1,
        "custom_steps_separated": [{"content": format!("Do {title}"), "expected": "It works"}],
        "labels": [],
    })
}

fn rail_transport(state: Arc<Mutex<Rail>>) -> Arc<Recorded> {
    Recorded::new(move |request| {
        let rail = state.lock().expect("lock").clone();
        let route = route(request);
        assert_eq!(request.url().host_str(), Some("acme.testrail.io"));
        if route.starts_with("/api/v2/get_project/5") {
            Reply::json(&json!({"id": 5, "name": "Shop", "suite_mode": 1}))
        } else if route.starts_with("/api/v2/get_cases/5") {
            let cases: Vec<Value> = rail
                .cases
                .iter()
                .map(|(id, (title, updated))| rail_case(*id, title, *updated))
                .collect();
            Reply::json(&json!({"offset": 0, "limit": 250, "size": cases.len(),
                "_links": {"next": null, "prev": null}, "cases": cases}))
        } else if route.starts_with("/api/v2/get_attachments_for_case/1") {
            let attachments = if rail.attachment {
                vec![
                    json!({"id": 301, "name": "notes.txt", "filename": "notes.txt", "size": 11,
                            "created_on": 1_700_000_000, "project_id": 5, "case_id": 1}),
                ]
            } else {
                Vec::new()
            };
            Reply::json(
                &json!({"offset": 0, "limit": 250, "size": attachments.len(),
                "_links": {"next": null, "prev": null}, "attachments": attachments}),
            )
        } else if route.starts_with("/api/v2/get_attachments_for_case/") {
            Reply::json(&json!({"offset": 0, "limit": 250, "size": 0,
                "_links": {"next": null, "prev": null}, "attachments": []}))
        } else if route.starts_with("/api/v2/get_attachment/301") {
            Reply::bytes(b"hello notes")
        } else if let Some(id) = route.strip_prefix("/api/v2/get_case/") {
            let id: u64 = id.parse().expect("a case id");
            let (title, updated) = rail.cases.get(&id).expect("a listed case");
            Reply::json(&rail_case(id, title, *updated))
        } else {
            panic!("unexpected request {route}")
        }
    })
}

#[tokio::test(flavor = "multi_thread")]
async fn testrail_indexes_cases_and_attachments_then_only_what_changed() {
    let state = Arc::new(Mutex::new(Rail {
        cases: BTreeMap::from([
            (1, ("Login", 100)),
            (2, ("Logout", 100)),
            (3, ("Search", 100)),
        ]),
        attachment: true,
    }));
    let settings = json!({"testrail_configuration": {
        "url": "https://acme.testrail.io", "email": "qa@acme.example", "password": "api-key"}});
    let transport = drill(
        "testrail",
        rail_transport(Arc::clone(&state)),
        &state,
        settings,
        json!({"project_id": "5", "include_attachments": true}),
        &[
            "case/1.json",
            "case/1/attachment/301/notes.txt",
            "case/2.json",
            "case/3.json",
        ],
        |rail| {
            rail.cases.insert(2, ("Logout everywhere", 200));
        },
        "case/2.json",
        |rail| {
            rail.cases.remove(&3);
        },
        "case/3.json",
    )
    .await;
    assert!(transport.seen().contains(&"GET /index.php".to_owned()));
}

#[tokio::test(flavor = "multi_thread")]
async fn testrail_removes_a_deleted_attachment_and_refuses_a_bad_credential() {
    let state = Arc::new(Mutex::new(Rail {
        cases: BTreeMap::from([(1, ("Login", 100))]),
        attachment: true,
    }));
    let settings = json!({"testrail_configuration": {
        "url": "https://acme.testrail.io", "email": "qa@acme.example", "password": "api-key"}});
    let arguments = json!({"project_id": "5", "include_attachments": true});
    let mut harness = harness(4, None).await;
    harness.runtime.transport = rail_transport(Arc::clone(&state));
    let first = run(&harness, "testrail", &settings, &arguments, None).await;
    assert_eq!(first.indexed_documents, 2, "{first:?}");
    state.lock().expect("lock").attachment = false;
    let second = run(
        &harness,
        "testrail",
        &settings,
        &arguments,
        Some((MODEL, 4)),
    )
    .await;
    assert_eq!(second.indexed_documents, 0);
    assert_eq!(
        second.removed_document_keys,
        vec!["case/1/attachment/301/notes.txt"]
    );
    refusal("testrail", &settings, &arguments, "refused the credential").await;
}

// --------------------------------------------------------------------- qTest

/// Cases: id → (`last_modified_date`, step text).
type Qtest = BTreeMap<u64, (&'static str, &'static str)>;

fn qtest_case(id: u64, modified: &str, step: &str) -> Value {
    json!({
        "id": id, "pid": format!("TC-{id}"), "name": format!("Case {id}"),
        "description": "<p>Checks it</p>", "precondition": "",
        "parent_id": 101, "created_date": "2026-09-01T00:00:00+00:00",
        "last_modified_date": modified, "version": "1.0",
        "properties": [{"field_id": 1, "field_name": "Status", "field_value": "401", "field_value_name": "New"}],
        "test_steps": [{"id": 1, "order": 1, "description": step, "expected": "ok"}],
    })
}

fn qtest_transport(state: Arc<Mutex<Qtest>>) -> Arc<Recorded> {
    Recorded::new(move |request| {
        let cases = state.lock().expect("lock").clone();
        let path = request.url().path();
        assert_eq!(request.url().host_str(), Some("acme.qtestnet.com"));
        if path == "/api/v3/projects/12/modules" {
            Reply::json(&json!([{"id": 101, "pid": "MD-7", "name": "Suite", "children": []}]))
        } else if path == "/api/v3/projects/12/test-cases" {
            let page: usize = query(request, "page")
                .and_then(|p| p.parse().ok())
                .unwrap_or(1);
            if page == 1 {
                Reply::json(&Value::Array(
                    cases
                        .iter()
                        .map(|(id, (modified, step))| qtest_case(*id, modified, step))
                        .collect(),
                ))
            } else {
                Reply::json(&json!([]))
            }
        } else {
            panic!("unexpected request {path}")
        }
    })
}

#[tokio::test(flavor = "multi_thread")]
async fn qtest_indexes_the_project_then_only_what_changed() {
    let state = Arc::new(Mutex::new(Qtest::from([
        (1, ("2026-09-02T00:00:00+00:00", "open")),
        (2, ("2026-09-02T00:00:00+00:00", "close")),
        (3, ("2026-09-02T00:00:00+00:00", "search")),
    ])));
    let settings = json!({"qtest_configuration": {"base_url": "https://acme.qtestnet.com", "qtest_api_token": "tok"},
                          "qtest_project_id": 12});
    drill(
        "qtest",
        qtest_transport(Arc::clone(&state)),
        &state,
        settings.clone(),
        json!({}),
        &["testcase/1.md", "testcase/2.md", "testcase/3.md"],
        |cases| {
            cases.insert(2, ("2026-09-09T00:00:00+00:00", "close everything"));
        },
        "testcase/2.md",
        |cases| {
            cases.remove(&3);
        },
        "testcase/3.md",
    )
    .await;
    refusal("qtest", &settings, &json!({}), "refused the credential").await;
}

// ---------------------------------------------------------------------- Xray

/// Tests: issueId → action text.
type Xray = BTreeMap<&'static str, &'static str>;

fn xray_test(issue: &str, action: &str) -> Value {
    json!({
        "issueId": issue, "projectId": "10000",
        "jira": {"key": format!("CALC-{issue}"), "summary": format!("Test {issue}"),
                 "description": "Desc", "created": "2026-09-01T00:00:00.000Z"},
        "testType": {"name": "Manual", "kind": "Steps"},
        "steps": [{"id": format!("st-{issue}"), "action": action, "data": "", "result": "ok",
                   "attachments": []}],
        "preconditions": {"total": 0, "start": 0, "limit": 100, "results": []},
        "unstructured": null, "gherkin": null,
    })
}

fn xray_transport(state: Arc<Mutex<Xray>>) -> Arc<Recorded> {
    Recorded::new(move |request| {
        let tests = state.lock().expect("lock").clone();
        let path = request.url().path();
        assert_eq!(request.url().host_str(), Some("xray.cloud.getxray.app"));
        if path == "/api/v1/authenticate" {
            Reply::json(&json!("jwt-token"))
        } else if path == "/api/v2/graphql" {
            assert_eq!(
                request
                    .headers()
                    .get("authorization")
                    .unwrap()
                    .to_str()
                    .unwrap(),
                "Bearer jwt-token"
            );
            let start = usize::try_from(
                body_json(request)["variables"]["start"]
                    .as_u64()
                    .unwrap_or(0),
            )
            .expect("start");
            let all: Vec<Value> = tests
                .iter()
                .map(|(id, action)| xray_test(id, action))
                .collect();
            let page: Vec<Value> = all.iter().skip(start).take(2).cloned().collect();
            Reply::json(&json!({"data": {"getTests": {
                "total": all.len(), "start": start, "limit": 100, "results": page}}}))
        } else {
            panic!("unexpected request {path}")
        }
    })
}

#[tokio::test(flavor = "multi_thread")]
async fn xray_indexes_a_jql_then_only_what_changed() {
    let state = Arc::new(Mutex::new(Xray::from([
        ("1001", "press plus"),
        ("1002", "press minus"),
        ("1003", "press equals"),
    ])));
    let settings = json!({"xray_configuration": {"client_id": "CID", "client_secret": "sec"}});
    drill(
        "xray_cloud",
        xray_transport(Arc::clone(&state)),
        &state,
        settings.clone(),
        json!({"jql": "project = CALC"}),
        &["test/1001.json", "test/1002.json", "test/1003.json"],
        |tests| {
            tests.insert("1002", "press minus twice");
        },
        "test/1002.json",
        |tests| {
            tests.remove("1003");
        },
        "test/1003.json",
    )
    .await;
    // Neither a JQL nor a GraphQL document: refused before any request.
    let mut harness = harness(4, None).await;
    harness.runtime.transport = Recorded::new(|_| panic!("no request"));
    let summary = run(&harness, "xray_cloud", &settings, &json!({}), None).await;
    assert_eq!(summary.status, IndexIngestStatusV1::Error as i32);
    assert_eq!(harness.fake.point_count(), 0);
    refusal(
        "xray_cloud",
        &settings,
        &json!({"jql": "x"}),
        "refused the client id or secret",
    )
    .await;
}

// ------------------------------------------------------------- Zephyr Scale

/// Cases: key → step text.
type Scale = BTreeMap<&'static str, &'static str>;

fn zephyr_case(key: &str, name: &str) -> Value {
    json!({
        "id": 1, "key": key, "name": name,
        "project": {"id": 10_000}, "createdOn": "2026-09-01T00:00:00Z",
        "objective": "obj", "labels": [], "priority": {"id": 1}, "status": {"id": 2},
        "folder": null, "customFields": {},
    })
}

fn envelope(values: &[Value]) -> Value {
    json!({"next": null, "startAt": 0, "maxResults": 200, "total": values.len(),
           "isLast": true, "values": values})
}

fn scale_transport(state: Arc<Mutex<Scale>>) -> Arc<Recorded> {
    Recorded::new(move |request| {
        let cases = state.lock().expect("lock").clone();
        let path = request.url().path().to_owned();
        assert_eq!(
            request.url().host_str(),
            Some("api.zephyrscale.smartbear.com")
        );
        if path == "/v2/folders" {
            Reply::json(&envelope(&[]))
        } else if path == "/v2/testcases" {
            let values: Vec<Value> = cases
                .keys()
                .map(|key| zephyr_case(key, &format!("Case {key}")))
                .collect();
            Reply::json(&envelope(&values))
        } else if let Some(key) = path
            .strip_prefix("/v2/testcases/")
            .and_then(|rest| rest.strip_suffix("/teststeps"))
        {
            let step = cases.get(key).expect("a listed case");
            Reply::json(&envelope(&[
                json!({"inline": {"description": step, "testData": null,
                "expectedResult": "ok", "customFields": {}, "reflectRef": null}, "testCase": null}),
            ]))
        } else {
            panic!("unexpected request {path}")
        }
    })
}

#[tokio::test(flavor = "multi_thread")]
async fn zephyr_scale_indexes_a_project_then_only_what_changed() {
    let state = Arc::new(Mutex::new(Scale::from([
        ("PROJ-T1", "open"),
        ("PROJ-T2", "close"),
        ("PROJ-T3", "search"),
    ])));
    let settings = json!({"zephyr_configuration": {"token": "tok"}});
    drill(
        "zephyr_scale",
        scale_transport(Arc::clone(&state)),
        &state,
        settings.clone(),
        json!({"project_key": "PROJ"}),
        &[
            "testcase/PROJ-T1.json",
            "testcase/PROJ-T2.json",
            "testcase/PROJ-T3.json",
        ],
        |cases| {
            cases.insert("PROJ-T2", "close it all");
        },
        "testcase/PROJ-T2.json",
        |cases| {
            cases.remove("PROJ-T3");
        },
        "testcase/PROJ-T3.json",
    )
    .await;
    refusal(
        "zephyr_scale",
        &settings,
        &json!({"project_key": "PROJ"}),
        "refused the credential",
    )
    .await;
    // A `jql` clause this connector cannot honour is refused up front.
    let mut harness = harness(4, None).await;
    harness.runtime.transport = Recorded::new(|_| panic!("no request"));
    let summary = run(
        &harness,
        "zephyr_scale",
        &settings,
        &json!({"project_key": "PROJ", "jql": "steps ~ \"x\""}),
        None,
    )
    .await;
    assert_eq!(summary.status, IndexIngestStatusV1::Error as i32);
}

// ---------------------------------------------------------- Zephyr Essential

fn essential_transport(state: Arc<Mutex<Scale>>) -> Arc<Recorded> {
    Recorded::new(move |request| {
        let cases = state.lock().expect("lock").clone();
        let path = request.url().path().to_owned();
        assert_eq!(
            request.url().host_str(),
            Some("prod-api.zephyr4jiracloud.com")
        );
        if path == "/v2/testcases" {
            let values: Vec<Value> = cases
                .keys()
                .map(|key| zephyr_case(key, &format!("Case {key}")))
                .collect();
            Reply::json(&envelope(&values))
        } else if let Some(key) = path
            .strip_prefix("/v2/testcases/")
            .and_then(|rest| rest.strip_suffix("/teststeps"))
        {
            let step = cases.get(key).expect("a listed case");
            Reply::json(&envelope(&[
                json!({"inline": {"description": step, "testData": null,
                "expectedResult": "ok", "customFields": {}, "reflectRef": null}, "testCase": null}),
            ]))
        } else {
            panic!("unexpected request {path}")
        }
    })
}

#[tokio::test(flavor = "multi_thread")]
async fn zephyr_essential_indexes_every_case_then_only_what_changed() {
    let state = Arc::new(Mutex::new(Scale::from([
        ("PROJ-T1", "open"),
        ("PROJ-T2", "close"),
        ("PROJ-T3", "search"),
    ])));
    let settings = json!({"zephyr_essential_configuration": {"token": "tok"}});
    drill(
        "zephyr_essential",
        essential_transport(Arc::clone(&state)),
        &state,
        settings.clone(),
        json!({}),
        &[
            "testcase/PROJ-T1.json",
            "testcase/PROJ-T2.json",
            "testcase/PROJ-T3.json",
        ],
        |cases| {
            cases.insert("PROJ-T2", "close it all");
        },
        "testcase/PROJ-T2.json",
        |cases| {
            cases.remove("PROJ-T3");
        },
        "testcase/PROJ-T3.json",
    )
    .await;
    refusal(
        "zephyr_essential",
        &settings,
        &json!({}),
        "refused the credential",
    )
    .await;
}

// --------------------------------------------------------- Zephyr Enterprise

/// Cases: `testcase.id` (the version id) → (lastModifiedOn, step text).
type Enterprise = BTreeMap<u64, (u64, &'static str)>;

fn enterprise_case(id: u64, modified: u64) -> Value {
    json!({"id": id, "testcaseId": id * 10, "name": format!("Case {id}"), "projectId": 75,
           "projectName": "Shop", "testcaseType": "Manual", "lastModifiedOn": modified})
}

fn enterprise_transport(state: Arc<Mutex<Enterprise>>) -> Arc<Recorded> {
    const PREFIX: &str = "/flex/services/rest/latest";
    Recorded::new(move |request| {
        let cases = state.lock().expect("lock").clone();
        let path = request.url().path().to_owned();
        assert_eq!(request.url().host_str(), Some("zephyr.corp.example"));
        if path == format!("{PREFIX}/testcase") {
            let results: Vec<Value> = cases
                .iter()
                .map(|(id, (modified, _))| json!({"testcase": enterprise_case(*id, *modified)}))
                .collect();
            Reply::json(&json!({"resultSize": results.len(), "results": results}))
        } else if path.ends_with("/teststep") {
            // The case's own version id (`testcase.id`) is what the steps
            // path takes.
            let id: u64 = path
                .trim_start_matches(&format!("{PREFIX}/testcase/"))
                .trim_end_matches("/teststep")
                .parse()
                .expect("version id");
            let (_, step) = cases.get(&id).expect("a listed case");
            Reply::json(&json!({"id": 5, "steps": [{"orderId": 1, "step": step, "result": "ok"}]}))
        } else if let Some(id) = path.strip_prefix(&format!("{PREFIX}/testcase/")) {
            let id: u64 = id.parse().expect("tree id");
            let (modified, _) = cases.get(&id).expect("a listed case");
            Reply::json(&json!({"testcase": enterprise_case(id, *modified)}))
        } else {
            panic!("unexpected request {path}")
        }
    })
}

#[tokio::test(flavor = "multi_thread")]
async fn zephyr_enterprise_indexes_a_zql_then_only_what_changed() {
    let state = Arc::new(Mutex::new(Enterprise::from([
        (1, (100, "open")),
        (2, (100, "close")),
        (3, (100, "search")),
    ])));
    let settings = json!({"zephyr_configuration": {"base_url": "https://zephyr.corp.example", "token": "tok"}});
    drill(
        "zephyr_enterprise",
        enterprise_transport(Arc::clone(&state)),
        &state,
        settings.clone(),
        json!({"zql": "folder=\"Smoke\""}),
        &["testcase/1.json", "testcase/2.json", "testcase/3.json"],
        |cases| {
            cases.insert(2, (200, "close it all"));
        },
        "testcase/2.json",
        |cases| {
            cases.remove(&3);
        },
        "testcase/3.json",
    )
    .await;
    refusal(
        "zephyr_enterprise",
        &settings,
        &json!({"zql": "x"}),
        "refused the credential",
    )
    .await;
}
