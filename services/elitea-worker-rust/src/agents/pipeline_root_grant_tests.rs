//! FU-335: a pipeline root's delegation grant ceiling.
//!
//! Every run test assembles through the real `PipelineNativeAgentAssembler::assemble`
//! (`bind_node_runtimes`, the frozen composition admission, the scoped resolver)
//! under an ENFORCING delegation policy: the shared fixture authority reports
//! narrowing only (`test_runtime_context_authority`), which is why no earlier
//! pipeline test met this. Only the platform RPC, the MCP server and the model
//! gateway are scripted. Map members run on the production PostgreSQL
//! assembler (`ELITEA_TEST_DATABASE_URL`).

use super::scoped_tests::{
    BatchConnector, Saved, reference, root_yaml, saved_version, text, tool_call_response,
};
use super::*;
use crate::protocol::output::RuntimeFailureKind;
use elitea_agent_runtime::delegation::{
    DelegationGrant, DelegationPolicy, DelegationScope, EdgeKey, EdgeKind, GrantSet, NarrowingMode,
    PrincipalBound, PrincipalToolkit,
};
use std::collections::BTreeMap;

const BATCH_URL: &str = "https://mcp.example.invalid/v1/mcp";
const UNUSED_URL: &str = "https://unused.mcp.example.invalid/mcp";

fn mcp_toolkit_at(id: u64, name: &str, url: &str) -> Value {
    json!({"id": id, "type": "mcp", "toolkit_name": name,
        "settings": {"url": url, "headers": null,
            "client_id": null, "client_secret": null, "scopes": null, "timeout": 30,
            "selected_tools": ["get_issues", "echo", "reverse"], "enable_caching": true,
            "cache_ttl": 300, "ssl_verify": true}})
}

/// The MCP toolkit row every level of the reported shape attaches.
fn mcp_toolkit() -> Value {
    mcp_toolkit_at(92, "batch", BATCH_URL)
}

/// Saved pipeline P1: one direct MCP node on the toolkit.
const MCP_NODE_PIPELINE: &str = "state:\n  input: str\n  messages: list\n  records: dict\nentry_point: direct\nnodes:\n  - id: direct\n    type: mcp\n    toolkit_name: batch\n    tool: echo\n    input_mapping:\n      release: {type: fixed, value: '1.2'}\n    output: [records]\n    transition: END\n";

/// Saved pipeline P2: one Agent node that runs the saved agent `inner`.
const AGENT_NODE_PIPELINE: &str = "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: inner\n    input_mapping:\n      task: {type: variable, value: input}\n    output: [answer]\n    transition: END\n";

/// A Map root whose member worker is the saved Agent `worker`.
const MAP_ROOT: &str = "state:\n  items: {type: list, value: [alpha]}\n  answer: {type: str, value: ''}\n  mapped: {type: list, value: []}\n  messages: {type: list, value: []}\nentry_point: each\nnodes:\n  - {id: each, type: map, worker: member, source: items, item: entity, index: item_index, outputs: [answer], destination: mapped, max_items: 4, max_concurrency: 1, reduction: ordered_collection, transition: END}\n  - {id: member, type: agent, tool: worker, input: [entity, item_index], input_mapping: {task: {type: fixed, value: partition}}, output: [answer]}\n";

const DEFAULT_DEPTH: u32 = elitea_agent_runtime::delegation::DEFAULT_MAX_DEPTH;

fn enforcing_policy(max_depth: u32) -> DelegationPolicy {
    DelegationPolicy::new(max_depth, NarrowingMode::Enforce, []).expect("enforcing policy")
}

fn enforcing(
    request: &super::super::request::AgentExecutionRequest,
) -> AuthorizedNativeAssembly<'_> {
    enforcing_at_depth(request, DEFAULT_DEPTH)
}

fn enforcing_at_depth(
    request: &super::super::request::AgentExecutionRequest,
    max_depth: u32,
) -> AuthorizedNativeAssembly<'_> {
    under(request, enforcing_policy(max_depth))
}

/// The claim of `request` under `policy`.
fn under(
    request: &super::super::request::AgentExecutionRequest,
    policy: DelegationPolicy,
) -> AuthorizedNativeAssembly<'_> {
    AuthorizedNativeAssembly::new(
        request,
        test_runtime_context_authority().with_delegation_policy_for_test(policy),
        AuthorizedNativeCommandBinding::fixture(),
    )
}

fn root_request(yaml: &str, tools: Vec<Value>) -> super::super::request::AgentExecutionRequest {
    let mut request = pipeline_request();
    let version = request
        .payload
        .application
        .get_mut("version_details")
        .and_then(Value::as_object_mut)
        .expect("application version fixture");
    version.insert("instructions".to_owned(), json!(yaml));
    version.insert("tools".to_owned(), Value::Array(tools));
    request
}

/// Main's answers for saved versions, keyed by identity, each carrying the
/// authenticated `frozen_definition_sha256` a real Main returns (a Map binds
/// its saved member by it). Every route asked is recorded.
struct SavedVersions {
    versions: BTreeMap<Saved, Value>,
    routes: Arc<Mutex<Vec<String>>>,
    memory_calls: Arc<Mutex<Vec<(String, Value)>>>,
}

#[tonic::async_trait]
impl ContextRoutes for SavedVersions {
    async fn answer(&self, call: ContextCall) -> RuntimeReply {
        let unavailable = || tonic::Status::unavailable("saved version fixture");
        self.routes
            .lock()
            .map_err(|_| unavailable())?
            .push(call.route.clone());
        let value = if call.route == "elitea-client-token" {
            json!({"schema_version":"elitea.runtime.elitea-client-token.v1","project_id":17,
                "token":"stub"})
        } else if call.route == "artifacts/list" {
            json!({
                "schema_version": "elitea.runtime.artifact-list.v1",
                "project_id": 17,
                "bucket": "docs",
                "files": [{
                    "name": "ok.txt",
                    "byte_length": 5,
                    "media_type": "text/plain",
                    "modified_at": "2026-11-22T00:00:00Z"
                }],
                "truncated": false
            })
        } else if call.route == "attachments/docs/ok.txt" {
            json!({
                "schema_version": "elitea.runtime.attachment-object.v1",
                "project_id": 17,
                "bucket": "docs",
                "name": "ok.txt",
                "media_type": "text/plain",
                "byte_length": 19,
                "content": "untrusted file body"
            })
        } else if call.route == "agent-memory/remember" {
            let body: Value = serde_json::from_slice(&call.body).unwrap_or(Value::Null);
            self.memory_calls
                .lock()
                .map_err(|_| unavailable())?
                .push((call.route.clone(), body));
            json!({
                "schema_version": "elitea.runtime.agent-memory-remember.v1",
                "project_id": 17,
                "entry_id": "55",
                "status": "pending"
            })
        } else if call.route == "agent-memory/recall" {
            let body: Value = serde_json::from_slice(&call.body).unwrap_or(Value::Null);
            self.memory_calls
                .lock()
                .map_err(|_| unavailable())?
                .push((call.route.clone(), body));
            json!({
                "schema_version": "elitea.runtime.agent-memory-recall.v1",
                "project_id": 17,
                "entries": []
            })
        } else {
            let parts = call.route.split('/').collect::<Vec<_>>();
            let identity = parts
                .get(1)
                .and_then(|value| value.parse::<u64>().ok())
                .zip(parts.get(3).and_then(|value| value.parse::<u64>().ok()))
                .filter(|_| {
                    parts.len() == 4 && parts[0] == "applications" && parts[2] == "versions"
                })
                .ok_or_else(unavailable)?;
            let details = self.versions.get(&identity).ok_or_else(unavailable)?;
            json!({"schema_version":"elitea.runtime.application-version.v1","project_id":17,
                "application_id":identity.0,"version_id":identity.1,"version_details":details,
                "frozen_definition_sha256":format!("{:02x}", identity.0).repeat(32)})
        };
        runtime_response(&value)
    }
}

/// The batch fixture toolset, recording the endpoint of every dial.
#[derive(Clone, Default)]
struct DialRecorder(Arc<Mutex<Vec<String>>>);

#[async_trait]
impl McpConnector for DialRecorder {
    async fn connect(
        &self,
        config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        self.0
            .lock()
            .expect("dial log")
            .push(config.endpoint().to_owned());
        BatchConnector.connect(config).await
    }
}

impl DialRecorder {
    fn dials_to(&self, url: &str) -> usize {
        self.0
            .lock()
            .expect("dial log")
            .iter()
            .filter(|endpoint| endpoint.as_str() == url)
            .count()
    }
}

struct Clients {
    platform: Arc<PlatformClient>,
    model_facade: Arc<ModelFacade>,
    captured: CapturedModelRequests,
    routes: Arc<Mutex<Vec<String>>>,
    memory_calls: Arc<Mutex<Vec<(String, Value)>>>,
}

fn runtime_clients(
    versions: &BTreeMap<Saved, Value>,
    outcomes: Vec<TestModelGatewayOutcome>,
) -> Clients {
    let routes = Arc::new(Mutex::new(Vec::new()));
    let memory_calls = Arc::new(Mutex::new(Vec::new()));
    let runtime = RuntimeContextClient::with_rpc(
        Routes(SavedVersions {
            versions: versions.clone(),
            routes: Arc::clone(&routes),
            memory_calls: Arc::clone(&memory_calls),
        }),
        RuntimeContextConfig {
            origin: "https://content.internal".to_owned(),
            deadline: Duration::from_secs(1),
            max_response_bytes: 32 * 1_024,
            max_application_response_bytes: 1_024 * 1_024,
            max_attachment_response_bytes: 1_024 * 1_024,
            max_artifact_response_bytes: 2 * 1_024 * 1_024,
        },
    )
    .expect("saved-version runtime-context fixture");
    let (gateway, captured) = test_model_gateway_client(outcomes, test_model_gateway_config())
        .expect("scripted model gateway");
    Clients {
        platform: Arc::new(PlatformClient::new(Arc::new(runtime))),
        model_facade: Arc::new(ModelFacade::from_gateway(gateway)),
        captured,
        routes,
        memory_calls,
    }
}

