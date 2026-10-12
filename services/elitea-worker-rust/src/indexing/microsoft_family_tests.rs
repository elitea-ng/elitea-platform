//! ADR-0030 Verification (2) for the I2 Microsoft families — `sharepoint`,
//! `ado_wiki`, `ado_boards`, `ado_plans` — end to end through the index run:
//! each family's connector is built from a real toolkit configuration and
//! `index_data` arguments, lists and fetches against a recorded HTTP fixture
//! (no network), and writes to the in-process fake of `elitea-vector`. Each
//! family indexes its fixture, indexes it again unchanged (the store sees no
//! write and no delete), then with one document changed and one removed
//! (only the changed document's chunks are rewritten; the removed
//! document's chunks go).

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use elitea_connectors::transport::header::{CONTENT_TYPE, HeaderValue};
use elitea_connectors::transport::{
    HeaderMap, Request, Response, StatusCode, Transport, TransportError,
};
use elitea_vector_index::client::toolkit_namespace;
use elitea_vector_index::testing::FakeVector;
use elitea_vector_index::{ClientConfig, VectorClient, pb};
use http_body_util::{BodyExt as _, Full};
use serde_json::{Value, json};
use tonic::body::Body;

use super::pipeline::RunLiveness;
use super::{IndexIngestRuntime, IndexRun, IndexRunLimits, run_index};
use crate::execution::ClaimLeaseError;
use crate::protocol::elitea::runtime::v1::{
    IndexIngestCommandV1, IndexIngestStatusV1, IndexIngestSummaryV1, VectorClaimTokenV1,
};
use crate::protocol::vector_token::VectorClaimToken;
use crate::transport::embeddings_facade::EmbeddingsClient;
use crate::transport::openai_compatible_facade::{
    ModelGatewayTransport, ModelGatewayTransportError,
};
use crate::transport::runtime_context::ClaimScopedEliteaContext;

const NS: &str = "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5c";
const TOKEN: &str = "elvc_0123456789abcdef0123456789abcdef0123456789b";
const MODEL: &str = "text-embedding-3-small";
const DIMENSION: usize = 4;

// ---------------------------------------------------------------- fixtures

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

/// A gateway that embeds every input deterministically (its byte sum).
struct Embeddings {
    calls: AtomicUsize,
}

#[async_trait]
impl ModelGatewayTransport for Embeddings {
    async fn post(
        &self,
        request: http::Request<Body>,
    ) -> Result<http::Response<Body>, ModelGatewayTransportError> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let body = request
            .into_body()
            .collect()
            .await
            .map_err(|_| ModelGatewayTransportError::Unavailable)?
            .to_bytes();
        let parsed: Value = serde_json::from_slice(&body).expect("json body");
        let data: Vec<Value> = parsed["input"]
            .as_array()
            .expect("inputs")
            .iter()
            .enumerate()
            .map(|(index, input)| {
                let sum: u32 = input.as_str().unwrap_or("").bytes().map(u32::from).sum();
                let mut vector = vec![1.0_f32; DIMENSION];
                #[allow(clippy::cast_precision_loss, reason = "a test score under 97")]
                let score = (sum % 97) as f32 / 97.0 + 0.01;
                vector[0] = score;
                json!({"object": "embedding", "index": index, "embedding": vector})
            })
            .collect();
        let reply =
            json!({"object": "list", "model": MODEL, "data": data, "usage": {"prompt_tokens": 1}});
        Ok(http::Response::builder()
            .status(StatusCode::OK)
            .version(http::Version::HTTP_2)
            .header("content-type", "application/json")
            .body(Body::new(Full::new(bytes::Bytes::from(reply.to_string()))))
            .expect("response"))
    }
}

struct Always;

impl RunLiveness for Always {
    fn ensure_running(&self) -> Result<(), ClaimLeaseError> {
        Ok(())
    }
}

struct Harness {
    runtime: IndexIngestRuntime,
    fake: FakeVector,
    embeddings: Arc<Embeddings>,
    token: VectorClaimToken,
    context: ClaimScopedEliteaContext,
    _server: tokio::task::JoinHandle<()>,
}

