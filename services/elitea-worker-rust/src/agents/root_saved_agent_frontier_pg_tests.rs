//! A Worker lost while a root Agent node runs a saved Agent, on real
//! `PostgreSQL`, one OS process per Worker.
//!
//! The root graph's `delegate` node runs the saved Agent (application 3,
//! version 4, `pipeline_delegating_agent`), which calls the saved pipeline
//! `helper-flow` (application 5, version 6, `helper_pipeline`: one LLM node)
//! as a tool. The crash process dies in the nested LLM node's model call. A
//! replacement Worker reclaims the same execution with a later claim through
//! the production inspect step.
//!
//! An Agent node running a saved Agent is not a recovery frontier
//! (`an_agent_node_is_a_recovery_frontier_only_for_a_frozen_saved_pipeline`;
//! FU-323 makes it one). The reclaim is refused before the run, typed and
//! registered, with one operator WARN line and no model call.

use super::*;

const CHILD_TEST: &str = "agents::pipeline_tests::fanout_effect_pg_tests::root_saved_agent_frontier::postgres_root_saved_agent_frontier_child";
const DATABASE_ENV: &str = "ELITEA_ROOT_AGENT_FRONTIER_PG_DATABASE";
const PHASE_ENV: &str = "ELITEA_ROOT_AGENT_FRONTIER_PG_PHASE";
const OUTCOME_LINE: &str = "ROOT_AGENT_FRONTIER_OUTCOME ";
/// The task the root Agent node hands its saved Agent; never logged.
const TOPIC: &str = "topic-r01x";

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

/// The reclaim ends with the registered `RECOVERY_FRONTIER_UNSUPPORTED`
/// terminal of an Agent step (was the generic `Internal`), before any model
/// call, with one WARN fault line carrying ids only.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_root_saved_agent_node_lost_mid_run_is_refused_typed_on_reclaim() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the root saved-Agent frontier proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
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
            .env(PHASE_ENV, phase);
        let output = run_phase(phase, command);
        let stdout = String::from_utf8_lossy(&output.stdout);
        let stderr = String::from_utf8_lossy(&output.stderr);
        assert_eq!(
            output.status.code(),
            expected,
            "{phase} process exit: {stdout} {stderr}"
        );
        if phase == "crash" {
            // The pipeline-tool call crossed a may-have-started boundary,
            // fenced to claim 1, and the root row's frontier is the Agent node.
            assert_eq!(tool_boundaries(&db.pool, "claim-1").await.len(), 1);
            let frontier = sqlx::query_scalar::<_, String>(
                "SELECT pending_nodes FROM elitea_runtime.agent_graph_checkpoints \
                 WHERE thread_id NOT LIKE '%/%' ORDER BY save_ordinal DESC LIMIT 1",
            )
            .fetch_one(&db.pool)
            .await
            .expect("the root row");
            assert_eq!(frontier, r#"["delegate"]"#);
            continue;
        }
        let outcome = stdout
            .lines()
            .find_map(|line| line.split_once(OUTCOME_LINE).map(|(_, value)| value))
            .unwrap_or_else(|| panic!("recover reports its outcome: {stdout}"));
        assert_eq!(
            outcome,
            "RecoveryFrontierAgentStep RUNTIME_ERROR_CODE_V1_RECOVERY_FRONTIER_UNSUPPORTED; model calls 0",
            "{stdout}"
        );
        let output = elitea_faults::logtest::ChildOutput {
            stdout: stdout.into_owned(),
            stderr: stderr.into_owned(),
        };
        let code = elitea_faults::codes::PIPELINE_RECOVERY_SAVED_AGENT_FRONTIER;
        let lines = output.operator_lines();
        let refusals = lines
            .iter()
            .filter(|line| line["code"] == code.as_str())
            .collect::<Vec<_>>();
        assert_eq!(refusals.len(), 1, "{}", output.stdout);
        let line = refusals[0];
        assert_eq!(line["level"], "WARN");
        assert_eq!(line["msg"], code.sentence());
        assert_eq!(line["event"], "pipeline_recovery_frontier_refused");
        assert_eq!(line["execution_id"], EXECUTION);
        assert_eq!(line["generation"], GENERATION);
        assert_eq!(line["node_id"], "delegate");
        assert_eq!(line["support_ref"], EXECUTION);
        assert!(
            !output.stdout.contains(TOPIC) && !output.stderr.contains(TOPIC),
            "the Agent's task reached the log"
        );
    }
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_saved_agent_frontier_child() {
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
                .unwrap_or_else(|error| panic!("crash: assemble the root pipeline: {error}"));
            let outcome = run_to_end(invocation).await;
            panic!("crash: the process outlived its crash point: {outcome:?}");
        }
        "recover" => {
            use tracing_subscriber::layer::SubscriberExt as _;
            use tracing_subscriber::util::SubscriberInitExt as _;
            tracing_subscriber::registry()
                .with(
                    crate::diagnostics::log_layer(Some("info"), std::io::stdout)
                        .expect("operator stream"),
                )
                .init();
            let (assembler, captured) = root_assembler(
                &pool,
                vec![
                    TestModelGatewayOutcome::Response(pipeline_text_response("nested answer")),
                    TestModelGatewayOutcome::Response(pipeline_text_response("outer answer")),
                ],
            );
            // The production reclaim's first step, as `checkpoint_recovery`
            // runs it: a refusal here becomes the run's terminal.
            let error = crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
                &assembler,
                &request,
                &AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
                test_session_authority_for_reclaim(EXECUTION, GENERATION, 2),
                Arc::new(TestStateWriterLease::current()),
            )
            .await
            .err()
            .expect("recover: the reclaim is refused at inspect");
            let kind = crate::execution::assembly_failure(&error);
            let wire = crate::protocol::elitea::runtime::v1::RuntimeErrorCodeV1::try_from(
                crate::protocol::output::runtime_error(kind).code,
            )
            .expect("a wire code");
            println!(
                "{OUTCOME_LINE}{kind:?} {}; model calls {}",
                wire.as_str_name(),
                model_requests(&captured)
            );
        }
        other => panic!("unknown phase {other}"),
    }
    pool.close().await;
}

