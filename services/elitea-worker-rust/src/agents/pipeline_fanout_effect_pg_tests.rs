//! D2 precondition (T-2A §7): an effectful Application (saved Agent) child of a
//! fixed Parallel writes the "may have started" marker before its tool runs, and
//! a replacement Worker never dispatches that tool again.
//!
//! Real `PostgreSQL`, one OS process per Worker (as `pipeline_restart_pg_tests.rs`),
//! production `PipelineNativeAgentAssembler::postgres` with the test-only
//! fan-out composition (`with_fixed_parallel_for_tests`: scoped Agent
//! participants, `FanoutTurnAuthority`, `ParallelCompilerBinding::for_tests`).
//!
//! `gather` runs three branches of the same saved Agent, one at a time in
//! declared order:
//! * `first`: the Agent answers in text and completes;
//! * `second`: the Agent calls `lookup_release`, an MCP tool without a read-only
//!   hint (effectful). The tool checks, from inside its own execution, that
//!   `PostgreSQL` already holds the child's `tool_may_have_started` boundary
//!   written by this claim, then kills the process;
//! * `third`: never started before the crash.
//!
//! A replacement Worker reclaims the same execution with a later claim. The
//! completed branch is replayed from its receipt, `second` fails typed
//! (`graph.fanout.may_have_started`) with zero tool calls and zero model calls,
//! and `third` runs. On Error `continue` lands a failed join entry; `stop` fails
//! the node.
//!
//! The D2 narrowing (T-2A §8) on the same harness: when `second` dies in its
//! model call it resumes from its own model checkpoint (R), and when it dies
//! in a read-only tool that tool runs again (I); both complete all three.
//!
//! The same three cases run with inline LLM branches (T-2A D1): each branch is
//! an `llm` node offered the same MCP tool, with its own model checkpoint.

use super::restart_pg_tests::{CHECKPOINT_MIGRATION, connect, model_requests, run_phase};
use super::*;
use crate::protocol::control::test_session_authority_for_reclaim;
use crate::state::postgres_session_tests::{IsolatedPostgres, install_schema};
use crate::state::{CheckpointLimits, SessionLimits, TestStateWriterLease};

const CHILD_TEST: &str =
    "agents::pipeline_tests::fanout_effect_pg_tests::postgres_fanout_effect_child";
const DATABASE_ENV: &str = "ELITEA_FANOUT_EFFECT_PG_DATABASE";
const ON_ERROR_ENV: &str = "ELITEA_FANOUT_EFFECT_PG_ON_ERROR";
const PHASE_ENV: &str = "ELITEA_FANOUT_EFFECT_PG_PHASE";
/// `effect` (effectful tool in flight), `read_only` (I) or `mid_model` (R).
const MODE_ENV: &str = "ELITEA_FANOUT_EFFECT_PG_MODE";
/// `agent` (saved Agent branches) or `llm` (inline LLM branches).
const KIND_ENV: &str = "ELITEA_FANOUT_EFFECT_PG_KIND";
const EXECUTION: &str = "0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e";
const GENERATION: u64 = 3;
/// The tool saw its own boundary in `PostgreSQL`, then the process died.
const CRASH_EXIT_CODE: i32 = 87;
/// The tool started without a durable boundary: the precondition fails.
const MARKER_MISSING_EXIT_CODE: i32 = 88;
const MAY_HAVE_STARTED: &str = "graph.fanout.may_have_started";

fn pipeline_yaml(on_error: &str) -> String {
    let branch = |id: &str, release: &str| {
        format!(
            "  - id: {id}_call\n    type: agent\n    tool: release-agent\n    input_mapping:\n      task: {{type: fixed, value: 'Summarize release {release}'}}\n    output: [{id}]\n"
        )
    };
    format!(
        "state:\n  first: str\n  second: str\n  third: str\n  joined: list\n  messages: list\nentry_point: gather\nnodes:\n  - id: gather\n    type: parallel\n    branches: [{{id: first, node: first_call}}, {{id: second, node: second_call}}, {{id: third, node: third_call}}]\n    max_concurrency: 1\n    wait: all\n    on_error: {on_error}\n    output: [joined]\n    transition: END\n{}{}{}",
        branch("first", "1.0"),
        branch("second", "1.1"),
        branch("third", "1.2"),
    )
}

fn request(on_error: &str) -> super::super::request::AgentExecutionRequest {
    request_for(on_error, "agent")
}

/// The same three branches as inline LLM nodes offered the same MCP tool.
fn inline_request(on_error: &str) -> super::super::request::AgentExecutionRequest {
    let mut request = llm_mcp_pipeline_request(
        "release intelligence",
        &["lookup_release"],
        &["lookup_release"],
    );
    let yaml = pipeline_yaml(on_error)
        .replace(
            "    type: agent\n    tool: release-agent\n",
            "    type: llm\n    tool_names:\n      release intelligence: [\"lookup_release\"]\n",
        )
        .replace(
            "    input_mapping:\n      task:",
            "    input_mapping:\n      system: {type: fixed, value: 'INLINE-BRANCH-SYSTEM'}\n      task:",
        );
    assert!(!yaml.contains("type: agent"), "{yaml}");
    assert!(yaml.contains("INLINE-BRANCH-SYSTEM"), "{yaml}");
    request.payload.application["version_details"]["instructions"] = json!(yaml);
    request
}

/// Every branch selects one saved participant of `agent_type`.
fn request_for(on_error: &str, agent_type: &str) -> super::super::request::AgentExecutionRequest {
    let mut request = agent_pipeline_request("release-agent", agent_type);
    request.payload.application["version_details"]["instructions"] = json!(pipeline_yaml(on_error));
    request
}

/// One saved Agent with one MCP tool that publishes no read-only hint.
fn effectful_agent() -> Value {
    json!({
        "agent_type": "agent", "instructions": "Read the release evidence and summarize it.",
        "meta": {}, "variables": [],
        "tools": [{"id": 92, "type": "mcp", "toolkit_name": "release intelligence",
            "settings": {"url": "https://mcp.example.invalid/v1/mcp", "headers": null,
                "client_id": null, "client_secret": null, "scopes": null, "timeout": 30,
                "selected_tools": ["lookup_release"], "enable_caching": true, "cache_ttl": 300,
                "ssl_verify": true}}],
        "llm_settings": {"model_name": "child-model", "model_project_id": 23,
            "max_tokens": 2048, "reasoning_effort": null, "temperature": 0.2,
            "openai_compatible": true}
    })
}

/// The saved Agent, which may also ask the user (`ask_user`).
fn asking_agent() -> Value {
    let mut agent = effectful_agent();
    agent["meta"] = json!({"internal_tools": ["ask_user"]});
    agent
}

/// Child model-scope sessions (`elitea-model-*`) holding a durable
/// `tool_may_have_started` boundary written by `claim`.
async fn tool_boundaries(pool: &sqlx::PgPool, claim: &str) -> Vec<String> {
    phase_boundaries(pool, claim, "tool_may_have_started").await
}

/// Child model-scope sessions holding a durable checkpoint `phase` written by `claim`.
async fn phase_boundaries(pool: &sqlx::PgPool, claim: &str, phase: &str) -> Vec<String> {
    sqlx::query_scalar::<_, String>(
        "SELECT DISTINCT session_id FROM elitea_runtime.agent_session_events \
         WHERE session_id LIKE 'elitea-model-%' \
           AND event_payload LIKE '%' || $4 || '%' \
           AND writer_claim_id = $1 AND writer_execution_id = $2 AND writer_generation = $3",
    )
    .bind(claim)
    .bind(EXECUTION)
    .bind(i64::try_from(GENERATION).expect("generation"))
    .bind(phase)
    .fetch_all(pool)
    .await
    .expect("read child checkpoint boundaries")
}

/// The crash process's tool. Inside its own execution it proves the boundary
/// is already durable, then dies with the call in flight: an effectful tool
/// behind `tool_may_have_started`, a read-only one behind a replayable
/// `delegation_pending` batch.
struct CrashingMcpConnector {
    pool: sqlx::PgPool,
    read_only: bool,
}

#[async_trait]
impl McpConnector for CrashingMcpConnector {
    async fn connect(
        &self,
        _config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        Ok(Arc::new(BasicToolset::new(
            "fixture_mcp",
            vec![Arc::new(CrashingMcpTool {
                inner: PipelineMcpTool {
                    calls: Arc::new(AtomicUsize::new(0)),
                    read_only: self.read_only,
                },
                pool: self.pool.clone(),
            })],
        )))
    }
}

