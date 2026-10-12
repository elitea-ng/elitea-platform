//! The index run against the in-process fake of `elitea-vector`
//! (`elitea_vector_index::testing`), a scripted embeddings gateway and a
//! fixture source: first run, incremental run (unchanged, changed, vanished),
//! clean run, the embedding-space fence, Stop, and the bounded record.

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
use super::{IndexIngestRuntime, IndexRun, IndexRunFailure, IndexRunLimits, run_index};
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
/// 4-dimensional vector per input (its byte sum), and counts the calls.
struct ScriptedEmbeddings {
    calls: AtomicUsize,
    dimension: usize,
    fail_with: Option<StatusCode>,
}

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
        if let Some(status) = self.fail_with {
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

struct Harness {
    runtime: IndexIngestRuntime,
    fake: FakeVector,
    embeddings: Arc<ScriptedEmbeddings>,
    token: VectorClaimToken,
    context: ClaimScopedEliteaContext,
    _server: tokio::task::JoinHandle<()>,
}

async fn harness(dimension: usize, fail_with: Option<StatusCode>) -> Harness {
    let fake = FakeVector::new(&[(TOKEN, 1)]);
    let (address, server) = fake.clone().serve().await.expect("serve");
    let mut config = ClientConfig::new(format!("http://{address}"));
    config.allow_insecure = true;
    let vector = VectorClient::connect_lazy(&config).expect("client");
    let embeddings = Arc::new(ScriptedEmbeddings {
        calls: AtomicUsize::new(0),
        dimension,
        fail_with,
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
            deadline_unix_millis: i64::MAX,
            now_unix_millis: &now,
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
    assert_eq!(summary.total_documents, 2);
    assert_eq!(summary.total_chunks, summary.indexed_chunks);
    assert_eq!(summary.reindex, Some(false));
    assert!(!summary.documents_truncated);
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
    assert_eq!(first.total_documents, 3);
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
    assert_eq!(second.total_documents, 2);
    assert_eq!(second.reindex, Some(true));
    assert_eq!(second.documents.len(), 1);
    assert_eq!(second.documents[0].document_key, "b.md");
    assert_eq!(second.documents[0].version, "v2");
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
    assert_eq!(clean.total_documents, 1);
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

#[tokio::test(flavor = "multi_thread")]
async fn a_gateway_refusal_marks_the_document_failed_and_the_run_partly_indexed() {
    let harness = harness(4, Some(StatusCode::FORBIDDEN)).await;
    let summary = run(
        &harness,
        vec![document("a.md", "v1", "alpha")],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.status, IndexIngestStatusV1::PartlyIndexed as i32);
    assert!(summary.failed_chunks >= 1);
    assert_eq!(summary.skipped.get("embedding_failed"), Some(&1));
    assert_eq!(summary.indexed_documents, 0);
    assert!(summary.embedding_model.is_empty());
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
    assert_eq!(needs_model.total_documents, 0);
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

#[test]
fn a_failure_summary_names_the_index_and_is_bounded() {
    let summary = failure_for_test("docs", &"x".repeat(20_000));
    assert_eq!(summary.status, IndexIngestStatusV1::Error as i32);
    assert!(summary.message.starts_with("Index 'docs' failed: "));
    assert!(summary.message.len() <= 8 * 1024);
    assert!(summary.embedding_model.is_empty());
    assert_eq!(summary.indexed_documents, 0);
}
