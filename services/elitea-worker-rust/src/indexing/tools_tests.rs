//! The index tools' claim host over the in-process fake of `elitea-vector`
//! (`elitea_vector_index::testing`) and a scripted embeddings gateway: the
//! five tools' results, the token fence, the step-back refusal without a
//! model, and the two indexing-runtime modes as the direct tool runtime sees
//! them — a toolkit frozen WITH Main's `indexes` catalog (Rust indexing)
//! serves the index tools, one frozen without it (Python indexing) does not.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use elitea_agent_runtime::toolkits::{
    IndexCatalog, IndexCatalogEntry, IndexToolAuthority, IndexToolCall, IndexToolHost,
    IndexToolName,
};
use elitea_vector_index::client::toolkit_namespace;
use elitea_vector_index::testing::FakeVector;
use elitea_vector_index::{ClientConfig, VectorClient, pb};
use http::{Request, Response, StatusCode, Version};
use http_body_util::{BodyExt as _, Full};
use serde_json::{Value, json};
use tonic::body::Body;

use super::tools::ClaimIndexToolHost;
use super::{IndexIngestRuntime, IndexRunLimits};
use crate::protocol::elitea::runtime::v1::VectorClaimTokenV1;
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
    _server: tokio::task::JoinHandle<()>,
}

async fn harness() -> Harness {
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
    ClaimIndexToolHost::new(
        Arc::clone(&harness.runtime),
        token,
        Arc::new(ClaimScopedEliteaContext::fixture(1, "pat")),
        None,
    )
}

async fn call(host: &ClaimIndexToolHost, tool: IndexToolName, arguments: Value) -> Value {
    let catalog = catalog();
    match host
        .call(IndexToolCall {
            tool,
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
    // available indexes, the right one deletes the namespace.
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
    assert_eq!(harness.fake.point_count(), 2);
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
    assert_eq!(harness.fake.point_count(), 0);
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
    let mut toolkit = json!({
        "id": 41,
        "type": "github",
        "toolkit_name": "repo",
        "settings": {
            "github_configuration": {
                "configuration_type": "github",
                "base_url": "https://github.example.test/api/v3/",
                "access_token": "fixture-token"
            },
            "repository": "https://github.example.test/EliteaAI/elitea-platform.git",
            "active_branch": "main",
            "base_branch": "main",
            "selected_tools": ["get_me", "list_indexes"],
            "pgvector_configuration": null,
            "embedding_model": null
        }
    });
    if let Some(indexes) = indexes {
        toolkit["indexes"] = indexes;
    }
    serde_json::to_vec(&toolkit).expect("settings")
}

fn frozen_catalog() -> Value {
    json!([{"name": "docs", "index_id": NS, "embedding_model": MODEL, "embedding_dimension": DIMENSION}])
}

async fn list_indexes_directly(
    settings: &[u8],
    index_tools: Option<&IndexToolAuthority>,
) -> Result<Value, DirectToolkitRuntimeErrorCode> {
    let request = DirectToolkitRequest::parse_call(
        "github",
        "41",
        "list_indexes",
        settings,
        b"{}",
        br#"{"toolkit_security":{}}"#,
    )
    .expect("request");
    DirectToolkitRuntime::default()
        .execute_test(&request, "execution", "call", index_tools)
        .await
        .map_err(|error| error.code())
}

/// `toolkit.call_tool.v1`: the index tools exist only where Main froze a
/// catalog (Rust indexing) AND this worker lends its vector store.
#[tokio::test]
async fn the_direct_runtime_serves_index_tools_only_under_rust_indexing() {
    let harness = harness().await;
    let authority = IndexToolAuthority::new(Arc::new(host(&harness, &harness.token)));

    // Rust indexing, vector store lent: the tool runs against the store.
    let listed = list_indexes_directly(&github_settings(Some(frozen_catalog())), Some(&authority))
        .await
        .expect("listed");
    assert_eq!(listed, json!("docs"));

    // Python indexing: Main wrote no catalog, the SDK worker owns the tool,
    // and this worker refuses it as not selected — whatever it could lend.
    assert_eq!(
        list_indexes_directly(&github_settings(None), Some(&authority)).await,
        Err(DirectToolkitRuntimeErrorCode::ToolNotSelected)
    );

    // Rust indexing on a worker without a vector store: no authority, the
    // tool is left out (and warned about) rather than bound to fail.
    assert_eq!(
        list_indexes_directly(&github_settings(Some(frozen_catalog())), None).await,
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
