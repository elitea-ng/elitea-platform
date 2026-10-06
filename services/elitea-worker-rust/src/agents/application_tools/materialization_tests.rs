//! Frozen-version materialization entry fixtures. No presentation catalog is supplied.

use super::*;
use crate::agents::assembly_tests::ordinary_request;
use crate::agents::model_scope::ModelScopeBackend;
use crate::agents::request::AgentExecutionKind;
use crate::protocol::control::test_runtime_context_authority;
use crate::transport::openai_compatible_facade::{
    CapturedModelRequests, TestModelGatewayOutcome, test_model_gateway_client,
    test_model_gateway_config, test_model_gateway_response,
};
use crate::transport::runtime_context::{
    RuntimeContextClient, RuntimeContextConfig, RuntimeContextRpc, RuntimeContextTransportError,
};
use adk_rust::graph::Checkpointer as _;
use bytes::Bytes;
use http::{Request, Response, Version};
use http_body_util::Full;
use std::sync::Mutex as StdMutex;
use std::time::Duration;
use tonic::body::Body;

const THREAD: &str = "materialization-thread";
const STATIC_GRAPH: &str = "interrupt_after: [tick]\nstate: {input: str, messages: list, count: {type: int, value: 0}, answer: str}\nentry_point: tick\nnodes:\n  - id: tick\n    type: state_modifier\n    template: '{{ count + 1 }}'\n    input: [count]\n    output: [count]\n    transition: report\n  - id: report\n    type: state_modifier\n    template: 'COUNT {{ count }}'\n    input: [count]\n    output: [answer]\n    transition: END\n";
const PLAIN_GRAPH: &str = "state: {input: str, messages: list, answer: str}\nentry_point: report\nnodes:\n  - id: report\n    type: state_modifier\n    template: 'DONE {{ input }}'\n    input: [input]\n    output: [answer]\n    transition: END\n";
const DELEGATE_GRAPH: &str = "state: {input: str, messages: list, answer: str}\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: worker\n    input_mapping: {task: {type: variable, value: input}}\n    output: [answer]\n    transition: END\n";

