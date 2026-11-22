//! FU-358: an effectful direct toolkit/MCP node inside a saved pipeline offered
//! as a tool of a non-member agent (both pipeline root -> Agent node ->
//! pipeline tool and ordinary chat agent -> pipeline tool) journals through a
//! claim-fenced node-recovery journal and never repeats an in-flight external
//! effect across Worker process replacement on real `PostgreSQL`.

use super::super::scoped_tests::{ordinary_agent_request, reference, run_ordinary_authorized};
use super::*;
use crate::agents::ordinary::OrdinaryNativeAgentAssembler;
use crate::protocol::control::test_session_authority_for_claim;
use crate::state::SessionLimits;

const CHILD_TEST: &str = "agents::pipeline_tests::fanout_effect_pg_tests::pipeline_tool_node_writer::postgres_fu358_kill_child";
const DATABASE_ENV: &str = "ELITEA_FU358_PG_DATABASE";
const SHAPE_ENV: &str = "ELITEA_FU358_PG_SHAPE";
const PHASE_ENV: &str = "ELITEA_FU358_PG_PHASE";
const OUTCOME_LINE: &str = "FU358_OUTCOME ";
const TOPIC: &str = "topic-fu358";

fn pipeline_root_request() -> super::super::super::request::AgentExecutionRequest {
    let mut request = agent_pipeline_request("release-agent", "agent");
    request.payload.application["version_details"]["instructions"] = json!(
        "state:\n  answer: str\n  messages: list\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: release-agent\n    input_mapping:\n      task: {type: fixed, value: 'topic-fu358'}\n    output: [answer, messages]\n    transition: END\n"
    );
    request
}

fn ordinary_chat_request() -> super::super::super::request::AgentExecutionRequest {
    ordinary_agent_request(vec![reference((5, 6), "pipeline", "helper-flow")])
}

fn ordinary_assembler(
    pool: &sqlx::PgPool,
    outcomes: Vec<TestModelGatewayOutcome>,
    connector: Arc<dyn McpConnector>,
) -> (OrdinaryNativeAgentAssembler, CapturedModelRequests) {
    let mut config = test_model_gateway_config();
    config.response_header_timeout = Duration::from_secs(60);
    let (platform, facade, captured) = saved_runtime_on(
        outcomes,
        pipeline_delegating_agent(),
        Some(direct_effect_pipeline()),
        None,
        config,
    );
    let assembler = OrdinaryNativeAgentAssembler::new(
        platform,
        facade,
        Arc::new(
            ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new())
                .expect("empty tool policy"),
        ),
    )
    .with_mcp_connector(connector)
    .with_postgres_sessions(pool.clone(), SessionLimits::default())
    .with_checkpoint_limits(CheckpointLimits::default());
    (assembler, captured)
}

fn root_pipeline_assembler(
    pool: &sqlx::PgPool,
    outcomes: Vec<TestModelGatewayOutcome>,
    connector: Arc<dyn McpConnector>,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    let mut config = test_model_gateway_config();
    config.response_header_timeout = Duration::from_secs(60);
    assembler_on(
        pool,
        saved_runtime_on(
            outcomes,
            pipeline_delegating_agent(),
            Some(direct_effect_pipeline()),
            None,
            config,
        ),
        connector,
    )
}

/// Counts external MCP dispatches in an isolated test table so the parent
/// process proves exact `1` call / `0` repeats across OS process replacement.
struct RecordingMcpConnector {
    pool: sqlx::PgPool,
    crash_in_flight: bool,
}