struct CrashingMcpTool {
    inner: PipelineMcpTool,
    pool: sqlx::PgPool,
}

#[async_trait]
impl Tool for CrashingMcpTool {
    fn name(&self) -> &str {
        self.inner.name()
    }
    fn description(&self) -> &str {
        self.inner.description()
    }
    fn parameters_schema(&self) -> Option<Value> {
        self.inner.parameters_schema()
    }
    fn is_read_only(&self) -> bool {
        self.inner.is_read_only()
    }
    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        let effectful = tool_boundaries(&self.pool, "claim-1").await.len();
        let replayable = phase_boundaries(&self.pool, "claim-1", "delegation_pending")
            .await
            .len();
        let durable = if self.inner.is_read_only() {
            (effectful, replayable) == (0, 1)
        } else {
            effectful == 1
        };
        std::process::exit(if durable {
            CRASH_EXIT_CODE
        } else {
            MARKER_MISSING_EXIT_CODE
        })
    }
}

/// The platform answers by request path, not call order: each scoped Agent
/// node materializes its own participant (same shape as the 2a-1 fixture).
/// The one saved participant every branch selects.
struct SavedAgentVersions {
    details: Value,
    /// A second saved participant (application 5, version 6), when a test
    /// mixes two kinds of branch.
    second: Option<Value>,
}

#[tonic::async_trait]
impl crate::transport::input_content::content_fake::ContextRoutes for SavedAgentVersions {
    async fn answer(
        &self,
        call: crate::transport::input_content::content_fake::ContextCall,
    ) -> Result<crate::transport::input_content::content_fake::ContextAnswer, tonic::Status> {
        let value = if call.route == "elitea-client-token" {
            json!({"schema_version":"elitea.runtime.elitea-client-token.v1","project_id":17,
                "token":"ephemeral-fanout-token"})
        } else if let Some(second) = self
            .second
            .as_ref()
            .filter(|_| call.route.ends_with("/5/versions/6"))
        {
            json!({"schema_version":"elitea.runtime.application-version.v1","project_id":17,
                "application_id":5,"version_id":6,"version_details":second,
                "frozen_definition_sha256":"cd".repeat(32)})
        } else if call.route.ends_with("/3/versions/4") {
            json!({"schema_version":"elitea.runtime.application-version.v1","project_id":17,
                "application_id":3,"version_id":4,"version_details":self.details,
                // Main's authenticated digest of the frozen version: the saved
                // participant fingerprint a Parallel branch is bound to.
                "frozen_definition_sha256":"ab".repeat(32)})
        } else {
            return Err(tonic::Status::unavailable("fixture route"));
        };
        Ok(crate::transport::input_content::content_fake::ContextAnswer::from(&value))
    }
}

fn saved_runtime(
    outcomes: Vec<TestModelGatewayOutcome>,
    details: Value,
) -> (Arc<PlatformClient>, Arc<ModelFacade>, CapturedModelRequests) {
    saved_runtime_with(outcomes, details, None)
}

fn saved_runtime_with(
    outcomes: Vec<TestModelGatewayOutcome>,
    details: Value,
    second: Option<Value>,
) -> (Arc<PlatformClient>, Arc<ModelFacade>, CapturedModelRequests) {
    let runtime = RuntimeContextClient::with_rpc(
        crate::transport::input_content::content_fake::Routes(SavedAgentVersions {
            details,
            second,
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
    (
        Arc::new(PlatformClient::new(Arc::new(runtime))),
        Arc::new(ModelFacade::from_gateway(gateway)),
        captured,
    )
}

/// One Worker process for `claim` on the same execution: a reclaim after a
/// crash has a later attempt, lease epoch and claim start.
fn assembler(
    pool: &sqlx::PgPool,
    outcomes: Vec<TestModelGatewayOutcome>,
    connector: Arc<dyn McpConnector>,
    details: Value,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    let (platform, model_facade, captured) = saved_runtime(outcomes, details);
    let assembler = PipelineNativeAgentAssembler::postgres(
        pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        Arc::new(
            ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new())
                .expect("empty tool policy"),
        ),
        platform,
        model_facade,
    )
    .with_mcp_connector(connector)
    .with_fixed_parallel_for_tests();
    (assembler, captured)
}

fn claim(
    request: &super::super::request::AgentExecutionRequest,
    attempt: u8,
) -> AuthorizedNativeAssembly<'_> {
    AuthorizedNativeAssembly::from_authorized(
        request,
        test_runtime_context_authority(),
        test_session_authority_for_reclaim(EXECUTION, GENERATION, attempt),
        Arc::new(TestStateWriterLease::current()),
        AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
    )
}

/// A reclaim as production runs it with model-checkpoint recovery on: inspect
/// the root, authorize the evidence, assemble the checkpoint, and release the
/// Runner only for the authorized frontier.
async fn recovered(
    assembler: &PipelineNativeAgentAssembler,
    request: &super::super::request::AgentExecutionRequest,
    execution: &str,
    attempt: u8,
) -> Result<AssembledNativeAgentInvocation<PipelineAgentCompletion>, String> {
    let evidence = crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
        assembler,
        request,
        &AuthorizedNativeCommandBinding::fixture_for_execution(execution),
        test_session_authority_for_reclaim(execution, GENERATION, attempt),
        Arc::new(TestStateWriterLease::current()),
    )
    .await
    .map_err(|error| format!("inspect: {error}"))?;
    let (authorizer, control) = crate::protocol::control::test_checkpoint_authorizer(
        execution,
        GENERATION,
        evidence.digest(),
    );
    let authorization = authorizer
        .authorize(&control, evidence)
        .await
        .map_err(|_| "authorize".to_owned())?;
    let pending = assembler
        .assemble_checkpoint(AuthorizedNativeAssembly::from_authorized(
            request,
            test_runtime_context_authority(),
            test_session_authority_for_reclaim(execution, GENERATION, attempt),
            Arc::new(TestStateWriterLease::current()),
            AuthorizedNativeCommandBinding::fixture_for_execution(execution),
        ))
        .await
        .map_err(|error| format!("assemble the checkpoint: {error}"))?;
    let (_, _, _, _, permit) = authorization.into_lifecycle_parts();
    pending
        .authorize_lifecycle(permit)
        .map_err(|error| format!("authorize the lifecycle: {error}"))
}

