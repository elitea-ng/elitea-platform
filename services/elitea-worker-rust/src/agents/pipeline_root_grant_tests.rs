//! FU-335: a pipeline root's delegation grant ceiling.
//!
//! Every test assembles through the real `PipelineNativeAgentAssembler::assemble`
//! (`bind_node_runtimes`, the frozen composition admission, the scoped resolver)
//! under an ENFORCING delegation policy: the shared fixture authority reports
//! narrowing only (`test_runtime_context_authority`), which is why no earlier
//! pipeline test met this. Only the platform RPC, the MCP server and the model
//! gateway are scripted.

use super::scoped_tests::{
    BatchConnector, Saved, reference, root_yaml, saved_version, text, tool_call_response,
};
use super::*;
use std::collections::BTreeMap;

/// The MCP toolkit row `id` every level of the reported shape attaches.
fn mcp_toolkit() -> Value {
    json!({"id": 92, "type": "mcp", "toolkit_name": "batch",
        "settings": {"url": "https://mcp.example.invalid/v1/mcp", "headers": null,
            "client_id": null, "client_secret": null, "scopes": null, "timeout": 30,
            "selected_tools": ["get_issues", "echo", "reverse"], "enable_caching": true,
            "cache_ttl": 300, "ssl_verify": true}})
}

/// Saved pipeline P1: one direct MCP node on the toolkit.
const MCP_NODE_PIPELINE: &str = "state:\n  input: str\n  messages: list\n  records: dict\nentry_point: direct\nnodes:\n  - id: direct\n    type: mcp\n    toolkit_name: batch\n    tool: echo\n    input_mapping:\n      release: {type: fixed, value: '1.2'}\n    output: [records]\n    structured_output: true\n    transition: END\n";

/// A Map root whose member worker is the saved Agent `worker`.
const MAP_ROOT: &str = "state:\n  items: {type: list, value: [alpha]}\n  answer: {type: str, value: ''}\n  mapped: {type: list, value: []}\n  messages: {type: list, value: []}\nentry_point: each\nnodes:\n  - {id: each, type: map, worker: member, source: items, item: entity, index: item_index, outputs: [answer], destination: mapped, max_items: 4, max_concurrency: 1, reduction: ordered_collection, transition: END}\n  - {id: member, type: agent, tool: worker, input: [entity, item_index], input_mapping: {task: {type: fixed, value: partition}}, output: [answer]}\n";

fn enforcing(
    request: &super::super::request::AgentExecutionRequest,
) -> AuthorizedNativeAssembly<'_> {
    let policy = elitea_agent_runtime::delegation::DelegationPolicy::new(
        elitea_agent_runtime::delegation::DEFAULT_MAX_DEPTH,
        elitea_agent_runtime::delegation::NarrowingMode::Enforce,
        [],
    )
    .expect("enforcing delegation policy");
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
/// its saved member by it).
struct SavedVersions(BTreeMap<Saved, Value>);

#[tonic::async_trait]
impl ContextRoutes for SavedVersions {
    async fn answer(&self, call: ContextCall) -> RuntimeReply {
        let unavailable = || tonic::Status::unavailable("saved version fixture");
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
            let details = self.0.get(&identity).ok_or_else(unavailable)?;
            json!({"schema_version":"elitea.runtime.application-version.v1","project_id":17,
                "application_id":identity.0,"version_id":identity.1,"version_details":details,
                "frozen_definition_sha256":format!("{:02x}", identity.0).repeat(32)})
        };
        runtime_response(&value)
    }
}

fn runtime_clients(
    versions: &BTreeMap<Saved, Value>,
    outcomes: Vec<TestModelGatewayOutcome>,
) -> (Arc<PlatformClient>, Arc<ModelFacade>, CapturedModelRequests) {
    let runtime = RuntimeContextClient::with_rpc(
        Routes(SavedVersions(versions.clone())),
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
    (
        Arc::new(PlatformClient::new(Arc::new(runtime))),
        Arc::new(ModelFacade::from_gateway(gateway)),
        captured,
    )
}

fn assembler(
    versions: &BTreeMap<Saved, Value>,
    outcomes: Vec<TestModelGatewayOutcome>,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    let (platform, model_facade, captured) = runtime_clients(versions, outcomes);
    (
        PipelineNativeAgentAssembler::with_state(
            Arc::new(InMemorySessionService::new()) as Arc<dyn SessionService>,
            Arc::new(MemoryCheckpointer::new()) as Arc<dyn Checkpointer>,
        )
        .with_runtime_clients(platform, model_facade)
        .with_mcp_connector(Arc::new(BatchConnector)),
        captured,
    )
}

fn offered_tools(captured: &CapturedModelRequests, request: usize) -> Vec<String> {
    let requests = captured.lock().expect("captured model requests");
    let Some(request) = requests.get(request) else {
        return Vec::new();
    };
    let body: Value = serde_json::from_slice(&request.body).expect("model request JSON");
    body["tools"]
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
        "assembly error code={:?} message={error} cause={:?}",
        error.code(),
        error.cause().map(|cause| (cause.code(), cause.detail()))
    )
}