struct Fixture {
    assembler: PipelineNativeAgentAssembler,
    captured: CapturedModelRequests,
    dials: DialRecorder,
    routes: Arc<Mutex<Vec<String>>>,
    memory_calls: Arc<Mutex<Vec<(String, Value)>>>,
}

fn fixture(versions: &BTreeMap<Saved, Value>, outcomes: Vec<TestModelGatewayOutcome>) -> Fixture {
    let clients = runtime_clients(versions, outcomes);
    let dials = DialRecorder::default();
    Fixture {
        assembler: PipelineNativeAgentAssembler::with_state(
            Arc::new(InMemorySessionService::new()) as Arc<dyn SessionService>,
            Arc::new(MemoryCheckpointer::new()) as Arc<dyn Checkpointer>,
        )
        .with_runtime_clients(clients.platform, clients.model_facade)
        .with_mcp_connector(Arc::new(dials.clone())),
        captured: clients.captured,
        dials,
        routes: clients.routes,
        memory_calls: clients.memory_calls,
    }
}

fn model_calls(captured: &CapturedModelRequests) -> usize {
    captured.lock().expect("captured model requests").len()
}

fn model_request(captured: &CapturedModelRequests, index: usize) -> Value {
    let requests = captured.lock().expect("captured model requests");
    serde_json::from_slice(&requests[index].body).expect("model request JSON")
}

fn offered_tools(captured: &CapturedModelRequests, request: usize) -> Vec<String> {
    if model_calls(captured) <= request {
        return Vec::new();
    }
    model_request(captured, request)["tools"]
        .as_array()
        .map(|tools| {
            tools
                .iter()
                .filter_map(|tool| tool["function"]["name"].as_str().map(str::to_owned))
                .collect()
        })
        .unwrap_or_default()
}

/// What one run ended with: its browser events, or the run error's code,
/// upstream code and the failure message a user would read.
#[derive(Debug)]
struct Outcome {
    browser: Vec<Value>,
    failure: Option<String>,
}

impl Outcome {
    fn says(&self, content: &str) -> bool {
        self.browser.iter().any(|event| event["content"] == content)
    }
}

async fn run(mut invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>) -> Outcome {
    invocation
        .project_start(timestamp(0))
        .expect("pipeline browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("pipeline start");
    let mut browser = Vec::new();
    loop {
        match run.next_event().await {
            Ok(Some(event)) => browser.extend(
                projector
                    .project(&event)
                    .expect("pipeline projection")
                    .into_iter()
                    .map(|event| current(&event)),
            ),
            Ok(None) => break,
            Err(error) => {
                return Outcome {
                    browser,
                    failure: Some(format!(
                        "run error code={:?} upstream={:?} message={:?}",
                        error.code(),
                        error.upstream_code(),
                        error.failure_message()
                    )),
                };
            }
        }
    }
    match completion.select().await {
        Ok(completed) => {
            browser.extend(
                projector
                    .finish_after_eos(completed, timestamp(2))
                    .expect("pipeline completion")
                    .into_iter()
                    .map(|event| current(&event)),
            );
            Outcome {
                browser,
                failure: None,
            }
        }
        Err(error) => Outcome {
            browser,
            failure: Some(format!("completion error {error}")),
        },
    }
}

fn describe(error: &super::super::runtime::NativeAgentAssemblyError) -> String {
    format!(
        "assembly error code={:?} message={error} cause={:?} delegation={:?}",
        error.code(),
        error.cause().map(|cause| (cause.code(), cause.detail())),
        error.delegation_notice().map(|notice| &notice.value)
    )
}

async fn completed(
    fixture: &Fixture,
    request: &super::super::request::AgentExecutionRequest,
) -> Outcome {
    completed_under(fixture, request, enforcing_policy(DEFAULT_DEPTH)).await
}

async fn completed_under(
    fixture: &Fixture,
    request: &super::super::request::AgentExecutionRequest,
    policy: DelegationPolicy,
) -> Outcome {
    let invocation = match fixture.assembler.assemble(under(request, policy)).await {
        Ok(invocation) => invocation,
        Err(error) => panic!("the run was refused at start: {}", describe(&error)),
    };
    let outcome = run(invocation).await;
    assert_eq!(outcome.failure, None, "{outcome:?}");
    outcome
}

/// The run ends at start, before any model call or dial, as #2235's refused
/// pipeline node does: `AUTHORIZATION_FAILED` (`runtime.authorization_failed`)
/// with the refusal code as its cause, carrying Main's `DelegationNotice`
/// (edge kind `pipeline_node`, keyed by the node's graph path, naming the
/// callee's id for Review delegation), which the lifecycle publishes first.
fn assert_refused_with_notice(
    error: &super::super::runtime::NativeAgentAssemblyError,
    graph_path: &str,
    reason: &str,
    toolkit: &str,
) {
    assert_refused_with_notice_from(error, graph_path, (reason, toolkit), None);
}

/// [`assert_refused_with_notice`] for a run that carries grants: the edge then
/// also names its caller, the root (`caller_id`).
fn assert_refused_with_notice_from(
    error: &super::super::runtime::NativeAgentAssemblyError,
    graph_path: &str,
    (reason, toolkit): (&str, &str),
    caller: Option<u64>,
) {
    assert_eq!(
        error.code(),
        super::super::runtime::NativeAgentAssemblyErrorCode::AuthorizationFailed,
        "{}",
        describe(error)
    );
    assert_eq!(
        error.cause().map(|cause| (cause.code(), cause.detail())),
        Some(("DELEGATION_GRANT_MISSING", Some("toolkit"))),
        "{}",
        describe(error)
    );
    assert_eq!(
        crate::execution::assembly_failure(error),
        RuntimeFailureKind::AuthorizationFailed,
        "{}",
        describe(error)
    );
    let notice = error
        .delegation_notice()
        .unwrap_or_else(|| panic!("no delegation notice: {}", describe(error)));
    let node = graph_path.rsplit('/').next().unwrap_or_default();
    assert_eq!(notice.event, "delegation_refused", "{}", describe(error));
    let mut edge = json!({"callee_name": node, "kind": "pipeline_node", "callee_id": 3});
    if let Some(caller) = caller {
        edge["caller_id"] = json!(caller);
    }
    assert_eq!(
        notice.value,
        json!({"kind": "refused", "code": "DELEGATION_GRANT_MISSING", "reason": reason,
            "missing": [{"kind": "toolkit", "name": toolkit}],
            "id": format!("pipeline_node:{graph_path}"), "edge": edge}),
        "{}",
        describe(error)
    );
}

fn refusal_in_answer_of(captured: &CapturedModelRequests, request: usize) -> String {
    model_request(captured, request)["messages"].to_string()
}

// ---- (a) the ceiling ----

/// The pipeline root attaches the MCP toolkit but no node of its own uses
/// it; its Agent node runs the saved Agent S that holds the same toolkit. S
/// is delegated (root ⊇ S) and runs with its tools. Failed first on fdab93771.
#[tokio::test(flavor = "current_thread")]
async fn a_pipeline_root_delegates_an_attached_but_unused_toolkit_to_its_agent_node() {
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Use the toolkit.", &json!({}), vec![mcp_toolkit()]),
    )]);
    let fixture = fixture(&versions, vec![text("worker answer")]);
    let request = root_request(
        &root_yaml("worker"),
        vec![mcp_toolkit(), reference((3, 4), "agent", "worker")],
    );
    let outcome = completed(&fixture, &request).await;
    assert!(outcome.says("worker answer"), "{outcome:?}");
    let offered = offered_tools(&fixture.captured, 0);
    assert!(
        ["get_issues", "echo", "reverse"]
            .iter()
            .all(|tool| offered.contains(&(*tool).to_owned())),
        "S runs with the toolkit its caller delegated: {offered:?}"
    );
}

/// The reported shape (chat 899): root -> Agent node -> S -> pipeline tool
/// P1 -> MCP node, each level attaching the toolkit. It completes, and P1's
/// MCP node really ran. Failed first on fdab93771.
#[tokio::test(flavor = "current_thread")]
async fn the_reported_root_agent_pipeline_tool_mcp_shape_completes() {
    let versions = BTreeMap::from([
        (
            (3, 4),
            saved_version(
                "agent",
                "Call the pipeline.",
                &json!({}),
                vec![mcp_toolkit(), reference((7, 8), "pipeline", "p1")],
            ),
        ),
        (
            (7, 8),
            saved_version(
                "pipeline",
                MCP_NODE_PIPELINE,
                &json!({}),
                vec![mcp_toolkit()],
            ),
        ),
    ]);
    let fixture = fixture(
        &versions,
        vec![
            TestModelGatewayOutcome::Response(tool_call_response(
                "p1-call",
                "elitea_agent_7_v_8",
                &json!({"task":"echo it"}),
            )),
            text("relayed p1"),
        ],
    );
    let request = root_request(
        &root_yaml("worker"),
        vec![mcp_toolkit(), reference((3, 4), "agent", "worker")],
    );
    let outcome = completed(&fixture, &request).await;
    assert!(outcome.says("relayed p1"), "{outcome:?}");
    assert_eq!(
        model_calls(&fixture.captured),
        2,
        "S calls P1, then answers"
    );
    let answer = refusal_in_answer_of(&fixture.captured, 1);
    assert!(
        !answer.contains("DELEGATION_GRANT_MISSING") && !answer.contains("\\\"failure\\\""),
        "P1's MCP node ran and its result reached S: {answer}"
    );
    assert!(
        answer.contains("ok") && answer.contains("response"),
        "{answer}"
    );
    assert_eq!(
        fixture.dials.dials_to(BATCH_URL),
        2,
        "S's own toolkit and P1's MCP node were both dialled"
    );
}

