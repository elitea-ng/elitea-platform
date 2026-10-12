//! The index tools' claim host over the in-process fake of `elitea-vector`
//! (`elitea_vector_index::testing`) and a scripted embeddings gateway: the
//! five tools' results, the token fence, the step-back refusal without a
//! model, `remove_index` asking Main over the control plane (never the
//! store), the non-streaming step-back model call, and the two
//! indexing-runtime modes as the direct tool runtime sees them — a toolkit
//! frozen WITH Main's `indexes` catalog (Rust indexing) serves the index
//! tools, one frozen without it (Python indexing) does not.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::futures::stream;
use adk_rust::{Content, Llm, LlmRequest, LlmResponse, LlmResponseStream};
use async_trait::async_trait;
use elitea_agent_runtime::toolkits::{
    IndexCatalog, IndexCatalogEntry, IndexToolAuthority, IndexToolCall, IndexToolHost,
    IndexToolName, IndexToolSupport, IndexToolsWithheld,
};
use elitea_vector_index::client::toolkit_namespace;
use elitea_vector_index::testing::FakeVector;
use elitea_vector_index::{ClientConfig, VectorClient, pb};
use http::{Request, Response, StatusCode, Version};
use http_body_util::{BodyExt as _, Full};
use serde_json::{Value, json};
use tonic::body::Body;

use super::tools::{ClaimIndexToolHost, StepbackBinding};
use super::{IndexIngestRuntime, IndexRunLimits};
use crate::protocol::control::{
    AgentControlError, ControlSemanticError, IndexRemovalBinding, IndexRemovalControl,
    IndexRemovalOutcome,
};
use crate::protocol::elitea::runtime::v1::{RemoveIndexRequestV1, VectorClaimTokenV1};
use crate::protocol::vector_token::VectorClaimToken;
use crate::toolkits::{DirectToolkitRequest, DirectToolkitRuntime, DirectToolkitRuntimeErrorCode};
use crate::transport::embeddings_facade::EmbeddingsClient;
use crate::transport::openai_compatible_facade::{
    ModelGatewayTransport, ModelGatewayTransportError,
};
use crate::transport::runtime_context::ClaimScopedEliteaContext;

const NS: &str = "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b";
const TOKEN: &str = "elvc_0123456789abcdef0123456789abcdef0123456789a";
const MODEL: &str = "text-embedding-3-small";
const DIMENSION: u32 = 4;

/// Answers every embeddings request with the unit vector along the first
/// axis, so a stored point along that axis scores 1 and one along another
/// axis scores 0 (under the default cut-off).
struct AxisEmbeddings;

#[async_trait]
impl ModelGatewayTransport for AxisEmbeddings {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, ModelGatewayTransportError> {
        assert_eq!(request.uri().path(), "/llm/v1/embeddings");
        let body = request
            .into_body()
            .collect()
            .await
            .map_err(|_| ModelGatewayTransportError::Unavailable)?
            .to_bytes();
        let parsed: Value = serde_json::from_slice(&body).expect("json body");
        assert_eq!(
            parsed["model"], MODEL,
            "the query is embedded with the index's model"
        );
        let inputs = parsed["input"].as_array().expect("inputs");
        let data: Vec<Value> = inputs
            .iter()
            .enumerate()
            .map(|(index, _)| json!({"object": "embedding", "index": index, "embedding": [1.0, 0.0, 0.0, 0.0]}))
            .collect();
        let reply = json!({"object": "list", "model": MODEL, "data": data});
        Ok(Response::builder()
            .status(StatusCode::OK)
            .version(Version::HTTP_2)
            .header("content-type", "application/json")
            .body(Body::new(Full::new(bytes::Bytes::from(reply.to_string()))))
            .expect("response"))
    }
}

struct Harness {
    runtime: Arc<IndexIngestRuntime>,
    fake: FakeVector,
    token: VectorClaimToken,
    removal: Option<Arc<RecordingRemoval>>,
    _server: tokio::task::JoinHandle<()>,
}

