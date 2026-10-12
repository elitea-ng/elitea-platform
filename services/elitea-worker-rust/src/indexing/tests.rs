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

use super::pipeline::{RunLiveness, failure_for_test, record_for_test};
use super::source::IndexSource;
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

const NS: &str = "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b";
const TOKEN: &str = "elvc_0123456789abcdef0123456789abcdef0123456789a";
const MODEL: &str = "text-embedding-3-small";

/// A gateway that answers every embeddings request with a deterministic
/// vector per input (its byte sum), and counts the calls. `fail_with` makes
/// it answer with that status instead: for every request, or only for a
/// request whose inputs contain `fail_marker`.
struct ScriptedEmbeddings {
    calls: AtomicUsize,
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

struct Always;

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

struct Harness {
    runtime: IndexIngestRuntime,
    fake: FakeVector,
    embeddings: Arc<ScriptedEmbeddings>,
    token: VectorClaimToken,
    context: ClaimScopedEliteaContext,
    _server: tokio::task::JoinHandle<()>,
}

async fn harness(dimension: usize, fail_with: Option<StatusCode>) -> Harness {
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
        vector,
        embeddings: EmbeddingsClient::with_transport(
            Arc::clone(&embeddings) as Arc<dyn ModelGatewayTransport>,
            Duration::from_secs(5),
        ),
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

fn document(key: &str, version: &str, text: &str) -> Document {
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

fn command(index_id: &str, expected: Option<(&str, u32)>) -> IndexIngestCommandV1 {
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

fn configuration() -> Vec<u8> {
    json!({"id": 7, "type": "fixture", "toolkit_name": "repo", "settings": {}})
        .to_string()
        .into_bytes()
}

fn parameters(clean: bool, extra: Value) -> Vec<u8> {
    let mut map = json!({"index_name": "docs", "clean_index": clean});
    if let Value::Object(extra) = extra {
        for (key, value) in extra {
            map[key] = value;
        }
    }
    map.to_string().into_bytes()
}

fn binding() -> Vec<u8> {
    json!({"schema_version": "v2", "model_name": MODEL, "resolved_model_group": MODEL, "route": "raw"})
        .to_string()
        .into_bytes()
}

/// Runs the pipeline against `documents` with the fixture source swapped in
/// for the toolkit's connector.
async fn run(
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
            embedding_binding: Some(&binding()),
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

pub(super) fn take_fixture_source() -> Option<IndexSource> {
    FIXTURE_SOURCE.with(|slot| slot.lock().expect("lock").take().map(IndexSource::Fixture))
}

async fn stored(harness: &Harness) -> Vec<pb::IndexedDocument> {
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
            document("logo.png", "v1", "\u{89}PNG"),
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
    assert_eq!(summary.indexed_documents, 2);
    assert_eq!(summary.updated, 0);
    assert_eq!(summary.skipped.get("unsupported_type"), Some(&1));
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
        &parameters(false, json!({})),
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

    let needs_model = run(
        &harness,
        vec![document("d.md", "v1", "delta")],
        &command(NS, None),
        &parameters(false, json!({"chunking_tool": "statistical"})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(needs_model.skipped.get("needs_model"), Some(&1));
    assert_eq!(needs_model.indexed_documents, 0);
    // The earlier document is no longer listed by the source, so it is gone.
    assert_eq!(needs_model.removed_document_keys, vec!["a.md"]);
    assert_eq!(needs_model.total_documents, Some(0));
    assert!(needs_model.whole_index_record);
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
            embedding_binding: Some(&binding()),
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
            embedding_binding: Some(&binding()),
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
            embedding_binding: Some(&binding()),
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
            embedding_binding: None,
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
