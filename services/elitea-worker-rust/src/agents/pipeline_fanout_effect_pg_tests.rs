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
const EXECUTION: &str = "execution-fanout-effect";
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
    let mut request = agent_pipeline_request("release-agent", "agent");
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

/// Child model-scope sessions (`elitea-model-*`) holding a durable
/// `tool_may_have_started` boundary written by `claim`.
async fn tool_boundaries(pool: &sqlx::PgPool, claim: &str) -> Vec<String> {
    sqlx::query_scalar::<_, String>(
        "SELECT DISTINCT session_id FROM elitea_runtime.agent_session_events \
         WHERE session_id LIKE 'elitea-model-%' \
           AND event_payload LIKE '%tool_may_have_started%' \
           AND writer_claim_id = $1 AND writer_execution_id = $2 AND writer_generation = $3",
    )
    .bind(claim)
    .bind(EXECUTION)
    .bind(i64::try_from(GENERATION).expect("generation"))
    .fetch_all(pool)
    .await
    .expect("read child tool boundaries")
}

/// The effectful tool of the crash process. Inside its own execution it
/// proves the boundary is already durable, then dies with the call in flight.
struct CrashingMcpConnector {
    pool: sqlx::PgPool,
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
                    read_only: false,
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
        false
    }
    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        let boundaries = tool_boundaries(&self.pool, "claim-1").await;
        std::process::exit(if boundaries.len() == 1 {
            CRASH_EXIT_CODE
        } else {
            MARKER_MISSING_EXIT_CODE
        })
    }
}

/// The platform answers by request path, not call order: each scoped Agent
/// node materializes its own participant (same shape as the 2a-1 fixture).
struct SavedAgentVersions;

#[tonic::async_trait]
impl crate::transport::input_content::content_fake::ContextRoutes for SavedAgentVersions {
    async fn answer(
        &self,
        call: crate::transport::input_content::content_fake::ContextCall,
    ) -> Result<crate::transport::input_content::content_fake::ContextAnswer, tonic::Status> {
        let value = if call.route == "elitea-client-token" {
            json!({"schema_version":"elitea.runtime.elitea-client-token.v1","project_id":17,
                "token":"ephemeral-fanout-token"})
        } else if call.route.ends_with("/3/versions/4") {
            json!({"schema_version":"elitea.runtime.application-version.v1","project_id":17,
                "application_id":3,"version_id":4,"version_details":effectful_agent(),
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
) -> (Arc<PlatformClient>, Arc<ModelFacade>, CapturedModelRequests) {
    let runtime = RuntimeContextClient::with_rpc(
        crate::transport::input_content::content_fake::Routes(SavedAgentVersions),
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
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    let (platform, model_facade, captured) = saved_runtime(outcomes);
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

/// Run the reclaimed pipeline to its end: `Ok(())` on completion, the
/// failure's code otherwise.
async fn run_to_end(
    mut invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> Result<(), String> {
    invocation
        .project_start(timestamp(1))
        .expect("pipeline browser start");
    let (mut run, _projector, completion) = invocation.start().expect("pipeline start");
    loop {
        match run.next_event().await {
            Err(error) => {
                return Err(format!(
                    "{error} {:?} {}",
                    error.upstream_code(),
                    error.diagnostic_detail()
                ));
            }
            Ok(Some(_)) => {}
            Ok(None) => break,
        }
    }
    completion
        .select()
        .await
        .map(|_| ())
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

async fn scenario(url: &str, on_error: &str) {
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
            // (a) the boundary of exactly the effectful child, fenced to claim 1.
            assert_eq!(tool_boundaries(&db.pool, "claim-1").await.len(), 1);
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
    scenario(&url, "continue").await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_effectful_fanout_child_may_have_started_stops_the_node() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the fan-out effect PostgreSQL proof");
        return;
    };
    scenario(&url, "stop").await;
}

#[tokio::test(flavor = "current_thread")]
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
    let pool = connect(&database).await;
    let request = request(&on_error);
    match std::env::var(PHASE_ENV).expect("phase").as_str() {
        "crash" => {
            let (assembler, _) = assembler(
                &pool,
                vec![
                    TestModelGatewayOutcome::Response(pipeline_text_response("first done")),
                    TestModelGatewayOutcome::Response(pipeline_mcp_tool_call_response()),
                ],
                Arc::new(CrashingMcpConnector { pool: pool.clone() }),
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
            // The effectful tool exits this process; reaching the end is the failure.
            let outcome = run_to_end(invocation).await;
            panic!("crash: the effectful tool never ran: {outcome:?}");
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
            );
            let invocation = assembler
                .assemble(claim(&request, 2))
                .await
                .unwrap_or_else(|error| {
                    panic!(
                        "recover: the replacement Worker assembles the reclaim: {error} {:?}",
                        error.cause().map(
                            elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code
                        )
                    )
                });
            let outcome = run_to_end(invocation).await;
            assert_eq!(
                tool_calls.load(Ordering::Acquire),
                0,
                "recover: the effectful tool is never dispatched again"
            );
            if on_error == "continue" {
                outcome.expect("recover: partial results complete the pipeline");
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
                assert!(
                    error.contains(MAY_HAVE_STARTED) || error.contains("graph.fanout"),
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