/// Main's `RemoveIndex`, as the worker sees it: records every request and
/// answers the scripted outcome (or a rejection).
struct RecordingRemoval {
    requests: Mutex<Vec<RemoveIndexRequestV1>>,
    outcome: Mutex<Result<IndexRemovalOutcome, &'static str>>,
}

impl RecordingRemoval {
    fn new(outcome: IndexRemovalOutcome) -> Arc<Self> {
        Arc::new(Self {
            requests: Mutex::new(Vec::new()),
            outcome: Mutex::new(Ok(outcome)),
        })
    }

    fn answer(&self, outcome: Result<IndexRemovalOutcome, &'static str>) {
        *self.outcome.lock().expect("outcome") = outcome;
    }

    fn requests(&self) -> Vec<RemoveIndexRequestV1> {
        self.requests.lock().expect("requests").clone()
    }
}

#[async_trait]
impl IndexRemovalControl for RecordingRemoval {
    async fn remove_index(
        &self,
        request: RemoveIndexRequestV1,
    ) -> Result<IndexRemovalOutcome, AgentControlError> {
        self.requests.lock().expect("requests").push(request);
        match *self.outcome.lock().expect("outcome") {
            Ok(outcome) => Ok(outcome),
            Err(message) => Err(AgentControlError::Semantic(
                ControlSemanticError::DependencyUnavailable(message),
            )),
        }
    }
}

async fn harness() -> Harness {
    harness_with_removal(Some(RecordingRemoval::new(IndexRemovalOutcome::Removed))).await
}

async fn harness_with_removal(removal: Option<Arc<RecordingRemoval>>) -> Harness {
    let fake = FakeVector::new(&[(TOKEN, 1)]);
    let (address, server) = fake.clone().serve().await.expect("serve");
    let mut config = ClientConfig::new(format!("http://{address}"));
    config.allow_insecure = true;
    let vector = VectorClient::connect_lazy(&config).expect("client");
    let runtime = IndexIngestRuntime {
        vector,
        embeddings: EmbeddingsClient::with_transport(
            Arc::new(AxisEmbeddings) as Arc<dyn ModelGatewayTransport>,
            Duration::from_secs(5),
        ),
        removal: removal
            .clone()
            .map(|removal| removal as Arc<dyn IndexRemovalControl>),
        platform_origin: "https://platform.internal".to_owned(),
        allowlist: elitea_connectors::egress::HostAllowlist::parse(Some("*")),
        transport: Arc::new(
            elitea_connectors::reqwest012::ReqwestTransport::hardened(
                &elitea_connectors::github::client::CLIENT_POLICY,
            )
            .expect("transport"),
        ),
        limits: IndexRunLimits::default(),
    };
    Harness {
        runtime: Arc::new(runtime),
        fake,
        token: token(&["toolkit_index"]),
        removal,
        _server: server,
    }
}

fn token(sources: &[&str]) -> VectorClaimToken {
    VectorClaimToken::from_receipt(Some(VectorClaimTokenV1 {
        bearer: TOKEN.to_owned(),
        expires_at_unix_millis: i64::MAX,
        allowed_sources: sources.iter().map(ToString::to_string).collect(),
    }))
    .expect("token")
}

fn space() -> pb::EmbeddingSpace {
    pb::EmbeddingSpace {
        model_slug: super::slug::vector_space_slug(MODEL),
        dimension: DIMENSION,
    }
}

fn catalog() -> IndexCatalog {
    IndexCatalog::new(vec![IndexCatalogEntry {
        name: "docs".to_owned(),
        index_id: NS.to_owned(),
        embedding_model: MODEL.to_owned(),
        embedding_dimension: DIMENSION,
    }])
}