// ---- (b) readable refusals ----

/// A root that does not hold the toolkit refuses S by name: the run ends at
/// start with #2235's authorization terminal and Main's notice naming `batch`,
/// before any model call or dial. On main the bound Agent node is offered the
/// refusal as a tool no node can answer and the run ends `agent.legacy`.
#[tokio::test(flavor = "current_thread")]
async fn a_root_agent_node_refused_a_toolkit_ends_typed_naming_it() {
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Use the toolkit.", &json!({}), vec![mcp_toolkit()]),
    )]);
    let fixture = fixture(&versions, vec![text("unexpected")]);
    let request = root_request(
        &root_yaml("worker"),
        vec![reference((3, 4), "agent", "worker")],
    );
    let Err(error) = fixture.assembler.assemble(enforcing(&request)).await else {
        panic!("a refused saved Agent must end the run at start");
    };
    assert_refused_with_notice(&error, "delegate", "no_grant", "batch");
    assert_eq!(model_calls(&fixture.captured), 0);
    assert_eq!(fixture.dials.dials_to(BATCH_URL), 0);
}

// ---- negative authorization ----

/// A toolkit attached only to a descendant (P1), not to the root or S, is
/// refused at its own hop (S -> P1): P1 is offered as a refusal, S reads it
/// by name, and the run completes. The descendant never widens a caller.
#[tokio::test(flavor = "current_thread")]
async fn a_toolkit_held_only_by_a_descendant_is_refused_at_its_own_hop() {
    descendant_refused_at_its_hop(Vec::new()).await;
}

/// Never transitive: the root holds the toolkit, S does not, P1 does. The
/// root's grant does not pass through S, so P1 is refused at S -> P1.
#[tokio::test(flavor = "current_thread")]
async fn the_root_ceiling_never_passes_through_a_caller_that_lacks_the_toolkit() {
    descendant_refused_at_its_hop(vec![mcp_toolkit()]).await;
}

async fn descendant_refused_at_its_hop(mut root_tools: Vec<Value>) {
    let versions = BTreeMap::from([
        (
            (3, 4),
            saved_version(
                "agent",
                "Call the pipeline.",
                &json!({}),
                vec![reference((7, 8), "pipeline", "p1")],
            ),
        ),
        (
            (7, 8),
            saved_version(
                "pipeline",
                MCP_NODE_PIPELINE,
                &json!({}),
                vec![mcp_toolkit()],
            ),
        ),
    ]);
    let fixture = fixture(
        &versions,
        vec![
            TestModelGatewayOutcome::Response(tool_call_response(
                "p1-call",
                "elitea_agent_7_v_8",
                &json!({"task":"echo it"}),
            )),
            text("p1 was refused"),
        ],
    );
    root_tools.push(reference((3, 4), "agent", "worker"));
    let request = root_request(&root_yaml("worker"), root_tools);
    let outcome = completed(&fixture, &request).await;
    assert!(outcome.says("p1 was refused"), "{outcome:?}");
    let answer = refusal_in_answer_of(&fixture.captured, 1);
    assert!(
        answer.contains("DELEGATION_GRANT_MISSING") && answer.contains("'batch'"),
        "S reads P1's refusal by name: {answer}"
    );
    assert_eq!(
        fixture.dials.dials_to(BATCH_URL),
        0,
        "P1's MCP node never ran"
    );
}

/// A toolkit the tool policy removes (`apply_policy`) is not in the ceiling:
/// attaching it to the root does not delegate it.
#[tokio::test(flavor = "current_thread")]
async fn a_toolkit_removed_by_policy_is_not_granted() {
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Use the toolkit.", &json!({}), vec![mcp_toolkit()]),
    )]);
    let mut fixture = fixture(&versions, vec![text("unexpected")]);
    fixture.assembler = fixture.assembler.with_tool_policy(Arc::new(
        ToolAdmissionPolicy::new(&["mcp".to_owned()], &BTreeMap::new()).expect("policy"),
    ));
    let request = root_request(
        &root_yaml("worker"),
        vec![mcp_toolkit(), reference((3, 4), "agent", "worker")],
    );
    let Err(error) = fixture.assembler.assemble(enforcing(&request)).await else {
        panic!("a policy-removed toolkit must not be delegated");
    };
    assert_refused_with_notice(&error, "delegate", "no_grant", "batch");
    assert_eq!(model_calls(&fixture.captured), 0);
    assert_eq!(fixture.dials.dials_to(BATCH_URL), 0);
}

/// A saved Agent of another project is refused before any effect, although
/// the root holds the toolkit it needs: the wider ceiling admits no foreign
/// child.
#[tokio::test(flavor = "current_thread")]
async fn a_foreign_project_agent_is_refused_although_the_root_holds_its_toolkit() {
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Use the toolkit.", &json!({}), vec![mcp_toolkit()]),
    )]);
    let fixture = fixture(&versions, vec![text("unexpected")]);
    // The ad hoc reference shape, the only one that names a project.
    let foreign = json!({"id":null,"type":"application","name":"worker","toolkit_name":"worker",
        "description":"A saved application.","author_id":11,"created_at":"2026-08-21T10:00:00Z",
        "settings":{"application_id":3,"application_version_id":4,
            "variables":[],"selected_tools":[]},
        "project_id":99,"participant_id":7,"agent_type":"agent"});
    let request = root_request(&root_yaml("worker"), vec![mcp_toolkit(), foreign]);
    let Err(error) = fixture.assembler.assemble(enforcing(&request)).await else {
        panic!("a foreign-project saved Agent must not run");
    };
    assert!(error.delegation_notice().is_none(), "{}", describe(&error));
    assert_eq!(model_calls(&fixture.captured), 0);
    assert_eq!(fixture.dials.dials_to(BATCH_URL), 0);
    assert!(
        !fixture
            .routes
            .lock()
            .expect("routes")
            .iter()
            .any(|route| route.starts_with("applications/3/")),
        "the foreign version is never read"
    );
}

/// C1: credentials and connections are materialized only for toolkits a node,
/// or a delegated descendant, uses. The root attaches `unused` (no node, no
/// descendant uses it): it is never dialled, so nothing is redeemed for it,
/// while the delegated toolkit is.
#[tokio::test(flavor = "current_thread")]
async fn an_attached_toolkit_nothing_uses_is_never_dialled() {
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Use the toolkit.", &json!({}), vec![mcp_toolkit()]),
    )]);
    let fixture = fixture(&versions, vec![text("worker answer")]);
    let request = root_request(
        &root_yaml("worker"),
        vec![
            mcp_toolkit(),
            mcp_toolkit_at(93, "unused", UNUSED_URL),
            reference((3, 4), "agent", "worker"),
        ],
    );
    let outcome = completed(&fixture, &request).await;
    assert!(outcome.says("worker answer"), "{outcome:?}");
    assert!(
        fixture.dials.dials_to(BATCH_URL) >= 1,
        "the delegated toolkit is dialled for S"
    );
    assert_eq!(
        fixture.dials.dials_to(UNUSED_URL),
        0,
        "the unused toolkit never is"
    );
}

/// The depth cap is unchanged: under a maximum of 1, root -> S runs and
/// S -> P1 (depth 2) is refused as a tool S reads.
#[tokio::test(flavor = "current_thread")]
async fn the_delegation_depth_cap_is_unchanged() {
    let versions = BTreeMap::from([
        (
            (3, 4),
            saved_version(
                "agent",
                "Call the pipeline.",
                &json!({}),
                vec![mcp_toolkit(), reference((7, 8), "pipeline", "p1")],
            ),
        ),
        (
            (7, 8),
            saved_version(
                "pipeline",
                MCP_NODE_PIPELINE,
                &json!({}),
                vec![mcp_toolkit()],
            ),
        ),
    ]);
    let fixture = fixture(
        &versions,
        vec![
            TestModelGatewayOutcome::Response(tool_call_response(
                "p1-call",
                "elitea_agent_7_v_8",
                &json!({"task":"echo it"}),
            )),
            text("too deep"),
        ],
    );
    let request = root_request(
        &root_yaml("worker"),
        vec![mcp_toolkit(), reference((3, 4), "agent", "worker")],
    );
    let invocation = match fixture
        .assembler
        .assemble(enforcing_at_depth(&request, 1))
        .await
    {
        Ok(invocation) => invocation,
        Err(error) => panic!("depth 1 admits S: {}", describe(&error)),
    };
    let outcome = run(invocation).await;
    assert_eq!(outcome.failure, None, "{outcome:?}");
    assert!(outcome.says("too deep"), "{outcome:?}");
    assert!(
        refusal_in_answer_of(&fixture.captured, 1).contains("DELEGATION_DEPTH_EXCEEDED"),
        "P1 is refused past the depth"
    );
    assert_eq!(fixture.dials.dials_to(BATCH_URL), 1, "S's own toolkit only");
}