/// Run the reclaimed pipeline to its end through the browser projector: the
/// `agent_fanout_member` statuses it emitted on completion, the failure's
/// code otherwise.
async fn run_to_end(
    invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> Result<Vec<Value>, String> {
    run_collecting(invocation).await.map(|(members, _)| members)
}

/// As [`run_to_end`], also returning every `interrupt_card_v1` raised.
async fn run_collecting(
    invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> Result<(Vec<Value>, Vec<Value>), String> {
    run_deciding(invocation, &|_| {}).await
}

/// As [`run_collecting`], calling `decide` with each card as its frame
/// arrives, while the run goes on (a user deciding live).
async fn run_deciding(
    invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
    decide: &dyn Fn(&Value),
) -> Result<(Vec<Value>, Vec<Value>), String> {
    run_keyed_deciding(invocation, decide)
        .await
        .map(|(members, cards, _)| (members, cards))
}

/// As [`run_collecting`], also returning each frame's `parent_agent_call_id`
/// with its first tier's ordinal, in frame order (frames without one skipped).
async fn run_keyed(
    invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> Result<(Vec<Value>, Vec<Value>, Vec<(String, Value)>), String> {
    run_keyed_deciding(invocation, &|_| {}).await
}

/// As [`run_keyed`], calling `decide` with each card as its frame arrives.
async fn run_keyed_deciding(
    mut invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
    decide: &dyn Fn(&Value),
) -> Result<(Vec<Value>, Vec<Value>, Vec<(String, Value)>), String> {
    invocation
        .project_start(timestamp(1))
        .expect("pipeline browser start");
    let (mut run, mut projector, completion) = invocation.start().expect("pipeline start");
    let mut members = Vec::new();
    let mut cards = Vec::new();
    let mut keys = Vec::new();
    loop {
        match run.next_event().await {
            Err(error) => {
                return Err(format!(
                    "{error} {:?} {:?}",
                    error.upstream_code(),
                    error.upstream_message_for_tests()
                ));
            }
            Ok(Some(event)) => {
                for frame in projector
                    .project(&event)
                    .map_err(|error| error.to_string())?
                {
                    // One pause authority: a fan-out never raises a legacy card;
                    // its only pause terminal is the park (fanout_parked_v1).
                    let metadata: Value =
                        serde_json::from_slice(&frame.response_metadata).unwrap_or_default();
                    if let Some(key) = metadata["parent_agent_call_id"].as_str() {
                        keys.push((
                            key.to_owned(),
                            metadata["parent_agent_path"][0]["sibling_ordinal"].clone(),
                        ));
                    }
                    let legacy = frame.r#type == "mcp_authorization_required"
                        || (frame.r#type == "agent_hitl_interrupt"
                            && (metadata.get("fanout_parked_v1").is_none()
                                || metadata.get("hitl_interrupts").is_some()
                                || metadata.get("hitl_interrupt").is_some()));
                    assert!(
                        !legacy,
                        "a legacy pause frame inside a fan-out: {}",
                        frame.r#type
                    );
                    if frame.r#type == "agent_interrupt_pending" {
                        let metadata: Value = serde_json::from_slice(&frame.response_metadata)
                            .expect("card frame metadata");
                        decide(&metadata["interrupt_card_v1"]);
                        cards.push(metadata["interrupt_card_v1"].clone());
                    }
                    if frame.r#type == "agent_fanout_member" {
                        let metadata: Value = serde_json::from_slice(&frame.response_metadata)
                            .expect("member frame metadata");
                        members.push(metadata["fanout_member_status_v1"].clone());
                    }
                }
            }
            Ok(None) => break,
        }
    }
    completion
        .select()
        .await
        .map(|_| (members, cards, keys))
        .map_err(|error| error.to_string())
}

/// The latest root checkpoint's joined entries.
async fn joined(pool: &sqlx::PgPool) -> Option<Vec<Value>> {
    let state = sqlx::query_scalar::<_, String>(
        "SELECT state FROM elitea_runtime.agent_graph_checkpoints \
         WHERE thread_id NOT LIKE '%/%' AND state LIKE '%\"joined\"%' \
         ORDER BY save_ordinal DESC LIMIT 1",
    )
    .fetch_optional(pool)
    .await
    .expect("read the joined root checkpoint")?;
    let state: Value = serde_json::from_str(&state).expect("root state JSON");
    state["joined"].as_array().cloned()
}

async fn scenario(url: &str, mode: &str, on_error: &str) {
    scenario_for(url, "agent", mode, on_error).await;
}

async fn scenario_for(url: &str, kind: &str, mode: &str, on_error: &str) {
    let db = IsolatedPostgres::create(url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    for (phase, expected) in [("crash", Some(CRASH_EXIT_CODE)), ("recover", Some(0))] {
        let mut command = std::process::Command::new(std::env::current_exe().expect("test binary"));
        command
            .args(["--exact", CHILD_TEST, "--nocapture", "--test-threads=1"])
            .env(DATABASE_ENV, &db.database_name)
            .env(ON_ERROR_ENV, on_error)
            .env(MODE_ENV, mode)
            .env(KIND_ENV, kind)
            .env(PHASE_ENV, phase);
        let output = run_phase(phase, command);
        let stdout = String::from_utf8_lossy(&output.stdout);
        assert_eq!(
            output.status.code(),
            expected,
            "{phase} process exit: {stdout} {}",
            String::from_utf8_lossy(&output.stderr)
        );
        if phase == "crash" {
            // (a) the boundary of exactly the effectful child, fenced to claim 1;
            // a read-only batch or a model call leaves a replayable one instead.
            assert_eq!(
                tool_boundaries(&db.pool, "claim-1").await.len(),
                usize::from(mode == "effect")
            );
            // (c) two branches of the same saved Agent: two model-scope sessions.
            let sessions = sqlx::query_scalar::<_, String>(
                "SELECT DISTINCT session_id FROM elitea_runtime.agent_session_events \
                 WHERE session_id LIKE 'elitea-model-%' AND writer_claim_id = 'claim-1'",
            )
            .fetch_all(&db.pool)
            .await
            .expect("read child model-scope sessions");
            assert_eq!(
                sessions.len(),
                2,
                "first and second branch own distinct model-scope sessions: {sessions:?}"
            );
        } else {
            assert!(stdout.contains("1 passed"), "{phase}: {stdout}");
        }
    }
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_effectful_fanout_child_may_have_started_continues_with_a_failed_entry() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario(&url, "effect", "continue").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_effectful_fanout_child_may_have_started_stops_the_node() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario(&url, "effect", "stop").await;
}

/// FU-246: two saved Agent members need a sensitive-tool approval; each raises
/// its own card, one is approved and one rejected live, only the approved call
/// runs, once, and the pipeline joins with no legacy pause frame.
#[tokio::test(flavor = "current_thread")]
async fn postgres_fanout_agent_branches_approve_and_reject_their_own_calls() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let (platform, model_facade, captured) = saved_runtime(
        vec![
            TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response()),
            TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response()),
            TestModelGatewayOutcome::Response(pipeline_text_response("done")),
            TestModelGatewayOutcome::Response(pipeline_text_response("done")),
            TestModelGatewayOutcome::Response(pipeline_text_response("done")),
        ],
        effectful_agent(),
    );
    let assembler = PipelineNativeAgentAssembler::postgres(
        db.pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        sensitive_pipeline_mcp_policy(),
        platform,
        model_facade,
    )
    .with_mcp_connector(Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::clone(&tool_calls),
        read_only: false,
    }))
    .with_fixed_parallel_for_tests()
    .with_fanout_decisions_for_tests(ledger.client(1));
    let request = request("continue");
    let invocation = assembler
        .assemble(claim(&request, 1))
        .await
        .expect("assemble the fan-out pipeline");
    let (_, cards) = run_deciding(invocation, &|card| {
        let key = card["interrupt_key"].as_str().expect("card key");
        match (card["fanout_v1"]["member"].as_str(), card["kind"].as_str()) {
            (Some("first"), Some("tool_guard")) => ledger.decide(key, "approve", None),
            (Some("second"), Some("tool_guard")) => ledger.decide(key, "reject", None),
            other => panic!("unexpected card {other:?}"),
        }
    })
    .await
    .expect("the approved and rejected members resume and the pipeline completes");
    assert_eq!(cards.len(), 2, "{cards:?}");
    assert_eq!(
        tool_calls.load(Ordering::Acquire),
        1,
        "only the approved call runs, once"
    );
    assert_eq!(model_requests(&captured), 5);
    let entries = joined(&db.pool).await.expect("joined entries");
    assert_eq!(entries.len(), 3, "{entries:?}");
    for entry in &entries {
        assert_eq!(entry["status"], "succeeded", "{entries:?}");
    }
    db.pool.close().await;
}

/// FU-246: a member whose saved Agent asks the user pauses with its own card
/// (one pause authority: no legacy pause frame), the card is answered live,
/// and only that member's Agent resumes with the answer; the siblings run and
/// the pipeline joins.
#[tokio::test(flavor = "current_thread")]
async fn postgres_fanout_agent_branch_asks_and_resumes_with_the_answer() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let (assembler, captured) = assembler(
        &db.pool,
        vec![
            TestModelGatewayOutcome::Response(pipeline_text_response("first done")),
            TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()),
            TestModelGatewayOutcome::Response(pipeline_text_response("third done")),
            TestModelGatewayOutcome::Response(pipeline_text_response("second done")),
        ],
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        }),
        asking_agent(),
    );
    let request = request("continue");
    let invocation = assembler
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .expect("assemble the fan-out pipeline");
    let (members, cards) = run_deciding(invocation, &|card| {
        let key = card["interrupt_key"].as_str().expect("card key");
        match (card["fanout_v1"]["member"].as_str(), card["kind"].as_str()) {
            (Some("second"), Some("ask_user")) => ledger.decide(key, "answer", Some("Staging")),
            other => panic!("unexpected card {other:?}"),
        }
    })
    .await
    .expect("the answered member resumes and the pipeline completes");
    assert_eq!(cards.len(), 1, "{cards:?}");
    assert_eq!(model_requests(&captured), 4);
    // The resumed Agent sees the answer to its question.
    let last = captured.lock().expect("captured model requests");
    let resumed = String::from_utf8_lossy(&last[3].body).into_owned();
    assert!(resumed.contains("Staging"), "{resumed}");
    drop(last);
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "second" && member["status"] == "end"),
        "{members:?}"
    );
    let entries = joined(&db.pool).await.expect("joined entries");
    assert_eq!(entries.len(), 3, "{entries:?}");
    for entry in &entries {
        assert_eq!(entry["status"], "succeeded", "{entries:?}");
    }
    db.pool.close().await;
}