/// FU-335 (a), at the root: the pipeline root attaches the MCP toolkit but no
/// node of its own uses it; its Agent node runs the saved Agent S that holds
/// the same toolkit. S must be delegated (root ⊇ S) and run with its tools.
#[tokio::test(flavor = "current_thread")]
async fn a_pipeline_root_delegates_an_attached_but_unused_toolkit_to_its_agent_node() {
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Use the toolkit.", &json!({}), vec![mcp_toolkit()]),
    )]);
    let (assembler, captured) = assembler(&versions, vec![text("worker answer")]);
    let request = root_request(
        &root_yaml("worker"),
        vec![mcp_toolkit(), reference((3, 4), "agent", "worker")],
    );
    let invocation = match assembler.assemble(enforcing(&request)).await {
        Ok(invocation) => invocation,
        Err(error) => panic!(
            "the pipeline root refused its own toolkit: {}",
            describe(&error)
        ),
    };
    let outcome = run(invocation).await;
    assert_eq!(outcome.failure, None, "{outcome:?}");
    assert!(
        outcome
            .browser
            .iter()
            .any(|event| event["content"] == "worker answer"),
        "{outcome:?}"
    );
    let offered = offered_tools(&captured, 0);
    assert!(
        ["get_issues", "echo", "reverse"]
            .iter()
            .all(|tool| offered.contains(&(*tool).to_owned())),
        "S runs with the toolkit its caller delegated: {offered:?}"
    );
}

/// FU-335 (a), the reported shape: root -> Agent node -> S -> pipeline tool
/// P1 -> MCP node, each level attaching the toolkit. It completes.
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
    let (assembler, captured) = assembler(
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
    let invocation = match assembler.assemble(enforcing(&request)).await {
        Ok(invocation) => invocation,
        Err(error) => panic!("the reported shape did not assemble: {}", describe(&error)),
    };
    let outcome = run(invocation).await;
    assert_eq!(outcome.failure, None, "{outcome:?}");
    assert!(
        outcome
            .browser
            .iter()
            .any(|event| event["content"] == "relayed p1"),
        "{outcome:?}"
    );
    let requests = captured.lock().expect("captured model requests");
    assert_eq!(requests.len(), 2, "S calls P1, then answers");
    let second: Value = serde_json::from_slice(&requests[1].body).expect("second request");
    assert!(
        !second["messages"]
            .to_string()
            .contains("DELEGATION_GRANT_MISSING"),
        "P1 must run, not answer a refusal: {second}"
    );
}

/// FU-335 (b), the readable refusal at the root: a root that does NOT hold the
/// toolkit refuses S, and the run must end with the DELEGATION text naming
/// the missing toolkit, never the generic internal or unsupported text.
#[tokio::test(flavor = "current_thread")]
async fn a_root_agent_node_refused_a_toolkit_names_the_missing_grant() {
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Use the toolkit.", &json!({}), vec![mcp_toolkit()]),
    )]);
    let (assembler, captured) = assembler(&versions, vec![text("unexpected")]);
    let request = root_request(
        &root_yaml("worker"),
        vec![reference((3, 4), "agent", "worker")],
    );
    let observed = match assembler.assemble(enforcing(&request)).await {
        Err(error) => describe(&error),
        Ok(invocation) => format!("{:?}", run(invocation).await),
    };
    assert_eq!(
        captured.lock().expect("captured model requests").len(),
        0,
        "a refused S never calls the model: {observed}"
    );
    assert!(
        observed.contains("DELEGATION_GRANT_MISSING") && observed.contains("batch"),
        "the refusal names the missing toolkit: {observed}"
    );
}

