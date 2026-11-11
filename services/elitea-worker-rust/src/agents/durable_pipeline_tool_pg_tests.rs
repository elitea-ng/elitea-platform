//! FU-291a on real `PostgreSQL`, one OS process per Worker: a root Agent node
//! whose saved Agent calls a saved pipeline as a tool, with the Worker lost
//! inside the nested pipeline.
//!
//! The root graph's `delegate` node runs the saved Agent (application 3,
//! version 4, `pipeline_delegating_agent`). That Agent calls the saved pipeline
//! `helper-flow` (application 5, version 6, `helper_pipeline`: one LLM node).
//! The crash process dies in the nested LLM node's model call; a replacement
//! Worker reclaims the same execution with a later claim through the
//! production inspect → authorize → `assemble_checkpoint` path.

use super::*;

const CHILD_TEST: &str = "agents::pipeline_tests::fanout_effect_pg_tests::durable_pipeline_tool::postgres_durable_pipeline_tool_child";
const DATABASE_ENV: &str = "ELITEA_PIPELINE_TOOL_PG_DATABASE";
const PHASE_ENV: &str = "ELITEA_PIPELINE_TOOL_PG_PHASE";
const OUTCOME_LINE: &str = "FU291_RECOVER_OUTCOME ";
const TOPIC: &str = "topic-r01x";

/// The root graph: one Agent node running the saved Agent.
fn root_request() -> super::super::super::request::AgentExecutionRequest {
    let mut request = agent_pipeline_request("release-agent", "agent");
    request.payload.application["version_details"]["instructions"] = json!(
        "state:\n  answer: str\n  messages: list\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: release-agent\n    input_mapping:\n      task: {type: fixed, value: 'topic-r01x'}\n    output: [answer, messages]\n    transition: END\n"
    );
    request
}

fn root_assembler(
    pool: &sqlx::PgPool,
    outcomes: Vec<TestModelGatewayOutcome>,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    let mut config = test_model_gateway_config();
    config.response_header_timeout = Duration::from_secs(60);
    assembler_on(
        pool,
        saved_runtime_on(
            outcomes,
            pipeline_delegating_agent(),
            Some(helper_pipeline()),
            None,
            config,
        ),
        pipeline_connector(),
    )
}

/// D8 (FU-291 plan §6): the base behaviour of a Worker lost inside a root
/// Agent node's pipeline-tool call, recorded before any FU-291a code.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_root_agent_node_lost_inside_its_pipeline_tool_is_not_resumed_on_the_base() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the FU-291a pipeline-tool proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let mut outcome = String::new();
    for (phase, expected) in [("crash", Some(CRASH_EXIT_CODE)), ("recover", Some(0))] {
        let mut command = std::process::Command::new(std::env::current_exe().expect("test binary"));
        command
            .args(["--exact", CHILD_TEST, "--nocapture", "--test-threads=1"])
            .env(DATABASE_ENV, &db.database_name)
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
            // The outer Agent dispatched the pipeline tool behind the
            // "may have started" boundary, fenced to claim 1.
            assert_eq!(
                tool_boundaries(&db.pool, "claim-1").await.len(),
                1,
                "the pipeline-tool call crossed a may-have-started boundary"
            );
            // The root row the reclaim inspects: the Agent node on its frontier.
            let frontier = sqlx::query_scalar::<_, String>(
                "SELECT pending_nodes FROM elitea_runtime.agent_graph_checkpoints \
                 WHERE thread_id NOT LIKE '%/%' ORDER BY save_ordinal DESC LIMIT 1",
            )
            .fetch_one(&db.pool)
            .await
            .expect("the root row");
            assert_eq!(frontier, r#"["delegate"]"#);
            // The nested pipeline ran on a memory checkpointer: no durable row.
            let nested = sqlx::query_scalar::<_, i64>(
                "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints \
                 WHERE thread_id LIKE '%/%'",
            )
            .fetch_one(&db.pool)
            .await
            .expect("nested rows");
            assert_eq!(nested, 0, "the nested pipeline wrote no durable row");
        } else {
            outcome = stdout
                .lines()
                .find_map(|line| line.split_once(OUTCOME_LINE).map(|(_, value)| value))
                .unwrap_or_else(|| panic!("recover reports its outcome: {stdout}"))
                .to_owned();
        }
    }
    eprintln!("D8 recover outcome: {outcome}");
    // D8 on the base (e77b467fd): the reclaim is refused at inspect, before
    // any model call. The cause is the root frontier, not the tool boundary:
    // an Agent node running a saved Agent is never a recovery frontier
    // (`compiler.rs` `recovery_frontier_supported`), and that refusal is an
    // uncaused `invalid_configuration`, which the lifecycle maps to
    // `RuntimeFailureKind::Internal`, not `RECOVERY_REFUSED`.
    assert_eq!(
        outcome,
        "refused before the run: inspect: the invocation-local native agent session could not be configured []; model calls 0"
    );
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_durable_pipeline_tool_child() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let pool = connect(&database).await;
    let request = root_request();
    match std::env::var(PHASE_ENV).expect("phase").as_str() {
        "crash" => {
            let (assembler, _) = root_assembler(
                &pool,
                vec![
                    TestModelGatewayOutcome::Response(nested_agent_call_response(
                        "call_root_1",
                        TOPIC,
                    )),
                    // The nested pipeline's LLM node: the Worker dies here.
                    TestModelGatewayOutcome::ExitProcess(CRASH_EXIT_CODE),
                ],
            );
            let invocation = assembler
                .assemble(claim(&request, 1))
                .await
                .unwrap_or_else(|error| {
                    panic!(
                        "crash: assemble the root pipeline: {error} {:?}",
                        error.cause().map(
                            elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code
                        )
                    )
                });
            let outcome = run_to_end(invocation).await;
            panic!("crash: the process outlived its crash point: {outcome:?}");
        }
        "recover" => {
            let (assembler, captured) = root_assembler(
                &pool,
                vec![
                    TestModelGatewayOutcome::Response(pipeline_text_response(&format!(
                        "nested answer for {TOPIC}"
                    ))),
                    TestModelGatewayOutcome::Response(pipeline_text_response(&format!(
                        "outer answer for {TOPIC}"
                    ))),
                ],
            );
            let outcome = match recovered(&assembler, &request, EXECUTION, 2).await {
                Err(error) => format!("refused before the run: {error}"),
                Ok(invocation) => match run_to_end(invocation).await {
                    Ok(_) => "completed".to_owned(),
                    Err(error) => format!("failed in the run: {error}"),
                },
            };
            println!(
                "{OUTCOME_LINE}{outcome}; model calls {}",
                model_requests(&captured)
            );
        }
        other => panic!("unknown phase {other}"),
    }
    pool.close().await;
}