/// C1 (a): an ordinary root and a pipeline root with the same attachments
/// hold the same ceiling. Both roots admit their tools the same way
/// (`FrozenToolSnapshot::from_request(..).apply_policy(..)`, `runtime.rs`); the
/// pipeline side is taken from its real admission. The alias-reduced snapshot
/// (the credential set, the pre-FU-335 ceiling) differs, so this test fails if
/// the pipeline ceiling is ever taken from it again.
#[test]
fn an_already_allowed_child_keeps_its_effective_grant_digest() {
    const GOLDEN: &str = "5523de3c6dbfd1fc0031ae0536be3e77a181c338e5edf0a440fa355e1077901c";
    let root = [mcp_toolkit(), mcp_toolkit_at(93, "unused", UNUSED_URL)];
    let policy = Arc::new(enforcing_policy(4));
    let full = DelegationScope::root(Arc::clone(&policy), GrantSet::from_tools(&root));
    let reduced = DelegationScope::root(policy, GrantSet::from_tools(&root[..1]));
    let delegate = |scope: &DelegationScope| {
        scope
            .delegate(
                &mut vec![mcp_toolkit()],
                &std::collections::BTreeSet::new(),
                &elitea_agent_runtime::egress::PublicPolicy::Open,
            )
            .expect("an allowed child")
            .evidence
    };
    let (new, old) = (delegate(&full), delegate(&reduced));
    assert_eq!(new, old);
    assert_eq!(new.effective_grant_digest, GOLDEN);
}

// ---- C2: refusal inside a saved pipeline used as a tool ----

fn pipeline_tool_with_a_refused_agent_node() -> BTreeMap<Saved, Value> {
    BTreeMap::from([
        (
            (3, 4),
            saved_version(
                "agent",
                "Use the pipeline only when asked.",
                &json!({}),
                vec![reference((7, 8), "pipeline", "p2")],
            ),
        ),
        (
            (7, 8),
            saved_version(
                "pipeline",
                AGENT_NODE_PIPELINE,
                &json!({}),
                vec![reference((9, 10), "agent", "inner")],
            ),
        ),
        (
            (9, 10),
            saved_version("agent", "Use the toolkit.", &json!({}), vec![mcp_toolkit()]),
        ),
    ])
}

/// C2: a saved pipeline tool whose Agent node runs a refused saved agent
/// stays scoped to that tool: S never calls it, and S's turn completes.
#[tokio::test(flavor = "current_thread")]
async fn an_uncalled_pipeline_tool_with_a_refused_agent_node_does_not_fail_its_agents_turn() {
    let fixture = fixture(
        &pipeline_tool_with_a_refused_agent_node(),
        vec![text("no tool needed")],
    );
    let request = root_request(
        &root_yaml("worker"),
        vec![mcp_toolkit(), reference((3, 4), "agent", "worker")],
    );
    let outcome = completed(&fixture, &request).await;
    assert!(outcome.says("no tool needed"), "{outcome:?}");
    assert!(
        offered_tools(&fixture.captured, 0).contains(&"elitea_agent_7_v_8".to_owned()),
        "P2 is still offered, as its refusal"
    );
}

/// C2: called, the same tool answers the refusal by name and the turn goes on.
#[tokio::test(flavor = "current_thread")]
async fn a_called_pipeline_tool_with_a_refused_agent_node_answers_the_refusal() {
    let fixture = fixture(
        &pipeline_tool_with_a_refused_agent_node(),
        vec![
            TestModelGatewayOutcome::Response(tool_call_response(
                "p2-call",
                "elitea_agent_7_v_8",
                &json!({"task":"go"}),
            )),
            text("p2 was refused"),
        ],
    );
    let request = root_request(
        &root_yaml("worker"),
        vec![mcp_toolkit(), reference((3, 4), "agent", "worker")],
    );
    let outcome = completed(&fixture, &request).await;
    assert!(outcome.says("p2 was refused"), "{outcome:?}");
    let answer = refusal_in_answer_of(&fixture.captured, 1);
    assert!(
        answer.contains("DELEGATION_GRANT_MISSING") && answer.contains("'batch'"),
        "S reads the refusal by name: {answer}"
    );
    assert_eq!(
        fixture.dials.dials_to(BATCH_URL),
        0,
        "the inner agent never ran"
    );
}

// ---- Map members: the fan-out runs only on the PostgreSQL assembler. ----

const PG_EXECUTION: &str = "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f";
const PG_GENERATION: u64 = 3;

/// The production PostgreSQL assembler with the test-only fan-out
/// composition, the Map's decision ledger and an enforcing claim.
async fn map_outcome(
    url: &str,
    versions: &BTreeMap<Saved, Value>,
    outcomes: Vec<TestModelGatewayOutcome>,
    root_tools: Vec<Value>,
    policy: DelegationPolicy,
) -> (
    Result<Outcome, super::super::runtime::NativeAgentAssemblyError>,
    CapturedModelRequests,
) {
    let db = crate::state::postgres_session_tests::IsolatedPostgres::create(url).await;
    crate::state::postgres_session_tests::install_schema(&db.pool).await;
    sqlx::raw_sql(super::restart_pg_tests::CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let clients = runtime_clients(versions, outcomes);
    let assembler = PipelineNativeAgentAssembler::postgres(
        db.pool.clone(),
        crate::state::SessionLimits::default(),
        crate::state::CheckpointLimits::default(),
        Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("empty tool policy")),
        clients.platform,
        clients.model_facade,
    )
    .with_mcp_connector(Arc::new(BatchConnector))
    .with_fixed_parallel_for_tests()
    .with_fanout_decisions_for_tests(ledger.client(1));
    let request = root_request(MAP_ROOT, root_tools);
    let claim = AuthorizedNativeAssembly::from_authorized(
        &request,
        test_runtime_context_authority().with_delegation_policy_for_test(policy),
        crate::protocol::control::test_session_authority_for_reclaim(
            PG_EXECUTION,
            PG_GENERATION,
            1,
        ),
        Arc::new(crate::state::TestStateWriterLease::current()),
        AuthorizedNativeCommandBinding::fixture_for_execution(PG_EXECUTION),
    );
    let outcome = match assembler.assemble(claim).await {
        Err(error) => Err(error),
        Ok(invocation) => Ok(run(invocation).await),
    };
    db.pool.close().await;
    (outcome, clients.captured)
}

/// Control: the Map fixture runs a member Agent that holds no toolkit.
#[tokio::test(flavor = "current_thread")]
async fn postgres_control_a_map_member_agent_without_toolkits_runs() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-335 Map proof");
        return;
    };
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Plain.", &json!({}), Vec::new()),
    )]);
    let (outcome, _) = map_outcome(
        &url,
        &versions,
        vec![text("item done")],
        vec![reference((3, 4), "agent", "worker")],
        enforcing_policy(DEFAULT_DEPTH),
    )
    .await;
    let outcome = outcome.unwrap_or_else(|error| panic!("{}", describe(&error)));
    assert_eq!(outcome.failure, None, "{outcome:?}");
}

/// The root attaches the toolkit, no root node uses it, the Map member Agent
/// holds it: the member runs with it. Failed first on fdab93771
/// ("the stored pipeline could not be compiled").
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_map_member_holding_the_roots_attached_toolkit_runs() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-335 Map proof");
        return;
    };
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Use the toolkit.", &json!({}), vec![mcp_toolkit()]),
    )]);
    let (outcome, captured) = map_outcome(
        &url,
        &versions,
        vec![text("item done")],
        vec![mcp_toolkit(), reference((3, 4), "agent", "worker")],
        enforcing_policy(DEFAULT_DEPTH),
    )
    .await;
    let outcome = outcome.unwrap_or_else(|error| panic!("{}", describe(&error)));
    assert_eq!(outcome.failure, None, "{outcome:?}");
    assert!(offered_tools(&captured, 0).contains(&"echo".to_owned()));
}

/// The Map member refused for a toolkit its root lacks ends at start with
/// the readable delegation terminal naming it, never "Configuration type is
/// not supported.". Failed first on fdab93771.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_map_member_refused_a_toolkit_ends_typed_naming_it() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-335 Map proof");
        return;
    };
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Use the toolkit.", &json!({}), vec![mcp_toolkit()]),
    )]);
    let (outcome, captured) = map_outcome(
        &url,
        &versions,
        vec![text("unexpected")],
        vec![reference((3, 4), "agent", "worker")],
        enforcing_policy(DEFAULT_DEPTH),
    )
    .await;
    let Err(error) = outcome else {
        panic!("a refused member must end the run at start: {outcome:?}");
    };
    assert_refused_with_notice(&error, "member", "no_grant", "batch");
    assert_eq!(model_calls(&captured), 0);
}
// ---- a refused Agent node inside a saved pipeline node ----

/// The root's pipeline node runs the saved pipeline P2, whose own Agent node
/// runs a saved agent the chain may not delegate to: the notice is keyed by
/// the full graph path. On main the run ends `agent.legacy`.
#[tokio::test(flavor = "current_thread")]
async fn an_agent_node_inside_a_saved_pipeline_node_is_refused_by_its_graph_path() {
    let versions = BTreeMap::from([
        (
            (7, 8),
            saved_version(
                "pipeline",
                AGENT_NODE_PIPELINE,
                &json!({}),
                vec![reference((3, 4), "agent", "inner")],
            ),
        ),
        (
            (3, 4),
            saved_version("agent", "Use the toolkit.", &json!({}), vec![mcp_toolkit()]),
        ),
    ]);
    let fixture = fixture(&versions, vec![text("unexpected")]);
    let request = root_request(&root_yaml("p2"), vec![reference((7, 8), "pipeline", "p2")]);
    let Err(error) = fixture.assembler.assemble(enforcing(&request)).await else {
        panic!("a refused nested Agent node must end the run at start");
    };
    // The caller of this edge is P2, the saved pipeline the node runs in.
    assert_refused_with_notice_from(&error, "delegate/delegate", ("no_grant", "batch"), Some(7));
    assert_eq!(model_calls(&fixture.captured), 0);
    assert_eq!(fixture.dials.dials_to(BATCH_URL), 0);
}

// ---- delegation grants on (ADR-0044 Amendment 1: E(Y) ∪ G) ----

/// The root's saved application id the grants name (Main's `root_application_id`).
const ROOT_ID: u64 = 1;
const OUTSIDE_URL: &str = "https://outside.mcp.example.invalid/mcp";