fn point(key: &str, text: &str, vector: [f32; 4]) -> pb::Point {
    pb::Point {
        document_key: key.to_owned(),
        document_version: "v1".to_owned(),
        parent_id: String::new(),
        chunk_id: "1".to_owned(),
        chunk_type: "document".to_owned(),
        acl: Vec::new(),
        text: text.to_owned(),
        metadata_json: json!({"source": format!("{key}.md"), "collection": "docs"}).to_string(),
        vector: vector.to_vec(),
    }
}

async fn seed(harness: &Harness) {
    harness
        .runtime
        .vector
        .upsert(
            &harness.token,
            space(),
            toolkit_namespace(NS),
            vec![
                point("alpha", "alpha text", [1.0, 0.0, 0.0, 0.0]),
                point("beta", "beta text", [0.0, 1.0, 0.0, 0.0]),
            ],
        )
        .await
        .expect("seeded");
    assert_eq!(harness.fake.point_count(), 2);
}

fn host(harness: &Harness, token: &VectorClaimToken) -> ClaimIndexToolHost {
    host_with_model(harness, token, None)
}

fn host_with_model(
    harness: &Harness,
    token: &VectorClaimToken,
    model: Option<StepbackBinding>,
) -> ClaimIndexToolHost {
    ClaimIndexToolHost::new(
        Arc::clone(&harness.runtime),
        token,
        Arc::new(ClaimScopedEliteaContext::fixture(1, "pat")),
        model,
        IndexRemovalBinding::fixture(),
    )
}

const TOOLKIT_ID: u64 = 41;

async fn call(host: &ClaimIndexToolHost, tool: IndexToolName, arguments: Value) -> Value {
    let catalog = catalog();
    match host
        .call(IndexToolCall {
            tool,
            toolkit_id: TOOLKIT_ID,
            catalog: &catalog,
            doctype: "document",
            arguments: &arguments,
        })
        .await
    {
        Ok(value) => value,
        Err(failure) => Value::String(format!("FAILED: {}", failure.message())),
    }
}