async fn harness(transport: Arc<Recorded>) -> Harness {
    let fake = FakeVector::new(&[(TOKEN, 1)]);
    let (address, server) = fake.clone().serve().await.expect("serve");
    let mut config = ClientConfig::new(format!("http://{address}"));
    config.allow_insecure = true;
    let vector = VectorClient::connect_lazy(&config).expect("client");
    let embeddings = Arc::new(Embeddings {
        calls: AtomicUsize::new(0),
    });
    let runtime = IndexIngestRuntime {
        vector,
        embeddings: EmbeddingsClient::with_transport(
            Arc::clone(&embeddings) as Arc<dyn ModelGatewayTransport>,
            Duration::from_secs(5),
        ),
        removal: None,
        platform_origin: "https://platform.internal".to_owned(),
        allowlist: elitea_connectors::egress::HostAllowlist::parse(Some(
            "graph.microsoft.com login.microsoftonline.com contoso.sharepoint.com dev.azure.com",
        )),
        transport,
        limits: IndexRunLimits {
            document_concurrency: 2,
            embedding_batch: 8,
            embedding_retry_backoff: Duration::from_millis(1),
            ..IndexRunLimits::default()
        },
        extractor: None,
    };
    let token = VectorClaimToken::from_receipt(Some(VectorClaimTokenV1 {
        bearer: TOKEN.to_owned(),
        expires_at_unix_millis: i64::MAX,
        allowed_sources: vec!["toolkit_index".to_owned()],
    }))
    .expect("token");
    Harness {
        runtime,
        fake,
        embeddings,
        token,
        context: ClaimScopedEliteaContext::fixture(1, "pat"),
        _server: server,
    }
}

impl Harness {
    async fn run(
        &self,
        toolkit_type: &str,
        settings: Value,
        arguments: Value,
        stamped: bool,
    ) -> IndexIngestSummaryV1 {
        let configuration =
            json!({"id": 7, "type": toolkit_type, "toolkit_name": "kit", "settings": settings})
                .to_string()
                .into_bytes();
        let mut parameters = json!({"index_name": "docs", "clean_index": false});
        if let Value::Object(extra) = arguments {
            for (key, value) in extra {
                parameters[key] = value;
            }
        }
        let parameters = parameters.to_string().into_bytes();
        let binding = json!({"schema_version": "v2", "model_name": MODEL, "resolved_model_group": MODEL, "route": "raw"})
            .to_string()
            .into_bytes();
        let (model, dimension) = if stamped {
            (MODEL, u32::try_from(DIMENSION).expect("dimension"))
        } else {
            ("", 0)
        };
        let command = IndexIngestCommandV1 {
            toolkit_configuration_entry_id: "c".into(),
            tool_parameters_entry_id: "p".into(),
            initiator: "user".into(),
            expected_embedding_model: model.to_owned(),
            expected_embedding_dimension: dimension,
            index_id: NS.to_owned(),
            ..Default::default()
        };
        let now = || 1_700_000_000_000_i64;
        run_index(
            &self.runtime,
            IndexRun {
                command: &command,
                toolkit_configuration: &configuration,
                tool_parameters: &parameters,
                embedding_binding: Some(&binding),
                vector_token: Some(&self.token),
                context: &self.context,
                lease: &Always,
                deadline_unix_millis: i64::MAX,
                now_unix_millis: &now,
            },
        )
        .await
        .expect("summary")
    }

    async fn stored(&self) -> BTreeMap<String, (String, u64)> {
        self.runtime
            .vector
            .list_documents(&self.token, toolkit_namespace(NS), None)
            .await
            .expect("list")
            .into_iter()
            .map(|document: pb::IndexedDocument| {
                (
                    document.document_key,
                    (document.document_version, document.chunk_count),
                )
            })
            .collect()
    }

    /// The store calls made since `before` that write or delete.
    fn writes_since(&self, before: usize) -> Vec<&'static str> {
        self.fake.calls()[before..]
            .iter()
            .copied()
            .filter(|call| matches!(*call, "upsert" | "delete"))
            .collect()
    }
}

