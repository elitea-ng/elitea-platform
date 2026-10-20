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
//! * `effectful` / `effectful_deny` / `effectful_crash`: the sensitive-tool card of an `agent`
//!   node whose tool changes data (FU-226). Approve runs the tool exactly once in a replacement
//!   Worker, and a repeated decision runs nothing. Block resumes the Agent once with the tool's
//!   blocked result. A Worker that dies after dispatching the approved call leaves a durable
//!   resume marker, so the next Worker refuses with the "may have started" reason and never
//!   calls the tool again.

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
    matches!(
        scenario,
        "sensitive"
            | "batch"
            | "effectful"
            | "effectful_deny"
            | "effectful_crash"
            | "effectful_later_turn"
            | "older_effectful"
    )
}

/// Scenarios whose card belongs to the one-call sensitive MCP Agent.
fn is_sensitive_mcp(scenario: &str) -> bool {
    matches!(
        scenario,
        "sensitive"
            | "effectful"
            | "effectful_deny"
            | "effectful_crash"
            | "effectful_later_turn"
            | "older_effectful"
    )
}

fn versions(scenario: &str) -> BTreeMap<Saved, Value> {
    if is_sensitive_mcp(scenario) {
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
        if *phase == "crash" {
            // The Worker dies inside the approved tool, after its dispatch.
            assert_eq!(
                output.status.code(),
                Some(CRASH_EXIT_CODE),
                "{scenario}/{phase} process did not die inside the tool: {stdout} {}",
                String::from_utf8_lossy(&output.stderr)
            );
            continue;
        }
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
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
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
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
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

const CRASH_EXIT_CODE: i32 = 87;

/// The sensitive MCP tool of the fixture, whose execution kills the Worker process: the
/// approved call was dispatched and its outcome is unknown.
struct CrashingMcpConnector;

#[async_trait]
impl McpConnector for CrashingMcpConnector {
    async fn connect(
        &self,
        _config: &RemoteMcpConfig,
    ) -> Result<Arc<dyn Toolset>, McpMaterializationError> {
        Ok(Arc::new(BasicToolset::new(
            "fixture_mcp",
            vec![Arc::new(CrashingMcpTool(PipelineMcpTool {
                calls: Arc::new(std::sync::atomic::AtomicUsize::new(0)),
                read_only: false,
            }))],
        )))
    }
}

struct CrashingMcpTool(PipelineMcpTool);

#[async_trait]
impl Tool for CrashingMcpTool {
    fn name(&self) -> &str {
        self.0.name()
    }
    fn description(&self) -> &str {
        self.0.description()
    }
    fn parameters_schema(&self) -> Option<Value> {
        self.0.parameters_schema()
    }
    fn is_read_only(&self) -> bool {
        false
    }
    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        _arguments: Value,
    ) -> adk_rust::Result<Value> {
        std::process::exit(CRASH_EXIT_CODE)
    }
}

/// Run a resumed pipeline to its end and return the upstream code of its failure.
async fn collect_pipeline_failure(
    mut invocation: AssembledNativeAgentInvocation<PipelineAgentCompletion>,
) -> Option<String> {
    invocation
        .project_start(timestamp(1))
        .expect("pipeline resume browser start");
    let (mut run, _projector, _completion) = invocation.start().expect("pipeline resume start");
    loop {
        match run.next_event().await {
            Err(error) => break error.upstream_code().map(str::to_owned),
            Ok(Some(_)) => {}
            Ok(None) => panic!("the approved effectful call must not complete"),
        }
    }
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_effectful_sensitive_tool_approval_in_a_graph_agent_node_runs_once_and_never_again()
 {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for the scoped continuation PostgreSQL proof"
        );
        return;
    };
    let db = run_scenario(&url, "effectful", &["start", "answer", "repeat"]).await;
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_effectful_approval_in_a_later_turn_of_the_conversation_runs_once() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for the scoped continuation PostgreSQL proof"
        );
        return;
    };
    let db = run_scenario(&url, "effectful_later_turn", &["first", "start", "answer"]).await;
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_effectful_approval_in_a_conversation_an_older_worker_paused_runs_once() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for the scoped continuation PostgreSQL proof"
        );
        return;
    };
    // The unscoped resolver: the card was paused without graph-call receipts.
    let db = run_scenario(&url, "older_effectful", &["start", "answer", "repeat"]).await;
    assert_eq!(
        receipt_rows(&db.pool).await,
        0,
        "the older conversation keeps its unscoped resolver"
    );
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_effectful_approval_dispatched_before_a_crash_is_refused_not_repeated() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for the scoped continuation PostgreSQL proof"
        );
        return;
    };
    let db = run_scenario(&url, "effectful_crash", &["start", "crash", "recover"]).await;
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_effectful_sensitive_tool_block_in_a_graph_agent_node_resumes_once() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!(
            "SKIP: set ELITEA_TEST_DATABASE_URL for the scoped continuation PostgreSQL proof"
        );
        return;
    };
    let db = run_scenario(&url, "effectful_deny", &["start", "deny"]).await;
    db.pool.close().await;
}

