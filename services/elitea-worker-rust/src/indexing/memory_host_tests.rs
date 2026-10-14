//! The `memory` toolkit's claim host (worker unit M9) over the in-process
//! fake of `elitea-vector` and a scripted embeddings gateway: the four tools
//! end to end, the embedding model the deployment names, isolation between
//! projects and namespaces, the token fence, and the failure sentence of each
//! cause.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use elitea_agent_runtime::toolkits::{IndexToolHost, MemoryToolCall, MemoryToolName};
use elitea_vector_index::testing::{FakeVector, status};
use elitea_vector_index::{ClientConfig, VectorClient};
use http::{Request, Response, StatusCode, Version};
use http_body_util::{BodyExt as _, Full};
use serde_json::{Value, json};
use tonic::Code;
use tonic::body::Body;

use super::tools::ClaimIndexToolHost;
use super::{IndexIngestRuntime, IndexRunLimits};
use crate::protocol::control::IndexRemovalBinding;
use crate::protocol::elitea::runtime::v1::VectorClaimTokenV1;
use crate::protocol::vector_token::VectorClaimToken;
use crate::transport::embeddings_facade::EmbeddingsClient;
use crate::transport::openai_compatible_facade::{
    ModelGatewayTransport, ModelGatewayTransportError,
};
use crate::transport::runtime_context::ClaimScopedEliteaContext;

const TOKEN_A: &str = "elvc_0123456789abcdef0123456789abcdef0123456789a";
const TOKEN_B: &str = "elvc_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const MEMORY_MODEL: &str = "memory-embedder";

/// Embeds by topic, records the model each request names, and can refuse.
struct TopicGateway {
    models: Mutex<Vec<String>>,
    requests: AtomicUsize,
    refuse: Option<StatusCode>,
}