/// The `batch` toolkit with only `tools` selected.
fn batch_with(tools: &[&str]) -> Value {
    let mut toolkit = mcp_toolkit();
    toolkit["settings"]["selected_tools"] = json!(tools);
    toolkit
}

/// The approved grant of the edge root -> S over `tools` of `batch`.
fn grant_of(callee: u64, tools: &[&str]) -> (EdgeKey, DelegationGrant) {
    let set = GrantSet::from_tools(&[batch_with(tools)]);
    let grant = DelegationGrant::new(41, set.digest(), set).expect("an approved grant");
    let edge = EdgeKey {
        caller: ROOT_ID,
        callee,
        kind: EdgeKind::PipelineChild,
    };
    (edge, grant)
}

/// A policy that carries `grants`, as Main sends it with the switch on.
fn granted(mode: NarrowingMode, grants: Vec<(EdgeKey, DelegationGrant)>) -> DelegationPolicy {
    DelegationPolicy::new(DEFAULT_DEPTH, mode, [])
        .expect("policy")
        .with_grants(ROOT_ID, grants)
        .expect("grants")
}

/// The run principal may use `batch` and `unused`, never `outside`.
fn principal() -> PrincipalBound {
    PrincipalBound::new(
        [
            PrincipalToolkit {
                id: 92,
                tools: None,
            },
            PrincipalToolkit {
                id: 93,
                tools: None,
            },
        ],
        None,
        Vec::<String>::new(),
    )
    .expect("principal bound")
}

/// S holds every tool of `batch`.
fn s_holding_batch() -> BTreeMap<Saved, Value> {
    BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Use the toolkit.", &json!({}), vec![mcp_toolkit()]),
    )])
}

/// An approved grant of the edge covers what the root lacks: S runs with it.
#[tokio::test(flavor = "current_thread")]
async fn with_grants_on_an_approved_edge_grant_covers_what_the_root_lacks() {
    let fixture = fixture(&s_holding_batch(), vec![text("worker answer")]);
    let request = root_request(
        &root_yaml("worker"),
        vec![reference((3, 4), "agent", "worker")],
    );
    let policy = granted(
        NarrowingMode::Enforce,
        vec![grant_of(3, &["get_issues", "echo", "reverse"])],
    );
    let outcome = completed_under(&fixture, &request, policy).await;
    assert!(outcome.says("worker answer"), "{outcome:?}");
    let offered = offered_tools(&fixture.captured, 0);
    assert!(
        ["get_issues", "echo", "reverse"]
            .iter()
            .all(|tool| offered.contains(&(*tool).to_owned())),
        "{offered:?}"
    );
}

/// Grants are on but this edge has none (missing, pending or revoked: Main
/// sends approved and stale grants only), and the root lacks the toolkit:
/// refused with Main's notice, which names the edge for Review delegation.
#[tokio::test(flavor = "current_thread")]
async fn with_grants_on_an_edge_without_a_grant_is_refused_with_the_notice() {
    let fixture = fixture(&s_holding_batch(), vec![text("unexpected")]);
    let request = root_request(
        &root_yaml("worker"),
        vec![reference((3, 4), "agent", "worker")],
    );
    let policy = granted(NarrowingMode::Enforce, vec![grant_of(9, &["echo"])]);
    let Err(error) = fixture.assembler.assemble(under(&request, policy)).await else {
        panic!("an edge without a grant must be refused");
    };
    assert_refused_with_notice_from(&error, "delegate", ("no_grant", "batch"), Some(ROOT_ID));
    assert_eq!(model_calls(&fixture.captured), 0);
    assert_eq!(fixture.dials.dials_to(BATCH_URL), 0);
}

/// FU-359: when every grant of a pipeline root is still pending (Main sends
/// `grants_enforced: true`, `root_application_id: ROOT_ID`, `grants: []`), a
/// refusal at either an Agent node or a saved-pipeline node still names
/// `caller_id == ROOT_ID` so the Web offers Review delegation.
#[tokio::test(flavor = "current_thread")]
async fn with_only_pending_grants_pipeline_root_refusals_name_the_root_caller() {
    let agent_fixture = fixture(&s_holding_batch(), vec![text("unexpected")]);
    let agent_request = root_request(
        &root_yaml("worker"),
        vec![reference((3, 4), "agent", "worker")],
    );
    let policy = granted(NarrowingMode::Enforce, Vec::new());
    assert!(policy.grants_enforced());
    assert_eq!(policy.root_application_id(), ROOT_ID);
    let Err(agent_error) = agent_fixture
        .assembler
        .assemble(under(&agent_request, policy.clone()))
        .await
    else {
        panic!("Agent node with only pending grants must be refused");
    };
    assert_refused_with_notice_from(
        &agent_error,
        "delegate",
        ("no_grant", "batch"),
        Some(ROOT_ID),
    );

    let pipeline_versions = BTreeMap::from([(
        (3, 4),
        saved_version(
            "pipeline",
            MCP_NODE_PIPELINE,
            &json!({}),
            vec![mcp_toolkit()],
        ),
    )]);
    let pipeline_fixture = fixture(&pipeline_versions, vec![text("unexpected")]);
    let pipeline_request = root_request(
        &root_yaml("subpipe"),
        vec![reference((3, 4), "pipeline", "subpipe")],
    );
    let Err(pipeline_error) = pipeline_fixture
        .assembler
        .assemble(under(&pipeline_request, policy))
        .await
    else {
        panic!("saved-pipeline node with only pending grants must be refused");
    };
    assert_refused_with_notice_from(
        &pipeline_error,
        "delegate",
        ("no_grant", "batch"),
        Some(ROOT_ID),
    );
}

/// A stale or partly approved grant covers only the set it recorded: what S
/// holds beyond it, and the root does not hold, is narrowed out.
#[tokio::test(flavor = "current_thread")]
async fn with_grants_on_a_partial_grant_covers_only_its_recorded_set() {
    let fixture = fixture(&s_holding_batch(), vec![text("worker answer")]);
    let request = root_request(
        &root_yaml("worker"),
        vec![reference((3, 4), "agent", "worker")],
    );
    let policy = granted(NarrowingMode::Enforce, vec![grant_of(3, &["echo"])]);
    let outcome = completed_under(&fixture, &request, policy).await;
    assert!(outcome.says("worker answer"), "{outcome:?}");
    let offered = offered_tools(&fixture.captured, 0);
    assert!(offered.contains(&"echo".to_owned()), "{offered:?}");
    assert!(
        !offered.contains(&"get_issues".to_owned()) && !offered.contains(&"reverse".to_owned()),
        "the grant never widens past its recorded set: {offered:?}"
    );
}

/// The ruling (ADR-0044 as written, E(Y) ∪ G): a grant adds what the caller
/// lacks and never takes away what the caller holds. The root attaches every
/// tool of `batch`; the partial grant does not cut S below it.
#[tokio::test(flavor = "current_thread")]
async fn with_grants_on_a_partial_grant_never_cuts_what_the_root_holds() {
    let fixture = fixture(&s_holding_batch(), vec![text("worker answer")]);
    let request = root_request(
        &root_yaml("worker"),
        vec![mcp_toolkit(), reference((3, 4), "agent", "worker")],
    );
    let policy = granted(NarrowingMode::Enforce, vec![grant_of(3, &["echo"])]);
    let outcome = completed_under(&fixture, &request, policy).await;
    assert!(outcome.says("worker answer"), "{outcome:?}");
    let offered = offered_tools(&fixture.captured, 0);
    assert!(
        ["get_issues", "echo", "reverse"]
            .iter()
            .all(|tool| offered.contains(&(*tool).to_owned())),
        "{offered:?}"
    );
}

/// A toolkit outside the run principal's bound is refused in both modes,
/// although the root attaches it: the wider ceiling never passes P.
#[tokio::test(flavor = "current_thread")]
async fn a_toolkit_outside_the_principal_is_refused_in_both_modes() {
    for mode in [NarrowingMode::Enforce, NarrowingMode::Report] {
        let versions = BTreeMap::from([(
            (3, 4),
            saved_version(
                "agent",
                "Use the toolkit.",
                &json!({}),
                vec![mcp_toolkit_at(94, "outside", OUTSIDE_URL)],
            ),
        )]);
        let fixture = fixture(&versions, vec![text("unexpected")]);
        let request = root_request(
            &root_yaml("worker"),
            vec![
                mcp_toolkit_at(94, "outside", OUTSIDE_URL),
                reference((3, 4), "agent", "worker"),
            ],
        );
        let policy = DelegationPolicy::new(DEFAULT_DEPTH, mode, [])
            .expect("policy")
            .with_principal(principal());
        let Err(error) = fixture.assembler.assemble(under(&request, policy)).await else {
            panic!("{mode:?}: a toolkit outside the principal must be refused");
        };
        assert_refused_with_notice(&error, "delegate", "principal", "outside");
        assert_eq!(model_calls(&fixture.captured), 0, "{mode:?}");
        assert_eq!(fixture.dials.dials_to(OUTSIDE_URL), 0, "{mode:?}");
    }
}

/// Report mode records a missing grant and runs: no refusal, no notice.
#[tokio::test(flavor = "current_thread")]
async fn report_mode_records_a_missing_grant_and_runs() {
    let fixture = fixture(&s_holding_batch(), vec![text("worker answer")]);
    let request = root_request(
        &root_yaml("worker"),
        vec![reference((3, 4), "agent", "worker")],
    );
    let policy =
        DelegationPolicy::new(DEFAULT_DEPTH, NarrowingMode::Report, []).expect("report policy");
    let outcome = completed_under(&fixture, &request, policy).await;
    assert!(outcome.says("worker answer"), "{outcome:?}");
}