type Identity = (u64, u64);
struct FrozenVersions {
    versions: BTreeMap<Identity, Value>,
    fingerprints: BTreeMap<Identity, String>,
    reads: Arc<StdMutex<Vec<Identity>>>,
}
#[async_trait]
impl RuntimeContextRpc for FrozenVersions {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, RuntimeContextTransportError> {
        let path = request.uri().path();
        let value = if path.ends_with("/elitea-client-token") {
            json!({"schema_version":"elitea.runtime.elitea-client-token.v1", "project_id":17, "token":"fixture-materialization-token"})
        } else {
            let parts = path.split('/').collect::<Vec<_>>();
            let identity = parts
                .get(parts.len() - 3)
                .and_then(|v| v.parse::<u64>().ok())
                .zip(parts.last().and_then(|v| v.parse::<u64>().ok()))
                .ok_or(RuntimeContextTransportError::Unavailable)?;
            self.reads.lock().unwrap().push(identity);
            let version = self
                .versions
                .get(&identity)
                .ok_or(RuntimeContextTransportError::Unavailable)?;
            let mut value = json!({"schema_version":"elitea.runtime.application-version.v1", "project_id":17,
                "application_id":identity.0,"version_id":identity.1,"version_details":version});
            if let Some(fingerprint) = self.fingerprints.get(&identity) {
                value["frozen_definition_sha256"] = json!(fingerprint);
            }
            value
        };
        let body = value.to_string();
        Response::builder()
            .status(200)
            .version(Version::HTTP_2)
            .header("content-type", "application/json")
            .header("cache-control", "private, no-cache, no-store")
            .header("pragma", "no-cache")
            .header("content-length", body.len())
            .body(Body::new(Full::new(Bytes::from(body))))
            .map_err(|_| RuntimeContextTransportError::Unavailable)
    }
}
fn reference(identity: Identity, kind: &str, alias: &str) -> Value {
    json!({"id":identity.0,"type":"application","name":alias,"toolkit_name":alias,
        "agent_type":kind,"author_id":11,"settings":{"application_id":identity.0,"application_version_id":identity.1},
        "meta":{},"variables":[],"is_pinned":false,"created_at":"2026-08-21T10:00:00Z"})
}
fn version(kind: &str, instructions: &str, tools: Vec<Value>) -> Value {
    json!({"agent_type":kind,"instructions":instructions,"meta":{},"variables":[],"tools":Value::Array(tools),
        "llm_settings":{"model_name":"child-model","model_project_id":23,"max_tokens":2048,
            "reasoning_effort":null,"temperature":0.2,"openai_compatible":true}})
}
fn tool_call_response() -> Response<Body> {
    let raw = concat!(
        "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"inner-call\",\"type\":\"function\",\"function\":{\"name\":\"elitea_agent_33_v_43\",\"arguments\":\"{\\\"task\\\":\\\"inner work\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}
struct Fixture {
    platform: PlatformClient,
    facade: Arc<ModelFacade>,
    reads: Arc<StdMutex<Vec<Identity>>>,
    models: CapturedModelRequests,
}
impl Fixture {
    fn new(versions: BTreeMap<Identity, Value>) -> Self {
        Self::with_outcome(versions, tool_call_response())
    }
    fn with_outcome(versions: BTreeMap<Identity, Value>, response: Response<Body>) -> Self {
        Self::with_outcomes(versions, vec![response])
    }
    fn with_outcomes(versions: BTreeMap<Identity, Value>, responses: Vec<Response<Body>>) -> Self {
        Self::with_outcomes_and_fingerprints(versions, responses, BTreeMap::new())
    }
    fn with_outcomes_and_fingerprints(
        versions: BTreeMap<Identity, Value>,
        responses: Vec<Response<Body>>,
        fingerprints: BTreeMap<Identity, String>,
    ) -> Self {
        let reads = Arc::new(StdMutex::new(Vec::new()));
        let rpc = RuntimeContextClient::with_rpc(
            FrozenVersions {
                versions,
                fingerprints,
                reads: reads.clone(),
            },
            RuntimeContextConfig {
                origin: "https://content.internal".to_owned(),
                deadline: Duration::from_secs(1),
                max_response_bytes: 32 * 1024,
                max_application_response_bytes: 1024 * 1024,
                max_attachment_response_bytes: 1024 * 1024,
                max_artifact_response_bytes: 2 * 1024 * 1024,
            },
        )
        .unwrap();
        let (gateway, models) = test_model_gateway_client(
            responses
                .into_iter()
                .map(TestModelGatewayOutcome::Response)
                .collect(),
            test_model_gateway_config(),
        )
        .unwrap();
        Self {
            platform: PlatformClient::new(Arc::new(rpc)),
            facade: Arc::new(ModelFacade::from_gateway(gateway)),
            reads,
            models,
        }
    }
    async fn build(
        &self,
        root: Value,
        scoped: bool,
    ) -> Result<
        (
            Option<MaterializedApplicationRuntime>,
            Vec<SkippedApplicationChild>,
        ),
        NativeAgentAssemblyError,
    > {
        let authority = test_runtime_context_authority();
        let context = Arc::new(
            self.platform
                .redeem_elitea_context(&authority)
                .await
                .unwrap(),
        );
        let root_version = version("agent", "Root", vec![root]);
        let frozen =
            FrozenToolSnapshot::from_version_details(root_version.as_object().unwrap()).unwrap();
        let policy = Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).unwrap());
        let profile =
            OrdinaryNoToolProfile::validate(&ordinary_request(AgentExecutionKind::Application))
                .unwrap();
        let model_sessions = ModelScopeSessions::new(
            ModelScopeBackend::Local(Arc::new(adk_rust::session::InMemorySessionService::new())),
            "execution-materialization".to_owned(),
            1,
            [1; 32],
        );
        // Test the same internal entry path without changing production admission.
        materialize_application_runtime(
            &frozen.apply_policy(&policy),
            &self.platform,
            &authority,
            context,
            &profile,
            ApplicationToolDependencies::new(
                self.facade.clone(),
                policy,
                Arc::new(crate::toolkits::AdkHttpMcpConnector::new()),
                &Map::new(),
            )
            .with_model_scopes(model_sessions)
            .with_conversation_thread(THREAD.to_owned())
            .with_materialization(if scoped {
                ApplicationMaterializationPath::new(true)
            } else {
                ApplicationMaterializationPath::default()
            }),
            None,
        )
        .await
    }
    async fn saved_tool_runtimes(
        &self,
    ) -> (
        crate::agents::graph::compiler::PipelineDefinition,
        crate::agents::graph::compiler::PipelineNodeRuntimes,
    ) {
        self.saved_tool_runtimes_for((31, 41)).await
    }
    async fn saved_tool_runtimes_for(
        &self,
        application: (u64, u64),
    ) -> (
        crate::agents::graph::compiler::PipelineDefinition,
        crate::agents::graph::compiler::PipelineNodeRuntimes,
    ) {
        let authority = test_runtime_context_authority();
        let context = Arc::new(
            self.platform
                .redeem_elitea_context(&authority)
                .await
                .unwrap(),
        );
        let policy = Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).unwrap());
        let fallback =
            OrdinaryNoToolProfile::validate(&ordinary_request(AgentExecutionKind::Application))
                .unwrap();
        let model_sessions = ModelScopeSessions::new(
            ModelScopeBackend::Local(Arc::new(adk_rust::session::InMemorySessionService::new())),
            "execution-materialization".to_owned(),
            1,
            [1; 32],
        );
        let (sender, _receiver) = pipeline_node_event_channel();
        let (definition, actual, _) = materialize_saved_pipeline_tool(
            &self.platform,
            &authority,
            context,
            self.facade.clone(),
            &(Arc::new(crate::toolkits::AdkHttpMcpConnector::new()) as Arc<dyn McpConnector>),
            &Map::new(),
            policy,
            &fallback,
            sender,
            application,
            None,
            model_sessions,
            None,
            THREAD.to_owned(),
            ApplicationMaterializationPath::new(true),
        )
        .await
        .unwrap();
        (definition, actual)
    }
}
struct Context {
    invocation: &'static str,
    content: Content,
    actions: StdMutex<adk_rust::EventActions>,
}
#[async_trait]
impl ReadonlyContext for Context {
    fn invocation_id(&self) -> &str {
        self.invocation
    }
    fn agent_name(&self) -> &'static str {
        "root-agent"
    }
    fn user_id(&self) -> &'static str {
        "user-1"
    }
    fn app_name(&self) -> &'static str {
        "elitea-agent-v1"
    }
    fn session_id(&self) -> &str {
        THREAD
    }
    fn branch(&self) -> &str {
        APPLICATION_BRANCH_ROOT
    }
    fn user_content(&self) -> &Content {
        &self.content
    }
}
#[async_trait]
impl CallbackContext for Context {
    fn artifacts(&self) -> Option<Arc<dyn Artifacts>> {
        None
    }
}
#[async_trait]
impl ToolContext for Context {
    fn function_call_id(&self) -> &'static str {
        "root-call"
    }
    fn actions(&self) -> adk_rust::EventActions {
        self.actions.lock().unwrap().clone()
    }
    fn set_actions(&self, actions: adk_rust::EventActions) {
        *self.actions.lock().unwrap() = actions;
    }
    async fn search_memory(&self, _query: &str) -> adk_rust::Result<Vec<adk_rust::MemoryEntry>> {
        Ok(Vec::new())
    }
}
async fn execute(runtime: &MaterializedApplicationRuntime) -> (Value, Vec<Event>) {
    let entry = &runtime.tools[0];
    let mut original = Event::new("original-turn");
    original.id = "original-batch".to_owned();
    original.author = "root-agent".to_owned();
    original.branch = APPLICATION_BRANCH_ROOT.to_owned();
    original.llm_response.content = Some(Content {
        role: "model".to_owned(),
        parts: vec![Part::FunctionCall {
            name: entry.tool.name().to_owned(),
            args: json!({"task":"root work"}),
            id: Some("root-call".to_owned()),
            thought_signature: None,
        }],
    });
    runtime
        .resume
        .observe_call_lineage(&original)
        .await
        .unwrap();
    let result = runtime.tools[0]
        .tool
        .execute(
            Arc::new(Context {
                invocation: "original-turn",
                content: Content::new("user"),
                actions: StdMutex::default(),
            }),
            json!({"task":"root work"}),
        )
        .await
        .unwrap();
    let mut receiver = runtime.events.take().await.unwrap();
    let mut events = vec![original];
    while let Ok(signal) = receiver.try_recv() {
        events.push(application_signal_event(signal).unwrap());
    }
    runtime.events.restore(receiver).await.unwrap();
    (result, events)
}
fn versions() -> BTreeMap<Identity, Value> {
    BTreeMap::from([
        (
            (31, 41),
            version(
                "pipeline",
                DELEGATE_GRAPH,
                vec![reference((32, 42), "agent", "worker")],
            ),
        ),
        (
            (32, 42),
            version(
                "agent",
                "Call the saved graph.",
                vec![reference((33, 43), "pipeline", "inner")],
            ),
        ),
        ((33, 43), version("pipeline", STATIC_GRAPH, vec![])),
    ])
}