/// The three-run Verification (2) drill shared by the families: `state`
/// is the recorded source, `mutate` changes one document and removes
/// another, `changed`/`removed` name them.
#[allow(clippy::too_many_arguments)] // The drill's inputs, spelled out per family.
async fn verify_incremental<S: Send + 'static>(
    harness: &Harness,
    toolkit_type: &str,
    settings: Value,
    arguments: Value,
    state: &Arc<Mutex<S>>,
    expected_keys: &[&str],
    mutate: impl FnOnce(&mut S),
    changed: &str,
    removed: &str,
) {
    let first = harness
        .run(toolkit_type, settings.clone(), arguments.clone(), false)
        .await;
    assert_eq!(first.status, IndexIngestStatusV1::Ok as i32, "{first:?}");
    let stored = harness.stored().await;
    let keys: Vec<&str> = stored.keys().map(String::as_str).collect();
    assert_eq!(
        keys, expected_keys,
        "{toolkit_type}: the first run indexed the fixture"
    );
    assert_eq!(first.indexed_documents, expected_keys.len() as u64);
    assert!(first.whole_index_record);
    let before = harness.fake.calls().len();
    let embed_calls = harness.embeddings.calls.load(Ordering::SeqCst);

    // Unchanged: nothing is written or deleted, nothing is embedded.
    let second = harness
        .run(toolkit_type, settings.clone(), arguments.clone(), true)
        .await;
    assert_eq!(second.status, IndexIngestStatusV1::Ok as i32, "{second:?}");
    assert_eq!(second.indexed_documents, 0);
    assert_eq!(second.unchanged_documents(), expected_keys.len() as u64);
    assert!(
        harness.writes_since(before).is_empty(),
        "{toolkit_type}: an unchanged source writes nothing"
    );
    assert_eq!(harness.embeddings.calls.load(Ordering::SeqCst), embed_calls);
    assert_eq!(harness.stored().await, stored);
    let before = harness.fake.calls().len();

    // One changed, one removed.
    mutate(&mut state.lock().expect("state"));
    let third = harness.run(toolkit_type, settings, arguments, true).await;
    assert_eq!(third.status, IndexIngestStatusV1::Ok as i32, "{third:?}");
    assert_eq!(
        third.indexed_documents, 1,
        "{toolkit_type}: only the changed document was indexed"
    );
    assert_eq!(third.updated, 1);
    assert_eq!(third.documents.len(), 1);
    assert_eq!(third.documents[0].document_key, changed);
    assert_eq!(third.removed_document_keys, vec![removed.to_owned()]);
    let writes = harness.writes_since(before);
    assert_eq!(
        writes,
        ["delete", "upsert", "delete"],
        "{toolkit_type}: the changed document's chunks replaced, the removed document's deleted"
    );
    let after = harness.stored().await;
    assert!(!after.contains_key(removed));
    assert_ne!(after[changed].0, stored[changed].0, "the version moved");
    for (key, (version, chunks)) in &stored {
        if key != changed && key != removed {
            assert_eq!(&after[key], &(version.clone(), *chunks), "{key} untouched");
        }
    }
}

trait Unchanged {
    fn unchanged_documents(&self) -> u64;
}

impl Unchanged for IndexIngestSummaryV1 {
    fn unchanged_documents(&self) -> u64 {
        // The store's total less what this run wrote: the unchanged documents.
        self.total_documents.unwrap_or(0) - self.indexed_documents
    }
}

fn query(request: &Request, name: &str) -> Option<String> {
    request
        .url()
        .query_pairs()
        .find(|(key, _)| key == name)
        .map(|(_, value)| value.into_owned())
}

fn body_json(request: &Request) -> Value {
    serde_json::from_slice(request.body().and_then(|b| b.as_bytes()).expect("body"))
        .expect("json body")
}

// -------------------------------------------------------------- sharepoint

/// A library: item id → (name, content tag, bytes).
type Library = BTreeMap<&'static str, (&'static str, String, String)>;

fn sharepoint_library() -> Library {
    BTreeMap::from([
        (
            "i1",
            (
                "README.md",
                "c1".to_owned(),
                "# Hello\n\nThe project readme.\n".to_owned(),
            ),
        ),
        (
            "i2",
            (
                "notes.txt",
                "c2".to_owned(),
                "Meeting notes, first version.".to_owned(),
            ),
        ),
        (
            "i3",
            (
                "old.md",
                "c3".to_owned(),
                "# Old\n\nTo be removed.\n".to_owned(),
            ),
        ),
    ])
}