/// Production composition: a pipeline with a fixed Parallel node needs the
/// claim's decision ledger; without it the run is refused typed before any
/// child starts (a card could never be decided).
#[tokio::test(flavor = "current_thread")]
async fn postgres_production_parallel_without_the_decision_ledger_is_refused_typed() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let (platform, model_facade, captured) = saved_runtime(Vec::new(), effectful_agent());
    let assembler = PipelineNativeAgentAssembler::postgres(
        db.pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        Arc::new(
            ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new())
                .expect("empty tool policy"),
        ),
        platform,
        model_facade,
    )
    .with_mcp_connector(Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::new(AtomicUsize::new(0)),
        read_only: false,
    }));
    let request = request("continue");
    let error = assembler
        .assemble(claim(&request, 1))
        .await
        .err()
        .expect("refused without the ledger");
    assert_eq!(
        error
            .cause()
            .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code),
        Some("graph.fanout.decisions_unavailable")
    );
    assert_eq!(model_requests(&captured), 0);
    db.pool.close().await;
}

/// Inline LLM branches (T-2A D1) over the root's MCP toolkit, which is
/// sensitive for `lookup_release`: `ask` asks the user, `guard` and `deny`
/// each call the tool.
fn inline_llm_request(on_error: &str) -> super::super::request::AgentExecutionRequest {
    let mut request = llm_mcp_pipeline_request(
        "release intelligence",
        &["lookup_release"],
        &["lookup_release"],
    );
    request.payload.internal_tools = vec![ASK_USER_TOOL_NAME.to_owned()];
    let branch = |id: &str, tools: &str| {
        format!(
            "  - id: {id}\n    type: llm\n    input_mapping:\n      system: {{type: fixed, value: 'INLINE-BRANCH-SYSTEM {id}'}}\n      task: {{type: fixed, value: 'Work on {id}'}}\n    output: [{id}_out]\n{tools}"
        )
    };
    let yaml = format!(
        "state:\n  ask_out: str\n  guard_out: str\n  deny_out: str\n  joined: list\n  messages: list\nentry_point: gather\nnodes:\n  - id: gather\n    type: parallel\n    branches: [{{id: first, node: ask}}, {{id: second, node: guard}}, {{id: third, node: deny}}]\n    max_concurrency: 1\n    wait: all\n    on_error: {on_error}\n    output: [joined]\n    transition: END\n{}{}{}",
        branch("ask", "    tool_names:\n      ask_user: [\"ask_user\"]\n"),
        branch(
            "guard",
            "    tool_names:\n      release intelligence: [\"lookup_release\"]\n"
        ),
        branch(
            "deny",
            "    tool_names:\n      release intelligence: [\"lookup_release\"]\n"
        ),
    );
    request.payload.application["version_details"]["instructions"] = json!(yaml);
    request
}

/// T-2A D1: inline LLM branches each pause with their own card (an
/// `ask_user` question and two sensitive-tool approvals), are answered,
/// approved and rejected live while the run goes on, resume and join; only
/// the approved tool runs, once, and nothing raises a legacy pause.
#[tokio::test(flavor = "current_thread")]
async fn postgres_inline_llm_branches_pause_with_their_own_cards_and_resume_live() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let (platform, model_facade, captured) = saved_runtime(
        vec![
            TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()),
            TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response()),
            TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response()),
            TestModelGatewayOutcome::Response(pipeline_text_response("resumed done")),
            TestModelGatewayOutcome::Response(pipeline_text_response("resumed done")),
            TestModelGatewayOutcome::Response(pipeline_text_response("resumed done")),
        ],
        effectful_agent(),
    );
    let assembler = PipelineNativeAgentAssembler::postgres(
        db.pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        sensitive_pipeline_mcp_policy(),
        platform,
        model_facade,
    )
    .with_mcp_connector(Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::clone(&tool_calls),
        read_only: false,
    }))
    .with_fixed_parallel_for_tests()
    .with_fanout_decisions_for_tests(ledger.client(1));
    let request = inline_llm_request("continue");
    let invocation = assembler
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| {
            panic!(
                "assemble inline LLM branches: {error} {:?}",
                error
                    .cause()
                    .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code)
            )
        });
    let (members, cards) = run_deciding(invocation, &|card| {
        let key = card["interrupt_key"].as_str().expect("card key");
        match (card["fanout_v1"]["member"].as_str(), card["kind"].as_str()) {
            (Some("first"), Some("ask_user")) => ledger.decide(key, "answer", Some("Staging")),
            (Some("second"), Some("tool_guard")) => ledger.decide(key, "approve", None),
            (Some("third"), Some("tool_guard")) => ledger.decide(key, "reject", None),
            other => panic!("unexpected card {other:?}"),
        }
    })
    .await
    .expect("the answered and approved branches resume and the pipeline completes");
    let kinds = cards
        .iter()
        .map(|card| {
            format!(
                "{}:{}",
                card["fanout_v1"]["member"].as_str().unwrap_or_default(),
                card["kind"].as_str().unwrap_or_default()
            )
        })
        .collect::<Vec<_>>();
    assert_eq!(
        kinds,
        ["first:ask_user", "second:tool_guard", "third:tool_guard"]
    );
    for card in &cards {
        // The member tier of an LLM branch is named by its node (§3.3).
        let tier = &card["parent_agent_path"][0];
        assert!(["ask", "guard", "deny"].contains(&tier["name"].as_str().unwrap_or_default()));
    }
    assert_eq!(
        tool_calls.load(Ordering::Acquire),
        1,
        "only the approved tool runs, once"
    );
    assert_eq!(model_requests(&captured), 6);
    // Browser finding 2026-10-28: an inline branch's model request carries its
    // own system instruction and offers its own bound tool.
    for request in captured.lock().expect("captured model requests").iter() {
        let body: Value = serde_json::from_slice(&request.body).expect("model request JSON");
        let rendered = body.to_string();
        let branch = ["ask", "guard", "deny"]
            .into_iter()
            .find(|branch| rendered.contains(&format!("Work on {branch}")))
            .unwrap_or_else(|| panic!("an unknown branch request: {rendered}"));
        assert_eq!(
            body["messages"][0]["content"],
            format!("INLINE-BRANCH-SYSTEM {branch}"),
            "{rendered}"
        );
        let tool = if branch == "ask" {
            "ask_user"
        } else {
            "lookup_release"
        };
        assert!(
            body["tools"]
                .as_array()
                .is_some_and(
                    |tools| tools.iter().any(|offered| offered["function"]["name"]
                        .as_str()
                        .is_some_and(|name| name.contains(tool)))
                ),
            "{branch} is not offered {tool}: {rendered}"
        );
    }
    assert!(members.iter().any(|member| member["status"] == "paused"));
    let entries = joined(&db.pool).await.expect("joined entries");
    assert_eq!(entries.len(), 3, "{entries:?}");
    for entry in &entries {
        assert_eq!(entry["status"], "succeeded", "{entries:?}");
    }
    db.pool.close().await;
}

/// A saved pipeline participant whose LLM node asks the user.
fn asking_pipeline() -> Value {
    json!({
        "agent_type": "pipeline",
        "instructions": "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: answer\nnodes:\n  - id: answer\n    type: llm\n    input_mapping:\n      task: {type: variable, value: input}\n    input: [messages]\n    output: [answer, messages]\n    tool_names:\n      ask_user: [\"ask_user\"]\n    transition: END\n",
        "meta": {"internal_tools": ["ask_user"]}, "variables": [], "tools": [],
        "llm_settings": null
    })
}