// ---------------------------------------------------------------------------
// FU-323a: a root Agent node running a saved Agent is a recovery frontier.
// The Worker is lost in the saved Agent's own model call; a replacement
// Worker resumes from that Agent's model checkpoint.
// ---------------------------------------------------------------------------

const RESUME_CHILD_TEST: &str = "agents::pipeline_tests::fanout_effect_pg_tests::root_saved_agent_frontier::postgres_root_saved_agent_resume_child";
const RESUMED_LINE: &str = "ROOT_AGENT_RESUMED_REQUEST ";

/// The saved Agent the root's `delegate` node runs: no tools, answers in text.
fn plain_saved_agent() -> Value {
    let mut agent = effectful_agent();
    agent["instructions"] = json!("Answer the topic you are given.");
    agent["tools"] = json!([]);
    agent
}

fn plain_assembler(
    pool: &sqlx::PgPool,
    outcomes: Vec<TestModelGatewayOutcome>,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    let mut config = test_model_gateway_config();
    config.response_header_timeout = Duration::from_secs(60);
    assembler_on(
        pool,
        saved_runtime_on(outcomes, plain_saved_agent(), None, None, config),
        pipeline_connector(),
    )
}

/// The latest root row: its state and pending nodes.
async fn latest_root(pool: &sqlx::PgPool) -> (Value, String, Value) {
    let (state, pending, metadata) = sqlx::query_as::<_, (String, String, String)>(
        "SELECT state, pending_nodes, metadata FROM elitea_runtime.agent_graph_checkpoints \
         WHERE thread_id NOT LIKE '%/%' ORDER BY save_ordinal DESC LIMIT 1",
    )
    .fetch_one(pool)
    .await
    .expect("the root row");
    (
        serde_json::from_str(&state).expect("root state JSON"),
        pending,
        serde_json::from_str(&metadata).expect("root metadata JSON"),
    )
}