fn sharepoint_fixture(state: Arc<Mutex<Library>>) -> Arc<Recorded> {
    Recorded::new(move |request| {
        let url = request.url();
        let library = state.lock().expect("state");
        match (url.host_str().unwrap_or_default(), url.path()) {
            ("login.microsoftonline.com", "/contoso.onmicrosoft.com/oauth2/v2.0/token") => {
                Reply::json(&json!({"access_token": "tok", "expires_in": 3600}))
            }
            ("graph.microsoft.com", "/v1.0/sites/contoso.sharepoint.com:/sites/eng") => {
                Reply::json(&json!({"id": "site-1"}))
            }
            ("graph.microsoft.com", "/v1.0/sites/site-1/drives") => Reply::json(&json!({
                "value": [{"id": "d1", "name": "Documents", "webUrl": "https://contoso.sharepoint.com/sites/eng/Shared%20Documents"}]
            })),
            ("graph.microsoft.com", "/v1.0/drives/d1/root/delta") => {
                // As Graph serves it: the root item first, every item with
                // its parent's id and no `parentReference.path`.
                let value: Vec<Value> = std::iter::once(
                    json!({"id": "root-d1", "name": "root", "root": {}, "folder": {"childCount": 3},
                           "parentReference": {"driveId": "d1"}}),
                )
                .chain(library.iter().map(|(id, (name, ctag, body))| {
                    json!({"id": id, "name": name, "size": body.len(), "cTag": ctag,
                           "lastModifiedDateTime": "2026-10-01T00:00:00Z", "file": {},
                           "webUrl": format!("https://contoso.sharepoint.com/sites/eng/{name}"),
                           "parentReference": {"driveId": "d1", "id": "root-d1"}})
                }))
                .collect();
                Reply::json(
                    &json!({"value": value, "@odata.deltaLink": "https://graph.microsoft.com/v1.0/drives/d1/root/delta?token=x"}),
                )
            }
            ("graph.microsoft.com", path) if path.ends_with("/content") => {
                let id = path
                    .trim_start_matches("/v1.0/drives/d1/items/")
                    .trim_end_matches("/content");
                match library.get(id) {
                    Some((_, _, body)) => Reply::bytes(body.as_bytes()),
                    None => Reply::status(StatusCode::NOT_FOUND),
                }
            }
            other => panic!("unexpected request {other:?}"),
        }
    })
}

#[tokio::test(flavor = "multi_thread")]
async fn sharepoint_indexes_its_fixture_then_only_what_changed() {
    let state = Arc::new(Mutex::new(sharepoint_library()));
    let harness = harness(sharepoint_fixture(Arc::clone(&state))).await;
    verify_incremental(
        &harness,
        "sharepoint",
        json!({"sharepoint_configuration": {
            "site_url": "https://contoso.sharepoint.com/sites/eng",
            "client_id": "app", "client_secret": "secret",
        }}),
        json!({"skip_extensions": ["png"]}),
        &state,
        &["Documents/i1", "Documents/i2", "Documents/i3"],
        |library| {
            library.insert(
                "i2",
                (
                    "notes.txt",
                    "c2-b".to_owned(),
                    "Meeting notes, revised and longer.".to_owned(),
                ),
            );
            library.remove("i3");
        },
        "Documents/i2",
        "Documents/i3",
    )
    .await;
    // The key is the item id; the human path lives in the metadata and the
    // media type comes from the name.
    let points = harness.stored().await;
    assert_eq!(points["Documents/i2"].0, "c2-b");
}

// ---------------------------------------------------------------- ado_wiki

/// Wiki pages: page path → (page id, blob id, content).
type Wiki = BTreeMap<&'static str, (u64, String, String)>;

fn wiki_pages() -> Wiki {
    BTreeMap::from([
        (
            "/Home",
            (1, "a".repeat(40), "# Home\n\nWelcome.\n".to_owned()),
        ),
        (
            "/Home/Setup",
            (2, "b".repeat(40), "# Setup\n\nInstall it.\n".to_owned()),
        ),
        (
            "/Legacy",
            (3, "c".repeat(40), "# Legacy\n\nGone soon.\n".to_owned()),
        ),
    ])
}

fn git_path(page: &str) -> String {
    format!("{}.md", page.replace(' ', "-"))
}