#[tokio::test]
async fn saved_agent_materialization_retains_sealed_identity_and_definition_digest() {
    let fixture = Fixture::with_outcomes_and_fingerprints(
        BTreeMap::from([((32, 42), version("agent", "Frozen task", vec![]))]),
        vec![],
        BTreeMap::from([((32, 42), "12".repeat(32))]),
    );
    let (materialized, skipped) = fixture
        .build(reference((32, 42), "agent", "worker alias"), true)
        .await
        .unwrap();
    assert!(skipped.is_empty());
    let runtime = materialized.unwrap();
    assert_eq!(runtime.tools.len(), 1);
    assert_eq!(runtime.tools[0].alias, "worker alias");
    let fingerprint = runtime.tools[0].saved_agent_fingerprint.unwrap();
    assert_eq!(fingerprint.application_id(), 32);
    assert_eq!(fingerprint.version_id(), 42);
    assert_eq!(fingerprint.definition_digest(), [0x12; 32]);
    assert_eq!(*fixture.reads.lock().unwrap(), vec![(32, 42)]);
    assert!(fixture.models.lock().unwrap().is_empty());
}

#[tokio::test]
async fn saved_agent_alias_does_not_replace_immutable_resolved_identity() {
    let mut fingerprints = Vec::new();
    for alias in ["worker", "renamed worker"] {
        let fixture = Fixture::with_outcomes_and_fingerprints(
            BTreeMap::from([((32, 42), version("agent", "Frozen task", vec![]))]),
            vec![],
            BTreeMap::from([((32, 42), "12".repeat(32))]),
        );
        let (runtime, _) = fixture
            .build(reference((32, 42), "agent", alias), true)
            .await
            .unwrap();
        fingerprints.push(runtime.unwrap().tools[0].saved_agent_fingerprint.unwrap());
        assert_eq!(*fixture.reads.lock().unwrap(), vec![(32, 42)]);
        assert!(fixture.models.lock().unwrap().is_empty());
    }
    assert!(fingerprints[0] == fingerprints[1]);
}

#[tokio::test]
async fn same_agent_alias_retains_distinct_saved_versions_and_definitions() {
    let mut fingerprints = Vec::new();
    for (identity, instructions, digest) in [
        ((32, 42), "First definition", "12".repeat(32)),
        ((32, 43), "Second definition", "34".repeat(32)),
    ] {
        let fixture = Fixture::with_outcomes_and_fingerprints(
            BTreeMap::from([(identity, version("agent", instructions, vec![]))]),
            vec![],
            BTreeMap::from([(identity, digest)]),
        );
        let (runtime, _) = fixture
            .build(reference(identity, "agent", "worker"), true)
            .await
            .unwrap();
        let fingerprint = runtime.unwrap().tools[0].saved_agent_fingerprint.unwrap();
        assert_eq!(fingerprint.application_id(), identity.0);
        assert_eq!(fingerprint.version_id(), identity.1);
        fingerprints.push(fingerprint);
        assert!(fixture.models.lock().unwrap().is_empty());
    }
    assert!(fingerprints[0] != fingerprints[1]);
    assert_ne!(
        fingerprints[0].definition_digest(),
        fingerprints[1].definition_digest()
    );
}

#[tokio::test]
async fn legacy_saved_agent_materialization_retains_no_editable_fingerprint_authority() {
    let mut child = version("agent", "Frozen task", vec![]);
    child["meta"]["frozen_definition_sha256"] = json!("12".repeat(32));
    let fixture = Fixture::with_outcomes(BTreeMap::from([((32, 42), child)]), vec![]);
    let (runtime, _) = fixture
        .build(reference((32, 42), "agent", "worker"), true)
        .await
        .unwrap();
    assert!(runtime.unwrap().tools[0].saved_agent_fingerprint.is_none());
    assert_eq!(*fixture.reads.lock().unwrap(), vec![(32, 42)]);
    assert!(fixture.models.lock().unwrap().is_empty());
}

