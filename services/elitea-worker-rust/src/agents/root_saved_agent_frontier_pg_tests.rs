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
//! Since FU-323a an Agent node running a saved Agent is a recovery frontier
//! where the inspect step proves where it stopped. Here the Agent was inside a
//! saved-pipeline tool call, which may have started and is not replayable
//! (FU-323b resumes it once FU-291 lands): the reclaim is refused before the
//! run with the registered "may have started, not repeated" end
//! (`RECOVERY_REFUSED`; #1933 ended it `RECOVERY_FRONTIER_UNSUPPORTED`), one
//! operator WARN line and no model call.

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

/// The reclaim ends with the registered `RECOVERY_REFUSED` terminal (the
/// pipeline tool may have started; was `RECOVERY_FRONTIER_UNSUPPORTED` under
/// #1933, `Internal` before it), before any model call, with one WARN fault
/// line carrying ids only.
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
            outcome, "RecoveryRefused RUNTIME_ERROR_CODE_V1_RECOVERY_REFUSED; model calls 0",
            "{stdout}"
        );
        let output = elitea_faults::logtest::ChildOutput {
            stdout: stdout.into_owned(),
            stderr: stderr.into_owned(),
        };
        let code = elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_MAY_HAVE_STARTED;
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

/// The ids of the root session's events that start call `call_id`.
async fn root_call_starts(pool: &sqlx::PgPool, call_id: &str) -> Vec<String> {
    sqlx::query_scalar::<_, String>(
        "SELECT event_payload FROM elitea_runtime.agent_session_events \
         WHERE session_id = (SELECT thread_id FROM elitea_runtime.agent_graph_checkpoints \
           WHERE thread_id NOT LIKE '%/%' LIMIT 1) \
         ORDER BY event_ordinal",
    )
    .fetch_all(pool)
    .await
    .expect("root session events")
    .iter()
    .filter_map(|payload| serde_json::from_str::<adk_rust::Event>(payload).ok())
    .filter(|event| {
        event
            .tool_calls()
            .iter()
            .any(|call| call.call_id == Some(call_id))
    })
    .map(|event| event.id)
    .collect()
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
            assert_eq!(
                tool_boundaries(&db.pool, "claim-1").await,
                Vec::<String>::new()
            );
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
        // C5: the root session holds the call's start once, the receipt's own
        // durable start event (the re-entry re-sends that same event, which
        // the session stores once), so a reload shows one card.
        let receipt = receipts
            .as_object()
            .and_then(|calls| calls.values().next())
            .expect("the node's receipt");
        let call_id = receipt["activation"]["call_id"].as_str().expect("call id");
        let starts = root_call_starts(&db.pool, call_id).await;
        assert_eq!(
            starts,
            [receipt["original_start"]["id"]
                .as_str()
                .expect("start id")
                .to_owned()],
            "one start, the receipt's"
        );
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

// ---------------------------------------------------------------------------
// FU-323a: the root saved-Agent frontier, one crash window per case (T2-T10)
// and the forged frontiers, through the same two-process harness.
// ---------------------------------------------------------------------------

const SCENARIO_CHILD_TEST: &str = "agents::pipeline_tests::fanout_effect_pg_tests::root_saved_agent_frontier::postgres_root_saved_agent_scenario_child";
const MODE_ENV: &str = "ELITEA_ROOT_AGENT_FRONTIER_PG_MODE";
const SCENARIO_LINE: &str = "ROOT_AGENT_SCENARIO ";

/// One recover phase's outcome, as its child reports it.
#[derive(serde::Deserialize, serde::Serialize)]
struct ScenarioOutcome {
    /// `ok`, or the reclaim's error text (with its cause code).
    outcome: String,
    model_calls: usize,
    tool_calls: usize,
}

/// Run `phases` of `mode` as one OS process each, applying `between` after the
/// first; the last phase's reported outcome and its whole stdout.
async fn scenario(
    pool: &sqlx::PgPool,
    database: &str,
    mode: &str,
    phases: &[(&str, Option<i32>)],
    between: &str,
) -> (ScenarioOutcome, String) {
    let mut last = None;
    for (index, (phase, expected)) in phases.iter().enumerate() {
        if index == 1 && !between.is_empty() {
            // Test-only fixture edits written in this file, never input.
            sqlx::raw_sql(sqlx::AssertSqlSafe(between.to_owned()))
                .execute(pool)
                .await
                .expect("apply the between-phase change");
        }
        let mut command = std::process::Command::new(std::env::current_exe().expect("test binary"));
        command
            .args([
                "--exact",
                SCENARIO_CHILD_TEST,
                "--nocapture",
                "--test-threads=1",
            ])
            .env(DATABASE_ENV, database)
            .env(MODE_ENV, mode)
            .env(PHASE_ENV, phase);
        let output = run_phase(phase, command);
        let stdout = String::from_utf8_lossy(&output.stdout).into_owned();
        let stderr = String::from_utf8_lossy(&output.stderr);
        assert_eq!(
            output.status.code(),
            *expected,
            "{mode} {phase} process exit: {stdout} {stderr}"
        );
        last = Some(stdout);
    }
    let stdout = last.expect("one phase");
    let outcome = stdout
        .lines()
        .find_map(|line| line.split_once(SCENARIO_LINE).map(|(_, value)| value))
        .map_or_else(
            || {
                // A process that ended at its crash point reports nothing.
                assert_eq!(
                    phases.last().and_then(|(_, code)| *code),
                    Some(CRASH_EXIT_CODE),
                    "{mode}: the last phase reports its outcome: {stdout}"
                );
                ScenarioOutcome {
                    outcome: "crashed".to_owned(),
                    model_calls: 0,
                    tool_calls: 0,
                }
            },
            |value| serde_json::from_str(value).expect("scenario outcome JSON"),
        );
    (outcome, stdout)
}

async fn scenario_db() -> Option<IsolatedPostgres> {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the root saved-Agent frontier proofs");
        return None;
    };
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    Some(db)
}