/// Commit 6: three saved-pipeline branches each ask the user. Every branch
/// raises one `ask_user` card frame under its own member tier, nothing raises a
/// legacy card, and with nothing decided the activation parks.
#[tokio::test(flavor = "current_thread")]
async fn postgres_fanout_pipeline_branches_that_ask_raise_one_card_each_and_park() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let (assembler, captured) = assembler(
        &db.pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        }),
        asking_pipeline(),
    );
    let request = request_for("continue", "pipeline");
    let invocation = assembler
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| {
            panic!(
                "assemble the fan-out pipeline: {error} {:?}",
                error
                    .cause()
                    .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code)
            )
        });
    let (members, cards, keys) = run_keyed(invocation).await.expect("the activation parks");
    assert_eq!(model_requests(&captured), 3);
    // One member, one key: its call, its descendants, its card and its
    // lifecycle all carry the member tier (`fo1_`, its ordinal), so the Web
    // renders one labelled accordion per member (browser finding 2026-10-27).
    let tiers = cards
        .iter()
        .map(|card| {
            card["parent_agent_path"][0]["call_id"]
                .as_str()
                .unwrap_or_default()
                .to_owned()
        })
        .collect::<Vec<_>>();
    assert_eq!(tiers.len(), 3);
    for (key, ordinal) in &keys {
        let position = tiers
            .iter()
            .position(|tier| tier == key)
            .unwrap_or_else(|| panic!("a frame keyed outside the member tiers: {key}"));
        if !ordinal.is_null() {
            assert_eq!(ordinal, &json!(position + 1), "{key}");
        }
    }
    assert!(keys.len() > 9, "{keys:?}");
    assert_eq!(cards.len(), 3, "{cards:?}");
    for (card, (ordinal, branch)) in cards
        .iter()
        .zip([(1, "first"), (2, "second"), (3, "third")])
    {
        assert_eq!(card["kind"], "ask_user");
        assert_eq!(card["available_actions"], json!(["answer"]));
        assert_eq!(card["display"]["guardrail_type"], "clarifying_question");
        assert_eq!(card["fanout_v1"]["member"], branch);
        assert_eq!(card["fanout_v1"]["ordinal"], ordinal);
        assert_eq!(card["parent_agent_path"][0]["sibling_ordinal"], ordinal);
    }
    let statuses = members
        .iter()
        .map(|member| member["status"].as_str().unwrap_or_default().to_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        statuses,
        ["start", "paused", "start", "paused", "start", "paused"]
    );
    db.pool.close().await;
}

/// A saved pipeline that answers without a model.
fn answering_pipeline() -> Value {
    json!({
        "agent_type": "pipeline",
        "instructions": "state:\n  input: str\n  messages: list\n  answer: str\nentry_point: answer\nnodes:\n  - id: answer\n    type: state_modifier\n    template: 'answered'\n    input: [input]\n    output: [answer]\n    transition: END\n",
        "meta": {}, "variables": [], "tools": [], "llm_settings": null
    })
}

/// A saved pipeline whose direct MCP node calls a tool with external effects.
/// A saved child has no fenced node writer, so the node is refused at binding.
fn direct_effect_pipeline() -> Value {
    let mut pipeline = effectful_agent();
    pipeline["agent_type"] = json!("pipeline");
    pipeline["instructions"] = json!(
        "state:\n  input: str\n  messages: list\n  answer: dict\nentry_point: rev\nnodes:\n  - id: rev\n    type: mcp\n    toolkit_name: release intelligence\n    tool: lookup_release\n    input_mapping: {}\n    output: [answer]\n    transition: END\n"
    );
    pipeline
}

/// Branches `first` and `third` answer; `second` runs `direct_effect_pipeline`.
fn mixed_request(on_error: &str) -> super::super::request::AgentExecutionRequest {
    let mut request = request_for(on_error, "pipeline");
    let yaml = pipeline_yaml(on_error).replacen(
        "  - id: second_call\n    type: agent\n    tool: release-agent\n",
        "  - id: second_call\n    type: agent\n    tool: direct-flow\n",
        1,
    );
    assert!(yaml.contains("tool: direct-flow"), "{yaml}");
    let version = &mut request.payload.application["version_details"];
    version["instructions"] = json!(yaml);
    let mut second = version["tools"][0].clone();
    second["id"] = json!(45);
    second["name"] = json!("direct-flow");
    second["toolkit_name"] = json!("direct-flow");
    second["settings"] = json!({"application_id": 5, "application_version_id": 6});
    version["tools"].as_array_mut().expect("tools").push(second);
    request
}

/// A member whose direct tool cannot run fails its own branch: under On Error
/// `continue` the run completes and joins it as a failed entry; it never ends
/// the root run with the member's own failure signal (browser finding,
/// 2026-10-27). Under `stop` the Parallel node fails the run.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_member_whose_tool_cannot_run_fails_its_branch_not_the_run() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    for on_error in ["continue", "stop"] {
        let db = IsolatedPostgres::create(&url).await;
        install_schema(&db.pool).await;
        sqlx::raw_sql(CHECKPOINT_MIGRATION)
            .execute(&db.pool)
            .await
            .expect("apply checkpoint migration");
        let tool_calls = Arc::new(AtomicUsize::new(0));
        let (platform, model_facade, captured) = saved_runtime_with(
            Vec::new(),
            answering_pipeline(),
            Some(direct_effect_pipeline()),
        );
        let assembler = PipelineNativeAgentAssembler::postgres(
            db.pool.clone(),
            SessionLimits::default(),
            CheckpointLimits::default(),
            Arc::new(
                ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new())
                    .expect("empty tool policy"),
            ),
            platform,
            model_facade,
        )
        .with_mcp_connector(Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::clone(&tool_calls),
            read_only: false,
        }))
        .with_fixed_parallel_for_tests();
        let request = mixed_request(on_error);
        let invocation = assembler
            .assemble(claim(&request, 1))
            .await
            .unwrap_or_else(|error| panic!("assemble the mixed fan-out: {error}"));
        let outcome = run_to_end(invocation).await;
        assert_eq!(tool_calls.load(Ordering::Acquire), 0, "{on_error}");
        assert_eq!(model_requests(&captured), 0, "{on_error}");
        if on_error == "continue" {
            let members = outcome.unwrap_or_else(|error| {
                panic!("continue: a member's failure ended the run: {error}")
            });
            assert!(
                members
                    .iter()
                    .any(|member| member["fanout_v1"]["member"] == "second"
                        && member["status"] == "failed"),
                "{members:?}"
            );
            let entries = joined(&db.pool).await.expect("joined entries");
            let statuses = entries
                .iter()
                .map(|entry| {
                    format!(
                        "{}:{}",
                        entry["branch_id"].as_str().unwrap_or_default(),
                        entry["status"].as_str().unwrap_or_default()
                    )
                })
                .collect::<Vec<_>>();
            assert_eq!(
                statuses,
                ["first:succeeded", "second:failed", "third:succeeded"]
            );
            // Typed and readable (FU-248), never the generic "failed".
            assert_eq!(
                entries[1]["error"]["code"],
                "graph.fanout.child_tool_unavailable"
            );
            assert!(
                entries[1]["error"]["message_safe"]
                    .as_str()
                    .is_some_and(|message| message.contains("cannot run yet")),
                "{entries:?}"
            );
        } else {
            let error = outcome.expect_err("stop: the failed branch fails the node");
            assert!(
                !error.contains("pipeline.tool_unavailable"),
                "stop: the member's own signal ended the run: {error}"
            );
        }
        db.pool.close().await;
    }
}

/// The wake of a parked fan-out of saved-pipeline branches (contract §8.1):
/// the parked fresh turn, an empty input and only the wake marker.
fn wake_request(on_error: &str) -> super::super::request::AgentExecutionRequest {
    wake_request_for(on_error, "pipeline")
}

/// [`wake_request`] for branches whose saved participant is `agent_type`.
fn wake_request_for(
    on_error: &str,
    agent_type: &str,
) -> super::super::request::AgentExecutionRequest {
    let mut request = request_for(on_error, agent_type);
    request.payload.user_input = super::super::request::UserInput::Text(String::new());
    request.payload.meta.insert(
        crate::agents::graph::fanout_wake::FANOUT_WAKE_META_KEY.to_owned(),
        json!({"schema": "elitea.pipeline.fanout-wake.v1",
            "response_message_id": "10000000-0000-4000-8000-000000000043",
            "decision_revision": 1,
            "parked_execution_id": EXECUTION,
            "parked_generation": GENERATION}),
    );
    request
}

/// FU-246: three saved Agent members ask the user and the activation parks;
/// one card is answered while parked; the wake (a new execution) resumes only
/// that member's Agent with the answer, the others park again with their
/// cards, and no legacy pause frame is raised.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_wake_resumes_the_answered_agent_branch_and_parks_the_rest() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let connector = || {
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        })
    };
    let (first, _) = assembler(
        &db.pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        connector(),
        asking_agent(),
    );
    let request = request("continue");
    let invocation = first
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| panic!("assemble the fan-out: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the activation parks");
    assert_eq!(cards.len(), 3, "{cards:?}");
    let third = cards[2]["interrupt_key"]
        .as_str()
        .expect("the third card's key")
        .to_owned();
    ledger.decide(&third, "answer", Some("Staging"));

    let (woken, captured) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "third done",
        ))],
        connector(),
        asking_agent(),
    );
    let wake = wake_request_for("continue", "agent");
    let invocation = woken
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(claim(&wake, 2))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("the wake resumes the answered member and parks again");
    assert_eq!(
        model_requests(&captured),
        1,
        "only the answered member calls the model"
    );
    let resumed = String::from_utf8_lossy(&captured.lock().expect("requests")[0].body).into_owned();
    assert!(resumed.contains("Staging"), "{resumed}");
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "third" && member["status"] == "end"),
        "{members:?}"
    );
    assert!(
        cards
            .iter()
            .filter(|card| card["interrupt_key"] != third.as_str())
            .count()
            >= 2,
        "{cards:?}"
    );
    db.pool.close().await;
}

