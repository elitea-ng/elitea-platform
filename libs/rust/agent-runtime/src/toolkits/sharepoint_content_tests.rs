//! The `SharePoint` tools that read files and pages as text and that change
//! things (`create_list_item`, `upload_file`, the `OneNote` edits), against a
//! fake Graph that records every request it receives.
//!
//! Each tool has its success on the wire (method, address, headers, body) and
//! each cause of failure its own sentence: a refused change is not an unknown
//! one, a lost answer is not a refusal, and nothing is retried.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_core::{AdkError, ErrorCategory, ReadonlyContext, Toolset};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE, HeaderMap};
use reqwest::{Method, Request, StatusCode, Url};
use serde_json::{Value, json};

use super::delegated_auth::delegated_authorization_requirement;
use super::families::office_host::{
    OfficeFailure, OfficeHostError, OfficeModelRequest, OfficeObjectWritten, OfficeToolHost,
    SharedOfficeToolHost, TextExtractionFailure,
};
use super::families::sharepoint::client::{
    SharePointApi, SharePointClient, SharePointClientError, SharePointClientErrorCode,
    SharePointHttpResponse, SharePointTransport, plain_http_transport,
};
use super::families::sharepoint::config::SharePointToolkitConfig;
use super::families::sharepoint::tools::test_build_with_api;
use super::sharepoint_tests::{SITE_URL, context, policy, settings, tokens};

const SITE: &str = "graph.microsoft.com/v1.0/sites/tenant.sharepoint.com,site-id,web-id";
const SITE_LOOKUP: &str = "graph.microsoft.com/v1.0/sites/tenant.sharepoint.com:/teams/project";

// ---------------------------------------------------------------- fake Graph

/// What a route answers: a stored HTTP answer or a transport failure.
#[derive(Clone)]
enum Reply {
    Http {
        status: u16,
        content_type: Option<&'static str>,
        location: Option<String>,
        body: Vec<u8>,
    },
    Fail(SharePointClientErrorCode),
}

impl Reply {
    fn json(status: u16, body: &Value) -> Self {
        Self::Http {
            status,
            content_type: Some("application/json; charset=utf-8"),
            location: None,
            body: serde_json::to_vec(body).expect("fixture JSON"),
        }
    }

    fn status(status: u16) -> Self {
        Self::Http {
            status,
            content_type: None,
            location: None,
            body: Vec::new(),
        }
    }

    fn html(body: &str) -> Self {
        Self::Http {
            status: 200,
            content_type: Some("text/html; charset=utf-8"),
            location: None,
            body: body.as_bytes().to_vec(),
        }
    }

    fn bytes(body: &[u8]) -> Self {
        Self::Http {
            status: 200,
            content_type: Some("application/octet-stream"),
            location: None,
            body: body.to_vec(),
        }
    }

    fn redirect(to: &str) -> Self {
        Self::Http {
            status: 302,
            content_type: None,
            location: Some(to.to_owned()),
            body: Vec::new(),
        }
    }
}

struct Seen {
    method: Method,
    key: String,
    url: Url,
    headers: HeaderMap,
    body: Vec<u8>,
}

#[derive(Default)]
struct FakeGraph {
    routes: Mutex<Vec<(Method, String, VecDeque<Reply>)>>,
    seen: Mutex<Vec<Seen>>,
}

impl FakeGraph {
    fn new() -> Arc<Self> {
        Arc::new(Self::default())
    }

    /// Answers `method` `key` (`host` + encoded path) with `replies` in turn;
    /// the last one repeats.
    fn on(self: &Arc<Self>, method: Method, key: &str, replies: Vec<Reply>) -> &Arc<Self> {
        self.routes
            .lock()
            .expect("routes")
            .push((method, key.to_owned(), replies.into()));
        self
    }

    /// The routes every tool that resolves the site, the libraries or the
    /// default library needs.
    fn with_site(self: &Arc<Self>) -> &Arc<Self> {
        self.on(
            Method::GET,
            SITE_LOOKUP,
            vec![Reply::json(
                200,
                &json!({"id":"tenant.sharepoint.com,site-id,web-id"}),
            )],
        );
        self.on(
            Method::GET,
            &format!("{SITE}/drives"),
            vec![Reply::json(
                200,
                &json!({"value":[
                    {"id":"drive-docs","name":"Documents","webUrl":"https://tenant.sharepoint.com/teams/project/Shared%20Documents"},
                    {"id":"drive-priv","name":"Private Docs","webUrl":"https://tenant.sharepoint.com/teams/project/Private%20Docs"}
                ]}),
            )],
        );
        self.on(
            Method::GET,
            &format!("{SITE}/drive"),
            vec![Reply::json(200, &json!({"id":"drive-docs"}))],
        )
    }

    fn with_list(self: &Arc<Self>) -> &Arc<Self> {
        self.on(
            Method::GET,
            &format!("{SITE}/lists"),
            vec![Reply::json(
                200,
                &json!({"value":[{"id":"list-1","displayName":"Tasks","name":"Tasks"}]}),
            )],
        )
    }

    fn requests(&self) -> Vec<(Method, String)> {
        self.seen
            .lock()
            .expect("seen")
            .iter()
            .map(|seen| (seen.method.clone(), seen.key.clone()))
            .collect()
    }

    fn count(&self, method: &Method, key: &str) -> usize {
        self.requests()
            .iter()
            .filter(|(m, k)| m == method && k == key)
            .count()
    }

    /// The one request made with `method` to `key`.
    fn only(&self, method: &Method, key: &str) -> Seen {
        let seen = self.seen.lock().expect("seen");
        let mut matching = seen
            .iter()
            .filter(|seen| seen.method == *method && seen.key == key);
        let found = matching.next().unwrap_or_else(|| {
            panic!(
                "no {method} {key}; saw {:?}",
                seen.iter().map(|s| (&s.method, &s.key)).collect::<Vec<_>>()
            )
        });
        assert!(matching.next().is_none(), "{method} {key} was sent twice");
        Seen {
            method: found.method.clone(),
            key: found.key.clone(),
            url: found.url.clone(),
            headers: found.headers.clone(),
            body: found.body.clone(),
        }
    }

    fn touched(&self, host: &str) -> bool {
        self.seen
            .lock()
            .expect("seen")
            .iter()
            .any(|seen| seen.url.host_str() == Some(host))
    }
}

#[async_trait]
impl SharePointTransport for FakeGraph {
    async fn execute(
        &self,
        request: Request,
    ) -> Result<SharePointHttpResponse, SharePointClientError> {
        let key = format!(
            "{}{}",
            request.url().host_str().unwrap_or_default(),
            request.url().path()
        );
        self.seen.lock().expect("seen").push(Seen {
            method: request.method().clone(),
            key: key.clone(),
            url: request.url().clone(),
            headers: request.headers().clone(),
            body: request
                .body()
                .and_then(|body| body.as_bytes())
                .map(<[u8]>::to_vec)
                .unwrap_or_default(),
        });
        let reply = {
            let mut routes = self.routes.lock().expect("routes");
            let route = routes
                .iter_mut()
                .find(|(method, route_key, _)| method == request.method() && *route_key == key)
                .unwrap_or_else(|| panic!("unrouted {} {key}", request.method()));
            if route.2.len() > 1 {
                route.2.pop_front().expect("a reply")
            } else {
                route.2.front().expect("a reply").clone()
            }
        };
        match reply {
            Reply::Http {
                status,
                content_type,
                location,
                body,
            } => Ok(SharePointHttpResponse {
                status: StatusCode::from_u16(status).expect("status"),
                content_type: content_type.map(Into::into),
                location: location.map(Into::into),
                oversize: false,
                body,
            }),
            Reply::Fail(code) => Err(SharePointClientError::fixture(code)),
        }
    }
}

fn client(graph: &Arc<FakeGraph>) -> Arc<dyn SharePointApi> {
    let config = SharePointToolkitConfig::parse(
        "Project SharePoint",
        &settings(&["get_lists"]),
        &tokens(SITE_URL),
    )
    .expect("authorized configuration");
    Arc::new(SharePointClient::with_transport(
        config.into_client_parts().expect("client parts"),
        graph.clone(),
    ))
}

// ------------------------------------------------------------------ fake host

/// The Office host of the tests: artifacts in a map, and a document reader
/// that returns the bytes as text unless told to fail.
struct FakeHost {
    objects: Mutex<BTreeMap<(String, String), Vec<u8>>>,
    read_failure: Mutex<Option<OfficeFailure>>,
    extraction_failure: Mutex<Option<TextExtractionFailure>>,
    /// A refusal `can_read_text` gives before any download.
    precheck_failure: Mutex<Option<TextExtractionFailure>>,
    extracted: Mutex<Vec<(String, usize)>>,
}

impl FakeHost {
    fn new() -> Arc<Self> {
        Arc::new(Self {
            objects: Mutex::new(BTreeMap::new()),
            read_failure: Mutex::new(None),
            extraction_failure: Mutex::new(None),
            precheck_failure: Mutex::new(None),
            extracted: Mutex::new(Vec::new()),
        })
    }

    fn with_object(self: &Arc<Self>, bucket: &str, name: &str, bytes: &[u8]) -> &Arc<Self> {
        self.objects
            .lock()
            .expect("objects")
            .insert((bucket.to_owned(), name.to_owned()), bytes.to_vec());
        self
    }

    fn shared(self: &Arc<Self>) -> SharedOfficeToolHost {
        self.clone()
    }
}

#[async_trait]
impl OfficeToolHost for FakeHost {
    async fn read_object(
        &self,
        bucket: &str,
        name: &str,
        max_bytes: u64,
    ) -> Result<Vec<u8>, OfficeHostError> {
        if let Some(kind) = *self.read_failure.lock().expect("failure") {
            return Err(OfficeHostError::new(kind, "scripted"));
        }
        let objects = self.objects.lock().expect("objects");
        let bytes = objects
            .get(&(bucket.to_owned(), name.to_owned()))
            .ok_or_else(|| OfficeHostError::new(OfficeFailure::NotFound, "missing"))?;
        if bytes.len() as u64 > max_bytes {
            return Err(OfficeHostError::new(OfficeFailure::TooLarge, "big"));
        }
        Ok(bytes.clone())
    }

    async fn write_object(
        &self,
        _: &str,
        _: &str,
        _: &str,
        _: Vec<u8>,
        _: bool,
    ) -> Result<OfficeObjectWritten, OfficeHostError> {
        Err(OfficeHostError::new(OfficeFailure::Unreachable, "unused"))
    }

    async fn complete(&self, _: OfficeModelRequest) -> Result<String, OfficeHostError> {
        Err(OfficeHostError::new(OfficeFailure::NoModel, "unused"))
    }

    fn can_read_text(&self, name: &str) -> Result<(), TextExtractionFailure> {
        if let Some(failure) = *self.precheck_failure.lock().expect("failure") {
            return Err(failure);
        }
        // Images and archives are not text, as the worker's host says.
        let lower = name.to_ascii_lowercase();
        if [".png", ".jpg", ".zip", ".mp4"]
            .iter()
            .any(|e| lower.ends_with(e))
        {
            return Err(TextExtractionFailure::Unsupported);
        }
        Ok(())
    }

    async fn extract_text(
        &self,
        name: &str,
        bytes: Vec<u8>,
    ) -> Result<String, TextExtractionFailure> {
        self.extracted
            .lock()
            .expect("extracted")
            .push((name.to_owned(), bytes.len()));
        if let Some(failure) = *self.extraction_failure.lock().expect("failure") {
            return Err(failure);
        }
        Ok(String::from_utf8_lossy(&bytes).into_owned())
    }
}

// ---------------------------------------------------------------- tool calls