/// Map member, grants on, an edge without a grant: refused with the notice.
#[tokio::test(flavor = "current_thread")]
async fn postgres_with_grants_on_a_map_member_without_a_grant_is_refused_with_the_notice() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-335 Map proof");
        return;
    };
    let (outcome, captured) = map_outcome(
        &url,
        &s_holding_batch(),
        vec![text("unexpected")],
        vec![reference((3, 4), "agent", "worker")],
        granted(NarrowingMode::Enforce, vec![grant_of(9, &["echo"])]),
    )
    .await;
    let Err(error) = outcome else {
        panic!("a refused member must end the run at start: {outcome:?}");
    };
    assert_refused_with_notice_from(&error, "member", ("no_grant", "batch"), Some(ROOT_ID));
    assert_eq!(model_calls(&captured), 0);
}

/// Map member, grants on, an approved grant: it runs with the toolkit.
#[tokio::test(flavor = "current_thread")]
async fn postgres_with_grants_on_a_map_member_with_an_approved_grant_runs() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-335 Map proof");
        return;
    };
    let (outcome, captured) = map_outcome(
        &url,
        &s_holding_batch(),
        vec![text("item done")],
        vec![reference((3, 4), "agent", "worker")],
        granted(
            NarrowingMode::Enforce,
            vec![grant_of(3, &["get_issues", "echo", "reverse"])],
        ),
    )
    .await;
    let outcome = outcome.unwrap_or_else(|error| panic!("{}", describe(&error)));
    assert_eq!(outcome.failure, None, "{outcome:?}");
    assert!(offered_tools(&captured, 0).contains(&"echo".to_owned()));
}

/// FU-354 on main's notice: a depth refusal at a graph node now carries
/// Main's notice too (reason `depth`), as #2235's pipeline node does. Under a
/// maximum of 1 the root's pipeline node P2 runs (depth 1) and its own Agent
/// node's saved agent (depth 2) is refused.
#[tokio::test(flavor = "current_thread")]
async fn a_depth_refusal_at_an_agent_node_carries_the_notice() {
    let versions = BTreeMap::from([
        (
            (7, 8),
            saved_version(
                "pipeline",
                AGENT_NODE_PIPELINE,
                &json!({}),
                vec![reference((3, 4), "agent", "inner")],
            ),
        ),
        (
            (3, 4),
            saved_version("agent", "Plain.", &json!({}), Vec::new()),
        ),
    ]);
    let fixture = fixture(&versions, vec![text("unexpected")]);
    let request = root_request(&root_yaml("p2"), vec![reference((7, 8), "pipeline", "p2")]);
    let Err(error) = fixture
        .assembler
        .assemble(enforcing_at_depth(&request, 1))
        .await
    else {
        panic!("an Agent node past the depth must end the run at start");
    };
    assert_eq!(
        error.cause().map(|cause| (cause.code(), cause.detail())),
        Some(("DELEGATION_DEPTH_EXCEEDED", Some("depth"))),
        "{}",
        describe(&error)
    );
    let notice = error
        .delegation_notice()
        .unwrap_or_else(|| panic!("no delegation notice: {}", describe(&error)));
    assert_eq!(notice.value["reason"], "depth", "{}", describe(&error));
    assert_eq!(notice.value["id"], "pipeline_node:delegate/delegate");
    assert_eq!(model_calls(&fixture.captured), 0);
}

// ---- parity: a pipeline root delegates exactly as an ordinary root ----

/// What `scope` does with S asking for `callee`: the evidence (effective
/// digest, refusal, grant and what it narrowed out) and the tools S keeps.
fn delegated(scope: &DelegationScope, callee: &[Value]) -> (Result<String, String>, Vec<Value>) {
    let mut tools = callee.to_vec();
    let outcome = scope
        .delegate_edge(
            Some(3),
            &mut tools,
            &std::collections::BTreeSet::new(),
            &elitea_agent_runtime::egress::PublicPolicy::Open,
        )
        .map(|delegation| format!("{:?}", delegation.evidence))
        .map_err(|refused| format!("{:?} {:?}", refused.refusal, refused.evidence));
    (outcome, tools)
}

/// The condition of the ruling: the fix never gives a pipeline root more than
/// an ordinary root with the same attachments, grants and policy. Both roots
/// are composed by their production code (`ordinary.rs` and
/// `pipeline::root_delegation_and_credentials`) under one claim carrying a
/// partial grant (only `echo`) and a principal bound without `outside`. They
/// must hold the same ceiling and give S the same effective set and the same
/// narrowing records, whether the root itself holds `get_issues` and
/// `reverse` (they reach S: a grant never takes away what the caller holds)
/// or not (they are narrowed out). A toolkit outside P is refused by both.
#[test]
fn a_pipeline_root_delegates_exactly_as_an_ordinary_root_with_the_same_inputs() {
    let all = ["get_issues", "echo", "reverse"];
    for (root_holds, root_batch) in [(true, batch_with(&all)), (false, batch_with(&["echo"]))] {
        let tools = vec![
            root_batch,
            mcp_toolkit_at(93, "unused", UNUSED_URL),
            mcp_toolkit_at(94, "outside", OUTSIDE_URL),
            reference((3, 4), "agent", "worker"),
        ];
        let authority = test_runtime_context_authority().with_delegation_policy_for_test(
            granted(NarrowingMode::Enforce, vec![grant_of(3, &["echo"])])
                .with_principal(principal()),
        );
        let admission = ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy");
        // The ordinary root, as `ordinary.rs` composes it.
        let mut ordinary = ordinary_request(AgentExecutionKind::Application);
        ordinary.payload.application["version_details"]["tools"] = Value::Array(tools.clone());
        let ordinary_snapshot = super::super::application_tools::principal_bounded_snapshot(
            &authority,
            crate::toolkits::FrozenToolSnapshot::from_request(&ordinary)
                .expect("ordinary snapshot")
                .apply_policy(&admission),
        );
        let ordinary_scope = super::super::application_tools::root_delegation_scope(
            &authority,
            AgentExecutionKind::Application,
            &ordinary_snapshot,
        );
        // The pipeline root, through its production composition.
        let pipeline = root_request(&root_yaml("worker"), tools);
        let (profile, _, toolsets, ..) = authorized(&pipeline)
            .admit_pipeline_with_policy(&admission)
            .expect("pipeline admission")
            .into_parts();
        let (pipeline_scope, credentials) =
            super::super::pipeline::root_delegation_and_credentials(&authority, &profile, toolsets);
        let case = format!("root holds get_issues and reverse: {root_holds}");
        assert_eq!(
            pipeline_scope.cache_key(),
            ordinary_scope.cache_key(),
            "{case}"
        );
        // The ceiling is own(root) ∩ P: never `outside`, whatever the root attaches.
        let own_in_p = DelegationScope::root(
            Arc::new(granted(NarrowingMode::Enforce, Vec::new()).with_principal(principal())),
            GrantSet::from_tools(&[
                if root_holds {
                    batch_with(&all)
                } else {
                    batch_with(&["echo"])
                },
                mcp_toolkit_at(93, "unused", UNUSED_URL),
            ]),
        );
        assert_eq!(
            pipeline_scope.grants().digest(),
            own_in_p.grants().digest(),
            "{case}"
        );
        // Credentials stay on what the root's own nodes select: its one Agent
        // node's alias, no toolkit.
        assert_eq!(credentials.len(), 1, "{case}");
        for callee in [
            vec![mcp_toolkit()],
            vec![mcp_toolkit_at(94, "outside", OUTSIDE_URL)],
        ] {
            let (pipeline_outcome, pipeline_tools) = delegated(&pipeline_scope, &callee);
            let (ordinary_outcome, ordinary_tools) = delegated(&ordinary_scope, &callee);
            assert_eq!(pipeline_outcome, ordinary_outcome, "{case}");
            assert_eq!(pipeline_tools, ordinary_tools, "{case}");
        }
        // The values, not only their equality.
        let (batch, kept) = delegated(&pipeline_scope, &[mcp_toolkit()]);
        let batch = batch.expect("S is delegated");
        let narrowed = batch.contains("get_issues") && batch.contains("reverse");
        assert_eq!(narrowed, !root_holds, "{case}: {batch}");
        assert_eq!(
            kept[0]["settings"]["selected_tools"],
            if root_holds {
                json!(["echo", "get_issues", "reverse"])
            } else {
                json!(["echo"])
            },
            "{case}"
        );
        let (outside, _) = delegated(
            &pipeline_scope,
            &[mcp_toolkit_at(94, "outside", OUTSIDE_URL)],
        );
        assert!(
            outside.is_err_and(|refused| refused.starts_with("PrincipalBound")),
            "{case}"
        );
    }
}

/// The credential set a pipeline root's own nodes materialize stays within
/// the run principal: a direct node on `outside` (not in P) gets no
/// credential, while the same pipeline without a principal bound keeps it.
#[test]
fn a_pipeline_roots_own_credentials_stay_within_the_principal() {
    let yaml = MCP_NODE_PIPELINE.replace("toolkit_name: batch", "toolkit_name: outside");
    let tools = vec![mcp_toolkit_at(94, "outside", OUTSIDE_URL)];
    let admission = ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy");
    for (bounded, expected) in [(true, 0), (false, 1)] {
        let mut policy = enforcing_policy(DEFAULT_DEPTH);
        if bounded {
            policy = policy.with_principal(principal());
        }
        let authority = test_runtime_context_authority().with_delegation_policy_for_test(policy);
        let pipeline = root_request(&yaml, tools.clone());
        let (profile, _, toolsets, ..) = authorized(&pipeline)
            .admit_pipeline_with_policy(&admission)
            .expect("pipeline admission")
            .into_parts();
        let (_, credentials) =
            super::super::pipeline::root_delegation_and_credentials(&authority, &profile, toolsets);
        assert_eq!(credentials.len(), expected, "bounded by P: {bounded}");
    }
}

