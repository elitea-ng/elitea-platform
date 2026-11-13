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
    DelegatedCallee, DelegationPolicy, DelegationRefusal, DelegationScope, GrantKind, GrantSet,
    NarrowingMode,
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

fn enforcing_policy(max_depth: u32) -> DelegationPolicy {
    DelegationPolicy::new(max_depth, NarrowingMode::Enforce, []).expect("enforcing policy")
}

fn enforcing(
    request: &super::super::request::AgentExecutionRequest,
) -> AuthorizedNativeAssembly<'_> {
    enforcing_at_depth(request, elitea_agent_runtime::delegation::DEFAULT_MAX_DEPTH)
}

fn enforcing_at_depth(
    request: &super::super::request::AgentExecutionRequest,
    max_depth: u32,
) -> AuthorizedNativeAssembly<'_> {
    AuthorizedNativeAssembly::new(
        request,
        test_runtime_context_authority()
            .with_delegation_policy_for_test(enforcing_policy(max_depth)),
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
                "token":"fixture"})
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
}

fn runtime_clients(
    versions: &BTreeMap<Saved, Value>,
    outcomes: Vec<TestModelGatewayOutcome>,
) -> Clients {
    let routes = Arc::new(Mutex::new(Vec::new()));
    let runtime = RuntimeContextClient::with_rpc(
        Routes(SavedVersions {
            versions: versions.clone(),
            routes: Arc::clone(&routes),
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
    }
}

struct Fixture {
    assembler: PipelineNativeAgentAssembler,
    captured: CapturedModelRequests,
    dials: DialRecorder,
    routes: Arc<Mutex<Vec<String>>>,
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
        error
            .delegation_refusal()
            .map(|(callee, refused)| (callee, refused.refusal.to_value()))
    )
}

async fn completed(
    fixture: &Fixture,
    request: &super::super::request::AgentExecutionRequest,
) -> Outcome {
    let invocation = match fixture.assembler.assemble(enforcing(request)).await {
        Ok(invocation) => invocation,
        Err(error) => panic!("the run was refused at start: {}", describe(&error)),
    };
    let outcome = run(invocation).await;
    assert_eq!(outcome.failure, None, "{outcome:?}");
    outcome
}

/// The run ends at start with the readable delegation terminal naming the
/// missing toolkit, before any model call or dial.
fn assert_refused_naming(
    error: &super::super::runtime::NativeAgentAssemblyError,
    callee: DelegatedCallee,
    toolkit: &str,
) {
    let (refused_callee, refused) = error
        .delegation_refusal()
        .unwrap_or_else(|| panic!("not a delegation refusal: {}", describe(error)));
    assert_eq!(refused_callee, callee, "{}", describe(error));
    assert_eq!(
        refused.refusal.to_value(),
        json!({"code":"DELEGATION_GRANT_MISSING","kind":"toolkit","name":toolkit}),
        "{}",
        describe(error)
    );
    assert_eq!(
        crate::execution::assembly_failure(error),
        RuntimeFailureKind::DelegationGrantMissing,
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
/// start with `DelegationGrantMissing` naming `batch`, before any model call
/// or dial. Failed first on fdab93771 (`agent.legacy`, "The runtime operation
/// failed.").
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
    assert_refused_naming(&error, DelegatedCallee::Agent, "batch");
    assert_eq!(model_calls(&fixture.captured), 0);
    assert_eq!(fixture.dials.dials_to(BATCH_URL), 0);
}

/// The notice names only the node kind and the toolkit's display name: never
/// a tool, a credential reference, a data class, a host or a URL.
#[test]
fn the_delegation_notice_names_only_the_node_kind_and_the_toolkit() {
    let identity = super::super::events::AssemblyNoticeIdentity {
        stream_id: "stream-1",
        message_id: "message-1",
        sio_event: "chat_predict",
        execution_generation: "3",
        thread_id: "thread-1",
    };
    let missing = |kind, name: &str, toolkit: Option<&str>| DelegationRefusal::GrantMissing {
        kind,
        name: name.to_owned(),
        toolkit: toolkit.map(str::to_owned),
    };
    let cases = [
        (
            missing(GrantKind::Toolkit, "Team GitHub", None),
            Some("Team GitHub"),
        ),
        (
            missing(GrantKind::Tool, "create_issue", Some("Team GitHub")),
            Some("Team GitHub"),
        ),
        (
            missing(
                GrantKind::Credential,
                "cred-admin-7f3a",
                Some("Team GitHub"),
            ),
            Some("Team GitHub"),
        ),
        (missing(GrantKind::DataClass, "pii", None), None),
        (
            missing(GrantKind::EgressHost, "internal.example.test", None),
            None,
        ),
    ];
    for (callee, node) in [
        (DelegatedCallee::Agent, "agent"),
        (DelegatedCallee::Pipeline, "pipeline"),
    ] {
        for (refusal, toolkit) in &cases {
            let event = super::super::events::delegation_refused_assembly_notice(
                &identity,
                callee,
                refusal,
                Utc.timestamp_opt(1_800_000_000, 0).single().expect("time"),
            )
            .expect("the notice builds");
            let content: Value = serde_json::from_slice(&event.content).expect("notice content");
            let metadata: Value =
                serde_json::from_slice(&event.response_metadata).expect("notice metadata");
            let shown = format!("{content}{metadata}");
            // A dedicated event Main shows only beside a FAILED terminal,
            // never the turn's final message; data only, no prose.
            assert_eq!(event.r#type, "agent_delegation_refused");
            assert_eq!(content, Value::Null);
            assert_eq!(
                metadata["delegation_refused"],
                json!({"code":"DELEGATION_GRANT_MISSING","node":node,"toolkit":toolkit})
            );
            for hidden in [
                "create_issue",
                "cred-admin-7f3a",
                "pii",
                "internal.example.test",
                "http",
            ] {
                assert!(!shown.contains(hidden), "{hidden} leaked: {shown}");
            }
        }
    }
    // A control-character or oversized name is bounded before it is sent:
    // at most `MAX_NOTICE_LABEL_BYTES`, the limit kept, one more cut.
    let notice_toolkit = |name: String| {
        let event = super::super::events::delegation_refused_assembly_notice(
            &identity,
            DelegatedCallee::Agent,
            &missing(GrantKind::Toolkit, &name, None),
            Utc.timestamp_opt(1_800_000_000, 0).single().expect("time"),
        )
        .expect("the notice builds");
        let metadata: Value =
            serde_json::from_slice(&event.response_metadata).expect("notice metadata");
        metadata["delegation_refused"]["toolkit"]
            .as_str()
            .expect("a toolkit name")
            .to_owned()
    };
    let limit = elitea_agent_runtime::toolkits::MAX_NOTICE_LABEL_BYTES;
    assert_eq!(notice_toolkit("x".repeat(limit)).len(), limit);
    assert_eq!(notice_toolkit("x".repeat(limit + 1)).len(), limit);
    let hostile = notice_toolkit(format!("evil\n{}", "x".repeat(4_096)));
    assert!(
        !hostile.contains('\n') && hostile.len() <= limit,
        "{hostile}"
    );
}

/// A missing grant has its own registered terminal under `AUTHORIZATION_FAILED`,
/// which an older Main replaces with the generic authorization terminal; a
/// depth refusal stays the generic authorization terminal.
#[test]
fn a_missing_grant_ends_with_its_registered_terminal_and_depth_stays_generic() {
    let refused =
        |refusal: DelegationRefusal| elitea_agent_runtime::delegation::RefusedDelegation {
            evidence: elitea_agent_runtime::delegation::DelegationEvidence {
                depth: 1,
                effective_grant_digest: String::new(),
                refusal: Some(refusal.clone()),
                enforced: true,
            },
            refusal,
        };
    let grant = super::super::application_tools::delegation_refused_error(
        DelegatedCallee::Agent,
        &refused(DelegationRefusal::GrantMissing {
            kind: GrantKind::Toolkit,
            name: "batch".to_owned(),
            toolkit: None,
        }),
    );
    let kind = crate::execution::assembly_failure(&grant);
    assert_eq!(kind, RuntimeFailureKind::DelegationGrantMissing);
    let error = crate::protocol::output::runtime_error(kind);
    assert_eq!(
        error.code,
        crate::protocol::elitea::runtime::v1::RuntimeErrorCodeV1::AuthorizationFailed as i32
    );
    assert!(!error.retryable);
    assert!(!error.safe_message.contains("batch"), "data-free terminal");
    // Review 1 item 3: it stands alone; no client or path promises a notice.
    assert!(
        !error.safe_message.contains("notice"),
        "{}",
        error.safe_message
    );
    assert_eq!(
        kind.older_main_fallback(),
        Some(RuntimeFailureKind::AuthorizationFailed)
    );
    let depth = super::super::application_tools::delegation_refused_error(
        DelegatedCallee::Agent,
        &refused(DelegationRefusal::DepthExceeded {
            depth: 2,
            max_depth: 1,
        }),
    );
    assert_eq!(
        crate::execution::assembly_failure(&depth),
        RuntimeFailureKind::AuthorizationFailed
    );
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
    assert_refused_naming(&error, DelegatedCallee::Agent, "batch");
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
    assert!(error.delegation_refusal().is_none(), "{}", describe(&error));
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
fn ordinary_and_pipeline_roots_with_the_same_attachments_hold_the_same_ceiling() {
    let tools = vec![
        mcp_toolkit(),
        mcp_toolkit_at(93, "unused", UNUSED_URL),
        reference((3, 4), "agent", "worker"),
    ];
    let policy = ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy");
    let authority =
        test_runtime_context_authority().with_delegation_policy_for_test(enforcing_policy(4));
    let mut ordinary = ordinary_request(AgentExecutionKind::Application);
    ordinary.payload.application["version_details"]["tools"] = Value::Array(tools.clone());
    let ordinary_snapshot = crate::toolkits::FrozenToolSnapshot::from_request(&ordinary)
        .expect("ordinary snapshot")
        .apply_policy(&policy);
    let ordinary_scope = super::super::application_tools::root_delegation_scope(
        &authority,
        AgentExecutionKind::Application,
        &ordinary_snapshot,
    );
    let pipeline = root_request(&root_yaml("worker"), tools);
    let admitted = authorized(&pipeline)
        .admit_pipeline_with_policy(&policy)
        .expect("pipeline admission");
    let (profile, _, toolsets, ..) = admitted.into_parts();
    let aliases = profile.definition().runtime_toolkit_aliases();
    let pipeline_scope = super::super::application_tools::root_delegation_scope(
        &authority,
        AgentExecutionKind::Application,
        &toolsets,
    );
    assert_eq!(pipeline_scope.cache_key(), ordinary_scope.cache_key());
    assert_eq!(
        pipeline_scope.grants().digest(),
        GrantSet::from_tools(&[mcp_toolkit(), mcp_toolkit_at(93, "unused", UNUSED_URL)]).digest(),
        "the ceiling is exactly the root's own toolkits"
    );
    let reduced = super::super::application_tools::root_delegation_scope(
        &authority,
        AgentExecutionKind::Application,
        &toolsets.retain_toolkit_names(&aliases),
    );
    assert_ne!(reduced.cache_key(), pipeline_scope.cache_key());
}

/// C3: a child that was already allowed (it asks only for a toolkit a root
/// node uses) gets the same effective grants, digest for digest, under the
/// new ceiling as under the alias-reduced one; the golden pins the value.
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
        test_runtime_context_authority().with_delegation_policy_for_test(enforcing_policy(
            elitea_agent_runtime::delegation::DEFAULT_MAX_DEPTH,
        )),
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
    )
    .await;
    let Err(error) = outcome else {
        panic!("a refused member must end the run at start: {outcome:?}");
    };
    assert_refused_naming(&error, DelegatedCallee::Agent, "batch");
    assert_eq!(model_calls(&captured), 0);
}