#[tokio::test]
#[allow(
    clippy::too_many_lines,
    reason = "one seeded store, the five tools in turn"
)]
async fn the_five_tools_answer_in_the_sdk_shapes_over_the_fake_store() {
    let harness = harness().await;
    seed(&harness).await;
    let host = host(&harness, &harness.token);

    assert_eq!(
        call(&host, IndexToolName::ListIndexes, json!({})).await,
        json!("docs")
    );

    let found = call(
        &host,
        IndexToolName::SearchIndex,
        json!({"query": "alpha", "index_name": "docs"}),
    )
    .await;
    let docs = found.as_array().expect("a result list");
    assert_eq!(
        docs.len(),
        1,
        "beta scores 0 and sits under the cut-off: {found}"
    );
    assert_eq!(docs[0]["page_content"], "alpha text");
    assert_eq!(docs[0]["metadata"]["source"], "alpha.md");
    assert!(docs[0]["score"].as_f64().expect("score") > 0.99);
    // Every index, with the cut-off off: both documents.
    let both = call(
        &host,
        IndexToolName::SearchIndex,
        json!({"query": "anything", "cut_off": 0, "output_fields": ["page_content"]}),
    )
    .await;
    assert_eq!(both.as_array().expect("list").len(), 2);
    assert_eq!(both[0], json!({"page_content": "alpha text"}));

    let unknown = call(
        &host,
        IndexToolName::SearchIndex,
        json!({"query": "alpha", "index_name": "nope"}),
    )
    .await;
    assert!(
        unknown
            .as_str()
            .is_some_and(|text| text.starts_with("Index 'nope' not found")),
        "{unknown}"
    );
    let invalid = call(
        &host,
        IndexToolName::SearchIndex,
        json!({"query": "alpha", "cut_off": 7}),
    )
    .await;
    assert!(
        invalid
            .as_str()
            .is_some_and(|text| text.contains("cut_off must be between 0 and 1")),
        "{invalid}"
    );

    // No model bound: the step-back pair refuse with a sentence that names
    // the alternative, and the store is not touched.
    let calls_before = harness.fake.calls().len();
    for tool in [
        IndexToolName::StepbackSearchIndex,
        IndexToolName::StepbackSummaryIndex,
    ] {
        let refused = call(&host, tool, json!({"query": "alpha"})).await;
        let text = refused.as_str().expect("text");
        assert!(text.starts_with("FAILED: "), "{text}");
        assert!(
            text.contains(tool.as_str()) && text.contains("search_index"),
            "{text}"
        );
    }
    assert_eq!(harness.fake.calls().len(), calls_before);

    // remove_index: an empty name is refused, a wrong one names the
    // available indexes (both without asking Main), the right one asks Main
    // under the claim — the frozen toolkit id, the name and the catalog's
    // registry id — and the worker deletes nothing from the store itself
    // (review round 1, finding 1).
    let removal = harness.removal.as_ref().expect("removal control");
    let empty = call(&host, IndexToolName::RemoveIndex, json!({})).await;
    assert!(
        empty
            .as_str()
            .is_some_and(|text| text.contains("needs an index name")),
        "{empty}"
    );
    let missing = call(
        &host,
        IndexToolName::RemoveIndex,
        json!({"index_name": "nope"}),
    )
    .await;
    assert!(
        missing
            .as_str()
            .is_some_and(|text| text.contains("Available indexes: docs")),
        "{missing}"
    );
    assert!(
        removal.requests().is_empty(),
        "no round trip for a bad name"
    );
    assert_eq!(harness.fake.point_count(), 2);
    let store_calls_before = harness.fake.calls().len();
    let removed = call(
        &host,
        IndexToolName::RemoveIndex,
        json!({"index_name": "docs"}),
    )
    .await;
    assert_eq!(
        removed,
        json!(
            "Index 'docs' has been removed from the vector store.\nAvailable indexes: No indexed collections"
        )
    );
    let requests = removal.requests();
    assert_eq!(requests.len(), 1);
    assert_eq!(requests[0].toolkit_id, 41);
    assert_eq!(requests[0].index_name, "docs");
    assert_eq!(requests[0].index_id, NS);
    assert_eq!(requests[0].claim_id, "claim-1");
    assert_eq!(
        requests[0]
            .identity
            .as_ref()
            .map(|identity| identity.execution_id.as_str()),
        Some("execution-one")
    );
    assert!(requests[0].fence.is_some());
    assert_eq!(
        harness.fake.point_count(),
        2,
        "the worker never deletes the points; Main's flow or its sweeper does"
    );
    assert_eq!(
        harness.fake.calls().len(),
        store_calls_before,
        "remove_index makes no vector-store call: {:?}",
        harness.fake.calls()
    );
    // Every call carried the claim's bearer and nothing else.
    let bearers = harness.fake.bearers();
    assert!(!bearers.is_empty());
    assert!(
        bearers
            .iter()
            .all(|bearer| bearer.trim_start_matches("Bearer ") == TOKEN),
        "{bearers:?}"
    );
}

#[tokio::test]
async fn a_token_without_the_toolkit_index_source_reaches_no_store() {
    let harness = harness().await;
    seed(&harness).await;
    let host = host(&harness, &token(&["deepwiki"]));
    let calls_before = harness.fake.calls().len();
    let refused = call(&host, IndexToolName::SearchIndex, json!({"query": "alpha"})).await;
    assert!(
        refused
            .as_str()
            .is_some_and(|text| text.contains("no project token for toolkit indexes")),
        "{refused}"
    );
    assert_eq!(harness.fake.calls().len(), calls_before);
}

fn github_settings(indexes: Option<Value>) -> Vec<u8> {
    toolkit_settings("github", "repo", indexes, &["get_me", "list_indexes"])
}

