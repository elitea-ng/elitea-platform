//! Scoped application continuation against real `PostgreSQL`, across Worker process replacement
//! (Point 5 Wave 2a-1).
//!
//! Each phase of a scenario runs in its own OS process with its own claim, as in
//! `pipeline_restart_pg_tests.rs`: the nested application tool names every child invocation from a
//! process-global counter, and a replacement Worker restarts it. The assembler is the production
//! `PipelineNativeAgentAssembler::postgres`.
//!
//! * `nested`: a root `agent` node calls a saved child pipeline whose `agent` node calls an
//!   ordinary Agent that asks (`ask_user`). Phase `start` pauses; phase `answer` (a replacement
//!   Worker) resumes and finishes; phase `repeat` (another replacement Worker) sends the same
//!   answer again and must resume nothing.
//! * `older`: a conversation an older worker paused inside an `agent` node (the one resolver
//!   this branch keeps for that case) is answered by the current Worker and finishes without ever
//!   writing a graph-call receipt.

use super::restart_pg_tests::{
    CHECKPOINT_MIGRATION, PHASE_DEADLINE, authorized_turn, connect, model_requests, pending_card,
    run_phase,
};
use super::scoped_tests::{
    Saved, answer, asking_agent, child_pipeline_yaml, reference, root_request, root_yaml,
    saved_runtime, saved_version,
};
use super::*;
use crate::agents::application_tools::ApplicationContinuation;
use crate::state::postgres_session_tests::{IsolatedPostgres, install_schema};
use crate::state::{CheckpointLimits, SessionLimits};
use std::collections::BTreeMap;

const CHILD_TEST: &str = "agents::pipeline_tests::scoped_pg_tests::postgres_scoped_child";
const DATABASE_ENV: &str = "ELITEA_SCOPED_PG_DATABASE";
const SCENARIO_ENV: &str = "ELITEA_SCOPED_PG_SCENARIO";
const PHASE_ENV: &str = "ELITEA_SCOPED_PG_PHASE";
const PENDING_ENV: &str = "ELITEA_SCOPED_PG_PENDING";
const PENDING_MARKER: &str = "SCOPED_PENDING=";
const RECEIPTS_KEY: &str = "elitea.pipeline.graph-calls.v1";