/// STEP 0 (FU-323 D8): lost mid model call, the root saved-Agent node
/// resumes from the Agent's model checkpoint. The replacement Worker sends
/// exactly one model request, equal to the lost one (C4), the answer lands
/// once, the call receipt completes and the frontier advances.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_root_saved_agent_node_lost_mid_model_resumes_from_its_model_checkpoint() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the root saved-Agent resume proof");
        return;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let mut lost = None;
    for (phase, expected) in [("crash", Some(CRASH_EXIT_CODE)), ("recover", Some(0))] {
        let mut command = std::process::Command::new(std::env::current_exe().expect("test binary"));
        command
            .args([
                "--exact",
                RESUME_CHILD_TEST,
                "--nocapture",
                "--test-threads=1",
            ])
            .env(DATABASE_ENV, &db.database_name)
            .env(PHASE_ENV, phase);
        let output = run_phase(phase, command);
        let stdout = String::from_utf8_lossy(&output.stdout);
        let stderr = String::from_utf8_lossy(&output.stderr);
        assert_eq!(
            output.status.code(),
            expected,
            "{phase} process exit: {stdout} {stderr}"
        );
        let line = |prefix: &str| {
            stdout
                .lines()
                .find_map(|line| line.split_once(prefix).map(|(_, value)| value.to_owned()))
        };
        if phase == "crash" {
            // The Agent's model step is durable under claim 1, and the
            // root row's frontier is the Agent node.
            assert_eq!(
                phase_boundaries(&db.pool, "claim-1", "model_pending")
                    .await
                    .len(),
                1
            );
            assert!(tool_boundaries(&db.pool, "claim-1").await.is_empty());
            let (_, pending, _) = latest_root(&db.pool).await;
            assert_eq!(pending, r#"["delegate"]"#);
            lost = line(crate::transport::openai_compatible_facade::EXIT_REQUEST_LINE);
            continue;
        }
        assert!(stdout.contains("1 passed"), "recover: {stdout} {stderr}");
        // C4: the restored request is the stored one, sent once more.
        let resumed = line(RESUMED_LINE).expect("recover reports its request");
        let lost = lost.take().expect("crash reports its lost request");
        assert_eq!(
            serde_json::from_str::<Value>(&resumed).expect("resumed request JSON"),
            serde_json::from_str::<Value>(&lost).expect("lost request JSON"),
            "the restored model request equals the lost one"
        );
        let (state, pending, metadata) = latest_root(&db.pool).await;
        assert_eq!(pending, "[]", "the frontier advanced");
        assert_eq!(state["answer"], "resumed answer", "{state}");
        let receipts = &metadata["elitea.pipeline.graph-calls.v1"]["calls"];
        let outcomes = receipts
            .as_object()
            .expect("graph-call receipts")
            .values()
            .map(|receipt| receipt["outcome"]["phase"].clone())
            .collect::<Vec<_>>();
        assert_eq!(outcomes, [json!("Completed")], "{receipts}");
        let advisory = sqlx::query_scalar::<_, i64>(
            "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'",
        )
        .fetch_one(&db.pool)
        .await
        .expect("advisory locks");
        assert_eq!(advisory, 0);
    }
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_root_saved_agent_resume_child() {
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
            let (assembler, _) = plain_assembler(
                &pool,
                vec![TestModelGatewayOutcome::ExitProcessPrintingRequest(
                    CRASH_EXIT_CODE,
                )],
            );
            let invocation = assembler
                .assemble(claim(&request, 1))
                .await
                .unwrap_or_else(|error| panic!("crash: assemble the root pipeline: {error}"));
            let outcome = run_to_end(invocation).await;
            panic!("crash: the process outlived its crash point: {outcome:?}");
        }
        "recover" => {
            let (assembler, captured) = plain_assembler(
                &pool,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "resumed answer",
                ))],
            );
            let invocation = recovered(&assembler, &request, EXECUTION, 2)
                .await
                .unwrap_or_else(|error| panic!("recover: the reclaim resumes: {error}"));
            run_to_end(invocation)
                .await
                .unwrap_or_else(|error| panic!("recover: the resumed node completes: {error}"));
            assert_eq!(model_requests(&captured), 1, "exactly one model request");
            let body = captured.lock().expect("captured requests")[0].body.clone();
            println!("{RESUMED_LINE}{}", String::from_utf8_lossy(&body));
        }
        other => panic!("unknown phase {other}"),
    }
    pool.close().await;
}