#[tokio::test(flavor = "current_thread")]
async fn postgres_three_call_batch_with_one_sensitive_call_resumes_once_after_process_replacement()
{
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
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
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
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
) -> (
    PipelineNativeAgentAssembler,
    CapturedModelRequests,
    Arc<std::sync::atomic::AtomicUsize>,
) {
    let tool_calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let (platform, model_facade, captured, _) = saved_runtime(&versions(scenario), outcomes);
    let assembler = PipelineNativeAgentAssembler::postgres(
        pool.clone(),
        SessionLimits::default(),
        CheckpointLimits::default(),
        if scenario == "batch" {
            super::scoped_tests::batch_policy()
        } else if is_sensitive_mcp(scenario) {
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
    } else if scenario == "effectful_crash" && phase == "crash" {
        assembler.with_mcp_connector(Arc::new(CrashingMcpConnector))
    } else if is_sensitive_mcp(scenario) {
        assembler.with_mcp_connector(Arc::new(PipelineMcpConnector {
            connections: Arc::new(std::sync::atomic::AtomicUsize::new(0)),
            tool_calls: Arc::clone(&tool_calls),
            read_only: scenario == "sensitive",
        }))
    } else {
        assembler
    };
    // Only the first leg of the older scenario is the older worker.
    let assembler = if matches!(scenario, "older" | "older_effectful") && phase == "start" {
        assembler.with_fresh_start_continuation(ApplicationContinuation::LegacyUnscoped)
    } else {
        assembler
    };
    (assembler, captured, tool_calls)
}

#[tokio::test(flavor = "current_thread")]
#[allow(clippy::too_many_lines)] // One child entry point owns every scenario phase of the replaced process.
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
            let (assembler, captured, _) = assembler(
                &pool,
                &scenario,
                &phase,
                vec![TestModelGatewayOutcome::Response(if scenario == "batch" {
                    super::scoped_tests::three_call_response()
                } else if is_sensitive_mcp(&scenario) {
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
        "first" => {
            // An earlier turn of the same conversation finishes before the card's turn.
            let (assembler, _, _) = assembler(
                &pool,
                &scenario,
                &phase,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "first turn done",
                ))],
            );
            let mut request = request(&scenario);
            request.binding.request_content_digest = [40; 32];
            let invocation = assembler
                .assemble(authorized_turn(&request, "execution-first", 1))
                .await
                .expect("first: assemble");
            let browser = collect_pipeline_completion(invocation).await;
            assert!(
                browser
                    .iter()
                    .any(|event| event["content"] == "first turn done"),
                "first: the earlier turn finishes: {browser:?}"
            );
        }
        "answer"
            if matches!(
                scenario.as_str(),
                "effectful" | "effectful_later_turn" | "older_effectful"
            ) =>
        {
            let card: Value = serde_json::from_str(&pending).expect("pending card");
            let (assembler, captured, tool_calls) = assembler(
                &pool,
                &scenario,
                &phase,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "child ran the approved change",
                ))],
            );
            let request = pipeline_application_resume_request(
                vec![card["interrupt_id"].as_str().expect("card id").to_owned()],
                "approve",
                "",
                [41; 32],
            );
            let invocation = assembler
                .assemble(authorized_turn(&request, "execution-answer", 2))
                .await
                .expect("answer: the replacement Worker assembles");
            let browser = collect_pipeline_completion(invocation).await;
            assert!(
                browser
                    .iter()
                    .any(|event| event["content"] == "child ran the approved change"),
                "answer: the approved call runs and the Agent finishes: {browser:?}"
            );
            assert_eq!(
                tool_calls.load(std::sync::atomic::Ordering::Acquire),
                1,
                "answer: the approved effectful tool runs exactly once"
            );
            assert_eq!(
                model_requests(&captured),
                1,
                "answer: only the resumed Agent runs"
            );
            println!("{PENDING_MARKER}{card}");
        }
        "crash" => {
            let card: Value = serde_json::from_str(&pending).expect("pending card");
            let (assembler, _, _) = assembler(&pool, &scenario, &phase, Vec::new());
            let request = pipeline_application_resume_request(
                vec![card["interrupt_id"].as_str().expect("card id").to_owned()],
                "approve",
                "",
                [44; 32],
            );
            let invocation = assembler
                .assemble(authorized_turn(&request, "execution-crash", 2))
                .await
                .expect("crash: the Worker assembles the approval");
            // The crashing tool exits this process; reaching the end is the failure.
            let _ = collect_pipeline_failure(invocation).await;
            panic!("crash: the approved tool was never dispatched");
        }
        "recover" => {
            let card: Value = serde_json::from_str(&pending).expect("pending card");
            let (assembler, captured, tool_calls) = assembler(
                &pool,
                &scenario,
                &phase,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "unexpected second effect",
                ))],
            );
            let request = pipeline_application_resume_request(
                vec![card["interrupt_id"].as_str().expect("card id").to_owned()],
                "approve",
                "",
                [44; 32],
            );
            // The replacement is the same execution re-claimed. The root dispatch journal
            // holds the approved call's dispatch without an outcome, so the decision is
            // refused as "may have started" before the tool binds a second time.
            let upstream = match assembler
                .assemble(authorized_turn(&request, "execution-crash", 3))
                .await
            {
                Ok(invocation) => collect_pipeline_failure(invocation).await,
                Err(error) => error
                    .cause()
                    .map(elitea_agent_runtime::assembly_error::NativeAgentAssemblyCause::code)
                    .map(str::to_owned),
            };
            assert_eq!(
                crate::protocol::output::model_failure(upstream.as_deref()),
                crate::protocol::output::RuntimeFailureKind::RecoveryRefused,
                "recover: the may-have-started reason, got {upstream:?}"
            );
            assert_eq!(
                tool_calls.load(std::sync::atomic::Ordering::Acquire),
                0,
                "recover: the approved effect is never repeated"
            );
            assert_eq!(model_requests(&captured), 0, "recover: no model call");
        }
        "deny" => {
            let card: Value = serde_json::from_str(&pending).expect("pending card");
            let (assembler, captured, tool_calls) = assembler(
                &pool,
                &scenario,
                &phase,
                vec![TestModelGatewayOutcome::Response(pipeline_text_response(
                    "child summarized the block",
                ))],
            );
            let request = pipeline_application_resume_request(
                vec![card["interrupt_id"].as_str().expect("card id").to_owned()],
                "block_with_comment",
                "not this one",
                [43; 32],
            );
            let invocation = assembler
                .assemble(authorized_turn(&request, "execution-deny", 2))
                .await
                .expect("deny: the replacement Worker assembles");
            let browser = collect_pipeline_completion(invocation).await;
            assert!(
                browser
                    .iter()
                    .any(|event| event["content"] == "child summarized the block"),
                "deny: the Agent resumes with the blocked result: {browser:?}"
            );
            assert_eq!(
                tool_calls.load(std::sync::atomic::Ordering::Acquire),
                0,
                "deny: the blocked tool never runs"
            );
            assert_eq!(
                model_requests(&captured),
                1,
                "deny: only the resumed Agent runs"
            );
        }
        "answer" => {
            let card: Value = serde_json::from_str(&pending).expect("pending card");
            let (assembler, captured, _) = assembler(
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
            let (assembler, captured, tool_calls) = assembler(
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
            assert_eq!(
                tool_calls.load(std::sync::atomic::Ordering::Acquire),
                0,
                "repeat: a repeated decision never runs the tool"
            );
        }
        other => panic!("unknown phase {other}"),
    }
    pool.close().await;
}