#[async_trait]
impl McpConnector for RecordingMcpConnector {
    async fn connect(
        &self,
        _config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn adk_rust::Toolset>, McpMaterializationError> {
        Ok(Arc::new(BasicToolset::new(
            "release intelligence",
            vec![Arc::new(RecordingMcpTool {
                pool: self.pool.clone(),
                crash_in_flight: self.crash_in_flight,
            })],
        )))
    }
}

struct RecordingMcpTool {
    pool: sqlx::PgPool,
    crash_in_flight: bool,
}

#[async_trait]
impl Tool for RecordingMcpTool {
    fn name(&self) -> &'static str {
        "lookup_release"
    }
    fn description(&self) -> &'static str {
        "Effectful MCP tool fixture for FU-358."
    }
    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({"type": "object", "properties": {}, "additionalProperties": true}))
    }
    fn is_read_only(&self) -> bool {
        false
    }
    async fn execute(&self, _: Arc<dyn ToolContext>, _: Value) -> adk_rust::Result<Value> {
        // Prove the fenced `n1:` journal persisted `Started` before the tool
        // ran, then record the external effect durably.
        let started_journals: i64 = sqlx::query_scalar(
            "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints \
             WHERE thread_id LIKE 'n1:%'",
        )
        .fetch_one(&self.pool)
        .await
        .expect("count started journals");
        if started_journals == 0 {
            std::process::exit(MARKER_MISSING_EXIT_CODE);
        }
        sqlx::query("INSERT INTO public.fu358_tool_calls (called_at) VALUES (clock_timestamp())")
            .execute(&self.pool)
            .await
            .expect("record external tool call");
        if self.crash_in_flight {
            std::process::exit(CRASH_EXIT_CODE);
        }
        Ok(json!({"status": "echoed-fu358"}))
    }
}

async fn install_call_counter(pool: &sqlx::PgPool) {
    sqlx::raw_sql(
        "CREATE TABLE IF NOT EXISTS public.fu358_tool_calls (\
            id SERIAL PRIMARY KEY, \
            called_at TIMESTAMPTZ NOT NULL\
        )",
    )
    .execute(pool)
    .await
    .expect("create fu358_tool_calls");
}

async fn recorded_tool_calls(pool: &sqlx::PgPool) -> i64 {
    sqlx::query_scalar("SELECT count(*) FROM public.fu358_tool_calls")
        .fetch_one(pool)
        .await
        .expect("count fu358_tool_calls")
}

/// Happy path: root pipeline -> Agent node -> pipeline tool -> effectful direct
/// MCP node runs once behind a claim-fenced `n1:` journal and completes.
#[tokio::test(flavor = "current_thread")]
async fn postgres_root_agent_node_runs_effectful_direct_pipeline_tool_once() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for FU-358 root pipeline PG proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    install_call_counter(&db.pool).await;

    let request = pipeline_root_request();
    let connector: Arc<dyn McpConnector> = Arc::new(RecordingMcpConnector {
        pool: db.pool.clone(),
        crash_in_flight: false,
    });
    let (assembler, captured) = root_pipeline_assembler(
        &db.pool,
        vec![
            TestModelGatewayOutcome::Response(nested_agent_call_response("call_root_1", TOPIC)),
            TestModelGatewayOutcome::Response(pipeline_text_response("fu358 pipeline done")),
        ],
        connector,
    );
    let invocation = assembler
        .assemble(claim(&request, 1))
        .await
        .expect("assemble root pipeline");
    run_to_end(invocation)
        .await
        .expect("root pipeline completes");
    let (state, pending): (String, String) = sqlx::query_as(
        "SELECT state, pending_nodes FROM elitea_runtime.agent_graph_checkpoints \
         WHERE thread_id NOT LIKE '%/%' AND thread_id NOT LIKE 'n1:%' \
         ORDER BY save_ordinal DESC LIMIT 1",
    )
    .fetch_one(&db.pool)
    .await
    .expect("completed root checkpoint");
    assert_eq!(pending, "[]", "root pipeline reached END: {state}");
    assert!(
        state.contains("fu358 pipeline done"),
        "expected completed answer in root state, got: {state}"
    );
    assert_eq!(recorded_tool_calls(&db.pool).await, 1);
    assert_eq!(model_requests(&captured), 2);
    assert_eq!(journal_threads(&db.pool).await.len(), 1);
    db.pool.close().await;
}