/// The sensitive-tool Agent of the existing in-memory fixture: one `lookup_release` MCP read the
/// policy marks sensitive, so the call pauses on a confirmation card.
fn sensitive_agent() -> Value {
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

/// Scenarios whose pause is a sensitive-tool confirmation card of an `agent` node's Agent.
fn is_card(scenario: &str) -> bool {
    matches!(scenario, "sensitive" | "batch")
}

fn versions(scenario: &str) -> BTreeMap<Saved, Value> {
    if scenario == "sensitive" {
        return BTreeMap::from([((3, 4), sensitive_agent())]);
    }
    if scenario == "batch" {
        return BTreeMap::from([((3, 4), super::scoped_tests::batch_agent())]);
    }
    let mut versions = BTreeMap::from([((5, 6), asking_agent())]);
    if scenario == "nested" {
        versions.insert(
            (3, 4),
            saved_version(
                "pipeline",
                &child_pipeline_yaml("worker"),
                &json!({}),
                vec![reference((5, 6), "agent", "worker")],
            ),
        );
    }
    versions
}

fn request(scenario: &str) -> super::super::request::AgentExecutionRequest {
    if is_card(scenario) {
        agent_pipeline_request("release-agent", "agent")
    } else if scenario == "nested" {
        root_request(
            &root_yaml("child-flow"),
            vec![reference((3, 4), "pipeline", "child-flow")],
        )
    } else {
        root_request(
            &root_yaml("worker"),
            vec![reference((5, 6), "agent", "worker")],
        )
    }
}

async fn receipt_rows(pool: &sqlx::PgPool) -> i64 {
    sqlx::query_scalar::<_, i64>(
        "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints WHERE metadata LIKE '%' || $1 || '%'",
    )
    .bind(RECEIPTS_KEY)
    .fetch_one(pool)
    .await
    .expect("count receipt-bearing checkpoints")
}

async fn run_scenario(url: &str, scenario: &str, phases: &[&str]) -> IsolatedPostgres {
    let db = IsolatedPostgres::create(url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let mut pending = String::new();
    for phase in phases {
        let mut command = std::process::Command::new(std::env::current_exe().expect("test binary"));
        command
            .args(["--exact", CHILD_TEST, "--nocapture", "--test-threads=1"])
            .env(DATABASE_ENV, &db.database_name)
            .env(SCENARIO_ENV, scenario)
            .env(PHASE_ENV, phase)
            .env(PENDING_ENV, &pending);
        let output = run_phase(phase, command);
        let stdout = String::from_utf8_lossy(&output.stdout);
        assert!(
            output.status.success(),
            "{scenario}/{phase} process failed (deadline {PHASE_DEADLINE:?}): {stdout} {}",
            String::from_utf8_lossy(&output.stderr)
        );
        assert!(
            stdout.contains("1 passed"),
            "{scenario}/{phase} process did not run the child: {stdout}"
        );
        if let Some(card) = stdout
            .lines()
            .find_map(|line| line.split_once(PENDING_MARKER).map(|(_, value)| value))
        {
            pending = card.to_owned();
        }
    }
    db
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_nested_ordinary_agent_in_a_saved_child_resumes_once_after_process_replacement() {
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for the scoped continuation PostgreSQL proof"
        );
        return;
    };
    let db = run_scenario(&url, "nested", &["start", "answer", "repeat"]).await;
    assert!(
        receipt_rows(&db.pool).await > 0,
        "the root checkpoint carries the original graph-call receipt"
    );
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_sensitive_tool_card_of_a_graph_agent_node_resumes_once_after_process_replacement()
{
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for the scoped continuation PostgreSQL proof"
        );
        return;
    };
    let db = run_scenario(&url, "sensitive", &["start", "answer", "repeat"]).await;
    assert!(
        receipt_rows(&db.pool).await > 0,
        "the root checkpoint carries the original graph-call receipt"
    );
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_three_call_batch_with_one_sensitive_call_resumes_once_after_process_replacement()
{
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for the scoped continuation PostgreSQL proof"
        );
        return;
    };
    let db = run_scenario(&url, "batch", &["start", "answer", "repeat"]).await;
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_conversation_an_older_worker_paused_continues_unscoped_without_receipts() {
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for the scoped continuation PostgreSQL proof"
        );
        return;
    };
    let db = run_scenario(&url, "older", &["start", "answer"]).await;
    assert_eq!(
        receipt_rows(&db.pool).await,
        0,
        "an older conversation keeps its resolver and never gains a graph-call receipt"
    );
    db.pool.close().await;
}