/// Browser finding 2026-10-27: saved-pipeline branches that ask the user park,
/// one card is answered, and the wake resumes exactly that child from its own
/// pause, while its unanswered siblings park again with their cards.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_wake_resumes_the_answered_pipeline_branch_and_parks_the_rest() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let connector = || {
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::new(AtomicUsize::new(0)),
            read_only: false,
        })
    };
    let (first, _) = assembler(
        &db.pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        connector(),
        asking_pipeline(),
    );
    let request = request_for("continue", "pipeline");
    let invocation = first
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| panic!("assemble the fan-out: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the activation parks");
    assert_eq!(cards.len(), 3, "{cards:?}");
    let third = cards[2]["interrupt_key"]
        .as_str()
        .expect("the third card's key")
        .to_owned();
    ledger.decide(&third, "answer", Some("red"));

    let (woken, captured) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "the saddle is red",
        ))],
        connector(),
        asking_pipeline(),
    );
    let wake = wake_request("continue");
    let invocation = woken
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(claim(&wake, 2))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("the wake resumes the answered child and parks again");
    assert_eq!(
        model_requests(&captured),
        1,
        "only the answered child calls the model"
    );
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "third" && member["status"] == "end"),
        "{members:?}"
    );
    // The unanswered members are restored paused, unchanged: in the wake only
    // the answered member runs (its lifecycle is the only one emitted), and
    // their cards are re-announced on the restore, never decided.
    assert!(
        members
            .iter()
            .all(|status| status["fanout_v1"]["member"] == "third"),
        "{members:?}"
    );
    assert!(
        cards
            .iter()
            .filter(|card| card["interrupt_key"] != third.as_str())
            .count()
            >= 2,
        "{cards:?}"
    );
    db.pool.close().await;
}

/// FU-249: a saved child pipeline whose root is a fixed Parallel of three
/// inline LLM nodes that may ask the user.
fn parallel_child_pipeline() -> Value {
    let branch = |id: &str| {
        format!(
            "  - id: ask_{id}\n    type: llm\n    input_mapping:\n      task: {{type: fixed, value: 'Ask about {id}'}}\n    input: [messages]\n    output: [{id}]\n    tool_names:\n      ask_user: [\"ask_user\"]\n"
        )
    };
    json!({
        "agent_type": "pipeline",
        "instructions": format!(
            "state:\n  input: str\n  messages: list\n  a: str\n  b: str\n  c: str\n  joined: list\nentry_point: gather\nnodes:\n  - id: gather\n    type: parallel\n    branches: [{{id: a, node: ask_a}}, {{id: b, node: ask_b}}, {{id: c, node: ask_c}}]\n    max_concurrency: 1\n    wait: all\n    on_error: continue\n    output: [joined]\n    transition: END\n{}{}{}",
            branch("a"),
            branch("b"),
            branch("c"),
        ),
        "meta": {"internal_tools": ["ask_user"]}, "variables": [], "tools": [],
        "llm_settings": null
    })
}

/// FU-249: the root pipeline runs the saved child through its `delegate` Agent node.
fn child_parallel_request() -> super::super::request::AgentExecutionRequest {
    agent_pipeline_request("release-agent", "pipeline")
}

/// [`child_parallel_request`] as the wake of the parked run.
fn child_parallel_wake() -> super::super::request::AgentExecutionRequest {
    let mut request = child_parallel_request();
    request.payload.user_input = super::super::request::UserInput::Text(String::new());
    request.payload.meta.insert(
        crate::agents::graph::fanout_wake::FANOUT_WAKE_META_KEY.to_owned(),
        json!({"schema": "elitea.pipeline.fanout-wake.v1",
            "response_message_id": "10000000-0000-4000-8000-000000000043",
            "decision_revision": 1,
            "parked_execution_id": EXECUTION,
            "parked_generation": GENERATION}),
    );
    request
}

/// The child Parallel's rows on the child thread (`<root>/delegate`), oldest
/// first: whether each carries the Agent node's activation stamp.
async fn child_parallel_rows(pool: &sqlx::PgPool) -> Vec<bool> {
    sqlx::query_scalar::<_, bool>(
        "SELECT metadata::text LIKE '%elitea.pipeline.application-activation.v1%' \
         FROM elitea_runtime.agent_graph_checkpoints \
         WHERE thread_id LIKE '%/delegate' ORDER BY save_ordinal",
    )
    .fetch_all(pool)
    .await
    .expect("read the child thread rows")
}

/// The latest child-thread state's `joined` entries.
async fn child_joined(pool: &sqlx::PgPool) -> Option<Vec<Value>> {
    let state = sqlx::query_scalar::<_, String>(
        "SELECT state FROM elitea_runtime.agent_graph_checkpoints \
         WHERE thread_id LIKE '%/delegate' AND state LIKE '%\"joined\"%' \
         ORDER BY save_ordinal DESC LIMIT 1",
    )
    .fetch_optional(pool)
    .await
    .expect("read the joined child checkpoint")?;
    let state: Value = serde_json::from_str(&state).expect("child state JSON");
    state["joined"].as_array().cloned()
}

/// FU-249: a fixed Parallel inside a saved child pipeline (root pipeline →
/// Agent node → saved pipeline). Its three branches ask, raise one card each
/// and the run parks; every row the child's Parallel wrote on the child thread
/// carries the Agent node's activation stamp. All three are answered while
/// parked; the wake (a new execution) re-enters the child and the Parallel
/// resumes each branch with its answer, joins, and the root completes.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_parallel_in_a_saved_child_pipeline_parks_and_a_wake_completes_it() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let (first, captured) = assembler(
        &db.pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        pipeline_connector(),
        parallel_child_pipeline(),
    );
    let request = child_parallel_request();
    let invocation = first
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| {
            panic!(
                "assemble the child fan-out: {error} {:?}",
                error
                    .cause()
                    .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code)
            )
        });
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the child's Parallel parks the run");
    assert_eq!(model_requests(&captured), 3);
    assert_eq!(cards.len(), 3, "{cards:?}");
    for (card, branch) in cards.iter().zip(["a", "b", "c"]) {
        assert_eq!(card["kind"], "ask_user");
        assert_eq!(card["fanout_v1"]["member"], branch);
    }
    let rows = child_parallel_rows(&db.pool).await;
    assert!(!rows.is_empty());
    assert!(rows.iter().all(|stamped| *stamped), "{rows:?}");
    for (card, answer) in cards.iter().zip(["red", "green", "blue"]) {
        ledger.decide(
            card["interrupt_key"].as_str().expect("a card key"),
            "answer",
            Some(answer),
        );
    }

    let (woken, captured) = assembler(
        &db.pool,
        ["the saddle is red", "the bridle is green", "the stirrup is blue"]
            .into_iter()
            .map(|text| TestModelGatewayOutcome::Response(pipeline_text_response(text)))
            .collect(),
        pipeline_connector(),
        parallel_child_pipeline(),
    );
    let wake = child_parallel_wake();
    let invocation = woken
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(claim(&wake, 2))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the wake completes the child's Parallel and the root");
    assert!(cards.is_empty(), "no card is raised again: {cards:?}");
    assert_eq!(model_requests(&captured), 3, "each answered branch calls the model once");
    let joined = child_joined(&db.pool).await.expect("the child's join");
    assert_eq!(joined.len(), 3, "{joined:?}");
    let rows = child_parallel_rows(&db.pool).await;
    assert!(rows.iter().all(|stamped| *stamped), "{rows:?}");
    db.pool.close().await;
}

fn pipeline_connector() -> Arc<PipelineMcpConnector> {
    Arc::new(PipelineMcpConnector {
        connections: Arc::new(AtomicUsize::new(0)),
        tool_calls: Arc::new(AtomicUsize::new(0)),
        read_only: false,
    })
}