async fn call(
    api: &Arc<dyn SharePointApi>,
    host: Option<&SharedOfficeToolHost>,
    tool: &str,
    arguments: Value,
) -> Result<Value, AdkError> {
    let toolset = test_build_with_api(
        "Project SharePoint",
        SITE_URL,
        &[tool.to_owned()],
        &policy(&[]),
        api,
        host,
    )
    .expect("toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("tools");
    tools[0].execute(context(), arguments).await
}

async fn ok(
    api: &Arc<dyn SharePointApi>,
    host: Option<&SharedOfficeToolHost>,
    tool: &str,
    arguments: Value,
) -> Value {
    call(api, host, tool, arguments)
        .await
        .unwrap_or_else(|error| panic!("{tool} failed: {error}"))
}

/// A call that fails as an ERROR (an invalid argument, a rejected token).
async fn err(
    api: &Arc<dyn SharePointApi>,
    host: Option<&SharedOfficeToolHost>,
    tool: &str,
    arguments: Value,
) -> AdkError {
    call(api, host, tool, arguments)
        .await
        .expect_err("the call must fail")
}

/// A call whose failure the tool words itself: the answer is a sentence, so
/// the model reads the cause (the runtime would reduce an error to a generic
/// line).
async fn refused(
    api: &Arc<dyn SharePointApi>,
    host: Option<&SharedOfficeToolHost>,
    tool: &str,
    arguments: Value,
) -> String {
    match call(api, host, tool, arguments).await {
        Ok(Value::String(sentence)) if sentence.starts_with("Could not ") => sentence,
        other => panic!("{tool} should answer a refusal sentence, got {other:?}"),
    }
}

fn assert_invalid_arguments(error: &AdkError) {
    assert_eq!(error.category, ErrorCategory::InvalidInput, "{error:?}");
}

// One fragment per cause the client can name.
const INVALID: &str = "rejected the request as invalid";
const FORBIDDEN: &str = "did not authorize the request";
const MISSING: &str = "was not found";
const CONFLICT: &str = "conflicts with the current state";
const TOO_BIG: &str = "exceeds its approved limit";
const RATE_LIMITED: &str = "rate limited";
const UNAVAILABLE: &str = "Microsoft Graph is unavailable";
const UNKNOWN: &str = "may or may not have been applied";
const BAD_ANSWER: &str = "invalid SharePoint response";

/// Each cause in its own words: no two of these may share a sentence.
fn assert_one_sentence_per_cause(sentences: &[String]) {
    let distinct = sentences.iter().collect::<std::collections::BTreeSet<_>>();
    assert_eq!(distinct.len(), sentences.len(), "{sentences:#?}");
}

// ------------------------------------------------------------ create_list_item

#[tokio::test]
async fn create_list_item_posts_the_fields_once_and_projects_the_item() {
    let graph = FakeGraph::new();
    graph.with_site().with_list().on(
        Method::POST,
        &format!("{SITE}/lists/list-1/items"),
        vec![Reply::json(
            201,
            &json!({"id":"7","fields":{"Title":"New"},"webUrl":"https://tenant.sharepoint.com/item/7","@odata.etag":"x"}),
        )],
    );
    let api = client(&graph);
    let result = ok(
        &api,
        None,
        "create_list_item",
        json!({"list_title":"tasks","fields":{"Title":"New","Status":"Open"}}),
    )
    .await;
    assert_eq!(
        result,
        json!({"id":"7","fields":{"Title":"New"},"webUrl":"https://tenant.sharepoint.com/item/7"})
    );
    let post = graph.only(&Method::POST, &format!("{SITE}/lists/list-1/items"));
    assert_eq!(
        serde_json::from_slice::<Value>(&post.body).expect("body"),
        json!({"fields":{"Title":"New","Status":"Open"}})
    );
    assert_eq!(post.headers[CONTENT_TYPE], "application/json");
    assert!(post.headers[AUTHORIZATION].is_sensitive());
}

#[tokio::test]
async fn create_list_item_rejects_what_the_sdk_rejects_before_any_request() {
    let graph = FakeGraph::new();
    let api = client(&graph);
    for arguments in [
        json!({"list_title":"Tasks","fields":{}}),
        json!({"list_title":"Tasks"}),
        json!({"list_title":"Tasks","fields":"Title=x"}),
        json!({"list_title":"","fields":{"Title":"x"}}),
        json!({"list_title":"Tasks","fields":{"":"x"}}),
        json!({"list_title":"Tasks","fields":{"Title":"x"},"extra":1}),
    ] {
        let error = err(&api, None, "create_list_item", arguments.clone()).await;
        assert_invalid_arguments(&error);
    }
    let oversized = json!({"list_title":"Tasks","fields":{"Title":"x".repeat(70_000)}});
    // The runtime refuses a single argument string over 64 KiB.
    assert!(
        call(&api, None, "create_list_item", oversized)
            .await
            .is_err()
    );
    assert!(graph.requests().is_empty(), "nothing was sent");
}

#[tokio::test]
async fn create_list_item_names_each_cause_and_never_retries() {
    // (answer, the code the model sees)
    let cases: Vec<(Reply, &str)> = vec![
        (Reply::status(400), INVALID),
        (Reply::status(403), FORBIDDEN),
        (Reply::status(404), MISSING),
        (Reply::status(409), CONFLICT),
        (Reply::status(413), TOO_BIG),
        (Reply::status(429), RATE_LIMITED),
        (Reply::status(503), UNAVAILABLE),
        (Reply::status(500), UNKNOWN),
        (Reply::status(502), UNKNOWN),
        (Reply::status(504), UNKNOWN),
        (Reply::Fail(SharePointClientErrorCode::Timeout), UNKNOWN),
        (
            Reply::Fail(SharePointClientErrorCode::ConnectionLost),
            UNKNOWN,
        ),
        // The connection never came up: the request was not sent.
        (
            Reply::Fail(SharePointClientErrorCode::DependencyUnavailable),
            UNAVAILABLE,
        ),
    ];
    for (reply, code) in cases {
        let graph = FakeGraph::new();
        graph.with_site().with_list().on(
            Method::POST,
            &format!("{SITE}/lists/list-1/items"),
            vec![reply],
        );
        let api = client(&graph);
        let sentence = refused(
            &api,
            None,
            "create_list_item",
            json!({"list_title":"Tasks","fields":{"Title":"x"}}),
        )
        .await;
        assert!(sentence.contains(code), "{sentence}");
        // A refused or unreachable request was not applied; only an answer
        // that did not come back leaves it unknown.
        assert_eq!(sentence.contains(UNKNOWN), code == UNKNOWN, "{sentence}");
        assert_eq!(
            graph.count(&Method::POST, &format!("{SITE}/lists/list-1/items")),
            1,
            "{code}: sent exactly once"
        );
    }
}

#[tokio::test]
async fn an_unknown_outcome_tells_the_model_to_check_before_repeating() {
    let graph = FakeGraph::new();
    graph.with_site().with_list().on(
        Method::POST,
        &format!("{SITE}/lists/list-1/items"),
        vec![Reply::Fail(SharePointClientErrorCode::Timeout)],
    );
    let api = client(&graph);
    let sentence = refused(
        &api,
        None,
        "create_list_item",
        json!({"list_title":"Tasks","fields":{"Title":"x"}}),
    )
    .await;
    assert_eq!(
        sentence,
        "Could not create the list item: SharePoint did not confirm the change: it may or may not have been applied, so check the target before repeating it."
    );
}

#[tokio::test]
async fn a_rejected_token_on_a_change_asks_for_authorization_and_sent_nothing_durable() {
    let graph = FakeGraph::new();
    graph.with_site().with_list().on(
        Method::POST,
        &format!("{SITE}/lists/list-1/items"),
        vec![Reply::status(401)],
    );
    let api = client(&graph);
    let error = err(
        &api,
        None,
        "create_list_item",
        json!({"list_title":"Tasks","fields":{"Title":"x"}}),
    )
    .await;
    assert!(delegated_authorization_requirement(&error).is_some());
}

#[tokio::test]
async fn create_list_item_with_an_unknown_list_is_not_found_and_posts_nothing() {
    let graph = FakeGraph::new();
    graph.with_site().with_list();
    let api = client(&graph);
    let sentence = refused(
        &api,
        None,
        "create_list_item",
        json!({"list_title":"Missing","fields":{"Title":"x"}}),
    )
    .await;
    assert!(sentence.contains(MISSING), "{sentence}");
    assert_eq!(
        graph.count(&Method::POST, &format!("{SITE}/lists/list-1/items")),
        0
    );
}

// ----------------------------------------------------------------- upload_file

const UPLOAD_ITEM: &str =
    "graph.microsoft.com/v1.0/drives/drive-docs/root:/Reports/a%20b.txt:/content";

fn uploaded_item() -> Value {
    json!({
        "id":"01ABC","name":"a b.txt","size":5,
        "webUrl":"https://tenant.sharepoint.com/teams/project/Shared%20Documents/Reports/a%20b.txt",
        "parentReference":{"path":"/drives/drive-docs/root:/Reports"},
        "file":{"mimeType":"text/plain"}
    })
}

#[tokio::test]
async fn upload_file_puts_text_into_the_library_folder_and_replaces_by_default() {
    let graph = FakeGraph::new();
    graph.with_site().on(
        Method::PUT,
        UPLOAD_ITEM,
        vec![Reply::json(201, &uploaded_item())],
    );
    let api = client(&graph);
    let result = ok(
        &api,
        None,
        "upload_file",
        json!({
            "folder_path":"/teams/project/Shared Documents/Reports",
            "filedata":"hello",
            "filename":"a b.txt"
        }),
    )
    .await;
    assert_eq!(
        result,
        json!({
            "id":"01ABC",
            "webUrl":"https://tenant.sharepoint.com/teams/project/Shared%20Documents/Reports/a%20b.txt",
            "path":"/drives/drive-docs/root:/Reports/a b.txt",
            "size":5,
            "mime_type":"text/plain"
        })
    );
    let put = graph.only(&Method::PUT, UPLOAD_ITEM);
    assert_eq!(put.body, b"hello");
    assert_eq!(
        put.url.query(),
        Some("@microsoft.graph.conflictBehavior=replace")
    );
    assert_eq!(put.headers[CONTENT_TYPE], "text/plain");
}

#[tokio::test]
async fn upload_file_with_replace_false_asks_graph_to_fail_on_a_taken_name() {
    let graph = FakeGraph::new();
    graph
        .with_site()
        .on(Method::PUT, UPLOAD_ITEM, vec![Reply::status(409)]);
    let api = client(&graph);
    let sentence = refused(
        &api,
        None,
        "upload_file",
        json!({
            "folder_path":"/teams/project/Shared Documents/Reports",
            "filedata":"hello","filename":"a b.txt","replace":false
        }),
    )
    .await;
    assert!(sentence.contains(CONFLICT), "{sentence}");
    assert!(
        !sentence.contains(UNKNOWN),
        "a refusal is not an unknown outcome"
    );
    let put = graph.only(&Method::PUT, UPLOAD_ITEM);
    assert_eq!(
        put.url.query(),
        Some("@microsoft.graph.conflictBehavior=fail")
    );
}

#[tokio::test]
async fn upload_file_resolves_another_library_and_the_library_root() {
    let graph = FakeGraph::new();
    graph.with_site().on(
        Method::PUT,
        "graph.microsoft.com/v1.0/drives/drive-priv/root:/note.txt:/content",
        vec![Reply::json(
            201,
            &json!({"id":"1","name":"note.txt","size":2,"parentReference":{"path":"/drives/drive-priv/root:"}}),
        )],
    );
    let api = client(&graph);
    let result = ok(
        &api,
        None,
        "upload_file",
        json!({"folder_path":"/teams/project/Private Docs","filedata":"hi","filename":"note.txt"}),
    )
    .await;
    assert_eq!(result["path"], "/drives/drive-priv/root:/note.txt");
    assert_eq!(result["mime_type"], "text/plain");
}

#[tokio::test]
async fn upload_file_names_a_target_that_round_trips() {
    // A name with a space, a hash, a percent sign and a non-ASCII letter
    // reaches Graph as one encoded segment and comes back as itself.
    let graph = FakeGraph::new();
    let key =
        "graph.microsoft.com/v1.0/drives/drive-docs/root:/Q%231%20100%25%20%C3%BCber.txt:/content";
    graph.with_site().on(
        Method::PUT,
        key,
        vec![Reply::json(
            201,
            &json!({"id":"1","name":"Q#1 100% über.txt","size":1,"parentReference":{"path":"/drives/drive-docs/root:"}}),
        )],
    );
    let api = client(&graph);
    let result = ok(
        &api,
        None,
        "upload_file",
        json!({"folder_path":"/teams/project/Shared Documents","filedata":"x","filename":"Q#1 100% über.txt"}),
    )
    .await;
    assert_eq!(result["path"], "/drives/drive-docs/root:/Q#1 100% über.txt");
    graph.only(&Method::PUT, key);
}

#[tokio::test]
async fn upload_file_refuses_names_that_could_leave_the_folder_or_break_the_address() {
    let graph = FakeGraph::new();
    graph.with_site();
    let api = client(&graph);
    for name in [
        "../x.txt",
        "a/b.txt",
        "a\\b.txt",
        "a:b.txt",
        "..",
        ".",
        "a?b.txt",
        "a*.txt",
        " lead.txt",
        "trail.",
        "a\u{7}b",
    ] {
        // Refused before any request, either as an invalid argument or as a
        // name SharePoint cannot hold.
        match call(
            &api,
            None,
            "upload_file",
            json!({"folder_path":"/teams/project/Shared Documents","filedata":"x","filename":name}),
        )
        .await
        {
            Err(error) => assert_invalid_arguments(&error),
            Ok(Value::String(sentence)) => {
                assert!(
                    sentence.starts_with("Could not upload the file: "),
                    "{sentence}"
                );
                assert!(sentence.contains(INVALID), "{name:?}: {sentence}");
            }
            other => panic!("{name:?}: {other:?}"),
        }
    }
    assert!(
        graph
            .requests()
            .iter()
            .all(|(method, _)| *method == Method::GET),
        "no write was sent"
    );
}

#[tokio::test]
async fn upload_file_validates_its_sources_with_the_sdk_sentences() {
    let graph = FakeGraph::new();
    let api = client(&graph);
    let host = FakeHost::new();
    host.with_object("bucket", "a.txt", b"x");
    let host = host.shared();
    let folder = "/teams/project/Shared Documents";
    for (arguments, message) in [
        (
            json!({"folder_path":folder}),
            "Either filepath or filedata must be provided",
        ),
        (
            json!({"folder_path":folder,"filedata":""}),
            "Either filepath or filedata must be provided",
        ),
        (
            json!({"folder_path":folder,"filepath":"/bucket/a.txt","filedata":"x"}),
            "Cannot specify both filepath and filedata",
        ),
        (
            json!({"folder_path":folder,"filedata":"x"}),
            "filename is required when using filedata",
        ),
        (
            json!({"folder_path":folder,"filepath":"bucket/a.txt"}),
            "filepath must have the format /{bucket}/{filename}",
        ),
        (
            json!({"folder_path":folder,"filepath":"/bucket"}),
            "filepath must have the format /{bucket}/{filename}",
        ),
    ] {
        let sentence = refused(&api, Some(&host), "upload_file", arguments.clone()).await;
        assert_eq!(
            sentence,
            format!("Could not upload the file: {message}."),
            "{arguments}"
        );
    }
    assert!(graph.requests().is_empty());
}

#[tokio::test]
async fn upload_file_reads_an_artifact_and_names_each_way_the_read_can_fail() {
    let folder = "/teams/project/Shared Documents/Reports";
    // From an artifact: the name defaults to the artifact's own.
    let graph = FakeGraph::new();
    graph.with_site().on(
        Method::PUT,
        "graph.microsoft.com/v1.0/drives/drive-docs/root:/Reports/a%20b.txt:/content",
        vec![Reply::json(201, &uploaded_item())],
    );
    let api = client(&graph);
    let host = FakeHost::new();
    host.with_object("bucket", "dir/a b.txt", b"from the bucket");
    let shared = host.shared();
    let result = ok(
        &api,
        Some(&shared),
        "upload_file",
        json!({"folder_path":folder,"filepath":"/bucket/dir/a b.txt"}),
    )
    .await;
    assert_eq!(result["id"], "01ABC");
    assert_eq!(
        graph.only(&Method::PUT, UPLOAD_ITEM).body,
        b"from the bucket"
    );

    // No host: the artifact cannot be read, and nothing is sent.
    let graph = FakeGraph::new();
    let api = client(&graph);
    let sentence = refused(
        &api,
        None,
        "upload_file",
        json!({"folder_path":folder,"filepath":"/bucket/a.txt"}),
    )
    .await;
    assert!(sentence.contains("lends no artifact store"), "{sentence}");
    assert!(graph.requests().is_empty());

    // Each failure of the artifact read is its own sentence.
    let mut seen = Vec::new();
    for (kind, fragment) in [
        (OfficeFailure::NotFound, "does not exist"),
        (OfficeFailure::AccessRefused, "refused access"),
        (OfficeFailure::TooLarge, "larger than the 50 MB"),
        (
            OfficeFailure::InvalidRequest,
            "did not accept the bucket or file name",
        ),
        (
            OfficeFailure::Unreachable,
            "could not be reached, so nothing was uploaded",
        ),
    ] {
        let graph = FakeGraph::new();
        let api = client(&graph);
        let host = FakeHost::new();
        *host.read_failure.lock().expect("failure") = Some(kind);
        let shared = host.shared();
        let sentence = refused(
            &api,
            Some(&shared),
            "upload_file",
            json!({"folder_path":folder,"filepath":"/bucket/a.txt"}),
        )
        .await;
        assert!(sentence.contains(fragment), "{sentence}");
        assert!(
            graph.requests().is_empty(),
            "nothing is sent for {fragment}"
        );
        seen.push(sentence);
    }
    assert_one_sentence_per_cause(&seen);
}

#[tokio::test]
async fn upload_file_failures_name_each_cause_and_a_lost_answer_is_unknown() {
    let cases: Vec<(Reply, &str)> = vec![
        (Reply::status(403), FORBIDDEN),
        (Reply::status(404), MISSING),
        (Reply::status(409), CONFLICT),
        (Reply::status(423), CONFLICT),
        (Reply::status(413), TOO_BIG),
        (Reply::status(429), RATE_LIMITED),
        (Reply::status(504), UNKNOWN),
        (Reply::Fail(SharePointClientErrorCode::Timeout), UNKNOWN),
        (
            Reply::Fail(SharePointClientErrorCode::ConnectionLost),
            UNKNOWN,
        ),
        (
            Reply::Fail(SharePointClientErrorCode::DependencyUnavailable),
            UNAVAILABLE,
        ),
    ];
    for (reply, code) in cases {
        let graph = FakeGraph::new();
        graph.with_site().on(Method::PUT, UPLOAD_ITEM, vec![reply]);
        let api = client(&graph);
        let sentence = refused(
            &api,
            None,
            "upload_file",
            json!({"folder_path":"/teams/project/Shared Documents/Reports","filedata":"hello","filename":"a b.txt"}),
        )
        .await;
        assert!(sentence.contains(code), "{sentence}");
        assert_eq!(sentence.contains(UNKNOWN), code == UNKNOWN, "{sentence}");
        assert_eq!(
            graph.count(&Method::PUT, UPLOAD_ITEM),
            1,
            "{code}: sent once"
        );
    }
}

const SESSION: &str =
    "graph.microsoft.com/v1.0/drives/drive-docs/root:/Reports/big.bin:/createUploadSession";
const UPLOAD_PATH: &str = "tenant.sharepoint.com/_api/v2.0/drives/d/uploadSession";

fn big_bytes() -> Vec<u8> {
    vec![7_u8; 6 * 1_024 * 1_024]
}

fn session_reply() -> Reply {
    Reply::json(
        200,
        &json!({"uploadUrl":"https://tenant.sharepoint.com/_api/v2.0/drives/d/uploadSession?guid=1"}),
    )
}

fn big_host() -> SharedOfficeToolHost {
    let host = FakeHost::new();
    host.with_object("bucket", "big.bin", &big_bytes());
    host.shared()
}

#[tokio::test]
async fn a_file_over_4_mib_goes_up_in_5_mib_chunks_without_the_bearer() {
    let graph = FakeGraph::new();
    graph
        .with_site()
        .on(Method::POST, SESSION, vec![session_reply()])
        .on(
            Method::PUT,
            UPLOAD_PATH,
            vec![
                Reply::json(202, &json!({"nextExpectedRanges":["5242880-"]})),
                Reply::json(
                    201,
                    &json!({"id":"BIG","name":"big.bin","size":6_291_456,"parentReference":{"path":"/drives/drive-docs/root:/Reports"}}),
                ),
            ],
        );
    let api = client(&graph);
    let host = big_host();
    let result = ok(
        &api,
        Some(&host),
        "upload_file",
        json!({"folder_path":"/teams/project/Shared Documents/Reports","filepath":"/bucket/big.bin"}),
    )
    .await;
    assert_eq!(result["id"], "BIG");
    assert_eq!(result["path"], "/drives/drive-docs/root:/Reports/big.bin");
    assert_eq!(result["mime_type"], "application/octet-stream");

    let session = graph.only(&Method::POST, SESSION);
    assert_eq!(
        serde_json::from_slice::<Value>(&session.body).expect("session body"),
        json!({"item":{"@microsoft.graph.conflictBehavior":"replace"}})
    );
    let seen = graph.seen.lock().expect("seen");
    let chunks = seen
        .iter()
        .filter(|seen| seen.method == Method::PUT)
        .collect::<Vec<_>>();
    assert_eq!(chunks.len(), 2);
    assert_eq!(
        chunks[0].headers["content-range"],
        "bytes 0-5242879/6291456"
    );
    assert_eq!(chunks[0].body.len(), 5 * 1_024 * 1_024);
    assert_eq!(
        chunks[1].headers["content-range"],
        "bytes 5242880-6291455/6291456"
    );
    assert_eq!(chunks[1].body.len(), 1_024 * 1_024);
    for chunk in chunks {
        assert!(
            chunk.headers.get(AUTHORIZATION).is_none(),
            "the upload URL is pre-authenticated; the bearer is not sent to it"
        );
    }
}

#[tokio::test]
async fn an_early_chunk_failure_cancels_the_session_and_is_a_plain_failure() {
    let graph = FakeGraph::new();
    graph
        .with_site()
        .on(Method::POST, SESSION, vec![session_reply()])
        .on(Method::PUT, UPLOAD_PATH, vec![Reply::status(500)])
        .on(Method::DELETE, UPLOAD_PATH, vec![Reply::status(204)]);
    let api = client(&graph);
    let host = big_host();
    let sentence = refused(
        &api,
        Some(&host),
        "upload_file",
        json!({"folder_path":"/teams/project/Shared Documents/Reports","filepath":"/bucket/big.bin"}),
    )
    .await;
    // Nothing is committed before the last chunk lands, so this one is not
    // an unknown outcome.
    assert!(sentence.contains(UNAVAILABLE), "{sentence}");
    assert!(!sentence.contains(UNKNOWN), "{sentence}");
    assert_eq!(
        graph.count(&Method::DELETE, UPLOAD_PATH),
        1,
        "session cancelled"
    );
    assert_eq!(
        graph.count(&Method::PUT, UPLOAD_PATH),
        1,
        "no further chunk"
    );
}

#[tokio::test]
async fn a_lost_answer_to_the_last_chunk_is_an_unknown_outcome_and_the_session_is_cancelled() {
    let graph = FakeGraph::new();
    graph
        .with_site()
        .on(Method::POST, SESSION, vec![session_reply()])
        .on(
            Method::PUT,
            UPLOAD_PATH,
            vec![
                Reply::json(202, &json!({})),
                Reply::Fail(SharePointClientErrorCode::Timeout),
            ],
        )
        .on(Method::DELETE, UPLOAD_PATH, vec![Reply::status(204)]);
    let api = client(&graph);
    let host = big_host();
    let sentence = refused(
        &api,
        Some(&host),
        "upload_file",
        json!({"folder_path":"/teams/project/Shared Documents/Reports","filepath":"/bucket/big.bin"}),
    )
    .await;
    assert!(sentence.contains(UNKNOWN), "{sentence}");
    assert_eq!(graph.count(&Method::DELETE, UPLOAD_PATH), 1);
}

#[tokio::test]
async fn an_upload_url_off_sharepoint_is_never_sent_the_file() {
    for upload_url in [
        "https://evil.example/upload",
        "http://tenant.sharepoint.com/upload",
        "https://tenant.sharepoint.com.evil.example/upload",
        "https://sharepoint.com/upload",
        "https://user:pw@tenant.sharepoint.com/upload",
        "https://tenant.sharepoint.com:8443/upload",
    ] {
        let graph = FakeGraph::new();
        graph.with_site().on(
            Method::POST,
            SESSION,
            vec![Reply::json(200, &json!({"uploadUrl":upload_url}))],
        );
        let api = client(&graph);
        let host = big_host();
        let sentence = refused(
            &api,
            Some(&host),
            "upload_file",
            json!({"folder_path":"/teams/project/Shared Documents/Reports","filepath":"/bucket/big.bin"}),
        )
        .await;
        assert!(sentence.contains(BAD_ANSWER), "{upload_url}: {sentence}");
        assert!(
            graph
                .requests()
                .iter()
                .all(|(method, _)| *method != Method::PUT),
            "{upload_url}: no chunk was sent"
        );
        assert!(!graph.touched("evil.example"));
    }
}

#[tokio::test]
async fn an_upload_over_50_mib_or_64_kib_of_text_is_refused_before_it_is_buffered() {
    let graph = FakeGraph::new();
    let api = client(&graph);
    let host = FakeHost::new();
    host.with_object("bucket", "huge.bin", &vec![0_u8; 50 * 1_024 * 1_024 + 1]);
    let shared = host.shared();
    let sentence = refused(
        &api,
        Some(&shared),
        "upload_file",
        json!({"folder_path":"/teams/project/Shared Documents","filepath":"/bucket/huge.bin"}),
    )
    .await;
    assert!(sentence.contains("larger than the 50 MB"), "{sentence}");
    // Text over 64 KiB does not fit one argument; a file goes up as an artifact.
    let error = err(
        &api,
        None,
        "upload_file",
        json!({"folder_path":"/x","filedata":"x".repeat(64 * 1_024 + 1),"filename":"a.txt"}),
    )
    .await;
    assert_eq!(error.category, ErrorCategory::InvalidInput);
    assert!(graph.requests().is_empty());
}

// ------------------------------------------------------------- read_document

const DOCS_CONTENT: &str =
    "graph.microsoft.com/v1.0/drives/drive-docs/root:/Folder/Notes.txt:/content";
const DOWNLOAD: &str = "tenant.sharepoint.com/_layouts/15/download.aspx";
const DOWNLOAD_URL: &str =
    "https://tenant.sharepoint.com/_layouts/15/download.aspx?UniqueId=abc&tempauth=XYZ";

fn with_download(graph: &Arc<FakeGraph>, content_key: &str, bytes: &[u8]) {
    graph
        .on(
            Method::GET,
            content_key,
            vec![Reply::redirect(DOWNLOAD_URL)],
        )
        .on(Method::GET, DOWNLOAD, vec![Reply::bytes(bytes)]);
}

#[tokio::test]
async fn read_document_follows_graphs_redirect_without_the_bearer_and_returns_the_text() {
    let graph = FakeGraph::new();
    graph.with_site();
    with_download(&graph, DOCS_CONTENT, b"line one\nline two\nline three\n");
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let text = ok(
        &api,
        Some(&host),
        "read_document",
        json!({"path":"/teams/project/Shared Documents/Folder/Notes.txt"}),
    )
    .await;
    assert_eq!(text, "line one\nline two\nline three\n");
    let graph_hop = graph.only(&Method::GET, DOCS_CONTENT);
    assert!(graph_hop.headers[AUTHORIZATION].is_sensitive());
    let download_hop = graph.only(&Method::GET, DOWNLOAD);
    assert!(
        download_hop.headers.get(AUTHORIZATION).is_none(),
        "the pre-authenticated URL gets no bearer"
    );
    assert_eq!(download_hop.url.query(), Some("UniqueId=abc&tempauth=XYZ"));
}

#[tokio::test]
async fn read_document_accepts_the_path_get_files_list_returned_and_keeps_odd_names() {
    // `Path` of a listed file is `/drives/<id>/root:/<folder>/<name>`, literal:
    // a name with a space, a hash and a percent sign reaches Graph as one
    // encoded segment per part.
    let key = "graph.microsoft.com/v1.0/drives/drive-docs/root:/100%25%20done%20%231/Q1%20%2350%25.txt:/content";
    let graph = FakeGraph::new();
    graph.with_site();
    with_download(&graph, key, b"odd");
    let api = client(&graph);
    let host = FakeHost::new();
    let shared = host.shared();
    let text = ok(
        &api,
        Some(&shared),
        "read_document",
        json!({"path":"/drives/drive-docs/root:/100% done #1/Q1 #50%.txt"}),
    )
    .await;
    assert_eq!(text, "odd");
    assert_eq!(
        host.extracted.lock().expect("extracted").as_slice(),
        [("Q1 #50%.txt".to_owned(), 3)],
        "the reader is told the file's own name"
    );
}

#[tokio::test]
async fn read_document_resolves_a_bare_path_by_probing_libraries_and_prefers_the_default() {
    // Only the private library has the folder.
    let graph = FakeGraph::new();
    graph
        .with_site()
        .on(
            Method::GET,
            "graph.microsoft.com/v1.0/drives/drive-docs/root:/Folder/Notes.txt",
            vec![Reply::status(404)],
        )
        .on(
            Method::GET,
            "graph.microsoft.com/v1.0/drives/drive-priv/root:/Folder/Notes.txt",
            vec![Reply::json(200, &json!({"id":"item"}))],
        );
    with_download(
        &graph,
        "graph.microsoft.com/v1.0/drives/drive-priv/root:/Folder/Notes.txt:/content",
        b"private",
    );
    let api = client(&graph);
    let host = FakeHost::new().shared();
    assert_eq!(
        ok(
            &api,
            Some(&host),
            "read_document",
            json!({"path":"Folder/Notes.txt"})
        )
        .await,
        "private"
    );

    // Both libraries have it: the default library is read.
    let graph = FakeGraph::new();
    graph.with_site();
    for drive in ["drive-docs", "drive-priv"] {
        graph.on(
            Method::GET,
            &format!("graph.microsoft.com/v1.0/drives/{drive}/root:/Folder/Notes.txt"),
            vec![Reply::json(200, &json!({"id":"item"}))],
        );
    }
    with_download(&graph, DOCS_CONTENT, b"default");
    let api = client(&graph);
    assert_eq!(
        ok(
            &api,
            Some(&host),
            "read_document",
            json!({"path":"Folder/Notes.txt"})
        )
        .await,
        "default"
    );
}

#[tokio::test]
async fn a_probe_that_fails_for_another_reason_is_an_error_not_a_skipped_library() {
    let graph = FakeGraph::new();
    graph.with_site().on(
        Method::GET,
        "graph.microsoft.com/v1.0/drives/drive-docs/root:/Folder/Notes.txt",
        vec![Reply::status(429)],
    );
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let sentence = refused(
        &api,
        Some(&host),
        "read_document",
        json!({"path":"Folder/Notes.txt"}),
    )
    .await;
    assert!(sentence.contains(RATE_LIMITED), "{sentence}");
}

#[tokio::test]
async fn read_document_refuses_a_library_root_and_executables_before_any_request() {
    let graph = FakeGraph::new();
    graph.with_site();
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let sentence = refused(
        &api,
        Some(&host),
        "read_document",
        json!({"path":"/teams/project/Shared Documents"}),
    )
    .await;
    assert!(sentence.contains("does not point to a file"), "{sentence}");
    assert_eq!(graph.count(&Method::GET, DOCS_CONTENT), 0);

    let graph = FakeGraph::new();
    let api = client(&graph);
    for path in [
        "/teams/project/Shared Documents/tool.EXE",
        "x/y.dll",
        "lib.so",
        "a.pyc",
    ] {
        let sentence = refused(&api, Some(&host), "read_document", json!({"path":path})).await;
        assert_eq!(
            sentence,
            "Could not read the document: Not supported type of files entered. Supported types are TXT, DOCX, PDF, PPTX, XLSX and XLS only.",
            "{path}"
        );
    }
    assert!(graph.requests().is_empty());
}

#[tokio::test]
async fn read_document_stays_inside_the_toolkits_site() {
    // A Graph drive path names its drive; only the site's own libraries are
    // read, whatever drive id the model was handed.
    let graph = FakeGraph::new();
    graph.with_site();
    graph.on(
        Method::GET,
        "graph.microsoft.com/v1.0/drives/b!personal/root:/secret.txt:/content",
        vec![Reply::bytes(b"must not be read")],
    );
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let sentence = refused(
        &api,
        Some(&host),
        "read_document",
        json!({"path":"/drives/b!personal/root:/secret.txt"}),
    )
    .await;
    assert!(sentence.contains(MISSING), "{sentence}");
    assert_eq!(
        graph.count(
            &Method::GET,
            "graph.microsoft.com/v1.0/drives/b!personal/root:/secret.txt:/content"
        ),
        0
    );
}

#[tokio::test]
async fn read_document_without_a_document_reader_downloads_nothing() {
    let graph = FakeGraph::new();
    graph.with_site();
    let api = client(&graph);
    let sentence = refused(
        &api,
        None,
        "read_document",
        json!({"path":"/drives/drive-docs/root:/a.txt"}),
    )
    .await;
    assert!(sentence.contains("lends no document reader"), "{sentence}");
    assert!(graph.requests().is_empty());
}

#[tokio::test]
async fn read_document_names_each_reader_failure_once() {
    let mut sentences = Vec::new();
    for (failure, fragment) in [
        (
            TextExtractionFailure::NoExtractor,
            "lends no document reader",
        ),
        (
            TextExtractionFailure::Unsupported,
            "file type is not supported",
        ),
        (
            TextExtractionFailure::Unreadable,
            "encrypted, damaged, or holds no text",
        ),
        (
            TextExtractionFailure::TooLarge,
            "over a limit of the document reader",
        ),
        (
            TextExtractionFailure::TimedOut,
            "took longer than the document reader allows",
        ),
        (
            TextExtractionFailure::Unreachable,
            "document reader could not be reached",
        ),
    ] {
        let graph = FakeGraph::new();
        graph.with_site();
        with_download(
            &graph,
            "graph.microsoft.com/v1.0/drives/drive-docs/root:/a.pdf:/content",
            b"%PDF",
        );
        let api = client(&graph);
        let host = FakeHost::new();
        *host.extraction_failure.lock().expect("failure") = Some(failure);
        let shared = host.shared();
        let sentence = refused(
            &api,
            Some(&shared),
            "read_document",
            json!({"path":"/drives/drive-docs/root:/a.pdf"}),
        )
        .await;
        assert!(sentence.contains(fragment), "{sentence}");
        sentences.push(sentence);
    }
    assert_one_sentence_per_cause(&sentences);
}

#[tokio::test]
async fn read_document_slices_lines_bounds_the_text_and_says_what_it_did_not_apply() {
    let body = "a\nb\nc\nd\n";
    let key = "graph.microsoft.com/v1.0/drives/drive-docs/root:/t.txt:/content";
    let graph = FakeGraph::new();
    graph.with_site();
    with_download(&graph, key, body.as_bytes());
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let path = json!("/drives/drive-docs/root:/t.txt");

    // Integer coercion is the shared exact one: 2.0, "3" and 1e0 are integers.
    let slice = ok(
        &api,
        Some(&host),
        "read_document",
        json!({"path":path,"start_line":2.0,"end_line":"3"}),
    )
    .await;
    assert_eq!(slice, "b\nc\n");
    let slice = ok(
        &api,
        Some(&host),
        "read_document",
        json!({"path":path,"start_line":1e0,"end_line":1}),
    )
    .await;
    assert_eq!(slice, "a\n");

    for (arguments, fragment) in [
        (json!({"path":path,"start_line":9}), "line range is empty"),
        (
            json!({"path":path,"start_line":3,"end_line":2}),
            "line range is inverted",
        ),
    ] {
        let sentence = refused(&api, Some(&host), "read_document", arguments.clone()).await;
        assert!(sentence.contains(fragment), "{arguments}: {sentence}");
    }
    // A line that is not a whole number of at least 1 is an invalid argument.
    for line in [
        json!(0),
        json!(2.5),
        json!("two"),
        json!(true),
        json!(-1),
        json!(1e30),
    ] {
        let error = err(
            &api,
            Some(&host),
            "read_document",
            json!({"path":path,"start_line":line}),
        )
        .await;
        assert_invalid_arguments(&error);
    }

    // Options this runtime reads past are named before the text.
    let text = ok(
        &api,
        Some(&host),
        "read_document",
        json!({"path":path,"page_number":2,"sheet_name":"Sheet1","is_capture_image":true}),
    )
    .await;
    assert_eq!(
        text,
        "[page_number, sheet_name, is_capture_image not applied: this runtime returns the document's text as a whole]\na\nb\nc\nd\n"
    );
}

#[tokio::test]
async fn read_document_over_the_read_limit_answers_the_content_too_large_guide() {
    let key = "graph.microsoft.com/v1.0/drives/drive-docs/root:/big.txt:/content";
    let graph = FakeGraph::new();
    graph.with_site();
    let body = "x\n".repeat(150_000);
    with_download(&graph, key, body.as_bytes());
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let guide = ok(
        &api,
        Some(&host),
        "read_document",
        json!({"path":"/drives/drive-docs/root:/big.txt"}),
    )
    .await;
    assert_eq!(guide["__result_status__"], "content_too_large");
    assert_eq!(guide["total_lines"], 150_000);
    assert_eq!(guide["read_limits"]["max_output_chars"], 200_000);
    // A slice inside the limit reads.
    let slice = ok(
        &api,
        Some(&host),
        "read_document",
        json!({"path":"/drives/drive-docs/root:/big.txt","start_line":1,"end_line":10}),
    )
    .await;
    assert_eq!(slice.as_str().map(str::len), Some(20));
}

#[tokio::test]
async fn read_document_failures_name_their_cause() {
    for (status, code) in [
        (404, MISSING),
        (403, FORBIDDEN),
        (429, RATE_LIMITED),
        (503, UNAVAILABLE),
    ] {
        let graph = FakeGraph::new();
        graph.with_site();
        graph.on(
            Method::GET,
            "graph.microsoft.com/v1.0/drives/drive-docs/root:/a.txt:/content",
            vec![Reply::status(status)],
        );
        let api = client(&graph);
        let host = FakeHost::new().shared();
        let sentence = refused(
            &api,
            Some(&host),
            "read_document",
            json!({"path":"/drives/drive-docs/root:/a.txt"}),
        )
        .await;
        assert!(sentence.contains(code), "{status}: {sentence}");
    }
    // A rejected token asks for authorization.
    let graph = FakeGraph::new();
    graph.with_site();
    graph.on(
        Method::GET,
        "graph.microsoft.com/v1.0/drives/drive-docs/root:/a.txt:/content",
        vec![Reply::status(401)],
    );
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let error = err(
        &api,
        Some(&host),
        "read_document",
        json!({"path":"/drives/drive-docs/root:/a.txt"}),
    )
    .await;
    assert!(delegated_authorization_requirement(&error).is_some());
}

#[tokio::test]
async fn a_redirect_off_sharepoint_is_not_followed_and_the_cap_is_enforced() {
    for target in [
        "https://evil.example/steal",
        "http://tenant.sharepoint.com/download",
        "https://tenant.sharepoint.com.evil.example/d",
        "https://tenant.sharepoint.com:8443/d",
        "https://u:p@tenant.sharepoint.com/d",
    ] {
        let graph = FakeGraph::new();
        graph.with_site();
        graph.on(
            Method::GET,
            "graph.microsoft.com/v1.0/drives/drive-docs/root:/a.txt:/content",
            vec![Reply::redirect(target)],
        );
        let api = client(&graph);
        let host = FakeHost::new().shared();
        let sentence = refused(
            &api,
            Some(&host),
            "read_document",
            json!({"path":"/drives/drive-docs/root:/a.txt"}),
        )
        .await;
        assert!(sentence.contains(BAD_ANSWER), "{target}: {sentence}");
        assert!(
            graph
                .requests()
                .iter()
                .all(|(_, key)| key.starts_with("graph.microsoft.com")),
            "{target}: only Graph was asked"
        );
    }
    // A redirect with no location is an invalid answer, not a follow.
    let graph = FakeGraph::new();
    graph.with_site();
    graph.on(
        Method::GET,
        "graph.microsoft.com/v1.0/drives/drive-docs/root:/a.txt:/content",
        vec![Reply::status(302)],
    );
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let sentence = refused(
        &api,
        Some(&host),
        "read_document",
        json!({"path":"/drives/drive-docs/root:/a.txt"}),
    )
    .await;
    assert!(sentence.contains(BAD_ANSWER), "{sentence}");

    // The cap holds on the bytes that come back.
    let graph = FakeGraph::new();
    graph.with_site();
    with_download(
        &graph,
        "graph.microsoft.com/v1.0/drives/drive-docs/root:/a.txt:/content",
        &vec![b'x'; 20 * 1_024 * 1_024 + 1],
    );
    let api = client(&graph);
    let sentence = refused(
        &api,
        Some(&host),
        "read_document",
        json!({"path":"/drives/drive-docs/root:/a.txt"}),
    )
    .await;
    assert!(sentence.contains(TOO_BIG), "{sentence}");
}

// ------------------------------------------------------- read_file_from_sharing_link

const SHARE_URL: &str = "https://tenant-my.sharepoint.com/:x:/p/user/AbC?e=1";

fn share_key(tail: &str) -> String {
    let id = format!("u!{}", URL_SAFE_NO_PAD.encode(SHARE_URL));
    format!("graph.microsoft.com/v1.0/shares/{id}/{tail}")
}

fn share_item(name: &str, size: u64, download: Option<&str>) -> Reply {
    let mut item = json!({"name":name,"size":size,"file":{}});
    if let Some(download) = download {
        item["@microsoft.graph.downloadUrl"] = json!(download);
    }
    Reply::json(200, &item)
}

#[tokio::test]
async fn a_sharing_link_is_resolved_by_graph_and_never_fetched_directly() {
    let graph = FakeGraph::new();
    graph
        .on(
            Method::GET,
            &share_key("driveItem"),
            vec![share_item(
                "Plan.txt",
                12,
                Some("https://tenant-my.sharepoint.com/personal/u/_layouts/15/download.aspx?x=1"),
            )],
        )
        .on(
            Method::GET,
            "tenant-my.sharepoint.com/personal/u/_layouts/15/download.aspx",
            vec![Reply::bytes(b"plan\ntext\n")],
        );
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let text = ok(
        &api,
        Some(&host),
        "read_file_from_sharing_link",
        json!({"sharing_url":SHARE_URL,"start_line":2}),
    )
    .await;
    assert_eq!(text, "text\n");
    let item = graph.only(&Method::GET, &share_key("driveItem"));
    assert_eq!(item.headers["prefer"], "redeemSharingLinkIfNecessary");
    assert!(item.headers[AUTHORIZATION].is_sensitive());
    assert!(
        graph.requests().iter().all(|(_, key)| !key.contains("AbC")),
        "the link itself is not requested"
    );
}

#[tokio::test]
async fn a_sharing_link_without_a_download_url_reads_the_shares_content_address() {
    let graph = FakeGraph::new();
    graph
        .on(
            Method::GET,
            &share_key("driveItem"),
            vec![share_item("Plan.md", 3, None)],
        )
        .on(
            Method::GET,
            &share_key("driveItem/content"),
            vec![Reply::redirect(DOWNLOAD_URL)],
        )
        .on(Method::GET, DOWNLOAD, vec![Reply::bytes(b"abc")]);
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let text = ok(
        &api,
        Some(&host),
        "read_file_from_sharing_link",
        json!({"sharing_url":SHARE_URL}),
    )
    .await;
    assert_eq!(text, "abc");
    assert!(
        graph
            .only(&Method::GET, &share_key("driveItem/content"))
            .headers[AUTHORIZATION]
            .is_sensitive()
    );
}

#[tokio::test]
async fn a_sharing_link_that_graph_refuses_is_not_retried_off_graph() {
    let graph = FakeGraph::new();
    graph.on(
        Method::GET,
        &share_key("driveItem"),
        vec![Reply::status(403)],
    );
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let sentence = refused(
        &api,
        Some(&host),
        "read_file_from_sharing_link",
        json!({"sharing_url":SHARE_URL}),
    )
    .await;
    assert!(sentence.contains(FORBIDDEN), "{sentence}");
    assert_eq!(graph.requests().len(), 1, "no direct fetch of the link");
}

#[tokio::test]
async fn a_sharing_link_is_checked_before_a_byte_is_downloaded() {
    let cases: Vec<(&str, u64, &str)> = vec![
        ("Big.docx", 20 * 1_024 * 1_024 + 1, "larger than the 20 MB"),
        ("photo.png", 3 * 1_024 * 1_024 + 1, "larger than the 20 MB"),
        ("README", 10, "no extension"),
        ("evil.zip.pdf", 10, "double extension"),
        ("bundle.zip", 10, "not supported"),
        ("movie.mp4", 10, "not supported"),
    ];
    for (name, size, fragment) in cases {
        let graph = FakeGraph::new();
        graph.on(
            Method::GET,
            &share_key("driveItem"),
            vec![share_item(name, size, Some(DOWNLOAD_URL))],
        );
        let api = client(&graph);
        let host = FakeHost::new().shared();
        let sentence = refused(
            &api,
            Some(&host),
            "read_file_from_sharing_link",
            json!({"sharing_url":SHARE_URL}),
        )
        .await;
        assert!(sentence.contains(fragment), "{name}: {sentence}");
        assert_eq!(graph.requests().len(), 1, "{name}: nothing was downloaded");
    }
    // A folder is not a file.
    let graph = FakeGraph::new();
    graph.on(
        Method::GET,
        &share_key("driveItem"),
        vec![Reply::json(200, &json!({"name":"Folder","folder":{}}))],
    );
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let sentence = refused(
        &api,
        Some(&host),
        "read_file_from_sharing_link",
        json!({"sharing_url":SHARE_URL}),
    )
    .await;
    assert!(sentence.contains("does not point to a file"), "{sentence}");
}

#[tokio::test]
async fn a_sharing_link_must_be_https_and_its_download_url_must_stay_on_sharepoint() {
    let graph = FakeGraph::new();
    let api = client(&graph);
    let host = FakeHost::new().shared();
    for url in [
        "http://tenant.sharepoint.com/:x:/s/a",
        "ftp://x/y",
        "not a url",
        "https://",
    ] {
        let sentence = refused(
            &api,
            Some(&host),
            "read_file_from_sharing_link",
            json!({"sharing_url":url}),
        )
        .await;
        assert!(sentence.contains(INVALID), "{url}: {sentence}");
    }
    assert!(graph.requests().is_empty());

    let graph = FakeGraph::new();
    graph.on(
        Method::GET,
        &share_key("driveItem"),
        vec![share_item("Plan.txt", 3, Some("https://evil.example/d"))],
    );
    let api = client(&graph);
    let sentence = refused(
        &api,
        Some(&host),
        "read_file_from_sharing_link",
        json!({"sharing_url":SHARE_URL}),
    )
    .await;
    assert!(sentence.contains(BAD_ANSWER), "{sentence}");
    assert!(!graph.touched("evil.example"));
}

// -------------------------------------------------------------------- OneNote

const ONENOTE: &str = "graph.microsoft.com/v1.0/sites/tenant.sharepoint.com,site-id,web-id/onenote";

fn onenote_key(tail: &str) -> String {
    format!("{ONENOTE}{tail}")
}

#[tokio::test]
async fn onenote_creations_post_once_and_project_the_created_object() {
    let graph = FakeGraph::new();
    graph
        .with_site()
        .on(
            Method::POST,
            &onenote_key("/notebooks"),
            vec![Reply::json(
                201,
                &json!({"id":"nb-1","displayName":"Team","createdDateTime":"2026-01-01T00:00:00Z","links":{"oneNoteWebUrl":{"href":"https://tenant.sharepoint.com/nb"}},"extra":1}),
            )],
        )
        .on(
            Method::POST,
            &onenote_key("/notebooks/nb-1/sections"),
            vec![Reply::json(
                201,
                &json!({"id":"sec-1","displayName":"Notes","createdDateTime":"t","pagesUrl":"https://graph/pages"}),
            )],
        )
        .on(
            Method::POST,
            &onenote_key("/sections/sec-1/pages"),
            vec![Reply::json(
                201,
                &json!({"id":"pg-1","title":"My Page","createdDateTime":"t","webUrl":"https://w","contentUrl":"https://c"}),
            )],
        );
    let api = client(&graph);

    let notebook = ok(
        &api,
        None,
        "onenote_create_notebook",
        json!({"display_name":"  Team  "}),
    )
    .await;
    assert_eq!(
        notebook,
        json!({"id":"nb-1","displayName":"Team","createdDateTime":"2026-01-01T00:00:00Z","webUrl":"https://tenant.sharepoint.com/nb"})
    );
    let post = graph.only(&Method::POST, &onenote_key("/notebooks"));
    assert_eq!(
        serde_json::from_slice::<Value>(&post.body).expect("body"),
        json!({"displayName":"Team"}),
        "the name is trimmed as the SDK trims it"
    );

    let section = ok(
        &api,
        None,
        "onenote_create_section",
        json!({"notebook_id":"nb-1","display_name":"Notes"}),
    )
    .await;
    assert_eq!(section["pagesUrl"], "https://graph/pages");

    let html = "<!DOCTYPE html><html><head><title>My Page</title></head><body><p>Hi\n\tthere</p></body></html>";
    let page = ok(
        &api,
        None,
        "onenote_create_page",
        json!({"section_id":"sec-1","html_content":html}),
    )
    .await;
    assert_eq!(
        page,
        json!({"id":"pg-1","title":"My Page","createdDateTime":"t","webUrl":"https://w","contentUrl":"https://c"})
    );
    let post = graph.only(&Method::POST, &onenote_key("/sections/sec-1/pages"));
    assert_eq!(post.headers[CONTENT_TYPE], "text/html");
    assert_eq!(post.body, html.as_bytes());
}

#[tokio::test]
async fn onenote_page_edits_patch_and_delete_with_the_sdk_confirmations() {
    let graph = FakeGraph::new();
    graph
        .with_site()
        .on(
            Method::PATCH,
            &onenote_key("/pages/pg-1/content"),
            vec![Reply::status(204)],
        )
        .on(
            Method::DELETE,
            &onenote_key("/pages/pg-1"),
            vec![Reply::status(204)],
        );
    let api = client(&graph);

    let commands = json!([
        {"target":"body","action":"append","content":"<p>New</p>"},
        {"target":"#old","action":"delete"},
        {"target":"#x","action":"insert","content":"<p>y</p>","position":"after"}
    ]);
    let message = ok(
        &api,
        None,
        "onenote_update_page",
        json!({"page_id":"pg-1","patch_commands":commands}),
    )
    .await;
    assert_eq!(
        message,
        "OneNote page 'pg-1' updated successfully with 3 patch command(s)."
    );
    let patch = graph.only(&Method::PATCH, &onenote_key("/pages/pg-1/content"));
    assert_eq!(patch.headers[CONTENT_TYPE], "application/json");
    assert_eq!(
        serde_json::from_slice::<Value>(&patch.body).expect("body"),
        commands
    );

    let message = ok(
        &api,
        None,
        "onenote_replace_page_content",
        json!({"page_id":"pg-1","html_content":"<p>whole body</p>"}),
    )
    .await;
    assert_eq!(
        message,
        "OneNote page 'pg-1' updated successfully with 1 patch command(s)."
    );
    let patches = graph
        .seen
        .lock()
        .expect("seen")
        .iter()
        .filter(|seen| seen.method == Method::PATCH)
        .map(|seen| serde_json::from_slice::<Value>(&seen.body).expect("body"))
        .collect::<Vec<_>>();
    assert_eq!(
        patches[1],
        json!([{"target":"body","action":"replace","content":"<p>whole body</p>"}])
    );

    let message = ok(&api, None, "onenote_delete_page", json!({"page_id":"pg-1"})).await;
    assert_eq!(message, "OneNote page 'pg-1' deleted successfully.");
}

#[tokio::test]
async fn onenote_changes_reject_bad_arguments_before_any_request() {
    let graph = FakeGraph::new();
    let api = client(&graph);
    for (tool, arguments) in [
        ("onenote_create_notebook", json!({"display_name":"   "})),
        (
            "onenote_create_section",
            json!({"notebook_id":"nb","display_name":""}),
        ),
        (
            "onenote_create_page",
            json!({"section_id":"s","html_content":"   \n"}),
        ),
        (
            "onenote_create_page",
            json!({"section_id":"s","html_content":"<p>\u{0}</p>"}),
        ),
        (
            "onenote_update_page",
            json!({"page_id":"p","patch_commands":[]}),
        ),
        (
            "onenote_update_page",
            json!({"page_id":"p","patch_commands":["x"]}),
        ),
        (
            "onenote_update_page",
            json!({"page_id":"p","patch_commands":[{"target":"body","action":"explode"}]}),
        ),
        (
            "onenote_update_page",
            json!({"page_id":"p","patch_commands":[{"action":"append"}]}),
        ),
        (
            "onenote_update_page",
            json!({"page_id":"p","patch_commands":[{"target":"body","action":"insert","position":"middle"}]}),
        ),
        (
            "onenote_replace_page_content",
            json!({"page_id":"p","html_content":""}),
        ),
        ("onenote_delete_page", json!({"page_id":""})),
        ("onenote_delete_page", json!({"page_id":"p","force":true})),
    ] {
        let error = err(&api, None, tool, arguments.clone()).await;
        assert_invalid_arguments(&error);
    }
    // The page markup is bounded to what one argument can carry.
    let error = err(
        &api,
        None,
        "onenote_create_page",
        json!({"section_id":"s","html_content":"x".repeat(64 * 1_024 + 1)}),
    )
    .await;
    assert_eq!(error.category, ErrorCategory::InvalidInput);
    assert!(graph.requests().is_empty());
}

#[tokio::test]
async fn every_onenote_change_names_its_causes_and_is_never_retried() {
    let changes: Vec<(&str, Value, Method, String)> = vec![
        (
            "onenote_create_notebook",
            json!({"display_name":"N"}),
            Method::POST,
            onenote_key("/notebooks"),
        ),
        (
            "onenote_create_section",
            json!({"notebook_id":"nb","display_name":"S"}),
            Method::POST,
            onenote_key("/notebooks/nb/sections"),
        ),
        (
            "onenote_create_page",
            json!({"section_id":"sec","html_content":"<html><head><title>t</title></head></html>"}),
            Method::POST,
            onenote_key("/sections/sec/pages"),
        ),
        (
            "onenote_update_page",
            json!({"page_id":"pg","patch_commands":[{"target":"body","action":"append","content":"x"}]}),
            Method::PATCH,
            onenote_key("/pages/pg/content"),
        ),
        (
            "onenote_replace_page_content",
            json!({"page_id":"pg","html_content":"x"}),
            Method::PATCH,
            onenote_key("/pages/pg/content"),
        ),
        (
            "onenote_delete_page",
            json!({"page_id":"pg"}),
            Method::DELETE,
            onenote_key("/pages/pg"),
        ),
    ];
    let causes: Vec<(Reply, &str)> = vec![
        (Reply::status(400), INVALID),
        (Reply::status(403), FORBIDDEN),
        (Reply::status(404), MISSING),
        (Reply::status(409), CONFLICT),
        (Reply::status(429), RATE_LIMITED),
        (Reply::status(503), UNAVAILABLE),
        (Reply::status(500), UNKNOWN),
        (Reply::Fail(SharePointClientErrorCode::Timeout), UNKNOWN),
        (
            Reply::Fail(SharePointClientErrorCode::ConnectionLost),
            UNKNOWN,
        ),
        (
            Reply::Fail(SharePointClientErrorCode::DependencyUnavailable),
            UNAVAILABLE,
        ),
    ];
    for (tool, arguments, method, key) in &changes {
        for (reply, code) in &causes {
            let graph = FakeGraph::new();
            graph
                .with_site()
                .on(method.clone(), key, vec![reply.clone()]);
            let api = client(&graph);
            let sentence = refused(&api, None, tool, arguments.clone()).await;
            assert!(sentence.contains(*code), "{tool}: {sentence}");
            assert_eq!(
                sentence.contains(UNKNOWN),
                *code == UNKNOWN,
                "{tool}: {sentence}"
            );
            assert_eq!(graph.count(method, key), 1, "{tool}: sent once");
        }
        // A rejected token asks for authorization.
        let graph = FakeGraph::new();
        graph
            .with_site()
            .on(method.clone(), key, vec![Reply::status(401)]);
        let api = client(&graph);
        let error = err(&api, None, tool, arguments.clone()).await;
        assert!(
            delegated_authorization_requirement(&error).is_some(),
            "{tool}"
        );
    }
}

#[tokio::test]
async fn a_create_whose_answer_cannot_be_read_is_still_applied() {
    // A 2xx is Graph's word that it happened: whatever the body looks like,
    // the answer is a success that says what could not be shown.
    for (tool, arguments, key) in [
        (
            "onenote_create_notebook",
            json!({"display_name":"N"}),
            onenote_key("/notebooks"),
        ),
        (
            "onenote_create_section",
            json!({"notebook_id":"nb","display_name":"S"}),
            onenote_key("/notebooks/nb/sections"),
        ),
        (
            "onenote_create_page",
            json!({"section_id":"sec","html_content":"<p>x</p>"}),
            onenote_key("/sections/sec/pages"),
        ),
    ] {
        let graph = FakeGraph::new();
        graph.with_site().on(
            Method::POST,
            &key,
            vec![Reply::Http {
                status: 201,
                content_type: Some("application/json"),
                location: None,
                body: b"<<<".to_vec(),
            }],
        );
        let api = client(&graph);
        let answer = ok(&api, None, tool, arguments).await;
        assert_eq!(
            answer,
            json!({"applied": true, "note": "applied; the response could not be read"}),
            "{tool}"
        );
        assert_eq!(graph.count(&Method::POST, &key), 1, "{tool}");
    }
}

/// A confirmed change is never a failure, whatever its answer: oversize,
/// unreadable, or a projection over the output cap.
#[tokio::test]
#[allow(clippy::too_many_lines, reason = "one scenario per kind of change")]
async fn a_change_that_graph_confirmed_is_never_reported_as_a_failure() {
    // Oversize body: the status is read before the size.
    let huge = Reply::Http {
        status: 201,
        content_type: Some("application/json"),
        location: None,
        body: serde_json::to_vec(
            &json!({"id":"9","fields":{"Notes":"x".repeat(2 * 1_024 * 1_024 + 10)}}),
        )
        .expect("json"),
    };
    let graph = FakeGraph::new();
    graph.with_site().with_list().on(
        Method::POST,
        &format!("{SITE}/lists/list-1/items"),
        vec![huge],
    );
    let api = client(&graph);
    let answer = ok(
        &api,
        None,
        "create_list_item",
        json!({"list_title":"Tasks","fields":{"Title":"x"}}),
    )
    .await;
    assert_eq!(
        answer,
        json!({"applied": true, "note": "applied; the response was too large to show"})
    );

    // Within the transport cap, over the output cap: the id still comes back.
    let big = Reply::json(
        201,
        &json!({"id":"9","fields":{"Notes":"x".repeat(600 * 1_024)},"webUrl":"https://w"}),
    );
    let graph = FakeGraph::new();
    graph.with_site().with_list().on(
        Method::POST,
        &format!("{SITE}/lists/list-1/items"),
        vec![big],
    );
    let api = client(&graph);
    let answer = ok(
        &api,
        None,
        "create_list_item",
        json!({"list_title":"Tasks","fields":{"Title":"x"}}),
    )
    .await;
    assert_eq!(
        answer,
        json!({"applied": true, "note": "applied; the response was too large to show", "id": "9"})
    );

    // An upload, small and chunked, and the OneNote edits.
    let graph = FakeGraph::new();
    graph.with_site().on(
        Method::PUT,
        UPLOAD_ITEM,
        vec![Reply::Http {
            status: 201,
            content_type: Some("application/json"),
            location: None,
            body: vec![b' '; 2 * 1_024 * 1_024 + 10],
        }],
    );
    let api = client(&graph);
    let answer = ok(
        &api,
        None,
        "upload_file",
        json!({"folder_path":"/teams/project/Shared Documents/Reports","filedata":"hello","filename":"a b.txt"}),
    )
    .await;
    assert_eq!(answer["applied"], true);

    let graph = FakeGraph::new();
    graph
        .with_site()
        .on(Method::POST, SESSION, vec![session_reply()])
        .on(
            Method::PUT,
            UPLOAD_PATH,
            vec![
                Reply::json(202, &json!({"nextExpectedRanges":["5242880-"]})),
                Reply::Http {
                    status: 201,
                    content_type: Some("application/json"),
                    location: None,
                    body: vec![b' '; 2 * 1_024 * 1_024 + 10],
                },
            ],
        );
    let api = client(&graph);
    let host = big_host();
    let answer = ok(
        &api,
        Some(&host),
        "upload_file",
        json!({"folder_path":"/teams/project/Shared Documents/Reports","filepath":"/bucket/big.bin"}),
    )
    .await;
    assert_eq!(answer["applied"], true);

    for (tool, arguments, method, key) in [
        (
            "onenote_update_page",
            json!({"page_id":"pg","patch_commands":[{"target":"body","action":"append","content":"x"}]}),
            Method::PATCH,
            onenote_key("/pages/pg/content"),
        ),
        (
            "onenote_delete_page",
            json!({"page_id":"pg"}),
            Method::DELETE,
            onenote_key("/pages/pg"),
        ),
    ] {
        let graph = FakeGraph::new();
        graph.with_site().on(
            method.clone(),
            &key,
            vec![Reply::Http {
                status: 200,
                content_type: Some("application/json"),
                location: None,
                body: vec![b' '; 2 * 1_024 * 1_024 + 10],
            }],
        );
        let api = client(&graph);
        let answer = ok(&api, None, tool, arguments).await;
        assert!(
            answer
                .as_str()
                .is_some_and(|text| text.contains("successfully")),
            "{tool}: {answer}"
        );
    }
}

const PAGE_HTML: &str = r#"<!DOCTYPE html><html lang="en-US"><head><title>Plan</title></head>
<body><div><p>Hello &amp; welcome</p>
<img src="https://graph.microsoft.com/v1.0/sites/tenant.sharepoint.com,site-id,web-id/onenote/resources/img-1/$value" alt="a chart" />
<p><object data-attachment="plan.pdf" data="https://graph.microsoft.com/v1.0/sites/tenant.sharepoint.com,site-id,web-id/onenote/resources/res-1/$value" type="application/pdf"></object></p>
<p>Bye</p></div></body></html>"#;

const CANONICAL_RES1: &str = "https://graph.microsoft.com/v1.0/sites/tenant.sharepoint.com,site-id,web-id/onenote/resources/res-1/content";

fn page_graph() -> Arc<FakeGraph> {
    let graph = FakeGraph::new();
    graph
        .with_site()
        .on(
            Method::GET,
            &onenote_key("/pages/pg-1/content"),
            vec![Reply::html(PAGE_HTML)],
        )
        .on(
            Method::GET,
            &onenote_key("/resources/res-1/content"),
            vec![Reply::bytes(b"PLAN TEXT\nsecond line")],
        );
    graph
}

#[tokio::test]
async fn onenote_list_attachments_gives_name_resource_id_and_canonical_address() {
    let graph = page_graph();
    let api = client(&graph);
    let listed = ok(
        &api,
        None,
        "onenote_list_attachments",
        json!({"page_id":"pg-1"}),
    )
    .await;
    assert_eq!(
        listed,
        json!([{"name":"plan.pdf","resource_id":"res-1","download_url":CANONICAL_RES1}])
    );
    // Listing does not download the attachment.
    assert_eq!(
        graph.count(&Method::GET, &onenote_key("/resources/res-1/content")),
        0
    );
}

#[tokio::test]
async fn onenote_read_page_renders_text_images_and_attachments_split_by_dashes() {
    let graph = page_graph();
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let text = ok(
        &api,
        Some(&host),
        "onenote_read_page",
        json!({"page_id":"pg-1"}),
    )
    .await;
    assert_eq!(
        text,
        format!(
            "Hello & welcome\n-----\n[image: a chart]\n-----\n[attachment: plan.pdf]\n  {CANONICAL_RES1}\n-----\nBye"
        )
    );
    assert_eq!(
        graph.count(&Method::GET, &onenote_key("/resources/res-1/content")),
        0,
        "no attachment is downloaded unless its content is asked for"
    );

    let text = ok(
        &api,
        Some(&host),
        "onenote_read_page",
        json!({"page_id":"pg-1","read_attachment_content":true,"capture_images":false}),
    )
    .await;
    assert_eq!(
        text,
        format!(
            "Hello & welcome\n-----\n[image: a chart]\n-----\n[attachment: plan.pdf]\n  {CANONICAL_RES1}\n  Content:\n  PLAN TEXT\n  second line\n-----\nBye"
        )
    );
}

#[tokio::test]
async fn onenote_read_page_items_prints_each_item_as_the_sdk_does() {
    let graph = page_graph();
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let text = ok(
        &api,
        Some(&host),
        "onenote_read_page_items",
        json!({"page_id":"pg-1","read_attachment_content":"true"}),
    )
    .await;
    assert_eq!(
        text,
        format!(
            "Hello & welcome\n-----\n[image: a chart]\n-----\n[attachment: plan.pdf]\n  download_url: {CANONICAL_RES1}\n  content:\n  PLAN TEXT\n  second line\n-----\nBye"
        )
    );
}

#[tokio::test]
async fn onenote_read_page_says_when_it_cannot_read_attachment_content() {
    let graph = page_graph();
    let api = client(&graph);
    let text = ok(
        &api,
        None,
        "onenote_read_page",
        json!({"page_id":"pg-1","read_attachment_content":true}),
    )
    .await;
    assert!(text.as_str().expect("text").starts_with(
        "[read_attachment_content not applied: this runtime lends no document reader]\n"
    ));
    assert_eq!(
        graph.count(&Method::GET, &onenote_key("/resources/res-1/content")),
        0
    );

    // A reader failure leaves that attachment without content, as the SDK does.
    let host = FakeHost::new();
    *host.extraction_failure.lock().expect("failure") = Some(TextExtractionFailure::Unreadable);
    let shared = host.shared();
    let text = ok(
        &api,
        Some(&shared),
        "onenote_read_page",
        json!({"page_id":"pg-1","read_attachment_content":true}),
    )
    .await;
    assert!(
        text.as_str()
            .expect("text")
            .contains("Content: not available: the attachment could not be read as text"),
        "{text}"
    );
}

#[tokio::test]
async fn onenote_read_page_never_fetches_an_address_that_is_not_a_site_resource() {
    let html =
        r#"<p><object data-attachment="x.pdf" data="https://evil.example/x.pdf"></object></p>"#;
    let graph = FakeGraph::new();
    graph.with_site().on(
        Method::GET,
        &onenote_key("/pages/pg-1/content"),
        vec![Reply::html(html)],
    );
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let text = ok(
        &api,
        Some(&host),
        "onenote_read_page",
        json!({"page_id":"pg-1","read_attachment_content":true}),
    )
    .await;
    assert_eq!(
        text,
        "[attachment: x.pdf]\n  https://evil.example/x.pdf\n  Content: not available: the attachment is not a OneNote resource of this site"
    );
    assert!(!graph.touched("evil.example"));

    let sentence = refused(
        &api,
        Some(&host),
        "onenote_read_attachment",
        json!({"page_id":"pg-1","attachment_name":"x.pdf"}),
    )
    .await;
    assert!(
        sentence.contains("no resolvable download address"),
        "{sentence}"
    );
    assert!(!graph.touched("evil.example"));
}

#[tokio::test]
async fn onenote_read_attachment_downloads_by_resource_id_and_parses_it() {
    let graph = page_graph();
    let api = client(&graph);
    let host = FakeHost::new();
    let shared = host.shared();
    let text = ok(
        &api,
        Some(&shared),
        "onenote_read_attachment",
        json!({"page_id":"pg-1","attachment_name":"plan.pdf","capture_images":false}),
    )
    .await;
    assert_eq!(text, "PLAN TEXT\nsecond line");
    assert_eq!(
        host.extracted.lock().expect("extracted").as_slice(),
        [("plan.pdf".to_owned(), 21)]
    );

    // The name is case-sensitive, and the sentence lists what exists.
    let sentence = refused(
        &api,
        Some(&shared),
        "onenote_read_attachment",
        json!({"page_id":"pg-1","attachment_name":"PLAN.pdf"}),
    )
    .await;
    assert_eq!(
        sentence,
        "Could not read the attachment: Attachment 'PLAN.pdf' not found on page 'pg-1'. Available attachments: ['plan.pdf']."
    );
    // No reader: nothing is downloaded.
    let before = graph.count(&Method::GET, &onenote_key("/resources/res-1/content"));
    let sentence = refused(
        &api,
        None,
        "onenote_read_attachment",
        json!({"page_id":"pg-1","attachment_name":"plan.pdf"}),
    )
    .await;
    assert!(sentence.contains("lends no document reader"), "{sentence}");
    assert_eq!(
        graph.count(&Method::GET, &onenote_key("/resources/res-1/content")),
        before
    );
}

#[tokio::test]
async fn onenote_reads_map_graph_failures_and_ask_for_authorization_on_401() {
    for (status, code) in [
        (404, MISSING),
        (403, FORBIDDEN),
        (429, RATE_LIMITED),
        (500, UNAVAILABLE),
    ] {
        let graph = FakeGraph::new();
        graph.with_site().on(
            Method::GET,
            &onenote_key("/pages/pg-1/content"),
            vec![Reply::status(status)],
        );
        let api = client(&graph);
        for tool in [
            "onenote_list_attachments",
            "onenote_read_page",
            "onenote_read_page_items",
        ] {
            let host = FakeHost::new().shared();
            let sentence = refused(&api, Some(&host), tool, json!({"page_id":"pg-1"})).await;
            assert!(sentence.contains(code), "{tool} {status}: {sentence}");
        }
    }
    let graph = FakeGraph::new();
    graph.with_site().on(
        Method::GET,
        &onenote_key("/pages/pg-1/content"),
        vec![Reply::status(401)],
    );
    let api = client(&graph);
    let error = err(&api, None, "onenote_read_page", json!({"page_id":"pg-1"})).await;
    assert!(delegated_authorization_requirement(&error).is_some());
}

#[tokio::test]
async fn a_page_over_the_size_limit_is_refused_and_over_the_text_limit_is_cut() {
    // The raw page is over the 512 KiB the family returns: refused at the source.
    let graph = FakeGraph::new();
    graph.with_site().on(
        Method::GET,
        &onenote_key("/pages/pg-1/content"),
        vec![Reply::html(&format!("<p>{}</p>", "word ".repeat(110_000)))],
    );
    let api = client(&graph);
    let sentence = refused(&api, None, "onenote_read_page", json!({"page_id":"pg-1"})).await;
    assert!(sentence.contains(TOO_BIG), "{sentence}");

    // A page within it whose text is over the 200000-character read limit.
    let graph = FakeGraph::new();
    graph.with_site().on(
        Method::GET,
        &onenote_key("/pages/pg-1/content"),
        vec![Reply::html(&format!("<p>{}</p>", "word ".repeat(50_000)))],
    );
    let api = client(&graph);
    // The page's own text is returned, cut at the limit and saying so.
    let text = ok(&api, None, "onenote_read_page", json!({"page_id":"pg-1"})).await;
    let text = text.as_str().expect("text");
    assert!(text.starts_with("word word"));
    assert!(
        text.ends_with("[page text cut at 200000 characters]"),
        "{}",
        &text[text.len() - 60..]
    );
}

// ------------------------------------------------------------ tool shapes

#[tokio::test]
async fn only_the_changes_are_not_read_only_and_none_of_them_runs_in_parallel() {
    let graph = FakeGraph::new();
    let api = client(&graph);
    let names = super::sdk_conformance::sdk_tool_names("sharepoint");
    let toolset = test_build_with_api(
        "Project SharePoint",
        SITE_URL,
        &names,
        &policy(&[]),
        &api,
        None,
    )
    .expect("toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("tools");
    let effects = [
        "create_list_item",
        "upload_file",
        "onenote_create_notebook",
        "onenote_create_section",
        "onenote_create_page",
        "onenote_update_page",
        "onenote_replace_page_content",
        "onenote_delete_page",
    ];
    assert_eq!(tools.len(), 22);
    for tool in &tools {
        let effect = effects.contains(&tool.name());
        assert_eq!(tool.is_read_only(), !effect, "{}", tool.name());
        assert_eq!(tool.is_concurrency_safe(), !effect, "{}", tool.name());
        assert!(
            tool.description()
                .ends_with("\nToolkit: Project SharePoint"),
            "{} was cut: {}",
            tool.name(),
            tool.description().len()
        );
    }
}

#[tokio::test]
async fn a_missing_token_keeps_every_new_tool_behind_the_authorization_guard() {
    let config = SharePointToolkitConfig::parse(
        "Project SharePoint",
        &settings(&[
            "read_document",
            "upload_file",
            "create_list_item",
            "onenote_read_page",
            "onenote_delete_page",
        ]),
        &serde_json::Map::new(),
    )
    .expect("guarded configuration");
    let materialized = super::families::sharepoint::tools::build_sharepoint_toolset(
        "Project SharePoint",
        config,
        &policy(&[]),
        None,
    )
    .expect("guarded tools");
    assert_eq!(materialized.delegated_authorization.tool_names().count(), 5);
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = materialized.toolset.tools(readonly).await.expect("tools");
    for tool in tools {
        let error = tool
            .execute(context(), json!({}))
            .await
            .expect_err("authorization required");
        assert!(
            delegated_authorization_requirement(&error).is_some(),
            "{}",
            tool.name()
        );
        assert_eq!(
            tool.is_read_only(),
            !matches!(
                tool.name(),
                "upload_file" | "create_list_item" | "onenote_delete_page"
            )
        );
    }
}

// -------------------------------------------- the real transport, on a socket

/// Serves one scripted answer per connection on a local socket.
async fn serve(script: Vec<Vec<u8>>, hold_after: bool) -> u16 {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let port = listener.local_addr().expect("addr").port();
    tokio::spawn(async move {
        let Ok((mut stream, _)) = listener.accept().await else {
            return;
        };
        let mut buffer = [0_u8; 4096];
        let _ = stream.read(&mut buffer).await;
        for part in script {
            if stream.write_all(&part).await.is_err() {
                return;
            }
            let _ = stream.flush().await;
        }
        if hold_after {
            tokio::time::sleep(std::time::Duration::from_secs(30)).await;
        }
    });
    port
}

fn request(port: u16) -> Request {
    Request::new(
        Method::GET,
        Url::parse(&format!("http://127.0.0.1:{port}/file")).expect("url"),
    )
}

#[tokio::test]
async fn the_real_transport_refuses_a_declared_oversize_body_before_reading_it() {
    let port = serve(
        vec![b"HTTP/1.1 200 OK\r\ncontent-length: 104857600\r\n\r\n".to_vec()],
        true,
    )
    .await;
    let transport = plain_http_transport();
    let started = std::time::Instant::now();
    let answer = transport
        .execute_bounded(request(port), 1_024)
        .await
        .expect("the status is read before the size");
    assert!(answer.oversize && answer.body.is_empty());
    assert_eq!(answer.status, StatusCode::OK);
    assert!(
        started.elapsed() < std::time::Duration::from_secs(5),
        "no body was awaited"
    );
}

#[tokio::test]
async fn the_real_transport_stops_a_body_that_outgrows_its_cap_without_a_length() {
    let mut script = vec![b"HTTP/1.1 200 OK\r\ntransfer-encoding: chunked\r\n\r\n".to_vec()];
    for _ in 0..8 {
        script.push(format!("{:x}\r\n{}\r\n", 512, "x".repeat(512)).into_bytes());
    }
    script.push(b"0\r\n\r\n".to_vec());
    let port = serve(script, false).await;
    let transport = plain_http_transport();
    let answer = transport
        .execute_bounded(request(port), 2_048)
        .await
        .expect("the status is read before the size");
    assert!(answer.oversize && answer.body.is_empty());
}

#[tokio::test]
async fn the_real_transport_tells_a_failed_connection_from_a_lost_answer_from_a_timeout() {
    // Nothing listens: the request never left.
    let port = {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind");
        listener.local_addr().expect("addr").port()
    };
    let transport = plain_http_transport();
    let error = transport
        .execute(request(port))
        .await
        .err()
        .expect("refused");
    assert_eq!(
        error.code(),
        SharePointClientErrorCode::DependencyUnavailable
    );
    assert_eq!(
        error.for_effect().code(),
        SharePointClientErrorCode::DependencyUnavailable
    );

    // The server answers part of a body and closes: the answer was lost.
    let port = serve(
        vec![b"HTTP/1.1 200 OK\r\ncontent-length: 10\r\n\r\nabc".to_vec()],
        false,
    )
    .await;
    let error = transport
        .execute(request(port))
        .await
        .err()
        .expect("cut short");
    assert_eq!(error.code(), SharePointClientErrorCode::ConnectionLost);
    assert_eq!(
        error.for_effect().code(),
        SharePointClientErrorCode::OutcomeUnknown
    );

    // The server never answers: the request timed out.
    let port = serve(Vec::new(), true).await;
    let mut slow = request(port);
    *slow.timeout_mut() = Some(std::time::Duration::from_millis(300));
    let error = transport.execute(slow).await.err().expect("timed out");
    assert_eq!(error.code(), SharePointClientErrorCode::Timeout);
    assert_eq!(
        error.for_effect().code(),
        SharePointClientErrorCode::OutcomeUnknown
    );
}

#[tokio::test]
async fn integer_arguments_use_the_exact_reading_for_every_limit() {
    // `limit` of read_list and `limit_files` use the same exact reader as the
    // line numbers: 2.0 is two, 2.5 is not an integer, a huge value is refused.
    let graph = FakeGraph::new();
    graph.with_site().with_list().on(
        Method::GET,
        &format!("{SITE}/lists/list-1/items"),
        vec![Reply::json(
            200,
            &json!({"value":[{"fields":{"a":1}},{"fields":{"a":2}},{"fields":{"a":3}}]}),
        )],
    );
    let api = client(&graph);
    let rows = ok(
        &api,
        None,
        "read_list",
        json!({"list_title":"Tasks","limit":2.0}),
    )
    .await;
    assert_eq!(rows.as_array().map(Vec::len), Some(2));
    for limit in [
        json!(2.5),
        json!(0),
        json!(-1),
        json!(1001),
        json!("many"),
        json!(1e30),
    ] {
        let error = err(
            &api,
            None,
            "read_list",
            json!({"list_title":"Tasks","limit":limit}),
        )
        .await;
        assert_invalid_arguments(&error);
    }
}

// ------------------------------------------------ review round 1: regressions

/// Only a Graph `OneNote` resource of THIS site is fetched with the person's
/// token; a picture on a CDN that happens to have `/resources/<id>` in its
/// path, and another site's resource, are listed as written and never fetched.
#[tokio::test]
async fn a_cdn_or_foreign_address_with_a_resources_path_is_not_a_onenote_resource() {
    let html = r#"<p>
<object data-attachment="cdn.pdf" data="https://cdn.example.com/files/resources/res-1/$value"></object>
<object data-attachment="other.pdf" data="https://graph.microsoft.com/v1.0/sites/other,site/onenote/resources/res-2/$value"></object>
<object data-attachment="mine.pdf" data="https://graph.microsoft.com/v1.0/sites/tenant.sharepoint.com,site-id,web-id/onenote/resources/res-3/$value"></object></p>"#;
    let graph = FakeGraph::new();
    graph
        .with_site()
        .on(
            Method::GET,
            &onenote_key("/pages/pg-1/content"),
            vec![Reply::html(html)],
        )
        .on(
            Method::GET,
            &onenote_key("/resources/res-3/content"),
            vec![Reply::bytes(b"mine")],
        );
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let listed = ok(
        &api,
        Some(&host),
        "onenote_list_attachments",
        json!({"page_id":"pg-1"}),
    )
    .await;
    assert_eq!(listed[0]["resource_id"], Value::Null);
    assert_eq!(
        listed[0]["download_url"], "https://cdn.example.com/files/resources/res-1/$value",
        "an address that is not a resource of this site is shown as written"
    );
    assert_eq!(listed[1]["resource_id"], Value::Null);
    assert_eq!(listed[2]["resource_id"], "res-3");

    let text = ok(
        &api,
        Some(&host),
        "onenote_read_page",
        json!({"page_id":"pg-1","read_attachment_content":true}),
    )
    .await;
    let text = text.as_str().expect("text");
    assert!(text.contains("[attachment: cdn.pdf]\n  https://cdn.example.com/files/resources/res-1/$value\n  Content: not available: the attachment is not a OneNote resource of this site"), "{text}");
    assert!(text.contains("Content:\n  mine"), "{text}");
    assert!(!graph.touched("cdn.example.com"));
    assert_eq!(
        graph.count(&Method::GET, &onenote_key("/resources/res-1/content")),
        0
    );
    assert_eq!(
        graph.count(&Method::GET, &onenote_key("/resources/res-2/content")),
        0
    );

    let sentence = refused(
        &api,
        Some(&host),
        "onenote_read_attachment",
        json!({"page_id":"pg-1","attachment_name":"cdn.pdf"}),
    )
    .await;
    assert!(
        sentence.contains("no resolvable download address"),
        "{sentence}"
    );
}

/// A folder that is in no library and exists nowhere is not found: nothing is
/// created and nothing falls back to the default library.
#[tokio::test]
async fn upload_into_a_folder_that_exists_nowhere_is_not_found_and_creates_nothing() {
    for folder in [
        "Nowhere/Reports",
        "/teams/project",
        "/teams/project/Unknown Library/x",
    ] {
        let graph = FakeGraph::new();
        graph.with_site();
        for drive in ["drive-docs", "drive-priv"] {
            let cleaned = folder
                .trim_start_matches("/teams/project")
                .trim_matches('/');
            if cleaned.is_empty() {
                continue;
            }
            let encoded = cleaned.replace(' ', "%20");
            graph.on(
                Method::GET,
                &format!("graph.microsoft.com/v1.0/drives/{drive}/root:/{encoded}"),
                vec![Reply::status(404)],
            );
        }
        let api = client(&graph);
        let sentence = refused(
            &api,
            None,
            "upload_file",
            json!({"folder_path":folder,"filedata":"x","filename":"a.txt"}),
        )
        .await;
        assert!(sentence.contains(MISSING), "{folder}: {sentence}");
        assert!(
            graph
                .requests()
                .iter()
                .all(|(method, _)| *method == Method::GET),
            "{folder}: nothing was written"
        );
    }
    // Inside a named library the SDK's path upload may create the subfolders.
    let graph = FakeGraph::new();
    graph.with_site().on(
        Method::PUT,
        "graph.microsoft.com/v1.0/drives/drive-docs/root:/New/Sub/a.txt:/content",
        vec![Reply::json(
            201,
            &json!({"id":"1","name":"a.txt","parentReference":{"path":"/drives/drive-docs/root:/New/Sub"}}),
        )],
    );
    let api = client(&graph);
    let done = ok(
        &api,
        None,
        "upload_file",
        json!({"folder_path":"/teams/project/Shared Documents/New/Sub","filedata":"x","filename":"a.txt"}),
    )
    .await;
    assert_eq!(done["id"], "1");
}

/// Probing is bounded, ordered with the default library first, and stops at
/// the first library that has the folder.
#[tokio::test]
async fn library_probes_stop_at_the_first_match_and_are_bounded() {
    let graph = FakeGraph::new();
    let drives = (0..30)
        .map(|n| {
            json!({"id":format!("d{n:02}"),"name":format!("L{n}"),"webUrl":format!("https://tenant.sharepoint.com/teams/project/L{n}")})
        })
        .collect::<Vec<_>>();
    graph
        .on(
            Method::GET,
            SITE_LOOKUP,
            vec![Reply::json(
                200,
                &json!({"id":"tenant.sharepoint.com,site-id,web-id"}),
            )],
        )
        .on(
            Method::GET,
            &format!("{SITE}/drives"),
            vec![Reply::json(200, &json!({"value":drives}))],
        )
        .on(
            Method::GET,
            &format!("{SITE}/drive"),
            vec![Reply::json(200, &json!({"id":"d07"}))],
        );
    for n in 0..30 {
        let reply = if n == 3 {
            Reply::json(200, &json!({"id":"x"}))
        } else {
            Reply::status(404)
        };
        graph.on(
            Method::GET,
            &format!("graph.microsoft.com/v1.0/drives/d{n:02}/root:/Shared/Folder"),
            vec![reply],
        );
    }
    graph.on(
        Method::PUT,
        "graph.microsoft.com/v1.0/drives/d03/root:/Shared/Folder/a.txt:/content",
        vec![Reply::json(
            201,
            &json!({"id":"1","name":"a.txt","parentReference":{"path":"p"}}),
        )],
    );
    let api = client(&graph);
    ok(
        &api,
        None,
        "upload_file",
        json!({"folder_path":"Shared/Folder","filedata":"x","filename":"a.txt"}),
    )
    .await;
    let probed = graph
        .requests()
        .iter()
        .filter(|(_, key)| key.ends_with("/root:/Shared/Folder"))
        .count();
    assert!(probed < 30, "stopped at the first match, probed {probed}");
    assert!(probed <= 10, "bounded concurrency, probed {probed}");
    assert!(
        graph.count(
            &Method::GET,
            "graph.microsoft.com/v1.0/drives/d07/root:/Shared/Folder"
        ) >= 1,
        "the default library is asked first"
    );
}

/// The reader is checked before a byte is downloaded: no reader, an unreadable
/// kind of file.
#[tokio::test]
#[allow(clippy::too_many_lines, reason = "one scenario per kind of file")]
async fn the_reader_and_the_file_kind_are_checked_before_anything_is_downloaded() {
    let graph = FakeGraph::new();
    graph.with_site();
    let api = client(&graph);
    for (failure, fragment) in [
        (
            TextExtractionFailure::NoExtractor,
            "lends no document reader",
        ),
        (
            TextExtractionFailure::Unsupported,
            "file type is not supported",
        ),
    ] {
        let host = FakeHost::new();
        *host.precheck_failure.lock().expect("failure") = Some(failure);
        let shared = host.shared();
        let sentence = refused(
            &api,
            Some(&shared),
            "read_document",
            json!({"path":"/drives/drive-docs/root:/Folder/Plan.pdf"}),
        )
        .await;
        assert!(sentence.contains(fragment), "{sentence}");
    }
    // By its extension: an image is not read, and is not downloaded.
    let host = FakeHost::new().shared();
    let sentence = refused(
        &api,
        Some(&host),
        "read_document",
        json!({"path":"/teams/project/Shared Documents/photo.png"}),
    )
    .await;
    assert!(
        sentence.contains("file type is not supported"),
        "{sentence}"
    );
    assert!(
        graph
            .requests()
            .iter()
            .all(|(_, key)| !key.ends_with("/content")),
        "no content was requested: {:?}",
        graph.requests()
    );

    // A sharing link: the name is known from the metadata, before the bytes.
    let graph = FakeGraph::new();
    graph.on(
        Method::GET,
        &share_key("driveItem"),
        vec![share_item("pic.png", 10, Some(DOWNLOAD_URL))],
    );
    let api = client(&graph);
    let sentence = refused(
        &api,
        Some(&host),
        "read_file_from_sharing_link",
        json!({"sharing_url":SHARE_URL}),
    )
    .await;
    assert!(
        sentence.contains("file type is not supported"),
        "{sentence}"
    );
    assert_eq!(graph.requests().len(), 1, "only the metadata was asked");
    let sentence = refused(
        &api,
        None,
        "read_file_from_sharing_link",
        json!({"sharing_url":SHARE_URL}),
    )
    .await;
    assert!(sentence.contains("lends no document reader"), "{sentence}");
    assert_eq!(graph.requests().len(), 1);

    // An attachment of a kind that cannot be read is not downloaded either.
    let html = r#"<p><object data-attachment="scan.png" data="https://graph.microsoft.com/v1.0/sites/tenant.sharepoint.com,site-id,web-id/onenote/resources/res-9/$value"></object></p>"#;
    let graph = FakeGraph::new();
    graph.with_site().on(
        Method::GET,
        &onenote_key("/pages/pg-1/content"),
        vec![Reply::html(html)],
    );
    let api = client(&graph);
    let text = ok(
        &api,
        Some(&host),
        "onenote_read_page",
        json!({"page_id":"pg-1","read_attachment_content":true}),
    )
    .await;
    assert!(
        text.as_str()
            .expect("text")
            .contains("Content: not available: the file type is not supported"),
        "{text}"
    );
    let sentence = refused(
        &api,
        Some(&host),
        "onenote_read_attachment",
        json!({"page_id":"pg-1","attachment_name":"scan.png"}),
    )
    .await;
    assert!(
        sentence.contains("file type is not supported"),
        "{sentence}"
    );
    assert_eq!(
        graph.count(&Method::GET, &onenote_key("/resources/res-9/content")),
        0
    );
}

/// The page keeps all its own text; attachments are read into what is left,
/// in order, each as much as fits, and those that do not fit are named.
#[tokio::test]
async fn attachments_are_fitted_into_the_page_limit_and_the_cut_ones_are_named() {
    let attachment = |n: u32| {
        format!(
            r#"<object data-attachment="a{n}.txt" data="https://graph.microsoft.com/v1.0/sites/tenant.sharepoint.com,site-id,web-id/onenote/resources/res-{n}/$value"></object>"#
        )
    };
    let paragraphs = "<p>line of page text</p>".repeat(6_000);
    let html = format!(
        "{paragraphs}{}{}{}",
        attachment(1),
        attachment(2),
        attachment(3)
    );
    let graph = FakeGraph::new();
    graph.with_site().on(
        Method::GET,
        &onenote_key("/pages/pg-1/content"),
        vec![Reply::html(&html)],
    );
    for n in 1..=3 {
        // 80 thousand characters each, in lines.
        let body = format!("attachment {n} line\n").repeat(4_000);
        graph.on(
            Method::GET,
            &onenote_key(&format!("/resources/res-{n}/content")),
            vec![Reply::bytes(body.as_bytes())],
        );
    }
    let api = client(&graph);
    let host = FakeHost::new().shared();
    let text = ok(
        &api,
        Some(&host),
        "onenote_read_page",
        json!({"page_id":"pg-1","read_attachment_content":true}),
    )
    .await;
    let text = text.as_str().expect("text");
    assert!(text.chars().count() <= 200_000, "{}", text.chars().count());
    // The page text is whole.
    assert_eq!(text.matches("line of page text").count(), 6_000);
    // The first attachment fits whole; the later ones are cut or left out,
    // by name, never silently.
    assert!(text.contains("[attachment: a1.txt]"));
    assert!(text.contains("Content:\n  attachment 1 line"));
    assert!(
        !text.contains("[page text cut"),
        "the page itself was not cut"
    );
    let named = |n: u32| {
        let at = text
            .find(&format!("[attachment: a{n}.txt]"))
            .expect("named");
        text[at..]
            .split("\n-----\n")
            .next()
            .expect("block")
            .to_owned()
    };
    let second = named(2);
    let third = named(3);
    assert!(
        second.contains("attachment text cut") || second.contains("Content: not available: cut"),
        "{}",
        &second[second.len().saturating_sub(200)..]
    );
    assert!(
        third.contains("attachment text cut") || third.contains("Content: not available: cut"),
        "{}",
        &third[third.len().saturating_sub(200)..]
    );
}

/// A resumable upload whose session still asks for bytes after the last chunk
/// is fed the missing range; one that never settles is a definite failure and
/// the session is cancelled.
#[tokio::test]
#[allow(
    clippy::too_many_lines,
    reason = "one scenario per way a session can end"
)]
async fn a_session_that_asks_for_missing_ranges_is_repaired_or_cancelled() {
    let item = Reply::json(
        201,
        &json!({"id":"BIG","name":"big.bin","size":6_291_456,"parentReference":{"path":"/drives/drive-docs/root:/Reports"}}),
    );
    // 202 for chunk 1; 202 asking for byte 5 MiB again after the last chunk;
    // then the item.
    let graph = FakeGraph::new();
    graph
        .with_site()
        .on(Method::POST, SESSION, vec![session_reply()])
        .on(
            Method::PUT,
            UPLOAD_PATH,
            vec![
                Reply::json(202, &json!({"nextExpectedRanges":["5242880-"]})),
                Reply::json(202, &json!({"nextExpectedRanges":["5242880-"]})),
                item,
            ],
        );
    let api = client(&graph);
    let host = big_host();
    let done = ok(
        &api,
        Some(&host),
        "upload_file",
        json!({"folder_path":"/teams/project/Shared Documents/Reports","filepath":"/bucket/big.bin"}),
    )
    .await;
    assert_eq!(done["id"], "BIG");
    let ranges = graph
        .seen
        .lock()
        .expect("seen")
        .iter()
        .filter(|seen| seen.method == Method::PUT)
        .map(|seen| {
            seen.headers["content-range"]
                .to_str()
                .expect("range")
                .to_owned()
        })
        .collect::<Vec<_>>();
    assert_eq!(
        ranges,
        [
            "bytes 0-5242879/6291456",
            "bytes 5242880-6291455/6291456",
            "bytes 5242880-6291455/6291456"
        ],
        "the missing range was sent again"
    );

    // Never settles: bounded repairs, then a definite failure and a cancel.
    let graph = FakeGraph::new();
    graph
        .with_site()
        .on(Method::POST, SESSION, vec![session_reply()])
        .on(
            Method::PUT,
            UPLOAD_PATH,
            vec![Reply::json(
                202,
                &json!({"nextExpectedRanges":["5242880-"]}),
            )],
        )
        .on(Method::DELETE, UPLOAD_PATH, vec![Reply::status(204)]);
    let api = client(&graph);
    let sentence = refused(
        &api,
        Some(&host),
        "upload_file",
        json!({"folder_path":"/teams/project/Shared Documents/Reports","filepath":"/bucket/big.bin"}),
    )
    .await;
    assert!(
        sentence.contains("did not accept the whole file"),
        "{sentence}"
    );
    assert!(
        !sentence.contains(UNKNOWN),
        "a definite failure: {sentence}"
    );
    assert_eq!(graph.count(&Method::DELETE, UPLOAD_PATH), 1);
    assert!(graph.count(&Method::PUT, UPLOAD_PATH) <= 6, "bounded");

    // A 202 on the last chunk that names no range at all: definite failure.
    let graph = FakeGraph::new();
    graph
        .with_site()
        .on(Method::POST, SESSION, vec![session_reply()])
        .on(
            Method::PUT,
            UPLOAD_PATH,
            vec![
                Reply::json(202, &json!({"nextExpectedRanges":["5242880-"]})),
                Reply::json(202, &json!({})),
            ],
        )
        .on(Method::DELETE, UPLOAD_PATH, vec![Reply::status(204)]);
    let api = client(&graph);
    let sentence = refused(
        &api,
        Some(&host),
        "upload_file",
        json!({"folder_path":"/teams/project/Shared Documents/Reports","filepath":"/bucket/big.bin"}),
    )
    .await;
    assert!(
        sentence.contains("did not accept the whole file"),
        "{sentence}"
    );
    assert_eq!(graph.count(&Method::DELETE, UPLOAD_PATH), 1);
}
