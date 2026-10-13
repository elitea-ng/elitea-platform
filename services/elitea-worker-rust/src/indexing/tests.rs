//! The index run against the in-process fake of `elitea-vector`
//! (`elitea_vector_index::testing`), a scripted embeddings gateway and a
//! fixture source: first run, incremental run (unchanged, changed, vanished),
//! clean run, the embedding-space fence, the gateway's refusal classes, a
//! failure after the store was touched, Stop, and the bounded record.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use elitea_content_source::{Acl, Document, DocumentRef};
use elitea_vector_index::client::toolkit_namespace;
use elitea_vector_index::testing::FakeVector;
use elitea_vector_index::{ClientConfig, VectorClient, pb};
use http::{Request, Response, StatusCode, Version};
use http_body_util::{BodyExt as _, Full};
use serde_json::{Map, Value, json};
use tonic::body::Body;

use super::inputs::IndexParameters;
use super::pipeline::{RunLiveness, failure_for_test, record_for_test};
use super::source::{IndexSource, PathFilter};
use super::{
    IndexIngestRuntime, IndexRun, IndexRunFailure, IndexRunLimits, Interruption, interrupt_summary,
    run_index,
};
use crate::execution::ClaimLeaseError;
use crate::protocol::elitea::runtime::v1::{
    IndexIngestCommandV1, IndexIngestDocumentV1, IndexIngestStatusV1, IndexIngestTerminalStateV1,
    VectorClaimTokenV1,
};
use crate::protocol::vector_token::VectorClaimToken;
use crate::transport::embeddings_facade::EmbeddingsClient;
use crate::transport::openai_compatible_facade::{
    ModelGatewayTransport, ModelGatewayTransportError,
};
use crate::transport::runtime_context::ClaimScopedEliteaContext;

pub(super) const NS: &str = "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b";
const TOKEN: &str = "elvc_0123456789abcdef0123456789abcdef0123456789a";
pub(super) const MODEL: &str = "text-embedding-3-small";

/// A gateway that answers every embeddings request with a deterministic
/// vector per input (its byte sum), and counts the calls. `fail_with` makes
/// it answer with that status instead: for every request, or only for a
/// request whose inputs contain `fail_marker`.
pub(super) struct ScriptedEmbeddings {
    pub(super) calls: AtomicUsize,
    dimension: usize,
    fail_with: Option<StatusCode>,
    fail_marker: Option<&'static str>,
}

/// The text that makes the scripted gateway refuse a request.
const FAIL_MARKER: &str = "FAILME";

#[async_trait]
impl ModelGatewayTransport for ScriptedEmbeddings {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, ModelGatewayTransportError> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        assert_eq!(request.uri().path(), "/llm/v1/embeddings");
        let body = request
            .into_body()
            .collect()
            .await
            .map_err(|_| ModelGatewayTransportError::Unavailable)?
            .to_bytes();
        let parsed: Value = serde_json::from_slice(&body).expect("json body");
        assert_eq!(parsed["model"], MODEL);
        let marked = self.fail_marker.is_none_or(|marker| {
            parsed["input"].as_array().is_some_and(|inputs| {
                inputs
                    .iter()
                    .any(|input| input.as_str().is_some_and(|text| text.contains(marker)))
            })
        });
        if let Some(status) = self.fail_with.filter(|_| marked) {
            return Ok(Response::builder()
                .status(status)
                .version(Version::HTTP_2)
                .body(Body::new(Full::new(bytes::Bytes::from_static(b"{}"))))
                .expect("response"));
        }
        let inputs = parsed["input"].as_array().expect("inputs");
        let data: Vec<Value> = inputs
            .iter()
            .enumerate()
            .map(|(index, input)| {
                let sum: u32 = input.as_str().unwrap_or("").bytes().map(u32::from).sum();
                let mut vector = vec![1.0_f32; self.dimension];
                #[allow(clippy::cast_precision_loss, reason = "a test score under 97")]
                let score = (sum % 97) as f32 / 97.0 + 0.01;
                vector[0] = score;
                json!({"object": "embedding", "index": index, "embedding": vector})
            })
            .collect();
        let reply =
            json!({"object": "list", "model": MODEL, "data": data, "usage": {"prompt_tokens": 1}});
        Ok(Response::builder()
            .status(StatusCode::OK)
            .version(Version::HTTP_2)
            .header("content-type", "application/json")
            .body(Body::new(Full::new(bytes::Bytes::from(reply.to_string()))))
            .expect("response"))
    }
}

pub(super) struct Always;

impl RunLiveness for Always {
    fn ensure_running(&self) -> Result<(), ClaimLeaseError> {
        Ok(())
    }
}

struct StopAfter {
    calls: AtomicUsize,
    after: usize,
}

impl RunLiveness for StopAfter {
    fn ensure_running(&self) -> Result<(), ClaimLeaseError> {
        if self.calls.fetch_add(1, Ordering::SeqCst) >= self.after {
            Err(ClaimLeaseError::Cancelled("stop requested"))
        } else {
            Ok(())
        }
    }
}

/// A lease lost (another worker may own the claim) from the `after`-th
/// check on.
struct LoseAfter {
    calls: AtomicUsize,
    after: usize,
}

impl RunLiveness for LoseAfter {
    fn ensure_running(&self) -> Result<(), ClaimLeaseError> {
        if self.calls.fetch_add(1, Ordering::SeqCst) >= self.after {
            Err(ClaimLeaseError::LeaseLost("lease expired"))
        } else {
            Ok(())
        }
    }
}

/// The liveness checks a first, non-clean run makes before its first
/// document: one after the source listing, one after the store listing.
/// The third check is the first document's, so a liveness that fails from
/// the fourth on stops the run after exactly one document was written.
const CHECKS_BEFORE_FIRST_DOCUMENT: usize = 2;

/// The clock of a run whose deadline passes after `calls` reads: every
/// liveness check reads it once.
fn deadline_after(calls: usize) -> (i64, impl Fn() -> i64) {
    const DEADLINE: i64 = 1_700_000_100_000;
    let reads = AtomicUsize::new(0);
    (DEADLINE, move || {
        if reads.fetch_add(1, Ordering::SeqCst) < calls {
            DEADLINE - 1_000
        } else {
            DEADLINE + 1
        }
    })
}

pub(super) struct Harness {
    pub(super) runtime: IndexIngestRuntime,
    pub(super) fake: FakeVector,
    pub(super) embeddings: Arc<ScriptedEmbeddings>,
    pub(super) token: VectorClaimToken,
    pub(super) context: ClaimScopedEliteaContext,
    pub(super) _server: tokio::task::JoinHandle<()>,
}

pub(super) async fn harness(dimension: usize, fail_with: Option<StatusCode>) -> Harness {
    harness_failing(dimension, fail_with, None).await
}