/// FU-353: ADR-0044 refuses ungranted nested `artifact` and `memory` toolkits
/// under `Enforce`, and `PrincipalBound` refuses principal-hidden ones in BOTH
/// `Enforce` and `Report` modes.
#[test]
fn fu353_delegation_refuses_ungranted_or_hidden_nested_artifact_and_memory_toolkits() {
    let artifact_kit = json!({
        "id": 50,
        "type": "artifact",
        "toolkit_name": "docs_kit",
        "settings": {"bucket": "docs", "selected_tools": ["list_files", "read_file"]}
    });
    let memory_kit = json!({
        "id": 51,
        "type": "memory",
        "toolkit_name": "notes_mem",
        "settings": {"namespace": "notes", "selected_tools": ["manage_memory", "search_memory"]}
    });
    let principal_with_both = PrincipalBound::new(
        [
            PrincipalToolkit {
                id: 50,
                tools: None,
            },
            PrincipalToolkit {
                id: 51,
                tools: None,
            },
        ],
        None,
        Vec::<String>::new(),
    )
    .expect("principal bound");
    let principal_with_neither =
        PrincipalBound::new(Vec::<PrincipalToolkit>::new(), None, Vec::<String>::new())
            .expect("empty principal bound");

    // Root holds neither toolkit; under Enforce, ungranted child artifact & memory are refused.
    let empty_root_policy =
        Arc::new(enforcing_policy(DEFAULT_DEPTH).with_principal(principal_with_both.clone()));
    let empty_scope = DelegationScope::root(empty_root_policy, GrantSet::from_tools(&[]));
    for kit in [&artifact_kit, &memory_kit] {
        let (outcome, _) = delegated(&empty_scope, std::slice::from_ref(kit));
        assert!(
            outcome.is_err(),
            "ungranted nested toolkit must be refused under Enforce: {outcome:?}"
        );
    }

    // Even when root holds both toolkits, PrincipalBound hiding them refuses in
    // BOTH Enforce and Report modes.
    for mode in [NarrowingMode::Enforce, NarrowingMode::Report] {
        let policy = Arc::new(
            DelegationPolicy::new(DEFAULT_DEPTH, mode, [])
                .expect("policy")
                .with_principal(principal_with_neither.clone()),
        );
        let root_scope = DelegationScope::root(
            policy,
            GrantSet::from_tools(&[artifact_kit.clone(), memory_kit.clone()]),
        );
        for kit in [&artifact_kit, &memory_kit] {
            let (outcome, _) = delegated(&root_scope, std::slice::from_ref(kit));
            assert!(
                outcome
                    .as_ref()
                    .is_err_and(|refused| refused.starts_with("PrincipalBound")),
                "principal-hidden toolkit must be refused in {mode:?}: {outcome:?}"
            );
        }
    }

    // When root holds both toolkits AND PrincipalBound admits them, delegation succeeds.
    let admitted_policy =
        Arc::new(enforcing_policy(DEFAULT_DEPTH).with_principal(principal_with_both));
    let admitted_scope = DelegationScope::root(
        admitted_policy,
        GrantSet::from_tools(&[artifact_kit.clone(), memory_kit.clone()]),
    );
    let (ok, kept) = delegated(&admitted_scope, &[artifact_kit, memory_kit]);
    assert!(ok.is_ok(), "admitted nested toolkits must succeed: {ok:?}");
    assert_eq!(kept.len(), 2);
}

/// FU-353 D6(a): a nested saved pipeline (`bind_saved_pipeline_runtimes`) gets
/// the run's pinned `ArtifactToolAuthority` and runs its direct `artifact`
/// toolkit node under an enforcing delegation policy.
#[tokio::test(flavor = "current_thread")]
async fn fu353_nested_saved_pipeline_runs_pinned_artifact_toolkit_node() {
    let artifact_kit = json!({
        "id": 50,
        "type": "artifact",
        "toolkit_name": "docs_kit",
        "settings": {"bucket": "docs", "selected_tools": ["list_files"]}
    });
    let child_yaml = "state:\n  input: str\n  messages: list\n  listing: dict\nentry_point: ls\nnodes:\n  - id: ls\n    type: toolkit\n    toolkit_name: docs_kit\n    tool: list_files\n    input_mapping: {}\n    output: [listing]\n    transition: END\n";
    let versions = BTreeMap::from([(
        (7, 8),
        saved_version(
            "pipeline",
            child_yaml,
            &json!({}),
            vec![artifact_kit.clone()],
        ),
    )]);
    let fixture = fixture(&versions, Vec::new());
    let request = root_request(
        &root_yaml("p_art"),
        vec![artifact_kit, reference((7, 8), "pipeline", "p_art")],
    );
    let invocation = fixture
        .assembler
        .assemble(enforcing(&request))
        .await
        .unwrap_or_else(|error| {
            panic!("nested artifact pipeline was refused: {}", describe(&error))
        });
    let _browser = collect_pipeline_completion(invocation).await;
    let routes = fixture.routes.lock().expect("routes").clone();
    assert!(
        routes.iter().any(|route| route == "artifacts/list"),
        "expected nested pipeline to call artifacts/list, saw: {routes:?}"
    );
}

/// FU-353 D1(a): nested `attachments` require BOTH caller ceiling AND child's
/// `"attachments"` toggle, including monotonic narrowing across pipeline hops.
#[tokio::test(flavor = "current_thread")]
async fn fu353_nested_attachments_follow_strict_monotonic_caller_ceiling() {
    let child_meta = json!({"internal_tools": ["attachments"]});
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Read attachments.", &child_meta, Vec::new()),
    )]);

    // 1. Pipeline root WITH attachments -> child WITH attachments => bound.
    let mut allowed_req = root_request(
        &root_yaml("worker"),
        vec![reference((3, 4), "agent", "worker")],
    );
    allowed_req.payload.application["version_details"]["meta"] =
        json!({"internal_tools": ["attachments"]});
    let allowed = fixture(&versions, vec![text("done")]);
    let inv = allowed
        .assembler
        .assemble(enforcing(&allowed_req))
        .await
        .expect("allowed attachments assemble");
    let _ = collect_pipeline_completion(inv).await;
    let tools = offered_tools(&allowed.captured, 0);
    assert!(tools.iter().any(|t| t == "list_attachments"), "{tools:?}");
    assert!(tools.iter().any(|t| t == "read_attachment"), "{tools:?}");
    assert!(tools.iter().any(|t| t == "search_attachment"), "{tools:?}");

    // 2. Pipeline root WITHOUT attachments -> child WITH attachments => withheld.
    let denied_req = root_request(
        &root_yaml("worker"),
        vec![reference((3, 4), "agent", "worker")],
    );
    let denied = fixture(&versions, vec![text("done")]);
    let inv = denied
        .assembler
        .assemble(enforcing(&denied_req))
        .await
        .expect("denied attachments assemble");
    let _ = collect_pipeline_completion(inv).await;
    let tools = offered_tools(&denied.captured, 0);
    assert!(!tools.iter().any(|t| t == "list_attachments"), "{tools:?}");
    assert!(!tools.iter().any(|t| t == "read_attachment"), "{tools:?}");
    assert!(!tools.iter().any(|t| t == "search_attachment"), "{tools:?}");

    // 3. Multi-hop narrowing: root WITH attachments -> nested pipeline P2 WITHOUT
    //    attachments -> child WITH attachments => withheld.
    let multi_versions = BTreeMap::from([
        (
            (7, 8),
            saved_version(
                "pipeline",
                AGENT_NODE_PIPELINE,
                &json!({}),
                vec![reference((3, 4), "agent", "inner")],
            ),
        ),
        (
            (3, 4),
            saved_version("agent", "Read attachments.", &child_meta, Vec::new()),
        ),
    ]);
    let mut multi_req = root_request(&root_yaml("p2"), vec![reference((7, 8), "pipeline", "p2")]);
    multi_req.payload.application["version_details"]["meta"] =
        json!({"internal_tools": ["attachments"]});
    let multi = fixture(&multi_versions, vec![text("done")]);
    let inv = multi
        .assembler
        .assemble(enforcing(&multi_req))
        .await
        .expect("multi-hop assemble");
    let _ = collect_pipeline_completion(inv).await;
    let tools = offered_tools(&multi.captured, 0);
    assert!(!tools.iter().any(|t| t == "list_attachments"), "{tools:?}");
    assert!(!tools.iter().any(|t| t == "read_attachment"), "{tools:?}");
}