fn toolkit_settings(
    toolkit_type: &str,
    toolkit_name: &str,
    indexes: Option<Value>,
    selected: &[&str],
) -> Vec<u8> {
    let settings = match toolkit_type {
        "github" => json!({
            "github_configuration": {
                "configuration_type": "github",
                "base_url": "https://github.example.test/api/v3/",
                "access_token": "fixture-token"
            },
            "repository": "https://github.example.test/EliteaAI/elitea-platform.git",
            "active_branch": "main",
            "base_branch": "main",
            "selected_tools": selected,
            "pgvector_configuration": null,
            "embedding_model": null
        }),
        "artifact" => json!({"bucket": "docs", "selected_tools": selected}),
        other => panic!("no fixture for {other}"),
    };
    let mut toolkit = json!({
        "id": TOOLKIT_ID,
        "type": toolkit_type,
        "toolkit_name": toolkit_name,
        "settings": settings
    });
    if let Some(indexes) = indexes {
        toolkit["indexes"] = indexes;
    }
    serde_json::to_vec(&toolkit).expect("settings")
}

fn frozen_catalog() -> Value {
    json!([{"name": "docs", "index_id": NS, "embedding_model": MODEL, "embedding_dimension": DIMENSION}])
}

async fn call_directly(
    toolkit_type: &str,
    tool_name: &str,
    settings: &[u8],
    index_tools: IndexToolSupport<'_>,
) -> Result<Value, DirectToolkitRuntimeErrorCode> {
    call_directly_with(toolkit_type, tool_name, settings, &json!({}), index_tools).await
}

async fn call_directly_with(
    toolkit_type: &str,
    tool_name: &str,
    settings: &[u8],
    arguments: &Value,
    index_tools: IndexToolSupport<'_>,
) -> Result<Value, DirectToolkitRuntimeErrorCode> {
    let request = DirectToolkitRequest::parse_call(
        toolkit_type,
        &TOOLKIT_ID.to_string(),
        tool_name,
        settings,
        &serde_json::to_vec(arguments).expect("arguments"),
        br#"{"toolkit_security":{}}"#,
    )
    .expect("request");
    DirectToolkitRuntime::default()
        .execute_test(&request, "execution", "call", index_tools)
        .await
        .map_err(|error| error.code())
}

async fn list_indexes_directly(
    settings: &[u8],
    index_tools: IndexToolSupport<'_>,
) -> Result<Value, DirectToolkitRuntimeErrorCode> {
    call_directly("github", "list_indexes", settings, index_tools).await
}

/// `toolkit.call_tool.v1`: the index tools exist only where Main froze a
/// catalog (Rust indexing) AND this worker lends its vector store.
#[tokio::test]
async fn the_direct_runtime_serves_index_tools_only_under_rust_indexing() {
    let harness = harness().await;
    let authority = IndexToolAuthority::new(Arc::new(host(&harness, &harness.token)));

    // Rust indexing, vector store lent: the tool runs against the store.
    let listed = list_indexes_directly(
        &github_settings(Some(frozen_catalog())),
        IndexToolSupport::Lent(&authority),
    )
    .await
    .expect("listed");
    assert_eq!(listed, json!("docs"));

    // Python indexing: Main wrote no catalog, the SDK worker owns the tool,
    // and this worker refuses it as not selected — whatever it could lend.
    assert_eq!(
        list_indexes_directly(&github_settings(None), IndexToolSupport::Lent(&authority)).await,
        Err(DirectToolkitRuntimeErrorCode::ToolNotSelected)
    );

    // Rust indexing on a worker without a vector store: the authority is
    // withheld with that reason, the tool is left out (and warned about)
    // rather than bound to fail.
    assert_eq!(
        list_indexes_directly(
            &github_settings(Some(frozen_catalog())),
            IndexToolSupport::Withheld(IndexToolsWithheld::NoVectorStore),
        )
        .await,
        Err(DirectToolkitRuntimeErrorCode::ToolNotSelected)
    );
    assert!(
        harness
            .fake
            .calls()
            .iter()
            .all(|call| *call == "list_indexes" || *call == "search" || *call == "delete"),
        "only the Rust-mode call reached the store: {:?}",
        harness.fake.calls()
    );
}