async fn harness_failing(
    dimension: usize,
    fail_with: Option<StatusCode>,
    fail_marker: Option<&'static str>,
) -> Harness {
    let fake = FakeVector::new(&[(TOKEN, 1)]);
    let (address, server) = fake.clone().serve().await.expect("serve");
    let mut config = ClientConfig::new(format!("http://{address}"));
    config.allow_insecure = true;
    let vector = VectorClient::connect_lazy(&config).expect("client");
    let embeddings = Arc::new(ScriptedEmbeddings {
        calls: AtomicUsize::new(0),
        dimension,
        fail_with,
        fail_marker,
    });
    let runtime = IndexIngestRuntime {
        chat_models: None,
        vector,
        embeddings: EmbeddingsClient::with_transport(
            Arc::clone(&embeddings) as Arc<dyn ModelGatewayTransport>,
            Duration::from_secs(5),
        ),
        removal: None,
        platform_origin: "https://platform.internal".to_owned(),
        allowlist: elitea_connectors::egress::HostAllowlist::parse(Some("*")),
        transport: Arc::new(
            elitea_connectors::reqwest012::ReqwestTransport::hardened(
                &elitea_connectors::github::client::CLIENT_POLICY,
            )
            .expect("transport"),
        ),
        limits: IndexRunLimits {
            document_concurrency: 2,
            embedding_batch: 3,
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

/// A listed document whose fetch fails (see `IndexSource::fetch`'s fixture
/// arm).
fn unfetchable(key: &str, version: &str, text: &str) -> Document {
    let mut document = document(key, version, text);
    document
        .metadata
        .insert("fixture_unfetchable".to_owned(), Value::Bool(true));
    document
}

pub(super) fn document(key: &str, version: &str, text: &str) -> Document {
    Document {
        reference: DocumentRef {
            key: key.to_owned(),
            version: version.to_owned(),
            mime: elitea_content_source::mime_of(key).to_owned(),
            size: text.len() as u64,
            acl: Acl::Project,
        },
        title: key.rsplit('/').next().unwrap_or(key).to_owned(),
        uri: None,
        bytes: text.as_bytes().to_vec(),
        metadata: Map::from_iter([("branch".to_owned(), Value::String("main".to_owned()))]),
    }
}

/// A PNG-signature file (a picture the vision step would describe).
pub(super) fn png_document(key: &str) -> Document {
    let mut document = document(key, "v1", "");
    document.bytes = b"\x89PNG\r\n\x1a\npixels".to_vec();
    document.reference.size = document.bytes.len() as u64;
    document
}

pub(super) fn command(index_id: &str, expected: Option<(&str, u32)>) -> IndexIngestCommandV1 {
    let (model, dimension) = expected.unwrap_or(("", 0));
    IndexIngestCommandV1 {
        toolkit_configuration_entry_id: "c".into(),
        tool_parameters_entry_id: "p".into(),
        initiator: "user".into(),
        expected_embedding_model: model.to_owned(),
        expected_embedding_dimension: dimension,
        index_id: index_id.to_owned(),
        ..Default::default()
    }
}

pub(super) fn configuration() -> Vec<u8> {
    json!({"id": 7, "type": "fixture", "toolkit_name": "repo", "settings": {}})
        .to_string()
        .into_bytes()
}

pub(super) fn parameters(clean: bool, extra: Value) -> Vec<u8> {
    let mut map = json!({"index_name": "docs", "clean_index": clean});
    if let Value::Object(extra) = extra {
        for (key, value) in extra {
            map[key] = value;
        }
    }
    map.to_string().into_bytes()
}

pub(super) fn binding() -> Vec<u8> {
    json!({"schema_version": "v2", "model_name": MODEL, "resolved_model_group": MODEL, "route": "raw"})
        .to_string()
        .into_bytes()
}

/// Runs the pipeline against `documents` with the fixture source swapped in
/// for the toolkit's connector.
pub(super) async fn run(
    harness: &Harness,
    documents: Vec<Document>,
    command: &IndexIngestCommandV1,
    parameters: &[u8],
    liveness: &dyn RunLiveness,
) -> Result<crate::protocol::elitea::runtime::v1::IndexIngestSummaryV1, IndexRunFailure> {
    let now = || 1_700_000_000_000_i64;
    run_timed(
        harness,
        documents,
        command,
        parameters,
        liveness,
        i64::MAX,
        &now,
    )
    .await
}

/// [`run`] with the command deadline and the clock the run reads.
#[allow(clippy::too_many_arguments)]
async fn run_timed(
    harness: &Harness,
    documents: Vec<Document>,
    command: &IndexIngestCommandV1,
    parameters: &[u8],
    liveness: &dyn RunLiveness,
    deadline_unix_millis: i64,
    now: &(dyn Fn() -> i64 + Send + Sync),
) -> Result<crate::protocol::elitea::runtime::v1::IndexIngestSummaryV1, IndexRunFailure> {
    FIXTURE_SOURCE.with(|slot| *slot.lock().expect("lock") = Some(documents));
    run_index(
        &harness.runtime,
        IndexRun {
            command,
            toolkit_configuration: &configuration(),
            tool_parameters: parameters,
            parsed: None,
            embedding_binding: Some(&binding()),
            llm_model: None,
            llm_configuration: None,
            chat_entries: None,
            vector_token: Some(&harness.token),
            context: &harness.context,
            lease: liveness,
            deadline_unix_millis,
            now_unix_millis: now,
        },
    )
    .await
}

thread_local! {
    /// The documents the `fixture` toolkit type lists (see
    /// `IndexSource::build`'s test arm).
    pub(super) static FIXTURE_SOURCE: Mutex<Option<Vec<Document>>> = const { Mutex::new(None) };
}

/// The staged documents as the `fixture` toolkit, with the scope its
/// arguments ask for: `fixture_skip_unsupported` (the code families'
/// file-type allowlist) and `fixture_attests_empty` (the artifact bucket's
/// attested empty listing).
pub(super) fn take_fixture_source(parameters: &IndexParameters) -> Option<IndexSource> {
    let flag = |name: &str| parameters.arguments.get(name) == Some(&Value::Bool(true));
    FIXTURE_SOURCE.with(|slot| {
        slot.lock().expect("lock").take().map(|documents| {
            IndexSource::Fixture(
                documents,
                PathFilter::for_test(
                    &parameters.whitelist,
                    &parameters.blacklist,
                    flag("fixture_skip_unsupported"),
                    flag("fixture_attests_empty"),
                ),
            )
        })
    })
}

pub(super) async fn stored(harness: &Harness) -> Vec<pb::IndexedDocument> {
    harness
        .runtime
        .vector
        .list_documents(&harness.token, toolkit_namespace(NS), None)
        .await
        .expect("list")
}

#[tokio::test(flavor = "multi_thread")]
async fn a_first_run_writes_every_document_and_stamps_the_space() {
    let harness = harness(4, None).await;
    let summary = run(
        &harness,
        vec![
            document(
                "README.md",
                "v1",
                "# Title\n\nSome words about the project.\n",
            ),
            document("src/app.py", "v1", "def main():\n    return 1\n"),
            png_document("logo.png"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    // An image is described by the chat model, and this harness has none:
    // the picture step is on by default and optional, so the skip is
    // reported but does not degrade the run.
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 2);
    assert_eq!(summary.updated, 0);
    assert_eq!(summary.skipped.get("no_model"), Some(&1));
    assert_eq!(summary.embedding_model, MODEL);
    assert_eq!(summary.embedding_dimension, 4);
    assert_eq!(summary.total_documents, Some(2));
    assert_eq!(summary.total_chunks, Some(summary.indexed_chunks));
    assert!(
        summary.whole_index_record,
        "a first run's record is the whole index"
    );
    assert_eq!(summary.reindex, Some(false));
    assert!(!summary.documents_truncated);
    // Before the index is stamped, the store is listed across every
    // collection (ADR-0030 decision 2: the first run stamps it).
    assert_eq!(harness.fake.listed_spaces(), vec![None]);
    let mut written: Vec<&str> = summary
        .documents
        .iter()
        .map(|d| d.document_key.as_str())
        .collect();
    written.sort_unstable();
    assert_eq!(written, ["README.md", "src/app.py"]);
    let points = stored(&harness).await;
    assert_eq!(points.len(), 2);
    assert!(points.iter().all(|p| p.document_version == "v1"));
    assert_eq!(harness.fake.point_count() as u64, summary.indexed_chunks);
    assert!(harness.embeddings.calls.load(Ordering::SeqCst) >= 1);
}

#[tokio::test(flavor = "multi_thread")]
async fn an_incremental_run_skips_unchanged_rewrites_changed_and_deletes_vanished() {
    let harness = harness(4, None).await;
    let first = run(
        &harness,
        vec![
            document("a.md", "v1", "alpha text one"),
            document("b.md", "v1", "bravo text one"),
            document("c.md", "v1", "charlie text one"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("first");
    assert_eq!(first.total_documents, Some(3));
    let calls_after_first = harness.embeddings.calls.load(Ordering::SeqCst);

    let second = run(
        &harness,
        vec![
            document("a.md", "v1", "alpha text one"),
            document("b.md", "v2", "bravo text two, longer than before"),
        ],
        &command(NS, Some((MODEL, 4))),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("second");
    assert_eq!(second.status, IndexIngestStatusV1::Ok as i32, "{second:?}");
    assert_eq!(second.indexed_documents, 1);
    assert_eq!(second.updated, 1);
    assert_eq!(second.removed_document_keys, vec!["c.md"]);
    assert_eq!(second.total_documents, Some(2));
    assert_eq!(second.reindex, Some(true));
    assert_eq!(second.documents.len(), 1);
    assert_eq!(second.documents[0].document_key, "b.md");
    assert_eq!(second.documents[0].version, "v2");
    assert!(
        !second.whole_index_record,
        "a.md is unchanged and outside the record"
    );
    // The stamped index is listed in its own space (finding: the listing
    // names the expected space when the command does).
    assert_eq!(
        harness.fake.listed_spaces().last().cloned().flatten(),
        Some((super::slug::vector_space_slug(MODEL), 4))
    );
    let stored_chunks: u64 = stored(&harness).await.iter().map(|p| p.chunk_count).sum();
    assert_eq!(second.total_chunks, Some(stored_chunks));
    // Only the changed document was embedded again.
    assert_eq!(
        harness.embeddings.calls.load(Ordering::SeqCst),
        calls_after_first + 1
    );
    let points = stored(&harness).await;
    let mut keys: Vec<(String, String)> = points
        .iter()
        .map(|p| (p.document_key.clone(), p.document_version.clone()))
        .collect();
    keys.sort();
    assert_eq!(
        keys,
        vec![
            ("a.md".to_owned(), "v1".to_owned()),
            ("b.md".to_owned(), "v2".to_owned())
        ]
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn a_clean_run_starts_the_index_empty_and_a_dimension_change_writes_nothing() {
    let harness = harness(4, None).await;
    run(
        &harness,
        vec![
            document("a.md", "v1", "alpha"),
            document("old.md", "v1", "gone"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("first");
    let clean = run(
        &harness,
        vec![document("a.md", "v1", "alpha")],
        &command(NS, Some((MODEL, 4))),
        &parameters(true, json!({})),
        &Always,
    )
    .await
    .expect("clean");
    assert_eq!(clean.indexed_documents, 1, "{clean:?}");
    assert_eq!(clean.total_documents, Some(1));
    assert!(clean.whole_index_record);
    let points = stored(&harness).await;
    assert_eq!(points.len(), 1);
    assert_eq!(points[0].document_key, "a.md");

    // The model now returns 8 dimensions against a 4-dimensional stamp: the
    // run fails before any write, and the existing points stay.
    let other = harness_with_fake(harness.fake.clone(), 8).await;
    let before = other.fake.point_count();
    let mismatch = run(
        &other,
        vec![document("a.md", "v2", "alpha two")],
        &command(NS, Some((MODEL, 4))),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(mismatch.status, IndexIngestStatusV1::Error as i32);
    assert_eq!(
        mismatch.terminal_state,
        IndexIngestTerminalStateV1::Failed as i32
    );
    assert!(
        mismatch
            .message
            .contains("embedding dimension changed from 4"),
        "{}",
        mismatch.message
    );
    assert!(mismatch.embedding_model.is_empty());
    assert_eq!(
        mismatch.total_documents, None,
        "nothing was written, so the summary says nothing about the store"
    );
    assert_eq!(other.fake.point_count(), before);
    assert_eq!(stored(&other).await[0].document_version, "v1");
}

/// A second harness over an already serving fake, with another dimension.
async fn harness_with_fake(fake: FakeVector, dimension: usize) -> Harness {
    let (address, server) = fake.clone().serve().await.expect("serve");
    let mut config = ClientConfig::new(format!("http://{address}"));
    config.allow_insecure = true;
    let base = harness(dimension, None).await;
    Harness {
        runtime: IndexIngestRuntime {
            vector: VectorClient::connect_lazy(&config).expect("client"),
            ..base.runtime
        },
        fake,
        embeddings: base.embeddings,
        token: base.token,
        context: base.context,
        _server: server,
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn a_different_expected_model_fails_before_listing() {
    let harness = harness(4, None).await;
    let summary = run(
        &harness,
        vec![document("a.md", "v1", "alpha")],
        &command(NS, Some(("other-model", 4))),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.status, IndexIngestStatusV1::Error as i32);
    assert!(
        summary
            .message
            .contains("embedding model changed from other-model")
    );
    assert_eq!(harness.embeddings.calls.load(Ordering::SeqCst), 0);
    assert_eq!(harness.fake.point_count(), 0);
}

/// A refusal no attempt cures (the credential, the project, the budget, an
/// unknown model: 4xx but 429) fails the run at once, with the reason; no
/// further document is embedded and nothing is written.
#[tokio::test(flavor = "multi_thread")]
async fn a_non_retryable_gateway_refusal_fails_the_run_at_once() {
    for (status, code) in [
        (StatusCode::FORBIDDEN, "model_gateway.forbidden"),
        (
            StatusCode::PAYMENT_REQUIRED,
            "model_gateway.budget_exhausted",
        ),
        (StatusCode::NOT_FOUND, "model_gateway.rejected"),
    ] {
        let harness = harness(4, Some(status)).await;
        let summary = run(
            &harness,
            vec![
                document("a.md", "v1", "alpha"),
                document("b.md", "v1", "bravo"),
                document("c.md", "v1", "charlie"),
            ],
            &command(NS, None),
            &parameters(false, json!({})),
            &Always,
        )
        .await
        .expect("summary");
        assert_eq!(
            summary.status,
            IndexIngestStatusV1::Error as i32,
            "{status}: {summary:?}"
        );
        assert_eq!(
            summary.terminal_state,
            IndexIngestTerminalStateV1::Failed as i32
        );
        assert!(
            summary
                .message
                .contains("the embedding gateway refused the run")
                && summary.message.contains(code),
            "{status}: {}",
            summary.message
        );
        assert_eq!(
            harness.embeddings.calls.load(Ordering::SeqCst),
            1,
            "{status}: one answer decided the run; nothing further was embedded"
        );
        assert_eq!(harness.fake.point_count(), 0);
        assert_eq!(summary.total_documents, None, "the store was not touched");
        assert!(summary.embedding_model.is_empty());
    }
}

/// A retryable failure (429, 5xx, a timeout) is retried, then fails the
/// document and the run goes on: the other documents are indexed and the
/// run is partly indexed.
#[tokio::test(flavor = "multi_thread")]
async fn a_retryable_gateway_failure_skips_the_document_and_the_run_goes_on() {
    let harness = harness_failing(4, Some(StatusCode::TOO_MANY_REQUESTS), Some(FAIL_MARKER)).await;
    let summary = run(
        &harness,
        vec![
            document("a.md", "v1", "alpha"),
            document("bad.md", "v1", &format!("bravo {FAIL_MARKER}")),
            document("c.md", "v1", "charlie"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::PartlyIndexed as i32,
        "{summary:?}"
    );
    assert!(summary.failed_chunks >= 1);
    assert_eq!(summary.skipped.get("embedding_failed"), Some(&1));
    assert_eq!(summary.indexed_documents, 2);
    assert_eq!(summary.total_documents, Some(2));
    assert_eq!(summary.embedding_model, MODEL);
    let points = stored(&harness).await;
    assert!(points.iter().all(|p| p.document_key != "bad.md"));
    assert_eq!(points.len(), 2);
}

/// Retryable failures on several documents in a row are a gateway outage,
/// not a property of the documents: after
/// `MAX_CONSECUTIVE_EMBEDDING_FAILURES` the run stops as FAILED rather than
/// marking every document skipped.
#[tokio::test(flavor = "multi_thread")]
async fn consecutive_retryable_failures_stop_the_run() {
    let harness = harness(4, Some(StatusCode::SERVICE_UNAVAILABLE)).await;
    let documents: Vec<Document> = (0..8)
        .map(|n| document(&format!("d{n}.md"), "v1", &format!("text {n}")))
        .collect();
    let summary = run(
        &harness,
        documents,
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Error as i32,
        "{summary:?}"
    );
    assert!(
        summary.message.contains(&format!(
            "failed for {} documents in a row",
            super::pipeline::MAX_CONSECUTIVE_EMBEDDING_FAILURES
        )),
        "{}",
        summary.message
    );
    // Each failed document was attempted thrice (the retry), and no later
    // document was embedded.
    assert_eq!(
        harness.embeddings.calls.load(Ordering::SeqCst),
        3 * super::pipeline::MAX_CONSECUTIVE_EMBEDDING_FAILURES as usize
    );
    assert_eq!(harness.fake.point_count(), 0);
    assert_eq!(summary.total_documents, None);
}

/// The totals describe the store after the run: a changed document whose
/// re-index failed (here: the fetch) keeps its old points, so it stays
/// counted with its old chunks, and the record is not the whole index.
#[tokio::test(flavor = "multi_thread")]
async fn totals_count_changed_documents_whose_old_points_stayed() {
    let harness = harness(4, None).await;
    let first_run: Vec<Document> = (0..10)
        .map(|n| document(&format!("d{n}.md"), "v1", &format!("text one {n}")))
        .collect();
    let first = run(
        &harness,
        first_run,
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("first");
    assert_eq!(first.total_documents, Some(10));
    let before: Vec<pb::IndexedDocument> = stored(&harness).await;
    let old_chunks = |key: &str| -> u64 {
        before
            .iter()
            .filter(|p| p.document_key == key)
            .map(|p| p.chunk_count)
            .sum()
    };

    // Every document changed; three cannot be fetched again.
    let second_run: Vec<Document> = (0..10)
        .map(|n| {
            let (key, version, text) = (format!("d{n}.md"), "v2", format!("text two {n} more"));
            if n % 3 == 0 {
                unfetchable(&key, version, &text)
            } else {
                document(&key, version, &text)
            }
        })
        .collect();
    let second = run(
        &harness,
        second_run,
        &command(NS, Some((MODEL, 4))),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("second");
    assert_eq!(
        second.status,
        IndexIngestStatusV1::PartlyIndexed as i32,
        "{second:?}"
    );
    assert_eq!(second.indexed_documents, 6);
    assert_eq!(second.skipped.get("fetch_failed"), Some(&4));
    assert_eq!(
        second.total_documents,
        Some(10),
        "the four documents that could not be re-indexed are still in the store"
    );
    let retained: u64 = ["d0.md", "d3.md", "d6.md", "d9.md"]
        .iter()
        .map(|key| old_chunks(key))
        .sum();
    assert_eq!(second.total_chunks, Some(second.indexed_chunks + retained));
    assert!(
        !second.whole_index_record,
        "four documents stand outside the record"
    );
    let after = stored(&harness).await;
    assert_eq!(after.len(), 10);
    assert_eq!(
        after.iter().filter(|p| p.document_version == "v1").count(),
        4
    );
    let stored_chunks: u64 = after.iter().map(|p| p.chunk_count).sum();
    assert_eq!(second.total_chunks, Some(stored_chunks));
}

/// A clean run that fails after it started writing reports the store it
/// left: the namespace was emptied, one document was written, so the record
/// is that document and it is the whole index.
#[tokio::test(flavor = "multi_thread")]
async fn a_clean_run_that_fails_mid_way_reports_the_store_it_left() {
    let harness = harness(4, None).await;
    run(
        &harness,
        vec![
            document("a.md", "v1", "alpha"),
            document("b.md", "v1", "bravo"),
            document("c.md", "v1", "charlie"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("first");
    assert_eq!(stored(&harness).await.len(), 3);
    // The second upsert of the clean run fails.
    harness.fake.fail_call(
        "upsert",
        2,
        elitea_vector_index::testing::status(tonic::Code::Unavailable),
    );
    let failed = run(
        &harness,
        vec![
            document("a.md", "v2", "alpha two"),
            document("b.md", "v2", "bravo two"),
            document("c.md", "v2", "charlie two"),
        ],
        &command(NS, Some((MODEL, 4))),
        &parameters(true, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        failed.status,
        IndexIngestStatusV1::Error as i32,
        "{failed:?}"
    );
    assert_eq!(
        failed.terminal_state,
        IndexIngestTerminalStateV1::Failed as i32
    );
    assert!(
        failed.message.contains("the vector store refused the run"),
        "{}",
        failed.message
    );
    assert_eq!(failed.indexed_documents, 1);
    assert_eq!(failed.total_documents, Some(1));
    assert_eq!(failed.total_chunks, Some(failed.indexed_chunks));
    assert_eq!(failed.documents.len(), 1);
    assert_eq!(failed.documents[0].version, "v2");
    assert!(failed.removed_document_keys.is_empty());
    assert!(
        failed.whole_index_record,
        "the namespace was emptied and one document written: the record is the store"
    );
    let points = stored(&harness).await;
    assert_eq!(points.len(), 1);
    assert_eq!(points[0].document_key, failed.documents[0].document_key);
    assert_eq!(points[0].document_version, "v2");
    assert_eq!(harness.fake.point_count() as u64, failed.indexed_chunks);
}

/// An incremental run that fails on its first write: the changed document
/// whose old points were deleted is reported removed (it is gone from the
/// store), the other changed document keeps its old points and stays
/// counted, and the record is not the whole index.
#[tokio::test(flavor = "multi_thread")]
async fn an_incremental_run_that_fails_mid_way_reports_dropped_and_retained_documents() {
    let harness = harness(4, None).await;
    let first = run(
        &harness,
        vec![
            document("a.md", "v1", "alpha"),
            document("b.md", "v1", "bravo"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("first");
    let before = stored(&harness).await;
    harness.fake.fail_call(
        "upsert",
        1,
        elitea_vector_index::testing::status(tonic::Code::Unavailable),
    );
    let failed = run(
        &harness,
        vec![
            document("a.md", "v2", "alpha two"),
            document("b.md", "v2", "bravo two"),
        ],
        &command(NS, Some((MODEL, 4))),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        failed.status,
        IndexIngestStatusV1::Error as i32,
        "{failed:?}"
    );
    assert_eq!(failed.indexed_documents, 0);
    assert!(failed.documents.is_empty());
    assert_eq!(
        failed.removed_document_keys.len(),
        1,
        "the document whose old points went before the write failed"
    );
    assert_eq!(failed.total_documents, Some(1));
    let dropped = failed.removed_document_keys[0].as_str();
    let retained_chunks: u64 = before
        .iter()
        .filter(|p| p.document_key != dropped)
        .map(|p| p.chunk_count)
        .sum();
    assert_eq!(failed.total_chunks, Some(retained_chunks));
    assert!(!failed.whole_index_record);
    assert_eq!(failed.reindex, Some(true));
    let after = stored(&harness).await;
    assert_eq!(after.len(), 1);
    assert_ne!(after[0].document_key, dropped);
    assert_eq!(after[0].document_version, "v1");
    assert_eq!(first.total_documents, Some(2));
}

/// A run whose only effect is to remove documents carries the (zero)
/// totals, present, and states its empty record is the whole index: the
/// registry resets its rows and totals instead of ignoring the run.
#[tokio::test(flavor = "multi_thread")]
async fn a_run_that_only_removes_documents_reports_zero_totals_present() {
    let harness = harness(4, None).await;
    run(
        &harness,
        vec![
            document("a.md", "v1", "alpha"),
            document("b.md", "v1", "bravo"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("first");
    let emptied = run(
        &harness,
        Vec::new(),
        &command(NS, Some((MODEL, 4))),
        // A source that attests it is empty (the artifact bucket).
        &parameters(false, json!({"fixture_attests_empty": true})),
        &Always,
    )
    .await
    .expect("second");
    assert_eq!(
        emptied.status,
        IndexIngestStatusV1::Ok as i32,
        "{emptied:?}"
    );
    assert_eq!(emptied.indexed_documents, 0);
    assert_eq!(emptied.removed_document_keys, vec!["a.md", "b.md"]);
    assert_eq!(emptied.total_documents, Some(0));
    assert_eq!(emptied.total_chunks, Some(0));
    assert!(emptied.whole_index_record);
    assert!(emptied.embedding_model.is_empty());
    assert_eq!(harness.fake.point_count(), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn stop_between_documents_is_a_cancellation() {
    let harness = harness(4, None).await;
    let result = run(
        &harness,
        vec![
            document("a.md", "v1", "alpha"),
            document("b.md", "v1", "bravo"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &StopAfter {
            calls: AtomicUsize::new(0),
            after: 1,
        },
    )
    .await;
    assert_eq!(result, Err(IndexRunFailure::Cancelled));
}

/// Stop after the first document was written (round 2, finding 1): the run
/// ends at the document boundary, the write it started completed, and the
/// summary is the store's record in the CANCELLED state, so Main records
/// the document and settles the execution cancelled.
#[tokio::test(flavor = "multi_thread")]
async fn a_stop_after_the_first_write_reports_the_store_and_ends_cancelled() {
    let harness = harness(4, None).await;
    let stopped = run(
        &harness,
        vec![
            document("a.md", "v1", "alpha"),
            document("b.md", "v1", "bravo"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &StopAfter {
            calls: AtomicUsize::new(0),
            after: CHECKS_BEFORE_FIRST_DOCUMENT + 1,
        },
    )
    .await
    .expect("a summary, not a runtime failure");
    assert_eq!(
        stopped.status,
        IndexIngestStatusV1::Error as i32,
        "{stopped:?}"
    );
    assert_eq!(
        stopped.terminal_state,
        IndexIngestTerminalStateV1::Cancelled as i32
    );
    assert!(
        stopped.message.contains("was stopped"),
        "{}",
        stopped.message
    );
    assert_eq!(stopped.indexed_documents, 1);
    assert_eq!(stopped.total_documents, Some(1));
    assert_eq!(stopped.total_chunks, Some(stopped.indexed_chunks));
    assert_eq!(stopped.documents.len(), 1);
    assert!(stopped.removed_document_keys.is_empty());
    assert!(
        stopped.whole_index_record,
        "the one document written is the whole store"
    );
    // Vectors were written, so the summary names the space they went into
    // (Main stamps the index only from an ok result).
    assert_eq!(stopped.embedding_model, MODEL);
    assert_eq!(stopped.embedding_dimension, 4);
    let points = stored(&harness).await;
    assert_eq!(points.len(), 1, "the started upsert completed");
    assert_eq!(points[0].document_key, stopped.documents[0].document_key);
    assert_eq!(harness.fake.point_count() as u64, stopped.indexed_chunks);
}

/// The deadline passes after the first document was written: the same
/// record, in the FAILED state (the execution settles failed).
#[tokio::test(flavor = "multi_thread")]
async fn a_deadline_after_the_first_write_reports_the_store_and_ends_failed() {
    let harness = harness(4, None).await;
    let (deadline, now) = deadline_after(CHECKS_BEFORE_FIRST_DOCUMENT + 1);
    let late = run_timed(
        &harness,
        vec![
            document("a.md", "v1", "alpha"),
            document("b.md", "v1", "bravo"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
        deadline,
        &now,
    )
    .await
    .expect("a summary, not a runtime failure");
    assert_eq!(late.status, IndexIngestStatusV1::Error as i32, "{late:?}");
    assert_eq!(
        late.terminal_state,
        IndexIngestTerminalStateV1::Failed as i32
    );
    assert!(late.message.contains("ran out of time"), "{}", late.message);
    assert_eq!(late.indexed_documents, 1);
    assert_eq!(late.total_documents, Some(1));
    assert_eq!(late.documents.len(), 1);
    assert!(late.whole_index_record);
    assert_eq!(stored(&harness).await.len(), 1);
}

/// A Stop or a deadline before anything was written stays a runtime failure
/// (the store is untouched, there is nothing to record), and a lost lease is
/// one at any point: another worker may own the claim, so nothing is
/// reported, but the write in flight still completed.
#[tokio::test(flavor = "multi_thread")]
async fn before_a_write_or_on_a_lost_lease_the_run_is_a_runtime_failure() {
    let harness = harness(4, None).await;
    let documents = || {
        vec![
            document("a.md", "v1", "alpha"),
            document("b.md", "v1", "bravo"),
        ]
    };
    let (deadline, now) = deadline_after(CHECKS_BEFORE_FIRST_DOCUMENT);
    let early = run_timed(
        &harness,
        documents(),
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
        deadline,
        &now,
    )
    .await;
    assert_eq!(early, Err(IndexRunFailure::DeadlineExceeded));
    assert_eq!(harness.fake.point_count(), 0);
    let lost = run(
        &harness,
        documents(),
        &command(NS, None),
        &parameters(false, json!({})),
        &LoseAfter {
            calls: AtomicUsize::new(0),
            after: CHECKS_BEFORE_FIRST_DOCUMENT + 1,
        },
    )
    .await;
    assert_eq!(lost, Err(IndexRunFailure::LeaseLost));
    assert_eq!(
        stored(&harness).await.len(),
        1,
        "the started upsert completed"
    );
}

/// A late Stop or deadline (after the run returned, before its terminal was
/// published) turns a summary that describes the store into the interrupted
/// form and leaves one that does not alone (finding 1, the processor's
/// post-run check).
#[test]
fn a_late_interruption_keeps_the_stores_record() {
    let mut touched = failure_for_test("docs", "x");
    touched.status = IndexIngestStatusV1::Ok as i32;
    touched.terminal_state = IndexIngestTerminalStateV1::Completed as i32;
    touched.message = "Index 'docs': 2 documents indexed.".to_owned();
    touched.total_documents = Some(2);
    touched.total_chunks = Some(5);
    touched.whole_index_record = true;
    assert!(interrupt_summary(&mut touched, Interruption::Stopped));
    assert_eq!(touched.status, IndexIngestStatusV1::Error as i32);
    assert_eq!(
        touched.terminal_state,
        IndexIngestTerminalStateV1::Cancelled as i32
    );
    assert_eq!(touched.total_documents, Some(2));
    assert!(touched.whole_index_record);
    assert!(
        touched.message.contains("was stopped"),
        "{}",
        touched.message
    );
    let message = touched.message.clone();
    assert!(interrupt_summary(&mut touched, Interruption::Stopped));
    assert_eq!(touched.message, message, "idempotent");
    let mut deadline = touched.clone();
    assert!(interrupt_summary(
        &mut deadline,
        Interruption::DeadlineExceeded
    ));
    assert_eq!(
        deadline.terminal_state,
        IndexIngestTerminalStateV1::Failed as i32
    );
    let mut untouched = failure_for_test("docs", "nothing written");
    assert!(!interrupt_summary(&mut untouched, Interruption::Stopped));
    assert_eq!(untouched, failure_for_test("docs", "nothing written"));
}

/// A clean run whose whole-namespace delete fails (finding 3): the delete
/// may or may not have happened, so the summary says nothing about the
/// store (no totals, no whole-index statement) and the registry keeps what
/// it had.
#[tokio::test(flavor = "multi_thread")]
async fn a_clean_run_whose_namespace_delete_fails_says_nothing_about_the_store() {
    let harness = harness(4, None).await;
    run(
        &harness,
        vec![
            document("a.md", "v1", "alpha"),
            document("b.md", "v1", "bravo"),
            document("c.md", "v1", "charlie"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("first");
    harness.fake.fail_call(
        "delete",
        1,
        elitea_vector_index::testing::status(tonic::Code::Unavailable),
    );
    let failed = run(
        &harness,
        vec![document("a.md", "v2", "alpha two")],
        &command(NS, Some((MODEL, 4))),
        &parameters(true, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        failed.status,
        IndexIngestStatusV1::Error as i32,
        "{failed:?}"
    );
    assert!(
        failed.message.contains("the vector store refused the run"),
        "{}",
        failed.message
    );
    assert_eq!(failed.total_documents, None);
    assert_eq!(failed.total_chunks, None);
    assert!(!failed.whole_index_record);
    assert!(failed.documents.is_empty());
    assert!(failed.removed_document_keys.is_empty());
    assert_eq!(failed.indexed_documents, 0);
    assert_eq!(stored(&harness).await.len(), 3, "nothing was written");
}

#[tokio::test(flavor = "multi_thread")]
async fn whitelist_blacklist_and_the_needs_model_chunker_are_honoured() {
    let harness = harness(4, None).await;
    let summary = run(
        &harness,
        vec![
            document("a.md", "v1", "alpha"),
            document("b.txt", "v1", "bravo"),
            document("c.md", "v1", "charlie"),
        ],
        &command(NS, None),
        &parameters(false, json!({"whitelist": ["*.md"], "blacklist": ["c*"]})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1);
    assert_eq!(summary.documents[0].document_key, "a.md");

    // `use_llm` is the image-file step: it never turns a text document into
    // one that needs a model.
    let with_use_llm = run(
        &harness,
        vec![document("d.md", "v1", "delta")],
        &command(NS, None),
        &parameters(false, json!({"chunking_config": {"use_llm": true}})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(with_use_llm.skipped.get("needs_model"), None);
    assert_eq!(with_use_llm.indexed_documents, 1);
    // The earlier document is no longer listed by the source, so it is gone.
    assert_eq!(with_use_llm.removed_document_keys, vec!["a.md"]);
    assert_eq!(with_use_llm.total_documents, Some(1));
    assert!(with_use_llm.whole_index_record);
}

#[tokio::test]
async fn malformed_inputs_are_invalid_input_not_a_summary() {
    let harness = harness(4, None).await;
    let now = || 1_700_000_000_000_i64;
    let run = run_index(
        &harness.runtime,
        IndexRun {
            command: &command(NS, None),
            toolkit_configuration: b"[]",
            tool_parameters: &parameters(false, json!({})),
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
    .await;
    assert!(
        matches!(run, Err(IndexRunFailure::InvalidInput(_))),
        "{run:?}"
    );
    let no_index = run_index(
        &harness.runtime,
        IndexRun {
            command: &command("", None),
            toolkit_configuration: &configuration(),
            tool_parameters: &parameters(false, json!({})),
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
    .await;
    assert!(matches!(no_index, Err(IndexRunFailure::InvalidInput(_))));
}

#[tokio::test]
async fn without_a_vector_token_or_a_model_the_run_fails_with_a_reason() {
    let harness = harness(4, None).await;
    let now = || 1_700_000_000_000_i64;
    let summary = run_index(
        &harness.runtime,
        IndexRun {
            command: &command(NS, None),
            toolkit_configuration: &configuration(),
            tool_parameters: &parameters(false, json!({})),
            parsed: None,
            embedding_binding: Some(&binding()),
            llm_model: None,
            llm_configuration: None,
            chat_entries: None,
            vector_token: None,
            context: &harness.context,
            lease: &Always,
            deadline_unix_millis: i64::MAX,
            now_unix_millis: &now,
        },
    )
    .await
    .expect("summary");
    assert_eq!(summary.status, IndexIngestStatusV1::Error as i32);
    assert!(summary.message.contains("no project token"));
    let no_model = run_index(
        &harness.runtime,
        IndexRun {
            command: &command(NS, None),
            toolkit_configuration: &configuration(),
            tool_parameters: &parameters(false, json!({})),
            parsed: None,
            embedding_binding: None,
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
    .expect("summary");
    assert!(no_model.message.contains("no embedding model"));
}

#[test]
fn the_document_record_is_bounded_and_flags_truncation() {
    let written: Vec<IndexIngestDocumentV1> = (0..2000)
        .map(|n| IndexIngestDocumentV1 {
            document_key: format!("docs/section-{n:04}/page.md"),
            version: "a".repeat(40),
            chunk_count: 3,
        })
        .collect();
    let (documents, removed, truncated) = record_for_test(written.clone(), vec!["gone".to_owned()]);
    assert!(truncated);
    assert!(documents.len() < 2000 && documents.len() > 100);
    assert!(
        removed.is_empty(),
        "removed keys come after the written ones"
    );
    let encoded: usize = documents
        .iter()
        .map(|d| prost::Message::encoded_len(d) + 3)
        .sum();
    assert!(encoded <= 40 * 1024);
    let (documents, removed, truncated) =
        record_for_test(written[..3].to_vec(), vec!["gone".to_owned()]);
    assert!(!truncated);
    assert_eq!(documents.len(), 3);
    assert_eq!(removed, vec!["gone"]);
}

/// The record is capped by count too (finding 2): 6000 tiny removed keys fit
/// the byte budget but not the protocol's 4096 per list, so the list is cut
/// there and flagged truncated; the same for tiny written documents.
#[test]
fn the_document_record_is_capped_at_the_protocol_bound() {
    let removed: Vec<String> = (0..6000).map(|n| format!("k{n}")).collect();
    let (documents, removed_keys, truncated) = record_for_test(Vec::new(), removed);
    assert!(truncated);
    assert!(documents.is_empty());
    assert_eq!(removed_keys.len(), 4096);
    assert_eq!(removed_keys[4095], "k4095");
    // Written documents cost more than a bare key, so the byte budget cuts
    // them first; either way the list never exceeds the protocol's bound.
    let written: Vec<IndexIngestDocumentV1> = (0..5000)
        .map(|n| IndexIngestDocumentV1 {
            document_key: format!("d{n}"),
            version: "v".to_owned(),
            chunk_count: 1,
        })
        .collect();
    let (documents, removed_keys, truncated) = record_for_test(written, vec!["gone".to_owned()]);
    assert!(truncated);
    assert!(
        documents.len() <= 4096 && documents.len() > 1000,
        "{}",
        documents.len()
    );
    assert!(
        removed_keys.is_empty(),
        "removed keys follow a truncated list"
    );
}

#[test]
fn a_failure_summary_names_the_index_and_is_bounded() {
    let summary = failure_for_test("docs", &"x".repeat(20_000));
    assert_eq!(summary.status, IndexIngestStatusV1::Error as i32);
    assert!(summary.message.starts_with("Index 'docs' failed: "));
    assert!(summary.message.len() <= 8 * 1024);
    assert!(summary.embedding_model.is_empty());
    assert_eq!(summary.indexed_documents, 0);
    assert_eq!(summary.total_documents, None);
    assert!(!summary.whole_index_record);
}

// ── the extraction sidecar ──────────────────────────────────────────────────

/// A scripted `elitea.extract.v1` server over a REAL Unix socket, standing
/// in for elitea-doc-extractor. The document's key decides the answer:
/// `skip-<reason>.<ext>` is that skip, `crash.<ext>` stops the server with
/// the call in flight, `hang.<ext>` never answers (the server stays up),
/// `fail.<ext>` answers a gRPC INTERNAL status (the server stays up; the
/// client records `extract_failed` and asks Capabilities again), `slow.<ext>`
/// answers text after a short while, anything else is text at once. Every
/// header it read is recorded with the bytes that followed; the most calls
/// it ever had in flight at once is counted.
mod fake_extractor {
    use std::path::Path;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::{Arc, Mutex};
    use std::time::Duration;

    use tokio::sync::Notify;
    use tonic::{Request, Response, Status, Streaming};

    use crate::protocol::extract::document_extract_service_server::{
        DocumentExtractService, DocumentExtractServiceServer,
    };
    use crate::protocol::extract::{
        CapabilitiesRequest, CapabilitiesResponse, ExtractLimits, ExtractRequest, ExtractResponse,
        ExtractSkip, ExtractedText, SkipReason, extract_request, extract_response,
    };

    pub(super) const TEXT: &str = "Refunds within thirty days.\n\nExchanges within sixty days.\n";

    /// (filename, content type, declared length, bytes received, input cap,
    /// asked timeout in ms).
    pub(super) type Seen = (String, String, u64, usize, u64, u32);

    /// What the real sidecar advertises by default.
    pub(super) fn capabilities(concurrency: u32) -> CapabilitiesResponse {
        CapabilitiesResponse {
            content_types: vec!["application/pdf".to_owned()],
            limits: Some(ExtractLimits {
                max_input_bytes: 50 << 20,
                max_pages: 1000,
                max_output_bytes: 20 << 20,
                timeout_millis: 120_000,
            }),
            version: "fake".to_owned(),
            concurrency,
            slot_wait_millis: 600_000,
            stream_idle_millis: 30_000,
            busy: 0,
            overruns: 0,
        }
    }

    pub(super) struct Fake {
        pub(super) seen: Mutex<Vec<Seen>>,
        pub(super) capabilities: CapabilitiesResponse,
        pub(super) capabilities_calls: AtomicUsize,
        in_flight: AtomicUsize,
        pub(super) max_in_flight: AtomicUsize,
        crash: Notify,
    }

    /// Lowers `in_flight` when a call ends, however it ends.
    struct InFlight<'a>(&'a Fake);

    impl Drop for InFlight<'_> {
        fn drop(&mut self) {
            self.0.in_flight.fetch_sub(1, Ordering::SeqCst);
        }
    }

    #[tonic::async_trait]
    impl DocumentExtractService for Fake {
        async fn extract(
            &self,
            request: Request<Streaming<ExtractRequest>>,
        ) -> Result<Response<ExtractResponse>, Status> {
            let now = self.in_flight.fetch_add(1, Ordering::SeqCst) + 1;
            self.max_in_flight.fetch_max(now, Ordering::SeqCst);
            let _in_flight = InFlight(self);
            let mut stream = request.into_inner();
            let Some(ExtractRequest {
                message: Some(extract_request::Message::Header(header)),
            }) = stream.message().await?
            else {
                return Err(Status::invalid_argument("header first"));
            };
            let mut received = 0usize;
            while let Some(message) = stream.message().await? {
                match message.message {
                    Some(extract_request::Message::Chunk(chunk)) => received += chunk.len(),
                    _ => return Err(Status::invalid_argument("chunks after the header")),
                }
            }
            self.seen.lock().expect("lock").push((
                header.filename.clone(),
                header.content_type.clone(),
                header.content_length,
                received,
                header.limits.map_or(0, |limits| limits.max_input_bytes),
                header.limits.map_or(0, |limits| limits.timeout_millis),
            ));
            let stem = header
                .filename
                .rsplit('/')
                .next()
                .and_then(|name| name.split('.').next())
                .unwrap_or_default()
                .to_owned();
            if stem == "crash" {
                self.crash.notify_one();
                std::future::pending::<()>().await;
            }
            if stem == "hang" {
                std::future::pending::<()>().await;
            }
            if stem == "fail" {
                return Err(Status::internal("scripted extractor failure"));
            }
            if stem == "slow" {
                tokio::time::sleep(Duration::from_millis(150)).await;
            }
            let outcome = match stem.strip_prefix("skip-") {
                Some(reason) => {
                    let reason = match reason {
                        "too_large" => SkipReason::TooLarge,
                        "too_many_pages" => SkipReason::TooManyPages,
                        "encrypted" => SkipReason::Encrypted,
                        "unsupported" => SkipReason::Unsupported,
                        "corrupt" => SkipReason::Corrupt,
                        "timeout" => SkipReason::Timeout,
                        "output_too_large" => SkipReason::OutputTooLarge,
                        "empty" => SkipReason::Empty,
                        other => panic!("unknown scripted reason {other}"),
                    };
                    extract_response::Outcome::Skip(ExtractSkip {
                        reason: reason as i32,
                        detail: format!("scripted {reason:?}"),
                    })
                }
                None => extract_response::Outcome::Text(ExtractedText {
                    text: TEXT.to_owned(),
                    extractor: "xberg".to_owned(),
                }),
            };
            Ok(Response::new(ExtractResponse {
                outcome: Some(outcome),
            }))
        }

        async fn capabilities(
            &self,
            _request: Request<CapabilitiesRequest>,
        ) -> Result<Response<CapabilitiesResponse>, Status> {
            self.capabilities_calls.fetch_add(1, Ordering::SeqCst);
            Ok(Response::new(self.capabilities.clone()))
        }
    }

    /// Serve a fake at `socket`, on a runtime of its own. That runtime is
    /// SHUT DOWN when a `crash` document arrives, which ends the accept loop
    /// AND every connection task (tonic spawns one per connection, so
    /// aborting the server task alone would leave the connections alive):
    /// the client's call fails with the connection, as a crashed sidecar's
    /// would.
    pub(super) fn serve(socket: &Path) -> Arc<Fake> {
        serve_with(socket, capabilities(4))
    }

    /// [`serve`] advertising `capabilities`.
    pub(super) fn serve_with(socket: &Path, capabilities: CapabilitiesResponse) -> Arc<Fake> {
        let fake = Arc::new(Fake {
            seen: Mutex::new(Vec::new()),
            capabilities,
            capabilities_calls: AtomicUsize::new(0),
            in_flight: AtomicUsize::new(0),
            max_in_flight: AtomicUsize::new(0),
            crash: Notify::new(),
        });
        let listener = bind(socket);
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(1)
            .enable_all()
            .build()
            .expect("fake runtime");
        let service = DocumentExtractServiceServer::from_arc(Arc::clone(&fake));
        runtime.spawn(async move {
            let listener = tokio::net::UnixListener::from_std(listener).expect("listener");
            let _ = tonic::transport::Server::builder()
                .add_service(service)
                .serve_with_incoming(tokio_stream::wrappers::UnixListenerStream::new(listener))
                .await;
        });
        let crashing = Arc::clone(&fake);
        tokio::spawn(async move {
            crashing.crash.notified().await;
            runtime.shutdown_background();
        });
        fake
    }

    /// What `elitea_engine_sidecar::bind` does; the worker does not link
    /// that crate. A std listener, handed to the fake's own runtime.
    fn bind(socket: &Path) -> std::os::unix::net::UnixListener {
        if let Some(parent) = socket.parent() {
            std::fs::create_dir_all(parent).expect("socket dir");
        }
        let _ = std::fs::remove_file(socket);
        let listener = std::os::unix::net::UnixListener::bind(socket).expect("bind");
        listener.set_nonblocking(true).expect("nonblocking");
        listener
    }
}

fn socket_path(name: &str) -> std::path::PathBuf {
    // Short: macOS caps a socket path at 104 bytes.
    std::env::temp_dir().join(format!("wx-{}-{name}.sock", std::process::id()))
}

fn with_extractor(mut harness: Harness, socket: &std::path::Path) -> Harness {
    let client = crate::transport::extractor_client::ExtractorClient::connect_lazy(socket)
        .expect("client")
        .with_deadline_margin(Duration::from_millis(200));
    harness.runtime.extractor = Some(Arc::new(client));
    harness
}

/// A document whose bytes are not text: what a connector fetched for a PDF
/// or an Office file. The fake extractor does not read them; the pipeline
/// must not try to either.
fn binary_document(key: &str, bytes: &[u8]) -> Document {
    let mut document = document(key, "v1", "");
    document.reference.size = bytes.len() as u64;
    document.bytes = bytes.to_vec();
    document
}

#[tokio::test(flavor = "multi_thread")]
async fn documents_go_to_the_extractor_and_are_indexed() {
    let socket = socket_path("ok");
    let fake = fake_extractor::serve(&socket);
    let harness = with_extractor(harness(4, None).await, &socket);
    let pdf = vec![0x25, 0x50, 0x44, 0x46, 0x2d, 0x31, 0x2e, 0x34, 0xff, 0xfe];
    // Larger than one streamed chunk (256 KiB): the bytes arrive whole.
    #[allow(clippy::cast_possible_truncation, reason = "a byte pattern")]
    let pptx: Vec<u8> = (0..300_000u32).map(|i| (i % 251) as u8).collect();
    let summary = run(
        &harness,
        vec![
            binary_document("docs/policy.pdf", &pdf),
            binary_document("decks/overview.pptx", &pptx),
            document("README.md", "v1", "# Title\n\nSome words.\n"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(summary.indexed_documents, 3);
    assert!(summary.skipped.is_empty(), "{:?}", summary.skipped);
    let mut seen = fake.seen.lock().expect("lock").clone();
    seen.sort();
    assert_eq!(
        seen,
        vec![
            (
                "decks/overview.pptx".to_owned(),
                "application/vnd.openxmlformats-officedocument.presentationml.presentation"
                    .to_owned(),
                pptx.len() as u64,
                pptx.len(),
                IndexRunLimits::default().max_document_bytes,
                120_000,
            ),
            (
                "docs/policy.pdf".to_owned(),
                "application/pdf".to_owned(),
                pdf.len() as u64,
                pdf.len(),
                IndexRunLimits::default().max_document_bytes,
                120_000,
            ),
        ],
        "the two documents, whole, with the run's document cap and extraction timeout; README.md stays local"
    );
    // Capabilities were asked once, before the first document, not per call.
    assert_eq!(fake.capabilities_calls.load(Ordering::SeqCst), 1);
    // The sidecar's text is what was chunked and written.
    let points = stored(&harness).await;
    assert!(points.iter().any(|p| p.document_key == "docs/policy.pdf"));
    let _ = std::fs::remove_file(&socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn extractor_skip_reasons_become_summary_labels() {
    let socket = socket_path("skips");
    let _fake = fake_extractor::serve(&socket);
    let harness = with_extractor(harness(4, None).await, &socket);
    let summary = run(
        &harness,
        vec![
            binary_document("skip-encrypted.pdf", b"x"),
            binary_document("skip-too_many_pages.pdf", b"x"),
            binary_document("skip-corrupt.docx", b"x"),
            binary_document("skip-too_large.pdf", b"x"),
            binary_document("skip-output_too_large.pdf", b"x"),
            binary_document("skip-timeout.pdf", b"x"),
            binary_document("skip-empty.xlsx", b"x"),
            binary_document("skip-unsupported.pdf", b"x"),
            document("kept.md", "v1", "# Kept\n\nIndexed as usual.\n"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1);
    let mut skipped: Vec<(&str, u64)> = summary
        .skipped
        .iter()
        .map(|(k, v)| (k.as_str(), *v))
        .collect();
    skipped.sort_unstable();
    assert_eq!(
        skipped,
        vec![
            ("empty", 1),
            ("encrypted", 1),
            ("extract_timeout", 1),
            ("too_large", 2),
            ("too_many_pages", 1),
            ("unreadable", 1),
            ("unsupported_type", 1),
        ]
    );
    let _ = std::fs::remove_file(&socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn an_extractor_crash_is_one_skipped_document_and_the_run_goes_on() {
    let socket = socket_path("crash");
    let _crashing = fake_extractor::serve(&socket);
    let harness = with_extractor(harness(4, None).await, &socket);
    let summary = run(
        &harness,
        vec![
            binary_document("crash.pdf", b"x"),
            document("kept.md", "v1", "# Kept\n\nIndexed as usual.\n"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(
        summary.skipped.get("extract_failed"),
        Some(&1),
        "{summary:?}"
    );
    let _ = std::fs::remove_file(&socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_reconnected_extractor_serves_the_documents_a_crash_skipped() {
    let socket = socket_path("reconnect");
    let _crashing = fake_extractor::serve(&socket);
    let harness = with_extractor(harness(4, None).await, &socket);
    let summary = run(
        &harness,
        vec![binary_document("crash.pdf", b"x")],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.skipped.get("extract_failed"),
        Some(&1),
        "{summary:?}"
    );
    // The sidecar is restarted on the same socket (a new container, the
    // same shared directory): the SAME client reaches it without any
    // reconfiguration, and a document is indexed again.
    let restarted = fake_extractor::serve(&socket);
    let summary = run(
        &harness,
        vec![binary_document("policy.pdf", b"x")],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert!(summary.skipped.is_empty(), "{summary:?}");
    assert_eq!(restarted.seen.lock().expect("lock").len(), 1);
    let _ = std::fs::remove_file(&socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn without_an_extractor_a_document_is_unsupported_and_without_a_socket_extract_failed() {
    // No sidecar configured: the pre-sidecar behaviour, byte for byte.
    let harness = harness(4, None).await;
    let summary = run(
        &harness,
        vec![binary_document("policy.pdf", b"x")],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.skipped.get("unsupported_type"),
        Some(&1),
        "{summary:?}"
    );
    // A sidecar configured but not running (nothing listens at the path):
    // the document is skipped `extract_failed`, the run goes on.
    let socket = socket_path("absent");
    let _ = std::fs::remove_file(&socket);
    let harness = with_extractor(harness, &socket);
    let summary = run(
        &harness,
        vec![
            binary_document("policy.pdf", b"x"),
            document("kept.md", "v1", "# Kept\n\nIndexed as usual.\n"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(
        summary.skipped.get("extract_failed"),
        Some(&1),
        "{summary:?}"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn in_flight_extractor_calls_are_capped_at_the_advertised_concurrency() {
    // The sidecar advertises ONE slot; the run prepares four documents at
    // once (document_concurrency 4 in `harness`). The client queues them
    // locally: the fake never sees two calls in flight, every document is
    // indexed, and nothing is `extract_failed`.
    let socket = socket_path("cap1");
    let fake = fake_extractor::serve_with(&socket, fake_extractor::capabilities(1));
    let harness = with_extractor(harness(4, None).await, &socket);
    let documents: Vec<Document> = (0..4)
        .map(|i| binary_document(&format!("slow-{i}/slow.pdf"), b"x"))
        .collect();
    let summary = run(
        &harness,
        documents,
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 4, "{summary:?}");
    assert!(summary.skipped.is_empty(), "{:?}", summary.skipped);
    assert_eq!(fake.max_in_flight.load(Ordering::SeqCst), 1);
    let sidecar = harness
        .runtime
        .extractor
        .as_ref()
        .expect("client")
        .sidecar_for_tests()
        .await
        .expect("learned");
    assert_eq!(sidecar.concurrency, 1);
    let _ = std::fs::remove_file(&socket);

    // Four slots advertised: the same four documents overlap at the fake.
    let socket = socket_path("cap4");
    let fake = fake_extractor::serve_with(&socket, fake_extractor::capabilities(4));
    let wide = with_extractor(self::harness(4, None).await, &socket);
    let documents: Vec<Document> = (0..4)
        .map(|i| binary_document(&format!("slow-{i}/slow.pdf"), b"x"))
        .collect();
    let summary = run(
        &wide,
        documents,
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 4, "{summary:?}");
    assert!(fake.max_in_flight.load(Ordering::SeqCst) > 1);
    assert!(fake.max_in_flight.load(Ordering::SeqCst) <= 4);
    let _ = std::fs::remove_file(&socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_failure_mid_run_does_not_widen_the_in_flight_cap() {
    // The sidecar advertises TWO slots; the run prepares three documents at
    // once. One document fails at the sidecar (INTERNAL; the server stays
    // up), which makes the client forget what it learned and ask again on
    // the next document while two slow calls still hold their permits.
    // The permits are one pool for the client's life: the re-learned size
    // adds nothing next to the live permits, and the fake never sees three
    // calls in flight. (A pool replaced on re-learning would have handed
    // out two fresh permits beside the two still held: up to four.)
    let socket = socket_path("relearn");
    let fake = fake_extractor::serve_with(&socket, fake_extractor::capabilities(2));
    let mut harness = with_extractor(harness(4, None).await, &socket);
    harness.runtime.limits.document_concurrency = 3;
    let documents: Vec<Document> = vec![
        binary_document("slow-0/slow.pdf", b"x"),
        binary_document("fail-0/fail.pdf", b"x"),
        binary_document("slow-1/slow.pdf", b"x"),
        binary_document("slow-2/slow.pdf", b"x"),
        binary_document("fail-1/fail.pdf", b"x"),
        binary_document("slow-3/slow.pdf", b"x"),
        binary_document("slow-4/slow.pdf", b"x"),
        binary_document("slow-5/slow.pdf", b"x"),
    ];
    let summary = run(
        &harness,
        documents,
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 6, "{summary:?}");
    assert_eq!(
        summary.skipped.get("extract_failed"),
        Some(&2),
        "{summary:?}"
    );
    assert!(
        fake.capabilities_calls.load(Ordering::SeqCst) >= 2,
        "the client asked again after a failure: {} call(s)",
        fake.capabilities_calls.load(Ordering::SeqCst)
    );
    assert!(
        fake.max_in_flight.load(Ordering::SeqCst) <= 2,
        "never more in flight than advertised: {}",
        fake.max_in_flight.load(Ordering::SeqCst)
    );
    let client = harness.runtime.extractor.as_ref().expect("client");
    assert_eq!(
        client.free_slots_for_tests(),
        2,
        "every permit came back to the one pool"
    );
    let _ = std::fs::remove_file(&socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn the_call_deadline_and_the_asked_timeout_follow_the_sidecars_capabilities() {
    // A sidecar advertising a 200 ms extraction cap, a 100 ms slot wait and
    // a 100 ms idle limit; the client's test margin is 200 ms. A document
    // the fake never answers must be given up at 200+100+100+200 = 600 ms
    // (not the old fixed five minutes) and recorded `extract_failed`; the
    // header must have asked for 200 ms (the run's default 120 s lowered to
    // the cap).
    let socket = socket_path("deadline");
    let mut caps = fake_extractor::capabilities(4);
    caps.limits.as_mut().expect("limits").timeout_millis = 200;
    caps.slot_wait_millis = 100;
    caps.stream_idle_millis = 100;
    let fake = fake_extractor::serve_with(&socket, caps);
    let harness = with_extractor(harness(4, None).await, &socket);
    let started = std::time::Instant::now();
    let summary = run(
        &harness,
        vec![
            binary_document("hang.pdf", b"x"),
            document("kept.md", "v1", "# Kept\n\nIndexed as usual.\n"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    let elapsed = started.elapsed();
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(
        summary.skipped.get("extract_failed"),
        Some(&1),
        "{summary:?}"
    );
    assert!(
        elapsed >= Duration::from_millis(550) && elapsed < Duration::from_secs(10),
        "gave up after {elapsed:?}, the derived deadline is 600 ms"
    );
    let seen = fake.seen.lock().expect("lock").clone();
    assert_eq!(seen.len(), 1, "{seen:?}");
    assert_eq!(
        seen[0].5, 200,
        "the asked timeout is the sidecar's cap: {seen:?}"
    );
    // After a failure the capabilities are asked again on the next document.
    let summary = run(
        &harness,
        vec![binary_document("policy.pdf", b"x")],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(fake.capabilities_calls.load(Ordering::SeqCst), 2);
    let _ = std::fs::remove_file(&socket);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_run_asking_less_extraction_time_than_the_cap_sends_its_own_figure() {
    let socket = socket_path("asked");
    let fake = fake_extractor::serve(&socket);
    let mut harness = with_extractor(harness(4, None).await, &socket);
    harness.runtime.limits.extract_timeout = Duration::from_secs(5);
    let summary = run(
        &harness,
        vec![binary_document("policy.pdf", b"x")],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    let seen = fake.seen.lock().expect("lock").clone();
    assert_eq!(seen[0].5, 5_000, "{seen:?}");
    let _ = std::fs::remove_file(&socket);
}