/// FU-353 D2(a1) / D5(a): nested saved agent binds target-scoped `agent_memory`
/// tools (`recall`/`remember`/`forget`, or `recall`-only for `write: "off"`),
/// sends `(target_application_id, target_version_id)` on the wire, and NEVER
/// binds root-only builders/planner/subagents.
#[tokio::test(flavor = "current_thread")]
async fn fu353_nested_agent_memory_binds_target_scoped_tools_and_excludes_builders() {
    let child_meta = json!({
        "memory": {"enabled": true, "write": "auto"},
        "internal_tools": ["skills_builder", "project_context_builder", "planner", "subagents"]
    });
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Use your memory.", &child_meta, Vec::new()),
    )]);
    let req = root_request(
        &root_yaml("worker"),
        vec![reference((3, 4), "agent", "worker")],
    );
    let fix = fixture(
        &versions,
        vec![
            TestModelGatewayOutcome::Response(tool_call_response(
                "call-mem-1",
                "remember",
                &json!({"kind": "fact", "content": "Child fact."}),
            )),
            text("saved"),
        ],
    );
    let inv = fix
        .assembler
        .assemble(enforcing(&req))
        .await
        .expect("nested memory assemble");
    let _ = collect_pipeline_completion(inv).await;

    let tools = offered_tools(&fix.captured, 0);
    assert!(tools.iter().any(|t| t == "recall"), "{tools:?}");
    assert!(tools.iter().any(|t| t == "remember"), "{tools:?}");
    assert!(tools.iter().any(|t| t == "forget"), "{tools:?}");
    for root_only in [
        "save_skill",
        "update_project_context",
        "create_plan",
        "spawn_agent",
    ] {
        assert!(!tools.iter().any(|t| t == root_only), "{tools:?}");
    }

    let calls = fix.memory_calls.lock().expect("memory calls").clone();
    assert_eq!(calls.len(), 1, "{calls:?}");
    assert_eq!(calls[0].0, "agent-memory/remember");
    assert_eq!(calls[0].1["target_application_id"], 3);
    assert_eq!(calls[0].1["target_version_id"], 4);

    // `write: "off"` binds `recall` only.
    let ro_versions = BTreeMap::from([(
        (3, 4),
        saved_version(
            "agent",
            "Read-only memory.",
            &json!({"memory": {"enabled": true, "write": "off"}}),
            Vec::new(),
        ),
    )]);
    let ro_fix = fixture(&ro_versions, vec![text("done")]);
    let inv = ro_fix
        .assembler
        .assemble(enforcing(&req))
        .await
        .expect("read-only nested memory assemble");
    let _ = collect_pipeline_completion(inv).await;
    let ro_tools = offered_tools(&ro_fix.captured, 0);
    assert!(ro_tools.iter().any(|t| t == "recall"), "{ro_tools:?}");
    assert!(!ro_tools.iter().any(|t| t == "remember"), "{ro_tools:?}");
    assert!(!ro_tools.iter().any(|t| t == "forget"), "{ro_tools:?}");
}

struct ShadowTool(&'static str);

#[async_trait]
impl Tool for ShadowTool {
    fn name(&self) -> &str {
        self.0
    }
    fn description(&self) -> &'static str {
        "shadow"
    }
    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({"type": "object"}))
    }
    async fn execute(
        &self,
        _ctx: Arc<dyn adk_rust::ToolContext>,
        _args: Value,
    ) -> adk_rust::Result<Value> {
        Ok(json!("shadowed"))
    }
}

struct ShadowConnector(&'static str);

#[async_trait]
impl McpConnector for ShadowConnector {
    async fn connect(
        &self,
        _config: &crate::toolkits::RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, crate::toolkits::McpMaterializationError> {
        Ok(Arc::new(adk_rust::tool::BasicToolset::new(
            "mock_shadow",
            vec![Arc::new(ShadowTool(self.0)) as Arc<dyn Tool>],
        )))
    }
}

fn shadow_mcp_kit(toolkit_name: &str, tool_name: &str) -> Value {
    json!({
        "id": 92,
        "type": "mcp",
        "toolkit_name": toolkit_name,
        "settings": {
            "url": BATCH_URL, "headers": null, "client_id": null, "client_secret": null,
            "scopes": null, "timeout": 30, "selected_tools": [tool_name],
            "enable_caching": true, "cache_ttl": 300, "ssl_verify": true
        }
    })
}

/// FU-353: `elitea_attachments` and `agent_memory` are reserved toolsets on
/// nested agents: a user toolkit sharing both reserved toolset name and tool
/// name is refused, and a colliding tool from another toolkit is aliased away
/// so the native tool keeps its bare name.
#[tokio::test(flavor = "current_thread")]
async fn fu353_nested_reserved_toolsets_reject_shadowing_by_user_toolkit() {
    let same_name_kit = shadow_mcp_kit("agent_memory", "recall");
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version(
            "agent",
            "Shadow attempt.",
            &json!({"memory": {"enabled": true, "write": "off"}}),
            vec![same_name_kit.clone()],
        ),
    )]);
    let req = root_request(
        &root_yaml("worker"),
        vec![same_name_kit, reference((3, 4), "agent", "worker")],
    );
    let clients = runtime_clients(&versions, vec![text("unexpected")]);
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::new(InMemorySessionService::new()) as Arc<dyn SessionService>,
        Arc::new(MemoryCheckpointer::new()) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(clients.platform, clients.model_facade)
    .with_mcp_connector(Arc::new(ShadowConnector("recall")));
    let Err(error) = assembler.assemble(enforcing(&req)).await else {
        panic!("nested toolkit shadowing reserved 'agent_memory' + 'recall' must be refused");
    };
    assert_eq!(
        error.code(),
        super::super::runtime::NativeAgentAssemblyErrorCode::InvalidConfiguration,
        "{}",
        describe(&error)
    );

    let other_kit = shadow_mcp_kit("external_files", "read_attachment");
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version(
            "agent",
            "Attachments + external MCP.",
            &json!({"internal_tools": ["attachments"]}),
            vec![other_kit.clone()],
        ),
    )]);
    let mut req = root_request(
        &root_yaml("worker"),
        vec![other_kit, reference((3, 4), "agent", "worker")],
    );
    req.payload.application["version_details"]["meta"] = json!({"internal_tools": ["attachments"]});
    let clients = runtime_clients(&versions, vec![text("done")]);
    let assembler = PipelineNativeAgentAssembler::with_state(
        Arc::new(InMemorySessionService::new()) as Arc<dyn SessionService>,
        Arc::new(MemoryCheckpointer::new()) as Arc<dyn Checkpointer>,
    )
    .with_runtime_clients(clients.platform, clients.model_facade)
    .with_mcp_connector(Arc::new(ShadowConnector("read_attachment")));
    let inv = assembler
        .assemble(enforcing(&req))
        .await
        .unwrap_or_else(|error| panic!("assemble failed: {}", describe(&error)));
    let _ = collect_pipeline_completion(inv).await;
    let tools = offered_tools(&clients.captured, 0);
    assert!(tools.iter().any(|t| t == "read_attachment"), "{tools:?}");
    assert!(
        tools.iter().any(|t| t == "external_files__read_attachment"),
        "user tool must be aliased away from reserved elitea_attachments: {tools:?}"
    );
}

/// FU-353 D1(a): an ad-hoc ordinary root (without `meta.internal_tools`)
/// inherits conversation attachments for nested agents that enable `"attachments"`
/// without adding `list_attachments` to the ad-hoc root itself.
#[tokio::test(flavor = "current_thread")]
async fn fu353_adhoc_ordinary_root_lends_attachments_to_nested_agent() {
    let child_meta = json!({"internal_tools": ["attachments"]});
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Inspect attachments.", &child_meta, Vec::new()),
    )]);
    let clients = runtime_clients(
        &versions,
        vec![
            TestModelGatewayOutcome::Response(tool_call_response(
                "call-nested-1",
                "elitea_agent_3_v_4",
                &json!({"task": "check files"}),
            )),
            text("nested done"),
            text("root done"),
        ],
    );
    let assembler = super::super::ordinary::OrdinaryNativeAgentAssembler::new(
        clients.platform,
        clients.model_facade,
        Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy")),
    );
    let mut request = ordinary_request(AgentExecutionKind::Adhoc);
    request.payload.tools = vec![reference((3, 4), "agent", "worker")];
    let invocation = assembler
        .assemble(enforcing(&request))
        .await
        .unwrap_or_else(|error| panic!("ad-hoc assemble failed: {}", describe(&error)));
    let (mut run, _, _) = invocation.start().expect("ad-hoc start");
    while run.next_event().await.expect("ad-hoc event").is_some() {}

    let root_tools = offered_tools(&clients.captured, 0);
    assert!(
        !root_tools.iter().any(|t| t == "list_attachments"),
        "ad-hoc root must not gain list_attachments: {root_tools:?}"
    );
    let child_tools = offered_tools(&clients.captured, 1);
    assert!(
        child_tools.iter().any(|t| t == "list_attachments"),
        "nested agent under ad-hoc root must get list_attachments: {child_tools:?}"
    );
    assert!(
        child_tools.iter().any(|t| t == "read_attachment"),
        "{child_tools:?}"
    );
    assert!(
        child_tools.iter().any(|t| t == "search_attachment"),
        "{child_tools:?}"
    );
}

/// FU-353-05: when a nested saved agent reads a conversation attachment via
/// `read_attachment` and then calls `remember`, the shared `UntrustedReads`
/// taint sets `from_tool_output: true` on `agent-memory/remember`.
#[tokio::test(flavor = "current_thread")]
async fn fu353_nested_attachment_read_taints_subsequent_remember_write() {
    let child_meta = json!({
        "internal_tools": ["attachments"],
        "memory": {"enabled": true, "write": "auto"}
    });
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Read then remember.", &child_meta, Vec::new()),
    )]);
    let mut req = root_request(
        &root_yaml("worker"),
        vec![reference((3, 4), "agent", "worker")],
    );
    req.payload.application["version_details"]["meta"] = json!({"internal_tools": ["attachments"]});
    let fix = fixture(
        &versions,
        vec![
            TestModelGatewayOutcome::Response(tool_call_response(
                "call-att-1",
                "read_attachment",
                &json!({"name": "ok.txt"}),
            )),
            TestModelGatewayOutcome::Response(tool_call_response(
                "call-mem-2",
                "remember",
                &json!({"kind": "fact", "content": "Fact derived from attachment."}),
            )),
            text("done"),
        ],
    );
    let inv = fix
        .assembler
        .assemble(enforcing(&req))
        .await
        .expect("assemble");
    let _ = collect_pipeline_completion(inv).await;

    let calls = fix.memory_calls.lock().expect("memory calls").clone();
    assert_eq!(calls.len(), 1, "{calls:?}");
    assert_eq!(calls[0].0, "agent-memory/remember");
    assert_eq!(calls[0].1["target_application_id"], 3);
    assert_eq!(calls[0].1["target_version_id"], 4);
    assert_eq!(
        calls[0].1["from_tool_output"], true,
        "read_attachment must record untrusted-read taint before remember"
    );
}