/// FU-335 (c): a saved Agent with Attachments and Agent memory on, run by a
/// pipeline root's Agent node, under a REPORTING policy (no grant can cut a
/// tool) and with no toolkit anywhere. If it is still offered no tools, the
/// cause is not the grant ceiling.
#[tokio::test(flavor = "current_thread")]
async fn a_pipeline_agent_node_offers_its_saved_agents_built_in_tools() {
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version(
            "agent",
            "Remember things.",
            &json!({"internal_tools": ["attachments"], "memory": {"enabled": true}}),
            Vec::new(),
        ),
    )]);
    let (assembler, captured) = assembler(&versions, vec![text("worker answer")]);
    let request = root_request(
        &root_yaml("worker"),
        vec![reference((3, 4), "agent", "worker")],
    );
    let invocation = assembler
        .assemble(authorized(&request))
        .await
        .unwrap_or_else(|error| panic!("{}", describe(&error)));
    let outcome = run(invocation).await;
    assert_eq!(outcome.failure, None, "{outcome:?}");
    let offered = offered_tools(&captured, 0);
    assert!(
        offered.contains(&"recall".to_owned()) && offered.contains(&"list_attachments".to_owned()),
        "the saved Agent's built-in tools are offered under a reporting policy: {offered:?}"
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
) -> (String, CapturedModelRequests) {
    let db = crate::state::postgres_session_tests::IsolatedPostgres::create(url).await;
    crate::state::postgres_session_tests::install_schema(&db.pool).await;
    sqlx::raw_sql(super::restart_pg_tests::CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let (platform, model_facade, captured) = runtime_clients(versions, outcomes);
    let assembler = PipelineNativeAgentAssembler::postgres(
        db.pool.clone(),
        crate::state::SessionLimits::default(),
        crate::state::CheckpointLimits::default(),
        Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("empty tool policy")),
        platform,
        model_facade,
    )
    .with_mcp_connector(Arc::new(BatchConnector))
    .with_fixed_parallel_for_tests()
    .with_fanout_decisions_for_tests(ledger.client(1));
    let request = root_request(MAP_ROOT, root_tools);
    let policy = elitea_agent_runtime::delegation::DelegationPolicy::new(
        elitea_agent_runtime::delegation::DEFAULT_MAX_DEPTH,
        elitea_agent_runtime::delegation::NarrowingMode::Enforce,
        [],
    )
    .expect("enforcing delegation policy");
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
    let observed = match assembler.assemble(claim).await {
        Err(error) => describe(&error),
        Ok(invocation) => format!("{:?}", run(invocation).await),
    };
    db.pool.close().await;
    (observed, captured)
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
    let (observed, _) = map_outcome(
        &url,
        &versions,
        vec![text("item done")],
        vec![reference((3, 4), "agent", "worker")],
    )
    .await;
    assert!(observed.contains("failure: None"), "{observed}");
}

/// FU-335 (a)+(b), in a Map member: the root attaches the toolkit, no root
/// node uses it, the member Agent holds it. The member runs.
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
    let (observed, captured) = map_outcome(
        &url,
        &versions,
        vec![text("item done")],
        vec![mcp_toolkit(), reference((3, 4), "agent", "worker")],
    )
    .await;
    assert!(observed.contains("failure: None"), "{observed}");
    let offered = offered_tools(&captured, 0);
    assert!(offered.contains(&"echo".to_owned()), "{offered:?}");
}

/// FU-335 (b), the readable refusal in a Map member: the root lacks the
/// toolkit, and the run ends with the DELEGATION refusal naming it, never
/// the generic unsupported-configuration text.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_map_member_refused_a_toolkit_names_the_missing_grant() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-335 Map proof");
        return;
    };
    let versions = BTreeMap::from([(
        (3, 4),
        saved_version("agent", "Use the toolkit.", &json!({}), vec![mcp_toolkit()]),
    )]);
    let (observed, captured) = map_outcome(
        &url,
        &versions,
        vec![text("unexpected")],
        vec![reference((3, 4), "agent", "worker")],
    )
    .await;
    assert_eq!(
        captured.lock().expect("captured model requests").len(),
        0,
        "{observed}"
    );
    assert!(
        observed.contains("DELEGATION_GRANT_MISSING") && observed.contains("batch"),
        "the member refusal names the missing toolkit: {observed}"
    );
}