fn assembler(
    pool: &sqlx::PgPool,
    scenario: &str,
    phase: &str,
    outcomes: Vec<TestModelGatewayOutcome>,
) -> (PipelineNativeAgentAssembler, CapturedModelRequests) {
    let (platform, model_facade, captured, _) = saved_runtime(&versions(scenario), outcomes);
    let assembler = PipelineNativeAgentAssembler::postgres(
        pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        if scenario == "batch" {
            super::scoped_tests::batch_policy()
        } else if scenario == "sensitive" {
            sensitive_pipeline_mcp_policy()
        } else {
            Arc::new(
                ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new())
                    .expect("empty tool policy"),
            )
        },
        platform,
        model_facade,
    );
    let assembler = if scenario == "batch" {
        assembler.with_mcp_connector(Arc::new(super::scoped_tests::BatchConnector))
    } else if scenario == "sensitive" {
        assembler.with_mcp_connector(Arc::new(PipelineMcpConnector {
            connections: Arc::new(std::sync::atomic::AtomicUsize::new(0)),
            tool_calls: Arc::new(std::sync::atomic::AtomicUsize::new(0)),
            read_only: true,
        }))
    } else {
        assembler
    };
    // Only the first leg of the older scenario is the older worker.
    let assembler = if scenario == "older" && phase == "start" {
        assembler.with_fresh_start_continuation(ApplicationContinuation::LegacyUnscoped)
    } else {
        assembler
    };
    (assembler, captured)
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_scoped_child() {
    let Ok(database) = std::env::var(DATABASE_ENV) else {
        return;
    };
    assert!(
        database.starts_with("elitea_rust_session_")
            && database
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_')
    );
    let scenario = std::env::var(SCENARIO_ENV).expect("scenario");
    let phase = std::env::var(PHASE_ENV).expect("phase");
    let pending = std::env::var(PENDING_ENV).unwrap_or_default();
    let pool = connect(&database).await;
    match phase.as_str() {
        "start" => {
            let (assembler, captured) = assembler(
                &pool,
                &scenario,
                &phase,
                vec![TestModelGatewayOutcome::Response(if scenario == "batch" {
                    super::scoped_tests::three_call_response()
                } else if scenario == "sensitive" {
                    pipeline_mcp_tool_call_response()
                } else {
                    super::restart_pg_tests::ask_user_call_response("call_ask_user_1")
                })],
            );
            let request = request(&scenario);
            let invocation = assembler
                .assemble(authorized_turn(&request, "execution-start", 1))
                .await
                .expect("start: assemble");
            let (interrupts, _) = collect_pipeline_pause(invocation).await;
            let card = pending_card(&interrupts);
            assert_eq!(model_requests(&captured), 1, "start: one Agent model call");
            println!("{PENDING_MARKER}{card}");
        }
        "answer" => {
            let card: Value = serde_json::from_str(&pending).expect("pending card");
            let (assembler, captured) = assembler(
                &pool,
                &scenario,
                &phase,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    if is_card(&scenario) {
                        "child resumed summary"
                    } else {
                        "answered once"
                    },
                ))],
            );
            let request = if is_card(&scenario) {
                pipeline_application_resume_request(
                    vec![card["interrupt_id"].as_str().expect("card id").to_owned()],
                    "approve",
                    "",
                    [41; 32],
                )
            } else {
                answer(request(&scenario), &card, 41)
            };
            let invocation = assembler
                .assemble(authorized_turn(&request, "execution-answer", 2))
                .await
                .expect("answer: the replacement Worker assembles");
            let expected = if is_card(&scenario) {
                "child resumed summary"
            } else {
                "answered once"
            };
            let browser = collect_pipeline_completion(invocation).await;
            assert!(
                browser.iter().any(|event| event["content"] == expected),
                "answer: the final answer reaches the browser: {browser:?}"
            );
            assert_eq!(
                model_requests(&captured),
                1,
                "answer: only the resumed Agent runs"
            );
            println!("{PENDING_MARKER}{card}");
        }
        "repeat" => {
            let card: Value = serde_json::from_str(&pending).expect("pending card");
            let (assembler, captured) = assembler(
                &pool,
                &scenario,
                &phase,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "unexpected second answer",
                ))],
            );
            let request = if is_card(&scenario) {
                pipeline_application_resume_request(
                    vec![card["interrupt_id"].as_str().expect("card id").to_owned()],
                    "approve",
                    "",
                    [42; 32],
                )
            } else {
                answer(request(&scenario), &card, 42)
            };
            if let Ok(invocation) = assembler
                .assemble(authorized_turn(&request, "execution-repeat", 3))
                .await
            {
                let _ = tokio::time::timeout(
                    std::time::Duration::from_secs(30),
                    collect_pipeline_completion(invocation),
                )
                .await;
            }
            assert_eq!(
                model_requests(&captured),
                0,
                "repeat: an answered card resumes nothing"
            );
        }
        other => panic!("unknown phase {other}"),
    }
    pool.close().await;
}