#[tokio::test]
async fn saved_graph_entry_builds_descendant_catalog_and_real_nested_static_tool() {
    let fixture = Fixture::new(versions());
    let (runtime, skipped) = fixture
        .build(reference((31, 41), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(skipped.is_empty());
    let runtime = runtime.unwrap();
    let graph = runtime
        .presentations
        .child_tools("elitea_agent_31_v_41")
        .unwrap();
    let agent = graph.child_tools("elitea_agent_32_v_42").unwrap();
    assert!(agent.contains_runtime_tool("elitea_agent_33_v_43"));
    let (result, events) = execute(&runtime).await;
    assert_eq!(nested_application_interrupt_ids(&result).unwrap().len(), 1);
    let leaf = events
        .iter()
        .find(|event| static_pipeline_tool_pause(event).unwrap().is_some())
        .unwrap();
    let pause = static_pipeline_tool_pause(leaf).unwrap().unwrap();
    assert!(pause.thread_id.starts_with(THREAD));
    assert!(
        leaf.provider_metadata
            .contains_key(crate::agents::application_pipeline::BOUNDARY_LEDGER_KEY)
    );
    let requests = fixture.models.lock().unwrap();
    assert_eq!(requests.len(), 1);
    let body: Value = serde_json::from_slice(&requests[0].body).unwrap();
    assert_eq!(body["tools"][0]["function"]["name"], "elitea_agent_33_v_43");
    assert_eq!(
        *fixture.reads.lock().unwrap(),
        vec![(31, 41), (32, 42), (33, 43)]
    );
}
#[tokio::test]
async fn ordinary_agent_entry_materializes_its_frozen_pipeline_attachments() {
    let fixture = Fixture::new(versions());
    let (runtime, skipped) = fixture
        .build(reference((32, 42), "agent", "worker"), true)
        .await
        .unwrap();
    assert!(skipped.is_empty());
    let runtime = runtime.unwrap();
    assert!(
        runtime
            .presentations
            .child_tools("elitea_agent_32_v_42")
            .unwrap()
            .contains_runtime_tool("elitea_agent_33_v_43")
    );
    let (result, events) = execute(&runtime).await;
    assert_eq!(nested_application_interrupt_ids(&result).unwrap().len(), 1);
    let pause = events
        .iter()
        .find_map(|event| static_pipeline_tool_pause(event).unwrap())
        .unwrap();
    assert!(pause.thread_id.starts_with(THREAD));
    assert_eq!(fixture.models.lock().unwrap().len(), 1);
}
#[tokio::test]
async fn saved_graph_entry_keeps_actual_descendant_presentations_across_graph_wrappers() {
    let mut input = versions();
    input.insert(
        (30, 40),
        version(
            "pipeline",
            DELEGATE_GRAPH,
            vec![reference((31, 41), "pipeline", "worker")],
        ),
    );
    let fixture = Fixture::new(input);
    let (runtime, skipped) = fixture
        .build(reference((30, 40), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(skipped.is_empty());
    let runtime = runtime.unwrap();
    let catalog = runtime
        .presentations
        .child_tools("elitea_agent_30_v_40")
        .unwrap()
        .child_tools("worker")
        .unwrap()
        .child_tools("elitea_agent_32_v_42")
        .unwrap();
    assert!(catalog.contains_runtime_tool("elitea_agent_33_v_43"));
    assert!(fixture.models.lock().unwrap().is_empty());
}
#[tokio::test]
async fn mixed_graph_agent_cycle_is_refused_before_provider_effects() {
    let mut input = versions();
    input.insert(
        (32, 42),
        version(
            "agent",
            "Cycle",
            vec![reference((31, 41), "pipeline", "cycle")],
        ),
    );
    let fixture = Fixture::new(input);
    let (runtime, skipped) = fixture
        .build(reference((31, 41), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(runtime.is_none());
    assert_eq!(skipped.len(), 1);
    assert!(fixture.models.lock().unwrap().is_empty());
    assert_eq!(
        *fixture.reads.lock().unwrap(),
        vec![(31, 41), (32, 42), (31, 41)]
    );
}
fn two_scope_versions() -> BTreeMap<Identity, Value> {
    let mut input = versions();
    input.insert(
        (33, 43),
        version(
            "pipeline",
            DELEGATE_GRAPH,
            vec![reference((34, 44), "agent", "worker")],
        ),
    );
    input.insert(
        (34, 44),
        version(
            "agent",
            "Call the final saved graph.",
            vec![reference((35, 45), "pipeline", "final")],
        ),
    );
    input.insert((35, 45), version("pipeline", STATIC_GRAPH, vec![]));
    input
}
fn final_tool_call_response() -> Response<Body> {
    let delta = json!({"choices":[{"delta":{"tool_calls":[{"index":0,"id":"final-call","type":"function","function":{"name":"elitea_agent_35_v_45","arguments":"{\"task\":\"final work\"}"}}]},"finish_reason":null}]}).to_string();
    let raw = format!(
        "data: {delta}\n\ndata: {{\"choices\":[{{\"delta\":{{}},\"finish_reason\":\"tool_calls\"}}]}}\n\ndata: [DONE]\n\n"
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}
#[tokio::test]
async fn second_ordinary_graph_scope_produces_bounded_exact_activation_chain() {
    let fixture = Fixture::with_outcomes(
        two_scope_versions(),
        vec![tool_call_response(), final_tool_call_response()],
    );
    let (runtime, skipped) = fixture
        .build(reference((31, 41), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(skipped.is_empty());
    let runtime = runtime.unwrap();
    let (paused, events) = execute(&runtime).await;
    assert_eq!(nested_application_interrupt_ids(&paused).unwrap().len(), 1);
    let pause = events
        .iter()
        .find_map(|event| static_pipeline_tool_pause(event).unwrap())
        .unwrap();
    let activations =
        crate::agents::pipeline::scoped_applications::activations_for_event(&pause.event).unwrap();
    assert_eq!(activations.len(), 2);
    let mut projected = pause.event.clone();
    crate::agents::events::strip_descendant_private_metadata(&mut projected);
    assert!(
        !projected
            .provider_metadata
            .contains_key(crate::agents::pipeline::scoped_applications::ACTIVATION_CHAIN_KEY)
    );
    assert!(
        !projected
            .provider_metadata
            .contains_key(crate::agents::application_pipeline::BOUNDARY_LEDGER_KEY)
    );
    assert_ne!(activations[0].call_id(), activations[1].call_id());
    let ledger =
        crate::agents::application_pipeline::projected_outer_boundaries(&pause.event).unwrap();
    assert_eq!(ledger.len(), 2);
    assert_ne!(ledger[0].thread, ledger[1].thread);
    let boundary = pipeline_application_boundary(&events, &pause.event)
        .unwrap()
        .unwrap();
    assert_eq!(
        boundary.scope_route.application_call_id,
        activations[1].call_id()
    );
    assert_eq!(
        *fixture.reads.lock().unwrap(),
        vec![(31, 41), (32, 42), (33, 43), (34, 44), (35, 45)]
    );
    assert_eq!(fixture.models.lock().unwrap().len(), 2);
}
#[tokio::test]
async fn third_ordinary_graph_scope_is_refused_before_provider_effects() {
    let mut input = two_scope_versions();
    input.insert(
        (35, 45),
        version(
            "pipeline",
            DELEGATE_GRAPH,
            vec![reference((36, 46), "agent", "worker")],
        ),
    );
    input.insert((36, 46), version("agent", "Leaf", vec![]));
    let fixture = Fixture::new(input);
    let (runtime, skipped) = fixture
        .build(reference((31, 41), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(runtime.is_none());
    assert_eq!(skipped.len(), 1);
    assert!(fixture.models.lock().unwrap().is_empty());
    assert!(!fixture.reads.lock().unwrap().contains(&(36, 46)));
}
#[tokio::test]
async fn second_scope_mixed_cycle_is_refused_before_provider_effects() {
    let mut input = two_scope_versions();
    input.insert(
        (34, 44),
        version(
            "agent",
            "Cycle",
            vec![reference((31, 41), "pipeline", "cycle")],
        ),
    );
    let fixture = Fixture::new(input);
    let (runtime, skipped) = fixture
        .build(reference((31, 41), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(runtime.is_none());
    assert_eq!(skipped.len(), 1);
    assert!(fixture.models.lock().unwrap().is_empty());
    assert_eq!(fixture.reads.lock().unwrap().last(), Some(&(31, 41)));
}
#[tokio::test]
async fn second_scope_mixed_graph_depth_is_refused_before_provider_effects() {
    let mut input = two_scope_versions();
    input.insert(
        (30, 40),
        version(
            "pipeline",
            DELEGATE_GRAPH,
            vec![reference((31, 41), "pipeline", "worker")],
        ),
    );
    input.insert(
        (29, 39),
        version(
            "pipeline",
            DELEGATE_GRAPH,
            vec![reference((30, 40), "pipeline", "worker")],
        ),
    );
    let fixture = Fixture::new(input);
    let (runtime, skipped) = fixture
        .build(reference((29, 39), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(runtime.is_none());
    assert_eq!(skipped.len(), 1);
    assert!(fixture.models.lock().unwrap().is_empty());
}
#[tokio::test]
async fn production_gate_keeps_ordinary_graph_descendants_closed() {
    let fixture = Fixture::new(versions());
    let (runtime, skipped) = fixture
        .build(reference((31, 41), "pipeline", "outer"), false)
        .await
        .unwrap();
    assert!(runtime.is_none());
    assert_eq!(skipped.len(), 1);
    assert_eq!(*fixture.reads.lock().unwrap(), vec![(31, 41)]);
    assert!(fixture.models.lock().unwrap().is_empty());
}
#[tokio::test]
async fn direct_graph_ask_user_remains_refused_under_scoped_materialization() {
    let mut graph = version(
        "pipeline",
        "state: {answer: str, messages: list}\nentry_point: ask\nnodes:\n  - id: ask\n    type: llm\n    output: [answer, messages]\n    transition: END\n",
        vec![],
    );
    graph["meta"] = json!({"internal_tools":["ask_user"]});
    let fixture = Fixture::new(BTreeMap::from([((33, 43), graph)]));
    let (runtime, skipped) = fixture
        .build(reference((33, 43), "pipeline", "ask"), true)
        .await
        .unwrap();
    assert!(runtime.is_none());
    assert_eq!(skipped.len(), 1);
    assert!(fixture.models.lock().unwrap().is_empty());
}
#[tokio::test]
async fn legacy_plain_saved_graph_remains_materialized_without_scoped_activation() {
    let fixture = Fixture::new(BTreeMap::from([(
        (33, 43),
        version("pipeline", PLAIN_GRAPH, vec![]),
    )]));
    let (runtime, skipped) = fixture
        .build(reference((33, 43), "pipeline", "plain"), false)
        .await
        .unwrap();
    assert!(skipped.is_empty());
    assert!(runtime.is_some());
    assert!(fixture.models.lock().unwrap().is_empty());
}

fn text_response(text: &str) -> Response<Body> {
    let delta = json!({"choices":[{"delta":{"content":text},"finish_reason":null}]}).to_string();
    let raw = format!(
        "data: {delta}\n\ndata: {{\"choices\":[{{\"delta\":{{}},\"finish_reason\":\"stop\"}}]}}\n\ndata: [DONE]\n\n"
    );
    test_model_gateway_response(Body::new(Full::<Bytes>::from(raw)))
}

#[tokio::test]
async fn rebuilt_saved_graph_entry_consumes_real_static_family_without_replaying_tick() {
    let original_fixture = Fixture::new(versions());
    let (runtime, skipped) = original_fixture
        .build(reference((31, 41), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(skipped.is_empty());
    let (paused, events) = execute(&runtime.unwrap()).await;
    let ids = nested_application_interrupt_ids(&paused).unwrap();
    let pause = events
        .iter()
        .find_map(|event| static_pipeline_tool_pause(event).unwrap())
        .unwrap();
    let unchanged = serde_json::to_vec(&events).unwrap();
    let meta = Map::from_iter([(
        "pipeline_static_tool_resume_v1".to_owned(),
        json!({"revision":1,"decisions":[{
            "pause_id":pause.pause_id,"child_thread_id":pause.thread_id,"tool_call_id":pause.parent_call_id,"action":"continue","value":"finish once"
        }]}),
    )]);
    let decisions = crate::agents::graph::static_tool_pause::parse_static_tool_decisions(&meta)
        .unwrap()
        .unwrap();
    let replacement = Fixture::with_outcome(versions(), text_response("ordinary resumed"));
    let (runtime, skipped) = replacement
        .build(reference((31, 41), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(skipped.is_empty());
    let runtime = runtime.unwrap();
    install_static_application_resume(&events, decisions, &runtime.presentations, &runtime.resume)
        .await
        .unwrap();
    let boundary = pipeline_application_boundary(&events, &pause.event)
        .unwrap()
        .unwrap();
    assert_real_scope_proof(&replacement, &events, &boundary).await;
    let mut replay = events[0].clone();
    replay.id = "replayed-batch".to_owned();
    replay.invocation_id = "resumed-turn".to_owned();
    replay.llm_response.provider_metadata = Some(json!({"elitea.application.replay_batch.v1":{
        "event_id":events[0].id,"interrupt_ids":ids,"call_ordinals":{"root-call":1}
    }}));
    runtime.resume.observe_call_lineage(&replay).await.unwrap();
    let result = runtime.tools[0]
        .tool
        .execute(
            Arc::new(Context {
                invocation: "resumed-turn",
                content: Content::new("user"),
                actions: StdMutex::default(),
            }),
            json!({"task":"root work"}),
        )
        .await
        .unwrap();
    assert!(nested_application_interrupt_ids(&result).is_none());
    assert!(result.to_string().contains("ordinary resumed"));
    let requests = replacement.models.lock().unwrap();
    assert_eq!(requests.len(), 1);
    let body: Value = serde_json::from_slice(&requests[0].body).unwrap();
    let messages = body["messages"].to_string();
    assert!(messages.contains("COUNT 1"));
    assert!(!messages.contains("COUNT 2"));
    assert_eq!(serde_json::to_vec(&events).unwrap(), unchanged);
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Keep the original family, fresh builder, and no-effect assertions in one ordered fixture.
async fn second_scope_fresh_builder_resumes_exact_static_leaf_and_reuses_completed_tick() {
    let original_fixture = Fixture::with_outcomes(
        two_scope_versions(),
        vec![tool_call_response(), final_tool_call_response()],
    );
    let (runtime, skipped) = original_fixture
        .build(reference((31, 41), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(skipped.is_empty());
    let (paused, events) = execute(&runtime.unwrap()).await;
    let ids = nested_application_interrupt_ids(&paused).unwrap();
    let pause = events
        .iter()
        .find_map(|event| static_pipeline_tool_pause(event).unwrap())
        .unwrap();
    let unchanged = serde_json::to_vec(&events).unwrap();
    let meta = Map::from_iter([(
        "pipeline_static_tool_resume_v1".to_owned(),
        json!({"revision":1,"decisions":[{
            "pause_id":pause.pause_id,"child_thread_id":pause.thread_id,"tool_call_id":pause.parent_call_id,"action":"continue","value":"finish once"
        }]}),
    )]);
    let decisions = crate::agents::graph::static_tool_pause::parse_static_tool_decisions(&meta)
        .unwrap()
        .unwrap();
    let replacement = Fixture::with_outcomes(
        two_scope_versions(),
        vec![
            text_response("inner ordinary resumed"),
            text_response("outer ordinary resumed"),
        ],
    );
    let (runtime, skipped) = replacement
        .build(reference((31, 41), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(skipped.is_empty());
    let runtime = runtime.unwrap();
    install_static_application_resume(&events, decisions, &runtime.presentations, &runtime.resume)
        .await
        .unwrap();
    let boundary = pipeline_application_boundary(&events, &pause.event)
        .unwrap()
        .unwrap();
    assert_real_scope_proof(&replacement, &events, &boundary).await;
    let mut replay = events[0].clone();
    replay.id = "second-scope-replayed-batch".to_owned();
    replay.invocation_id = "resumed-turn".to_owned();
    replay.llm_response.provider_metadata = Some(json!({"elitea.application.replay_batch.v1":{
        "event_id":events[0].id,"interrupt_ids":ids,"call_ordinals":{"root-call":1}
    }}));
    runtime.resume.observe_call_lineage(&replay).await.unwrap();
    let result = runtime.tools[0]
        .tool
        .execute(
            Arc::new(Context {
                invocation: "resumed-turn",
                content: Content::new("user"),
                actions: StdMutex::default(),
            }),
            json!({"task":"root work"}),
        )
        .await
        .unwrap();
    assert!(nested_application_interrupt_ids(&result).is_none());
    assert!(
        result.to_string().contains("outer ordinary resumed"),
        "{result}"
    );
    let requests = replacement.models.lock().unwrap();
    assert_eq!(requests.len(), 2);
    let bodies = requests
        .iter()
        .map(|request| serde_json::from_slice::<Value>(&request.body).unwrap())
        .collect::<Vec<_>>();
    assert!(bodies[0]["messages"].to_string().contains("COUNT 1"));
    assert!(
        !bodies
            .iter()
            .any(|body| body["messages"].to_string().contains("COUNT 2"))
    );
    assert_eq!(serde_json::to_vec(&events).unwrap(), unchanged);
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Keep the original family, fresh builder, and no-effect assertions in one ordered fixture.
async fn second_scope_proof_refuses_changed_start_child_edge_and_frame_order() {
    use crate::agents::graph::static_tool_pause::PipelineCheckpointFamilyView as _;
    let fixture = Fixture::with_outcomes(
        two_scope_versions(),
        vec![tool_call_response(), final_tool_call_response()],
    );
    let (runtime, skipped) = fixture
        .build(reference((31, 41), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(skipped.is_empty());
    let (_, events) = execute(&runtime.unwrap()).await;
    let before = serde_json::to_vec(&events).unwrap();
    let pause = events
        .iter()
        .find_map(|event| static_pipeline_tool_pause(event).unwrap())
        .unwrap();
    let boundary = pipeline_application_boundary(&events, &pause.event)
        .unwrap()
        .unwrap();
    let family =
        crate::agents::application_pipeline::decoded_pipeline_tool_family(&boundary.pending_event)
            .unwrap();
    let checkpoints = adk_rust::graph::MemoryCheckpointer::new();
    checkpoints.save(family.root()).await.unwrap();
    for path in family.catalog().unwrap().descendants.keys() {
        checkpoints
            .save(
                family
                    .checkpoint(&format!("{}/{path}", boundary.checkpoint_thread_id))
                    .unwrap(),
            )
            .await
            .unwrap();
    }
    let replacement = Fixture::new(two_scope_versions());
    let (_, rebuilt) = replacement.saved_tool_runtimes().await;
    let lineage =
        crate::agents::application_pipeline::PipelineToolCallLineage::from_call(&events[0], 0)
            .unwrap();
    let key = "elitea.pipeline.application-chain.v1";
    for (field, changed) in [
        ("original_start_event_id", json!("foreign-graph-start")),
        ("original_start_digest", json!(vec![7; 32])),
        ("child_application_call_id", json!("foreign-child-call")),
    ] {
        let mut corrupt = events.clone();
        let leaf = corrupt
            .iter_mut()
            .find(|event| event.id == pause.event.id)
            .unwrap();
        let mut raw: Value = serde_json::from_str(&leaf.provider_metadata[key]).unwrap();
        raw["outer"][0][field] = changed;
        leaf.provider_metadata
            .insert(key.to_owned(), raw.to_string());
        let selected = leaf.clone();
        if let Ok(Some(candidate)) = pipeline_application_boundary(&corrupt, &selected) {
            assert!(
                rebuilt
                    .application_scopes()
                    .unwrap()
                    .validate_saved_tool_continuation(
                        &candidate.checkpoint_thread_id,
                        &checkpoints,
                        &candidate.scope_route,
                        &candidate.scope_events,
                        &lineage
                    )
                    .await
                    .is_err(),
                "changed {field} must fail exact parent proof"
            );
        }
    }
    let mut duplicate = pause.event.clone();
    let mut raw: Value = serde_json::from_str(&duplicate.provider_metadata[key]).unwrap();
    let frame = raw["outer"][0].clone();
    raw["outer"].as_array_mut().unwrap().push(frame);
    duplicate
        .provider_metadata
        .insert(key.to_owned(), raw.to_string());
    assert!(
        crate::agents::pipeline::scoped_applications::activations_for_event(&duplicate).is_err()
    );
    let mut swapped = pause.event.clone();
    let ledger_key = crate::agents::application_pipeline::BOUNDARY_LEDGER_KEY;
    let mut raw: Value = serde_json::from_str(&swapped.provider_metadata[ledger_key]).unwrap();
    raw["outer"].as_array_mut().unwrap().reverse();
    swapped
        .provider_metadata
        .insert(ledger_key.to_owned(), raw.to_string());
    assert!(crate::agents::application_pipeline::projected_outer_boundaries(&swapped).is_err());
    assert_eq!(serde_json::to_vec(&events).unwrap(), before);
    assert!(replacement.models.lock().unwrap().is_empty());
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Keep the original family, fresh builder, and no-effect assertions in one ordered fixture.
async fn second_scope_resume_refuses_inner_frozen_graph_drift_before_leaf_execution() {
    use crate::agents::graph::static_tool_pause::PipelineCheckpointFamilyView as _;
    let fixture = Fixture::with_outcomes(
        two_scope_versions(),
        vec![tool_call_response(), final_tool_call_response()],
    );
    let (runtime, skipped) = fixture
        .build(reference((31, 41), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(skipped.is_empty());
    let (paused, events) = execute(&runtime.unwrap()).await;
    let ids = nested_application_interrupt_ids(&paused).unwrap();
    let pause = events
        .iter()
        .find_map(|event| static_pipeline_tool_pause(event).unwrap())
        .unwrap();
    let meta = Map::from_iter([(
        "pipeline_static_tool_resume_v1".to_owned(),
        json!({"revision":1,"decisions":[{
            "pause_id":pause.pause_id,"child_thread_id":pause.thread_id,"tool_call_id":pause.parent_call_id,"action":"continue","value":"finish once"
        }]}),
    )]);
    let decisions = crate::agents::graph::static_tool_pause::parse_static_tool_decisions(&meta)
        .unwrap()
        .unwrap();
    let mut changed = two_scope_versions();
    let instructions = DELEGATE_GRAPH.replace("transition: END", "transition: finish")
        + "  - id: finish\n    type: state_modifier\n    template: 'changed {{ answer }}'\n    input: [answer]\n    output: [answer]\n    transition: END\n";
    changed.get_mut(&(33, 43)).unwrap()["instructions"] = json!(instructions);
    let replacement = Fixture::with_outcomes(changed, vec![text_response("refusal observed")]);
    let recorded = events
        .iter()
        .find_map(|event| {
            let family =
                crate::agents::application_pipeline::decoded_pipeline_tool_family(event).ok()?;
            (family
                .catalog()
                .and_then(|catalog| catalog.root.as_ref())
                .is_some_and(|revision| revision.application_id == 33))
            .then_some(family)
        })
        .unwrap();
    let (_, rebuilt) = replacement.saved_tool_runtimes_for((33, 43)).await;
    assert!(rebuilt.checkpoint_catalog() != recorded.catalog());
    let (runtime, skipped) = replacement
        .build(reference((31, 41), "pipeline", "outer"), true)
        .await
        .unwrap();
    assert!(skipped.is_empty());
    let runtime = runtime.unwrap();
    install_static_application_resume(&events, decisions, &runtime.presentations, &runtime.resume)
        .await
        .unwrap();
    let mut replay = events[0].clone();
    replay.id = "drift-replayed-batch".to_owned();
    replay.invocation_id = "resumed-turn".to_owned();
    replay.llm_response.provider_metadata = Some(json!({"elitea.application.replay_batch.v1":{
        "event_id":events[0].id,"interrupt_ids":ids,"call_ordinals":{"root-call":1}
    }}));
    runtime.resume.observe_call_lineage(&replay).await.unwrap();
    let result = runtime.tools[0]
        .tool
        .execute(
            Arc::new(Context {
                invocation: "resumed-turn",
                content: Content::new("user"),
                actions: StdMutex::default(),
            }),
            json!({"task":"root work"}),
        )
        .await
        .unwrap();
    assert!(result.to_string().contains("refusal observed"));
    let requests = replacement.models.lock().unwrap();
    assert_eq!(
        requests.len(),
        1,
        "only the unchanged outer Agent may complete"
    );
    let body: Value = serde_json::from_slice(&requests[0].body).unwrap();
    let messages = body["messages"].to_string();
    assert!(messages.contains("tool.invalid_input"));
    assert!(!messages.contains("COUNT 1"));
}

async fn assert_real_scope_proof(
    replacement: &Fixture,
    events: &[Event],
    boundary: &PipelineApplicationBoundary,
) {
    use crate::agents::graph::static_tool_pause::PipelineCheckpointFamilyView as _;
    let family =
        crate::agents::application_pipeline::decoded_pipeline_tool_family(&boundary.pending_event)
            .unwrap();
    let root_checkpoint = family.root().clone();
    let recorded_catalog = family.catalog().unwrap().clone();
    let descendant_checkpoints = recorded_catalog
        .descendants
        .keys()
        .filter_map(|path| {
            family
                .checkpoint(&format!("{}/{path}", root_checkpoint.thread_id))
                .cloned()
        })
        .collect::<Vec<_>>();
    let receipt = crate::agents::pipeline::scope_receipts::receipt_for_node(
        &root_checkpoint,
        &boundary.scope_route.leaf_node_name,
    )
    .unwrap()
    .unwrap();
    assert_eq!(
        root_checkpoint.step,
        receipt.activation().step(),
        "real family step differs from its original activation"
    );
    let (definition, actual) = replacement.saved_tool_runtimes().await;
    let registry = actual.application_scopes().unwrap();
    registry
        .validate_catalog(&definition, actual.checkpoint_catalog().unwrap())
        .unwrap();
    assert!(
        actual.checkpoint_catalog() == Some(&recorded_catalog),
        "rebuilt catalog differs from the real persisted family"
    );
    let checkpoints = adk_rust::graph::MemoryCheckpointer::new();
    checkpoints.save(&root_checkpoint).await.unwrap();
    for checkpoint in &descendant_checkpoints {
        checkpoints.save(checkpoint).await.unwrap();
    }
    let lineage =
        crate::agents::application_pipeline::PipelineToolCallLineage::from_call(&events[0], 0)
            .unwrap();
    let start = boundary
        .scope_events
        .iter()
        .find(|event| {
            event
                .tool_calls()
                .iter()
                .any(|call| call.call_id == Some(boundary.scope_route.application_call_id.as_str()))
        })
        .unwrap();
    assert_static_receipt_edges(
        start,
        receipt.original_start(),
        &boundary.checkpoint_thread_id,
        &lineage,
    );
    assert!(
        registry
            .validate_continuation(
                &boundary.checkpoint_thread_id,
                &checkpoints,
                &boundary.scope_route,
                &boundary.scope_events
            )
            .await
            .is_err(),
        "a native-root installer must refuse the outer transport receipt"
    );
    registry
        .validate_saved_tool_continuation(
            &boundary.checkpoint_thread_id,
            &checkpoints,
            &boundary.scope_route,
            &boundary.scope_events,
            &lineage,
        )
        .await
        .expect("actual rebuilt registry must prove the real original scope family");
}

fn assert_static_receipt_edges(
    event: &Event,
    original: &Event,
    thread: &str,
    lineage: &crate::agents::application_pipeline::PipelineToolCallLineage,
) {
    use crate::agents::application_pipeline::{
        STATIC_TOOL_THREAD_METADATA_KEY, validate_static_thread_for_saved_start,
    };
    assert!(
        event
            .provider_metadata
            .contains_key(STATIC_TOOL_THREAD_METADATA_KEY)
    );
    assert!(
        !original
            .provider_metadata
            .contains_key(STATIC_TOOL_THREAD_METADATA_KEY)
    );
    validate_static_thread_for_saved_start(event, thread, lineage).unwrap();
    let raw = event
        .provider_metadata
        .get(STATIC_TOOL_THREAD_METADATA_KEY)
        .unwrap();
    for (field, value) in [
        ("original_batch_event_id", json!("foreign-batch")),
        ("original_ordinal", json!(2)),
        (
            "arguments_digest",
            json!(format!("sha256:{}", "0".repeat(64))),
        ),
    ] {
        let mut encoded: Value = serde_json::from_str(raw).unwrap();
        encoded["lineage"][field] = value;
        let mut forged = event.clone();
        forged.provider_metadata.insert(
            STATIC_TOOL_THREAD_METADATA_KEY.to_owned(),
            encoded.to_string(),
        );
        assert!(validate_static_thread_for_saved_start(&forged, thread, lineage).is_err());
    }
    let mut forged = event.clone();
    forged.provider_metadata.insert(
        DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(),
        format!("{thread}-foreign"),
    );
    assert!(validate_static_thread_for_saved_start(&forged, thread, lineage).is_err());
    assert!(
        validate_static_thread_for_saved_start(event, &format!("{thread}-foreign"), lineage)
            .is_err()
    );
}