fn wiki_fixture(state: Arc<Mutex<Wiki>>) -> Arc<Recorded> {
    const ROOT: &str = "1111111111111111111111111111111111111111";
    Recorded::new(move |request| {
        let pages = state.lock().expect("state");
        let path = request.url().path();
        match path {
            "/contoso/Fabrikam/_apis/wiki/wikis/Fabrikam.wiki" => Reply::json(&json!({
                "id": "w1", "name": "Fabrikam.wiki", "repositoryId": "repo-1", "versions": [{"version": "wikiMaster"}]
            })),
            "/contoso/Fabrikam/_apis/wiki/wikis/Fabrikam.wiki/pages" => {
                let sub_pages: Vec<Value> = pages
                    .iter()
                    .map(|(page, (id, _, _))| json!({"id": id, "path": page, "gitItemPath": git_path(page)}))
                    .collect();
                Reply::json(&json!({"path": "/", "subPages": sub_pages}))
            }
            "/contoso/Fabrikam/_apis/git/repositories/repo-1/items" => {
                Reply::json(&json!({"objectId": ROOT}))
            }
            p if p == format!("/contoso/Fabrikam/_apis/git/repositories/repo-1/trees/{ROOT}") => {
                let entries: Vec<Value> = pages
                    .iter()
                    .map(|(page, (_, blob, body))| {
                        json!({"relativePath": git_path(page).trim_start_matches('/'), "gitObjectType": "blob",
                               "objectId": blob, "size": body.len(), "mode": "100644"})
                    })
                    .collect();
                Reply::json(&json!({"treeEntries": entries}))
            }
            p if p.starts_with("/contoso/Fabrikam/_apis/git/repositories/repo-1/blobs/") => {
                let blob = p.rsplit('/').next().unwrap_or_default();
                match pages.values().find(|(_, id, _)| id == blob) {
                    Some((_, _, body)) => Reply::bytes(body.as_bytes()),
                    None => Reply::status(StatusCode::NOT_FOUND),
                }
            }
            other => panic!("unexpected request {other}"),
        }
    })
}

#[tokio::test(flavor = "multi_thread")]
async fn ado_wiki_indexes_its_fixture_then_only_what_changed() {
    let state = Arc::new(Mutex::new(wiki_pages()));
    let harness = harness(wiki_fixture(Arc::clone(&state))).await;
    verify_incremental(
        &harness,
        "ado_wiki",
        json!({"ado_configuration": {"organization_url": "https://dev.azure.com/contoso", "token": "pat"}, "project": "Fabrikam"}),
        json!({"wiki_identifier": "Fabrikam.wiki", "chunking_tool": "markdown"}),
        &state,
        &["Home.md", "Home/Setup.md", "Legacy.md"],
        |pages| {
            pages.insert("/Home/Setup", (2, "d".repeat(40), "# Setup\n\nInstall it, then configure it.\n".to_owned()));
            pages.remove("/Legacy");
        },
        "Home/Setup.md",
        "Legacy.md",
    )
    .await;
}

// -------------------------------------------------------------- ado_boards

