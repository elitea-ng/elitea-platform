//! The pipeline's `artifact` `create_file` node behind the same fenced journal
//! as every effectful direct tool: the write is dispatched once, a lost step
//! checkpoint replays the committed result, and a started attempt whose result
//! was never recorded is reconciled, never written again.
use super::*;
use crate::protocol::control::test_runtime_context_authority;
use crate::toolkits::ArtifactToolAuthority;
use crate::toolkits::families::artifact::config::ArtifactToolkitConfig;
use crate::toolkits::families::artifact::tools::build_artifact_toolset;
use crate::transport::platform_client::PlatformClient;
use crate::transport::platform_writer::ClaimPlatformWriter;
use crate::transport::runtime_context::{
    RuntimeContextClient, RuntimeContextConfig, RuntimeContextRpc, RuntimeContextTransportError,
};
use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, Toolset};
use bytes::Bytes;
use http::{Request, Response, StatusCode, Version};
use http_body_util::Full;
use tonic::body::Body;

const ARTIFACT_NODE: &str = r"
id: mk
type: toolkit
toolkit_name: test
tool: create_file
input_mapping:
  filename: {type: fixed, value: verify-1ab920d-edge.txt}
  filedata: {type: fixed, value: edge identity ok}
output: [created]
transition: END
";

/// Main's claim-bound content listener, counting every write it accepts.
struct WriteListener(Arc<AtomicUsize>);

#[async_trait]
impl RuntimeContextRpc for WriteListener {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, RuntimeContextTransportError> {
        assert!(
            request
                .uri()
                .path()
                .ends_with("/runtime-context/artifacts/write")
        );
        self.0.fetch_add(1, Ordering::SeqCst);
        let raw = json!({
            "schema_version": "elitea.runtime.artifact-write.v1",
            "project_id": 17,
            "bucket": "test",
            "name": "verify-1ab920d-edge.txt",
            "media_type": "text/plain",
            "byte_length": 16,
        })
        .to_string();
        Ok(Response::builder()
            .status(StatusCode::OK)
            .version(Version::HTTP_2)
            .header("content-type", "application/json")
            .header("cache-control", "private, no-cache, no-store")
            .header("pragma", "no-cache")
            .header("content-length", raw.len())
            .body(Body::new(Full::new(Bytes::from(raw))))
            .expect("artifact write response"))
    }
}

async fn create_file_tool(writes: &Arc<AtomicUsize>) -> Arc<dyn Tool> {
    let client = RuntimeContextClient::with_rpc(
        WriteListener(Arc::clone(writes)),
        RuntimeContextConfig {
            origin: "https://content.internal:443".to_owned(),
            deadline: std::time::Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1_024 * 1_024,
            max_attachment_response_bytes: 1_024 * 1_024,
            max_artifact_response_bytes: 2 * 1_024 * 1_024,
        },
    )
    .expect("artifact runtime-context client");
    let authority = ArtifactToolAuthority::new(Arc::new(ClaimPlatformWriter::new(
        Arc::new(PlatformClient::new(Arc::new(client))),
        Arc::new(test_runtime_context_authority()),
    )));
    let settings = json!({"bucket": "test", "selected_tools": ["create_file"]});
    let config = ArtifactToolkitConfig::parse(settings.as_object().expect("artifact settings"))
        .expect("artifact configuration");
    let policy =
        Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("artifact tool policy"));
    let toolset =
        build_artifact_toolset("test", &config, &policy, &authority).expect("artifact toolset");
    let readonly: Arc<dyn ReadonlyContext> =
        Arc::new(SimpleToolContext::new("artifact-journal").with_session_id("session-1"));
    let mut tools = toolset.tools(readonly).await.expect("artifact tools");
    assert_eq!(tools.len(), 1);
    let tool = tools.remove(0);
    assert_eq!(tool.name(), "create_file");
    assert!(
        !tool.is_read_only(),
        "an artifact write is an external effect"
    );
    tool
}

fn artifact_node(factory: &Arc<ScopedFactory>, tool: Arc<dyn Tool>) -> DirectToolNode {
    artifact_node_declaring(factory, tool, "str")
}