/// Review round 1, finding 4: the artifact family is skipped on
/// `toolkit.call_tool.v1` (no artifact authority there, #906), but an
/// artifact toolkit's index tools are served when Main froze a catalog.
#[tokio::test]
async fn the_direct_runtime_serves_an_artifact_toolkits_index_tools() {
    let harness = harness().await;
    seed(&harness).await;
    let authority = IndexToolAuthority::new(Arc::new(host(&harness, &harness.token)));
    let settings = toolkit_settings("artifact", "files", Some(frozen_catalog()), &[]);
    let listed = call_directly(
        "artifact",
        "list_indexes",
        &settings,
        IndexToolSupport::Lent(&authority),
    )
    .await
    .expect("listed");
    assert_eq!(listed, json!("docs"));
    let found = call_directly_with(
        "artifact",
        "search_index",
        &settings,
        &json!({"query": "anything", "cut_off": 0}),
        IndexToolSupport::Lent(&authority),
    )
    .await
    .expect("searched");
    // The artifact family's doctype is `document`: plain dicts.
    assert_eq!(found.as_array().expect("hits").len(), 2);
    // The family's own tools stay unserved on this path.
    assert_eq!(
        call_directly(
            "artifact",
            "list_files",
            &settings,
            IndexToolSupport::Lent(&authority),
        )
        .await,
        Err(DirectToolkitRuntimeErrorCode::ToolNotSelected)
    );
    // And without a catalog the artifact toolkit is what it was: unsupported
    // here.
    assert_eq!(
        call_directly(
            "artifact",
            "list_indexes",
            &toolkit_settings("artifact", "files", None, &[]),
            IndexToolSupport::Lent(&authority),
        )
        .await,
        Err(DirectToolkitRuntimeErrorCode::UnsupportedToolkit)
    );
}

/// Review round 1, finding 2: the claim context (a platform round trip) is
/// redeemed for a `toolkit.call_tool.v1` run only when the call names an
/// index tool of a toolkit carrying a catalog. The processor gates the
/// redemption on exactly this predicate.
#[test]
fn only_an_index_tool_call_on_a_catalog_toolkit_asks_for_the_claim_context() {
    let parse = |tool: &str, settings: &[u8]| {
        DirectToolkitRequest::parse_call(
            "github",
            &TOOLKIT_ID.to_string(),
            tool,
            settings,
            b"{}",
            br#"{"toolkit_security":{}}"#,
        )
        .expect("request")
    };
    let with_catalog = toolkit_settings(
        "github",
        "repo",
        Some(frozen_catalog()),
        &["read_file", "list_indexes", "search_index"],
    );
    let without = toolkit_settings("github", "repo", None, &["read_file", "list_indexes"]);
    // github read_file under rust indexing: the catalog is there, the tool
    // is not an index tool, no context fetch.
    let read_file = parse("read_file", &with_catalog);
    assert!(read_file.carries_index_catalog());
    assert!(!read_file.calls_index_tool());
    for tool in ["list_indexes", "search_index", "remove_index"] {
        assert!(parse(tool, &with_catalog).calls_index_tool(), "{tool}");
    }
    // Python indexing: no catalog, no fetch, whatever the tool is called.
    assert!(!parse("list_indexes", &without).calls_index_tool());
}