const CRASH_THEN_RECOVER: &[(&str, Option<i32>)] =
    &[("crash", Some(CRASH_EXIT_CODE)), ("recover", Some(0))];

/// Every root row's receipt revision after the node's `Started` one is
/// removed: a Worker lost after the Agent's answer was persisted, before the
/// call receipt completed (T3). The rows are deleted newest first, as a
/// rollback of what the lost process did after that point.
const REWIND_TO_STARTED_RECEIPT: &str = "DELETE FROM elitea_runtime.agent_graph_checkpoints \
     WHERE thread_id NOT LIKE '%/%' AND save_ordinal > (\
       SELECT min(save_ordinal) FROM elitea_runtime.agent_graph_checkpoints \
       WHERE thread_id NOT LIKE '%/%' AND metadata LIKE '%\"phase\":\"Started\"%')";

/// T3: the Agent's answer is persisted, the receipt is still `Started`: the
/// answer is served from the Agent's completion receipt, with no model call.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_root_saved_agent_answered_before_its_receipt_completed_is_served() {
    let Some(db) = scenario_db().await else {
        return;
    };
    let (outcome, _) = scenario(
        &db.pool,
        &db.database_name,
        "answered",
        &[("crash", Some(0))],
        "",
    )
    .await;
    assert_eq!(outcome.outcome, "ok");
    sqlx::raw_sql(REWIND_TO_STARTED_RECEIPT)
        .execute(&db.pool)
        .await
        .expect("rewind past the started receipt");
    let (_, pending, metadata) = latest_root(&db.pool).await;
    assert_eq!(pending, r#"["delegate"]"#, "the node is the frontier again");
    let phases = metadata["elitea.pipeline.graph-calls.v1"]["calls"]
        .as_object()
        .expect("receipts")
        .values()
        .map(|receipt| receipt["outcome"]["phase"].clone())
        .collect::<Vec<_>>();
    assert_eq!(phases, [json!("Started")]);
    let (outcome, _) = scenario(
        &db.pool,
        &db.database_name,
        "answered",
        &[("recover", Some(0))],
        "",
    )
    .await;
    assert_eq!(outcome.outcome, "ok");
    assert_eq!(outcome.model_calls, 0, "served from the completion receipt");
    let (state, pending, _) = latest_root(&db.pool).await;
    assert_eq!(pending, "[]");
    assert_eq!(state["answer"], "answered once", "{state}");
    db.pool.close().await;
}