fn artifact_node_declaring(
    factory: &Arc<ScopedFactory>,
    tool: Arc<dyn Tool>,
    created_type: &str,
) -> DirectToolNode {
    let resolver = Arc::new(Resolver {
        tool,
        sensitive: None,
    });
    let types = BTreeMap::from([("created".to_owned(), created_type.to_owned())]);
    let definition = DirectToolNodeDefinition::from_yaml(ARTIFACT_NODE).expect("artifact node");
    DirectToolNode::new(definition, types, resolver).with_node_recovery(factory.clone())
}

async fn artifact_journal(factory: &ScopedFactory) -> (NodeAttemptJournal, NodeJournalSnapshot) {
    let digest = DirectToolNodeDefinition::from_yaml(ARTIFACT_NODE)
        .expect("artifact node")
        .config_digest();
    let activation =
        NodeAttemptActivation::from_context("mk", digest, &context(state())).expect("activation");
    let journal = factory
        .open(&activation, &NodeRecoveryPolicy::default())
        .await
        .expect("journal");
    let snapshot = journal.load().await.expect("snapshot");
    (journal, snapshot)
}

#[tokio::test]
async fn artifact_write_runs_once_and_its_committed_result_is_replayed() {
    let writes = Arc::new(AtomicUsize::new(0));
    let factory = scoped();
    let node = artifact_node(&factory, create_file_tool(&writes).await);
    let first = node
        .execute(&context(state()))
        .await
        .expect("artifact write");
    assert!(
        first.updates["created"]
            .to_string()
            .contains("verify-1ab920d-edge.txt"),
        "{:?}",
        first.updates
    );
    assert_eq!(writes.load(Ordering::SeqCst), 1);
    let (_, snapshot) = artifact_journal(&factory).await;
    assert!(matches!(
        snapshot.ledger.phase(),
        NodeAttemptPhase::Completed { .. }
    ));
    let replay = node
        .execute(&context(state()))
        .await
        .expect("replayed write");
    assert_eq!(replay.updates, first.updates);
    assert_eq!(
        writes.load(Ordering::SeqCst),
        1,
        "the write is never repeated"
    );
}

#[tokio::test]
async fn a_started_artifact_write_without_a_result_is_never_written_again() {
    let writes = Arc::new(AtomicUsize::new(0));
    let factory = scoped();
    let (journal, initial) = artifact_journal(&factory).await;
    journal
        .append(&initial, initial.ledger.start_attempt(90).unwrap(), None)
        .await
        .unwrap();
    let node = artifact_node(&factory, create_file_tool(&writes).await);
    for _ in 0..2 {
        let output = node.execute(&context(state())).await.unwrap();
        assert!(recovery_card(&output));
        assert!(output.updates.is_empty());
    }
    assert_eq!(writes.load(Ordering::SeqCst), 0);
}

/// The write answers with TEXT, as the SDK's does. A YAML that declares the
/// output `dict` (the reported pipeline) gets a typed result failure after the
/// one write, never a silently coerced value and never a second write.
#[tokio::test]
async fn a_text_write_result_is_not_coerced_into_a_dict_output() {
    let writes = Arc::new(AtomicUsize::new(0));
    let factory = scoped();
    let node = artifact_node_declaring(&factory, create_file_tool(&writes).await, "dict");
    let Err(error) = node.execute(&context(state())).await else {
        panic!("text was projected into a dict output");
    };
    assert!(error.to_string().contains("state_projection"), "{error}");
    assert_eq!(writes.load(Ordering::SeqCst), 1);
    // The write happened but its result was not recorded: re-entry is the
    // operator reconciliation card, not a second write.
    let Ok(again) = node.execute(&context(state())).await else {
        panic!("re-entry after an unrecorded write must reconcile");
    };
    assert!(recovery_card(&again));
    assert!(again.updates.is_empty());
    assert_eq!(
        writes.load(Ordering::SeqCst),
        1,
        "the write is never repeated"
    );
}