/// Three saved-pipeline branches ask and park; the third card is answered.
async fn parked_with_the_third_answered(
    pool: &sqlx::PgPool,
    ledger: &Arc<crate::agents::graph::fanout_test_ledger::TestLedger>,
) -> String {
    parked_with_the_third_answered_for(pool, ledger, "pipeline", asking_pipeline()).await
}

/// [`parked_with_the_third_answered`] for saved participants of `agent_type`.
async fn parked_with_the_third_answered_for(
    pool: &sqlx::PgPool,
    ledger: &Arc<crate::agents::graph::fanout_test_ledger::TestLedger>,
    agent_type: &str,
    details: Value,
) -> String {
    let (first, _) = assembler(
        pool,
        (0..3)
            .map(|_| TestModelGatewayOutcome::Response(pipeline_ask_user_call_response()))
            .collect(),
        pipeline_connector(),
        details,
    );
    let request = request_for("continue", agent_type);
    let invocation = first
        .with_fanout_decisions_for_tests(ledger.client(1))
        .assemble(claim(&request, 1))
        .await
        .unwrap_or_else(|error| panic!("assemble the fan-out: {error}"));
    let (_, cards) = run_collecting(invocation)
        .await
        .expect("the activation parks");
    let third = cards[2]["interrupt_key"]
        .as_str()
        .expect("the third card's key")
        .to_owned();
    ledger.decide(&third, "answer", Some("red"));
    third
}

/// Browser finding 2026-10-27: a wake whose process is lost before its first
/// root save is reclaimed through the production checkpoint recovery (inspect,
/// authorize, assemble the checkpoint) under its own execution. Its root still
/// holds the park an earlier execution wrote; it runs again as the wake (I),
/// never ending as an internal failure with the answer left undelivered.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_wake_lost_before_its_first_root_save_recovers_as_the_wake() {
    const WAKE_EXECUTION: &str = "execution-fanout-wake";
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let third = parked_with_the_third_answered(&db.pool, &ledger).await;

    // The wake's first claim was lost before it saved its root: its reclaim
    // inspects the root, which still holds the park of the first execution.
    let (woken, captured) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "the saddle is red",
        ))],
        pipeline_connector(),
        asking_pipeline(),
    );
    let woken = woken.with_fanout_decisions_for_tests(ledger.client(2));
    let wake = wake_request("continue");
    let invocation = recovered(&woken, &wake, WAKE_EXECUTION, 2)
        .await
        .unwrap_or_else(|error| panic!("the lost wake recovers from its park: {error}"));
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("the recovered wake resumes the answered child and parks again");
    assert_eq!(model_requests(&captured), 1, "only the answered child runs");
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "third" && member["status"] == "end"),
        "{members:?}"
    );
    assert!(
        cards
            .iter()
            .filter(|card| card["interrupt_key"] != third.as_str())
            .count()
            >= 2,
        "{cards:?}"
    );
    db.pool.close().await;
}

/// FU-246: the wake applied the answer to a saved Agent member's card, and the
/// process was lost while that member's resumed Agent was in its model call.
/// The reclaim (production recovery under the wake's own execution) resumes
/// that Agent with the answer: the answer is never asked again, the answered
/// member never raises a new card, and the unanswered members park again.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_wake_lost_mid_model_resumes_the_answered_agent_branch() {
    const WAKE_EXECUTION: &str = "execution-fanout-wake";
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let third =
        parked_with_the_third_answered_for(&db.pool, &ledger, "agent", asking_agent()).await;

    let (lost, lost_requests) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Pending],
        pipeline_connector(),
        asking_agent(),
    );
    let wake = wake_request_for("continue", "agent");
    let invocation = lost
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(AuthorizedNativeAssembly::from_authorized(
            &wake,
            test_runtime_context_authority(),
            test_session_authority_for_reclaim(WAKE_EXECUTION, GENERATION, 2),
            Arc::new(TestStateWriterLease::current()),
            AuthorizedNativeCommandBinding::fixture_for_execution(WAKE_EXECUTION),
        ))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    let outcome =
        tokio::time::timeout(Duration::from_millis(800), run_collecting(invocation)).await;
    assert!(
        outcome.is_err(),
        "the answered member's Agent waits in its model call: {outcome:?}"
    );
    assert_eq!(model_requests(&lost_requests), 1);

    let (woken, captured) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "third done",
        ))],
        pipeline_connector(),
        asking_agent(),
    );
    let woken = woken.with_fanout_decisions_for_tests(ledger.client(3));
    let invocation = recovered(&woken, &wake, WAKE_EXECUTION, 3)
        .await
        .unwrap_or_else(|error| panic!("the lost wake recovers: {error}"));
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("the recovered wake finishes the answered member and parks again");
    assert_eq!(
        model_requests(&captured),
        1,
        "only the answered member's step: {members:?}"
    );
    let resumed = String::from_utf8_lossy(&captured.lock().expect("requests")[0].body).into_owned();
    assert!(resumed.contains("red"), "{resumed}");
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "third" && member["status"] == "end"),
        "{members:?}"
    );
    let keys = cards
        .iter()
        .filter_map(|card| card["interrupt_key"].as_str())
        .collect::<std::collections::BTreeSet<_>>();
    assert_eq!(keys.len(), 2, "{cards:?}");
    assert!(!keys.contains(third.as_str()), "{cards:?}");
    db.pool.close().await;
}

/// Browser finding 2026-10-27: the wake applied the answer, its child was in
/// its model call when the process was lost. The reclaim (production recovery
/// under the wake's own execution) resumes that model step with the answer
/// (R): the answer is never asked again and the ledger is never re-read for it.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_wake_lost_mid_model_resumes_the_answered_child() {
    const WAKE_EXECUTION: &str = "execution-fanout-wake";
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let ledger = crate::agents::graph::fanout_test_ledger::TestLedger::new();
    let third = parked_with_the_third_answered(&db.pool, &ledger).await;

    // The wake's first claim applies the answer, then its process is lost
    // while the answered child's model call is in flight.
    let (lost, lost_requests) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Pending],
        pipeline_connector(),
        asking_pipeline(),
    );
    let wake = wake_request("continue");
    let invocation = lost
        .with_fanout_decisions_for_tests(ledger.client(2))
        .assemble(AuthorizedNativeAssembly::from_authorized(
            &wake,
            test_runtime_context_authority(),
            // Claim ordinals stay unique across both executions, as claim ids are.
            test_session_authority_for_reclaim(WAKE_EXECUTION, GENERATION, 2),
            Arc::new(TestStateWriterLease::current()),
            AuthorizedNativeCommandBinding::fixture_for_execution(WAKE_EXECUTION),
        ))
        .await
        .unwrap_or_else(|error| panic!("assemble the wake: {error}"));
    // Dropped inside the gateway's 1 s header timeout: the process is gone.
    let outcome =
        tokio::time::timeout(Duration::from_millis(800), run_collecting(invocation)).await;
    assert!(
        outcome.is_err(),
        "the answered child waits in its model call: {outcome:?}"
    );
    assert_eq!(
        model_requests(&lost_requests),
        1,
        "the answered child's step was in flight"
    );

    let (woken, captured) = assembler(
        &db.pool,
        vec![TestModelGatewayOutcome::Response(pipeline_text_response(
            "the saddle is red",
        ))],
        pipeline_connector(),
        asking_pipeline(),
    );
    let woken = woken.with_fanout_decisions_for_tests(ledger.client(3));
    let invocation = recovered(&woken, &wake, WAKE_EXECUTION, 3)
        .await
        .unwrap_or_else(|error| panic!("the lost wake recovers: {error}"));
    let (members, cards) = run_collecting(invocation)
        .await
        .expect("the recovered wake finishes the answered child and parks again");
    assert_eq!(
        model_requests(&captured),
        1,
        "only the answered child's step: {members:?} {:?}",
        cards
            .iter()
            .map(|card| (
                card["interrupt_key"].clone(),
                card["display"]["message"].clone()
            ))
            .collect::<Vec<_>>()
    );
    assert!(
        members
            .iter()
            .any(|member| member["fanout_v1"]["member"] == "third" && member["status"] == "end"),
        "{members:?}"
    );
    // The answered member never asks again: only the two unanswered members'
    // cards are (re-)announced, never a new card of the answered one.
    let keys = cards
        .iter()
        .filter_map(|card| card["interrupt_key"].as_str())
        .collect::<std::collections::BTreeSet<_>>();
    assert_eq!(keys.len(), 2, "{cards:?}");
    assert!(!keys.contains(third.as_str()), "{cards:?}");
    db.pool.close().await;
}