/// T4 (D2 at the root): lost in a read-only tool, the batch is replayed: the
/// tool runs once more and the Agent answers with one model call (I).
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_root_saved_agent_lost_in_a_read_only_tool_replays_it() {
    let Some(db) = scenario_db().await else {
        return;
    };
    let (outcome, _) = scenario(
        &db.pool,
        &db.database_name,
        "read_only",
        CRASH_THEN_RECOVER,
        "",
    )
    .await;
    assert_eq!(outcome.outcome, "ok");
    assert_eq!(outcome.tool_calls, 1, "the read-only tool runs again");
    assert_eq!(
        outcome.model_calls, 1,
        "the saved batch replays without a model call"
    );
    let (state, pending, _) = latest_root(&db.pool).await;
    assert_eq!(pending, "[]");
    assert_eq!(state["answer"], "resumed answer", "{state}");
    db.pool.close().await;
}

/// T5: lost in an effectful tool, the reclaim ends with the registered "may
/// have started, not repeated" refusal before the run: no tool, no model call.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_root_saved_agent_lost_in_an_effectful_tool_is_refused_and_not_repeated() {
    let Some(db) = scenario_db().await else {
        return;
    };
    let (outcome, stdout) = scenario(
        &db.pool,
        &db.database_name,
        "effect",
        CRASH_THEN_RECOVER,
        "",
    )
    .await;
    assert!(
        outcome
            .outcome
            .contains(crate::protocol::output::RECOVERY_REFUSED_UPSTREAM_CODE),
        "{}",
        outcome.outcome
    );
    assert_eq!((outcome.tool_calls, outcome.model_calls), (0, 0));
    assert!(
        stdout
            .contains(elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_MAY_HAVE_STARTED.as_str())
    );
    db.pool.close().await;
}

/// T6: Agent -> Agent, lost in the nested Agent's model call. The outer Agent
/// replays its delegated call and the nested Agent resumes from its own scope:
/// the nested restored request and the outer answer, two model calls (R).
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_root_saved_agent_lost_in_its_nested_agents_model_call_resumes() {
    let Some(db) = scenario_db().await else {
        return;
    };
    let (outcome, _) = scenario(
        &db.pool,
        &db.database_name,
        "agent_tool",
        CRASH_THEN_RECOVER,
        "",
    )
    .await;
    assert_eq!(outcome.outcome, "ok");
    assert_eq!(outcome.model_calls, 2);
    let (state, pending, _) = latest_root(&db.pool).await;
    assert_eq!(pending, "[]");
    assert_eq!(state["answer"], "outer answer", "{state}");
    db.pool.close().await;
}

/// T10: lost again in the restored model call (claim 2), the next reclaim
/// (claim 3) resumes from the same scope: one model request, the answer once.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_root_saved_agent_lost_twice_mid_model_resumes_again() {
    let Some(db) = scenario_db().await else {
        return;
    };
    let (outcome, _) = scenario(
        &db.pool,
        &db.database_name,
        "second_kill",
        &[
            ("crash", Some(CRASH_EXIT_CODE)),
            ("recover", Some(CRASH_EXIT_CODE)),
            ("recover_again", Some(0)),
        ],
        "",
    )
    .await;
    assert_eq!(outcome.outcome, "ok");
    assert_eq!(outcome.model_calls, 1);
    let (state, pending, _) = latest_root(&db.pool).await;
    assert_eq!(pending, "[]");
    assert_eq!(state["answer"], "resumed answer", "{state}");
    db.pool.close().await;
}

/// T8: a newer claim taking over between the older claim's inspect and its
/// assembly fences the older claim out: refused as a lost writer, nothing
/// written, no model call.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_takeover_between_a_root_agent_inspect_and_assembly_fences_the_older_claim() {
    let Some(db) = scenario_db().await else {
        return;
    };
    let (outcome, _) = scenario(
        &db.pool,
        &db.database_name,
        "takeover",
        CRASH_THEN_RECOVER,
        "",
    )
    .await;
    assert_eq!(outcome.outcome, "fenced");
    assert_eq!(outcome.model_calls, 0);
    db.pool.close().await;
}