/// A comment as the comments API returns one: (id, version, text).
type Comments = Vec<(u64, u64, &'static str)>;
/// Work items: id → (rev, comments, description).
type Board = BTreeMap<u64, (u64, Comments, String)>;

fn board() -> Board {
    BTreeMap::from([
        (
            1,
            (
                3,
                vec![(11, 1, "<p>Reproduced on main.</p>")],
                "<p>Crash on <b>start</b></p>".to_owned(),
            ),
        ),
        (2, (1, vec![], "Slow page".to_owned())),
        (3, (2, vec![], "Typo in footer".to_owned())),
    ])
}

fn board_fixture(state: Arc<Mutex<Board>>) -> Arc<Recorded> {
    fn item(id: u64, rev: u64, comments: &Comments, description: &str, attachments: bool) -> Value {
        // As in Azure DevOps, a field edit and a comment add or edit are
        // each a revision: the change date moves with them.
        let moved = rev + comments.iter().map(|(_, version, _)| version).sum::<u64>();
        let changed = format!("2026-10-01T00:{moved:02}:00Z");
        let comments = comments.len();
        let mut relations = Vec::new();
        if attachments {
            relations.push(json!({"rel": "AttachedFile", "url": "https://dev.azure.com/contoso/p/_apis/wit/attachments/att-1",
                                  "attributes": {"id": 5, "name": "log.txt", "resourceSize": 9,
                                                 "resourceModifiedDate": "2026-09-30T00:00:00Z"}}));
        }
        json!({"id": id, "rev": rev, "fields": {
            "System.Title": format!("Item {id}"), "System.WorkItemType": "Bug", "System.State": "Active",
            "System.CommentCount": comments, "System.ChangedDate": changed,
            "System.Description": description,
        }, "relations": relations})
    }
    Recorded::new(move |request| {
        let items = state.lock().expect("state");
        let path = request.url().path();
        match path {
            "/contoso/Fabrikam/_apis/wit/wiql" => {
                let ids: Vec<Value> = items.keys().map(|id| json!({"id": id})).collect();
                Reply::json(&json!({"workItems": ids}))
            }
            "/contoso/Fabrikam/_apis/wit/workitemsbatch" => {
                let asked: Vec<u64> = body_json(request)["ids"]
                    .as_array()
                    .expect("ids")
                    .iter()
                    .filter_map(Value::as_u64)
                    .collect();
                let value: Vec<Value> = items
                    .iter()
                    .filter(|(id, _)| asked.contains(id))
                    .map(|(id, (rev, comments, description))| {
                        item(*id, *rev, comments, description, *id == 1)
                    })
                    .collect();
                Reply::json(&json!({"value": value}))
            }
            p if p.starts_with("/contoso/Fabrikam/_apis/wit/workitems/") => {
                let id: u64 = p
                    .rsplit('/')
                    .next()
                    .and_then(|s| s.parse().ok())
                    .expect("id");
                assert_eq!(query(request, "$expand").as_deref(), Some("all"));
                match items.get(&id) {
                    Some((rev, comments, description)) => {
                        Reply::json(&item(id, *rev, comments, description, id == 1))
                    }
                    None => Reply::status(StatusCode::NOT_FOUND),
                }
            }
            p if p.starts_with("/contoso/Fabrikam/_apis/wit/workItems/")
                && p.ends_with("/comments") =>
            {
                let id: u64 = p
                    .trim_start_matches("/contoso/Fabrikam/_apis/wit/workItems/")
                    .trim_end_matches("/comments")
                    .parse()
                    .expect("id");
                let comments: Vec<Value> = items
                    .get(&id)
                    .map(|(_, comments, _)| comments.as_slice())
                    .unwrap_or_default()
                    .iter()
                    .map(|(comment_id, version, text)| {
                        json!({"id": comment_id, "version": version, "text": text, "workItemId": id,
                               "createdBy": {"displayName": "Ada"}, "createdDate": "2026-10-01T01:00:00Z",
                               "modifiedDate": format!("2026-10-01T01:0{version}:00Z")})
                    })
                    .collect();
                Reply::json(
                    &json!({"totalCount": comments.len(), "count": comments.len(), "comments": comments}),
                )
            }
            "/contoso/p/_apis/wit/attachments/att-1" => Reply::bytes(b"log lines"),
            other => panic!("unexpected request {other}"),
        }
    })
}

#[tokio::test(flavor = "multi_thread")]
async fn ado_boards_indexes_work_items_with_their_dependents_then_only_what_changed() {
    let state = Arc::new(Mutex::new(board()));
    let harness = harness(board_fixture(Arc::clone(&state))).await;
    verify_incremental(
        &harness,
        "ado_boards",
        json!({"ado_configuration": {"organization_url": "https://dev.azure.com/contoso", "token": "pat"}, "project": "Fabrikam"}),
        json!({"wiql": "SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = @project"}),
        &state,
        &[
            "work_items/1.json",
            "work_items/1/attachments/log.txt",
            "work_items/1/comments.md",
            "work_items/2.json",
            "work_items/3.json",
        ],
        |items| {
            items.insert(2, (2, vec![], "Slow page, now measured at 4 s".to_owned()));
            items.remove(&3);
        },
        "work_items/2.json",
        "work_items/3.json",
    )
    .await;
    // Each document is versioned by its own identity: the work item by its
    // revision, the attachment by its GUID, size and date, the discussion
    // by a hash over its comments.
    let points = harness.stored().await;
    assert_eq!(points["work_items/1.json"].0, "3");
    assert_eq!(
        points["work_items/1/attachments/log.txt"].0,
        "att-1:9:2026-09-30T00:00:00Z"
    );
    let (hash, state) = points["work_items/1/comments.md"]
        .0
        .split_once('@')
        .expect("hash@count@date");
    assert_eq!(hash.len(), 64);
    assert!(state.starts_with("1@"), "{state}");
}

/// The keys the second of two runs wrote, after `mutate` changed the board.
async fn written_after(mutate: impl FnOnce(&mut Board)) -> (Vec<String>, Harness) {
    let state = Arc::new(Mutex::new(board()));
    let harness = harness(board_fixture(Arc::clone(&state))).await;
    let settings = json!({"ado_configuration": {"organization_url": "https://dev.azure.com/contoso", "token": "pat"}, "project": "Fabrikam"});
    let arguments = json!({"wiql": "SELECT [System.Id] FROM WorkItems"});
    harness
        .run("ado_boards", settings.clone(), arguments.clone(), false)
        .await;
    mutate(&mut state.lock().expect("state"));
    let summary = harness.run("ado_boards", settings, arguments, true).await;
    let mut written: Vec<String> = summary
        .documents
        .iter()
        .map(|d| d.document_key.clone())
        .collect();
    written.sort_unstable();
    (written, harness)
}

#[tokio::test(flavor = "multi_thread")]
async fn a_field_edit_re_indexes_only_the_work_item_document() {
    // rev 3 → 4, the discussion and the attachment untouched.
    let (written, harness) = written_after(|items| {
        items.insert(
            1,
            (
                4,
                vec![(11, 1, "<p>Reproduced on main.</p>")],
                "<p>Crash on <b>start</b>, also on resume</p>".to_owned(),
            ),
        );
    })
    .await;
    assert_eq!(written, ["work_items/1.json"]);
    let points = harness.stored().await;
    assert_eq!(points["work_items/1.json"].0, "4");
    assert_eq!(
        points["work_items/1/attachments/log.txt"].0,
        "att-1:9:2026-09-30T00:00:00Z"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn a_new_comment_re_indexes_only_the_discussion_document() {
    let (written, harness) = written_after(|items| {
        items.insert(
            1,
            (
                3,
                vec![
                    (11, 1, "<p>Reproduced on main.</p>"),
                    (12, 1, "Fixed in #42."),
                ],
                "<p>Crash on <b>start</b></p>".to_owned(),
            ),
        );
    })
    .await;
    assert_eq!(written, ["work_items/1/comments.md"]);
    assert_eq!(harness.stored().await["work_items/1.json"].0, "3");

    // An edited comment (version 1 → 2) too.
    let (written, _) = written_after(|items| {
        items.insert(
            1,
            (
                3,
                vec![(11, 2, "<p>Reproduced on main and release.</p>")],
                "<p>Crash on <b>start</b></p>".to_owned(),
            ),
        );
    })
    .await;
    assert_eq!(written, ["work_items/1/comments.md"]);
}

// --------------------------------------------------------------- ado_plans

/// Test cases: (suite, case id) → (rev, title).
type Plan = BTreeMap<(u64, u64), (u64, String)>;

fn plan() -> Plan {
    BTreeMap::from([
        ((10, 100), (1, "Smoke".to_owned())),
        ((11, 101), (2, "Login works".to_owned())),
        ((11, 102), (1, "Logout works".to_owned())),
    ])
}

fn plan_fixture(state: Arc<Mutex<Plan>>) -> Arc<Recorded> {
    Recorded::new(move |request| {
        let cases = state.lock().expect("state");
        let path = request.url().path();
        match path {
            "/contoso/Fabrikam/_apis/testplan/Plans/7/suites" => Reply::json(&json!({
                "value": [{"id": 10, "name": "Root"}, {"id": 11, "name": "Login"}]
            })),
            p if p.starts_with("/contoso/Fabrikam/_apis/testplan/Plans/7/Suites/") => {
                let suite: u64 = p
                    .trim_start_matches("/contoso/Fabrikam/_apis/testplan/Plans/7/Suites/")
                    .trim_end_matches("/TestCase")
                    .parse()
                    .expect("suite");
                let value: Vec<Value> = cases
                    .iter()
                    .filter(|((s, _), _)| *s == suite)
                    .map(|((_, id), (rev, title))| {
                        json!({"workItem": {"id": id, "name": title, "workItemFields": [
                            {"System.Rev": rev},
                        ]}})
                    })
                    .collect();
                Reply::json(&json!({"value": value}))
            }
            p if p.starts_with("/contoso/Fabrikam/_apis/wit/workitems/") => {
                let id: u64 = p
                    .rsplit('/')
                    .next()
                    .and_then(|s| s.parse().ok())
                    .expect("id");
                match cases.iter().find(|((_, case), _)| *case == id) {
                    Some((_, (rev, title))) => {
                        Reply::json(&json!({"id": id, "rev": rev, "fields": {
                            "System.Rev": rev, "System.State": "Design",
                            "System.ChangedDate": "2026-10-01T00:00:00Z",
                            "System.Description": format!("Checks that {title}."),
                            "Microsoft.VSTS.TCM.Steps": "<steps><step id=\"1\"><parameterizedString>Do it</parameterizedString><parameterizedString>Done</parameterizedString></step></steps>",
                        }}))
                    }
                    None => Reply::status(StatusCode::NOT_FOUND),
                }
            }
            other => panic!("unexpected request {other}"),
        }
    })
}

#[tokio::test(flavor = "multi_thread")]
async fn ado_plans_indexes_suites_and_cases_then_only_what_changed() {
    let state = Arc::new(Mutex::new(plan()));
    let harness = harness(plan_fixture(Arc::clone(&state))).await;
    verify_incremental(
        &harness,
        "ado_plans",
        json!({"ado_configuration": {"organization_url": "https://dev.azure.com/contoso", "token": "pat"}, "project": "Fabrikam"}),
        json!({"plan_id": 7}),
        &state,
        &[
            "plans/7/suites/10/100.md",
            "plans/7/suites/11/101.md",
            "plans/7/suites/11/102.md",
        ],
        |cases| {
            cases.insert((11, 101), (3, "Login works with SSO".to_owned()));
            cases.remove(&(10, 100));
        },
        "plans/7/suites/11/101.md",
        "plans/7/suites/10/100.md",
    )
    .await;
}

// ------------------------------------------------------------- refusals

#[tokio::test(flavor = "multi_thread")]
async fn a_family_without_its_required_argument_fails_with_the_reason() {
    let harness = harness(Recorded::new(|_| panic!("no request"))).await;
    let settings = json!({"ado_configuration": {"organization_url": "https://dev.azure.com/contoso", "token": "pat"}, "project": "Fabrikam"});
    let summary = harness
        .run("ado_boards", settings.clone(), json!({}), false)
        .await;
    assert_eq!(summary.status, IndexIngestStatusV1::Error as i32);
    assert!(
        summary.message.contains("`wiql` is required"),
        "{}",
        summary.message
    );
    let summary = harness.run("ado_plans", settings, json!({}), false).await;
    assert!(
        summary.message.contains("`plan_id` is required"),
        "{}",
        summary.message
    );
    let summary = harness
        .run(
            "sharepoint",
            json!({"sharepoint_configuration": {"site_url": "https://contoso.sharepoint.com", "client_id": "a", "client_secret": "b"}}),
            json!({"include_onenote": true}),
            false,
        )
        .await;
    assert!(
        summary.message.contains("include_onenote"),
        "{}",
        summary.message
    );
    assert!(harness.fake.calls().is_empty(), "nothing reached the store");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_refused_credential_fails_the_run_before_the_store_is_touched() {
    let transport = Recorded::new(|_| Reply::status(StatusCode::UNAUTHORIZED));
    let harness = harness(Arc::clone(&transport)).await;
    let summary = harness
        .run(
            "ado_wiki",
            json!({"ado_configuration": {"organization_url": "https://dev.azure.com/contoso", "token": "bad"}, "project": "Fabrikam", "default_wiki_identifier": "w"}),
            json!({}),
            false,
        )
        .await;
    assert_eq!(summary.status, IndexIngestStatusV1::Error as i32);
    assert!(summary.message.contains("HTTP 401"), "{}", summary.message);
    assert!(
        summary.total_documents.is_none(),
        "the store was never touched"
    );
    assert_eq!(transport.seen().len(), 1);
    assert!(!harness.fake.calls().contains(&"upsert"));
}