/// Main's other answers to `remove_index` become sentences, and a Main that
/// fails leaves the model told that nothing was deleted; a worker without a
/// control plane refuses up front.
#[tokio::test]
async fn remove_index_renders_mains_decisions_as_sentences() {
    let harness = harness().await;
    seed(&harness).await;
    let host = host(&harness, &harness.token);
    let removal = harness.removal.as_ref().expect("removal control");
    for (outcome, expected) in [
        (Ok(IndexRemovalOutcome::NotFound), "no longer exists"),
        (
            Ok(IndexRemovalOutcome::Forbidden),
            "not allowed to remove index 'docs'",
        ),
        (Ok(IndexRemovalOutcome::ActiveRun), "still being indexed"),
        (Err("main is down"), "nothing was deleted"),
    ] {
        removal.answer(outcome);
        let text = call(
            &host,
            IndexToolName::RemoveIndex,
            json!({"index_name": "docs"}),
        )
        .await;
        let text = text.as_str().expect("text");
        assert!(text.starts_with("FAILED: "), "{text}");
        assert!(text.contains(expected), "{text}");
        assert!(
            !text.contains("main is down"),
            "Main's error text does not reach the model: {text}"
        );
    }
    assert_eq!(removal.requests().len(), 4);
    assert_eq!(harness.fake.point_count(), 2);

    let without = harness_with_removal(None).await;
    seed(&without).await;
    let unwired = self::host(&without, &without.token);
    let refused = call(
        &unwired,
        IndexToolName::RemoveIndex,
        json!({"index_name": "docs"}),
    )
    .await;
    assert!(
        refused
            .as_str()
            .is_some_and(|text| text.contains("no control plane")),
        "{refused}"
    );
    assert_eq!(without.fake.point_count(), 2);
}

/// A chat model that answers a streaming call the way the facades do — text
/// deltas, then a final frame carrying the WHOLE text — and a non-streaming
/// call with one complete frame. Collecting every frame of the streaming
/// shape doubles the text; the step-back model must ask non-streaming
/// (review round 1, finding 7).
struct FramedModel {
    streamed: AtomicBool,
}

#[async_trait]
impl Llm for FramedModel {
    fn name(&self) -> &'static str {
        "framed-fixture"
    }

    async fn generate_content(
        &self,
        _request: LlmRequest,
        streaming: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        if streaming {
            self.streamed.store(true, Ordering::SeqCst);
            let frames = vec![
                Ok(LlmResponse {
                    content: Some(Content::new("model").with_text("alpha ")),
                    partial: true,
                    ..Default::default()
                }),
                Ok(LlmResponse {
                    content: Some(Content::new("model").with_text("text")),
                    partial: true,
                    ..Default::default()
                }),
                Ok(LlmResponse {
                    content: Some(Content::new("model").with_text("alpha text")),
                    turn_complete: true,
                    finish_reason: Some(adk_rust::FinishReason::Stop),
                    ..Default::default()
                }),
            ];
            return Ok(Box::pin(stream::iter(frames)));
        }
        Ok(Box::pin(stream::once(async {
            Ok(LlmResponse {
                content: Some(Content::new("model").with_text("alpha text")),
                turn_complete: true,
                finish_reason: Some(adk_rust::FinishReason::Stop),
                ..Default::default()
            })
        })))
    }
}

#[tokio::test]
async fn the_step_back_model_is_called_without_streaming_so_its_text_is_not_doubled() {
    let harness = harness().await;
    seed(&harness).await;
    let model = Arc::new(FramedModel {
        streamed: AtomicBool::new(false),
    });
    let host = host_with_model(
        &harness,
        &harness.token,
        Some(StepbackBinding {
            model: Arc::clone(&model) as Arc<dyn Llm>,
            model_name: "chat-model".to_owned(),
        }),
    );
    // stepback_summary_index returns the model's second reply verbatim: a
    // doubled collection would read "alpha textalpha text".
    let summary = call(
        &host,
        IndexToolName::StepbackSummaryIndex,
        json!({"query": "alpha", "index_name": "docs"}),
    )
    .await;
    assert_eq!(summary, json!("alpha text"));
    assert!(
        !model.streamed.load(Ordering::SeqCst),
        "the step-back prompts are non-streaming calls"
    );
    // stepback_search_index searches with the model's reply as the query and
    // renders the hits as text.
    let found = call(
        &host,
        IndexToolName::StepbackSearchIndex,
        json!({"query": "alpha", "index_name": "docs"}),
    )
    .await;
    let text = found.as_str().expect("rendered hits");
    assert!(text.contains("alpha text"), "{text}");
    assert!(!text.starts_with("FAILED"), "{text}");
}