/// The forged or unprovable frontiers: each is refused typed before the run,
/// with its own operator cause, and nothing calls the model.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_forged_root_agent_frontier_is_refused_typed() {
    let Some(db) = scenario_db().await else {
        return;
    };
    let scope = "session_id LIKE 'elitea-model-%'";
    let root = "thread_id NOT LIKE '%/%'";
    let cases: [(&str, String, elitea_faults::Code); 4] = [
        (
            "receipt names another Agent",
            format!(
                "UPDATE elitea_runtime.agent_graph_checkpoints SET metadata = \
                 replace(metadata, '\"name\":\"elitea_agent_3_v_4\"', '\"name\":\"elitea_agent_3_v_9\"') \
                 WHERE {root}"
            ),
            elitea_faults::codes::PIPELINE_RECOVERY_AGENT_RECEIPT_MISMATCH,
        ),
        (
            "another execution's model checkpoint",
            format!(
                "UPDATE elitea_runtime.agent_sessions SET state = \
                 replace(state, '{EXECUTION}', 'ffffffffffffffffffffffffffffffff') WHERE {scope}"
            ),
            elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_FOREIGN_EXECUTION,
        ),
        (
            "another generation's model checkpoint",
            format!(
                "UPDATE elitea_runtime.agent_sessions SET state = \
                 replace(state, '\"generation\":{GENERATION}', '\"generation\":9') WHERE {scope}; \
                 UPDATE elitea_runtime.agent_session_events SET event_payload = \
                 replace(event_payload, '\"generation\":{GENERATION}', '\"generation\":9') WHERE {scope}"
            ),
            elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_INVALID,
        ),
        (
            "a session with events and no checkpoint",
            format!("UPDATE elitea_runtime.agent_sessions SET state = '{{}}' WHERE {scope}"),
            elitea_faults::codes::PIPELINE_RECOVERY_AGENT_STEP_SESSION_WITHOUT_CHECKPOINT,
        ),
    ];
    for (case, tamper, code) in cases {
        let db = scenario_db().await.expect("a database per case");
        let (outcome, stdout) = scenario(
            &db.pool,
            &db.database_name,
            "forged",
            CRASH_THEN_RECOVER,
            &tamper,
        )
        .await;
        assert!(
            outcome.outcome.contains(code.as_str()),
            "{case}: {}",
            outcome.outcome
        );
        assert_eq!(outcome.model_calls, 0, "{case}");
        assert!(stdout.contains(code.as_str()), "{case}: the fault line");
        assert!(
            !stdout.contains(TOPIC),
            "{case}: the Agent's task reached the log"
        );
        db.pool.close().await;
    }
    db.pool.close().await;
}

/// The Agent's model step changed between the claim's inspect and its
/// assembly (a consistent edit of checkpoint and marker): the assembly-time
/// evidence differs from the one Main granted, so the run is refused before
/// any model call (D1 a).
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_root_agent_step_changed_after_its_grant_is_refused() {
    let Some(db) = scenario_db().await else {
        return;
    };
    let (outcome, _) = scenario(
        &db.pool,
        &db.database_name,
        "changed_after_grant",
        CRASH_THEN_RECOVER,
        "",
    )
    .await;
    assert!(
        outcome.outcome.starts_with("authorize the lifecycle"),
        "{}",
        outcome.outcome
    );
    assert_eq!(outcome.model_calls, 0);
    db.pool.close().await;
}

/// T9 (C2): a lock held on the root checkpoint writer row, then on the
/// Agent's model-scope writer row, makes each inspect read fail transiently
/// (`lock_timeout`): retained, not a refusal. Once released, the same reclaim
/// resumes the node with one model call.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_transient_read_failure_at_a_root_agent_inspect_is_retained() {
    let Some(db) = scenario_db().await else {
        return;
    };
    let (outcome, _) = scenario(
        &db.pool,
        &db.database_name,
        "transient",
        CRASH_THEN_RECOVER,
        "",
    )
    .await;
    assert_eq!(outcome.outcome, "retained root, retained scope, ok");
    assert_eq!(outcome.model_calls, 1);
    db.pool.close().await;
}