/// Happy path: ordinary chat agent -> pipeline tool -> effectful direct MCP
/// node runs once behind a session-fenced `n1:` journal and completes.
#[tokio::test(flavor = "current_thread")]
async fn postgres_ordinary_agent_runs_effectful_direct_pipeline_tool_once() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for FU-358 ordinary chat PG proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    install_call_counter(&db.pool).await;

    let request = ordinary_chat_request();
    let connector: Arc<dyn McpConnector> = Arc::new(RecordingMcpConnector {
        pool: db.pool.clone(),
        crash_in_flight: false,
    });
    let (assembler, captured) = ordinary_assembler(
        &db.pool,
        vec![
            TestModelGatewayOutcome::Response(nested_agent_call_response("call_ord_1", TOPIC)),
            TestModelGatewayOutcome::Response(pipeline_text_response("fu358 ordinary done")),
        ],
        connector,
    );
    let (browser, paused) = run_ordinary_authorized(&assembler, claim(&request, 1)).await;
    assert!(!paused, "ordinary turn must complete without pausing");
    let terminal = browser.last().expect("terminal browser frame");
    assert_eq!(terminal["type"], "full_message", "{browser:?}");
    assert!(
        terminal["content"]
            .as_str()
            .is_some_and(|text| text.contains("fu358 ordinary done")),
        "{terminal:?}"
    );
    assert_eq!(recorded_tool_calls(&db.pool).await, 1);
    assert_eq!(model_requests(&captured), 2);
    assert_eq!(journal_threads(&db.pool).await.len(), 1);
    db.pool.close().await;
}

async fn run_kill_in_flight(shape: &str) {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for FU-358 kill-in-flight PG proof ({shape})"
        );
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    install_call_counter(&db.pool).await;

    for (phase, expected) in [("crash", Some(CRASH_EXIT_CODE)), ("recover", Some(0))] {
        let mut command = std::process::Command::new(std::env::current_exe().expect("test binary"));
        command
            .args(["--exact", CHILD_TEST, "--nocapture", "--test-threads=1"])
            .env(DATABASE_ENV, &db.database_name)
            .env(SHAPE_ENV, shape)
            .env(PHASE_ENV, phase);
        let output = run_phase(phase, command);
        let stdout = String::from_utf8_lossy(&output.stdout);
        let stderr = String::from_utf8_lossy(&output.stderr);
        assert_eq!(
            output.status.code(),
            expected,
            "{shape}/{phase} exit: {stdout} {stderr}"
        );
        if phase == "crash" {
            // Exactly 1 external effect occurred, preceded by a durable `n1:`
            // Started journal row and a claim-1 `tool_may_have_started` marker.
            assert_eq!(
                recorded_tool_calls(&db.pool).await,
                1,
                "{shape}: 1 call before kill"
            );
            assert_eq!(
                journal_threads(&db.pool).await.len(),
                1,
                "{shape}: durable n1: journal row recorded Started before dispatch"
            );
            if shape == "pipeline" {
                assert_eq!(
                    tool_boundaries(&db.pool, "claim-1").await.len(),
                    1,
                    "{shape}: tool_may_have_started boundary durable under claim 1"
                );
            } else {
                let ordinary_boundaries: i64 = sqlx::query_scalar(
                    "SELECT count(*) FROM elitea_runtime.agent_session_events \
                     WHERE event_payload LIKE '%tool_may_have_started%' \
                       AND writer_claim_id = 'claim-1' \
                       AND writer_execution_id = $1 \
                       AND writer_generation = $2",
                )
                .bind(EXECUTION)
                .bind(i64::try_from(GENERATION).expect("generation"))
                .fetch_one(&db.pool)
                .await
                .expect("ordinary boundary count");
                assert_eq!(
                    ordinary_boundaries, 1,
                    "{shape}: ordinary tool_may_have_started boundary durable under claim 1"
                );
            }
            continue;
        }
        let outcome = stdout
            .lines()
            .find_map(|line| line.split_once(OUTCOME_LINE).map(|(_, value)| value))
            .unwrap_or_else(|| panic!("{shape}/recover reports outcome: {stdout}"));
        assert_eq!(
            recorded_tool_calls(&db.pool).await,
            1,
            "{shape}: 0 repeats after process replacement ({outcome})"
        );
        if shape == "pipeline" {
            assert_eq!(
                outcome, "RecoveryRefused RUNTIME_ERROR_CODE_V1_RECOVERY_REFUSED; model calls 0",
                "{stdout}"
            );
        } else {
            assert_eq!(outcome, "OrdinaryReclaimRefused; model calls 0", "{stdout}");
        }
    }
    db.pool.close().await;
}

/// Two-process real-PG kill-in-flight: pipeline root -> Agent node -> pipeline
/// tool -> effectful direct MCP node (`1` call, `0` repeats).
#[tokio::test(flavor = "current_thread")]
async fn postgres_root_agent_node_killed_inside_effectful_direct_pipeline_tool_never_repeats_it() {
    run_kill_in_flight("pipeline").await;
}