/// R: `second` dies in its model call and resumes from its model checkpoint.
#[tokio::test(flavor = "current_thread")]
async fn postgres_fanout_child_caught_mid_model_resumes_from_its_checkpoint() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario(&url, "mid_model", "stop").await;
}

/// I: `second` dies in a read-only tool, which runs again.
#[tokio::test(flavor = "current_thread")]
async fn postgres_fanout_child_read_only_tool_in_flight_runs_again() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario(&url, "read_only", "stop").await;
}

/// T-2A D1, the D2 classes for an inline LLM branch: mid-model R.
#[tokio::test(flavor = "current_thread")]
async fn postgres_inline_llm_branch_caught_mid_model_resumes_from_its_checkpoint() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario_for(&url, "llm", "mid_model", "stop").await;
}

/// Inline LLM branch, read-only tool in flight: I.
#[tokio::test(flavor = "current_thread")]
async fn postgres_inline_llm_branch_read_only_tool_in_flight_runs_again() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario_for(&url, "llm", "read_only", "stop").await;
}

/// Inline LLM branch, effectful tool in flight without a receipt: the
/// branch fails `graph.fanout.may_have_started` and the tool never runs
/// again; On Error decides the node.
#[tokio::test(flavor = "current_thread")]
async fn postgres_inline_llm_branch_effect_in_flight_may_have_started() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario_for(&url, "llm", "effect", "continue").await;
    scenario_for(&url, "llm", "effect", "stop").await;
}

#[tokio::test(flavor = "current_thread")]
#[allow(clippy::too_many_lines)] // One child process per phase: crash, then recover with its assertions.
async fn postgres_fanout_effect_child() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let on_error = std::env::var(ON_ERROR_ENV).expect("on_error");
    let mode = std::env::var(MODE_ENV).expect("mode");
    let pool = connect(&database).await;
    let request = match std::env::var(KIND_ENV).expect("kind").as_str() {
        "llm" => inline_request(&on_error),
        _ => request(&on_error),
    };
    match std::env::var(PHASE_ENV).expect("phase").as_str() {
        "crash" => {
            let second = if mode == "mid_model" {
                TestModelGatewayOutcome::ExitProcess(CRASH_EXIT_CODE)
            } else {
                TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response())
            };
            let (assembler, _) = assembler(
                &pool,
                vec![
                    TestModelGatewayOutcome::Response(pipeline_text_response("first done")),
                    second,
                ],
                Arc::new(CrashingMcpConnector {
                    pool: pool.clone(),
                    read_only: mode == "read_only",
                }),
                effectful_agent(),
            );
            let invocation = assembler
                .assemble(claim(&request, 1))
                .await
                .unwrap_or_else(|error| {
                    panic!(
                        "crash: assemble the fan-out pipeline: {error} {:?}",
                        error.cause().map(
                            elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code
                        )
                    )
                });
            // The tool or the model call exits this process; reaching the end is the failure.
            let outcome = run_to_end(invocation).await;
            panic!("crash: the process outlived its crash point: {outcome:?}");
        }
        "recover" if mode != "effect" => {
            let tool_calls = Arc::new(AtomicUsize::new(0));
            let (assembler, captured) = assembler(
                &pool,
                vec![
                    TestModelGatewayOutcome::Response(pipeline_text_response("second done")),
                    TestModelGatewayOutcome::Response(pipeline_text_response("third done")),
                ],
                Arc::new(PipelineMcpConnector {
                    connections: Arc::new(AtomicUsize::new(0)),
                    tool_calls: Arc::clone(&tool_calls),
                    // The same tool declarations as the crashed process offered.
                    read_only: mode == "read_only",
                }),
                effectful_agent(),
            );
            // The production reclaim: model-checkpoint recovery (profile ON).
            let invocation = recovered(&assembler, &request, EXECUTION, 2)
                .await
                .expect("recover: the replacement Worker recovers the reclaim");
            let members = run_to_end(invocation)
                .await
                .expect("recover: the resumed child completes the pipeline");
            // `first` is served from its receipt; `second` and `third` raise
            // their lifecycle in order, under their frozen member identity.
            let statuses = members
                .iter()
                .map(|member| {
                    let tier = &member["parent_agent_path"][0];
                    assert_eq!(tier["sibling_ordinal"], member["fanout_v1"]["ordinal"]);
                    assert!(
                        tier["call_id"]
                            .as_str()
                            .is_some_and(|id| id.starts_with("fo1_"))
                    );
                    assert_eq!(member["fanout_v1"]["total"], 3);
                    (
                        member["fanout_v1"]["member"]
                            .as_str()
                            .unwrap_or_default()
                            .to_owned(),
                        member["status"].as_str().unwrap_or_default().to_owned(),
                    )
                })
                .collect::<Vec<_>>();
            assert_eq!(
                statuses,
                [
                    ("second", "start"),
                    ("second", "end"),
                    ("third", "start"),
                    ("third", "end")
                ]
                .map(|(member, status)| (member.to_owned(), status.to_owned()))
            );
            // R resumes the saved model request; I replays the saved call
            // batch without a model call and runs the read-only tool again.
            assert_eq!(
                tool_calls.load(Ordering::Acquire),
                usize::from(mode == "read_only")
            );
            assert_eq!(
                model_requests(&captured),
                2,
                "recover: `second` once (after its tool or its saved request) and `third`"
            );
            let entries = joined(&pool).await.expect("joined entries");
            assert_eq!(entries.len(), 3, "{entries:?}");
            for (entry, id) in entries.iter().zip(["first", "second", "third"]) {
                assert_eq!(entry["branch_id"], id);
                assert_eq!(entry["status"], "succeeded", "{entries:?}");
            }
            assert_eq!(
                tool_boundaries(&pool, "claim-2").await,
                Vec::<String>::new()
            );
        }
        "recover" => {
            let tool_calls = Arc::new(AtomicUsize::new(0));
            let (assembler, captured) = assembler(
                &pool,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "third done",
                ))],
                Arc::new(PipelineMcpConnector {
                    connections: Arc::new(AtomicUsize::new(0)),
                    tool_calls: Arc::clone(&tool_calls),
                    read_only: false,
                }),
                effectful_agent(),
            );
            let invocation = recovered(&assembler, &request, EXECUTION, 2)
                .await
                .unwrap_or_else(|error| {
                    panic!("recover: the replacement Worker recovers the reclaim: {error}")
                });
            let outcome = run_to_end(invocation).await;
            assert_eq!(
                tool_calls.load(Ordering::Acquire),
                0,
                "recover: the effectful tool is never dispatched again"
            );
            if on_error == "continue" {
                let members = outcome.expect("recover: partial results complete the pipeline");
                // The re-run `second` ends failed as data; `third` runs.
                let statuses = members
                    .iter()
                    .map(|member| {
                        format!(
                            "{}:{}",
                            member["fanout_v1"]["member"].as_str().unwrap_or_default(),
                            member["status"].as_str().unwrap_or_default()
                        )
                    })
                    .collect::<Vec<_>>();
                assert_eq!(
                    statuses,
                    ["second:start", "second:failed", "third:start", "third:end"]
                );
                assert_eq!(
                    model_requests(&captured),
                    1,
                    "recover: only `third` calls the model"
                );
                let entries = joined(&pool).await.expect("joined entries");
                assert_eq!(entries.len(), 3, "{entries:?}");
                assert_eq!(entries[0]["branch_id"], "first");
                assert_eq!(entries[0]["status"], "succeeded");
                assert_eq!(entries[1]["branch_id"], "second");
                assert_eq!(entries[1]["status"], "failed");
                assert_eq!(entries[1]["error"]["code"], MAY_HAVE_STARTED);
                assert_eq!(entries[2]["branch_id"], "third");
                assert_eq!(entries[2]["status"], "succeeded");
            } else {
                let error = outcome.expect_err("recover: On Error stop fails the node");
                // The run ends with the registered "may have started, not
                // repeated" reason, as an unscoped Agent node's approved call.
                assert!(
                    error.contains(crate::protocol::output::RECOVERY_REFUSED_UPSTREAM_CODE),
                    "recover: a typed fan-out failure: {error}"
                );
                assert_eq!(
                    model_requests(&captured),
                    0,
                    "recover: nothing calls the model"
                );
            }
            assert!(
                tool_boundaries(&pool, "claim-2").await.is_empty(),
                "recover: no child crossed a tool boundary again"
            );
        }
        other => panic!("unknown phase {other}"),
    }
    pool.close().await;
}