/// The Agent's model scope removed (its session, events and writer row): the
/// state of a Worker lost after the call receipt, before the Agent's first
/// model step was persisted (T2b). Built from a later crash by deleting what
/// the lost process wrote after that point; stated as a database fixture.
const DROP_MODEL_SCOPE: &str = "DELETE FROM elitea_runtime.agent_session_events \
       WHERE session_id LIKE 'elitea-model-%'; \
     DELETE FROM elitea_runtime.agent_sessions WHERE session_id LIKE 'elitea-model-%'; \
     DELETE FROM elitea_runtime.agent_session_writers WHERE session_id LIKE 'elitea-model-%'";

/// [`DROP_MODEL_SCOPE`] and every root receipt revision: a Worker lost after
/// the frontier was saved, before the node prepared its call (T2).
const DROP_RECEIPT_AND_MODEL_SCOPE: &str = "DELETE FROM elitea_runtime.agent_session_events \
       WHERE session_id LIKE 'elitea-model-%'; \
     DELETE FROM elitea_runtime.agent_sessions WHERE session_id LIKE 'elitea-model-%'; \
     DELETE FROM elitea_runtime.agent_session_writers WHERE session_id LIKE 'elitea-model-%'; \
     DELETE FROM elitea_runtime.agent_graph_checkpoints \
       WHERE thread_id NOT LIKE '%/%' AND metadata LIKE '%elitea.pipeline.graph-calls.v1%'";