/// Two-process real-PG kill-in-flight: ordinary chat agent -> pipeline tool ->
/// effectful direct MCP node (`1` call, `0` repeats).
#[tokio::test(flavor = "current_thread")]
async fn postgres_ordinary_agent_killed_inside_effectful_direct_pipeline_tool_never_repeats_it() {
    run_kill_in_flight("ordinary").await;
}

async fn run_pipeline_kill_phase(pool: &sqlx::PgPool, phase: &str) {
    let request = pipeline_root_request();
    match phase {
        "crash" => {
            let connector: Arc<dyn McpConnector> = Arc::new(RecordingMcpConnector {
                pool: pool.clone(),
                crash_in_flight: true,
            });
            let (assembler, _) = root_pipeline_assembler(
                pool,
                vec![
                    TestModelGatewayOutcome::Response(nested_agent_call_response(
                        "call_kill_pipe",
                        TOPIC,
                    )),
                    TestModelGatewayOutcome::Response(pipeline_text_response("never reached")),
                ],
                connector,
            );
            let invocation = assembler
                .assemble(claim(&request, 1))
                .await
                .expect("crash: assemble pipeline");
            let outcome = run_to_end(invocation).await;
            panic!("crash: process outlived kill point: {outcome:?}");
        }
        "recover" => {
            let connector: Arc<dyn McpConnector> = Arc::new(RecordingMcpConnector {
                pool: pool.clone(),
                crash_in_flight: false,
            });
            let (assembler, captured) = root_pipeline_assembler(
                pool,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "must not run",
                ))],
                connector,
            );
            let error = crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
                &assembler,
                &request,
                &AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
                test_session_authority_for_reclaim(EXECUTION, GENERATION, 2),
                Arc::new(TestStateWriterLease::current()),
            )
            .await
            .err()
            .expect("recover: pipeline reclaim refused at inspect");
            let kind = crate::execution::assembly_failure(&error);
            let wire = crate::protocol::elitea::runtime::v1::RuntimeErrorCodeV1::try_from(
                crate::protocol::output::runtime_error(kind).code,
            )
            .expect("wire code");
            println!(
                "{OUTCOME_LINE}{kind:?} {}; model calls {}",
                wire.as_str_name(),
                model_requests(&captured)
            );
        }
        other => panic!("unknown pipeline phase: {other}"),
    }
}

async fn run_ordinary_kill_phase(pool: &sqlx::PgPool, phase: &str) {
    let request = ordinary_chat_request();
    match phase {
        "crash" => {
            let connector: Arc<dyn McpConnector> = Arc::new(RecordingMcpConnector {
                pool: pool.clone(),
                crash_in_flight: true,
            });
            let (assembler, _) = ordinary_assembler(
                pool,
                vec![
                    TestModelGatewayOutcome::Response(nested_agent_call_response(
                        "call_kill_ord",
                        TOPIC,
                    )),
                    TestModelGatewayOutcome::Response(pipeline_text_response("never reached")),
                ],
                connector,
            );
            let _ = run_ordinary_authorized(&assembler, claim(&request, 1)).await;
            panic!("crash: ordinary process outlived kill point");
        }
        "recover" => {
            let connector: Arc<dyn McpConnector> = Arc::new(RecordingMcpConnector {
                pool: pool.clone(),
                crash_in_flight: false,
            });
            let (assembler, captured) = ordinary_assembler(
                pool,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "must not run",
                ))],
                connector,
            );
            let _error = crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
                &assembler,
                &request,
                &AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
                test_session_authority_for_claim(EXECUTION, GENERATION, 2),
                Arc::new(TestStateWriterLease::current()),
            )
            .await
            .err()
            .expect("recover: ordinary reclaim refused at inspect");
            println!(
                "{OUTCOME_LINE}OrdinaryReclaimRefused; model calls {}",
                model_requests(&captured)
            );
        }
        other => panic!("unknown ordinary phase: {other}"),
    }
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_fu358_kill_child() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let shape = std::env::var(SHAPE_ENV).expect("shape");
    let phase = std::env::var(PHASE_ENV).expect("phase");
    let pool = connect(&database).await;
    match shape.as_str() {
        "pipeline" => run_pipeline_kill_phase(&pool, &phase).await,
        "ordinary" => run_ordinary_kill_phase(&pool, &phase).await,
        other => panic!("unknown shape: {other}"),
    }
    pool.close().await;
}