#[async_trait]
impl ModelGatewayTransport for TopicGateway {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, ModelGatewayTransportError> {
        self.requests.fetch_add(1, Ordering::SeqCst);
        let body = request
            .into_body()
            .collect()
            .await
            .map_err(|_| ModelGatewayTransportError::Unavailable)?
            .to_bytes();
        let parsed: Value = serde_json::from_slice(&body).expect("json body");
        self.models
            .lock()
            .expect("models")
            .push(parsed["model"].as_str().expect("model").to_owned());
        if let Some(refusal) = self.refuse {
            return Ok(Response::builder()
                .status(refusal)
                .version(Version::HTTP_2)
                .body(Body::new(Full::new(bytes::Bytes::from_static(b"{}"))))
                .expect("response"));
        }
        let data: Vec<Value> = parsed["input"]
            .as_array()
            .expect("inputs")
            .iter()
            .enumerate()
            .map(|(index, input)| {
                let text = input.as_str().unwrap_or("").to_lowercase();
                let topic = |word: &str| f64::from(u8::from(text.contains(word)));
                let mut vector = vec![topic("pasta"), topic("paris"), topic("rust")];
                if vector.iter().all(|value| *value == 0.0) {
                    vector[0] = 0.01;
                }
                json!({"object": "embedding", "index": index, "embedding": vector})
            })
            .collect();
        let reply = json!({"object": "list", "model": MEMORY_MODEL, "data": data});
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
    gateway: Arc<TopicGateway>,
    _server: tokio::task::JoinHandle<()>,
}

async fn harness(memory_model: Option<&str>, refuse: Option<StatusCode>) -> Harness {
    let fake = FakeVector::new(&[(TOKEN_A, 1), (TOKEN_B, 2)]);
    let (address, server) = fake.clone().serve().await.expect("serve");
    let mut config = ClientConfig::new(format!("http://{address}"));
    config.allow_insecure = true;
    let vector = VectorClient::connect_lazy(&config).expect("client");
    let gateway = Arc::new(TopicGateway {
        models: Mutex::new(Vec::new()),
        requests: AtomicUsize::new(0),
        refuse,
    });
    let mut runtime = IndexIngestRuntime {
        chat_models: None,
        memory_model: None,
        vector,
        embeddings: EmbeddingsClient::with_transport(
            Arc::clone(&gateway) as Arc<dyn ModelGatewayTransport>,
            Duration::from_secs(5),
        ),
        removal: None,
        platform_origin: "https://platform.internal".to_owned(),
        allowlist: elitea_connectors::egress::HostAllowlist::parse(Some("*")),
        extractor: None,
        transport: Arc::new(
            elitea_connectors::reqwest012::ReqwestTransport::hardened(
                &elitea_connectors::github::client::CLIENT_POLICY,
            )
            .expect("transport"),
        ),
        limits: IndexRunLimits::default(),
    };
    if let Some(model) = memory_model {
        runtime = runtime.with_memory_embedding_model(model);
    }
    Harness {
        runtime: Arc::new(runtime),
        fake,
        gateway,
        _server: server,
    }
}

fn token(bearer: &str, sources: &[&str]) -> VectorClaimToken {
    VectorClaimToken::from_receipt(Some(VectorClaimTokenV1 {
        bearer: bearer.to_owned(),
        expires_at_unix_millis: i64::MAX,
        allowed_sources: sources.iter().map(ToString::to_string).collect(),
    }))
    .expect("token")
}

fn host(harness: &Harness, token: &VectorClaimToken) -> ClaimIndexToolHost {
    ClaimIndexToolHost::new(
        Arc::clone(&harness.runtime),
        token,
        Arc::new(ClaimScopedEliteaContext::fixture(1, "pat")),
        None,
        IndexRemovalBinding::fixture(),
    )
}

async fn call(
    host: &ClaimIndexToolHost,
    tool: MemoryToolName,
    namespace: &str,
    arguments: Value,
) -> String {
    match host
        .memory(MemoryToolCall {
            tool,
            namespace,
            arguments: &arguments,
        })
        .await
    {
        Ok(Value::String(text)) => text,
        Ok(other) => panic!("a memory tool answers text, not {other}"),
        Err(failure) => format!("FAILED: {}", failure.message()),
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn the_four_tools_work_end_to_end_with_the_deployments_model() {
    let harness = harness(Some(MEMORY_MODEL), None).await;
    let token = token(TOKEN_A, &["toolkit_index"]);
    let host = host(&harness, &token);
    assert!(host.serves_memory());

    let stored = call(
        &host,
        MemoryToolName::ManageMemory,
        "team",
        json!({"content": "pasta needs salt", "key": "pasta", "metadata": {"src": "chef"}}),
    )
    .await;
    assert_eq!(stored, "Successfully stored memory with key: pasta");
    call(
        &host,
        MemoryToolName::ManageMemory,
        "team",
        json!({"content": "flight to Paris", "key": "trip"}),
    )
    .await;

    let found = call(
        &host,
        MemoryToolName::SearchMemory,
        "team",
        json!({"query": "pasta?", "limit": 1}),
    )
    .await;
    let parsed: Value = serde_json::from_str(&found).expect("json");
    assert_eq!(parsed[0]["key"], "pasta");
    assert_eq!(parsed[0]["metadata"]["src"], "chef");
    assert_eq!(parsed.as_array().expect("list").len(), 1);

    let got = call(
        &host,
        MemoryToolName::GetMemory,
        "team",
        json!({"key": "trip"}),
    )
    .await;
    assert!(got.contains("\"content\": \"flight to Paris\""), "{got}");
    assert_eq!(
        call(
            &host,
            MemoryToolName::DeleteMemory,
            "team",
            json!({"key": "trip"})
        )
        .await,
        "Successfully deleted memory with key: trip"
    );
    assert_eq!(
        call(
            &host,
            MemoryToolName::GetMemory,
            "team",
            json!({"key": "trip"})
        )
        .await,
        "No memory found with key: trip"
    );

    // Every embedding used the deployment's memory model, nothing else.
    let models = harness.gateway.models.lock().expect("models").clone();
    assert!(!models.is_empty());
    assert!(
        models.iter().all(|model| model == MEMORY_MODEL),
        "{models:?}"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn projects_and_namespaces_never_share_memory() {
    let harness = harness(Some(MEMORY_MODEL), None).await;
    let (a, b) = (
        token(TOKEN_A, &["toolkit_index"]),
        token(TOKEN_B, &["toolkit_index"]),
    );
    let (host_a, host_b) = (host(&harness, &a), host(&harness, &b));
    call(
        &host_a,
        MemoryToolName::ManageMemory,
        "shared",
        json!({"content": "pasta of A", "key": "k"}),
    )
    .await;

    // Project B, same namespace string and key.
    assert_eq!(
        call(
            &host_b,
            MemoryToolName::GetMemory,
            "shared",
            json!({"key": "k"})
        )
        .await,
        "No memory found with key: k"
    );
    assert_eq!(
        call(
            &host_b,
            MemoryToolName::SearchMemory,
            "shared",
            json!({"query": "pasta"})
        )
        .await,
        "No memories found matching your query."
    );
    // Project A, another namespace.
    assert_eq!(
        call(
            &host_a,
            MemoryToolName::GetMemory,
            "elsewhere",
            json!({"key": "k"})
        )
        .await,
        "No memory found with key: k"
    );
    // B's delete of the same key leaves A's memory.
    call(
        &host_b,
        MemoryToolName::DeleteMemory,
        "shared",
        json!({"key": "k"}),
    )
    .await;
    assert!(
        call(
            &host_a,
            MemoryToolName::GetMemory,
            "shared",
            json!({"key": "k"})
        )
        .await
        .contains("pasta of A")
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn memory_is_not_served_without_a_model_or_a_source() {
    let without_model = harness(None, None).await;
    let token_a = token(TOKEN_A, &["toolkit_index"]);
    let host_a = host(&without_model, &token_a);
    assert!(!host_a.serves_memory());
    assert!(
        call(
            &host_a,
            MemoryToolName::ManageMemory,
            "n",
            json!({"content": "x"})
        )
        .await
        .contains("no memory embedding model configured")
    );

    let with_model = harness(Some(MEMORY_MODEL), None).await;
    let deepwiki_only = token(TOKEN_A, &["deepwiki"]);
    let fenced = host(&with_model, &deepwiki_only);
    assert!(!fenced.serves_memory());
    let sentence = call(
        &fenced,
        MemoryToolName::SearchMemory,
        "n",
        json!({"query": "x"}),
    )
    .await;
    assert!(
        sentence.contains("Failed to search memory") && sentence.contains("no project token"),
        "{sentence}"
    );
    assert_eq!(with_model.gateway.requests.load(Ordering::SeqCst), 0);
    assert_eq!(with_model.fake.point_count(), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn each_failure_cause_names_itself() {
    // The gateway refuses (budget): nothing is written.
    let refusing = harness(Some(MEMORY_MODEL), Some(StatusCode::PAYMENT_REQUIRED)).await;
    let token_a = token(TOKEN_A, &["toolkit_index"]);
    let host_a = host(&refusing, &token_a);
    let sentence = call(
        &host_a,
        MemoryToolName::ManageMemory,
        "n",
        json!({"content": "pasta"}),
    )
    .await;
    assert!(
        sentence.starts_with(
            "FAILED: Failed to store memory: the memory embedding model could not embed the text"
        ) && sentence.contains("the model gateway refused"),
        "{sentence}"
    );
    assert_eq!(refusing.fake.point_count(), 0);

    // The vector store is down.
    let working = harness(Some(MEMORY_MODEL), None).await;
    let host_a = host(&working, &token_a);
    working.fake.fail_next(status(Code::Unavailable));
    let sentence = call(
        &host_a,
        MemoryToolName::SearchMemory,
        "n",
        json!({"query": "pasta"}),
    )
    .await;
    assert!(
        sentence.contains("Failed to search memory: the vector store is unavailable right now"),
        "{sentence}"
    );
    // The token was revoked.
    working.fake.fail_next(status(Code::Unauthenticated));
    let sentence = call(
        &host_a,
        MemoryToolName::DeleteMemory,
        "n",
        json!({"key": "k"}),
    )
    .await;
    assert!(
        sentence.contains("Failed to delete memory: the vector store refused this run's token"),
        "{sentence}"
    );
    // Malformed arguments, and a limit.
    let sentence = call(&host_a, MemoryToolName::GetMemory, "n", json!({})).await;
    assert!(
        sentence.contains("Failed to retrieve memory: the tool arguments are invalid"),
        "{sentence}"
    );
    let sentence = call(
        &host_a,
        MemoryToolName::ManageMemory,
        "n",
        json!({"content": "x".repeat(20_000)}),
    )
    .await;
    assert!(
        sentence.contains("a memory holds at most 16384 bytes"),
        "{sentence}"
    );
    assert_eq!(working.fake.point_count(), 0);
}