/// T2 / T2b: lost before the receipt, or after it but before the Agent's
/// first model step: the node runs its Agent fresh, once (one model call), and
/// the frontier advances. Nothing had been sent, so nothing repeats.
#[tokio::test(flavor = "current_thread")]
async fn postgres_a_root_saved_agent_lost_before_its_first_model_step_runs_once() {
    for (case, rewind, receipt) in [
        ("before the receipt", DROP_RECEIPT_AND_MODEL_SCOPE, false),
        ("before the first model step", DROP_MODEL_SCOPE, true),
    ] {
        let Some(db) = scenario_db().await else {
            return;
        };
        let (crashed, _) = scenario(
            &db.pool,
            &db.database_name,
            "fresh",
            &[("crash", Some(CRASH_EXIT_CODE))],
            "",
        )
        .await;
        assert_eq!(crashed.outcome, "crashed");
        sqlx::raw_sql(rewind)
            .execute(&db.pool)
            .await
            .expect("rewind the lost process's writes");
        let (_, pending, metadata) = latest_root(&db.pool).await;
        assert_eq!(pending, r#"["delegate"]"#, "{case}");
        assert_eq!(
            metadata.get("elitea.pipeline.graph-calls.v1").is_some(),
            receipt,
            "{case}"
        );
        let (outcome, _) = scenario(
            &db.pool,
            &db.database_name,
            "fresh",
            &[("recover", Some(0))],
            "",
        )
        .await;
        assert_eq!(outcome.outcome, "ok", "{case}");
        assert_eq!(outcome.model_calls, 1, "{case}");
        let (state, pending, _) = latest_root(&db.pool).await;
        assert_eq!(pending, "[]", "{case}");
        assert_eq!(state["answer"], "resumed answer", "{case}: {state}");
        db.pool.close().await;
    }
}

/// Budget: the proof's cost on a reclaim. Twenty pre-assembly inspects with a
/// restorable Agent step (receipt check, one model-scope open and one read)
/// against twenty with no receipt (no scope read), on one database; the
/// child prints p50/p99 for the mapping.
#[tokio::test(flavor = "current_thread")]
async fn postgres_the_root_agent_proof_cost_is_measured() {
    let Some(db) = scenario_db().await else {
        return;
    };
    let (outcome, stdout) = scenario(
        &db.pool,
        &db.database_name,
        "budget",
        CRASH_THEN_RECOVER,
        "",
    )
    .await;
    assert_eq!(outcome.outcome, "measured");
    assert_eq!(outcome.model_calls, 0);
    let line = stdout
        .lines()
        .find_map(|line| {
            line.split_once("ROOT_AGENT_PROOF_BUDGET ")
                .map(|(_, value)| value)
        })
        .expect("the budget line");
    eprintln!("ROOT_AGENT_PROOF_BUDGET {line}");
    db.pool.close().await;
}

fn report(outcome: &str, captured: &CapturedModelRequests, tool_calls: usize) {
    println!(
        "{SCENARIO_LINE}{}",
        serde_json::to_string(&ScenarioOutcome {
            outcome: outcome.to_owned(),
            model_calls: model_requests(captured),
            tool_calls,
        })
        .expect("outcome JSON")
    );
}

fn scenario_assembler(
    pool: &sqlx::PgPool,
    mode: &str,
    outcomes: Vec<TestModelGatewayOutcome>,
    connector: Arc<dyn McpConnector>,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    let mut config = test_model_gateway_config();
    config.response_header_timeout = Duration::from_secs(60);
    let (details, second) = match mode {
        "read_only" | "effect" => (effectful_agent(), None),
        "agent_tool" => (delegating_agent(), Some(helper_agent())),
        _ => (plain_saved_agent(), None),
    };
    assembler_on(
        pool,
        saved_runtime_on(outcomes, details, second, None, config),
        connector,
    )
}

#[tokio::test(flavor = "current_thread")]
#[allow(clippy::too_many_lines)] // One child per process, one arm per crash window.
async fn postgres_root_saved_agent_scenario_child() {
    use tracing_subscriber::layer::SubscriberExt as _;
    use tracing_subscriber::util::SubscriberInitExt as _;
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    tracing_subscriber::registry()
        .with(
            crate::diagnostics::log_layer(Some("info"), std::io::stdout).expect("operator stream"),
        )
        .init();
    let pool = connect(&database).await;
    let request = root_request();
    let mode = std::env::var(MODE_ENV).expect("mode");
    let phase = std::env::var(PHASE_ENV).expect("phase");
    let tool_calls = Arc::new(AtomicUsize::new(0));
    let recovering_connector = |read_only: bool| -> Arc<dyn McpConnector> {
        Arc::new(PipelineMcpConnector {
            connections: Arc::new(AtomicUsize::new(0)),
            tool_calls: Arc::clone(&tool_calls),
            read_only,
        })
    };
    if phase == "crash" {
        let (outcomes, connector): (_, Arc<dyn McpConnector>) = match mode.as_str() {
            "read_only" | "effect" => (
                vec![TestModelGatewayOutcome::Response(
                    pipeline_mcp_tool_call_response(),
                )],
                Arc::new(CrashingMcpConnector {
                    pool: pool.clone(),
                    read_only: mode == "read_only",
                }),
            ),
            "agent_tool" => (
                vec![
                    TestModelGatewayOutcome::Response(nested_agent_call_response(
                        "call_root_1",
                        TOPIC,
                    )),
                    TestModelGatewayOutcome::ExitProcess(CRASH_EXIT_CODE),
                ],
                pipeline_connector(),
            ),
            "answered" => (
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "answered once",
                ))],
                pipeline_connector(),
            ),
            _ => (
                vec![TestModelGatewayOutcome::ExitProcess(CRASH_EXIT_CODE)],
                pipeline_connector(),
            ),
        };
        let (assembler, _) = scenario_assembler(&pool, &mode, outcomes, connector);
        let invocation = assembler
            .assemble(claim(&request, 1))
            .await
            .unwrap_or_else(|error| panic!("crash: assemble the root pipeline: {error}"));
        let outcome = run_to_end(invocation).await;
        // Only `answered` runs to its end: the parent rewinds what follows.
        assert_eq!(
            mode, "answered",
            "crash: outlived its crash point: {outcome:?}"
        );
        outcome.expect("answered: the run completes");
        println!(
            "{SCENARIO_LINE}{}",
            serde_json::to_string(&ScenarioOutcome {
                outcome: "ok".to_owned(),
                model_calls: 1,
                tool_calls: 0,
            })
            .expect("outcome JSON")
        );
        pool.close().await;
        return;
    }
    let attempt = if phase == "recover_again" { 3 } else { 2 };
    match mode.as_str() {
        "takeover" => {
            let (older, captured) =
                scenario_assembler(&pool, &mode, Vec::new(), pipeline_connector());
            crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
                &older,
                &request,
                &AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
                test_session_authority_for_reclaim(EXECUTION, GENERATION, 2),
                Arc::new(TestStateWriterLease::current()),
            )
            .await
            .expect("claim 2 inspects the frontier");
            let (newer, _) = scenario_assembler(&pool, &mode, Vec::new(), pipeline_connector());
            crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
                &newer,
                &request,
                &AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
                test_session_authority_for_reclaim(EXECUTION, GENERATION, 3),
                Arc::new(TestStateWriterLease::current()),
            )
            .await
            .expect("claim 3 takes over and inspects");
            let rows = || async {
                sqlx::query_as::<_, (i64, i64)>(
                    "SELECT (SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints), \
                            (SELECT count(*) FROM elitea_runtime.agent_session_events)",
                )
                .fetch_one(&pool)
                .await
                .expect("count rows")
            };
            let before = rows().await;
            let assembled = older
                .assemble_checkpoint(AuthorizedNativeAssembly::from_authorized(
                    &request,
                    test_runtime_context_authority(),
                    test_session_authority_for_reclaim(EXECUTION, GENERATION, 2),
                    Arc::new(TestStateWriterLease::current()),
                    AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
                ))
                .await;
            let Err(refusal) = assembled else {
                panic!("the fenced claim 2 must not assemble after claim 3 took over");
            };
            assert_eq!(
                refusal.code(),
                elitea_agent_runtime::assembly_error::NativeAgentAssemblyErrorCode::AuthorizationFailed,
                "{refusal}"
            );
            assert_eq!(rows().await, before, "claim 2 wrote nothing");
            report("fenced", &captured, 0);
        }
        "transient" => {
            let (assembler, captured) = scenario_assembler(
                &pool,
                &mode,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "resumed answer",
                ))],
                pipeline_connector(),
            );
            let mut seen = Vec::new();
            for (name, lock) in [
                (
                    "root",
                    "SELECT 1 FROM elitea_runtime.agent_graph_checkpoint_writers \
                     WHERE thread_id NOT LIKE '%/%' FOR UPDATE",
                ),
                (
                    "scope",
                    "SELECT 1 FROM elitea_runtime.agent_session_writers \
                     WHERE session_id LIKE 'elitea-model-%' FOR UPDATE",
                ),
            ] {
                let mut holder = pool.begin().await.expect("lock holder");
                let locked = sqlx::query(lock)
                    .fetch_all(&mut *holder)
                    .await
                    .expect("hold the writer row");
                assert_eq!(locked.len(), 1, "{name}: one row held");
                let error = crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
                    &assembler,
                    &request,
                    &AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
                    test_session_authority_for_reclaim(EXECUTION, GENERATION, 2),
                    Arc::new(TestStateWriterLease::current()),
                )
                .await
                .err()
                .unwrap_or_else(|| panic!("{name}: the held row blocks the inspect read"));
                assert!(
                    crate::agents::session::is_transient_inspect_failure(&error),
                    "{name}: {error} {:?}",
                    error
                        .cause()
                        .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code)
                );
                holder.rollback().await.expect("release the row");
                seen.push(format!("retained {name}"));
            }
            let outcome = match recovered(&assembler, &request, EXECUTION, 2).await {
                Ok(invocation) => match run_to_end(invocation).await {
                    Ok(_) => "ok".to_owned(),
                    Err(error) => format!("run: {error}"),
                },
                Err(error) => error,
            };
            seen.push(outcome);
            report(&seen.join(", "), &captured, 0);
        }
        "budget" => {
            let (assembler, captured) =
                scenario_assembler(&pool, &mode, Vec::new(), pipeline_connector());
            let measure = || async {
                let mut times = Vec::new();
                for _ in 0..20 {
                    let started = std::time::Instant::now();
                    crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
                        &assembler,
                        &request,
                        &AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
                        test_session_authority_for_reclaim(EXECUTION, GENERATION, 2),
                        Arc::new(TestStateWriterLease::current()),
                    )
                    .await
                    .expect("the reclaim inspects");
                    times.push(started.elapsed().as_micros());
                }
                times.sort_unstable();
                (times[9], times[19])
            };
            let with_proof = measure().await;
            sqlx::raw_sql(DROP_RECEIPT_AND_MODEL_SCOPE)
                .execute(&pool)
                .await
                .expect("drop the receipt and the scope");
            let without = measure().await;
            println!(
                "ROOT_AGENT_PROOF_BUDGET restorable p50={}us p99={}us; no receipt p50={}us p99={}us",
                with_proof.0, with_proof.1, without.0, without.1
            );
            report("measured", &captured, 0);
        }
        "changed_after_grant" => {
            let (assembler, captured) = scenario_assembler(
                &pool,
                &mode,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "never",
                ))],
                pipeline_connector(),
            );
            let evidence = crate::agents::runtime::NativeAgentAssembler::inspect_checkpoint(
                &assembler,
                &request,
                &AuthorizedNativeCommandBinding::fixture_for_execution(EXECUTION),
                test_session_authority_for_reclaim(EXECUTION, GENERATION, 2),
                Arc::new(TestStateWriterLease::current()),
            )
            .await
            .expect("claim 2 inspects");
            let (authorizer, control) = crate::protocol::control::test_checkpoint_authorizer(
                EXECUTION,
                GENERATION,
                evidence.digest(),
            );
            let authorization = authorizer
                .authorize(&control, evidence)
                .await
                .unwrap_or_else(|_| panic!("authorize"));
            // A consistent edit of the Agent's saved request: still a valid
            // step, but not the one Main granted.
            let scope = "session_id LIKE 'elitea-model-%'";
            sqlx::raw_sql(sqlx::AssertSqlSafe(format!(
                "UPDATE elitea_runtime.agent_sessions SET state = replace(state, '{TOPIC}', 'topic-r02x') WHERE {scope}; \
                 UPDATE elitea_runtime.agent_session_events SET event_payload = replace(event_payload, '{TOPIC}', 'topic-r02x') WHERE {scope}"
            )))
            .execute(&pool)
            .await
            .expect("edit the saved step");
            let pending = assembler
                .assemble_checkpoint(claim(&request, 2))
                .await
                .expect("the edited step is still a valid step");
            let (_, _, _, _, permit) = authorization.into_lifecycle_parts();
            let outcome = match pending.authorize_lifecycle(permit) {
                Ok(_) => "assembled".to_owned(),
                Err(error) => format!("authorize the lifecycle: {error}"),
            };
            report(&outcome, &captured, 0);
        }
        _ => {
            let (outcomes, connector) = match (mode.as_str(), phase.as_str()) {
                ("second_kill", "recover") => (
                    vec![TestModelGatewayOutcome::ExitProcess(CRASH_EXIT_CODE)],
                    pipeline_connector() as Arc<dyn McpConnector>,
                ),
                ("read_only", _) => (
                    vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                        "resumed answer",
                    ))],
                    recovering_connector(true),
                ),
                ("effect", _) => (Vec::new(), recovering_connector(false)),
                ("agent_tool", _) => (
                    vec![
                        TestModelGatewayOutcome::Response(pipeline_text_response("nested answer")),
                        TestModelGatewayOutcome::Response(pipeline_text_response("outer answer")),
                    ],
                    pipeline_connector() as Arc<dyn McpConnector>,
                ),
                ("answered", _) => (Vec::new(), pipeline_connector() as Arc<dyn McpConnector>),
                _ => (
                    vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                        "resumed answer",
                    ))],
                    pipeline_connector() as Arc<dyn McpConnector>,
                ),
            };
            let (assembler, captured) = scenario_assembler(&pool, &mode, outcomes, connector);
            let outcome = match recovered(&assembler, &request, EXECUTION, attempt).await {
                Ok(invocation) => match run_to_end(invocation).await {
                    Ok(_) => "ok".to_owned(),
                    Err(error) => format!("run: {error}"),
                },
                Err(error) => error,
            };
            report(&outcome, &captured, tool_calls.load(Ordering::Acquire));
        }
    }
    pool.close().await;
}
